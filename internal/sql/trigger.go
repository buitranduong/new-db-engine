package sql

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// Row triggers with an SQL body (docs/10 section 5, docs/12 P7.1):
//
//	CREATE [OR REPLACE] TRIGGER name {BEFORE | AFTER} {INSERT | UPDATE | DELETE} [OR ...]
//	    ON table [FOR [EACH] ROW] [WHEN (condition)]
//	BEGIN statement; ... END
//
// in the manner of SQLite (https://www.sqlite.org/lang_createtrigger.html):
// the body is a list of INSERT, UPDATE, DELETE and SELECT statements in
// which NEW.col and OLD.col are the row being written. There is no
// procedural language (no CREATE FUNCTION, no EXECUTE FUNCTION).
//
// Two extensions make the usual cases possible without one:
//   - SET NEW.col = expr [, NEW.col2 = expr2] in a BEFORE INSERT/UPDATE
//     trigger changes the row about to be written (SQLite cannot);
//   - SELECT RAISE(ABORT, 'message') [WHERE cond] and
//     SELECT CASE WHEN cond THEN RAISE(ABORT, 'message') END fail the
//     statement (error P0001). ABORT, FAIL and ROLLBACK all do that: the
//     statement fails, and with it the transaction it was running in
//     unless it is an autocommit. IGNORE is not supported.
//
// A trigger runs inside the transaction of the statement that fired it:
// what it writes commits or rolls back with it. Triggers fire in name
// order. A trigger that writes to a table with triggers fires those, up to
// maxTriggerDepth levels (then 54001), which ends a recursion that does not.
// A replica never runs a trigger: it applies the rows the leader wrote.
//
// The definition is a row of a hidden system table (core/trigger.go). The
// body is compiled once per definition (NEW.col / OLD.col become $n
// parameters bound at each firing) and kept with the cached definition.
//
// Triggers can also name a PL/pgSQL function (EXECUTE FUNCTION f(args), trigfunc.go) and be
// UPDATE OF col or FOR EACH STATEMENT.
//
// Not supported: INSTEAD OF, WHEN with OLD in an INSERT trigger or NEW in a
// DELETE trigger, TRUNCATE triggers, and ALTER TABLE ... RENAME of a table
// that has triggers.

const maxTriggerDepth = 16

// CreateTrigger is CREATE [OR REPLACE] TRIGGER.
type CreateTrigger struct {
	Name        string
	OrReplace   bool
	IfNotExists bool
	Timing      string // before, after
	Events      []string
	Table       string
	When        string   // SQL text, "" if none
	Body        string   // SQL text between BEGIN and END
	Func        string   // EXECUTE FUNCTION name(args): the trigger function ("" for an SQL body)
	Args        []string // its arguments (TG_ARGV)
	Level       string   // "row" or "statement"
	UpdateOf    []string // UPDATE OF col, ...
}

// DropTrigger is DROP TRIGGER [IF EXISTS] name [ON table].
type DropTrigger struct {
	Name     string
	Table    string // "": the table that has a trigger of this name
	IfExists bool
}

func (*CreateTrigger) stmt() {}
func (*DropTrigger) stmt()   {}

// ---- parsing (called from parse.go) ----

