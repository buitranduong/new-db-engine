package sql

import (
	"fmt"
	"strconv"

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
	var right FromNode = &FromTable{Name: st.Table, Alias: st.Alias}
	if keys, ok := s.mergeKeys(tx, t, qual, st, params); ok {
		right = &FromKeyed{Name: st.Table, Alias: st.Alias, Keys: keys}
	}
	q := &Select{Rel: &FromJoin{Kind: "left", L: st.Source, R: right, On: st.On}}
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
				done, err := s.deleteRow(tx, t, tgs, old)
				if err != nil {
					return nil, err
				}
				if done {
					n++
				}
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
			if done, err := s.updateRow(tx, t, tgs, old, upd); err != nil {
				return nil, err
			} else if done != nil {
				n++
			}
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
				var skip bool
				if row, skip, err = s.fireTriggers(tx, t, tgs, "before", "insert", nil, row); err != nil {
					return nil, err
				} else if skip {
					continue
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
				if _, _, err := s.fireTriggers(tx, t, tgs, "after", "insert", nil, row); err != nil {
					return nil, err
				}
			}
			n++
		}
	}
	return &Result{Tag: fmt.Sprintf("MERGE %d", n)}, nil
}

// ---- keyed probe ----
//
// When ON has a conjunct "target.pk = X" with X reading only the source, the
// target rows that can match are those whose key is one of the values of X.
// The join then reads just those rows by key (point reads) instead of the
// whole target table: for 200k source rows and a 1M-row target the full scan
// was most of the time of the statement.

// FromKeyed is a FROM item that reads the rows of a table with the given
// primary keys (made by MERGE only; never parsed).
type FromKeyed struct {
	Name, Alias string
	Keys        []string
}

func (*FromKeyed) fromNode() {}

type keyedPlan struct {
	t    *schema.Table
	c    []relCol
	keys []string
}

func (p *keyedPlan) cols() []relCol { return p.c }
func (p *keyedPlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	rows := make([][]Datum, 0, len(p.keys))
	for i, k := range p.keys {
		if i&1023 == 1023 && qx.s.interrupted() {
			return nil, core.ErrInterrupted
		}
		r, ok, err := qx.tx.Get(p.t.Name, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		d := make([]Datum, len(p.t.Columns))
		for j := range p.t.Columns {
			d[j] = fromCol(&p.t.Columns[j], r[j])
		}
		if err := qx.charge(d); err != nil {
			return nil, err
		}
		rows = append(rows, d)
	}
	return rows, nil
}

// compileKeyed plans a FromKeyed (the hook of compileFrom).
func (c *compiler) compileKeyed(f *FromKeyed) (fromPlan, error) {
	t, err := c.s.table(f.Name)
	if err != nil {
		return nil, err
	}
	c.noteDep(f.Name)
	qual := f.Alias
	if qual == "" {
		qual = f.Name
	}
	p := &keyedPlan{t: t, keys: f.Keys}
	for i, col := range t.Columns {
		p.c = append(p.c, relCol{qual: qual, name: col.Name, kind: colKind(&t.Columns[i]), ty: colTy(&t.Columns[i]), sys: col.Hidden})
	}
	return p, nil
}

// mergeKeys finds the keys the target rows that ON can match have, or
// ok=false when ON has no usable "target.pk = X" conjunct.
func (s *Session) mergeKeys(tx *core.Tx, t *schema.Table, qual string, st *Merge, params []Datum) (keys []string, ok bool) {
	if !t.HasPK() || len(t.KeyCols) > 1 || t.Key == schema.RowIDCol || t.Key == schema.KeyCol {
		return nil, false
	}
	var conj []Expr
	var split func(x Expr)
	split = func(x Expr) {
		if b, isB := x.(*Binary); isB && b.Op == "and" {
			split(b.L)
			split(b.R)
			return
		}
		conj = append(conj, x)
	}
	split(st.On)
	isKey := func(x Expr) bool {
		c, isC := x.(*ColRef)
		return isC && c.Name == t.Key && c.Table == qual
	}
	sourceOnly := func(x Expr) bool {
		good := true
		walk(x, func(e Expr) {
			switch e := e.(type) {
			case *ColRef:
				if e.Table == "" || e.Table == qual {
					good = false
				}
			case *Func:
				if aggFuncs[e.Name] || e.Over != nil {
					good = false
				}
			case *Subquery, *Exists, *InSub, *AnyAll:
				good = false
			}
		})
		return good
	}
	var x Expr
	for _, c := range conj {
		b, isB := c.(*Binary)
		if !isB || b.Op != "=" {
			continue
		}
		switch {
		case isKey(b.L) && sourceOnly(b.R):
			x = b.R
		case isKey(b.R) && sourceOnly(b.L):
			x = b.L
		}
		if x != nil {
			break
		}
	}
	if x == nil {
		return nil, false
	}
	res, err := s.runQuery(tx, &Select{Distinct: true, Items: []SelItem{{X: x}}, Rel: st.Source}, params)
	if err != nil || len(res.Rows) == 0 {
		return nil, false // let the full plan report the error
	}
	keyKind := kindOf(t.Columns[t.KeyIndex()].T)
	seen := make(map[string]struct{}, len(res.Rows))
	for _, r := range res.Rows {
		v := r[0]
		if v.IsNull() {
			continue
		}
		cv, err := coerce(v, keyKind)
		if err != nil {
			return nil, false
		}
		k := cv.S
		if keyKind == KInt {
			k = strconv.FormatInt(cv.I, 10)
		}
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	return keys, true
}
