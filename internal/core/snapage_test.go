package core

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"stask/internal/schema"
)

// A stale snapshot older than MaxSnapshotAge stops holding back the
// horizon, and its transaction then fails with ErrSnapshotTooOld.
func TestMaxSnapshotAge(t *testing.T) {
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 256 << 10, NoCheckpoint: true, Logf: t.Logf,
		GCRetention: -1, MaxSnapshotAge: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.CreateTable(kvTable())
	put := func(k string, n int64) {
		t.Helper()
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(k, n)}}); err != nil {
			t.Fatal(err)
		}
	}
	put("a", 1)
	old := e.Begin(SnapshotIsolation)
	defer old.Abort()
	if _, _, err := old.Get("kv", "a"); err != nil {
		t.Fatal(err)
	}
	// a snapshot at the durable point never expires: nothing can be
	// reclaimed for it anyway, and new transactions still pin that LSN
	idle := e.Begin(SnapshotIsolation)
	put("a", 2) // old and idle are now stale
	if h := e.horizon(); h != old.Snapshot() {
		t.Fatalf("horizon %d, want the pinned snapshot %d", h, old.Snapshot())
	}
	time.Sleep(100 * time.Millisecond)
	if st := e.GCStats(); st.OldestSnapshotAgeMs < 50 || st.SnapshotCut != 0 {
		t.Fatalf("stats before the cap: %+v", st)
	}
	time.Sleep(300 * time.Millisecond)
	fresh := e.Begin(SnapshotIsolation)
	defer fresh.Abort()
	if h := e.horizon(); h <= old.Snapshot() || h != fresh.Snapshot() {
		t.Fatalf("horizon %d: want it past the expired snapshot %d, at the fresh one %d", h, old.Snapshot(), fresh.Snapshot())
	}
	st := e.GCStats()
	t.Logf("%+v", st)
	if st.SnapshotCut <= old.Snapshot() || st.OldestSnapshotAgeMs != 0 {
		t.Fatalf("stats after the cap: %+v", st)
	}
	if _, _, err := old.Get("kv", "a"); !errors.Is(err, ErrSnapshotTooOld) {
		t.Fatalf("read on the expired snapshot: %v", err)
	}
	if err := old.ScanAll("kv", func(string, schema.Row) bool { return true }); !errors.Is(err, ErrSnapshotTooOld) {
		t.Fatalf("scan on the expired snapshot: %v", err)
	}
	if _, err := idle.Commit(); !errors.Is(err, ErrSnapshotTooOld) {
		t.Fatalf("commit on the expired snapshot: %v", err)
	}
	if r, _, err := fresh.Get("kv", "a"); err != nil || r[1].I != 2 {
		t.Fatalf("fresh transaction: %v %v", r, err)
	}
	if _, err := fresh.Commit(); err != nil {
		t.Fatal(err)
	}

	// no writes since Begin: the snapshot is the durable point and stays
	// valid however old it gets
	quiet := e.Begin(SnapshotIsolation)
	time.Sleep(400 * time.Millisecond)
	e.horizon()
	if _, _, err := quiet.Get("kv", "a"); err != nil {
		t.Fatalf("current snapshot expired: %v", err)
	}
	quiet.Put("kv", row("b", 1))
	if _, err := quiet.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestScanInterrupt(t *testing.T) {
	e := open(t, t.TempDir(), 4<<20)
	defer e.Close()
	e.CreateTable(kvTable())
	const n = 20000
	for i := 0; i < n; i += 1000 {
		ops := make([]WriteOp, 0, 1000)
		for j := i; j < i+1000; j++ {
			ops = append(ops, WriteOp{Table: "kv", Row: row(fmt.Sprintf("k%06d", j), int64(j))})
		}
		if _, err := e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	tx := e.Begin(SnapshotIsolation)
	defer tx.Abort()
	polls := 0
	tx.SetInterrupt(func() bool { polls++; return polls >= 3 })
	seen := 0
	err := tx.ScanAll("kv", func(string, schema.Row) bool { seen++; return true })
	if !errors.Is(err, ErrInterrupted) || seen >= n || seen < 2*interruptRows {
		t.Fatalf("ScanAll: err=%v after %d rows", err, seen)
	}
	stop := false
	tx.SetInterrupt(func() bool { return stop })
	spawn := func(f func()) bool { go f(); return true }
	cnt := 0
	ok, err := tx.ScanAllParallel("kv", nil, 0, spawn, func(int) bool { return true }, func(w, seq, idx int, r schema.Row) bool {
		if cnt++; cnt == ScanChunk {
			stop = true
		}
		return true
	})
	if !ok || !errors.Is(err, ErrInterrupted) || cnt >= n {
		t.Fatalf("ScanAllParallel: ok=%v err=%v after %d rows", ok, err, cnt)
	}
}
