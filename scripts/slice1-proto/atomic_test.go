package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Multi-table atomicity: every transaction writes sentinel row S (sims) with
// Rate=ver%6+5 AND the dimension (carriers.A name = ver). A reader must see both from the same ver.
// B: one published State. Per-table: two structures, each under its own RWMutex, applied one after the other
// (the shape of Columnar.Flush today).
func TestMultiTableSnapshot(t *testing.T) {
	rows := buildRows(100000)
	w := NewWriter(buildBase(rows, true, 0), defaultDim())
	var stop atomic.Bool
	var torn, reads atomic.Int64
	var wg sync.WaitGroup
	// per-table-lock variant
	var mu1, mu2 sync.RWMutex
	var simVer, dimVer int
	var torn2, reads2 atomic.Int64
	for r := 0; r < 2; r++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				st := w.Current()
				if st.LSN == 0 {
					continue
				}
				ver := int(st.sentinel()) - 5
				var dv int
				fmt.Sscan(st.Dim.car[0].Name, &dv)
				reads.Add(1)
				if ver%6 != dv%6 {
					torn.Add(1)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for !stop.Load() {
				mu1.RLock()
				a := simVer
				mu1.RUnlock()
				time.Sleep(time.Microsecond)
				mu2.RLock()
				b := dimVer
				mu2.RUnlock()
				reads2.Add(1)
				if a != b {
					torn2.Add(1)
				}
			}
		}()
	}
	key := genRow(5)
	for ver := 1; ver <= 20000; ver++ {
		r := key
		r.Rate = uint8(5 + ver%6)
		d := *w.dim
		d.car[0].Name = fmt.Sprint(ver)
		w.Apply(Txn{LSN: uint64(ver), Ops: []Op{{Kind: OpPut, Row: r}, {Kind: OpDim, Dim: &d}}})
		w.Publish()
		mu1.Lock()
		simVer = ver
		mu1.Unlock()
		mu2.Lock()
		dimVer = ver
		mu2.Unlock()
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("B single published State: %d reads, %d torn", reads.Load(), torn.Load())
	t.Logf("per-table locks applied one after the other (today's Flush shape; reader locks per table): %d reads, %d torn", reads2.Load(), torn2.Load())
	if torn.Load() != 0 {
		t.Fatal("B must never tear")
	}
}