// createTrigger parses what follows CREATE when it is a trigger; ok is
// false (and nothing is consumed) if it is not.
func (p *parser) createTrigger() (st Stmt, ok bool, err error) {
	save := p.i
	replace := p.acceptKw("or", "replace")
	if !p.acceptKw("trigger") {
		p.i = save
		return nil, false, nil
	}
	c := &CreateTrigger{OrReplace: replace}
	c.IfNotExists = p.acceptKw("if", "not", "exists")
	if c.Name, err = p.ident(); err != nil {
		return nil, true, err
	}
	switch {
	case p.acceptKw("before"):
		c.Timing = "before"
	case p.acceptKw("after"):
		c.Timing = "after"
	case p.isKw("instead"):
		return nil, true, errf(codeFeature, "INSTEAD OF triggers are not supported")
	default:
		return nil, true, p.errHere("syntax error at or near %q (expected BEFORE or AFTER)", p.peek().s)
	}
	for {
		var ev string
		switch {
		case p.acceptKw("insert"):
			ev = "insert"
		case p.acceptKw("delete"):
			ev = "delete"
		case p.acceptKw("update"):
			ev = "update"
			if p.acceptKw("of") {
				for {
					col, err := p.ident()
					if err != nil {
						return nil, true, err
					}
					if !slices.Contains(c.UpdateOf, col) {
						c.UpdateOf = append(c.UpdateOf, col)
					}
					if !p.acceptOp(",") {
						break
					}
				}
			}
		case p.isKw("truncate"):
			return nil, true, errf(codeFeature, "TRUNCATE triggers are not supported")
		default:
			return nil, true, p.errHere("syntax error at or near %q (expected INSERT, UPDATE or DELETE)", p.peek().s)
		}
		if !slices.Contains(c.Events, ev) {
			c.Events = append(c.Events, ev)
		}
		if !p.acceptKw("or") {
			break
		}
	}
	if err := p.expectKw("on"); err != nil {
		return nil, true, err
	}
	if c.Table, err = p.qualifiedName(); err != nil {
		return nil, true, err
	}
	if p.acceptKw("for") {
		p.acceptKw("each")
		switch {
		case p.acceptKw("statement"):
			c.Level = "statement"
		case p.acceptKw("row"):
			c.Level = "row"
		default:
			return nil, true, p.errHere("syntax error at or near %q (expected ROW or STATEMENT)", p.peek().s)
		}
	}
	if len(c.UpdateOf) > 0 && !slices.Contains(c.Events, "update") {
		return nil, true, errf(codeFeature, "UPDATE OF can only be used with UPDATE events")
	}
	if p.acceptKw("when") {
		if err := p.expectOp("("); err != nil {
			return nil, true, err
		}
		start, depth := p.peek().pos, 1
		for depth > 0 {
			t := p.next()
			switch {
			case t.k == tEOF:
				return nil, true, p.errHere("syntax error at end of input (unclosed WHEN condition)")
			case t.k == tOp && t.s == "(":
				depth++
			case t.k == tOp && t.s == ")":
				depth--
				if depth == 0 {
					c.When = strings.TrimSpace(p.src[start:t.pos])
				}
			}
		}
		if c.When == "" {
			return nil, true, p.errHere("syntax error: empty WHEN condition")
		}
	}
	if p.isKw("referencing") {
		return nil, true, errf(codeFeature, "REFERENCING (transition tables) is not supported")
	}
	if p.acceptKw("execute") {
		if !p.acceptKw("function") && !p.acceptKw("procedure") {
			return nil, true, p.errHere("syntax error at or near %q (expected FUNCTION or PROCEDURE)", p.peek().s)
		}
		names, err := p.dottedName()
		if err != nil {
			return nil, true, err
		}
		c.Func = names[len(names)-1]
		if err := p.expectOp("("); err != nil {
			return nil, true, err
		}
		for !p.acceptOp(")") {
			if len(c.Args) > 0 {
				if err := p.expectOp(","); err != nil {
					return nil, true, err
				}
			}
			t := p.next()
			if t.k != tString && t.k != tNumber && t.k != tIdent && t.k != tQuoted {
				return nil, true, p.errHere("syntax error at or near %q in trigger arguments", t.s)
			}
			c.Args = append(c.Args, t.s)
		}
		if c.Level == "" {
			c.Level = "statement" // PostgreSQL default
		}
		return c, true, nil
	}
	if c.Level == "" {
		c.Level = "row" // the SQL-body form (SQLite style) is row-level unless FOR EACH STATEMENT says otherwise
	}
	if err := p.expectKw("begin"); err != nil {
		return nil, true, err
	}
	start, cases := p.peek().pos, 0
	for {
		t := p.peek()
		if t.k == tEOF {
			return nil, true, p.errHere("syntax error at end of input (expected END)")
		}
		if t.k == tIdent && t.s == "case" {
			cases++
		} else if t.k == tIdent && t.s == "end" {
			if cases == 0 {
				c.Body = strings.TrimSpace(p.src[start:t.pos])
				p.next()
				break
			}
			cases--
		}
		p.next()
	}
	return c, true, nil
}

