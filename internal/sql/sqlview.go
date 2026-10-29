package sql

import (
	"fmt"
	"slices"
	"strings"

	"stask/internal/core"
)

// SQL views: CREATE [OR REPLACE] VIEW and DROP VIEW (docs/05). Not to be
// confused with views.go, the columnar projections of tables.
//
// A view is stored (core/sqlview.go) as the SQL text of its query, the
// output column names fixed at creation and the relations it reads
// directly. The planner replaces a reference to the view by that query as a
// derived table (expandView), so views nest, and a definition that would
// reach itself is refused when it is created (42P17). The definition is
// read at the transaction's snapshot, like any row.
//
// DDL is applied at once and not at commit, like CREATE TABLE.
//
// The catalogs list views: pg_class (relkind 'v'), pg_attribute (text
// columns: output types are not recorded), pg_views, information_schema.views,
// .tables (table_type 'VIEW') and .columns; regclass resolves them.

// CreateView is CREATE [OR REPLACE] VIEW name [(cols)] AS query.
type CreateView struct {
	Name      string
	OrReplace bool
	Cols      []string
	Query     Stmt
	Text      string // the SQL text of Query
}

// DropView is DROP VIEW [IF EXISTS] name [, ...] [CASCADE | RESTRICT].
type DropView struct {
	Names    []string
	IfExists bool
	Cascade  bool
}

func (*CreateView) stmt() {}
func (*DropView) stmt()   {}

const codeInvalidRecursion = "42P17"

// ---- parsing (called from parse.go) ----

// createView parses what follows CREATE when it is a view; ok is false
// (and nothing is consumed) if it is not.
func (p *parser) createView() (st Stmt, ok bool, err error) {
	save := p.i
	replace := p.acceptKw("or", "replace")
	if p.acceptKw("materialized", "view") {
		return nil, true, errf(codeFeature, "materialized views are not supported")
	}
	if !p.acceptKw("view") {
		if replace {
			return nil, true, errf(codeFeature, "only CREATE OR REPLACE VIEW is supported")
		}
		p.i = save
		return nil, false, nil
	}
	c := &CreateView{OrReplace: replace}
	if c.Name, err = p.qualifiedName(); err != nil {
		return nil, true, err
	}
	if p.acceptOp("(") {
		for {
			n, err := p.ident()
			if err != nil {
				return nil, true, err
			}
			c.Cols = append(c.Cols, n)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, true, err
		}
	}
	if err := p.expectKw("as"); err != nil {
		return nil, true, err
	}
	start := p.peek().pos
	if c.Query, err = p.queryStmt(); err != nil {
		return nil, true, err
	}
	c.Text = strings.TrimSpace(p.src[start:p.peek().pos])
	return c, true, nil
}

// dropView parses what follows DROP VIEW.
func (p *parser) dropView() (Stmt, error) {
	d := &DropView{IfExists: p.acceptKw("if", "exists")}
	var err error
	if d.Names, err = p.tableList(); err != nil {
		return nil, err
	}
	if d.Cascade = p.acceptKw("cascade"); !d.Cascade {
		p.acceptKw("restrict")
	}
	return d, nil
}

// ---- planner ----

// viewScope is the state of the views being expanded by one compiler.
type viewScope struct {
	stack []string        // views being expanded, outermost first
	deps  map[string]bool // relations read directly by the query being defined; nil: not recorded
}

func (c *compiler) noteDep(name string) {
	if c.vs != nil && c.vs.deps != nil {
		c.vs.deps[name] = true
	}
}

// expandView plans a reference to a view as a derived table. ok is false
// if name is not a view.
func (c *compiler) expandView(f *FromTable, qual string) (fromPlan, bool, error) {
	def, ok, err := c.s.E.View(c.tx, f.Name)
	if err != nil {
		return nil, false, AsError(err)
	}
	if !ok {
		return nil, false, nil
	}
	if c.vs == nil {
		c.vs = &viewScope{}
	}
	if slices.Contains(c.vs.stack, f.Name) {
		return nil, true, errf(codeInvalidRecursion, "infinite recursion detected in the definition of view %q", f.Name)
	}
	c.noteDep(f.Name)
	saved := c.vs.deps
	c.vs.deps = nil // only the direct dependencies of the view being defined
	c.vs.stack = append(c.vs.stack, f.Name)
	p, err := c.compileViewDef(def)
	c.vs.stack = c.vs.stack[:len(c.vs.stack)-1]
	c.vs.deps = saved
	if err != nil {
		return nil, true, err
	}
	n := len(def.Cols)
	base := p.outCols()
	if len(base) < n {
		return nil, true, errf(codeFeature, "view %q is stale: its query now returns fewer columns", f.Name)
	}
	cols := make([]relCol, n)
	for i := range cols {
		cols[i] = relCol{name: def.Cols[i], kind: base[i].kind, ty: base[i].ty}
	}
	return &viewPlan{sub: &subPlan{p: p, c: renameCols(cols, f.ColAliases, qual)}, n: len(base)}, true, nil
}

