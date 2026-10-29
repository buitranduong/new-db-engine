package sql

import (
	"strings"
)

// CREATE TABLE ... AS, SELECT ... INTO, CREATE TABLE ... (LIKE ...), TEMP and
// UNLOGGED tables, materialized views, REFRESH MATERIALIZED VIEW and MERGE
// (docs/05, the last section). This file holds the syntax; ctas_exec.go,
// temptable.go and merge.go hold the behaviour.

// CreateAs is CREATE [TEMP|UNLOGGED] TABLE name [(cols)] AS query [WITH [NO]
// DATA] (also SELECT ... INTO) and CREATE MATERIALIZED VIEW name [(cols)] AS
// query [WITH [NO] DATA] (Mat).
type CreateAs struct {
	Name        string
	Cols        []string
	Query       Stmt
	Text        string // the SQL text of Query (a materialized view keeps it)
	IfNotExists bool
	Mat         bool
	NoData      bool
	Temp        bool
	Unlogged    bool
	OnCommit    string // temp tables: "", "preserve", "delete", "drop"
}

// RefreshMat is REFRESH MATERIALIZED VIEW [CONCURRENTLY] name [WITH [NO] DATA].
type RefreshMat struct {
	Name         string
	Concurrently bool
	NoData       bool
}

// DropMat is DROP MATERIALIZED VIEW [IF EXISTS] name [, ...] [CASCADE|RESTRICT].
type DropMat struct {
	Names    []string
	IfExists bool
	Cascade  bool
}

// tableExt is what CREATE TABLE has beyond the plain form.
type tableExt struct {
	Temp, Unlogged bool
	OnCommit       string
	Likes          []likeDef
	Mat            string // materialized view: the SQL of its query
	NoData         bool   // materialized view created WITH NO DATA
	done           bool   // already expanded by createTableExt
}

// likeDef is one (LIKE source ...) element, spliced in at column At.
type likeDef struct {
	Table                                string
	At                                   int
	Defaults, Constraints, Indexes, Iden bool
}

// IntoClause is SELECT ... INTO [TEMP|UNLOGGED] [TABLE] name.
type IntoClause struct {
	Name           string
	Temp, Unlogged bool
}

// MergeWhen is one WHEN clause of MERGE.
type MergeWhen struct {
	Matched    bool
	And        Expr
	Action     string // update, delete, insert, nothing
	Set        []Assign
	Cols       []string
	Values     []Expr
	Overriding string
	Defaults   bool // INSERT DEFAULT VALUES
}

// Merge is MERGE INTO target [AS alias] USING source ON cond WHEN ... .
type Merge struct {
	Table, Alias string
	Source       FromNode
	On           Expr
	Whens        []MergeWhen
}

func (*CreateAs) stmt()   {}
func (*RefreshMat) stmt() {}
func (*DropMat) stmt()    {}
func (*Merge) stmt()      {}

func (c *CreateTable) ext() *tableExt {
	if c.Ext == nil {
		c.Ext = &tableExt{}
	}
	return c.Ext
}

