package wal

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestOpKindAt compares OpKindAt with the full decoder on every op of
// records of every kind, with keys and values longer than its read window.
func TestOpKindAt(t *testing.T) {
	l, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.PrepareAppend(0, 0); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	var positions []Pos
	for i := 0; i < 200; i++ {
		kind := []uint8{KindTx, KindTxPart, KindMove}[i%3]
		ops := make([]Op, 1+rng.Intn(20))
		for j := range ops {
			op := &ops[j]
			op.Kind = OpPut
			if rng.Intn(3) == 0 {
				op.Kind = OpDel
			}
			op.Table = uint32(rng.Intn(300))
			op.Key = bytes.Repeat([]byte{'k'}, rng.Intn(100))
			if op.Kind == OpPut {
				op.Val = bytes.Repeat([]byte{'v'}, rng.Intn(500))
			}
			if rng.Intn(2) == 0 {
				op.Prev = Loc{Ok: true, Seg: uint32(rng.Intn(1000)), Off: rng.Uint32(), Idx: uint16(rng.Intn(5000))}
			}
			op.OrigLSN, op.OrigTS = rng.Uint64()>>1, rng.Int63()
		}
		buf, _ := AppendRecord(nil, kind, uint64(i+1), rng.Int63(), ops, nil, nil)
		p, err := l.Append(buf)
		if err != nil {
			t.Fatal(err)
		}
		positions = append(positions, p)
	}
	ddl, _ := AppendRecord(nil, KindDDL, 1000, 1, nil, []byte(`{"id":1}`), nil)
	ddlAt, _ := l.Append(ddl)
	for _, p := range positions {
		rec, err := l.ReadRecord(p.Seg, p.Off)
		if err != nil {
			t.Fatal(err)
		}
		for i, op := range rec.Ops {
			k, err := l.OpKindAt(p.Seg, p.Off, uint16(i))
			if err != nil || k != op.Kind {
				t.Fatalf("record at %d op %d: kind %d %v, want %d", p.Off, i, k, err, op.Kind)
			}
		}
		if _, err := l.OpKindAt(p.Seg, p.Off, uint16(len(rec.Ops))); err == nil {
			t.Fatalf("record at %d: op %d of %d accepted", p.Off, len(rec.Ops), len(rec.Ops))
		}
	}
	if _, err := l.OpKindAt(ddlAt.Seg, ddlAt.Off, 0); err == nil {
		t.Fatal("DDL record accepted")
	}
}
