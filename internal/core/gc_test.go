package core

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/schema"
	"stask/internal/wal"
)

func openGC(t *testing.T, dir string) *Engine {
	t.Helper()
	e, err := Open(Options{Dir: dir, SegmentSize: 256 << 10, NoCheckpoint: true, Logf: t.Logf,
		GCRetention: -1, GCMaxLiveRatio: 0.9, GCRateBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func walFiles(t *testing.T, dir string) int {
	m, _ := filepath.Glob(filepath.Join(dir, "wal", "*.wal"))
	return len(m)
}

// verify compares the engine with the model (key -> n, absent = deleted).
func verify(t *testing.T, e *Engine, model map[string]int64) {
	t.Helper()
	for k, n := range model {
		r, _, err := e.Get("kv", k)
		if err != nil || r == nil || r[1].I != n || r[2].S != row(k, n)[2].S {
			t.Fatalf("key %s: got %v err=%v, want n=%d", k, r, err, n)
		}
	}
	rows, err := e.ScanRange("kv", "", "", 1<<30, e.DurableLSN())
	if err != nil || len(rows) != len(model) {
		t.Fatalf("scan: %d rows (want %d) err=%v", len(rows), len(model), err)
	}
	tb, _ := e.Table("kv")
	if tb.LiveRows() != int64(len(model)) {
		t.Fatalf("live rows %d, want %d", tb.LiveRows(), len(model))
	}
}

func TestGCReclaimsAndRecovers(t *testing.T) {
	dir := t.TempDir()
	e := openGC(t, dir)
	e.CreateTable(kvTable())
	model := map[string]int64{}
	put := func(k string, n int64) {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(k, n)}}); err != nil {
			t.Fatal(err)
		}
		model[k] = n
	}
	for i := 0; i < 1000; i++ {
		put(fmt.Sprint(i), 0)
	}
	oldSnap := e.DurableLSN()
	for round := int64(1); round <= 6; round++ {
		for i := 0; i < 1000; i++ {
			put(fmt.Sprint(i), round)
		}
	}
	for i := 0; i < 1000; i += 4 {
		if _, err := e.Write([]WriteOp{{Table: "kv", Key: fmt.Sprint(i)}}); err != nil {
			t.Fatal(err)
		}
		delete(model, fmt.Sprint(i))
	}
	before := walFiles(t, dir)
	n, err := e.GCRound()
	if err != nil || n == 0 {
		t.Fatalf("gc round: reclaimed %d, err %v", n, err)
	}
	after := walFiles(t, dir)
	st := e.GCStats()
	t.Logf("segments %d -> %d, %+v", before, after, st)
	if after > before/2 {
		t.Fatalf("expected most segments to be reclaimed: %d -> %d", before, after)
	}
	verify(t, e, model)
	if _, _, err := e.GetAt("kv", "1", oldSnap); !errors.Is(err, ErrSnapshotTooOld) {
		t.Fatalf("read below the floor must fail, got %v", err)
	}
	if h, err := e.History("kv", "1", 100); err != nil || len(h) == 0 || h[0].Row[1].I != 6 {
		t.Fatalf("history after gc: %+v %v", h, err)
	}

	// more writes after GC, then restart without a final checkpoint
	for i := 0; i < 300; i++ {
		put(fmt.Sprint(i), 100)
	}
	e.Close()
	e = openGC(t, dir)
	verify(t, e, model)
	// second generation of reclamation, then restart again
	for i := 1; i < 1000; i += 3 {
		put(fmt.Sprint(i), 200)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := e.GCRound(); err != nil {
		t.Fatal(err)
	}
	verify(t, e, model)
	e.Close()
	e = openGC(t, dir)
	defer e.Close()
	verify(t, e, model)
}

func TestGCRespectsPinnedSnapshot(t *testing.T) {
	e := openGC(t, t.TempDir())
	defer e.Close()
	e.CreateTable(kvTable())
	// versions superseded before the snapshot is pinned are garbage; the
	// last of them stay needed by it and must be relocated, not lost
	for i := 0; i < 500; i++ {
		for r := int64(-2); r <= 0; r++ { // same segments: 2/3 garbage, 1/3 needed
			e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), r)}})
		}
	}
	tx := e.Begin(SnapshotIsolation)
	for r := int64(1); r <= 8; r++ {
		for i := 0; i < 500; i++ {
			e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), r)}})
		}
		// reclaim while the old versions are still needed by tx: they must
		// be relocated, not lost
		if _, err := e.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%+v", e.GCStats())
	if e.Stats.GCReclaimedSegs.Load() == 0 || e.GCStats().Remapped == 0 {
		t.Fatal("expected reclamation with relocated history while a snapshot is pinned")
	}
	for i := 0; i < 500; i++ {
		r, ok, err := tx.Get("kv", fmt.Sprint(i))
		if err != nil || !ok || r[1].I != 0 {
			t.Fatalf("pinned snapshot lost key %d: %v %v %v", i, r, ok, err)
		}
	}
	tx.Abort()
	for i := 0; i < 3; i++ {
		if _, err := e.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.GCStats(); st.Remapped != 0 {
		t.Fatalf("remap entries must be released once no snapshot needs them: %+v", st)
	}
}

