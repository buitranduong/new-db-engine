package core

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/schema"
)

const lkN = 800

func lkIndexEqual(t *testing.T, e *Engine, model map[string]int64, when string) {
	t.Helper()
	e.waitInstalled(e.DurableLSN())
	snap := e.DurableLSN()
	ts, _ := e.Table("lk")
	for v := int64(0); v < 50; v++ {
		want := map[string]bool{}
		for id, n := range model {
			if n == v {
				want[id] = true
			}
		}
		lo := encInt(v)
		got := map[string]bool{}
		ok, err := e.IndexScan(ts, "lk_n", snap, lo, PrefixEnd(lo), false, func(pk string, r schema.Row) bool {
			if got[pk] {
				t.Errorf("%s: n=%d: %s twice", when, v, pk)
			}
			got[pk] = true
			return true
		})
		if err != nil || !ok {
			t.Fatalf("%s: index scan: ok=%v err=%v", when, ok, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: n=%d: index has %d rows, table %d", when, v, len(got), len(want))
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("%s: n=%d: %s missing from the index", when, v, id)
			}
		}
	}
}

func lkSecDB(t *testing.T, minDead int64, hook func(string)) (*Engine, map[string]int64) {
	t.Helper()
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf, ArenaCompactMinDead: minDead, SidxHook: hook})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if _, err := e.CreateTable(longKeyTable()); err != nil {
		t.Fatal(err)
	}
	model := map[string]int64{}
	for i := 0; i < lkN; i++ {
		model[longID(i)] = int64(i % 50)
		if _, err := e.Write([]WriteOp{{Table: "lk", Row: lkRow(longID(i), int64(i%50))}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.CreateIndex("lk", schema.IndexDef{Name: "lk_n", Columns: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	return e, model
}

// churn moves every row to another n (leaving dead entries) and sweeps them,
// so the arena holds dead key bytes.
func lkChurn(t *testing.T, e *Engine, model map[string]int64, shift int) {
	t.Helper()
	for i := 0; i < lkN; i++ {
		model[longID(i)] = int64((i + shift) % 50)
		if _, err := e.Write([]WriteOp{{Table: "lk", Row: lkRow(longID(i), int64((i+shift)%50))}}); err != nil {
			t.Fatal(err)
		}
	}
	e.waitInstalled(e.DurableLSN())
	e.sweepIndexes(e.DurableLSN())
}

// The arena of a RAM secondary index is rebuilt online: writers run during the
// rebuild, nothing is lost or doubled, the dead key bytes are gone.
func TestSecArenaCompaction(t *testing.T) {
	e, model := lkSecDB(t, -1, nil)
	lkChurn(t, e, model, 7)
	ts, _ := e.Table("lk")
	si := ts.secNamed("lk_n")
	dead0 := si.tr.Load().ar.Dead.Load()
	if dead0 == 0 {
		t.Fatal("test set-up: no dead key bytes in the index arena")
	}
	lkIndexEqual(t, e, model, "before")
	// quiet: a compaction leaves exactly the live keys in a fresh arena
	live0 := si.tr.Load().ar.Live.Load()
	if _, err := e.CompactSecArena("lk", "lk_n"); err != nil {
		t.Fatal(err)
	}
	if ar := si.tr.Load().ar; ar.Dead.Load() != 0 || ar.Live.Load() >= live0 || ar.Live.Load() == 0 {
		t.Fatalf("arena after a quiet compaction: live %d (was %d) dead %d", ar.Live.Load(), live0, ar.Dead.Load())
	}
	lkIndexEqual(t, e, model, "after quiet")

	var stop atomic.Bool
	var wg sync.WaitGroup
	var mu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(3))
		for !stop.Load() {
			i := 5000 + rng.Intn(400)
			id, n := longID(i), rng.Int63n(50)
			if rng.Intn(4) == 0 {
				if _, err := e.Write([]WriteOp{{Table: "lk", Key: id}}); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				delete(model, id)
				mu.Unlock()
				continue
			}
			if _, err := e.Write([]WriteOp{{Table: "lk", Row: lkRow(id, n)}}); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			model[id] = n
			mu.Unlock()
		}
	}()
	freed, err := e.CompactSecArena("lk", "lk_n")
	// compact again while the writer is still running
	if _, err2 := e.CompactSecArena("lk", "lk_n"); err2 != nil {
		t.Errorf("second compaction: %v", err2)
	}
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	_ = freed // the arena grows in 1 MB chunks: judge it by its live and dead key bytes
	if e.SecArenaCompactions.Load() != 3 {
		t.Fatalf("compactions = %d", e.SecArenaCompactions.Load())
	}
	if si.mig != nil {
		t.Fatal("conversion log left on")
	}
	lkIndexEqual(t, e, model, "after")
	// the compacted index keeps working: sweeping after more churn, then equal again
	lkChurn(t, e, model, 11)
	lkIndexEqual(t, e, model, "after churn")
	if ar := si.tr.Load().ar; ar.Dead.Load() == 0 || ar.Dead.Load() >= dead0+int64(lkN*40) {
		t.Fatalf("dead key bytes after churn: %d (before the compaction %d)", ar.Dead.Load(), dead0)
	}
}

// Compaction refuses a disk index, a dropped index, and does not disturb a
// running storage conversion's log.
func TestSecArenaCompactionRefusals(t *testing.T) {
	e, model := lkSecDB(t, -1, nil)
	lkChurn(t, e, model, 7)
	ts, _ := e.Table("lk")
	si := ts.secNamed("lk_n")
	si.mu.Lock()
	si.mig = e.newMig(si) // as if a conversion were recording
	si.mu.Unlock()
	if _, err := e.CompactSecArena("lk", "lk_n"); err == nil {
		t.Fatal("compaction ran during a conversion")
	}
	si.mu.Lock()
	si.mig = nil
	si.mu.Unlock()
	if _, err := e.CompactSecArena("lk", "nosuch"); err == nil {
		t.Fatal("unknown index accepted")
	}
	si.retired.Store(true)
	if _, err := e.CompactSecArena("lk", "lk_n"); err == nil {
		t.Fatal("retired index compacted")
	}
	si.retired.Store(false)
	lkIndexEqual(t, e, model, "after refusals")
}

// The background compactor reclaims once the dead bytes pass the threshold.
func TestSecArenaAutoCompactor(t *testing.T) {
	e, model := lkSecDB(t, 1, nil)
	lkChurn(t, e, model, 7)
	deadline := time.Now().Add(30 * time.Second)
	for e.SecArenaCompactions.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if e.SecArenaCompactions.Load() == 0 {
		t.Fatal("the compactor did not run")
	}
	lkIndexEqual(t, e, model, "after auto")
}

// Writes made after the copy was taken and before it is published (the window
// in which the recorded mutations matter) are in the compacted index: new
// keys, moved keys (an add and a dead mark) and deleted keys (a sweep).
func TestSecArenaCompactionCatchUp(t *testing.T) {
	var e *Engine
	var model map[string]int64
	fired := 0
	hook := func(step string) {
		if step != "secarena-built" || e == nil {
			return
		}
		fired++
		for i := 0; i < 300; i++ {
			id := longID(6000 + i)
			model[id] = int64(i % 50)
			if _, err := e.Write([]WriteOp{{Table: "lk", Row: lkRow(id, int64(i%50))}}); err != nil {
				t.Error(err)
			}
		}
		for i := 0; i < 300; i++ { // move existing rows
			id := longID(i)
			model[id] = int64((i + 3) % 50)
			if _, err := e.Write([]WriteOp{{Table: "lk", Row: lkRow(id, int64((i+3)%50))}}); err != nil {
				t.Error(err)
			}
		}
		for i := 300; i < 400; i++ {
			delete(model, longID(i))
			if _, err := e.Write([]WriteOp{{Table: "lk", Key: longID(i)}}); err != nil {
				t.Error(err)
			}
		}
		e.waitInstalled(e.DurableLSN())
		e.sweepIndexes(e.DurableLSN()) // removals recorded as well
	}
	e, model = lkSecDB(t, -1, hook)
	lkChurn(t, e, model, 7)
	if _, err := e.CompactSecArena("lk", "lk_n"); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("hook fired %d times", fired)
	}
	lkIndexEqual(t, e, model, "catch-up")
	ts, _ := e.Table("lk")
	if ts.secNamed("lk_n").mig != nil {
		t.Fatal("conversion log left on")
	}
}
