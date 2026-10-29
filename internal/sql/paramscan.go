package sql

import "stask/internal/schema"

// Parameter discovery for statements that carry queries inside DML
// (INSERT .. SELECT, UPDATE .. FROM (subquery), DELETE .. USING, WITH ..
// DML, EXPLAIN). The extended protocol needs the exact parameter count at
// Parse/Describe time; maxParam (types.go) only looked at VALUES rows and
// top-level expressions, so `INSERT INTO t SELECT $1, c FROM x WHERE k = $2`
// described 0 parameters (08P01 at Bind). Item 1 of docs/14 SG rows.

// maxParamAny is the highest $n anywhere in st, subqueries and nested
// statements included.
func maxParamAny(st Stmt) int {
	switch q := st.(type) {
	case nil:
		return 0
	case *Select, *SetOp:
		return maxParamDeep(st)
	case *Insert:
		n := 0
		var items []SelItem
		for _, r := range q.Rows {
			for _, x := range r {
				items = append(items, SelItem{X: x})
			}
		}
		if q.OnConflict != nil {
			for _, a := range q.OnConflict.Set {
				items = append(items, SelItem{X: a.X})
			}
		}
		items = append(items, q.Returning...)
		var where Expr
		if q.OnConflict != nil {
			where = q.OnConflict.Where
		}
		n = max(n, maxParamDeep(&Select{Items: items, Where: where}))
		if q.Query != nil {
			n = max(n, maxParamAny(q.Query))
		}
		return n
	case *Update:
		items := append([]SelItem(nil), q.Returning...)
		for _, a := range q.Set {
			items = append(items, SelItem{X: a.X})
		}
		return maxParamDeep(&Select{Items: items, Where: q.Where, Rel: q.From})
	case *Delete:
		return maxParamDeep(&Select{Items: q.Returning, Where: q.Where, Rel: q.Using})
	case *WithDML:
		n := 0
		for _, c := range q.With {
			if c.Q != nil {
				n = max(n, maxParamAny(c.Q))
			}
			if c.Scalar != nil {
				n = max(n, maxParamDeep(&Select{Items: []SelItem{{X: c.Scalar}}}))
			}
		}
		return max(n, maxParamAny(q.Main))
	case *Explain:
		return maxParamAny(q.Stmt)
	}
	return 0
}

// insertSelectKinds infers $n kinds for INSERT .. SELECT: a parameter that
// is a select item takes the kind of the target column it feeds (or its
// CAST type), and the source query's own WHERE/ON uses are typed against the
// source table.
func (s *Session) insertSelectKinds(st *Insert, target *schema.Table, pk map[int]Kind) {
	sel, ok := st.Query.(*Select)
	if !ok || target == nil {
		return
	}
	setKind := func(n int, k Kind) {
		if _, done := pk[n]; !done && k != KNull {
			pk[n] = k
		}
	}
	for j, it := range sel.Items {
		if it.Star {
			break // positions after * are unknown
		}
		ci := j
		if st.Cols != nil {
			if j >= len(st.Cols) {
				continue
			}
			var found bool
			if ci, found = target.Col(st.Cols[j]); !found {
				continue
			}
		}
		if ci >= len(target.Columns) {
			continue
		}
		switch x := it.X.(type) {
		case *Param:
			setKind(x.N, colKind(&target.Columns[ci]))
		case *Cast:
			if p, isP := x.X.(*Param); isP {
				setKind(p.N, kindOfType(x.Type))
			}
		}
	}
	if sel.From != "" && sel.Rel == nil {
		if src, err := s.table(sel.From); err == nil {
			for n, k := range paramKinds(sel, &scope{t: src, alias: sel.Alias}) {
				setKind(n, k)
			}
		}
	}
}

// ---- parameter kinds through the whole statement tree ----

// mergedScope is a scope over several tables at once (a FROM list and the
// enclosing queries' tables): columns are found by "alias.col" or by bare
// name (the first table wins), for typing parameters only.
func mergedScope(ts []scopeTable) *scope {
	syn := &schema.Table{Name: "\x00merged"}
	idx := map[string]int{}
	for _, st := range ts {
		for i := range st.t.Columns {
			c := st.t.Columns[i]
			n := len(syn.Columns)
			syn.Columns = append(syn.Columns, c)
			for _, q := range []string{st.alias, st.t.Name} {
				if q != "" {
					if _, dup := idx[q+"."+c.Name]; !dup {
						idx[q+"."+c.Name] = n
					}
				}
			}
			if _, dup := idx[c.Name]; !dup {
				idx[c.Name] = n
			}
		}
	}
	return &scope{t: syn, merged: idx}
}

type scopeTable struct {
	t     *schema.Table
	alias string
}

// fromTables lists the base tables a FROM tree names.
func (s *Session) fromTables(f FromNode, out []scopeTable) []scopeTable {
	switch f := f.(type) {
	case *FromTable:
		if t, err := s.table(f.Name); err == nil {
			out = append(out, scopeTable{t, f.Alias})
		}
	case *FromJoin:
		out = s.fromTables(f.L, out)
		out = s.fromTables(f.R, out)
	}
	return out
}

