package sql

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// INSTEAD OF triggers on views, transition tables (REFERENCING OLD/NEW TABLE) and TRUNCATE triggers
// (docs/14 appendix Z).
//
// INSTEAD OF: an INSERT / UPDATE / DELETE whose target is a view with INSTEAD OF row triggers is
// not rewritten onto a base table: the view's rows are read (UPDATE and DELETE) or the new rows
// evaluated (INSERT), and the trigger function runs once per row with OLD / NEW; the statement's
// tag counts the rows for which the function returned something else than NULL.
//
// Transition tables: while an INSERT / UPDATE / DELETE runs on a table that has an AFTER statement
// trigger with REFERENCING, the rows written are collected (fireTriggers sees each one); when the
// trigger fires, its OLD / NEW table names resolve to those rows (compileFrom), in memory.
//
// TRUNCATE triggers are statement-level, BEFORE or AFTER, and fire for each table named.

// transCollector gathers the rows one statement wrote.
type transCollector struct {
	table string
	t     *schema.Table
	old   []schema.Row
	nw    []schema.Row
}

func (c *transCollector) add(old, nw schema.Row) {
	if old != nil {
		c.old = append(c.old, append(schema.Row(nil), old...))
	}
	if nw != nil {
		c.nw = append(c.nw, append(schema.Row(nil), nw...))
	}
}

// transTab is a transition table as a query sees it.
type transTab struct {
	t    *schema.Table
	rows []schema.Row
}

func (s *Session) transTable(name string) *transTab {
	if s.transTabs == nil {
		return nil
	}
	return s.transTabs[name]
}

// relColsOf lists the visible columns of t as relation columns qualified by qual.
func relColsOf(t *schema.Table, qual string) []relCol {
	var cols []relCol
	for _, ci := range t.VisibleCols() {
		c := &t.Columns[ci]
		k := colKind(c)
		cols = append(cols, relCol{qual: qual, name: c.Name, kind: k, ty: colTy(c).forKind(k)})
	}
	return cols
}

func (tt *transTab) datums() [][]Datum {
	out := make([][]Datum, len(tt.rows))
	for i, r := range tt.rows {
		row := make([]Datum, 0, len(tt.t.Columns))
		for _, ci := range tt.t.VisibleCols() {
			row = append(row, fromCol(&tt.t.Columns[ci], r[ci]))
		}
		out[i] = row
	}
	return out
}

// installTransition makes the transition tables of tg visible; the returned func removes them.
func (s *Session) installTransition(t *schema.Table, tg *core.Trigger) func() {
	if tg.TransOld == "" && tg.TransNew == "" {
		return func() {}
	}
	prev := s.transTabs
	m := map[string]*transTab{}
	for k, v := range prev {
		m[k] = v
	}
	var old, nw []schema.Row
	if c := s.trans; c != nil && c.table == t.Name {
		old, nw = c.old, c.nw
	}
	if tg.TransOld != "" {
		m[tg.TransOld] = &transTab{t: t, rows: old}
	}
	if tg.TransNew != "" {
		m[tg.TransNew] = &transTab{t: t, rows: nw}
	}
	s.transTabs = m
	return func() { s.transTabs = prev }
}

// wantsTransition reports whether an AFTER statement trigger on table needs the rows of event.
func wantsTransition(tgs []*core.Trigger, event string) bool {
	for _, tg := range tgs {
		if tg.Statement() && tg.Timing == "after" && slices.Contains(tg.Events, event) && (tg.TransOld != "" || tg.TransNew != "") {
			return true
		}
	}
	return false
}

