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
	limit := QueryMemLimit
	var over atomic.Bool
	ok, err := qx.tx.ScanAllParallel(p.t.Name, nil, helpers, spawnHelper, keepHelper, func(w, seq, idx int, r schema.Row) bool {
		bs := buckets[w]
		if len(bs) == 0 || bs[len(bs)-1].seq != seq {
			bs = append(bs, scanBucket{seq: seq, rows: make([][]Datum, 0, 2048)})
		}
		b := &bs[len(bs)-1]
		d := make([]Datum, n)
		for i := range cols {
			d[i] = fromCol(&cols[i], r[i])
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
	_ = over.Load()
	return rows, true, nil
}
