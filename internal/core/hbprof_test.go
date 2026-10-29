package core

import (
	"fmt"
	"os"
	"testing"
	"time"

	"stask/internal/wal"
)

func TestHBProf(t *testing.T) {
	if os.Getenv("STASK_HISTBENCH") == "" {
		t.Skip()
	}
	dir := t.TempDir()
	e, err := Open(Options{Dir: dir, SegmentSize: 8 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	e.CreateTable(simTable(false))
	n := 400
	g := newSimGen(n, false)
	for round := 0; round < 300; round++ {
		var ops []WriteOp
		for _, i := range g.rnd.Perm(n) {
			ops = append(ops, WriteOp{Table: "sim", Row: g.row(i, round, 1)})
		}
		e.Write(ops)
	}
	for _, s := range e.log.Segments() {
		if s < e.log.WriteSeg() {
			e.log.Compress(s, wal.CodecZstd)
		}
	}
	for it := 0; it < 3; it++ {
		t0 := time.Now()
		for k := 0; k < 100; k++ {
			h, err := e.History("sim", fmt.Sprintf("09%08d", k), 1000)
			if err != nil || len(h) != 300 {
				t.Fatal(len(h), err)
			}
		}
		t.Logf("100 histories of 300 versions: %v each", time.Since(t0)/100)
	}
	e.Close()
}
