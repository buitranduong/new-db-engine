package sql

// needsGeneral reports whether a SELECT uses anything the single-table
// fast path (selectRows, and the columnar views behind it) does not
// handle; those run on the general executor (query.go).
func needsGeneral(st *Select) bool {
	if virtualTables[st.From] {
		return true
	}
	if st.Rel != nil || len(st.With) > 0 || st.Sets != nil || st.DistinctOn != nil || st.LimitBy != nil || len(st.Windows) > 0 {
		return true
	}
	for _, o := range st.OrderBy {
		if o.Nulls != 0 {
			return true
		}
	}
	for _, g := range st.GroupBy {
		if l, ok := g.(*Lit); ok && l.V.K == KInt {
			return true // GROUP BY ordinal
		}
		if cr, ok := g.(*ColRef); ok && cr.Table == "" {
			for _, it := range st.Items {
				if it.Alias == cr.Name {
					if c, isCol := it.X.(*ColRef); !isCol || c.Name != cr.Name {
						return true // GROUP BY a select alias
					}
				}
			}
		}
	}
	found := false
	check := func(x Expr) {
		walk(x, func(e Expr) {
			switch f := e.(type) {
			case *Subquery, *Exists, *InSub, *AnyAll, *RowExpr:
				found = true
			case *Func:
				if f.Over != nil || f.Filter != nil || f.Order != nil || f.Within != nil || f.Params != nil ||
					aggNames[f.Name] && !aggFuncs[f.Name] || f.Name == "grouping" || isSelectSRF(f.Name) || isJSONArrSRF(f.Name) || f.Name == "stask_srf" ||
					sessionFuncs[f.Name] {
					found = true
				}
			}
		})
	}
	for _, it := range st.Items {
		check(it.X)
	}
	check(st.Where)
	check(st.Having)
	for _, g := range st.GroupBy {
		check(g)
	}
	for _, o := range st.OrderBy {
		check(o.X)
	}
	check(st.Limit)
	check(st.Offset)
	return found
}

// retryGeneral reports whether a fast-path error may only mean the fast
// path lacks a feature (a name that is a select alias, say): the general
// executor then gets the statement.
func retryGeneral(err error) bool {
	e, ok := err.(*Error)
	if !ok {
		return false
	}
	switch e.Code {
	case codeUndefinedColumn, codeFeature, codeUndefinedFunction, codeDatatypeMismatch:
		return true
	}
	return false
}
