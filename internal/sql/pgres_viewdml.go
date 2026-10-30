package sql

// Automatically updatable views: INSERT, UPDATE and DELETE on a view whose
// query is a plain projection of one table (or of another such view) are
// rewritten to act on that table, as PostgreSQL does. Anything richer
// (joins, aggregates, DISTINCT, LIMIT, expressions in the select list)
// keeps the "cannot change view" error. A view column that is renamed
// (SELECT id AS k) can be named in INSERT's column list and UPDATE's SET,
// but not in WHERE or RETURNING, which are only rewritten when every
// column keeps its name.

type simpleView struct {
	base   string
	where  Expr
	cols   map[string]string // view column -> base column
	ident  bool              // every view column keeps its name
	colSeq []string          // view columns in order
}

// simpleViewOf describes view name if it is automatically updatable.
func (s *Session) simpleViewOf(name string) (*simpleView, bool) {
	v, ok, _ := s.viewAt(s.tx, name)
	if !ok || v == nil {
		return nil, false
	}
	st, err := ParseOne(v.Query)
	if err != nil {
		return nil, false
	}
	q, ok := st.(*Select)
	if !ok || q.Distinct || len(q.DistinctOn) > 0 || len(q.GroupBy) > 0 || q.Having != nil || q.Rel != nil ||
		q.From == "" || q.Limit != nil || q.Offset != nil || len(q.With) > 0 || len(q.Windows) > 0 ||
		q.Into != nil || len(q.Items) != len(v.Cols) {
		return nil, false
	}
	if q.Where != nil && (hasAggOrWin(q.Where) || hasSubquery(q.Where)) {
		return nil, false
	}
	sv := &simpleView{base: q.From, where: q.Where, cols: map[string]string{}, ident: true, colSeq: v.Cols}
	for i, it := range q.Items {
		cr, ok := it.X.(*ColRef)
		if it.Star || !ok {
			return nil, false
		}
		sv.cols[v.Cols[i]] = cr.Name
		if cr.Name != v.Cols[i] {
			sv.ident = false
		}
	}
	return sv, true
}

func (sv *simpleView) mapCol(c string) (string, error) {
	b, ok := sv.cols[c]
	if !ok {
		return "", errf(codeUndefinedColumn, "column %q does not exist", c)
	}
	return b, nil
}

// viewDML returns the statement rewritten onto the base table, or st itself
// when its target is not an automatically updatable view.
func (s *Session) viewDML(st Stmt) (Stmt, error) {
	var name string
	switch q := st.(type) {
	case *Insert:
		name = q.Table
	case *Update:
		name = q.Table
	case *Delete:
		name = q.Table
	default:
		return st, nil
	}
	if _, err := s.userTable(name); err == nil || !s.isView(name) {
		return st, nil
	}
	sv, ok := s.simpleViewOf(name)
	if !ok {
		return st, nil
	}
	switch q := st.(type) {
	case *Insert:
		n := *q
		n.Table = sv.base
		cols := q.Cols
		if cols == nil {
			cols = sv.colSeq
		}
		n.Cols = nil
		for _, c := range cols {
			b, err := sv.mapCol(c)
			if err != nil {
				return nil, err
			}
			n.Cols = append(n.Cols, b)
		}
		if sv.blocks(q.Returning, nil) || q.OnConflict != nil && !sv.ident {
			return st, nil
		}
		return &n, nil
	case *Update:
		n := *q
		n.Table = sv.base
		if q.Alias == "" {
			n.Alias = ""
		}
		n.Set = nil
		for _, a := range q.Set {
			b, err := sv.mapCol(a.Col)
			if err != nil {
				return nil, err
			}
			if sv.blocks(nil, a.X) {
				return st, nil
			}
			a.Col = b
			n.Set = append(n.Set, a)
		}
		if sv.blocks(q.Returning, q.Where) {
			return st, nil
		}
		if sv.where != nil {
			if n.Where == nil {
				n.Where = sv.where
			} else {
				n.Where = &Binary{Op: "and", L: sv.where, R: n.Where}
			}
		}
		return &n, nil
	case *Delete:
		n := *q
		n.Table = sv.base
		if sv.blocks(q.Returning, q.Where) {
			return st, nil
		}
		if sv.where != nil {
			if n.Where == nil {
				n.Where = sv.where
			} else {
				n.Where = &Binary{Op: "and", L: sv.where, R: n.Where}
			}
		}
		return &n, nil
	}
	return st, nil
}

// blocks reports whether a RETURNING list or WHERE clause names a renamed
// view column (or uses *), which the rewrite does not translate.
func (sv *simpleView) blocks(ret []SelItem, where Expr) bool {
	bad := false
	check := func(x Expr) {
		walk(x, func(e Expr) {
			if cr, ok := e.(*ColRef); ok {
				if b, ok := sv.cols[cr.Name]; !ok || b != cr.Name {
					bad = true
				}
			}
		})
	}
	for _, it := range ret {
		if it.Star {
			bad = true
		}
		check(it.X)
	}
	check(where)
	return bad
}
