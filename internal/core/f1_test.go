package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stask/internal/schema"
	"stask/internal/wal"
)

// flipInSealed writes rows, checkpoints, closes, and flips one byte in the
// value of the given key (which lives in a sealed segment the checkpoint
// covers). It returns the segment and the offset of the flipped byte.
func flipInSealed(t *testing.T, dir, key string) (seg uint32, at int64) {
	t.Helper()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	_, v, err := e.Get("kv", key)
	if err != nil || v == nil {
		t.Fatalf("get %s: %v", key, err)
	}
	if v.Loc.Seg >= e.Log().WriteSeg() {
		t.Fatalf("key %s is in the active segment %d", key, v.Loc.Seg)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	seg, at = v.Loc.Seg, v.ValOff+int64(v.ValLen)-20
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", seg)), os.O_RDWR, 0)
	if err != nil {
		matches, _ := filepath.Glob(filepath.Join(dir, "wal", "*"))
		t.Fatalf("open segment: %v (have %v)", err, matches)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], at); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x01
	if _, err := f.WriteAt(b[:], at); err != nil {
		t.Fatal(err)
	}
	return seg, at
}

// TestSealedSegmentCorruptionIsLoud is the reproduction of F1 (drill D4): a
// flipped byte in the value of a row in a sealed segment that a checkpoint
// covers. Recovery does not replay that segment, so startup cannot see it;
// the read must refuse the row with an error that names the segment and the
// offset, never return the changed value.
func TestSealedSegmentCorruptionIsLoud(t *testing.T) {
	dir := t.TempDir()
	seg, _ := flipInSealed(t, dir, "10")
	before := wal.ReadCRCErrors.Load()

	e := open(t, dir, 1<<20)
	defer e.Close()
	if e.Stats.Recovery.ReplayRecords > 1000 {
		t.Fatalf("recovery replayed %d records: the checkpoint did not cover the sealed segments", e.Stats.Recovery.ReplayRecords)
	}
	r, _, err := e.Get("kv", "10")
	if err == nil {
		t.Fatalf("F1: a corrupted row was served without an error: %v", r[2].S[len(r[2].S)-30:])
	}
	var ce *wal.CorruptError
	if !errors.Is(err, wal.ErrCorrupt) || !errors.As(err, &ce) || ce.Seg != seg {
		t.Fatalf("error %v is not a corruption error naming segment %d", err, seg)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("segment %d offset", seg)) {
		t.Fatalf("message does not name segment and offset: %v", err)
	}
	if r != nil {
		t.Fatalf("partial data returned with the error: %v", r)
	}
	if wal.ReadCRCErrors.Load() != before+1 {
		t.Fatalf("stask_read_crc_errors_total did not count the refusal")
	}
	// a scan over the damaged table is refused, not silently short or wrong
	if err := e.ScanSnapshot("kv", e.DurableLSN(), func(string, schema.Row, *Version) bool { return true }); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("scan over a damaged frame: %v", err)
	}
	// rows of intact frames are still served
	if r, _, err := e.Get("kv", "4000"); err != nil || r == nil {
		t.Fatalf("intact row: %v %v", r, err)
	}
}

// TestReadVerifyOffServesDamage documents the measurement switch: with the
// check off the damaged row is served (this is the behaviour F1 describes).
func TestReadVerifyOffServesDamage(t *testing.T) {
	dir := t.TempDir()
	flipInSealed(t, dir, "10")
	e := open(t, dir, 1<<20)
	defer e.Close()
	e.Log().SetReadVerify(false)
	if _, _, err := e.Get("kv", "10"); err != nil {
		t.Fatalf("with verification off: %v", err)
	}
}
