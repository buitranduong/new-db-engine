package sql

import "stask/internal/core"

// supfix: more correlated EXISTS / IN subqueries run as a hash semi-join (docs/14 Appendix E item 6),
// beyond semijoin.go (which needs equality correlation and a plain SELECT):
//
//   - residual predicates that mention the outer row besides the equalities
//     (EXISTS (.. WHERE i.number = s.number AND i.price > s.price)): the inner rows are hashed by
//     the equality key once, and each outer row tests only the rows of its bucket;
//   - GROUP BY / HAVING inside the subquery: the subquery runs once grouped by its own groups plus
//     the correlation keys, and HAVING filters the groups; an aggregate without GROUP BY but with
//     HAVING is grouped by the keys, and a key with no inner rows takes the HAVING value of an empty
//     input (PostgreSQL gives such a subquery one row);
//   - DISTINCT, ORDER BY and LIMIT n (n >= 1) inside EXISTS, which do not change its answer.
//
// Anything else keeps the nested-loop path, which is also the fallback of every rewritten subquery
// (mixed key kinds). Semantics checked against PostgreSQL 16 in scripts/pgdiff/h_supfix.sql.

// extSub is the extra state of a boundSub rewritten by this file.
type extSub struct {
	k       *selPlan // residual form: scan of the inner relation (items = key expressions, where = inner-only filters)
	resid   Expr     // residual predicate over k's from row and the outer row (OuterRef)
	empty   qplan    // HAVING of an aggregate without GROUP BY, evaluated on an empty input (nil: not needed)
	present qplan    // the distinct keys that have inner rows at all (same case)
}

type extState struct {
	exists  map[string]struct{}
	present map[string]struct{}
	buckets map[string][][]Datum
	cls     []Kind
	mixed   bool

	emptyHit, emptyRun bool // HAVING on an empty input, evaluated once per execution
}

