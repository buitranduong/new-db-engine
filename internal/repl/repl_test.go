package repl

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

func kvTable() schema.Table {
	return schema.Table{Name: "kv", Key: "id", Columns: []schema.Column{
		{Name: "id", Type: "string"}, {Name: "n", Type: "int"}, {Name: "s", Type: "string"},
	}}
}

func row(id string, n int64) schema.Row {
	return schema.Row{{S: id}, {I: n}, {S: fmt.Sprintf("v-%s-%d-%0200d", id, n, n)}}
}

// dump is the table at snapshot snap, key -> row.
func dump(t *testing.T, e *core.Engine, snap uint64) map[string]string {
	t.Helper()
	m := map[string]string{}
	if err := e.ScanSnapshot("kv", snap, func(k string, r schema.Row, _ *core.Version) bool {
		m[k] = fmt.Sprint(r)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

func waitLSN(t *testing.T, e *core.Engine, lsn uint64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for e.DurableLSN() < lsn {
		if time.Now().After(deadline) {
			t.Fatalf("replica at lsn %d, leader at %d", e.DurableLSN(), lsn)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// same checks that the replica holds exactly the leader's data at the
// leader's current durable point.
func same(t *testing.T, leader, replica *core.Engine) {
	t.Helper()
	l := leader.DurableLSN()
	waitLSN(t, replica, l)
	want, got := dump(t, leader, l), dump(t, replica, l)
	if len(want) != len(got) {
		t.Fatalf("lsn %d: leader %d rows, replica %d", l, len(want), len(got))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("lsn %d key %s: leader %q, replica %q", l, k, v, got[k])
		}
	}
	lt, _ := leader.Table("kv")
	rt, _ := replica.Table("kv")
	if lt.LiveRows() != rt.LiveRows() {
		t.Fatalf("live rows: leader %d, replica %d", lt.LiveRows(), rt.LiveRows())
	}
	t.Logf("lsn %d: %d rows identical", l, len(want))
}

// writer runs puts, deletes, large transactions (split into several log
// frames that cross segments) and leader log reclamation until stop.
func writer(t *testing.T, e *core.Engine, stop chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	r := rand.New(rand.NewSource(1))
	for i := int64(1); ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		var ops []core.WriteOp
		switch x := r.Intn(100); {
		case x < 2: // large transaction
			for j := 0; j < 70000; j++ {
				ops = append(ops, core.WriteOp{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("big-%d", j)}, {I: i}, {S: "x"}}})
			}
		case x < 20:
			ops = []core.WriteOp{{Table: "kv", Key: fmt.Sprint(r.Intn(3000))}}
		default:
			for j := 0; j < 1+r.Intn(5); j++ {
				ops = append(ops, core.WriteOp{Table: "kv", Row: row(fmt.Sprint(r.Intn(3000)), i)})
			}
		}
		if _, err := e.Write(ops); err != nil {
			t.Error(err)
			return
		}
		if i%300 == 0 {
			if _, err := e.GCRound(); err != nil {
				t.Error(err)
				return
			}
		}
	}
}

func TestReplicaFollowsLeader(t *testing.T) {
	base := t.TempDir()
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 1 << 20,
		NoCheckpoint: true, Logf: t.Logf, GCRetention: -1, GCMaxLiveRatio: 0.9, GCRateBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	if _, err := leader.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if _, err := leader.Write([]core.WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), 0)}}); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := Serve(leader, "127.0.0.1:0", nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	stopW := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go writer(t, leader, stopW, &wg)

	// base backup and catch-up while the leader writes and reclaims
	rdir := filepath.Join(base, "replica")
	openReplica := func() (*core.Engine, chan struct{}, *sync.WaitGroup) {
		segMax, err := Prepare(srv.Addr(), rdir, nil, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		// PreZero creates the next segment ahead of time, as staskd does by
		// default: a restart must still continue in the current segment
		r, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf, PreZero: true})
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		var fw sync.WaitGroup
		fw.Add(1)
		go func() { defer fw.Done(); Follow(r, srv.Addr(), nil, stop, t.Logf) }()
		return r, stop, &fw
	}
	time.Sleep(500 * time.Millisecond)
	replica, stopF, fw := openReplica()
	time.Sleep(3 * time.Second)
	close(stopW)
	wg.Wait()
	same(t, leader, replica)

	if _, err := replica.Write([]core.WriteOp{{Table: "kv", Row: row("x", 1)}}); !errors.Is(err, core.ErrReadOnly) {
		t.Fatalf("write on replica: %v, want ErrReadOnly", err)
	}

	// restart the replica, then continue with more writes and reclamation
	close(stopF)
	fw.Wait()
	replica.Close()
	stopW = make(chan struct{})
	wg.Add(1)
	go writer(t, leader, stopW, &wg)
	time.Sleep(time.Second)
	replica, stopF, fw = openReplica()
	defer func() { close(stopF); fw.Wait(); replica.Close() }()
	time.Sleep(3 * time.Second)
	close(stopW)
	wg.Wait()
	for i := 0; i < 3; i++ {
		if _, err := leader.GCRound(); err != nil {
			t.Fatal(err)
		}
	}
	same(t, leader, replica)

	// the replica removes what the leader reclaimed (heartbeats carry it)
	deadline := time.Now().Add(10 * time.Second)
	for replica.OldestSegment() < leader.OldestSegment() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if replica.OldestSegment() < leader.OldestSegment() {
		t.Fatalf("replica keeps segments from %d, leader from %d", replica.OldestSegment(), leader.OldestSegment())
	}
	if leader.Stats.GCReclaimedSegs.Load() == 0 || replica.Stats.GCReclaimedSegs.Load() == 0 {
		t.Fatalf("reclaimed segments: leader %d, replica %d (the test must exercise trimming)",
			leader.Stats.GCReclaimedSegs.Load(), replica.Stats.GCReclaimedSegs.Load())
	}
	t.Logf("segments from %d on both; reclaimed: leader %d, replica %d", leader.OldestSegment(),
		leader.Stats.GCReclaimedSegs.Load(), replica.Stats.GCReclaimedSegs.Load())
	same(t, leader, replica)
}
