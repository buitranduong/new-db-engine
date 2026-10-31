package main

// PROTOTYPE measurements on the REAL engine: core primary-index lookups,
// the real columnar view (scan, apply, compaction, lock interference) and
// the log scan that approach A needs to learn the changed keys.

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/core"
	"stask/internal/proj"
	"stask/internal/schema"
	"stask/internal/wal"
)

func simRow(r Row) schema.Row {
	return schema.Row{{S: string(r.Key[:])}, {S: catNames[r.Cat]}, {I: int64(r.Price)}, {S: telNames[r.Tel]}, {I: int64(r.Rate)}}
}

func commitOps(e *core.Engine, ops []Op) uint64 {
	tx := e.Begin(core.SnapshotIsolation)
	for i := range ops {
		var err error
		if ops[i].Kind == OpDel {
			err = tx.Delete("sims", string(ops[i].Row.Key[:]))
		} else {
			err = tx.Put("sims", simRow(ops[i].Row))
		}
		if err != nil {
			panic(err)
		}
	}
	lsn, err := tx.Commit()
	if err != nil {
		panic(err)
	}
	return lsn
}

func runCore(n int, dir, out string) {
	N := int64(n)
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	fmt.Printf("# core mode N=%d GOMAXPROCS=%d load1=%s\n", n, runtime.GOMAXPROCS(0), loadavg())
	e, err := core.Open(core.Options{Dir: dir, NoCheckpoint: true, Logf: func(string, ...any) {}, ViewSaveEvery: -1, NoViewFile: true})
	if err != nil {
		panic(err)
	}
	defer e.Close()
	tb := schema.Table{Name: "sims", Key: "number", Columnar: true, Columns: []schema.Column{
		{Name: "number", Type: "string"}, {Name: "category", Type: "text"}, {Name: "price", Type: "int"},
		{Name: "tel", Type: "text"}, {Name: "store_rate", Type: "int"}}}
	ts, err := e.CreateTable(tb)
	if err != nil {
		panic(err)
	}
	col := proj.NewColumnar()
	f := e.Follow(col)
	for !f.Ready() {
		time.Sleep(10 * time.Millisecond)
	}
	m0 := heapMB()
	t0 := time.Now()
	var last uint64
	const per = 5000
	batch := make([]Op, 0, per)
	for g := int64(0); g < N; g++ {
		batch = append(batch, Op{Kind: OpPut, Row: genRow(g)})
		if len(batch) == per {
			last = commitOps(e, batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		last = commitOps(e, batch)
	}
	tLoad := time.Since(t0)
	f.WaitApplied(last, 10*time.Minute)
	fmt.Printf("CORE load %d rows (tx of %d, view following): commit %s => %.0f rows/s; view caught up %s after start\n", n, per, ms(tLoad), float64(n)/tLoad.Seconds(), ms(time.Since(t0)))
	m1 := heapMB()
	fmt.Printf("heap after load (core + current view, GC'd): %.0f MB => %.0f B/row for BOTH (core versions+indexes+view)\n", m1-m0, (m1-m0)*1e6/float64(n))

	// (1) current view: scan of the filter columns
	flt := []proj.Filter{{Col: "category", Op: "=", Value: "TAM_HOA"}, {Col: "price", Op: ">=", Value: int64(1000000)}, {Col: "price", Op: "<=", Value: int64(3000000)}}
	tid := ts.ID
	runQ := func(q proj.AggQuery) (time.Duration, *proj.Result) {
		var ds []time.Duration
		var r *proj.Result
		for i := 0; i < 7; i++ {
			qq := q
			t := time.Now()
			var err error
			r, err = col.Aggregate(tid, &qq)
			if err != nil {
				panic(err)
			}
			ds = append(ds, time.Since(t))
		}
		return med(ds), r
	}
	d1, r1 := runQ(proj.AggQuery{Table: "sims", Filters: flt, Aggs: []proj.Agg{{Fn: "count"}}})
	fmt.Printf("VIEW(current) count(*) WHERE category/price filter: median of 7 = %s, result %v (scanned_rows %d)\n", ms(d1), r1.Rows, r1.Scanned)
	d1b, r1b := runQ(proj.AggQuery{Table: "sims", Filters: flt, GroupBy: []string{"tel"}, Aggs: []proj.Agg{{Fn: "count"}, {Fn: "max", Col: "store_rate"}}})
	fmt.Printf("VIEW(current) GROUP BY tel count,max(store_rate) same filter: median of 7 = %s, %v\n", ms(d1b), r1b.Rows)
	for _, wk := range []int{1, 4} {
		d, _ := runQ(proj.AggQuery{Table: "sims", Filters: flt, Aggs: []proj.Agg{{Fn: "count"}}, Workers: wk})
		fmt.Printf("VIEW(current) count(*) filter workers=%d: %s\n", wk, ms(d))
	}

	// (3A) delta cost: log keys + GetAt of K keys on the real core
	for _, K := range []int{1000, 100000, 1000000} {
		ops := deltaOps(N, K)
		startPos := e.Log().WritePos()
		snapA, _, _ := e.DurablePoint()
		t := time.Now()
		var lsn uint64
		for i := 0; i < len(ops); i += 1000 {
			lsn = commitOps(e, ops[i:min(len(ops), i+1000)])
		}
		tc := time.Since(t)
		f.WaitApplied(lsn, 10*time.Minute)
		// A.1: scan the log from the view position to the durable point, keys only
		keys := make(map[string]struct{}, K)
		var recs int
		t = time.Now()
		_, endPos, _ := e.DurablePoint()
		seg, off := startPos.Seg, startPos.Off
		for {
			limit := int64(-1)
			if seg == endPos.Seg {
				limit = endPos.Off
			}
			end, more, err := e.Log().Scan(seg, off, limit, func(r *wal.Record) error {
				if r.LSN <= snapA || r.Kind == wal.KindDDL {
					return nil
				}
				recs++
				for i := range r.Ops {
					if r.Ops[i].Table == ts.ID {
						keys[string(r.Ops[i].Key)] = struct{}{}
					}
				}
				return nil
			})
			if err != nil {
				fmt.Println("scan err", err)
				break
			}
			if seg >= endPos.Seg {
				break
			}
			_ = more
			seg, off = seg+1, 0
			_ = end
		}
		tLog := time.Since(t)
		// A.2: GetAt each changed key at the snapshot (hot, decoded row)
		var ds []time.Duration
		var got int
		for rep := 0; rep < 3; rep++ {
			t = time.Now()
			got = 0
			for k := range keys {
				r, _, err := e.GetAt("sims", k, lsn)
				if err != nil {
					panic(err)
				}
				if r != nil {
					got++
				}
			}
			ds = append(ds, time.Since(t))
		}
		tGet := med(ds)
		// A.3: exclusion list through a key directory lookup (prototype Dir, 11-byte keys)
		fmt.Printf("A K=%d: core commit %s; log scan for keys %s (%d records, %d distinct keys); GetAt x%d (%d live) median of 3 = %s => %.2f us/key; A read-time overhead = %s\n",
			K, ms(tc), ms(tLog), recs, len(keys), len(keys), got, ms(tGet), float64(tGet.Microseconds())/float64(len(keys)), ms(tLog+tGet))
	}
	// random-order point read cost on the core (cold-ish: random keys over the whole table)
	{
		dur, _, _ := e.DurablePoint()
		var ds []time.Duration
		for rep := 0; rep < 7; rep++ {
			t := time.Now()
			for i := 0; i < 100000; i++ {
				g := (int64(i)*2654435761 + int64(rep)) % N
				gr := genRow(g)
				e.GetAt("sims", string(gr.Key[:]), dur)
			}
			ds = append(ds, time.Since(t))
		}
		fmt.Printf("core GetAt random key over %d rows: %.2f us/key (median of 7 x100k)\n", n, float64(med(ds).Microseconds())/100000)
	}

	// (4A) interference: long scans vs the follower applying a steady stream
	interfA(e, f, col, tid, flt, N)
	compactA(e, f, col, tid, flt, N)
	_ = strconv.Itoa
	_ = sync.Mutex{}
	_ = atomic.Bool{}
}

func interfA(e *core.Engine, f *core.Follower, col *proj.Columnar, tid uint32, flt []proj.Filter, N int64) {
	for _, withWriter := range []bool{false, true} {
		for _, readers := range []int{1} {
			var stop atomic.Bool
			var wg sync.WaitGroup
			var mu sync.Mutex
			var rl []time.Duration
			for r := 0; r < readers; r++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var mine []time.Duration
					for !stop.Load() {
						q := proj.AggQuery{Table: "sims", Filters: flt, GroupBy: []string{"tel"}, Aggs: []proj.Agg{{Fn: "count"}}, Workers: 1}
						t := time.Now()
						col.Aggregate(tid, &q)
						mine = append(mine, time.Since(t))
					}
					mu.Lock()
					rl = append(rl, mine...)
					mu.Unlock()
				}()
			}
			var lags []time.Duration
			var maxLsnGap uint64
			var ops int
			t0 := time.Now()
			if withWriter {
				var wgw sync.WaitGroup
				wgw.Add(1)
				go func() {
					defer wgw.Done()
					i := int64(0)
					for time.Since(t0) < 20*time.Second {
						var batch []Op
						for k := 0; k < 500; k++ {
							g := (i*7919 + int64(k)*104729) % N
							r := genRow(g)
							r.Price = int32(1000000 + (g+i)*31%2500000)
							r.Rate = uint8(5 + (g+i)%6)
							batch = append(batch, Op{Kind: OpPut, Row: r})
						}
						commitOps(e, batch)
						ops += 500
						i++
						time.Sleep(5 * time.Millisecond)
					}
				}()
				for time.Since(t0) < 20*time.Second {
					d, _, _ := e.DurablePoint()
					if gap := d - f.Applied(); gap > maxLsnGap {
						maxLsnGap = gap
					}
					lags = append(lags, f.Lag())
					time.Sleep(20 * time.Millisecond)
				}
				wgw.Wait()
			} else {
				time.Sleep(10 * time.Second)
			}
			stop.Store(true)
			wg.Wait()
			fmt.Printf("INTERF A(current view) writer=%v: %d scans (count by tel w=1) p50 %s p99 %s max %s", withWriter, len(rl), ms(pct(rl, .5)), ms(pct(rl, .99)), ms(pct(rl, 1)))
			if withWriter {
				fmt.Printf("; core wrote %d ops (%.0f ops/s); view lag p50 %s p99 %s max %s; max LSN gap durable-applied %d", ops, float64(ops)/20, ms(pct(lags, .5)), ms(pct(lags, .99)), ms(pct(lags, 1)), maxLsnGap)
			}
			fmt.Println()
		}
	}
}