// createCtas is the hook at the top of createStmt (after CREATE): it takes
// the forms of CREATE this file owns; ok is false (nothing consumed) for the
// rest. A plain CREATE TEMP|UNLOGGED TABLE (columns listed) is parsed by the
// ordinary code, which is re-entered with the flags removed.
func (p *parser) createCtas() (st Stmt, ok bool, err error) {
	save := p.i
	temp, unlogged := false, false
loop:
	for {
		switch {
		case p.acceptKw("global"), p.acceptKw("local"):
		case p.acceptKw("temp"), p.acceptKw("temporary"):
			temp = true
		case p.acceptKw("unlogged"):
			unlogged = true
		default:
			break loop
		}
	}
	if p.acceptKw("materialized", "view") {
		if temp || unlogged {
			return nil, true, errf(codeFeature, "materialized views cannot be temporary or unlogged")
		}
		c := &CreateAs{Mat: true, IfNotExists: p.acceptKw("if", "not", "exists")}
		if c.Name, err = p.qualifiedName(); err != nil {
			return nil, true, err
		}
		if err = p.ctasRest(c); err != nil {
			return nil, true, err
		}
		return c, true, nil
	}
	tpos := p.i
	if !p.acceptKw("table") {
		p.i = save
		return nil, false, nil
	}
	c := &CreateAs{Temp: temp, Unlogged: unlogged, IfNotExists: p.acceptKw("if", "not", "exists")}
	if c.Name, err = p.qualifiedName(); err != nil {
		return nil, true, err
	}
	isAs := p.isKw("as") || p.isKw("on") && p.peekN(1).s == "commit"
	if !isAs && p.isOp("(") { // (col, ...) AS
		depth, j := 0, 0
		for ; p.i+j < len(p.toks); j++ {
			t := p.peekN(j)
			if t.k == tEOF {
				break
			}
			if t.k == tOp && t.s == "(" {
				depth++
			} else if t.k == tOp && t.s == ")" {
				if depth--; depth == 0 {
					break
				}
			}
		}
		k := j + 1
		if p.peekN(k).s == "on" && p.peekN(k+1).s == "commit" { // ON COMMIT {PRESERVE ROWS | DELETE ROWS | DROP}
			k += 3
			if w := p.peekN(k - 1).s; w == "preserve" || w == "delete" {
				k++
			}
		}
		if n := p.peekN(k); n.k == tIdent && n.s == "as" {
			isAs = true
		}
	}
	if isAs {
		if err = p.ctasRest(c); err != nil {
			return nil, true, err
		}
		return c, true, nil
	}
	p.i = save
	if !temp && !unlogged {
		return nil, false, nil
	}
	// plain CREATE TEMP|UNLOGGED TABLE: let createStmt parse it again from TABLE
	p.toks[tpos-1] = token{k: tIdent, s: "create", pos: p.toks[tpos-1].pos}
	p.i = tpos - 1
	st, err = p.createStmt()
	if err != nil {
		return nil, true, err
	}
	if ct, isCT := st.(*CreateTable); isCT {
		e := ct.ext()
		e.Temp, e.Unlogged = temp, unlogged
	}
	return st, true, nil
}

// ctasRest reads [(cols)] [ON COMMIT ..] AS query [WITH [NO] DATA].
func (p *parser) ctasRest(c *CreateAs) error {
	if p.acceptOp("(") {
		for {
			n, err := p.ident()
			if err != nil {
				return err
			}
			c.Cols = append(c.Cols, n)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return err
		}
	}
	if oc, ok, err := p.onCommit(); err != nil {
		return err
	} else if ok {
		c.OnCommit = oc
	}
	if err := p.expectKw("as"); err != nil {
		return err
	}
	start := p.peek().pos
	var err error
	if c.Query, err = p.queryStmt(); err != nil {
		return err
	}
	c.Text = strings.TrimSpace(p.src[start:p.peek().pos])
	if p.acceptKw("with", "no", "data") {
		c.NoData = true
	} else {
		p.acceptKw("with", "data")
	}
	return nil
}

// onCommit reads ON COMMIT {PRESERVE ROWS | DELETE ROWS | DROP}.
func (p *parser) onCommit() (string, bool, error) {
	switch {
	case p.acceptKw("on", "commit", "preserve", "rows"):
		return "preserve", true, nil
	case p.acceptKw("on", "commit", "delete", "rows"):
		return "delete", true, nil
	case p.acceptKw("on", "commit", "drop"):
		return "drop", true, nil
	case p.isKw("on") && p.peekN(1).s == "commit":
		return "", true, p.errHere("syntax error at or near %q", p.peekN(2).s)
	}
	return "", false, nil
}

