package core

import (
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/schema"
)

// TestCommitFairness reproduces the SR finding (docs/15 section 6): small
// order-like transactions run next to a bulk loader writing big transactions
// into an indexed table. It reports the small transactions' p50/p99 with the
// loader idle and running. Opt in with STASK_FAIR=1; knobs: STASK_FAIR_IDX
// (number of indexes, default 3), _ROWS (table size), _BULK (rows per bulk
// transaction), _SMALL (small writers), _SECS (seconds per phase).
func TestCommitFairness(t *testing.T) {
	if os.Getenv("STASK_FAIR") == "" {
		t.Skip("set STASK_FAIR=1")
	}
	geti := func(k string, d int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return v
		}
		return d
	}
	nidx, rows, bulk, nsmall, secs := geti("STASK_FAIR_IDX", 3), geti("STASK_FAIR_ROWS", 300000), geti("STASK_FAIR_BULK", 2000), geti("STASK_FAIR_SMALL", 8), geti("STASK_FAIR_SECS", 6)
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 64 << 20, NoCheckpoint: os.Getenv("STASK_FAIR_CKPT") == ""})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	cols := []schema.Column{{Name: "id", Type: "string"}}
	for _, c := range []string{"a", "b", "c", "d", "e"} {
		cols = append(cols, schema.Column{Name: c, Type: "int"})
	}
	if _, err := e.CreateTable(schema.Table{Name: "sims", Key: "id", Columns: cols}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable(schema.Table{Name: "orders", Key: "id", Columns: cols[:3]}); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, v int64) schema.Row {
		return schema.Row{{S: id}, {I: v * 7 % 100003}, {I: v * 13 % 9973}, {I: v % 101}, {I: v * 31}, {I: v}}
	}
	key := func(i int) string { return fmt.Sprintf("0%09d", i*37%1000003) }
	for i := 0; i < rows; i += 2000 {
		var ops []WriteOp
		for j := i; j < min(i+2000, rows); j++ {
			ops = append(ops, WriteOp{Table: "sims", Row: mk(key(j), int64(j))})
		}
		if _, err := e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []string{"a", "b", "c", "d", "e"}[:nidx] {
		if err := e.CreateIndex("sims", schema.IndexDef{Name: "s_" + c, Columns: []string{c}}); err != nil {
			t.Fatal(err)
		}
	}
	var ver atomic.Int64
	ver.Store(1 << 20)
	var ord atomic.Int64
	small := func(stop *atomic.Bool, lat *[]time.Duration, mu *sync.Mutex, wg *sync.WaitGroup, seed int) {
		defer wg.Done()
		x := uint64(seed)*2654435761 + 1
		for !stop.Load() {
			x = x*6364136223846793005 + 1442695040888963407
			i := int(x>>33) % rows
			t0 := time.Now()
			// hold a number (indexed columns change) + create an order
			o := ord.Add(1)
			_, err := e.Write([]WriteOp{
				{Table: "sims", Row: mk(key(i), ver.Add(1))},
				{Table: "orders", Row: schema.Row{{S: "o" + strconv.FormatInt(o, 10)}, {I: o}, {I: 1}}},
			})
			d := time.Since(t0)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			*lat = append(*lat, d)
			mu.Unlock()
			time.Sleep(time.Duration(x>>20%50) * time.Millisecond) // think 0-50ms
		}
	}
	pct := func(l []time.Duration, p float64) float64 {
		if len(l) == 0 {
			return 0
		}
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
		return float64(l[min(int(float64(len(l))*p), len(l)-1)].Microseconds()) / 1000
	}
	phase := func(name string, withBulk bool) (p50 float64) {
		var stop atomic.Bool
		var lat []time.Duration
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < nsmall; i++ {
			wg.Add(1)
			go small(&stop, &lat, &mu, &wg, i+1)
		}
		var bulkRows, bulkTx int
		var bwg sync.WaitGroup
		if withBulk {
			bwg.Add(1)
			go func() {
				defer bwg.Done()
				for base := 0; !stop.Load(); base += bulk {
					// sorted by key; 90% existing keys (value change), 10% new
					idx := make([]string, bulk)
					for j := range idx {
						n := base + j
						if n%10 == 0 {
							idx[j] = fmt.Sprintf("1%09d", n)
						} else {
							idx[j] = key(n * 7 % rows)
						}
					}
					sort.Strings(idx)
					ops := make([]WriteOp, 0, bulk)
					for j, k := range idx {
						if j > 0 && idx[j-1] == k {
							continue
						}
						ops = append(ops, WriteOp{Table: "sims", Row: mk(k, ver.Add(1))})
					}
					if _, err := e.Write(ops); err != nil {
						t.Error(err)
						return
					}
					bulkRows += len(ops)
					bulkTx++
				}
			}()
		}
		time.Sleep(time.Duration(secs) * time.Second)
		stop.Store(true)
		wg.Wait()
		bwg.Wait()
		mu.Lock()
		defer mu.Unlock()
		p50 = pct(lat, .5)
		fmt.Printf("FAIR %-10s idx=%d small n=%d p50=%.2fms p90=%.2fms p99=%.2fms | bulk tx=%d rows=%d (%.0f rows/s)\n",
			name, nidx, len(lat), p50, pct(lat, .9), pct(lat, .99), bulkTx, bulkRows, float64(bulkRows)/float64(secs))
		return
	}
	phase("warm", false)
	base := phase("baseline", false)
	if pf := os.Getenv("STASK_FAIR_PROF"); pf != "" {
		f, _ := os.Create(pf)
		pprof.StartCPUProfile(f)
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	with := phase("with-bulk", true)
	if os.Getenv("STASK_FAIR_PROF") != "" {
		pprof.StopCPUProfile()
	}
	base2 := phase("baseline2", false)
	fmt.Printf("FAIR ratio p50 with-bulk/baseline = %.2f (baselines %.2f %.2f)\n", with/((base+base2)/2), base, base2)
}
