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
			if row[i], err = toValueCol(&c, r[i]); err != nil {
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

// foreignRefs are the column references of a RETURNING list that name no
// column of the target table: they belong to the FROM / USING tables and
// are evaluated by the query that finds the rows.
func foreignRefs(items []SelItem, t *schema.Table, qual string) []*ColRef {
	var out []*ColRef
	for _, it := range items {
		walk(it.X, func(e Expr) {
			c, ok := e.(*ColRef)
			if !ok {
				return
			}
			if c.Table == "" || c.Table == qual || c.Table == t.Name {
				if _, ok := t.Col(c.Name); ok || c.Table != "" {
					return
				}
			}
			out = append(out, c)
		})
	}
	return out
}

// bindForeign replaces the foreign references of items by the values the
// query found for the row.
func bindForeign(items []SelItem, refs []*ColRef, vals []Datum) []SelItem {
	if len(refs) == 0 {
		return items
	}
	out := make([]SelItem, len(items))
	for i, it := range items {
		out[i] = it
		if it.X == nil {
			continue
		}
		out[i].X, _ = rebuildDeep(it.X, func(e Expr) (Expr, bool) {
			if c, ok := e.(*ColRef); ok {
				for j, r := range refs {
					if r == c {
						return &Lit{V: vals[j]}, true
					}
				}
			}
			return nil, false
		})
	}
	return out
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
	qual := st.Alias
	if qual == "" {
		qual = st.Table
	}
	fx := foreignRefs(st.Returning, t, qual)
	for _, r := range fx {
		extra = append(extra, r)
	}
	var rec *repairRec // occ_repair.go: the join values of SET and WHERE, to evaluate them again
	if r := s.rep; r != nil && r.st == Stmt(st) {
		rec = r
		for _, ref := range r.foreignOf(t, qual, setExprs(st.Set), st.Where) {
			extra = append(extra, ref)
		}
	}
	var rows []schema.Row
	var vals [][]Datum
	err := s.lockedRows(tx, t, updateLockMode(t, st.Set), func() ([]schema.Row, error) {
		var e error
		rows, vals, e = s.targetRows(tx, t, st.Table, st.Alias, st.From, st.Where, extra, params)
		return rows, e
	})
	if err != nil {
		return nil, err
	}
	sc := &scope{t: t, alias: st.Alias}
	ctx := &evalCtx{sc: sc, params: params, now: nowMicros(), sess: s}
	tgs := s.E.Triggers(t.Name)
	res := &Result{HasRows: st.Returning != nil}
	if st.Returning != nil {
		res.Cols = returningCols(sc, st.Returning, paramKinds(st, sc))
	}
	skipped := 0
	for ri, old := range rows {
		upd := append(schema.Row(nil), old...)
		for i, ci := range cols {
			if upd[ci], err = toValueCol(&t.Columns[ci], vals[ri][i]); err != nil {
				return nil, err
			}
		}
		if err := checkRow(t, upd); err != nil {
			return nil, err
		}
		if upd, err = s.updateRow(tx, t, tgs, old, upd); err != nil {
			return nil, err
		}
		if upd == nil { // skipped by a BEFORE trigger
			skipped++
			continue
		}
		if rec != nil {
			rec.keep(t.KeyOf(old), vals[ri][len(cols)+len(fx):])
		}
		if err := s.returning(sc, ctx, bindForeign(st.Returning, fx, vals[ri][len(cols):]), upd, res); err != nil {
			return nil, err
		}
	}
	res.Tag = fmt.Sprintf("UPDATE %d", len(rows)-skipped)
	return res, nil
}

func (s *Session) deleteGeneral(tx *core.Tx, t *schema.Table, st *Delete, params []Datum) (*Result, error) {
	qual := st.Alias
	if qual == "" {
		qual = st.Table
	}
	fx := foreignRefs(st.Returning, t, qual)
	extra := make([]Expr, len(fx))
	for i, r := range fx {
		extra[i] = r
	}
	var rec *repairRec // occ_repair.go
	if r := s.rep; r != nil && r.st == Stmt(st) {
		rec = r
		for _, ref := range r.foreignOf(t, qual, st.Where) {
			extra = append(extra, ref)
		}
	}
	var rows []schema.Row
	var vals [][]Datum
	err := s.lockedRows(tx, t, lmUpdate, func() ([]schema.Row, error) {
		var e error
		rows, vals, e = s.targetRows(tx, t, st.Table, st.Alias, st.Using, st.Where, extra, params)
		return rows, e
	})
	if err != nil {
		return nil, err
	}
	sc := &scope{t: t, alias: st.Alias}
	ctx := &evalCtx{sc: sc, params: params, now: nowMicros(), sess: s}
	tgs := s.E.Triggers(t.Name)
	res := &Result{HasRows: st.Returning != nil}
	if st.Returning != nil {
		res.Cols = returningCols(sc, st.Returning, paramKinds(st, sc))
	}
	skipped := 0
	for ri, r := range rows {
		done, err := s.deleteRow(tx, t, tgs, r)
		if err != nil {
			return nil, err
		}
		if !done {
			skipped++
			continue
		}
		if rec != nil {
			rec.keep(t.KeyOf(r), vals[ri][len(fx):])
		}
		if err := s.returning(sc, ctx, bindForeign(st.Returning, fx, vals[ri]), r, res); err != nil {
			return nil, err
		}
	}
	res.Tag = fmt.Sprintf("DELETE %d", len(rows)-skipped)
	return res, nil
}