// dropTrigger parses what follows DROP TRIGGER.
func (p *parser) dropTrigger() (Stmt, error) {
	d := &DropTrigger{IfExists: p.acceptKw("if", "exists")}
	var err error
	if d.Name, err = p.ident(); err != nil {
		return nil, err
	}
	if p.acceptKw("on") {
		if d.Table, err = p.qualifiedName(); err != nil {
			return nil, err
		}
	}
	if !p.acceptKw("cascade") {
		p.acceptKw("restrict")
	}
	return d, nil
}

// ---- compiling a body ----

const (
	stepRun   = iota // a statement
	stepSet          // SET NEW.col = expr: st is the SELECT of the values
	stepRaise        // RAISE(...): st is the SELECT of the condition (nil: always)
)

type trigRef struct {
	isNew bool
	col   string
}

type trigStep struct {
	kind int
	st   Stmt
	cols []string // stepSet
	msg  string   // stepRaise
}

// trigProg is a compiled trigger: statements over $1..$n, the n values of
// refs, taken from the NEW and OLD rows at each firing.
type trigProg struct {
	refs  []trigRef
	when  Stmt // SELECT (condition), nil if none
	steps []trigStep
}

type trigCompiler struct {
	t       *schema.Table
	tg      *core.Trigger
	refs    []trigRef
	hasNew  bool
	hasOld  bool
	before  bool
	isDelet bool
}

func (c *trigCompiler) param(isNew bool, col string) int {
	r := trigRef{isNew, col}
	if i := slices.Index(c.refs, r); i >= 0 {
		return i + 1
	}
	c.refs = append(c.refs, r)
	return len(c.refs)
}

// rewrite returns the text of toks[a:b] with NEW.col and OLD.col replaced
// by parameters.
func (c *trigCompiler) rewrite(src string, toks []token, a, b int) (string, error) {
	var sb strings.Builder
	for i := a; i < b; {
		tk := toks[i]
		if tk.k == tIdent && (tk.s == "new" || tk.s == "old") && i+2 < b && toks[i+1].k == tOp && toks[i+1].s == "." &&
			(toks[i+2].k == tIdent || toks[i+2].k == tQuoted) {
			col := toks[i+2].s
			isNew := tk.s == "new"
			if isNew && !c.hasNew {
				return "", errf("42P17", "a trigger on DELETE cannot reference NEW")
			}
			if !isNew && !c.hasOld {
				return "", errf("42P17", "a trigger on INSERT cannot reference OLD")
			}
			if _, ok := c.t.Col(col); !ok {
				return "", errf(codeUndefinedColumn, "record %q has no field %q", tk.s, col)
			}
			sb.WriteString("$" + strconv.Itoa(c.param(isNew, col)) + " ")
			i += 3
			continue
		}
		sb.WriteString(src[tk.pos:toks[i+1].pos])
		i++
	}
	return sb.String(), nil
}

func (c *trigCompiler) parse(sql string) (Stmt, error) {
	st, err := ParseOne(sql)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errf(codeSyntax, "empty statement in trigger body")
	}
	return st, nil
}

// splitTop splits toks[a:b] at the top-level tokens that are operator op.
func splitTop(toks []token, a, b int, op string) [][2]int {
	var out [][2]int
	depth, start := 0, a
	for i := a; i < b; i++ {
		if toks[i].k != tOp {
			continue
		}
		switch toks[i].s {
		case "(":
			depth++
		case ")":
			depth--
		case op:
			if depth == 0 {
				out = append(out, [2]int{start, i})
				start = i + 1
			}
		}
	}
	return append(out, [2]int{start, b})
}

func kw(toks []token, i int, s string) bool {
	return i < len(toks) && toks[i].k == tIdent && toks[i].s == s
}

