package core

import (
	"errors"
	"sync/atomic"
	"testing"

	"stask/internal/schema"
	"stask/internal/wal"
)

type fakeQuorum struct {
	calls atomic.Int64
	last  atomic.Uint64
	err   error
}

func (q *fakeQuorum) Replicate(lsn uint64, end wal.Pos) error {
	q.calls.Add(1)
	q.last.Store(lsn)
	return q.err
}

func TestQuorumHook(t *testing.T) {
	q := &fakeQuorum{}
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Quorum: q})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.CreateTable(schema.Table{Name: "kv", Key: "id", Columns: []schema.Column{{Name: "id", Type: "string"}, {Name: "n", Type: "int"}}}); err != nil {
		t.Fatal(err)
	}
	lsn, err := e.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "a"}, {I: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if q.calls.Load() == 0 || q.last.Load() < lsn {
		t.Fatalf("Replicate calls %d, last %d, write lsn %d", q.calls.Load(), q.last.Load(), lsn)
	}
	// the write was not visible before Replicate returned: here it returned,
	// so it is
	if e.DurableLSN() < lsn {
		t.Fatal("not published")
	}
	q.err = errors.New("lost the majority")
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "b"}, {I: 2}}}}); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("a failed quorum must give ErrUnknownOutcome, got %v", err)
	}
	if row, _, _ := e.Get("kv", "b"); row != nil {
		t.Fatal("a batch without quorum became visible")
	}
}

func TestNoQuorumDefault(t *testing.T) {
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.opts.Quorum != nil {
		t.Fatal("quorum must default to off")
	}
}