// keyTracker is a consumer that records the last LSN applied per key.
type keyTracker struct {
	mu   sync.Mutex
	last map[string]int64
}

func (k *keyTracker) Name() string { return "tracker" }
func (k *keyTracker) Bootstrap(e *Engine, snap uint64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.last = map[string]int64{}
	return e.ScanSnapshot("kv", snap, func(key string, r schema.Row, _ *Version) bool {
		k.last[key] = r[1].I
		return true
	})
}
func (k *keyTracker) Apply(_ *Engine, rec *wal.Record) {
	if rec.Kind == wal.KindMove {
		panic("follower passed a relocation record to a consumer")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, op := range rec.Ops {
		if op.Kind == wal.OpDel {
			delete(k.last, string(op.Key))
			continue
		}
		var s schema.Table = kvTable()
		s.Init()
		r, _ := s.Decode(op.Val)
		k.last[string(op.Key)] = r[1].I
	}
}
func (k *keyTracker) Flush(uint64) {}

// Writers, transactional readers, scans and a log follower run while
// reclamation rounds run back to back.
func TestGCConcurrent(t *testing.T) {
	dir := t.TempDir()
	e := openGC(t, dir)
	e.CreateTable(kvTable())
	const keys = 400
	var mu sync.Mutex
	model := map[string]int64{}
	tr := &keyTracker{}
	f := e.Follow(tr)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var ops, readErrs atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for n := int64(1); ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				k := fmt.Sprint(rng.Intn(keys/4)*4 + w) // writers own disjoint keys
				var op WriteOp
				del := rng.Intn(10) == 0
				if del {
					op = WriteOp{Table: "kv", Key: k}
				} else {
					op = WriteOp{Table: "kv", Row: row(k, n)}
				}
				if _, err := e.Write([]WriteOp{op}); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if del {
					delete(model, k)
				} else {
					model[k] = n
				}
				mu.Unlock()
				ops.Add(1)
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tx := e.Begin(SnapshotIsolation)
				first := map[string]int64{}
				for k := 0; k < keys; k += 20 {
					r, _, err := tx.Get("kv", fmt.Sprint(k))
					if err != nil {
						readErrs.Add(1)
						t.Errorf("tx read: %v", err)
						return
					}
					if r != nil {
						first[fmt.Sprint(k)] = r[1].I
					}
				}
				time.Sleep(20 * time.Millisecond) // let writers and GC move on
				rows, err := tx.Scan("kv", "", "", 1<<20)
				if err != nil {
					t.Errorf("tx scan: %v", err)
					return
				}
				for k := 0; k < keys; k += 20 {
					r, _, err := tx.Get("kv", fmt.Sprint(k))
					if err != nil {
						t.Errorf("tx re-read: %v", err)
						return
					}
					was, had := first[fmt.Sprint(k)]
					if (r != nil) != had || r != nil && r[1].I != was {
						t.Errorf("snapshot changed under a transaction: key %d %v vs %v", k, r, was)
						return
					}
				}
				tx.Abort()
				_ = rows
			}
		}()
	}
	deadline := time.Now().Add(4 * time.Second)
	var reclaimed int
	for time.Now().Before(deadline) {
		n, err := e.GCRound()
		if err != nil {
			t.Fatal(err)
		}
		reclaimed += n
	}
	close(stop)
	wg.Wait()
	t.Logf("ops=%d reclaimed=%d segments on disk=%d %+v", ops.Load(), reclaimed, walFiles(t, dir), e.GCStats())
	if reclaimed == 0 {
		t.Fatal("nothing reclaimed")
	}
	verify(t, e, model)
	if !f.WaitApplied(e.DurableLSN(), 5*time.Second) {
		t.Fatal("follower did not catch up")
	}
	tr.mu.Lock()
	if len(tr.last) != len(model) {
		t.Fatalf("follower has %d keys, core %d", len(tr.last), len(model))
	}
	for k, n := range model {
		if tr.last[k] != n {
			t.Fatalf("follower key %s = %d, want %d", k, tr.last[k], n)
		}
	}
	tr.mu.Unlock()
	e.Close()
	e = openGC(t, dir)
	defer e.Close()
	verify(t, e, model)
	_ = os.Remove
}