// raiseAt parses RAISE ( kind , 'message' ) at toks[i]; next is the index
// after the closing parenthesis.
func raiseAt(toks []token, i int) (msg string, next int, err error) {
	if i+5 >= len(toks) || !kw(toks, i, "raise") || !(toks[i+1].k == tOp && toks[i+1].s == "(") {
		return "", 0, errf(codeSyntax, "expected RAISE(ABORT, 'message')")
	}
	if toks[i+2].k != tIdent {
		return "", 0, errf(codeSyntax, "RAISE needs ABORT, FAIL or ROLLBACK")
	}
	switch toks[i+2].s {
	case "abort", "fail", "rollback":
	case "ignore":
		return "", 0, errf(codeFeature, "RAISE(IGNORE) is not supported")
	default:
		return "", 0, errf(codeSyntax, "RAISE needs ABORT, FAIL or ROLLBACK")
	}
	if !(toks[i+3].k == tOp && toks[i+3].s == ",") || toks[i+4].k != tString || !(toks[i+5].k == tOp && toks[i+5].s == ")") {
		return "", 0, errf(codeSyntax, "RAISE needs a message: RAISE(ABORT, 'message')")
	}
	return toks[i+4].s, i + 6, nil
}

// step compiles the statement toks[a:b] (a < b) of the body src.
func (c *trigCompiler) step(src string, toks []token, a, b int) (trigStep, error) {
	first := toks[a].s
	if toks[a].k != tIdent {
		return trigStep{}, errf(codeSyntax, "syntax error at or near %q in trigger body", toks[a].s)
	}
	cond := func(ca, cb int) (Stmt, error) {
		txt, err := c.rewrite(src, toks, ca, cb)
		if err != nil {
			return nil, err
		}
		return c.parse("SELECT (" + txt + ")")
	}
	switch first {
	case "select":
		// SELECT RAISE(...) [WHERE cond]
		if kw(toks, a+1, "raise") {
			msg, n, err := raiseAt(toks, a+1)
			if err != nil {
				return trigStep{}, err
			}
			s := trigStep{kind: stepRaise, msg: msg}
			if n < b {
				if !kw(toks, n, "where") || n+1 >= b {
					return trigStep{}, errf(codeSyntax, "syntax error at or near %q after RAISE", toks[n].s)
				}
				if s.st, err = cond(n+1, b); err != nil {
					return trigStep{}, err
				}
			}
			return s, nil
		}
		// SELECT CASE WHEN cond THEN RAISE(...) END
		if kw(toks, a+1, "case") && kw(toks, a+2, "when") {
			depth, then := 0, -1
			for i := a + 3; i < b; i++ {
				if kw(toks, i, "case") {
					depth++
				} else if kw(toks, i, "end") && depth > 0 {
					depth--
				} else if kw(toks, i, "then") && depth == 0 {
					then = i
					break
				}
			}
			if then > 0 && kw(toks, then+1, "raise") {
				msg, n, err := raiseAt(toks, then+1)
				if err != nil {
					return trigStep{}, err
				}
				if n+1 != b || !kw(toks, n, "end") {
					return trigStep{}, errf(codeSyntax, "only CASE WHEN cond THEN RAISE(...) END is supported")
				}
				s := trigStep{kind: stepRaise, msg: msg}
				if s.st, err = cond(a+3, then); err != nil {
					return trigStep{}, err
				}
				return s, nil
			}
		}
		fallthrough
	case "insert", "update", "delete", "with":
		txt, err := c.rewrite(src, toks, a, b)
		if err != nil {
			return trigStep{}, err
		}
		st, err := c.parse(txt)
		if err != nil {
			return trigStep{}, err
		}
		switch st.(type) {
		case *Select, *SetOp, *Insert, *Update, *Delete:
			return trigStep{kind: stepRun, st: st}, nil
		}
		return trigStep{}, errf(codeFeature, "this statement is not allowed in a trigger body")
	case "set":
		if !kw(toks, a+1, "new") {
			return trigStep{}, errf(codeFeature, "only SET NEW.column = value is allowed in a trigger body")
		}
		if !c.before || c.isDelet {
			return trigStep{}, errf(codeFeature, "SET NEW.column is only allowed in a BEFORE INSERT or BEFORE UPDATE trigger")
		}
		var cols, vals []string
		for _, sp := range splitTop(toks, a+1, b, ",") {
			i := sp[0]
			if !(kw(toks, i, "new") && i+4 < sp[1] && toks[i+1].s == "." && toks[i+3].k == tOp && toks[i+3].s == "=") {
				return trigStep{}, errf(codeSyntax, "expected NEW.column = value in SET")
			}
			col := toks[i+2].s
			if _, ok := c.t.Col(col); !ok {
				return trigStep{}, errf(codeUndefinedColumn, "record \"new\" has no field %q", col)
			}
			v, err := c.rewrite(src, toks, i+4, sp[1])
			if err != nil {
				return trigStep{}, err
			}
			cols = append(cols, col)
			vals = append(vals, v)
		}
		st, err := c.parse("SELECT " + strings.Join(vals, ", "))
		if err != nil {
			return trigStep{}, err
		}
		return trigStep{kind: stepSet, st: st, cols: cols}, nil
	}
	return trigStep{}, errf(codeFeature, "statement %q is not allowed in a trigger body", strings.ToUpper(first))
}

