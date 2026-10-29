package core

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"stask/internal/schema"
	"stask/internal/wal"
)

// recQuorum records each batch the leader's engine hands to Replicate.
type recQuorum struct {
	mu   sync.Mutex
	ends []wal.Pos
	lsns []uint64
}

func (q *recQuorum) Replicate(lsn uint64, end wal.Pos) error {
	q.mu.Lock()
	q.ends, q.lsns = append(q.ends, end), append(q.lsns, lsn)
	q.mu.Unlock()
	return nil
}

func has(t *testing.T, e *Engine, k string) bool {
	t.Helper()
	row, _, err := e.Get("kv", k)
	if err != nil {
		t.Fatal(err)
	}
	return row != nil
}

// TestClusterEngine: a cluster-mode engine follows a leader through Apply
// (byte for byte), refuses writes, is promoted and writes, and reopening
// with a cut drops exactly the records after the cut LSN.
func TestClusterEngine(t *testing.T) {
	q := &recQuorum{}
	L, err := Open(Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Quorum: q})
	if err != nil {
		t.Fatal(err)
	}
	defer L.Close()
	if _, err := L.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := L.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: fmt.Sprintf("k%d", i)}, {I: int64(i)}, {S: ""}}}}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	openC := func(o Options) *Engine {
		o.Dir, o.SegmentSize, o.NoCheckpoint, o.Cluster = dir, 4<<20, true, true
		e, err := Open(o)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	F := openC(Options{Cut: true})
	prev := wal.Pos{}
	for _, end := range q.ends {
		buf := make([]byte, end.Off-prev.Off)
		if _, err := L.Log().ReadAt(end.Seg, prev.Off, buf); err != nil {
			t.Fatal(err)
		}
		if err := F.Apply(prev, buf); err != nil {
			t.Fatal(err)
		}
		prev = end
	}
	if F.DurableLSN() != L.DurableLSN() || !has(t, F, "k19") {
		t.Fatalf("follower lsn %d, leader %d", F.DurableLSN(), L.DurableLSN())
	}
	if _, err := F.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "x"}, {I: 1}, {S: ""}}}}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a follower must refuse writes, got %v", err)
	}
	if err := F.Promote(); err != nil {
		t.Fatal(err)
	}
	if F.ReadOnly() {
		t.Fatal("still read-only after Promote")
	}
	lsn, err := F.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "p"}, {I: 7}, {S: ""}}}})
	if err != nil || lsn != L.DurableLSN()+1 {
		t.Fatalf("write on the promoted engine: lsn %d (leader had %d) err %v", lsn, L.DurableLSN(), err)
	}
	if err := F.Apply(wal.Pos{}, nil); err == nil {
		t.Fatal("Apply must be refused on a writable engine")
	}
	F.Close()

	cutAt := q.lsns[9] // keep the first ten batches: the table and nine rows
	F = openC(Options{Cut: true, CutLSN: cutAt})
	if F.DurableLSN() != cutAt || !has(t, F, "k7") || has(t, F, "k12") || has(t, F, "p") {
		t.Fatalf("after the cut at lsn %d: durable %d, k7 %v, k12 %v, p %v", cutAt, F.DurableLSN(), has(t, F, "k7"), has(t, F, "k12"), has(t, F, "p"))
	}
	// the cut log continues where the leader's did: the next batch applies
	next := q.ends[9]
	buf := make([]byte, q.ends[10].Off-next.Off)
	L.Log().ReadAt(next.Seg, next.Off, buf)
	if err := F.Apply(next, buf); err != nil || !has(t, F, "k9") {
		t.Fatalf("applying the batch after the cut: %v", err)
	}
	F.Close()
	// a cut beyond the end changes nothing
	F = openC(Options{Cut: true, CutLSN: 1 << 40})
	if F.DurableLSN() != q.lsns[10] {
		t.Fatalf("cut beyond the end: durable %d, want %d", F.DurableLSN(), q.lsns[10])
	}
	F.Close()
}