// compileViewDef plans the stored query of a view. It sees no scope of the
// query that uses the view (no CTEs, no outer columns).
func (c *compiler) compileViewDef(def *core.View) (qplan, error) {
	st, err := ParseOne(def.Query)
	if err != nil || st == nil {
		return nil, errf(codeInternal, "view %q has an invalid definition", def.Name)
	}
	return c.compile(st, &qscope{lvl: &qlevel{}})
}

// viewPlan is a view in a FROM clause: its query, trimmed to the columns
// the view had when it was created (the table behind a SELECT * may have
// gained columns since, as in PostgreSQL).
type viewPlan struct {
	sub *subPlan
	n   int // columns of the query
}

func (p *viewPlan) cols() []relCol { return p.sub.c }
func (p *viewPlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	rows, err := p.sub.run(qx, outer)
	if err != nil || len(p.sub.c) == p.n {
		return rows, err
	}
	out := make([][]Datum, len(rows))
	for i, r := range rows {
		out[i] = r[:len(p.sub.c)]
	}
	return out, nil
}

// ---- statements ----

func (s *Session) isView(name string) bool {
	if name == "" {
		return false
	}
	_, ok, _ := s.E.View(s.tx, name)
	return ok
}

func (s *Session) viewDDL(st Stmt) (*Result, error) {
	switch st := st.(type) {
	case *CreateView:
		return s.createView(st)
	case *DropView:
		return s.dropViews(st)
	}
	return nil, errf(codeFeature, "unsupported statement %T", st)
}

func (s *Session) createView(st *CreateView) (*Result, error) {
	old, exists, err := s.E.View(nil, st.Name)
	if err != nil {
		return nil, AsError(err)
	}
	if !exists && s.relationExists(st.Name) || exists && !st.OrReplace {
		return nil, errf(codeDuplicateTable, "relation %q already exists", st.Name)
	}
	if maxParamDeep(st.Query) > 0 {
		return nil, errf(codeUndefinedParam, "there is no parameter $1")
	}
	// the view being defined is on the stack: reaching it again is a cycle
	c := &compiler{s: s, vs: &viewScope{stack: []string{st.Name}, deps: map[string]bool{}}}
	p, err := c.compile(st.Query, &qscope{lvl: &qlevel{}})
	if err != nil {
		return nil, err
	}
	out := p.outCols()
	if len(st.Cols) > len(out) {
		return nil, errf(codeSyntax, "CREATE VIEW specifies more column names than columns")
	}
	def := core.View{Name: st.Name, Query: st.Text}
	for i, oc := range out {
		n := oc.name
		if i < len(st.Cols) {
			n = st.Cols[i]
		}
		if slices.Contains(def.Cols, n) {
			return nil, errf("42701", "column %q specified more than once", n)
		}
		def.Cols = append(def.Cols, n)
	}
	for d := range c.vs.deps {
		def.Deps = append(def.Deps, d)
	}
	slices.Sort(def.Deps)
	if exists {
		// as in PostgreSQL, a replacement keeps the existing columns and
		// may add new ones at the end
		oc := &compiler{s: s, vs: &viewScope{stack: []string{st.Name}}}
		op, err := oc.compileViewDef(old)
		if err != nil {
			return nil, err
		}
		oldOut := op.outCols()
		if len(def.Cols) < len(old.Cols) {
			return nil, errf("42P16", "cannot drop columns from view")
		}
		for i, n := range old.Cols {
			switch {
			case def.Cols[i] != n:
				return nil, errf("42P16", "cannot change name of view column %q to %q", n, def.Cols[i])
			case i < len(oldOut) && out[i].kind != oldOut[i].kind && out[i].kind != KNull && oldOut[i].kind != KNull:
				return nil, errf("42P16", "cannot change data type of view column %q", n)
			}
		}
	}
	if err := s.E.PutView(def, st.OrReplace); err != nil {
		return nil, AsError(err)
	}
	return &Result{Tag: "CREATE VIEW"}, nil
}

