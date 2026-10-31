package sql

// UPDATE .. SET (a, b, ..) = (SELECT ..) (PostgreSQL docs, UPDATE: "a
// multiple-column assignment from a sub-SELECT"). The sub-SELECT is
// evaluated per row; no row gives NULLs and more than one row is an error,
// exactly as for a scalar subquery. Each column becomes an ordinary
// assignment of a scalar subquery that keeps the one matching output item
// of the sub-SELECT, so correlation, UPDATE .. FROM and RETURNING work as
// they do for single-column assignments. The sub-SELECT runs once per
// column instead of once per row.
func multiSubAssigns(cols []string, q Stmt) ([]Assign, error) {
	sel, ok := q.(*Select)
	if !ok {
		return nil, errf(codeFeature, "UPDATE SET (columns) = (subquery): only a plain SELECT is supported")
	}
	for _, it := range sel.Items {
		if it.Star {
			return nil, errf(codeFeature, "UPDATE SET (columns) = (SELECT *) is not supported; list the columns")
		}
	}
	if len(sel.Items) != len(cols) {
		return nil, errf(codeSyntax, "number of columns does not match number of values")
	}
	if sel.Distinct || len(sel.DistinctOn) > 0 {
		return nil, errf(codeFeature, "UPDATE SET (columns) = (SELECT DISTINCT ..) is not supported")
	}
	out := make([]Assign, len(cols))
	for i, c := range cols {
		one := *sel
		one.cached = nil
		one.Items = []SelItem{{X: sel.Items[i].X}}
		out[i] = Assign{c, &Subquery{Q: &one}}
	}
	return out, nil
}
