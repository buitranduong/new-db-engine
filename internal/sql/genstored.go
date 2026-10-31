package sql

import (
	"fmt"
	"strings"

	"stask/internal/schema"
)

// Generated columns: GENERATED ALWAYS AS (expr) STORED. PostgreSQL 16 has only
// STORED (no VIRTUAL, which arrived in 18). Precedent: PostgreSQL docs, section 5.3
// "Generated Columns" and CREATE TABLE. The value is computed from the other
// columns of the row on every INSERT and UPDATE (computeGenerated, called by
// checkRow, the one place every write path goes through) and stored with the
// row like any other value, so indexes, constraints, CDC and the columnar
// view need nothing new. The expression text is kept in schema.Column.Generated
// (additive, omitempty: older data has no such column).

// nonImmutable are functions a generation expression may not use (42P17).
var nonImmutable = map[string]bool{
	"now": true, "clock_timestamp": true, "statement_timestamp": true, "transaction_timestamp": true,
	"timeofday": true, "random": true, "setseed": true, "nextval": true, "currval": true, "lastval": true,
	"setval": true, "gen_random_uuid": true, "uuid_generate_v4": true, "txid_current": true,
	"pg_backend_pid": true, "current_user": true, "session_user": true, "current_schema": true,
	"current_database": true, "version": true, "current_setting": true, "set_config": true,
	"generateuuidv4": true, "rand": true, "age": true, "current_schemas": true,
}

// generatedExpr parses "AS (expr) STORED" of a column definition.
func (p *parser) generatedExpr(cd *ColDef) error {
	p.next() // AS
	if err := p.expectOp("("); err != nil {
		return err
	}
	start := p.peek().pos
	if _, err := p.expr(); err != nil {
		return err
	}
	text := strings.TrimSpace(p.src[start:p.peek().pos])
	if err := p.expectOp(")"); err != nil {
		return err
	}
	if !p.acceptKw("stored") {
		return p.errHere("syntax error at or near %q", p.peek().s)
	}
	cd.Generated = text
	return nil
}

// validateGenerated checks the generation expression of column name of t
// (whose columns, generated ones included, are already listed).
func validateGenerated(t *schema.Table, name, text string) error {
	x, err := parseCheck(text)
	if err != nil {
		return err
	}
	var bad error
	walk(x, func(e Expr) {
		if bad != nil {
			return
		}
		switch e := e.(type) {
		case *Subquery, *Exists, *InSub:
			bad = errf(codeFeature, "cannot use subquery in column generation expression")
		case *ColRef:
			i := colIndex(t, e.Name)
			switch {
			case i < 0:
				bad = errf(codeUndefinedColumn, "column %q does not exist", e.Name)
			case t.Columns[i].Generated != "" || e.Name == name:
				bad = errf("42P17", "cannot use generated column %q in column generation expression", e.Name)
			}
		case *Func:
			switch {
			case isAggCall(e):
				bad = errf("42803", "aggregate functions are not allowed in column generation expressions")
			case e.Over != nil:
				bad = errf("42P20", "window functions are not allowed in column generation expressions")
			case nonImmutable[e.Name] || volatileFuncs[e.Name] || sessionFuncs[e.Name]:
				bad = errf("42P17", "generation expression is not immutable")
			}
		}
	})
	return bad
}

// hasGenerated reports whether the table has a stored generated column.
func hasGenerated(t *schema.Table) bool {
	for i := range t.Columns {
		if t.Columns[i].Generated != "" {
			return true
		}
	}
	return false
}

// computeGenerated sets the generated columns of row r from its other columns.
func computeGenerated(t *schema.Table, r schema.Row) error {
	var ctx *evalCtx
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Generated == "" {
			continue
		}
		x, err := parseCheck(c.Generated)
		if err != nil {
			return err
		}
		if ctx == nil {
			ctx = &evalCtx{sc: &scope{t: t}, row: r, now: nowMicros()}
		}
		v, err := ctx.eval(x)
		if err != nil {
			return err
		}
		if r[i], err = (*Session)(nil).valCol(c, v); err != nil {
			return err
		}
	}
	return nil
}

