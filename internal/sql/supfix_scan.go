package sql

import (
	"sort"
	"sync/atomic"

	"stask/internal/schema"
)

// supfix: the general executor materializes a table with a parallel scan (the elastic helpers of
// lanes.go decode rows and build Datums on several cores), instead of one core decoding 260k rows
// (docs/14 Appendix E item 6: the shadow-mode query reserved_sold_have_items spent most of its time
// there). Row order is the sequential scan's. A table with own uncommitted writes, a small table or
// a lane without spare cores keeps the sequential scan.

const parallelScanMinRows = 20000

type scanBucket struct {
	seq  int
	rows [][]Datum
}

// runParallel returns ok=false when the scan should run sequentially.
func (p *tablePlan) runParallel(qx *qexec) ([][]Datum, bool, error) {
	return p.runParallelWhere(qx, nil, nil)
}

// pushableWhere: a WHERE the scan helpers may evaluate themselves (no subquery, no aggregate or
// window, no function with side effects or per-statement state): the same expression the
// caller would run row by row afterwards.
func pushableWhere(x Expr) bool {
	ok := true
	walk(x, func(e Expr) {
		switch e := e.(type) {
		case *Lit, *Param, *Slot, *Unary, *Binary, *IsNull, *InList, *Between, *Like, *Cast, *Case:
		case *Func:
			if e.Over != nil || isAggCall(e) || !pushableFuncs[e.Name] {
				ok = false
			}
		default:
			ok = false
		}
	})
	return ok
}

var pushableFuncs = map[string]bool{"lower": true, "upper": true, "length": true, "md5": true, "abs": true, "coalesce": true,
	"substr": true, "substring": true, "left": true, "right": true, "btrim": true, "trim": true}

// runParallelWhere is runParallel keeping only the rows where evaluates true; the helpers evaluate it,
// so a selective filter on a large table neither holds the whole table in memory nor runs on one core.
// The caller must not apply where again when ok.
func (p *tablePlan) runParallelWhere(qx *qexec, ctx *evalCtx, where Expr) ([][]Datum, bool, error) {
	if qx.tx == nil || qx.s == nil || qx.s.E == nil {
		return nil, false, nil
	}
	helpers := min(ScanWorkers-1, int(scanHelpers.Load()))
	if helpers < 1 {
		return nil, false, nil
	}
	ts, err := qx.s.E.Table(p.t.Name)
	if err != nil || ts.LiveRows() < parallelScanMinRows {
		return nil, false, nil
	}
	cols := p.t.Columns
	n := len(cols)
	buckets := make([][]scanBucket, helpers+1)
	var held atomic.Int64
	limit := qx.limit()
	var over atomic.Bool
	var wctx []evalCtx
	var scratch [][]Datum
	var werr []error
	var used []int
	if where != nil {
		seen := make([]bool, n)
		walk(where, func(e Expr) {
			if sl, ok := e.(*Slot); ok && sl.I >= 0 && sl.I < n && !seen[sl.I] {
				seen[sl.I] = true
				used = append(used, sl.I)
			}
		})
		wctx, scratch, werr = make([]evalCtx, helpers+1), make([][]Datum, helpers+1), make([]error, helpers+1)
		for w := range wctx {
			wctx[w] = evalCtx{params: ctx.params, now: ctx.now, qx: qx, outer: ctx.outer}
			scratch[w] = make([]Datum, n)
		}
	}
	ok, err := qx.tx.ScanAllParallel(p.t.Name, nil, helpers, spawnHelper, keepHelper, func(w, seq, idx int, r schema.Row) bool {
		var d []Datum
		if where != nil {
			sc := scratch[w]
			for _, i := range used { // only the columns WHERE reads: the rest is converted for the rows that stay
				sc[i] = fromCol(&cols[i], r[i])
			}
			wctx[w].drow = sc
			v, err := wctx[w].evalBool(where)
			if err != nil {
				werr[w] = err
				return false
			}
			if !v.Bool() {
				return true
			}
			d = make([]Datum, n)
			for i := range cols {
				d[i] = fromCol(&cols[i], r[i])
			}
		}
		bs := buckets[w]
		if len(bs) == 0 || bs[len(bs)-1].seq != seq {
			bs = append(bs, scanBucket{seq: seq, rows: make([][]Datum, 0, 2048)})
		}
		b := &bs[len(bs)-1]
		if d == nil {
			d = make([]Datum, n)
			for i := range cols {
				d[i] = fromCol(&cols[i], r[i])
			}
		}
		b.rows = append(b.rows, d)
		buckets[w] = bs
		if limit > 0 && held.Add(rowBytes(d)) > limit {
			over.Store(true)
			return false
		}
		return true
	})
	if err != nil {
		return nil, true, err
	}
	if !ok {
		return nil, false, nil
	}
	for _, e := range werr {
		if e != nil {
			return nil, true, e
		}
	}
	if over.Load() {
		// The helpers stopped at the budget, so what they collected is a PREFIX of the table.
		// qx.chargeAll below reserves memory in 1 MB steps and can accept that prefix, which
		// silently dropped the tail of the table (a 10M-row scan returned 8.63M rows). Fail
		// like the sequential scan does.
		return nil, true, qx.overBudget(limit)
	}
	var all []scanBucket
	total := 0
	for _, bs := range buckets {
		for _, b := range bs {
			all = append(all, b)
			total += len(b.rows)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	rows := make([][]Datum, 0, total)
	for _, b := range all {
		rows = append(rows, b.rows...)
	}
	if err := qx.chargeAll(rows); err != nil {
		return nil, true, err
	}
	return rows, true, nil
}
