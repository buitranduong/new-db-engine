package main

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

func med(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)/2]
}
func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}
func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
func heapMB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / 1e6
}

var q0 = R2Q{Cat: 0, Lo: 1000000, Hi: 3000000, Quota: [3]int{20, 50, 30}}

func dumpPage(path string, out []Out) {
	f, _ := os.Create(path)
	defer f.Close()
	for _, o := range out {
		fmt.Fprintf(f, "%s\t%s\t%d\t%d\n", o.Key, o.Name, o.Price, o.Rate)
	}
}

func defaultDim() *Dim {
	return &Dim{car: [3]Carrier{{"A", true}, {"B", true}, {"C", true}}}
}

func timeR2(st *State, q R2Q, reps int) (time.Duration, R2Stats) {
	var ds []time.Duration
	var s R2Stats
	for i := 0; i < reps; i++ {
		t := time.Now()
		_, s = st.R2(q)
		ds = append(ds, time.Since(t))
	}
	return med(ds), s
}

func buildRows(n int) []Row {
	rows := make([]Row, n)
	for g := range rows {
		rows[g] = genRow(int64(g))
	}
	return rows
}

func runProto(n int, outDir string) {
	N := int64(n)
	fmt.Printf("# scale N=%d rows, GOMAXPROCS=%d, load1=%s\n", n, runtime.GOMAXPROCS(0), loadavg())
	rows := buildRows(n)
	m0 := heapMB()
	t := time.Now()
	bu := buildBase(rows, false, 0)
	fmt.Printf("build base unsorted: %s\n", ms(time.Since(t)))
	t = time.Now()
	bs := buildBase(rows, true, 0)
	fmt.Printf("build base sorted(cat,price): %s\n", ms(time.Since(t)))
	rows = nil
	m1 := heapMB()
	fmt.Printf("memory both bases (cols+keys+zonemaps): %.0f MB => %.1f B/row each (price i32, cat/tel/rate u8, key 11B)\n", m1-m0, (m1-m0)*1e6/float64(n)/2)

	for _, v := range []struct {
		name string
		b    *Base
	}{{"unsorted", bu}, {"sorted", bs}} {
		t = time.Now()
		w := NewWriter(v.b, defaultDim())
		dirT := time.Since(t)
		st := w.Current()
		for _, wk := range []int{1, 4, 8} {
			q := q0
			q.Page, q.Workers = 1, wk
			d, s := timeR2(st, q, 7)
			fmt.Printf("R2 base-only %-8s workers=%d page1: median %s (chunks read %d pruned %d, rows scanned %d passed %d)\n", v.name, wk, ms(d), s.ChunksRead, s.ChunksPruned, s.ScannedBase, s.Passed)
		}
		for p := 1; p <= 3; p++ {
			q := q0
			q.Page, q.Workers = p, 4
			out, _ := st.R2(q)
			dumpPage(fmt.Sprintf("%s/base_%s_p%d.tsv", outDir, v.name, p), out)
		}
		m2 := heapMB()
		fmt.Printf("row directory build (%s): %s; heap with dir: +%.0f MB => dir %.1f B/row\n", v.name, ms(dirT), m2-m1, (m2-m1)*1e6/float64(n))
		m1 = m2
		_ = w
		if v.name == "unsorted" {
			bu = nil
		}
	}
	// ---- delta on the sorted base
	for _, K := range []int{1000, 100000, 1000000} {
		w := NewWriter(bs, defaultDim())
		ops := deltaOps(N, K)
		var lsn uint64
		txns := toTxns(ops, 1000, &lsn)
		mb := heapMB()
		t = time.Now()
		for _, tx := range txns {
			w.Apply(tx)
			w.Publish()
		}
		d := time.Since(t)
		ma := heapMB()
		fmt.Printf("B apply+publish K=%d (%d ops, %d txns of 1000): %s => %.2f us/op; tail %d rows; mem +%.1f MB (%.0f B/tail row incl. dir+bits)\n",
			K, len(ops), len(txns), ms(d), float64(d.Microseconds())/float64(len(ops)), w.Current().Tail.n, ma-mb, (ma-mb)*1e6/float64(w.Current().Tail.n))
		st := w.Current()
		q := q0
		q.Page, q.Workers = 1, 4
		dm, s := timeR2(st, q, 7)
		fmt.Printf("R2 base+tail K=%d workers=4 page1: median %s (tail rows scanned %d)\n", K, ms(dm), s.ScannedTail)
		q.Workers = 1
		dm, _ = timeR2(st, q, 7)
		fmt.Printf("R2 base+tail K=%d workers=1 page1: median %s\n", K, ms(dm))
		if K == 100000 {
			writeDeltaCSV(outDir+"/delta.csv", N, ops)
			for p := 1; p <= 3; p++ {
				q := q0
				q.Page, q.Workers = p, 4
				out, _ := st.R2(q)
				dumpPage(fmt.Sprintf("%s/delta_p%d.tsv", outDir, p), out)
			}
			// dimension change at a later LSN: rename C, deactivate B
			d2 := *st.Dim
			d2.car[2].Name = "C2"
			lsn++
			w.Apply(Txn{LSN: lsn, Ops: []Op{{Kind: OpDim, Dim: &d2}}})
			w.Publish()
			for p := 1; p <= 3; p++ {
				q := q0
				q.Page, q.Workers = p, 4
				out, _ := w.Current().R2(q)
				dumpPage(fmt.Sprintf("%s/dimC2_p%d.tsv", outDir, p), out)
			}
			d3 := d2
			d3.car[1].Active = false
			lsn++
			w.Apply(Txn{LSN: lsn, Ops: []Op{{Kind: OpDim, Dim: &d3}}})
			w.Publish()
			for p := 1; p <= 3; p++ {
				q := q0
				q.Page, q.Workers = p, 4
				out, _ := w.Current().R2(q)
				dumpPage(fmt.Sprintf("%s/dimBoff_p%d.tsv", outDir, p), out)
			}
		}
		// merge with readers running
		if K == 1000000 {
			interfB(w, bs, N, &lsn)
		}
	}
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	if len(b) > 14 {
		return string(b[:14])
	}
	return string(b)
}

