package sql

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"stask/internal/core"
	"stask/internal/schema"
)

// Trigger functions: CREATE TRIGGER .. EXECUTE FUNCTION f(args) where f is a PL/pgSQL function
// RETURNS trigger (docs/14 appendix W). The function runs in the transaction of the statement
// that fired the trigger, with these variables: NEW and OLD (as NEW.col / OLD.col, assignable
// in a BEFORE trigger), TG_OP, TG_NAME, TG_TABLE_NAME, TG_RELNAME, TG_TABLE_SCHEMA, TG_WHEN,
// TG_LEVEL, TG_NARGS and TG_ARGV[n]. RETURN NEW / OLD / NULL: in a BEFORE row trigger NULL skips
// the row (no write, no AFTER triggers, not counted); the row returned is the one written.
// RAISE EXCEPTION ends the statement with the SQLSTATE given (P0001 by default).
//
// Not supported: NEW / OLD used as a whole (to_jsonb(NEW), NEW IS NULL, assigning a record),
// transition tables (REFERENCING), constraint triggers, INSTEAD OF, TRUNCATE triggers. AFTER row
// triggers run right after each row (PostgreSQL queues them to the end of the statement).
// Statement-level triggers fire for INSERT, UPDATE and DELETE statements, also when no row
// matches; they do not fire for COPY, MERGE or a data-modifying WITH.

// plCache keeps parsed function bodies by text.
var plCache sync.Map // string -> *plBlock

func cachedPlParse(body string, names []string) (*plBlock, error) {
	key := body + "\x00" + strings.Join(names, ",")
	if b, ok := plCache.Load(key); ok {
		return b.(*plBlock), nil
	}
	b, err := plParse(body, names)
	if err != nil {
		return nil, err
	}
	n := 0
	plCache.Range(func(any, any) bool { n++; return n < 2000 })
	if n >= 2000 {
		plCache.Clear()
	}
	plCache.Store(key, b)
	return b, nil
}

// assignCols lists the columns an UPDATE assigns (lower case), for UPDATE OF triggers.
func assignCols(set []Assign) []string {
	out := make([]string, 0, len(set))
	for _, a := range set {
		out = append(out, strings.ToLower(a.Col))
	}
	return out
}

// pushSetCols records the columns the running UPDATE assigns; the returned func restores the
// previous value (statements nest through triggers).
func (s *Session) pushSetCols(cols []string) func() {
	old := s.setCols
	s.setCols = cols
	return func() { s.setCols = old }
}

// updateOfHit tells whether an UPDATE OF trigger fires: PostgreSQL looks at the columns the
// statement assigns, not at whether their value changed. If the statement is not known to
// this session (MERGE, internal repair), a changed value counts.
func (s *Session) updateOfHit(t *schema.Table, tg *core.Trigger, old, nw schema.Row) bool {
	if s.setCols != nil {
		for _, c := range tg.UpdateOf {
			if slices.Contains(s.setCols, strings.ToLower(c)) {
				return true
			}
		}
		return false
	}
	for _, c := range tg.UpdateOf {
		if ci, ok := t.Col(c); ok && old != nil && nw != nil && old[ci] != nw[ci] {
			return true
		}
	}
	return false
}

// triggerVars builds the variables of a trigger function run.
func triggerVars(t *schema.Table, tg *core.Trigger, timing, event string, old, nw schema.Row) map[string]Datum {
	level := "ROW"
	if tg.Statement() {
		level = "STATEMENT"
	}
	v := map[string]Datum{
		"tg_op": TextD(strings.ToUpper(event)), "tg_name": TextD(tg.Name), "tg_table_name": TextD(t.Name),
		"tg_relname": TextD(t.Name), "tg_table_schema": TextD("public"), "tg_when": TextD(strings.ToUpper(timing)),
		"tg_level": TextD(level), "tg_nargs": IntD(int64(len(tg.Args))), "found": BoolD(false),
	}
	for i, a := range tg.Args {
		v["tg_argv["+strconv.Itoa(i)+"]"] = TextD(a)
	}
	if !tg.Statement() {
		for _, ci := range t.VisibleCols() {
			c := &t.Columns[ci]
			if nw != nil {
				v["new."+c.Name] = fromCol(c, nw[ci])
			}
			if old != nil {
				v["old."+c.Name] = fromCol(c, old[ci])
			}
		}
	}
	return v
}