// compileTrigger compiles the WHEN condition and the body of tg against
// the table t, and checks them.
func compileTrigger(t *schema.Table, tg *core.Trigger) (*trigProg, error) {
	c := &trigCompiler{t: t, tg: tg, before: tg.Timing == "before"}
	c.hasNew = slices.Contains(tg.Events, "insert") || slices.Contains(tg.Events, "update")
	c.hasOld = slices.Contains(tg.Events, "update") || slices.Contains(tg.Events, "delete")
	if tg.Statement() {
		c.hasNew, c.hasOld = false, false
	}
	c.isDelet = !c.hasNew
	p := &trigProg{}
	if tg.When != "" {
		toks, err := lex(tg.When)
		if err != nil {
			return nil, err
		}
		// PostgreSQL: NEW is invalid in the WHEN of a trigger with a DELETE event, OLD with an INSERT event
		bn, bo := c.hasNew, c.hasOld
		c.hasNew, c.hasOld = !slices.Contains(tg.Events, "delete") && c.hasNew, !slices.Contains(tg.Events, "insert") && c.hasOld
		txt, err := c.rewrite(tg.When, toks, 0, len(toks)-1)
		c.hasNew, c.hasOld = bn, bo
		if err != nil {
			return nil, err
		}
		if p.when, err = c.parse("SELECT (" + txt + ")"); err != nil {
			return nil, err
		}
	}
	if tg.Func != "" {
		p.refs = c.refs
		return p, nil
	}
	toks, err := lex(tg.Body)
	if err != nil {
		return nil, err
	}
	start := 0
	for i := 0; i < len(toks); i++ {
		if toks[i].k == tEOF || toks[i].k == tOp && toks[i].s == ";" {
			if i > start {
				st, err := c.step(tg.Body, toks, start, i)
				if err != nil {
					return nil, err
				}
				p.steps = append(p.steps, st)
			}
			start = i + 1
		}
	}
	if len(p.steps) == 0 {
		return nil, errf(codeSyntax, "a trigger needs at least one statement between BEGIN and END")
	}
	p.refs = c.refs
	return p, nil
}

func progOf(t *schema.Table, tg *core.Trigger) (*trigProg, error) {
	if p, ok := tg.Prog.Load().(*trigProg); ok {
		return p, nil
	}
	p, err := compileTrigger(t, tg)
	if err != nil {
		return nil, err
	}
	tg.Prog.Store(p)
	return p, nil
}

// ---- running ----