// deepKinds infers $n kinds for every statement shape, descending into
// subqueries, joins, CTEs and DML. Only typed uses are recorded (text is the
// default), the first typed use wins.
func (s *Session) deepKinds(st Stmt) map[int]Kind {
	pk := map[int]Kind{}
	s.kindsStmt(st, nil, pk)
	return pk
}

func (s *Session) kindsStmt(st Stmt, outer []scopeTable, pk map[int]Kind) {
	merge := func(m map[int]Kind) {
		for n, k := range m {
			if _, done := pk[n]; !done && k != KText && k != KNull {
				pk[n] = k
			}
		}
	}
	subs := func(sc *scope, outerTs []scopeTable, xs ...Expr) {
		for _, x := range xs {
			walk(x, func(e Expr) {
				switch e := e.(type) {
				case *Subquery:
					s.kindsStmt(e.Q, outerTs, pk)
				case *Exists:
					s.kindsStmt(e.Q, outerTs, pk)
				case *InSub:
					s.kindsStmt(e.Q, outerTs, pk)
				case *AnyAll:
					s.kindsStmt(e.Q, outerTs, pk)
				}
			})
		}
	}
	var fromSubs func(f FromNode, ts []scopeTable)
	fromSubs = func(f FromNode, ts []scopeTable) {
		switch f := f.(type) {
		case *FromJoin:
			fromSubs(f.L, ts)
			fromSubs(f.R, ts)
			if f.On != nil {
				merge(paramKinds(&Select{Where: f.On}, mergedScope(ts)))
				subs(nil, ts, f.On)
			}
		case *FromSubquery:
			s.kindsStmt(f.Q, ts, pk)
		}
	}
	switch q := st.(type) {
	case *Select:
		for _, c := range q.With {
			if c.Q != nil {
				s.kindsStmt(c.Q, outer, pk)
			}
		}
		var ts []scopeTable
		if q.Rel != nil {
			ts = s.fromTables(q.Rel, nil)
			fromSubs(q.Rel, append(append([]scopeTable(nil), ts...), outer...))
		} else if q.From != "" {
			if t, err := s.table(q.From); err == nil {
				ts = []scopeTable{{t, q.Alias}}
			}
		}
		all := append(append([]scopeTable(nil), ts...), outer...)
		if len(all) > 0 {
			merge(paramKinds(q, mergedScope(all)))
		} else {
			merge(paramKinds(q, &scope{}))
		}
		xs := []Expr{q.Where, q.Having, q.Limit, q.Offset}
		for _, it := range q.Items {
			xs = append(xs, it.X)
		}
		for _, g := range q.GroupBy {
			xs = append(xs, g)
		}
		for _, o := range q.OrderBy {
			xs = append(xs, o.X)
		}
		subs(nil, all, xs...)
	case *SetOp:
		for _, c := range q.With {
			if c.Q != nil {
				s.kindsStmt(c.Q, outer, pk)
			}
		}
		s.kindsStmt(q.L, outer, pk)
		s.kindsStmt(q.R, outer, pk)
	case *WithDML:
		for _, c := range q.With {
			if c.Q != nil {
				s.kindsStmt(c.Q, outer, pk)
			}
		}
		s.kindsStmt(q.Main, outer, pk)
	case *Insert:
		t, err := s.table(q.Table)
		if err != nil {
			return
		}
		if q.Query != nil {
			m := map[int]Kind{}
			s.insertSelectKinds(q, t, m)
			merge(m)
			s.kindsStmt(q.Query, outer, pk)
		}
		ts := []scopeTable{{t, ""}}
		merge(paramKinds(&Insert{Table: q.Table, Cols: q.Cols, Rows: q.Rows, OnConflict: q.OnConflict, Returning: q.Returning}, &scope{t: t}))
		merge(paramKinds(&Select{Items: q.Returning}, mergedScope(ts)))
	case *Update:
		t, err := s.table(q.Table)
		if err != nil {
			return
		}
		ts := append([]scopeTable{{t, q.Alias}}, s.fromTables(q.From, nil)...)
		sc := mergedScope(ts)
		for _, a := range q.Set {
			if i, ok := t.Col(a.Col); ok {
				if p, isP := a.X.(*Param); isP {
					if _, done := pk[p.N]; !done {
						pk[p.N] = colKind(&t.Columns[i])
					}
				}
			}
		}
		items := append([]SelItem(nil), q.Returning...)
		xs := []Expr{q.Where}
		for _, a := range q.Set {
			items = append(items, SelItem{X: a.X})
			xs = append(xs, a.X)
		}
		merge(paramKinds(&Select{Items: items, Where: q.Where}, sc))
		if q.From != nil {
			fromSubs(q.From, ts)
		}
		subs(nil, ts, xs...)
	case *Delete:
		t, err := s.table(q.Table)
		if err != nil {
			return
		}
		ts := append([]scopeTable{{t, q.Alias}}, s.fromTables(q.Using, nil)...)
		merge(paramKinds(&Select{Items: q.Returning, Where: q.Where}, mergedScope(ts)))
		if q.Using != nil {
			fromSubs(q.Using, ts)
		}
		subs(nil, ts, q.Where)
	}
}
