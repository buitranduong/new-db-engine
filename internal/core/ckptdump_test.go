package core

import (
	"fmt"
	"strings"
	"testing"

	"stask/internal/wal"
)

// TestReadCheckpoint checks the reference decoder against the index the
// checkpoint was written from: same keys in order, same heads.
func TestReadCheckpoint(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	defer e.Close()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		id := fmt.Sprint(i)
		if i%5 == 0 { // long keys live in the key arena
			id = fmt.Sprintf("long-%s-%06d", strings.Repeat("k", i%30), i)
		}
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(id, int64(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Key: "7"}, {Table: "kv", Row: row("8", 800)}}); err != nil {
		t.Fatal(err)
	}
	// a fuzzy checkpoint holds what is installed (the log has the rest)
	e.waitInstalled(e.DurableLSN())
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	ts, err := e.Table("kv")
	if err != nil {
		t.Fatal(err)
	}
	var want []CheckpointEntry
	tr := ts.idx.Load()
	tr.ascend("", func(en *entry) bool {
		want = append(want, CheckpointEntry{Table: ts.Schema().ID, Key: strings.Clone(tr.keyOf(en)), LSN: en.lsn,
			Loc:    wal.Loc{Ok: true, Seg: en.seg, Off: en.off, Idx: en.idx},
			ValOff: en.valOff, ValLen: en.valLen, Deleted: en.flags&entDeleted != 0})
		return true
	})
	var got []CheckpointEntry
	hdr, err := ReadCheckpoint(LatestCheckpoint(dir), func(ce CheckpointEntry) error {
		got = append(got, ce)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Version != ckptVersion || len(hdr.Tables) != 1 || hdr.Tables[0].Name != "kv" {
		t.Fatalf("header %+v", hdr)
	}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
