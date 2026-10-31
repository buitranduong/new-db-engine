package sql

import "stask/internal/core"

// valuesSubqueries makes INSERT ... VALUES (..., (SELECT ...), ...) work:
// a VALUES expression cannot refer to the target's columns, so each
// expression that holds a subquery is run once, as a query of its own, and
// replaced by its value. The statement is copied, not changed (a prepared
// statement runs again).
func (s *Session) valuesSubqueries(tx *core.Tx, st *Insert, params []Datum) (*Insert, error) {
	var out *Insert
	copied := map[int]bool{}
	for ri, row := range st.Rows {
		for xi, x := range row {
			if x == nil || !hasSubquery(x) {
				continue
			}
			if out == nil {
				c := *st
				c.Rows = make([][]Expr, len(st.Rows))
				copy(c.Rows, st.Rows)
				out = &c
			}
			if !copied[ri] {
				out.Rows[ri] = append([]Expr(nil), row...)
				copied[ri] = true
			}
			res, err := s.runQuery(tx, &Select{Items: []SelItem{{X: x}}}, params)
			if err != nil {
				return nil, err
			}
			var v Datum
			switch len(res.Rows) {
			case 0:
				v = Null
			case 1:
				v = res.Rows[0][0]
			default:
				return nil, errf(codeCardinality, "more than one row returned by a subquery used as an expression")
			}
			out.Rows[ri][xi] = &Lit{V: v}
		}
	}
	if out == nil {
		return st, nil
	}
	return out, nil
}
