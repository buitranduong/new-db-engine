package core

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/schema"
)

func stageEngine(t *testing.T, dir string, create bool) *Engine {
	t.Helper()
	e, err := Open(Options{Dir: dir, SegmentSize: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if create {
		cols := []schema.Column{{Name: "id", Type: "string"}, {Name: "n", Type: "int"}, {Name: "m", Type: "int"}}
		if _, err := e.CreateTable(schema.Table{Name: "kv", Key: "id", Columns: cols}); err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{"n", "m"} {
			if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_" + c, Columns: []string{c}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return e
}

// checkIndexes compares, for every value in [0,dom), an index scan with a
// full scan, for both indexes.
func checkIndexes(t *testing.T, e *Engine, dom int64) {
	t.Helper()
	snap := e.DurableLSN()
	e.waitInstalled(snap)
	for v := int64(0); v < dom; v++ {
		got := sorted(scanEq(t, e, "kv", "kv_n", snap, v))
		if want := bruteEq(e, "kv", snap, v); !sameKeys(got, want) {
			t.Fatalf("index kv_n value %d: index has %d rows, table %d", v, len(got), len(want))
		}
	}
	ts, _ := e.Table("kv")
	for v := int64(0); v < dom; v++ {
		lo := encInt(v)
		var got []string
		ok, err := e.IndexScan(ts, "kv_m", snap, lo, PrefixEnd(lo), false, func(pk string, r schema.Row) bool {
			got = append(got, pk)
			return true
		})
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		var want []string
		e.ScanSnapshot("kv", snap, func(k string, r schema.Row, _ *Version) bool {
			if r[2].I == v {
				want = append(want, k)
			}
			return true
		})
		if len(got) != len(want) {
			t.Fatalf("index kv_m value %d: index has %d rows, table %d", v, len(got), len(want))
		}
	}
}

// Big transactions (staged) racing with small ones on overlapping keys: the
// indexes must agree with the table afterwards, in memory and after a
// restart (rebuilt from the log).
func TestStagedBigTxVsSmall(t *testing.T) {
	old := stageMinAdds
	stageMinAdds = 64
	defer func() { stageMinAdds = old }()
	const dom, keys = 40, 600
	dir := t.TempDir()
	e := stageEngine(t, dir, true)
	var ops []WriteOp
	for i := 0; i < keys; i++ {
		ops = append(ops, WriteOp{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("k%04d", i)}, {I: int64(i % dom)}, {I: int64(i * 7 % dom)}}})
	}
	if _, err := e.Write(ops); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var bigs, smalls atomic.Int64
	for g := 0; g < 3; g++ { // big transactions: 100 rows x 2 indexes = 200 adds > 64
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(g)))
			for !stop.Load() {
				seen := map[int]bool{}
				var ops []WriteOp
				for len(ops) < 100 {
					i := rnd.Intn(keys)
					if seen[i] {
						continue
					}
					seen[i] = true
					ops = append(ops, WriteOp{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("k%04d", i)}, {I: rnd.Int63n(dom)}, {I: rnd.Int63n(dom)}}})
				}
				if _, err := e.Write(ops); err != nil {
					t.Error(err)
					return
				}
				bigs.Add(1)
			}
		}(g)
	}
	for g := 0; g < 4; g++ { // small transactions on the same keys
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(100 + g)))
			for !stop.Load() {
				i, j := rnd.Intn(keys), rnd.Intn(keys)
				if i == j {
					continue
				}
				_, err := e.Write([]WriteOp{
					{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("k%04d", i)}, {I: rnd.Int63n(dom)}, {I: rnd.Int63n(dom)}}},
					{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("k%04d", j)}, {I: rnd.Int63n(dom)}, {I: rnd.Int63n(dom)}}},
				})
				if err != nil {
					t.Error(err)
					return
				}
				smalls.Add(1)
			}
		}(g)
	}
	time.Sleep(2 * time.Second)
	stop.Store(true)
	wg.Wait()
	if bigs.Load() == 0 || smalls.Load() == 0 {
		t.Fatalf("no progress: bigs=%d smalls=%d", bigs.Load(), smalls.Load())
	}
	t.Logf("bigs=%d smalls=%d", bigs.Load(), smalls.Load())
	checkIndexes(t, e, dom)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = stageEngine(t, dir, false)
	defer e.Close()
	checkIndexes(t, e, dom)
}

// A staged transaction that fails validation leaves no entries behind, and a
// staged one that succeeds after a concurrent commit on one of its keys
// keeps its entries alive.
func TestStagedFailureCleanup(t *testing.T) {
	old := stageMinAdds
	stageMinAdds = 64
	defer func() { stageMinAdds = old }()
	e := stageEngine(t, t.TempDir(), true)
	defer e.Close()
	entries := func() int {
		n := 0
		for _, s := range e.SecIndexStats() {
			n += s.Entries
		}
		return n
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "hot"}, {I: 1}, {I: 1}}}}); err != nil {
		t.Fatal(err)
	}
	before := entries()
	tx := e.Begin(SnapshotIsolation)
	for i := 0; i < 100; i++ {
		if err := tx.Put("kv", schema.Row{{S: fmt.Sprintf("f%03d", i)}, {I: int64(1000 + i)}, {I: int64(2000 + i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Put("kv", schema.Row{{S: "hot"}, {I: 5000}, {I: 5001}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "hot"}, {I: 2}, {I: 2}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("commit: want ErrConflict, got %v", err)
	}
	e.waitInstalled(e.DurableLSN())
	// +2: the concurrent write added (2) and (2) for "hot"; nothing from the failed tx
	if got := entries(); got != before+2 {
		t.Fatalf("entries after failed staged tx: %d, want %d (leak)", got, before+2)
	}
	// a big raw write on top of a concurrently changed key is kept whole
	var ops []WriteOp
	for i := 0; i < 100; i++ {
		ops = append(ops, WriteOp{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("g%03d", i)}, {I: 7}, {I: 8}}})
	}
	ops = append(ops, WriteOp{Table: "kv", Row: schema.Row{{S: "hot"}, {I: 7}, {I: 8}}})
	if _, err := e.Write(ops); err != nil {
		t.Fatal(err)
	}
	snap := e.DurableLSN()
	e.waitInstalled(snap)
	if got := len(scanEq(t, e, "kv", "kv_n", snap, 7)); got != 101 {
		t.Fatalf("kv_n=7 rows via index: %d, want 101", got)
	}
}

// Checkpoint and GC (index sweep) with staged writes in the mix.
func TestStagedCheckpointGC(t *testing.T) {
	old := stageMinAdds
	stageMinAdds = 64
	defer func() { stageMinAdds = old }()
	dir := t.TempDir()
	e := stageEngine(t, dir, true)
	for round := 0; round < 5; round++ {
		var ops []WriteOp
		for i := 0; i < 200; i++ {
			ops = append(ops, WriteOp{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("k%04d", i)}, {I: int64((i + round) % 30)}, {I: int64(i % 30)}}})
		}
		if _, err := e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	e.waitInstalled(e.DurableLSN())
	e.sweepIndexes(e.DurableLSN())
	checkIndexes(t, e, 30)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = stageEngine(t, dir, false)
	defer e.Close()
	checkIndexes(t, e, 30)
}