// tableTail is the hook at the end of CREATE TABLE (columns listed): ON COMMIT.
func (p *parser) tableTail(c *CreateTable) error {
	oc, ok, err := p.onCommit()
	if err != nil {
		return err
	}
	if ok {
		c.ext().OnCommit = oc
	}
	return nil
}

// likeElem reads a (LIKE source [INCLUDING|EXCLUDING ..]) element of a
// CREATE TABLE column list; ok is false if the next element is not one.
func (p *parser) likeElem(c *CreateTable) (ok bool, err error) {
	if !p.acceptKw("like") {
		return false, nil
	}
	l := likeDef{At: len(c.Cols)}
	if l.Table, err = p.qualifiedName(); err != nil {
		return true, err
	}
	for {
		var inc bool
		switch {
		case p.acceptKw("including"):
			inc = true
		case p.acceptKw("excluding"):
		default:
			c.ext().Likes = append(c.ext().Likes, l)
			return true, nil
		}
		t := p.next()
		if t.k != tIdent {
			return true, p.errHere("syntax error at or near %q", t.s)
		}
		switch t.s {
		case "all":
			l.Defaults, l.Constraints, l.Indexes, l.Iden = inc, inc, inc, inc
		case "defaults":
			l.Defaults = inc
		case "constraints":
			l.Constraints = inc
		case "indexes":
			l.Indexes = inc
		case "identity":
			l.Iden = inc
		case "comments", "compression", "generated", "statistics", "storage":
		default:
			return true, p.errHere("syntax error at or near %q", t.s)
		}
	}
}

// dropMat is the hook of DROP (after DROP): DROP MATERIALIZED VIEW.
func (p *parser) dropMat() (Stmt, bool, error) {
	if !p.acceptKw("materialized", "view") {
		return nil, false, nil
	}
	d := &DropMat{IfExists: p.acceptKw("if", "exists")}
	var err error
	if d.Names, err = p.tableList(); err != nil {
		return nil, true, err
	}
	if d.Cascade = p.acceptKw("cascade"); !d.Cascade {
		p.acceptKw("restrict")
	}
	return d, true, nil
}

// ctasStatement is the hook of statement() for the statements this file owns
// as a first word: MERGE and REFRESH.
func (p *parser) ctasStatement(word string) (Stmt, bool, error) {
	switch word {
	case "merge":
		st, err := p.mergeStmt()
		return st, true, err
	case "refresh":
		p.next()
		if err := p.expectKw("materialized"); err != nil {
			return nil, true, err
		}
		if err := p.expectKw("view"); err != nil {
			return nil, true, err
		}
		r := &RefreshMat{Concurrently: p.acceptKw("concurrently")}
		var err error
		if r.Name, err = p.qualifiedName(); err != nil {
			return nil, true, err
		}
		if p.acceptKw("with", "no", "data") {
			r.NoData = true
		} else {
			p.acceptKw("with", "data")
		}
		return r, true, nil
	}
	return nil, false, nil
}

// parseInto reads what follows INTO in a SELECT ... INTO.
func (p *parser) parseInto() (*IntoClause, error) {
	in := &IntoClause{}
loop:
	for {
		switch {
		case p.acceptKw("temp"), p.acceptKw("temporary"):
			in.Temp = true
		case p.acceptKw("unlogged"):
			in.Unlogged = true
		default:
			break loop
		}
	}
	p.acceptKw("table")
	var err error
	in.Name, err = p.qualifiedName()
	return in, err
}

// intoWrap turns SELECT ... INTO t ... into CREATE TABLE t AS SELECT ....
func intoWrap(st Stmt, err error) (Stmt, error) {
	if err != nil {
		return st, err
	}
	sel, ok := st.(*Select)
	if !ok || sel.Into == nil {
		return st, nil
	}
	in := sel.Into
	q := *sel
	q.Into = nil
	return &CreateAs{Name: in.Name, Query: &q, Temp: in.Temp, Unlogged: in.Unlogged}, nil
}

