package repl

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// TestReplicaHistory: the leader keeps 6 days of history and relocates old
// versions while reclaiming; the replica, which only follows the log and
// removes the segments the leader reclaimed, reads the same history, also
// after a restart (the relocation table is not shipped, the replica derives
// it from the relocation records).
func TestReplicaHistory(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cmu sync.Mutex
	clock := func() time.Time { cmu.Lock(); defer cmu.Unlock(); return now }
	advance := func(d time.Duration) { cmu.Lock(); now = now.Add(d); cmu.Unlock() }
	const day = 24 * time.Hour
	const keys, ret = 500, 6 * day
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 1 << 20, NoCheckpoint: true,
		Logf: t.Logf, GCRetention: ret, GCMaxLiveRatio: 0.9, GCRateBytes: -1, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	if _, err := leader.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(leader, "127.0.0.1:0", nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	write := func(from, to int) {
		for d := from; d < to; d++ {
			advance(day)
			leader.MarkNow()
			var ops []core.WriteOp
			for i := 0; i < keys; i++ {
				if i%10 == 0 || (i+d)%4 == 0 {
					ops = append(ops, core.WriteOp{Table: "kv", Row: row(fmt.Sprint(i), int64(d))})
				}
			}
			if _, err := leader.Write(ops); err != nil {
				t.Fatal(err)
			}
			leader.MarkNow()
		}
	}
	write(0, 14)

	openReplicaAt := func(rdir string) (*core.Engine, func()) {
		segMax, err := Prepare(srv.Addr(), rdir, nil, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		r, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf, Now: clock, GCRetention: ret})
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		var fw sync.WaitGroup
		fw.Add(1)
		go func() { defer fw.Done(); Follow(r, srv.Addr(), nil, stop, t.Logf) }()
		return r, func() { close(stop); fw.Wait(); r.Close() }
	}
	rdir := filepath.Join(base, "replica")
	openReplica := func() (*core.Engine, func()) { return openReplicaAt(rdir) }
	replica, closeReplica := openReplica()
	defer func() { closeReplica() }()
	waitLSN(t, replica, leader.DurableLSN())

	write(14, 30)
	waitLSN(t, replica, leader.DurableLSN())
	for i := 0; i < 8; i++ {
		if _, err := leader.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
	st := leader.GCStats()
	if st.ReclaimedSegments == 0 || st.Remapped == 0 {
		t.Fatalf("the test must reclaim and relocate old versions: %+v", st)
	}
	waitLSN(t, replica, leader.DurableLSN())
	deadline := time.Now().Add(15 * time.Second)
	for replica.OldestSegment() < leader.OldestSegment() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if replica.OldestSegment() < leader.OldestSegment() {
		t.Fatalf("replica keeps segments from %d, leader from %d", replica.OldestSegment(), leader.OldestSegment())
	}

	cut := clock().Add(-ret).UnixNano()
	compare := func(label string) {
		t.Helper()
		inWindow := 0
		for i := 0; i < keys; i++ {
			k := fmt.Sprint(i)
			a, err := leader.History("kv", k, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			b, err := replica.History("kv", k, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			// the replica may keep more at the old end (it trims whole
			// segments the leader removed), never less inside the window
			gotBase := false
			bm := map[uint64]schema.Row{}
			for _, v := range b {
				bm[v.LSN] = v.Row
			}
			for _, v := range a {
				if gotBase {
					break // older than the version current when the window began: may be gone
				}
				if v.TS <= cut {
					gotBase = true
				}
				r, ok := bm[v.LSN]
				if !ok || r[1].I != v.Row[1].I {
					t.Fatalf("%s: key %s: version lsn %d (n=%d) is on the leader but %v on the replica (leader %d versions, replica %d)",
						label, k, v.LSN, v.Row[1].I, ok, len(a), len(b))
				}
				if v.TS > cut {
					inWindow++
				}
			}
		}
		t.Logf("%s: %d versions inside the window identical", label, inWindow)
		// AS OF a time inside the window, whole table, leader against replica
		at := clock().Add(-4 * day).UnixNano()
		lt, err := leader.TimeSnapshot(at)
		if err != nil {
			t.Fatal(err)
		}
		rt, err := replica.TimeSnapshot(at)
		if err != nil {
			t.Fatal(err)
		}
		got := func(e *core.Engine, ts core.TimeSnap) map[string]int64 {
			m := map[string]int64{}
			if err := e.ScanSnapshotTime("kv", ts, func(k string, r schema.Row, _ *core.Version) bool { m[k] = r[1].I; return true }); err != nil {
				t.Fatalf("scan at %+v (replica=%v): %v; stats %+v", ts, e.IsReplica(), err, e.GCStats())
			}
			return m
		}
		lm, rm := got(leader, lt), got(replica, rt)
		if len(lm) != len(rm) || len(lm) == 0 {
			t.Fatalf("%s: AS OF -4d: leader %d rows, replica %d", label, len(lm), len(rm))
		}
		for k, v := range lm {
			if rm[k] != v {
				t.Fatalf("%s: AS OF -4d key %s: leader %d, replica %d", label, k, v, rm[k])
			}
		}
	}
	compare("after trim")

	// restart the replica: its relocation table comes back from disk
	closeReplica()
	replica, closeReplica = openReplica()
	waitLSN(t, replica, leader.DurableLSN())
	compare("after replica restart")

	// a replica created after the leader relocated old versions gets the
	// relocation table with its base backup
	closeReplica()
	replica, closeReplica = openReplicaAt(filepath.Join(base, "replica2"))
	waitLSN(t, replica, leader.DurableLSN())
	compare("replica created after relocation")
}
