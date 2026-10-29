package sql

import "sync/atomic"

// Hash semi-join / anti-join for correlated EXISTS and IN subqueries
// (docs/14 SG rows, item 2).
//
// `x EXISTS (SELECT .. FROM s WHERE s.k = t.k AND <inner-only filters>)` was
// re-run for every outer row (nested loop, >60 s for 50k x 200k rows). When
// the correlation is a conjunction of equalities between an expression of the
// subquery's own tables and an expression of the enclosing query, the
// subquery is run once without the correlation conjuncts, projecting the
// inner key expressions, and the outer row looks its key up in a hash table:
//
//	EXISTS      -> hit            NOT EXISTS -> miss        (semi / anti join)
//	x IN  (..)  -> per-key set    x NOT IN   -> per-key set (NULL rules kept)
//
// SQL semantics kept: a NULL key never equals anything, so a NULL on either
// side is a miss (EXISTS false, NOT EXISTS true); x [NOT] IN (subquery rows
// of the same key) follows three-valued logic (a NULL x or a NULL among the
// rows makes a non-match unknown, never TRUE for NOT IN). Anything the
// rewrite cannot prove equivalent (non-equality correlation, aggregates,
// LIMIT, DISTINCT, GROUP BY, window, CTE, a key whose runtime kind class
// differs between the two sides) keeps the nested-loop path, which stays as
// the fallback of every rewritten subquery.

type hashSub struct {
	exists map[string]struct{}
	sets   map[string]*inSet
	cls    []Kind // kind class of each inner key column (KNull: not seen yet)
	valCls Kind   // kind class of the IN value column
	mixed  bool   // a column held two kind classes: use the nested path
}

func kindClass(k Kind) Kind {
	switch k {
	case KInt, KFloat, KNumeric:
		return KFloat
	case KDate:
		return KTime
	}
	return k
}

func splitConj(x Expr, out []Expr) []Expr {
	if b, ok := x.(*Binary); ok && b.Op == "and" {
		return splitConj(b.R, splitConj(b.L, out))
	}
	if x != nil {
		out = append(out, x)
	}
	return out
}

func joinAnd(xs []Expr) Expr {
	var r Expr
	for _, x := range xs {
		if r == nil {
			r = x
		} else {
			r = &Binary{Op: "and", L: r, R: x}
		}
	}
	return r
}

// decorrelate tries the rewrite of subquery q (EXISTS: val nil; IN: val is
// the left side). It returns the bound subquery or ok=false.
// NoDecorrelate turns the rewrite off (tests compare both paths).
var NoDecorrelate bool

// decorrelated counts subqueries rewritten to the hash form (tests, EXPLAIN).
var decorrelated atomic.Int64

func (b *binder) decorrelate(orig *boundSub, q Stmt, lhs Expr) (*boundSub, bool) {
	sel, ok := q.(*Select)
	if NoDecorrelate || !ok || sel.Distinct || len(sel.GroupBy) > 0 || sel.Having != nil || sel.Limit != nil || sel.Offset != nil ||
		len(sel.With) > 0 || len(sel.Sets) > 0 || len(sel.DistinctOn) > 0 || sel.LimitBy != nil || len(sel.Windows) > 0 ||
		sel.ForUpd || sel.Where == nil || (sel.From == "" && sel.Rel == nil) {
		return nil, false
	}
	for _, it := range sel.Items {
		if it.Star && lhs != nil || it.X != nil && hasAggOrWin(it.X) {
			return nil, false
		}
	}
	if lhs != nil {
		if len(sel.Items) != 1 || sel.Items[0].Star {
			return nil, false
		}
		if _, isRow := lhs.(*RowExpr); isRow {
			return nil, false
		}
	}
	innerOK := func(x Expr) bool {
		p, err := b.c.compile(&Select{Items: []SelItem{{X: x}}, From: sel.From, Alias: sel.Alias, Rel: sel.Rel}, b.sc)
		return err == nil && !p.isCorrelated()
	}
	outerOK := func(x Expr) bool {
		if hasSubOrAgg(x) {
			return false
		}
		p, err := b.c.compile(&Select{Items: []SelItem{{X: x}}}, b.sc)
		return err == nil && p.isCorrelated()
	}
	var inner, outer, rest []Expr
	for _, cj := range splitConj(sel.Where, nil) {
		if e, ok := cj.(*Binary); ok && e.Op == "=" {
			switch {
			case innerOK(e.L) && outerOK(e.R):
				inner, outer = append(inner, e.L), append(outer, e.R)
				continue
			case innerOK(e.R) && outerOK(e.L):
				inner, outer = append(inner, e.R), append(outer, e.L)
				continue
			}
		}
		rest = append(rest, cj)
	}
	if len(inner) == 0 {
		return nil, false
	}
	items := make([]SelItem, 0, len(inner)+1)
	for _, x := range inner {
		items = append(items, SelItem{X: x})
	}
	if lhs != nil {
		items = append(items, SelItem{X: sel.Items[0].X})
	}
	res := &Select{Items: items, From: sel.From, Alias: sel.Alias, Rel: sel.Rel, Where: joinAnd(rest)}
	plan, err := b.c.compile(res, b.sc)
	if err != nil || plan.isCorrelated() {
		return nil, false
	}
	hs := &boundSub{kind: orig.kind, plan: plan, not: orig.not, fallback: orig}
	for _, x := range outer {
		bx, err := b.bind(x)
		if err != nil {
			return nil, false
		}
		hs.corr = append(hs.corr, bx)
	}
	if lhs != nil {
		bx, err := b.bind(lhs)
		if err != nil {
			return nil, false
		}
		hs.x = bx
	}
	decorrelated.Add(1)
	return hs, true
}

