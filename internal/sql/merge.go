package sql

import (
	"fmt"

	"stask/internal/core"
	"stask/internal/schema"
)

// MERGE (PostgreSQL 15+, docs/05 last section).
//
// MERGE INTO target USING source ON cond WHEN [NOT] MATCHED ... is planned
// as one query on the general executor, like UPDATE ... FROM:
//
//	SELECT target.*, target.key IS NOT NULL, <the expressions of the WHEN
//	clauses> FROM source LEFT JOIN target ON cond
//
// which yields, for every source row, each target row it matches (or the
// source row alone when it matches none). Each result row is then acted on
// by the first WHEN clause that fits it: UPDATE, DELETE, INSERT or nothing.
// A target row that two source rows both UPDATE or DELETE is the 21000
// error of PostgreSQL. The whole statement runs in one transaction, so a
// write conflict retries it as a whole (autocommit) like UPDATE ... FROM.
// Not supported: RETURNING, WHEN NOT MATCHED BY SOURCE (PostgreSQL 17).

// mergeClause is the result-column positions one WHEN clause reads.
type mergeClause struct {
	w       *MergeWhen
	cond    int   // column of the AND condition, -1: none
	set     []int // UPDATE: column of each assignment (-1: DEFAULT)
	setCols []int // UPDATE: target column of each assignment
	vals    []int // INSERT: column of each value (-1: DEFAULT)
	icols   []int // INSERT: target column of each value
}

