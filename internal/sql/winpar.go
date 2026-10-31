package sql

import (
	"slices"
	"sync"
	"sync/atomic"

	"stask/internal/core"
)

// Parallel pieces of the window executor. They use the elastic scan helpers (lanes.go), so a wide
// window query takes only cores nothing else wants, and the calling goroutine always makes progress.

const parallelWindowMinRows = 20000

// windowExprsPushable: every expression of the window can be evaluated by several goroutines at once
// (no subquery, whose caches belong to the query).
func windowExprsPushable(w *winCall) bool {
	for _, x := range w.part {
		if !pushableWhere(x) {
			return false
		}
	}
	for _, s := range w.order {
		if !pushableWhere(s.x) {
			return false
		}
	}
	for _, x := range w.args {
		if !pushableWhere(x) {
			return false
		}
	}
	return true
}

// parallelRange runs fn over [0,n) in chunks on the caller and the helpers; each goroutine gets its own
// evalCtx copied from base. The first error wins.
func parallelRange(n int, fn func(lo, hi int, c *evalCtx) error, base *evalCtx) error {
	const chunk = 4096
	helpers := min(ScanWorkers-1, int(scanHelpers.Load()))
	var next atomic.Int64
	var emu sync.Mutex
	var ferr error
	var wg sync.WaitGroup
	work := func(w int) {
		c := &evalCtx{params: base.params, now: base.now, qx: base.qx, outer: base.outer, sess: base.sess}
		for {
			if w > 0 && !keepHelper(w) {
				return
			}
			lo := int(next.Add(chunk)) - chunk
			if lo >= n {
				return
			}
			var err error
			if base.qx != nil && base.qx.s != nil && base.qx.s.interrupted() {
				err = core.ErrInterrupted // a cancelled statement stops between chunks
			} else {
				err = fn(lo, min(lo+chunk, n), c)
			}
			if err != nil {
				emu.Lock()
				if ferr == nil {
					ferr = err
				}
				emu.Unlock()
				next.Store(int64(n))
				return
			}
		}
	}
	for w := 1; w <= helpers; w++ {
		w := w
		wg.Add(1)
		if !spawnHelper(func() { defer wg.Done(); work(w) }) {
			wg.Done()
			break
		}
	}
	work(0)
	wg.Wait()
	return ferr
}

// parallelSort sorts idx with cmp: runs sorted on the helpers, then merged pairwise, level by level.
// The result is the same as slices.SortFunc's whenever cmp is a total order (the window code makes it one).
func parallelSort(idx []int, cmp func(a, b int) int) {
	helpers := min(ScanWorkers-1, int(scanHelpers.Load()))
	k := min(helpers+1, 8)
	if k < 2 || len(idx) < 2*parallelWindowMinRows {
		slices.SortFunc(idx, cmp)
		return
	}
	size := (len(idx) + k - 1) / k
	var runs [][]int
	for lo := 0; lo < len(idx); lo += size {
		runs = append(runs, idx[lo:min(lo+size, len(idx))])
	}
	doAll := func(fns []func()) {
		var wg sync.WaitGroup
		for i := 1; i < len(fns); i++ {
			f := fns[i]
			wg.Add(1)
			if !spawnHelper(func() { defer wg.Done(); f() }) {
				wg.Done()
				f()
			}
		}
		fns[0]()
		wg.Wait()
	}
	var fns []func()
	for _, r := range runs {
		r := r
		fns = append(fns, func() { slices.SortFunc(r, cmp) })
	}
	doAll(fns)
	buf := make([]int, len(idx))
	src, dst := idx, buf
	for len(runs) > 1 {
		var next [][]int
		fns = fns[:0]
		off := 0
		for i := 0; i < len(runs); i += 2 {
			if i+1 == len(runs) {
				r := runs[i]
				o := off
				fns = append(fns, func() { copy(dst[o:o+len(r)], r) })
				next = append(next, dst[o:o+len(r)])
				off += len(r)
				continue
			}
			a, b := runs[i], runs[i+1]
			o := off
			fns = append(fns, func() { mergeInts(dst[o:o+len(a)+len(b)], a, b, cmp) })
			next = append(next, dst[o:o+len(a)+len(b)])
			off += len(a) + len(b)
		}
		doAll(fns)
		runs = next
		src, dst = dst, src
	}
	if &runs[0][0] != &idx[0] {
		copy(idx, runs[0])
	}
	_ = src
}

func mergeInts(dst, a, b []int, cmp func(a, b int) int) {
	i, j, o := 0, 0, 0
	for i < len(a) && j < len(b) {
		if cmp(b[j], a[i]) < 0 {
			dst[o] = b[j]
			j++
		} else {
			dst[o] = a[i]
			i++
		}
		o++
	}
	o += copy(dst[o:], a[i:])
	copy(dst[o:], b[j:])
}