// runTriggerFunc runs the function of tg. It returns the row to write (nil: the one passed in),
// and skip=true when a BEFORE row trigger returned NULL.
func (s *Session) runTriggerFunc(tx *core.Tx, t *schema.Table, tg *core.Trigger, timing, event string, old, nw schema.Row) (schema.Row, bool, error) {
	d, ok := s.loadFunc(tg.Func)
	if !ok {
		return nil, false, errf(codeUndefinedFunction, "function %s() does not exist", tg.Func)
	}
	if d.Ret != "trigger" {
		return nil, false, errf("42P17", "function %s must return type trigger", tg.Func)
	}
	if d.Lang != "plpgsql" {
		return nil, false, errf("42P13", "trigger functions must be written in plpgsql")
	}
	blk, err := cachedPlParse(d.Body, nil)
	if err != nil {
		return nil, false, err
	}
	if s.plDepth >= maxPLDepth {
		return nil, false, errf("54001", "stack depth limit exceeded")
	}
	s.plDepth++
	defer func() { s.plDepth-- }()
	r := &plRun{s: s, vars: triggerVars(t, tg, timing, event, old, nw), tx: tx}
	if _, err := r.block(blk, nil); err != nil {
		return nil, false, err
	}
	if !r.returned {
		return nil, false, errf("2F005", "control reached end of trigger procedure without RETURN")
	}
	if tg.Statement() || timing == "after" {
		return nil, false, nil
	}
	switch r.retMode {
	case "null":
		return nil, true, nil
	case "old":
		if event == "update" {
			return append(schema.Row(nil), old...), false, nil // the row is written as it was
		}
		return nil, false, nil
	}
	if r.retMode == "" { // RETURN <expression>: a row value; only NULL (the SQL NULL) is meaningful
		if r.ret.IsNull() {
			return nil, true, nil
		}
		return nil, false, errf("42804", "trigger function returned a value that is not a record")
	}
	if len(r.dirty) == 0 || nw == nil {
		return nil, false, nil
	}
	out := append(schema.Row(nil), nw...)
	for name := range r.dirty {
		ci, ok := t.Col(name)
		if !ok {
			continue
		}
		if out[ci], err = toValueCol(&t.Columns[ci], r.vars["new."+name]); err != nil {
			return nil, false, err
		}
	}
	return out, false, nil
}

// withStmtTriggers runs f (an INSERT, UPDATE or DELETE statement on table) between the BEFORE
// and AFTER statement-level triggers of that table.
func (s *Session) withStmtTriggers(tx *core.Tx, table, event string, f func() (*Result, error)) (*Result, error) {
	tgs := s.E.Triggers(table)
	if tgs == nil || s.noTrig {
		return f()
	}
	has := false
	for _, tg := range tgs {
		if tg.Statement() && slices.Contains(tg.Events, event) {
			has = true
		}
	}
	if !has {
		return f()
	}
	if err := s.fireStmtTriggers(tx, table, tgs, "before", event); err != nil {
		return nil, err
	}
	res, err := f()
	if err != nil {
		return nil, err
	}
	if err := s.fireStmtTriggers(tx, table, tgs, "after", event); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Session) fireStmtTriggers(tx *core.Tx, table string, tgs []*core.Trigger, timing, event string) error {
	var t *schema.Table
	for _, tg := range tgs {
		if !tg.Statement() || !tg.Fires(timing, event) {
			continue
		}
		if t == nil {
			ts, err := s.E.UserTable(table)
			if err != nil {
				return errf(codeUndefinedTable, "relation %q does not exist", table)
			}
			t = ts.Schema()
			if s.trigDepth >= maxTriggerDepth {
				return errf("54001", "stack depth limit exceeded: triggers of %q nested more than %d levels", table, maxTriggerDepth)
			}
			s.trigDepth++
			defer func() { s.trigDepth-- }()
		}
		if tg.Func != "" {
			if _, _, err := s.runTriggerFunc(tx, t, tg, timing, event, nil, nil); err != nil {
				return err
			}
			continue
		}
		prog, err := progOf(t, tg)
		if err != nil {
			return err
		}
		if prog.when != nil {
			if ok, err := s.trigTrue(tx, prog.when, nil); err != nil {
				return err
			} else if !ok {
				continue
			}
		}
		for _, step := range prog.steps {
			if err := s.runStep(tx, t, step, nil, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateTriggerFunc checks, at CREATE TRIGGER, that the function exists and is a trigger function.
func (s *Session) validateTriggerFunc(name string) error {
	d, ok := s.loadFunc(name)
	if !ok {
		return errf(codeUndefinedFunction, "function %s() does not exist", name)
	}
	if d.Ret != "trigger" {
		return errf("42P17", "function %s must return type trigger", name)
	}
	if d.Lang != "plpgsql" {
		return errf("42P13", "trigger functions must be written in plpgsql")
	}
	_, err := cachedPlParse(d.Body, nil)
	return err
}

// triggerDef renders pg_get_triggerdef.
func triggerDef(tg *core.Trigger) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TRIGGER %s %s ", tg.Name, strings.ToUpper(tg.Timing))
	for i, e := range tg.Events {
		if i > 0 {
			b.WriteString(" OR ")
		}
		b.WriteString(strings.ToUpper(e))
		if e == "update" && len(tg.UpdateOf) > 0 {
			b.WriteString(" OF " + strings.Join(tg.UpdateOf, ", "))
		}
	}
	level := "ROW"
	if tg.Statement() {
		level = "STATEMENT"
	}
	fmt.Fprintf(&b, " ON public.%s FOR EACH %s", tg.Table, level)
	if tg.When != "" {
		fmt.Fprintf(&b, " WHEN (%s)", tg.When)
	}
	if tg.Func != "" {
		args := make([]string, len(tg.Args))
		for i, a := range tg.Args {
			args[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
		}
		fmt.Fprintf(&b, " EXECUTE FUNCTION %s(%s)", tg.Func, strings.Join(args, ", "))
	}
	return b.String()
}