// TestVersionTS: the commit time from the ring or a record's header is the
// one a full load gives, for a key of a multi-operation transaction and for
// versions the log cleaner relocated (whose records carry a commit time per
// operation).
func TestVersionTS(t *testing.T) {
	e := openGC(t, t.TempDir())
	e.CreateTable(kvTable())
	var moved bool
	same := func(k string) {
		t.Helper()
		_, v, err := e.Get("kv", k)
		if err != nil || v == nil {
			t.Fatalf("get %s: %v", k, err)
		}
		w := *v
		ts, err := e.VersionTS(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Fill(&w); err != nil {
			t.Fatal(err)
		}
		if ts != w.TS || ts == 0 {
			t.Fatalf("key %s: VersionTS %d, full load %d", k, ts, w.TS)
		}
		// the header path, which the ring of recent commits hides here
		kind, lsn, hts, err := e.log.ReadHeader(v.Loc.Seg, int64(v.Loc.Off))
		if err != nil {
			t.Fatal(err)
		}
		if kind != wal.KindMove && (hts != w.TS || lsn != w.LSN) {
			t.Fatalf("key %s: header ts %d lsn %d, full load ts %d lsn %d", k, hts, lsn, w.TS, w.LSN)
		}
		moved = moved || kind == wal.KindMove
	}

	var ops []WriteOp
	for i := 0; i < 50; i++ {
		ops = append(ops, WriteOp{Table: "kv", Row: row(fmt.Sprint(i), 1)})
	}
	if _, err := e.Write(ops); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // a later commit time for the rest
	for i := 50; i < 1000; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"0", "49", "50", "999"} {
		same(k)
	}
	// overwrite most keys so that the cleaner relocates the rest
	for round := 0; round < 4; round++ {
		for i := 100; i < 1000; i++ {
			if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(round))}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := e.GCRound(); err != nil || n == 0 {
		t.Fatalf("gc round: reclaimed %d, err %v", n, err)
	}
	if e.GCStats().MovedVersions == 0 {
		t.Fatal("no version was relocated")
	}
	for _, k := range []string{"0", "49", "50", "99", "500"} {
		same(k)
	}
	if !moved {
		t.Fatal("no relocated version was read")
	}
}