// mergeStmt parses MERGE INTO t [AS a] USING src ON cond WHEN ... THEN ... .
func (p *parser) mergeStmt() (Stmt, error) {
	p.next()
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	m := &Merge{}
	var err error
	p.acceptKw("only")
	if m.Table, m.Alias, err = p.tableAlias(); err != nil {
		return nil, err
	}
	if err = p.expectKw("using"); err != nil {
		return nil, err
	}
	if m.Source, err = p.fromItem(); err != nil {
		return nil, err
	}
	if err = p.expectKw("on"); err != nil {
		return nil, err
	}
	if m.On, err = p.expr(); err != nil {
		return nil, err
	}
	for p.acceptKw("when") {
		w := MergeWhen{}
		if p.acceptKw("not", "matched") {
			if p.acceptKw("by", "source") {
				return nil, errf(codeFeature, "WHEN NOT MATCHED BY SOURCE is not supported (PostgreSQL 17)")
			}
			p.acceptKw("by", "target")
		} else if p.acceptKw("matched") {
			w.Matched = true
		} else {
			return nil, p.errHere("syntax error at or near %q", p.peek().s)
		}
		if p.acceptKw("and") {
			if w.And, err = p.expr(); err != nil {
				return nil, err
			}
		}
		if err = p.expectKw("then"); err != nil {
			return nil, err
		}
		switch {
		case p.acceptKw("do", "nothing"):
			w.Action = "nothing"
		case w.Matched && p.acceptKw("update"):
			w.Action = "update"
			if err = p.expectKw("set"); err != nil {
				return nil, err
			}
			if w.Set, err = p.assignments(); err != nil {
				return nil, err
			}
		case w.Matched && p.acceptKw("delete"):
			w.Action = "delete"
		case !w.Matched && p.acceptKw("insert"):
			w.Action = "insert"
			if p.acceptOp("(") {
				for {
					n, err := p.ident()
					if err != nil {
						return nil, err
					}
					w.Cols = append(w.Cols, n)
					if !p.acceptOp(",") {
						break
					}
				}
				if err = p.expectOp(")"); err != nil {
					return nil, err
				}
			}
			switch {
			case p.acceptKw("overriding", "system", "value"):
				w.Overriding = "system"
			case p.acceptKw("overriding", "user", "value"):
				w.Overriding = "user"
			}
			if p.acceptKw("default", "values") {
				w.Defaults = true
				break
			}
			if err = p.expectKw("values"); err != nil {
				return nil, err
			}
			if err = p.expectOp("("); err != nil {
				return nil, err
			}
			if w.Values, err = p.exprListDefault(); err != nil {
				return nil, err
			}
		default:
			return nil, p.errHere("syntax error at or near %q", p.peek().s)
		}
		m.Whens = append(m.Whens, w)
	}
	if len(m.Whens) == 0 {
		return nil, p.errHere("syntax error at or near %q (expected WHEN)", p.peek().s)
	}
	return m, nil
}

// exprListDefault reads expr, ... ) where an item may be DEFAULT.
func (p *parser) exprListDefault() ([]Expr, error) {
	var out []Expr
	for {
		if p.acceptKw("default") {
			out = append(out, &DefaultExpr{})
		} else {
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		if p.acceptOp(")") {
			return out, nil
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
	}
}

// maxParamExt is maxParamAny for the statements of this file.
func maxParamExt(st Stmt) int {
	switch q := st.(type) {
	case *CreateAs:
		return maxParamAny(q.Query)
	case *Merge:
		items := []SelItem{{X: q.On}}
		for _, w := range q.Whens {
			if w.And != nil {
				items = append(items, SelItem{X: w.And})
			}
			for _, a := range w.Set {
				items = append(items, SelItem{X: a.X})
			}
			for _, x := range w.Values {
				items = append(items, SelItem{X: x})
			}
		}
		return maxParamDeep(&Select{Items: items, Rel: q.Source})
	}
	return 0
}