// viewDependents returns the views that depend, directly or through other
// views, on a relation of set (and are not in it), in discovery order, and
// for each the relation it reads directly.
func (s *Session) viewDependents(set map[string]bool) ([]string, map[string]string) {
	all := s.E.Views()
	found := map[string]string{}
	var order []string
	for changed := true; changed; {
		changed = false
		for _, v := range all {
			if set[v.Name] || found[v.Name] != "" {
				continue
			}
			for _, d := range v.Deps {
				if set[d] || found[d] != "" {
					found[v.Name] = d
					order = append(order, v.Name)
					changed = true
					break
				}
			}
		}
	}
	return order, found
}

// checkDependents refuses to remove the relations of set (of the given
// kind: "view" or "table") while views depend on them, unless cascade; it
// returns the views to drop with them.
func (s *Session) checkDependents(set map[string]bool, kind string, cascade bool) ([]string, string, error) {
	deps, via := s.viewDependents(set)
	if len(deps) == 0 {
		return nil, "", nil
	}
	if !cascade {
		k := "table"
		if s.isView(via[deps[0]]) {
			k = "view"
		}
		return nil, "", errf("2BP01", "cannot drop %s %s because other objects depend on it: view %s depends on %s %s",
			kind, via[deps[0]], deps[0], k, via[deps[0]])
	}
	var notes []string
	for _, d := range deps {
		notes = append(notes, "drop cascades to view "+d)
	}
	return deps, strings.Join(notes, "; "), nil
}

func (s *Session) dropViews(st *DropView) (*Result, error) {
	res := &Result{Tag: "DROP VIEW"}
	set := map[string]bool{}
	var names []string
	var skipped []string
	for _, n := range st.Names {
		if _, ok, err := s.E.View(nil, n); err != nil {
			return nil, AsError(err)
		} else if ok {
			if !set[n] {
				set[n] = true
				names = append(names, n)
			}
			continue
		}
		if _, err := s.E.UserTable(n); err == nil {
			return nil, errf(codeWrongObjectType, "%q is not a view", n)
		}
		if !st.IfExists {
			return nil, errf(codeUndefinedTable, "view %q does not exist", n)
		}
		skipped = append(skipped, fmt.Sprintf("view %q does not exist, skipping", n))
	}
	cascaded, note, err := s.checkDependents(set, "view", st.Cascade)
	if err != nil {
		return nil, err
	}
	if err := s.E.DropViews(append(names, cascaded...)); err != nil {
		return nil, AsError(err)
	}
	res.Notice = strings.Join(append(skipped, note), "; ")
	res.Notice = strings.Trim(res.Notice, "; ")
	return res, nil
}

// dropTableViews is called by DROP TABLE: views that read the tables block
// the drop, or go with it under CASCADE.
func (s *Session) dropTableViews(tables []string, cascade bool) (string, error) {
	set := map[string]bool{}
	for _, t := range tables {
		set[t] = true
	}
	deps, note, err := s.checkDependents(set, "table", cascade)
	if err != nil || len(deps) == 0 {
		return "", err
	}
	return note, asErr(s.E.DropViews(deps))
}

// asErr is AsError that keeps a nil error nil.
func asErr(err error) error {
	if err == nil {
		return nil
	}
	return AsError(err)
}

// alterTableViews refuses ALTER TABLE actions that would invalidate the
// stored text of views reading the table.
func (s *Session) alterTableViews(st *AlterTable) error {
	for _, a := range st.Actions {
		switch a.Kind {
		case "drop_column", "set_type", "rename_table", "rename_column":
		default:
			continue
		}
		deps, via := s.viewDependents(map[string]bool{st.Name: true})
		for _, d := range deps {
			if via[d] == st.Name {
				return errf("2BP01", "cannot alter table %s because view %s depends on it", st.Name, d)
			}
		}
		return nil
	}
	return nil
}