func (s *Session) merge(tx *core.Tx, st *Merge, params []Datum) (*Result, error) {
	t, err := s.table(st.Table)
	if err != nil {
		return nil, err
	}
	if t.MatView != "" {
		return nil, errf(codeWrongObject, "cannot change materialized view %q", st.Table)
	}
	qual := st.Alias
	if qual == "" {
		qual = st.Table
	}
	nt := len(t.Columns)
	q := &Select{Rel: &FromJoin{Kind: "left", L: st.Source, R: &FromTable{Name: st.Table, Alias: st.Alias}, On: st.On}}
	for _, c := range t.Columns {
		q.Items = append(q.Items, SelItem{X: &ColRef{Table: qual, Name: c.Name}})
	}
	isMatched := Expr(&IsNull{X: &ColRef{Table: qual, Name: t.Key}, Not: true})
	q.Items = append(q.Items, SelItem{X: isMatched})
	// guard: an expression of a clause is only evaluated for the rows the clause can see
	guard := func(matched bool, x Expr) Expr {
		c := isMatched
		if !matched {
			c = &IsNull{X: &ColRef{Table: qual, Name: t.Key}}
		}
		return &Case{Whens: []When{{Cond: c, Then: x}}}
	}
	push := func(x Expr) int {
		q.Items = append(q.Items, SelItem{X: x})
		return len(q.Items) - 1
	}
	clauses := make([]*mergeClause, len(st.Whens))
	for i := range st.Whens {
		w := &st.Whens[i]
		mc := &mergeClause{w: w, cond: -1}
		clauses[i] = mc
		if w.And != nil {
			mc.cond = push(guard(w.Matched, w.And))
		}
		switch w.Action {
		case "update":
			for _, a := range w.Set {
				ci, ok := t.Col(a.Col)
				if !ok {
					return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", a.Col, st.Table)
				}
				mc.setCols = append(mc.setCols, ci)
				if _, def := a.X.(*DefaultExpr); def {
					mc.set = append(mc.set, -1)
				} else {
					mc.set = append(mc.set, push(guard(true, a.X)))
				}
			}
		case "insert":
			if w.Defaults {
				break
			}
			if w.Cols == nil {
				mc.icols = t.VisibleCols()
			} else {
				for _, cn := range w.Cols {
					ci, ok := t.Col(cn)
					if !ok {
						return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", cn, st.Table)
					}
					mc.icols = append(mc.icols, ci)
				}
			}
			if len(w.Values) > len(mc.icols) {
				return nil, errf(codeSyntax, "INSERT has more expressions than target columns")
			}
			if len(w.Values) < len(mc.icols) {
				return nil, errf(codeSyntax, "INSERT has more target columns than expressions")
			}
			for _, x := range w.Values {
				if _, def := x.(*DefaultExpr); def {
					mc.vals = append(mc.vals, -1)
				} else {
					mc.vals = append(mc.vals, push(guard(false, x)))
				}
			}
		}
	}
	res, err := s.runQuery(tx, q, params)
	if err != nil {
		return nil, err
	}
	sc := &scope{t: t, alias: st.Alias}
	ctx := &evalCtx{sc: sc, params: params, now: nowMicros(), sess: s}
	tgs := s.E.Triggers(t.Name)
	touched := map[string]bool{}
	n := 0
	for _, r := range res.Rows {
		matched := !r[nt].IsNull() && r[nt].I != 0
		var mc *mergeClause
		for _, c := range clauses {
			if c.w.Matched != matched {
				continue
			}
			if c.cond >= 0 {
				v := r[c.cond]
				if v.IsNull() || v.I == 0 {
					continue
				}
			}
			mc = c
			break
		}
		if mc == nil || mc.w.Action == "nothing" {
			continue
		}
		switch mc.w.Action {
		case "update", "delete":
			old := make(schema.Row, nt)
			for i := range t.Columns {
				if old[i], err = toValueCol(&t.Columns[i], r[i]); err != nil {
					return nil, err
				}
			}
			key := t.KeyOf(old)
			if touched[key] {
				return nil, errf("21000", "MERGE command cannot affect row a second time")
			}
			touched[key] = true
			if mc.w.Action == "delete" {
				if err := s.deleteRow(tx, t, tgs, old); err != nil {
					return nil, err
				}
				n++
				continue
			}
			upd := append(schema.Row(nil), old...)
			for j, ci := range mc.setCols {
				var d Datum
				if mc.set[j] < 0 {
					if d, err = s.columnDefault(t, ci, ctx.now); err != nil {
						return nil, err
					}
				} else {
					d = r[mc.set[j]]
				}
				if upd[ci], err = toValueCol(&t.Columns[ci], d); err != nil {
					return nil, err
				}
			}
			if err := checkRow(t, upd); err != nil {
				return nil, err
			}
			if t.KeyOf(upd) != key {
				return nil, errf(codeFeature, "MERGE ... UPDATE cannot change the primary key")
			}
			if _, err = s.updateRow(tx, t, tgs, old, upd); err != nil {
				return nil, err
			}
			n++
		case "insert":
			exprs := make([]Expr, len(mc.vals))
			for j, vi := range mc.vals {
				if vi < 0 {
					exprs[j] = &DefaultExpr{}
				} else {
					exprs[j] = &Lit{V: r[vi]}
				}
			}
			rc, vals, err := s.rowValues(t, mc.icols, exprs, ctx, mc.w.Overriding)
			if err != nil {
				return nil, err
			}
			row, err := s.buildRow(t, rc, vals)
			if err != nil {
				return nil, err
			}
			if tgs != nil {
				if row, err = s.fireTriggers(tx, t, tgs, "before", "insert", nil, row); err != nil {
					return nil, err
				}
			}
			key := t.KeyOf(row)
			if _, exists, err := tx.Get(t.Name, key); err != nil {
				return nil, err
			} else if exists {
				return nil, dupKeyErr(t, key)
			}
			if err := tx.InsertAs(t, row); err != nil {
				return nil, err
			}
			if tgs != nil {
				if _, err := s.fireTriggers(tx, t, tgs, "after", "insert", nil, row); err != nil {
					return nil, err
				}
			}
			n++
		}
	}
	return &Result{Tag: fmt.Sprintf("MERGE %d", n)}, nil
}