func (c *evalCtx) evalHashSub(b *boundSub) (Datum, error) {
	qx := c.qx
	if qx.hashSubs == nil {
		qx.hashSubs = map[*boundSub]*hashSub{}
	}
	nk := len(b.corr)
	h := qx.hashSubs[b]
	if h == nil {
		rel, err := b.plan.run(qx, c)
		if err != nil {
			return Null, err
		}
		h = &hashSub{cls: make([]Kind, nk)}
		if b.kind == "exists" {
			h.exists = make(map[string]struct{}, len(rel.rows))
		} else {
			h.sets = map[string]*inSet{}
		}
		var kb []byte
		for _, row := range rel.rows {
			key := row[:nk]
			null := false
			for i, v := range key {
				if v.IsNull() {
					null = true
					continue
				}
				if k := kindClass(v.K); h.cls[i] == KNull {
					h.cls[i] = k
				} else if h.cls[i] != k {
					h.mixed = true
				}
			}
			if null {
				continue
			}
			kb = appendGroupKey(kb[:0], normKeys(key))
			if h.exists != nil {
				h.exists[string(kb)] = struct{}{}
				continue
			}
			set := h.sets[string(kb)]
			if set == nil {
				set = &inSet{keys: map[string]bool{}}
				h.sets[string(kb)] = set
			}
			v := row[nk]
			if v.IsNull() {
				set.hasNull = true
				continue
			}
			if k := kindClass(v.K); h.valCls == KNull {
				h.valCls = k
			} else if h.valCls != k {
				h.mixed = true
			}
			set.keys[groupKey(normKeys([]Datum{v}))] = true
		}
		qx.hashSubs[b] = h
	}
	if h.mixed {
		return c.evalSub(b.fallback)
	}
	ck := make([]Datum, nk)
	for i, x := range b.corr {
		v, err := c.eval(x)
		if err != nil {
			return Null, err
		}
		if !v.IsNull() && h.cls[i] != KNull && kindClass(v.K) != h.cls[i] {
			return c.evalSub(b.fallback) // kinds the hash cannot compare: nested semantics
		}
		ck[i] = v
	}
	miss := false
	for _, v := range ck {
		miss = miss || v.IsNull()
	}
	var set *inSet
	if !miss && h.exists == nil {
		if lv, err := c.eval(b.x); err == nil && !lv.IsNull() && h.valCls != KNull && kindClass(lv.K) != h.valCls {
			return c.evalSub(b.fallback)
		}
	}
	if !miss {
		key := groupKey(normKeys(ck))
		if h.exists != nil {
			_, hit := h.exists[key]
			return BoolD(hit), nil
		}
		set = h.sets[key]
	} else if h.exists != nil {
		return BoolD(false), nil
	}
	// IN / NOT IN over the rows of this key
	lhs, err := c.eval(b.x)
	if err != nil {
		return Null, err
	}
	if set == nil { // no rows for this key: IN is false, NOT IN true, whatever lhs is
		return BoolD(b.not), nil
	}
	if lhs.IsNull() {
		return Null, nil
	}
	if set.keys[groupKey(normKeys([]Datum{lhs}))] {
		return BoolD(!b.not), nil
	}
	if set.hasNull {
		return Null, nil
	}
	return BoolD(b.not), nil
}