// checkTriggerShape applies PostgreSQL's rules on what a trigger definition may combine.
func (s *Session) checkTriggerShape(st *CreateTrigger) error {
	isView := false
	if _, err := s.E.UserTable(st.Table); err != nil && s.isView(st.Table) {
		isView = true
	}
	truncate := slices.Contains(st.Events, "truncate")
	switch {
	case st.Timing == "instead" && !isView:
		return errf(codeWrongObjectType, "%q is a table; tables cannot have INSTEAD OF triggers", st.Table)
	case st.Timing == "instead" && st.Level == "statement":
		return errf("42P17", "INSTEAD OF triggers must be FOR EACH ROW")
	case st.Timing == "instead" && truncate:
		return errf("42P17", "INSTEAD OF triggers cannot be defined on TRUNCATE")
	case st.Timing == "instead" && st.When != "":
		return errf("42P17", "INSTEAD OF triggers cannot have WHEN conditions")
	case st.Timing == "instead" && len(st.UpdateOf) > 0:
		return errf("42P17", "INSTEAD OF triggers cannot have column lists")
	case isView && st.Timing != "instead" && st.Level != "statement":
		return errf(codeWrongObjectType, "%q is a view; views cannot have row-level BEFORE or AFTER triggers", st.Table)
	case truncate && st.Level == "row":
		return errf(codeFeature, "TRUNCATE FOR EACH ROW triggers are not supported")
	case (st.TransOld != "" || st.TransNew != "") && st.Timing != "after":
		return errf("42P17", "transition table name can only be specified for an AFTER trigger")
	case (st.TransOld != "" || st.TransNew != "") && st.Level == "row":
		return errf(codeFeature, "transition tables with FOR EACH ROW triggers are not supported (use FOR EACH STATEMENT)")
	case (st.TransOld != "" || st.TransNew != "") && truncate:
		return errf("42P17", "TRUNCATE triggers with transition tables are not supported")
	case (st.TransOld != "" || st.TransNew != "") && len(st.Events) > 1:
		return errf(codeFeature, "transition tables cannot be specified for triggers with more than one event")
	case st.TransOld != "" && !slices.Contains(st.Events, "update") && !slices.Contains(st.Events, "delete"):
		return errf("42P17", "OLD TABLE can only be specified for a DELETE or UPDATE trigger")
	case st.TransNew != "" && !slices.Contains(st.Events, "update") && !slices.Contains(st.Events, "insert"):
		return errf("42P17", "NEW TABLE can only be specified for an INSERT or UPDATE trigger")
	case len(st.UpdateOf) > 0 && (st.TransOld != "" || st.TransNew != ""):
		return errf(codeFeature, "transition tables cannot be specified for triggers with column lists")
	}
	return nil
}

// createViewTrigger stores a trigger on a view (INSTEAD OF row triggers, statement triggers).
func (s *Session) createViewTrigger(st *CreateTrigger) (*Result, error) {
	if _, exists := s.E.Trigger(st.Table, st.Name); exists && !st.OrReplace {
		if st.IfNotExists {
			return &Result{Tag: "CREATE TRIGGER", Notice: fmt.Sprintf("trigger %q for relation %q already exists, skipping", st.Name, st.Table)}, nil
		}
		return nil, errf(codeDuplicateObject, "trigger %q for relation %q already exists", st.Name, st.Table)
	}
	if st.Func == "" {
		return nil, errf(codeFeature, "triggers on views must EXECUTE FUNCTION")
	}
	if err := s.validateTriggerFunc(st.Func); err != nil {
		return nil, err
	}
	tg := &core.Trigger{Name: st.Name, Table: st.Table, Timing: st.Timing, Events: st.Events, When: st.When,
		Func: st.Func, Args: st.Args, Level: st.Level, TransOld: st.TransOld, TransNew: st.TransNew}
	if err := s.E.PutTrigger(tg, st.OrReplace); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE TRIGGER"}, nil
}

// ---- TRUNCATE ----