func (p *trigProg) params(t *schema.Table, old, nw schema.Row) ([]Datum, error) {
	if len(p.refs) == 0 {
		return nil, nil
	}
	out := make([]Datum, len(p.refs))
	for i, r := range p.refs {
		row := old
		if r.isNew {
			row = nw
		}
		if row == nil {
			continue // Null: OLD in an INSERT, NEW in a DELETE
		}
		ci, ok := t.Col(r.col)
		if !ok {
			return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", r.col, t.Name)
		}
		out[i] = fromCol(&t.Columns[ci], row[ci])
	}
	return out, nil
}

func (s *Session) trigTrue(tx *core.Tx, st Stmt, params []Datum) (bool, error) {
	res, err := s.runQuery(tx, st, params)
	if err != nil {
		return false, err
	}
	return len(res.Rows) > 0 && len(res.Rows[0]) > 0 && res.Rows[0][0].Bool(), nil
}

// runStep runs one step of an SQL-body trigger. It returns the NEW row after a SET NEW.col step
// (nw itself if the step did not change it).
func (s *Session) runStep(tx *core.Tx, t *schema.Table, step trigStep, params []Datum, nw schema.Row, changed *bool) error {
	switch step.kind {
	case stepRun:
		var err error
		switch step.st.(type) {
		case *Select, *SetOp:
			_, err = s.runQuery(tx, step.st, params)
		default:
			_, err = s.runHere(tx, step.st, params)
		}
		return err
	case stepRaise:
		if step.st != nil {
			if ok, err := s.trigTrue(tx, step.st, params); err != nil {
				return err
			} else if !ok {
				return nil
			}
		}
		return errf("P0001", "%s", step.msg)
	}
	return nil
}

// fireTriggers runs the row triggers of a table for one row and returns the row to write: NEW
// as the BEFORE triggers left it (nw itself if none changed it). skip is true when a BEFORE
// trigger returned NULL: the row is not written. The AFTER triggers' result is nw.
func (s *Session) fireTriggers(tx *core.Tx, t *schema.Table, tgs []*core.Trigger, timing, event string,
	old, nw schema.Row) (schema.Row, bool, error) {
	if s.noTrig {
		return nw, false, nil
	}
	entered := false
	for _, tg := range tgs {
		if tg.Statement() || !tg.Fires(timing, event) {
			continue
		}
		if len(tg.UpdateOf) > 0 && event == "update" && !s.updateOfHit(t, tg, old, nw) {
			continue
		}
		if !entered {
			if s.trigDepth >= maxTriggerDepth {
				return nil, false, errf("54001", "stack depth limit exceeded: triggers of %q nested more than %d levels", t.Name, maxTriggerDepth)
			}
			s.trigDepth++
			defer func() { s.trigDepth-- }()
			entered = true
		}
		prog, err := progOf(t, tg)
		if err != nil {
			return nil, false, err
		}
		if prog.when != nil {
			params, err := prog.params(t, old, nw)
			if err != nil {
				return nil, false, err
			}
			if ok, err := s.trigTrue(tx, prog.when, params); err != nil {
				return nil, false, err
			} else if !ok {
				continue
			}
		}
		if tg.Func != "" {
			out, skip, err := s.runTriggerFunc(tx, t, tg, timing, event, old, nw)
			if err != nil {
				return nil, false, err
			}
			if skip {
				return nil, true, nil
			}
			if out != nil {
				if err := checkRow(t, out); err != nil {
					return nil, false, err
				}
				nw = out
			}
			continue
		}
		changed := false
		for _, step := range prog.steps {
			params, err := prog.params(t, old, nw)
			if err != nil {
				return nil, false, err
			}
			if step.kind != stepSet {
				if err := s.runStep(tx, t, step, params, nw, &changed); err != nil {
					return nil, false, err
				}
				continue
			}
			res, err := s.runQuery(tx, step.st, params)
			if err != nil {
				return nil, false, err
			}
			if len(res.Rows) != 1 {
				return nil, false, errf(codeInternal, "SET NEW.column did not produce a row")
			}
			if !changed {
				nw = append(schema.Row(nil), nw...)
				changed = true
			}
			for i, col := range step.cols {
				ci, ok := t.Col(col)
				if !ok {
					return nil, false, errf(codeUndefinedColumn, "column %q of relation %q does not exist", col, t.Name)
				}
				if nw[ci], err = toValueCol(&t.Columns[ci], res.Rows[0][i]); err != nil {
					return nil, false, err
				}
			}
		}
		if changed {
			if err := checkRow(t, nw); err != nil {
				return nil, false, err
			}
		}
	}
	return nw, false, nil
}