// errGeneratedValue is the error for a value given to a generated column
// (SQLSTATE 428C9, generated_always, for INSERT and UPDATE alike).
func errGeneratedValue(c *schema.Column, update bool) error {
	msg := fmt.Sprintf("cannot insert a non-DEFAULT value into column %q", c.Name)
	if update {
		msg = fmt.Sprintf("column %q can only be updated to DEFAULT", c.Name)
	}
	return &Error{Code: "428C9", Msg: msg, Detail: fmt.Sprintf("Column %q is a generated column.", c.Name)}
}

// checkSetGenerated refuses UPDATE ... SET of a generated column to anything but DEFAULT.
func checkSetGenerated(t *schema.Table, set []Assign) error {
	for _, a := range set {
		if i, ok := t.Col(a.Col); ok && t.Columns[i].Generated != "" {
			if _, def := a.X.(*DefaultExpr); !def {
				return errGeneratedValue(&t.Columns[i], true)
			}
		}
	}
	return nil
}

// expandGeneratedDeps handles DROP COLUMN of a column that stored generated
// columns use: refused (2BP01) unless CASCADE, which drops them too.
func (s *Session) expandGeneratedDeps(st *AlterTable) error {
	ts, err := s.userTable(st.Name)
	if err != nil {
		return nil
	}
	t := ts.Schema()
	if !hasGenerated(t) {
		return nil
	}
	beingDropped := map[string]bool{}
	for _, a := range st.Actions {
		if a.Kind == "drop_column" {
			beingDropped[a.Column] = true
		}
	}
	var out []AlterAction
	for _, a := range st.Actions {
		if a.Kind == "drop_column" {
			for _, g := range t.Columns {
				if g.Generated == "" || beingDropped[g.Name] {
					continue
				}
				x, err := parseCheck(g.Generated)
				if err != nil || !refersToCol(x, a.Column) {
					continue
				}
				if !a.Cascade {
					return &Error{Code: codeDependentObjects, Msg: fmt.Sprintf("cannot drop column %s of table %s because other objects depend on it", a.Column, t.Name),
						Detail: fmt.Sprintf("column %s of table %s depends on column %s of table %s", g.Name, t.Name, a.Column, t.Name),
						Hint:   "Use DROP ... CASCADE to drop the dependent objects too."}
				}
				beingDropped[g.Name] = true
				out = append(out, AlterAction{Kind: "drop_column", Column: g.Name, Cascade: true})
			}
		}
		out = append(out, a)
	}
	st.Actions = out
	return nil
}

// rewriteGenerated stores the values of columns added by ADD COLUMN ...
// GENERATED in every existing row, as PostgreSQL's table rewrite does.
func (s *Session) rewriteGenerated(table string, cols []string) error {
	set := make([]Assign, len(cols))
	for i, c := range cols {
		set[i] = Assign{Col: c, X: &DefaultExpr{}}
	}
	_, err := s.exec(&Update{Table: table, Set: set}, nil)
	return err
}

// renameInExpr rewrites the references to column old in an expression text
// (RENAME COLUMN keeps generation expressions valid, as PostgreSQL's parse
// trees do).
func renameInExpr(text, old, nw string) string {
	toks, err := lex(text)
	if err != nil {
		return text
	}
	var b strings.Builder
	last := 0
	for i, t := range toks {
		if t.k != tIdent || t.s != old || t.pos+len(old) > len(text) || !strings.EqualFold(text[t.pos:t.pos+len(old)], old) {
			continue
		}
		if i+1 < len(toks) && toks[i+1].k == tOp && toks[i+1].s == "(" {
			continue // a function of that name
		}
		if i > 0 && toks[i-1].k == tOp && toks[i-1].s == "." {
			continue
		}
		b.WriteString(text[last:t.pos])
		b.WriteString(nw)
		last = t.pos + len(old)
	}
	b.WriteString(text[last:])
	return b.String()
}
