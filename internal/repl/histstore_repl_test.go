package repl

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/histstore"
)

// TestReplicaHistoryStore: the history store follows a replica's own copy of
// the log. The leader keeps 6 days; the replica's store holds all 30 days
// of every key, also across a restart of the replica and its store, and
// AS OF a time older than the leader's log is answered on the replica.
func TestReplicaHistoryStore(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cmu sync.Mutex
	clock := func() time.Time { cmu.Lock(); defer cmu.Unlock(); return now }
	advance := func(d time.Duration) { cmu.Lock(); now = now.Add(d); cmu.Unlock() }
	const day = 24 * time.Hour
	const keys, ret, days = 500, 6 * day, 30
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 256 << 10, NoCheckpoint: true,
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

	rdir := filepath.Join(base, "replica")
	segMax, err := Prepare(srv.Addr(), rdir, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	type node struct {
		e     *core.Engine
		st    *histstore.Store
		f     *core.Follower
		close func()
	}
	openReplica := func() *node {
		r, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf, Now: clock, GCRetention: ret})
		if err != nil {
			t.Fatal(err)
		}
		st, err := histstore.Open(histstore.Options{Dir: filepath.Join(rdir, "histstore"), Retention: 60 * day, Manual: true, Now: clock, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		c := histstore.NewConsumer(st, "kv")
		f := r.Follow(c)
		c.Attach(r)
		stop := make(chan struct{})
		var fw sync.WaitGroup
		fw.Add(1)
		go func() { defer fw.Done(); Follow(r, srv.Addr(), nil, stop, t.Logf) }()
		return &node{r, st, f, func() { close(stop); fw.Wait(); r.Close(); st.Close() }}
	}
	rep := openReplica()
	defer func() { rep.close() }()

	wrote := map[string][]int64{} // key -> days it was written
	for d := 0; d < days; d++ {
		advance(day)
		leader.MarkNow()
		var ops []core.WriteOp
		for i := 0; i < keys; i++ {
			if i%10 == 0 || (i+d)%4 == 0 {
				ops = append(ops, core.WriteOp{Table: "kv", Row: row(fmt.Sprint(i), int64(d))})
				wrote[fmt.Sprint(i)] = append(wrote[fmt.Sprint(i)], int64(d))
			}
		}
		if _, err := leader.Write(ops); err != nil {
			t.Fatal(err)
		}
		leader.MarkNow()
	}
	waitLSN(t, rep.e, leader.DurableLSN())
	if !rep.f.WaitApplied(rep.e.DurableLSN(), 20*time.Second) {
		t.Fatalf("the store on the replica did not catch up: %d of %d", rep.f.Applied(), rep.e.DurableLSN())
	}
	for i := 0; i < 8; i++ {
		if _, err := leader.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
	waitLSN(t, rep.e, leader.DurableLSN())
	if st := leader.GCStats(); st.ReclaimedSegments == 0 {
		t.Fatalf("the test must reclaim leader log: %+v", st)
	}
	// the log keeps 6 days, so the store only needs its sealed segments for what lies beyond
	if err := rep.st.Maintain(); err != nil {
		t.Fatal(err)
	}
	check := func(label string) {
		t.Helper()
		for i := 0; i < keys; i++ {
			k := fmt.Sprint(i)
			h, err := rep.e.History("kv", k, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			w := wrote[k]
			if len(h) != len(w) {
				t.Fatalf("%s: key %s: %d versions on the replica, %d written", label, k, len(h), len(w))
			}
			for j, v := range h { // newest first
				if want := w[len(w)-1-j]; v.Row[1].I != want {
					t.Fatalf("%s: key %s version %d: %d, want %d", label, k, j, v.Row[1].I, want)
				}
			}
		}
		// AS OF 3.5 days after the start (written by day 2), far older than the leader's 6 day log
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(3*day + 12*time.Hour)
		for _, i := range []int{0, 10, 30} {
			k := fmt.Sprint(i)
			hh, found, err := rep.e.AsOfTime("kv", k, at.UnixNano())
			if err != nil || !found || hh.Row[1].I != 2 {
				t.Fatalf("%s: AS OF day 3 of key %s: %+v %v %v", label, k, hh, found, err)
			}
		}
	}
	check("replica")
	rep.close()
	rep = openReplica()
	waitLSN(t, rep.e, leader.DurableLSN())
	if !rep.f.WaitApplied(rep.e.DurableLSN(), 20*time.Second) {
		t.Fatal("the store did not catch up after the restart")
	}
	if err := rep.st.Maintain(); err != nil {
		t.Fatal(err)
	}
	check("replica after restart")
}