// updateRow writes the new version upd of the row old (changing the key
// if the update did) around the BEFORE and AFTER UPDATE triggers tgs (nil:
// none). It returns the row written, nil if a BEFORE trigger skipped the row.
func (s *Session) updateRow(tx *core.Tx, t *schema.Table, tgs []*core.Trigger, old, upd schema.Row) (schema.Row, error) {
	var err error
	if tgs != nil {
		var skip bool
		if upd, skip, err = s.fireTriggers(tx, t, tgs, "before", "update", old, upd); err != nil {
			return nil, err
		} else if skip {
			return nil, nil
		}
	}
	if ok, nk := t.KeyOf(old), t.KeyOf(upd); ok != nk {
		if _, exists, err := tx.Get(t.Name, nk); err != nil {
			return nil, err
		} else if exists {
			return nil, dupKeyErr(t, nk)
		}
		if err := tx.CheckKeyChange(t, old, upd); err != nil {
			if fe := fkErrorOf(err); fe != nil {
				return nil, fe
			}
			return nil, err
		}
		if err := s.keyChangeAllowed(t, old, upd); err != nil {
			return nil, err
		}
		if err := tx.Delete(t.Name, ok); err != nil {
			return nil, err
		}
		if err := tx.InsertAs(t, upd); err != nil {
			return nil, err
		}
	} else if err := tx.PutAs(t, upd); err != nil {
		return nil, err
	}
	if tgs != nil {
		if _, _, err := s.fireTriggers(tx, t, tgs, "after", "update", old, upd); err != nil {
			return nil, err
		}
	}
	return upd, nil
}

// deleteRow deletes the row r around the DELETE triggers tgs (nil: none). It returns false if a
// BEFORE trigger skipped the row.
func (s *Session) deleteRow(tx *core.Tx, t *schema.Table, tgs []*core.Trigger, r schema.Row) (bool, error) {
	if tgs != nil {
		if _, skip, err := s.fireTriggers(tx, t, tgs, "before", "delete", r, nil); err != nil {
			return false, err
		} else if skip {
			return false, nil
		}
	}
	if err := tx.Delete(t.Name, t.KeyOf(r)); err != nil {
		return false, err
	}
	if tgs != nil {
		_, _, err := s.fireTriggers(tx, t, tgs, "after", "delete", r, nil)
		return true, err
	}
	return true, nil
}

// ---- statements ----

func (s *Session) triggerDDL(st Stmt) (*Result, error) {
	switch st := st.(type) {
	case *CreateTrigger:
		return s.createTrigger(st)
	case *DropTrigger:
		return s.dropTrigger(st)
	}
	return nil, errf(codeFeature, "unsupported statement %T", st)
}

