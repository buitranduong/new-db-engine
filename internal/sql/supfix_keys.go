package sql

// supfix: structural keys for the expression kinds exprKey (exec.go) does not know, so that
// "SELECT CASE .. END, count(*) .. GROUP BY CASE .. END" matches the repeated expression
// (PostgreSQL matches by equal expression trees).

func stripPtrsMore(x Expr) (any, bool) {
	list := func(xs []Expr) []any {
		out := make([]any, 0, len(xs))
		for _, e := range xs {
			out = append(out, stripPtrs(e))
		}
		return out
	}
	switch e := x.(type) {
	case *Case:
		parts := []any{"case", stripPtrs(e.Operand)}
		for _, w := range e.Whens {
			parts = append(parts, stripPtrs(w.Cond), stripPtrs(w.Then))
		}
		return append(parts, stripPtrs(e.Else)), true
	case *IsNull:
		return []any{"isnull", e.Not, stripPtrs(e.X)}, true
	case *InList:
		return []any{"in", e.Not, stripPtrs(e.X), list(e.List)}, true
	case *Between:
		return []any{"between", e.Not, stripPtrs(e.X), stripPtrs(e.Lo), stripPtrs(e.Hi)}, true
	case *Like:
		return []any{"like", e.Not, e.ICase, stripPtrs(e.X), stripPtrs(e.Pat)}, true
	case *RowExpr:
		return []any{"row", list(e.Items)}, true
	}
	return nil, false
}
