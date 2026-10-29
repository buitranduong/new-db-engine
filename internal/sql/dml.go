package sql

import (
	"fmt"

	"stask/internal/core"
	"stask/internal/schema"
)

// DML beyond one table: INSERT ... SELECT, UPDATE ... FROM, DELETE ...
// USING and subqueries in UPDATE / DELETE. The rows to write are found by
// a query on the general executor, then written as the single-table
// statements write them.

func hasSubquery(x Expr) bool {
	found := false
	walk(x, func(e Expr) {
		switch e.(type) {
		case *Subquery, *Exists, *InSub, *AnyAll:
			found = true
		}
	})
	return found
}

func setHasSubquery(set []Assign) bool {
	for _, a := range set {
		if hasSubquery(a.X) {
			return true
		}
	}
	return false
}

func (s *Session) insertQuery(tx *core.Tx, st *Insert, params []Datum) (*Result, error) {
	res, err := s.runQuery(tx, st.Query, params)
	if err != nil {
		return nil, err
	}
	ins := *st
	ins.Query = nil
	ins.Rows = make([][]Expr, len(res.Rows))
	for i, r := range res.Rows {
		row := make([]Expr, len(r))
		for j, v := range r {
			row[j] = &Lit{V: v}
		}
		ins.Rows[i] = row
	}
	if len(ins.Rows) == 0 {
		return &Result{HasRows: st.Returning != nil, Tag: "INSERT 0 0"}, nil
	}
	return s.insert(tx, &ins, params)
}

// targetRows runs SELECT <target columns>, extra... FROM target [, from]
// WHERE where and returns each target row (once, first match) with its
// extra values.
func (s *Session) targetRows(tx *core.Tx, t *schema.Table, table, alias string, from FromNode, where Expr,
	extra []Expr, params []Datum) ([]schema.Row, [][]Datum, error) {
	qual := alias
	if qual == "" {
		qual = table
	}
	var rel FromNode = &FromTable{Name: table, Alias: alias}
	if from != nil {
		rel = &FromJoin{Kind: "cross", L: rel, R: from}
	}
	q := &Select{Rel: rel, Where: where}
	for _, c := range t.Columns {
		q.Items = append(q.Items, SelItem{X: &ColRef{Table: qual, Name: c.Name}})
	}
	for _, x := range extra {
		q.Items = append(q.Items, SelItem{X: x})
	}
	res, err := s.runQuery(tx, q, params)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var rows []schema.Row
	var vals [][]Datum
	nc := len(t.Columns)
	for _, r := range res.Rows {
		row := make(schema.Row, nc)
		for i, c := range t.Columns {
			if row[i], err = toValue(c.T, r[i]); err != nil {
				return nil, nil, err
			}
		}
		k := t.KeyOf(row)
		if seen[k] {
			continue
		}
		seen[k] = true
		rows = append(rows, row)
		vals = append(vals, r[nc:])
	}
	return rows, vals, nil
}

func (s *Session) updateGeneral(tx *core.Tx, t *schema.Table, st *Update, params []Datum) (*Result, error) {
	extra := make([]Expr, len(st.Set))
	cols := make([]int, len(st.Set))
	for i, a := range st.Set {
		ci, ok := t.Col(a.Col)
		if !ok {
			return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", a.Col, t.Name)
		}
		cols[i], extra[i] = ci, a.X
	}
	rows, vals, err := s.targetRows(tx, t, st.Table, st.Alias, st.From, st.Where, extra, params)
	if err != nil {
		return nil, err
	}
	sc := &scope{t: t}
	ctx := &evalCtx{sc: sc, params: params, now: nowMillis(), sess: s}
	res := &Result{HasRows: st.Returning != nil}
	if st.Returning != nil {
		res.Cols = returningCols(sc, st.Returning, paramKinds(st, sc))
	}
	for ri, old := range rows {
		upd := append(schema.Row(nil), old...)
		for i, ci := range cols {
			if upd[ci], err = toValue(t.Columns[ci].T, vals[ri][i]); err != nil {
				return nil, err
			}
		}
		if err := checkRow(t, upd); err != nil {
			return nil, err
		}
		if ok, nk := t.KeyOf(old), t.KeyOf(upd); ok != nk {
			if _, exists, err := tx.Get(t.Name, nk); err != nil {
				return nil, err
			} else if exists {
				return nil, errf(codeUniqueViolation, "duplicate key value violates unique constraint \"%s_pkey\": Key (%s)=(%s) already exists", t.Name, t.Key, nk)
			}
			if err := tx.Delete(t.Name, ok); err != nil {
				return nil, err
			}
			if err := tx.InsertAs(t, upd); err != nil {
				return nil, err
			}
		} else if err := tx.PutAs(t, upd); err != nil {
			return nil, err
		}
		if err := s.returning(sc, ctx, st.Returning, upd, res); err != nil {
			return nil, err
		}
	}
	res.Tag = fmt.Sprintf("UPDATE %d", len(rows))
	return res, nil
}

func (s *Session) deleteGeneral(tx *core.Tx, t *schema.Table, st *Delete, params []Datum) (*Result, error) {
	rows, _, err := s.targetRows(tx, t, st.Table, st.Alias, st.Using, st.Where, nil, params)
	if err != nil {
		return nil, err
	}
	sc := &scope{t: t}
	ctx := &evalCtx{sc: sc, params: params, now: nowMillis(), sess: s}
	res := &Result{HasRows: st.Returning != nil}
	if st.Returning != nil {
		res.Cols = returningCols(sc, st.Returning, paramKinds(st, sc))
	}
	for _, r := range rows {
		if err := tx.Delete(t.Name, t.KeyOf(r)); err != nil {
			return nil, err
		}
		if err := s.returning(sc, ctx, st.Returning, r, res); err != nil {
			return nil, err
		}
	}
	res.Tag = fmt.Sprintf("DELETE %d", len(rows))
	return res, nil
}