// fireTruncate runs the statement-level TRUNCATE triggers of the tables, BEFORE or AFTER.
func (s *Session) fireTruncate(tables []string, timing string) error {
	for _, n := range tables {
		tgs := s.E.Triggers(n)
		for _, tg := range tgs {
			if !tg.Statement() || !tg.Fires(timing, "truncate") {
				continue
			}
			if err := s.beginFnTx(); err != nil {
				return err
			}
			if err := s.fireStmtTriggers(s.tx, n, []*core.Trigger{tg}, timing, "truncate"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- INSTEAD OF ----

// hasInsteadTriggers reports whether name is a view with INSTEAD OF triggers.
func (s *Session) hasInsteadTriggers(name string) bool {
	tgs := s.E.Triggers(name)
	if tgs == nil {
		return false
	}
	if _, err := s.E.UserTable(name); err == nil {
		return false
	}
	for _, tg := range tgs {
		if tg.Timing == "instead" {
			return true
		}
	}
	return false
}

// insteadDML runs INSERT / UPDATE / DELETE on a view that has INSTEAD OF triggers; handled=false if
// st is not such a statement.
func (s *Session) insteadDML(tx *core.Tx, st Stmt, params []Datum) (res *Result, handled bool, err error) {
	var name, event string
	switch q := st.(type) {
	case *Insert:
		name, event = q.Table, "insert"
	case *Update:
		name, event = q.Table, "update"
	case *Delete:
		name, event = q.Table, "delete"
	default:
		return nil, false, nil
	}
	tgs := s.E.Triggers(name)
	if tgs == nil {
		return nil, false, nil
	}
	var inst []*core.Trigger
	for _, tg := range tgs {
		if tg.Fires("instead", event) {
			inst = append(inst, tg)
		}
	}
	if len(inst) == 0 {
		return nil, false, nil
	}
	if _, terr := s.E.UserTable(name); terr == nil {
		return nil, false, nil
	}
	run := func(old, nw []Datum, cols []Column) (bool, error) {
		counted := false
		for _, tg := range inst {
			ret, err := s.runInsteadFunc(tx, tg, name, cols, event, old, nw)
			if err != nil {
				return false, err
			}
			counted = ret
			if !ret {
				break
			}
		}
		return counted, nil
	}
	star := []SelItem{{Star: true}}
	switch q := st.(type) {
	case *Insert:
		if q.Returning != nil {
			return nil, true, errf(codeFeature, "RETURNING on a view with INSTEAD OF triggers is not supported")
		}
		vres, err := s.runQuery(tx, &Select{Items: star, From: name, Limit: &Lit{IntD(0)}}, nil)
		if err != nil {
			return nil, true, err
		}
		cols := vres.Cols
		colIdx := make([]int, 0, len(cols))
		if q.Cols == nil {
			for i := range cols {
				colIdx = append(colIdx, i)
			}
		} else {
			for _, c := range q.Cols {
				found := -1
				for i, vc := range cols {
					if strings.EqualFold(vc.Name, c) {
						found = i
					}
				}
				if found < 0 {
					return nil, true, errf(codeUndefinedColumn, "column %q of relation %q does not exist", c, name)
				}
				colIdx = append(colIdx, found)
			}
		}
		var src [][]Datum
		if q.Query != nil {
			r, err := s.runQuery(tx, q.Query, params)
			if err != nil {
				return nil, true, err
			}
			src = r.Rows
		} else {
			for _, row := range q.Rows {
				items := make([]SelItem, len(row))
				for i, x := range row {
					items[i] = SelItem{X: x}
				}
				r, err := s.runQuery(tx, &Select{Items: items}, params)
				if err != nil {
					return nil, true, err
				}
				src = append(src, r.Rows...)
			}
		}
		n := 0
		for _, vals := range src {
			nw := make([]Datum, len(cols))
			for i, ci := range colIdx {
				if i < len(vals) {
					nw[ci] = vals[i]
				}
			}
			ok, err := run(nil, nw, cols)
			if err != nil {
				return nil, true, err
			}
			if ok {
				n++
			}
		}
		return &Result{Tag: "INSERT 0 " + strconv.Itoa(n)}, true, nil
	case *Update:
		if q.Returning != nil {
			return nil, true, errf(codeFeature, "RETURNING on a view with INSTEAD OF triggers is not supported")
		}
		items := append([]SelItem{{Star: true}}, make([]SelItem, 0, len(q.Set))...)
		for _, a := range q.Set {
			items = append(items, SelItem{X: a.X})
		}
		r, err := s.runQuery(tx, &Select{Items: items, From: name, Alias: q.Alias, Where: q.Where}, params)
		if err != nil {
			return nil, true, err
		}
		nv := len(r.Cols) - len(q.Set)
		cols := r.Cols[:nv]
		n := 0
		for _, row := range r.Rows {
			old := row[:nv]
			nw := append([]Datum(nil), old...)
			for i, a := range q.Set {
				for ci, c := range cols {
					if strings.EqualFold(c.Name, a.Col) {
						nw[ci] = row[nv+i]
					}
				}
			}
			ok, err := run(old, nw, cols)
			if err != nil {
				return nil, true, err
			}
			if ok {
				n++
			}
		}
		return &Result{Tag: "UPDATE " + strconv.Itoa(n)}, true, nil
	case *Delete:
		if q.Returning != nil {
			return nil, true, errf(codeFeature, "RETURNING on a view with INSTEAD OF triggers is not supported")
		}
		r, err := s.runQuery(tx, &Select{Items: star, From: name, Alias: q.Alias, Where: q.Where}, params)
		if err != nil {
			return nil, true, err
		}
		n := 0
		for _, row := range r.Rows {
			ok, err := run(row, nil, r.Cols)
			if err != nil {
				return nil, true, err
			}
			if ok {
				n++
			}
		}
		return &Result{Tag: "DELETE " + strconv.Itoa(n)}, true, nil
	}
	return nil, false, nil
}

// runInsteadFunc runs the function of an INSTEAD OF trigger for one row; it reports whether the
// function returned a row (the row counts) rather than NULL.
func (s *Session) runInsteadFunc(tx *core.Tx, tg *core.Trigger, view string, cols []Column, event string, old, nw []Datum) (bool, error) {
	d, ok := s.loadFunc(tg.Func)
	if !ok {
		return false, errf(codeUndefinedFunction, "function %s() does not exist", tg.Func)
	}
	blk, err := cachedPlParse(d.Body, nil)
	if err != nil {
		return false, err
	}
	if s.plDepth >= maxPLDepth {
		return false, errf("54001", "stack depth limit exceeded")
	}
	s.plDepth++
	defer func() { s.plDepth-- }()
	vars := map[string]Datum{
		"tg_op": TextD(strings.ToUpper(event)), "tg_name": TextD(tg.Name), "tg_table_name": TextD(view),
		"tg_relname": TextD(view), "tg_table_schema": TextD("public"), "tg_when": TextD("INSTEAD OF"),
		"tg_level": TextD("ROW"), "tg_nargs": IntD(int64(len(tg.Args))), "found": BoolD(false),
	}
	for i, a := range tg.Args {
		vars["tg_argv["+strconv.Itoa(i)+"]"] = TextD(a)
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = strings.ToLower(c.Name)
	}
	set := func(prefix string, vals []Datum) {
		if vals == nil {
			return
		}
		for i, n := range names {
			vars[prefix+"."+n] = vals[i]
		}
		vars[prefix+".*"] = TextD(strings.Join(names, ","))
	}
	set("new", nw)
	set("old", old)
	r := &plRun{s: s, vars: vars, tx: tx}
	if _, err := r.block(blk, nil); err != nil {
		return false, err
	}
	if !r.returned {
		return false, errf("2F005", "control reached end of trigger procedure without RETURN")
	}
	switch r.retMode {
	case "null":
		return false, nil
	case "new", "old":
		return true, nil
	}
	return !r.ret.IsNull(), nil
}
