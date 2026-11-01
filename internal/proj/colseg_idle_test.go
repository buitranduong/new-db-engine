package proj

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// An idle ring is trimmed to the newest state: the base replaced by a merge
// becomes garbage-collectable (finalizer fires), while a reader that kept an
// older state still sees it, and a snapshot older than the ring finds no state.
func TestSegIdleRingTrimReleasesOldBase(t *testing.T) {
	oldMin, oldFrac, oldIdle := SegMergeMinTail, SegMergeFrac, SegIdleTrim
	SegMergeMinTail, SegMergeFrac, SegIdleTrim = 50, 4, 100*time.Millisecond
	t.Cleanup(func() { SegMergeMinTail, SegMergeFrac, SegIdleTrim = oldMin, oldFrac, oldIdle })
	e, c := segTestEngine(t)
	t.Cleanup(c.Stop)
	ts, err := e.UserTable("sims")
	if err != nil {
		t.Fatal(err)
	}
	id := ts.Schema().ID

	for d := time.Now().Add(10 * time.Second); !c.Ready() && time.Now().Before(d); {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.Ready() {
		t.Fatal("segment state not built")
	}
	var freed atomic.Bool
	func() { // the first (bootstrap) base: only the test closure and the ring may reference it
		b := c.Current().Tbls[id].Base
		runtime.SetFinalizer(b, func(*SegBase) { freed.Store(true) })
	}()
	firstLSN := c.Current().LSN

	var last uint64
	for b := 0; b < 6; b++ {
		tx := e.Begin(core.SnapshotIsolation)
		for i := 0; i < 40; i++ {
			_ = tx.Put("sims", schema.Row{{S: fmt.Sprintf("09%09d", b*40+i)}, {S: "TAM_HOA"}, {I: 1000000}, {S: "A"}, {I: 7}})
		}
		l, err := tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		last = l
	}
	waitSeg(t, c, last)
	deadline := time.Now().Add(20 * time.Second)
	for c.Merges.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		c.Follower().WaitApplied(last, 10*time.Millisecond)
		c.Flush(last)
	}
	if c.Merges.Load() == 0 {
		t.Fatal("no merge finished")
	}
	// a reader (explicit transaction) pins the state of the moment, which still holds the first base
	pinned := c.StateAt(firstLSN)
	if pinned == nil || pinned.Tbls[id].Base.N() != 0 {
		t.Fatal("expected the bootstrap state in the ring")
	}
	pinnedRows := pinned.Tbls[id].Base.N()

	waitTrim := func() {
		d := time.Now().Add(10 * time.Second)
		for time.Now().Before(d) {
			c.Flush(last)
			if n, _ := c.RingStats(); n == 1 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		n, b := c.RingStats()
		t.Fatalf("idle ring was not trimmed: %d states, %d bases", n, b)
	}
	waitTrim()
	if _, b := c.RingStats(); b != 1 {
		t.Fatalf("after the trim the ring holds %d bases, want 1", b)
	}
	if c.IdleTrims.Load() == 0 {
		t.Fatal("IdleTrims not counted")
	}
	if c.StateAt(firstLSN) != nil {
		t.Fatal("a snapshot older than the trimmed ring must find no state (fall-back)")
	}
	if cur := c.Current(); cur == nil || cur.LSN < last || StateHolds(cur, id) == 0 {
		t.Fatal("the newest state must stay")
	}
	// the pinned reader still sees its state; the old base is NOT collectable yet
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if freed.Load() {
		t.Fatal("the base was freed while a reader still pinned it")
	}
	if pinned.Tbls[id].Base.N() != pinnedRows || pinned.LSN != firstLSN {
		t.Fatal("the pinned state changed")
	}
	runtime.KeepAlive(pinned)
	pinned = nil
	d := time.Now().Add(10 * time.Second)
	for !freed.Load() && time.Now().Before(d) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	if !freed.Load() {
		t.Fatal("the old base was not garbage-collectable after the trim and the release of the pin")
	}
	// a new publish after the trim refills the ring normally
	tx := e.Begin(core.SnapshotIsolation)
	_ = tx.Put("sims", schema.Row{{S: "09999999999"}, {S: "TAM_HOA"}, {I: 1}, {S: "A"}, {I: 7}})
	l, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	waitSeg(t, c, l)
	if n, _ := c.RingStats(); n < 2 {
		t.Fatalf("ring %d after a new publish", n)
	}
}

// StateHolds is the number of base rows plus tail rows of table id in s.
func StateHolds(s *SegState, id uint32) int {
	t := s.Tbls[id]
	if t == nil {
		return 0
	}
	return t.Base.N() + t.Tail.n
}
