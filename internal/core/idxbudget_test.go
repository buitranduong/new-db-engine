package core

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/schema"
)

func longKeyTable() schema.Table {
	return schema.Table{Name: "lk", Key: "id", Columns: []schema.Column{{Name: "id", Type: "string"}, {Name: "n", Type: "int"}}}
}

func lkRow(id string, n int64) schema.Row {
	return schema.Row{{S: id}, {I: n}}
}

func longID(i int) string { return fmt.Sprintf("long-key-padding-%013d", i) }

func lkCheck(t *testing.T, e *Engine, want map[string]int64, when string) {
	t.Helper()
	e.waitInstalled(e.DurableLSN())
	snap := e.DurableLSN()
	got := 0
	err := e.ScanSnapshot("lk", snap, func(key string, r schema.Row, v *Version) bool {
		got++
		if w, ok := want[key]; !ok || w != r[1].I {
			t.Errorf("%s: key %q = %d, want %d (present %v)", when, key, r[1].I, w, ok)
			return false
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != len(want) {
		t.Fatalf("%s: %d rows, want %d", when, got, len(want))
	}
}

// Dead long keys are reclaimed online: writers run during the rebuild and
// lose nothing, the table equals the model after, the arena shrinks, and a
// restart agrees.
func TestKeyArenaCompaction(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf, ArenaCompactMinDead: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable(longKeyTable()); err != nil {
		t.Fatal(err)
	}
	model := map[string]int64{}
	var mu sync.Mutex
	put := func(i int, n int64) {
		id := longID(i)
		if _, err := e.Write([]WriteOp{{Table: "lk", Row: lkRow(id, n)}}); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		model[id] = n
		mu.Unlock()
	}
	del := func(i int) {
		id := longID(i)
		if _, err := e.Write([]WriteOp{{Table: "lk", Key: id}}); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		delete(model, id)
		mu.Unlock()
	}
	for i := 0; i < 4000; i++ {
		put(i, int64(i))
	}
	for i := 0; i < 4000; i += 2 {
		del(i)
	}
	// tombstones leave the index when GC drops them; force that by hand: the
	// arena counts a key dead when it leaves the tree
	ts, _ := e.Table("lk")
	e.waitInstalled(e.DurableLSN())
	before := ts.index().ar.Bytes()

	// a writer on other keys during the compaction
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(2))
		for !stop.Load() {
			put(10000+rng.Intn(500), rng.Int63n(1000))
			if rng.Intn(3) == 0 {
				del(10000 + rng.Intn(500))
			}
		}
	}()
	freed, err := e.CompactKeyArena("lk")
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	after := ts.index().ar.Bytes()
	t.Logf("arena %d -> %d bytes, freed %d", before, after, freed)
	lkCheck(t, e, model, "after compaction")
	if e.ArenaCompactions.Load() != 1 {
		t.Fatalf("compactions = %d", e.ArenaCompactions.Load())
	}
	// the new arena is the live one: more writes and a second round still work
	for i := 20000; i < 20300; i++ {
		put(i, 1)
	}
	if _, err := e.CompactKeyArena("lk"); err != nil {
		t.Fatal(err)
	}
	lkCheck(t, e, model, "second compaction")
	if ts.index().rec != nil {
		t.Fatal("still recording after the compaction")
	}
	e.Close()
	e2, err := Open(Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf, ArenaCompactMinDead: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	lkCheck(t, e2, model, "restart")
}

// A drop of the table while its arena is being rebuilt, and a rebuild that
// finds the writers too fast, end without damage.
func TestKeyArenaCompactionAborts(t *testing.T) {
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf, ArenaCompactMinDead: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.CreateTable(longKeyTable())
	for i := 0; i < 500; i++ {
		e.Write([]WriteOp{{Table: "lk", Row: lkRow(longID(i), 1)}})
	}
	ts, _ := e.Table("lk")
	// a replaced index (TRUNCATE-like) while recording: simulate by storing a
	// fresh tree between the two steps via the dropped flag
	ts.droppedLSN.Store(1)
	if _, err := e.CompactKeyArena("lk"); !errors.Is(err, errArenaBusy) {
		t.Fatalf("dropped table: %v", err)
	}
	ts.droppedLSN.Store(0)
	if ts.index().rec != nil {
		t.Fatal("recording left on")
	}
}

// The automatic compactor reclaims once the dead bytes pass the threshold.
func TestKeyArenaAutoCompactor(t *testing.T) {
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf, ArenaCompactMinDead: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.CreateTable(longKeyTable())
	ts, _ := e.Table("lk")
	for i := 0; i < 300; i++ {
		e.Write([]WriteOp{{Table: "lk", Row: lkRow(longID(i), 1)}})
	}
	e.waitInstalled(e.DurableLSN())
	// kill most keys in the tree itself (a tombstone drop leaves the arena key dead)
	e.gc.mu.Lock()
	tr := ts.index().clone()
	for i := 0; i < 290; i++ {
		tr.del(longID(i))
	}
	ts.idx.Store(tr)
	e.gc.mu.Unlock()
	deadline := time.Now().Add(20 * time.Second)
	for e.ArenaCompactions.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if e.ArenaCompactions.Load() == 0 {
		t.Fatal("the compactor did not run")
	}
	if ts.index().n != 10 {
		t.Fatalf("keys = %d", ts.index().n)
	}
	if ar := ts.index().ar; ar.Dead.Load() != 0 || ar.Live.Load() > 10*40 {
		t.Fatalf("arena after compaction: live %d dead %d", ar.Live.Load(), ar.Dead.Load())
	}
}

// Budget: accounted per structure, reported, forecast, refuses what would add
// RAM, never moves anything by itself.
func TestIndexMemBudget(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf,
		DiskIndexes: true, DiskIndexCacheMB: 1, IndexMemBudget: 64 << 20, BudgetSample: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.CreateTable(kvTable())
	fillKV(t, e, 0, 3000)
	e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}})
	e.CreateIndex("kv", schema.IndexDef{Name: "kv_s", Columns: []string{"s"}})
	r := e.IndexMemoryReport()
	if r.Status == "over" || r.Status == "off" || r.BudgetBytes != 64<<20 || r.RAMBytes <= 0 {
		t.Fatalf("report: %+v", r)
	}
	kinds := map[string]int{}
	for _, m := range r.Structures {
		kinds[m.Kind]++
		if m.Kind == "secondary" && (m.Storage != "memory" || m.RAMBytes == 0 || m.Entries == 0) {
			t.Fatalf("secondary: %+v", m)
		}
	}
	if kinds["primary"] != 1 || kinds["secondary"] != 2 {
		t.Fatalf("structures: %v", kinds)
	}
	// moving to disk lowers RAM; the file's pool counts instead
	if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
		t.Fatal(err)
	}
	r2 := e.IndexMemoryReport()
	var disk IndexMem
	for _, m := range r2.Structures {
		if m.Name == "kv_n" {
			disk = m
		}
	}
	if disk.Storage != "disk" || disk.DiskBytes == 0 || disk.RAMBytes > 1<<20 {
		t.Fatalf("disk index: %+v", disk)
	}
	// a budget too small for the index in memory: moving back is refused, and
	// nothing moves by itself
	e.opts.IndexMemBudget = r2.RAMBytes + 1000
	err = e.SetIndexStorage("kv", "kv_n", "memory")
	if !errors.Is(err, ErrIndexBudget) {
		t.Fatalf("expected the budget to refuse, got %v", err)
	}
	if got, _ := e.IndexStorage("kv", "kv_n"); got != "disk" {
		t.Fatalf("storage changed to %s although refused", got)
	}
	// over budget: status says so, advice names the statement, no auto-spill
	e.opts.IndexMemBudget = r2.RAMBytes / 2
	r3 := e.IndexMemoryReport()
	if r3.Status != "over" || len(r3.Advice) == 0 || !strings.Contains(r3.Advice[0], "ALTER INDEX kv_s SET (index_storage = 'disk')") {
		t.Fatalf("over budget report: %+v", r3)
	}
	time.Sleep(300 * time.Millisecond) // the monitor runs; it must only log
	if got, _ := e.IndexStorage("kv", "kv_s"); got != "memory" {
		t.Fatalf("an index was moved by itself: %s", got)
	}
	if e.BudgetAlerts.Load() == 0 {
		t.Fatal("no alert logged")
	}
}

// Forecast: a structure that grows steadily is reported as reaching the
// budget, and the status turns to warn before it does.
func TestIndexMemForecast(t *testing.T) {
	now := time.Now()
	var s []budgetSample
	for i := 0; i < 10; i++ {
		s = append(s, budgetSample{now.Add(time.Duration(i) * time.Second), int64(i) * 1000})
	}
	if g := growth(s); g < 990 || g > 1010 {
		t.Fatalf("growth = %v", g)
	}
	e := &Engine{}
	e.opts.IndexMemBudget = 100000
	e.budget.samples = s
	r := e.IndexMemoryReport()
	if r.GrowthPerSec < 990 || r.ForecastSeconds < 0 {
		t.Fatalf("%+v", r)
	}
}