func (b *binder) supfixDecorrelate(orig *boundSub, q Stmt, lhs Expr) (*boundSub, bool, bool) {
	sel, ok := q.(*Select)
	if NoDecorrelate || !ok || len(sel.With) > 0 || len(sel.Sets) > 0 || len(sel.DistinctOn) > 0 || sel.LimitBy != nil ||
		len(sel.Windows) > 0 || sel.ForUpd || sel.Where == nil || (sel.From == "" && sel.Rel == nil) {
		return nil, false, false
	}
	exists := lhs == nil
	// LIMIT n (n >= 1) does not change whether an EXISTS subquery has a row
	limitOK := sel.Limit == nil && sel.Offset == nil
	if l, isLit := sel.Limit.(*Lit); isLit && exists && sel.Offset == nil && l.V.K == KInt && l.V.I >= 1 {
		limitOK = true
	}
	if !limitOK {
		return nil, false, false
	}
	grouped := len(sel.GroupBy) > 0 || sel.Having != nil
	aggItems := false
	for _, it := range sel.Items {
		if it.Star && lhs != nil {
			return nil, false, false
		}
		if it.X != nil && hasAggOrWin(it.X) {
			aggItems = true
		}
	}
	implicit := !grouped && aggItems
	if implicit {
		return nil, false, false // always one row (EXISTS) or a NULL row per empty key (IN): nested path
	}
	if lhs != nil {
		if len(sel.Items) != 1 || sel.Items[0].Star {
			return nil, false, false
		}
		if _, isRow := lhs.(*RowExpr); isRow {
			return nil, false, false
		}
	}
	implicitHaving := len(sel.GroupBy) == 0 && sel.Having != nil
	if implicitHaving && !exists {
		return nil, false, false
	}
	// positions and aliases in GROUP BY refer to the select list we replace
	for _, g := range sel.GroupBy {
		if l, isLit := g.(*Lit); isLit && l.V.K == KInt {
			return nil, false, false
		}
		if c, isCol := g.(*ColRef); isCol && c.Table == "" {
			for _, it := range sel.Items {
				if it.Alias == c.Name {
					return nil, false, false
				}
			}
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
	var inner, outer, restIn, restOut []Expr
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
		if innerOK(cj) {
			restIn = append(restIn, cj)
		} else {
			restOut = append(restOut, cj)
		}
	}
	if len(inner) == 0 {
		return nil, false, false
	}
	residual := len(restOut) > 0
	plainShape := !sel.Distinct && sel.Limit == nil && !grouped
	if !residual && plainShape {
		return nil, false, false // semijoin.go handles it
	}
	if residual && (grouped || aggItems || !exists) {
		return nil, true, false
	}
	boundKeys := func() ([]Expr, bool) {
		var out []Expr
		for _, x := range outer {
			bx, err := b.bind(x)
			if err != nil {
				return nil, false
			}
			out = append(out, bx)
		}
		return out, true
	}
	if residual {
		items := make([]SelItem, 0, len(inner))
		for _, x := range inner {
			items = append(items, SelItem{X: x})
		}
		kp, err := b.c.compile(&Select{Items: items, From: sel.From, Alias: sel.Alias, Rel: sel.Rel, Where: joinAnd(restIn)}, b.sc)
		if err != nil || kp.isCorrelated() {
			return nil, true, false
		}
		ksel, isSel := kp.(*selPlan)
		if !isSel || ksel.from == nil || ksel.grouped || len(ksel.wins) > 0 || len(ksel.srfs) > 0 {
			return nil, true, false
		}
		rp, err := b.c.compile(&Select{Items: []SelItem{{X: joinAnd(restOut)}}, From: sel.From, Alias: sel.Alias, Rel: sel.Rel}, b.sc)
		if err != nil {
			return nil, true, false
		}
		rsel, isSel := rp.(*selPlan)
		if !isSel || len(rsel.items) != 1 || len(rsel.from.cols()) != len(ksel.from.cols()) {
			return nil, true, false
		}
		corr, ok := boundKeys()
		if !ok {
			return nil, true, false
		}
		hs := &boundSub{kind: orig.kind, plan: kp, not: orig.not, fallback: orig, corr: corr,
			ext: &extSub{k: ksel, resid: rsel.items[0]}}
		decorrelated.Add(1)
		return hs, true, true
	}
	// grouped / DISTINCT / LIMIT forms: one run of the subquery, grouped by its groups plus the keys
	gb := append([]Expr(nil), sel.GroupBy...)
	if grouped {
		seen := map[string]bool{}
		for _, g := range gb {
			seen[exprKey2(g)] = true
		}
		for _, x := range inner {
			if k := exprKey2(x); !seen[k] {
				seen[k] = true
				gb = append(gb, x)
			}
		}
	}
	items := make([]SelItem, 0, len(inner)+1)
	for _, x := range inner {
		items = append(items, SelItem{X: x})
	}
	if lhs != nil {
		items = append(items, SelItem{X: sel.Items[0].X})
	}
	res := &Select{Items: items, From: sel.From, Alias: sel.Alias, Rel: sel.Rel, Where: joinAnd(restIn)}
	if grouped {
		res.GroupBy, res.Having = gb, sel.Having
	}
	plan, err := b.c.compile(res, b.sc)
	if err != nil || plan.isCorrelated() {
		return nil, true, false
	}
	corr, ok := boundKeys()
	if !ok {
		return nil, true, false
	}
	hs := &boundSub{kind: orig.kind, plan: plan, not: orig.not, fallback: orig, corr: corr}
	if lhs != nil {
		bx, err := b.bind(lhs)
		if err != nil {
			return nil, true, false
		}
		hs.x = bx
	}
	if implicitHaving {
		ep, err := b.c.compile(&Select{Items: []SelItem{{X: &Lit{V: IntD(1)}}}, From: sel.From, Alias: sel.Alias, Rel: sel.Rel,
			Where: &Lit{V: BoolD(false)}, Having: sel.Having}, b.sc)
		if err != nil || ep.isCorrelated() {
			return nil, true, false
		}
		pitems := make([]SelItem, 0, len(inner))
		for _, x := range inner {
			pitems = append(pitems, SelItem{X: x})
		}
		pp, err := b.c.compile(&Select{Items: pitems, From: sel.From, Alias: sel.Alias, Rel: sel.Rel,
			Where: joinAnd(restIn), GroupBy: inner}, b.sc)
		if err != nil || pp.isCorrelated() {
			return nil, true, false
		}
		hs.ext = &extSub{empty: ep, present: pp}
	}
	decorrelated.Add(1)
	return hs, true, true
}

func (st *extState) emptyResult(e *extSub, qx *qexec, c *evalCtx) (bool, error) {
	if !st.emptyRun {
		rel, err := e.empty.run(qx, c)
		if err != nil {
			return false, err
		}
		st.emptyHit, st.emptyRun = len(rel.rows) > 0, true
	}
	return st.emptyHit, nil
}

// evalExtSub evaluates a boundSub rewritten by supfixDecorrelate that needs more than evalHashSub:
// the residual form and the HAVING-without-GROUP-BY form. (GROUP BY / DISTINCT / LIMIT forms use
// evalHashSub unchanged.)
func (c *evalCtx) evalExtSub(b *boundSub) (Datum, error) {
	qx := c.qx
	if qx.extSubs == nil {
		qx.extSubs = map[*boundSub]*extState{}
	}
	nk := len(b.corr)
	st := qx.extSubs[b]
	if st == nil {
		st = &extState{cls: make([]Kind, nk)}
		var kb []byte
		note := func(key []Datum) bool { // false: NULL in the key (never matches)
			null := false
			for i, v := range key {
				if v.IsNull() {
					null = true
					continue
				}
				if k := kindClass(v.K); st.cls[i] == KNull {
					st.cls[i] = k
				} else if st.cls[i] != k {
					st.mixed = true
				}
			}
			return !null
		}
		if b.ext.k != nil {
			k := b.ext.k
			rows, err := k.from.run(qx, c)
			if err != nil {
				return Null, err
			}
			st.buckets = make(map[string][][]Datum)
			ctx := qx.ctx(c)
			key := make([]Datum, nk)
			for i, r := range rows {
				if i&4095 == 4095 && qx.s.interrupted() {
					return Null, core.ErrInterrupted
				}
				ctx.drow = r
				if k.where != nil {
					v, err := ctx.evalBool(k.where)
					if err != nil {
						return Null, err
					}
					if !v.Bool() {
						continue
					}
				}
				for j := 0; j < nk; j++ {
					v, err := ctx.eval(k.items[j])
					if err != nil {
						return Null, err
					}
					key[j] = v
				}
				if !note(key) {
					continue
				}
				kb = appendGroupKey(kb[:0], normKeys(key))
				st.buckets[string(kb)] = append(st.buckets[string(kb)], r)
			}
		} else {
			rel, err := b.plan.run(qx, c)
			if err != nil {
				return Null, err
			}
			st.exists = make(map[string]struct{}, len(rel.rows))
			for _, row := range rel.rows {
				if !note(row[:nk]) {
					continue
				}
				kb = appendGroupKey(kb[:0], normKeys(row[:nk]))
				st.exists[string(kb)] = struct{}{}
			}
			if b.ext.present != nil {
				prel, err := b.ext.present.run(qx, c)
				if err != nil {
					return Null, err
				}
				st.present = make(map[string]struct{}, len(prel.rows))
				for _, row := range prel.rows {
					if !note(row[:nk]) {
						continue
					}
					kb = appendGroupKey(kb[:0], normKeys(row[:nk]))
					st.present[string(kb)] = struct{}{}
				}
			}
		}
		qx.extSubs[b] = st
	}
	if st.mixed {
		return c.evalSub(b.fallback)
	}
	ck := make([]Datum, nk)
	null := false
	for i, x := range b.corr {
		v, err := c.eval(x)
		if err != nil {
			return Null, err
		}
		if !v.IsNull() && st.cls[i] != KNull && kindClass(v.K) != st.cls[i] {
			return c.evalSub(b.fallback)
		}
		null = null || v.IsNull()
		ck[i] = v
	}
	if b.ext.k == nil { // HAVING without GROUP BY
		if !null {
			key := groupKey(normKeys(ck))
			if _, hit := st.exists[key]; hit {
				return BoolD(true), nil
			}
			if _, has := st.present[key]; has { // the key's group exists and fails HAVING
				return BoolD(false), nil
			}
		}
		eh, err := st.emptyResult(b.ext, qx, c)
		return BoolD(eh), err
	}
	if null {
		return BoolD(false), nil
	}
	cands := st.buckets[groupKey(normKeys(ck))]
	if len(cands) == 0 {
		return BoolD(false), nil
	}
	ctx := qx.ctx(c)
	for _, r := range cands {
		ctx.drow = r
		v, err := ctx.evalBool(b.ext.resid)
		if err != nil {
			return Null, err
		}
		if v.Bool() {
			return BoolD(true), nil
		}
	}
	return BoolD(false), nil
}