// compactA: update ~6M rows so that dead positions pass half of the view
// (maybeCompact copies every live row under the table write lock) while a
// reader loops; report the worst reader stall and the view lag.
func compactA(e *core.Engine, f *core.Follower, col *proj.Columnar, tid uint32, flt []proj.Filter, N int64) {
	var stop atomic.Bool
	var wg sync.WaitGroup
	type ev struct {
		at  time.Duration
		dur time.Duration
	}
	var slow []ev
	var rl []time.Duration
	t0 := time.Now()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			q := proj.AggQuery{Table: "sims", Filters: flt, GroupBy: []string{"tel"}, Aggs: []proj.Agg{{Fn: "count"}}, Workers: 1}
			t := time.Now()
			col.Aggregate(tid, &q)
			d := time.Since(t)
			rl = append(rl, d)
			if d > 300*time.Millisecond {
				slow = append(slow, ev{time.Since(t0), d})
			}
		}
	}()
	var lags []time.Duration
	n0, d0 := col.Stats(tid)
	fmt.Printf("COMPACT A start: view positions %d, dead %d\n", n0, d0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var last uint64
		for g := int64(0); g < 9000000; g += 5000 {
			var batch []Op
			for k := int64(0); k < 5000; k++ {
				r := genRow(g + k)
				r.Price = int32(1000000 + (g+k)*17%2500000)
				batch = append(batch, Op{Kind: OpPut, Row: r})
			}
			last = commitOps(e, batch)
		}
		f.WaitApplied(last, 10*time.Minute)
	}()
	for {
		select {
		case <-done:
			stop.Store(true)
			wg.Wait()
			n1, d1 := col.Stats(tid)
			fmt.Printf("COMPACT A end: view positions %d, dead %d\n", n1, d1)
			fmt.Printf("COMPACT A(current view): 9M updates over %s; reader scans %d p50 %s p99 %s max %s; scans > 300 ms: %v; view lag p50 %s p99 %s max %s\n",
				ms(time.Since(t0)), len(rl), ms(pct(rl, .5)), ms(pct(rl, .99)), ms(pct(rl, 1)), slow, ms(pct(lags, .5)), ms(pct(lags, .99)), ms(pct(lags, 1)))
			return
		default:
			lags = append(lags, f.Lag())
			time.Sleep(20 * time.Millisecond)
		}
	}
}
