package core

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"stask/internal/wal"
)

type txLog struct {
	lsn  uint64
	ops  []string // "I k n", "U k n", "D k"
	end  wal.Pos
	unkn int
}

func collect(t *testing.T, e *Engine, slot string, o ChangeOpts) []txLog {
	t.Helper()
	var out []txLog
	var cur *txLog
	_, err := e.SlotChanges(slot, o, func(c *Change) error {
		switch c.Kind {
		case ChangeBegin:
			out = append(out, txLog{lsn: c.LSN})
			cur = &out[len(out)-1]
		case ChangeCommit:
			if c.LSN != cur.lsn {
				t.Fatalf("commit lsn %d inside transaction %d", c.LSN, cur.lsn)
			}
			cur.end = c.End
		case ChangeDelete:
			cur.ops = append(cur.ops, "D "+c.Key)
		default:
			cur.ops = append(cur.ops, fmt.Sprintf("%c %s %d", c.Kind, c.Key, c.Row[1].I))
		}
		if c.OldUnknown {
			cur.unkn++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCDCSlot(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	w := func(ops ...WriteOp) {
		t.Helper()
		if _, err := e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	put := func(k string, n int64) WriteOp { return WriteOp{Table: "kv", Row: row(k, n)} }
	del := func(k string) WriteOp { return WriteOp{Table: "kv", Key: k} }

	w(put("before", 1)) // committed before the slot: not delivered
	if _, err := e.CreateSlot("s1", "wal2json"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateSlot("s1", "wal2json"); !errors.Is(err, ErrSlotExists) {
		t.Fatalf("duplicate slot: %v", err)
	}
	if _, err := e.CreateSlot("Bad-Name", ""); err == nil {
		t.Fatal("bad slot name accepted")
	}
	w(put("a", 1), put("b", 2))
	w(put("a", 10))               // update
	w(del("b"))                   // delete
	w(del("absent"))              // deleting nothing: no change
	w(put("b", 3))                // insert after delete
	w(put("before", 2), del("a")) // update of a row older than the slot, delete
	want := [][]string{{"I a 1", "I b 2"}, {"U a 10"}, {"D b"}, {}, {"I b 3"}, {"U before 2", "D a"}}
	check := func(got []txLog, want [][]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%d transactions, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if fmt.Sprint(got[i].ops) != fmt.Sprint(want[i]) {
				t.Fatalf("transaction %d: %v, want %v", i, got[i].ops, want[i])
			}
			if got[i].unkn != 0 {
				t.Fatalf("transaction %d: %d changes with unknown old version", i, got[i].unkn)
			}
		}
	}
	// peeking does not consume
	check(collect(t, e, "s1", ChangeOpts{}), want)
	got := collect(t, e, "s1", ChangeOpts{})
	check(got, want)

	// up to a position: the first two transactions
	check(collect(t, e, "s1", ChangeOpts{UpTo: got[1].end, Consume: true}), want[:2])
	// at most 1 change, consumed: the next transaction (with 1 change)
	check(collect(t, e, "s1", ChangeOpts{MaxChanges: 1, Consume: true}), want[2:3])

	// a transaction split into several frames comes as one
	big := make([]WriteOp, 0, wal.MaxFrameOps+10)
	for i := 0; i < wal.MaxFrameOps+10; i++ {
		big = append(big, put(fmt.Sprintf("big%05d", i), int64(i)))
	}
	w(big...)

	// the slot survives a restart at its consumed position
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	if s := e.Slots(); len(s) != 1 || s[0].Name != "s1" {
		t.Fatalf("slots after restart: %+v", s)
	}
	got = collect(t, e, "s1", ChangeOpts{Consume: true})
	check(got[:3], want[3:])
	if len(got) != 4 || len(got[3].ops) != wal.MaxFrameOps+10 || got[3].ops[0] != "I big00000 0" {
		t.Fatalf("big transaction: %d transactions, last with %d changes", len(got), len(got[len(got)-1].ops))
	}
	if rest := collect(t, e, "s1", ChangeOpts{}); len(rest) != 0 {
		t.Fatalf("%d transactions left after consuming all", len(rest))
	}
	if err := e.DropSlot("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SlotChanges("s1", ChangeOpts{}, func(*Change) error { return nil }); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("dropped slot: %v", err)
	}
}

// TestCDCWait checks a waiting consumer sees transactions as they commit.
func TestCDCWait(t *testing.T) {
	e := open(t, t.TempDir(), 1<<20)
	defer e.Close()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateSlot("w", ""); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	done := make(chan error, 1)
	go func() {
		_, err := e.SlotChanges("w", ChangeOpts{Wait: stop, Consume: true}, func(c *Change) error {
			if c.Kind == ChangeInsert {
				mu.Lock()
				seen = append(seen, c.Key)
				n := len(seen)
				mu.Unlock()
				if n == 3 {
					close(stop)
				}
			}
			return nil
		})
		done <- err
	}()
	for i := 0; i < 3; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[0 1 2]" {
		t.Fatalf("seen %v", seen)
	}
}

// TestOnCommitOrder checks commit hooks run in LSN order under concurrency.
func TestOnCommitOrder(t *testing.T) {
	e := open(t, t.TempDir(), 1<<20)
	defer e.Close()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []uint64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				tx := e.Begin(SnapshotIsolation)
				tx.Put("kv", row(fmt.Sprintf("%d-%d", g, i), 1))
				tx.OnCommit(func(lsn uint64) {
					mu.Lock()
					order = append(order, lsn)
					mu.Unlock()
				})
				if _, err := tx.Commit(); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	// commits are answered once durable; hooks run when installed, just after
	e.waitInstalled(e.DurableLSN())
	// the last batch's hooks run after it is published as installed
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 800 {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 800 {
		t.Fatalf("%d hooks ran, want 800", len(order))
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("hook %d: lsn %d after %d", i, order[i], order[i-1])
		}
	}
}

// TestCDCCatalog: publications and replica identities persist, and an
// update of a REPLICA IDENTITY FULL table carries the old row.
func TestCDCCatalog(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	if err := e.CreatePublication(Publication{Name: "p", Tables: []string{"kv"}, Insert: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.CreatePublication(Publication{Name: "p"}); !errors.Is(err, ErrPublicationExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := e.SetReplicaIdentityFull("kv", true); err != nil {
		t.Fatal(err)
	}
	if err := e.SetReplicaIdentityFull("nope", true); err == nil {
		t.Fatal("identity of a missing table accepted")
	}
	if _, err := e.CreateSlot("s", "pgoutput"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("k", 1)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("k", 2)}}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	ps := e.Publications()
	if len(ps) != 1 || ps[0].Name != "p" || !ps[0].Has("kv") || ps[0].Update || !e.ReplicaIdentityFull("kv") {
		t.Fatalf("after restart: %+v full=%v", ps, e.ReplicaIdentityFull("kv"))
	}
	var got []string
	_, err := e.SlotChanges("s", ChangeOpts{}, func(c *Change) error {
		if c.Kind == ChangeUpdate {
			got = append(got, fmt.Sprint(c.OldRow[1].I, "->", c.Row[1].I))
		}
		return nil
	})
	if err != nil || len(got) != 1 || got[0] != "1->2" {
		t.Fatalf("update with old row: %v %v", got, err)
	}
	if err := e.SetReplicaIdentityFull("kv", false); err != nil || e.ReplicaIdentityFull("kv") {
		t.Fatalf("back to DEFAULT: %v", err)
	}
	if err := e.DropPublication("p"); err != nil || len(e.Publications()) != 0 {
		t.Fatalf("drop: %v", err)
	}
}