func (s *Session) createTrigger(st *CreateTrigger) (*Result, error) {
	t, err := s.E.UserTable(st.Table)
	if err != nil {
		if s.isView(st.Table) {
			return nil, errf(codeWrongObjectType, "%q is a view: views cannot have row-level triggers", st.Table)
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", st.Table)
	}
	_, exists := s.E.Trigger(st.Table, st.Name)
	if exists && !st.OrReplace {
		if st.IfNotExists {
			return &Result{Tag: "CREATE TRIGGER", Notice: fmt.Sprintf("trigger %q for relation %q already exists, skipping", st.Name, st.Table)}, nil
		}
		return nil, errf(codeDuplicateObject, "trigger %q for relation %q already exists", st.Name, st.Table)
	}
	tg := &core.Trigger{Name: st.Name, Table: st.Table, Timing: st.Timing, Events: st.Events, When: st.When, Body: st.Body,
		Func: st.Func, Args: st.Args, Level: st.Level, UpdateOf: st.UpdateOf}
	for _, c := range tg.UpdateOf {
		if _, ok := t.Schema().Col(c); !ok {
			return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", c, st.Table)
		}
	}
	if tg.Func != "" {
		if err := s.validateTriggerFunc(tg.Func); err != nil {
			return nil, err
		}
	}
	if tg.Statement() && st.When != "" && strings.Contains(strings.ToLower(st.When), "new.") {
		return nil, errf("0A000", "statement-level trigger cannot have columns in its WHEN condition")
	}
	if _, err := compileTrigger(t.Schema(), tg); err != nil {
		return nil, err
	}
	if err := s.E.PutTrigger(tg, st.OrReplace); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE TRIGGER"}, nil
}

func (s *Session) dropTrigger(st *DropTrigger) (*Result, error) {
	table := st.Table
	if table == "" {
		for _, tg := range s.E.AllTriggers() {
			if tg.Name == st.Name {
				table = tg.Table
				break
			}
		}
	}
	if _, ok := s.E.Trigger(table, st.Name); !ok {
		if st.IfExists {
			return &Result{Tag: "DROP TRIGGER", Notice: fmt.Sprintf("trigger %q does not exist, skipping", st.Name)}, nil
		}
		return nil, errf(codeUndefinedObject, "trigger %q does not exist", st.Name)
	}
	if err := s.E.DropTrigger(table, st.Name); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "DROP TRIGGER"}, nil
}

// alterTableTriggers refuses to rename a table that has triggers (their
// definitions name it).
func (s *Session) alterTableTriggers(st *AlterTable) error {
	for _, a := range st.Actions {
		if a.Kind == "rename_table" && s.E.Triggers(st.Name) != nil {
			return errf(codeFeature, "cannot rename table %q: it has triggers; drop them first", st.Name)
		}
	}
	return nil
}

// ---- catalog ----

// trigOID is the OID of a trigger in pg_trigger.
func trigOID(table, name string) int64 {
	h := fnv.New32a()
	h.Write([]byte(table + "\x1f" + name))
	return trigOIDBase + int64(h.Sum32()&0x0fffffff)
}

const trigOIDBase = 0x70000000

// pgTriggerRows lists the triggers for pg_catalog.pg_trigger. tgtype has
// the bits of PostgreSQL: ROW 1, BEFORE 2, INSERT 4, DELETE 8, UPDATE 16.
func pgTriggerRows(c *catSnap) [][]Datum {
	var rows [][]Datum
	for _, tg := range c.s.E.AllTriggers() {
		var rel int64
		for _, t := range c.tables {
			if t.Name == tg.Table {
				rel = tableOID(t)
			}
		}
		if rel == 0 {
			continue
		}
		typ := int64(1)
		if tg.Statement() {
			typ = 0
		}
		if tg.Timing == "before" {
			typ |= 2
		}
		for _, ev := range tg.Events {
			typ |= map[string]int64{"insert": 4, "delete": 8, "update": 16}[ev]
		}
		qual := Null
		if tg.When != "" {
			qual = TextD(tg.When)
		}
		var foid int64
		if tg.Func != "" {
			foid = funcOID(tg.Func)
		}
		attr := vecText() // int2vector of the UPDATE OF columns, empty if none
		if len(tg.UpdateOf) > 0 {
			var nums []int
			for _, uc := range tg.UpdateOf {
				for _, t := range c.tables {
					if t.Name == tg.Table {
						if i, ok := t.Col(uc); ok {
							nums = append(nums, t.Columns[i].Num)
						}
					}
				}
			}
			attr = vecText(nums...)
		}
		rows = append(rows, []Datum{IntD(trigOID(tg.Table, tg.Name)), IntD(rel), IntD(0), TextD(tg.Name), IntD(foid),
			IntD(typ), TextD("O"), BoolD(false), IntD(0), IntD(0), IntD(0), BoolD(false), BoolD(false), IntD(int64(len(tg.Args))),
			attr, TextD(""), qual, Null, Null})
	}
	return rows
}
