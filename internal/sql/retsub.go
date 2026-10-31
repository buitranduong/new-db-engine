package sql

import "stask/internal/core"

// returningSubqueries lets RETURNING hold a subquery that does not refer to
// the target's columns: each such item is run once, as a query of its own,
// before the statement writes (PostgreSQL gives RETURNING the statement's
// snapshot), and replaced by its value. An item that refers to the row
// stays as it is and is refused with 0A000 when it is evaluated.
func (s *Session) returningSubqueries(tx *core.Tx, items []SelItem, params []Datum) ([]SelItem, error) {
	var out []SelItem
	for i, it := range items {
		if it.X == nil || !hasSubquery(it.X) {
			continue
		}
		res, err := s.runQuery(tx, &Select{Items: []SelItem{{X: it.X}}}, params)
		if err != nil {
			switch AsError(err).Code {
			case codeUndefinedColumn, codeUndefinedTable:
				continue // refers to the row
			}
			return nil, err
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
			continue
		}
		if out == nil {
			out = append([]SelItem(nil), items...)
		}
		name := itemName(it)
		out[i] = SelItem{X: &Lit{V: res.Rows[0][0]}, Alias: name}
	}
	if out == nil {
		return items, nil
	}
	return out, nil
}