// interfB: steady update stream + readers, with a merge in the middle.
func interfB(w *Writer, bs *Base, N int64, lsn *uint64) {
	for _, withWriter := range []bool{false, true} {
		var stop atomic.Bool
		var wg sync.WaitGroup
		var mu sync.Mutex
		var rl []time.Duration
		for r := 0; r < 2; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var mine []time.Duration
				for !stop.Load() {
					st := w.Current()
					q := q0
					q.Page, q.Workers = 1, 1
					t := time.Now()
					st.R2(q)
					mine = append(mine, time.Since(t))
				}
				mu.Lock()
				rl = append(rl, mine...)
				mu.Unlock()
			}()
		}
		var wl []time.Duration
		var ops int
		t0 := time.Now()
		merges0 := w.Stats.Merges
		if withWriter {
			i := int64(0)
			for time.Since(t0) < 20*time.Second {
				if time.Since(t0) > 5*time.Second && w.Stats.Merges == merges0 {
					w.StartMerge(true)
				}
				tx := Txn{}
				*lsn++
				tx.LSN = *lsn
				for k := 0; k < 500; k++ {
					g := (i*7919 + int64(k)*104729) % N
					r := genRow(g)
					r.Price = int32(1000000 + (g+i)*31%2500000)
					r.Rate = uint8(5 + (g+i)%6)
					tx.Ops = append(tx.Ops, Op{Kind: OpPut, Row: r})
				}
				i++
				t := time.Now()
				w.Apply(tx)
				w.Publish()
				wl = append(wl, time.Since(t))
				ops += 500
				time.Sleep(5 * time.Millisecond)
			}
		} else {
			time.Sleep(10 * time.Second)
		}
		stop.Store(true)
		wg.Wait()
		el := time.Since(t0)
		fmt.Printf("INTERF B writer=%v: %d reads (2 readers, R2 w=1 page1) p50 %s p99 %s max %s", withWriter, len(rl), ms(pct(rl, .5)), ms(pct(rl, .99)), ms(pct(rl, 1)))
		if withWriter {
			fmt.Printf("; writer %d ops in %.0fs (%.0f ops/s), apply+publish per txn(500 ops) p50 %s p99 %s max %s, merges done %d",
				ops, el.Seconds(), float64(ops)/el.Seconds(), ms(pct(wl, .5)), ms(pct(wl, .99)), ms(pct(wl, 1)), w.Stats.Merges-merges0)
		}
		fmt.Println()
	}
}
