package repl

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"stask/internal/core"
)

// TestCDCStateSurvivesFailover (P6.3): a slot, a publication and a replica
// identity created on the leader are on a replica, and when the replica is
// promoted (the leader is gone, the replica directory is opened as a
// leader) the slot is where the subscriber confirmed it and streams
// exactly the transactions the subscriber has not confirmed: none lost,
// none twice.
func TestCDCStateSurvivesFailover(t *testing.T) {
	base := t.TempDir()
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	if err := leader.CreatePublication(core.Publication{Name: "p", Tables: []string{"kv"}, Insert: true, Update: true, Delete: true}); err != nil {
		t.Fatal(err)
	}
	if err := leader.SetReplicaIdentityFull("kv", true); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.CreateSlot("s", "pgoutput"); err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(leader, "127.0.0.1:0", nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	rdir := filepath.Join(base, "replica")
	segMax, err := Prepare(srv.Addr(), rdir, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); Follow(replica, srv.Addr(), nil, stop, t.Logf) }()

	for i := 0; i < 5; i++ {
		if _, err := leader.Write([]core.WriteOp{{Table: "kv", Row: row(fmt.Sprintf("a%d", i), 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	// the subscriber applies the five transactions and confirms
	var end = leader.Slots()[0].Pos()
	n, err := leader.SlotChanges("s", core.ChangeOpts{}, func(c *core.Change) error {
		if c.Kind == core.ChangeCommit {
			end = c.End
		}
		return nil
	})
	if err != nil || n != 5 {
		t.Fatalf("subscriber got %d transactions: %v", n, err)
	}
	confirmed, err := leader.ConfirmSlot("s", end)
	if err != nil {
		t.Fatal(err)
	}
	// three more, not yet confirmed when the leader dies
	for i := 0; i < 3; i++ {
		if _, err := leader.Write([]core.WriteOp{{Table: "kv", Row: row(fmt.Sprintf("b%d", i), 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	waitLSN(t, replica, leader.DurableLSN())
	close(stop)
	wg.Wait()
	replica.Close()
	srv.Close()
	leader.Close() // the leader dies

	p, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Logf: t.Logf})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	defer p.Close()
	if ps := p.Publications(); len(ps) != 1 || ps[0].Name != "p" || !ps[0].Has("kv") {
		t.Fatalf("publications after promotion: %+v", ps)
	}
	if !p.ReplicaIdentityFull("kv") {
		t.Fatal("replica identity lost")
	}
	ss := p.Slots()
	if len(ss) != 1 || ss[0].Name != "s" || ss[0].Pos() != confirmed {
		t.Fatalf("slots after promotion: %+v, want s at %v", ss, confirmed)
	}
	var keys []string
	if _, err := p.SlotChanges("s", core.ChangeOpts{}, func(c *core.Change) error {
		if c.Kind == core.ChangeInsert {
			keys = append(keys, c.Key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != "[b0 b1 b2]" {
		t.Fatalf("after promotion the slot streams %v, want [b0 b1 b2]", keys)
	}
	// and the promoted node carries on: a new write reaches the slot
	if _, err := p.Write([]core.WriteOp{{Table: "kv", Row: row("c0", 1)}}); err != nil {
		t.Fatal(err)
	}
}
