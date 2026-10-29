package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stask/internal/wal"
)

func waitScrub(t *testing.T, e *Engine) ScrubReport {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if r := e.ScrubStatus(); !r.Running && !r.Started.IsZero() {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scrub did not finish")
	return ScrubReport{}
}

func TestScrub(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	defer e.Close()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6000; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	e.waitInstalled(e.DurableLSN())
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.Log().Segments()); n < 3 {
		t.Fatalf("only %d segments", n)
	}

	// clean data: nothing found, every record and the checkpoint counted
	if !e.StartScrub(0) {
		t.Fatal("scrub did not start")
	}
	r := waitScrub(t, e)
	if !r.OK() || r.Segments < 3 || r.Records < 6000 || r.Checkpoints != 1 || r.Bytes < 1<<20 {
		t.Fatalf("clean scrub: %+v", r)
	}

	// flip one byte inside a record of the first segment
	segs := e.Log().Segments()
	flip := func(path string, off int64) {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var b [1]byte
		if _, err := f.ReadAt(b[:], off); err != nil {
			t.Fatal(err)
		}
		b[0] ^= 0xff
		if _, err := f.WriteAt(b[:], off); err != nil {
			t.Fatal(err)
		}
	}
	seg0 := filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", segs[0]))
	if _, err := os.Stat(seg0); err != nil {
		ents, _ := os.ReadDir(filepath.Join(dir, "wal"))
		t.Fatalf("segment file name: %v (dir has %v)", err, ents)
	}
	flip(seg0, 300)
	before := ScrubErrorsTotal()
	e.StartScrub(0)
	r = waitScrub(t, e)
	if r.OK() || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], fmt.Sprintf("segment %d", segs[0])) {
		t.Fatalf("corrupted segment not reported: %+v", r)
	}
	if ScrubErrorsTotal() != before+1 {
		t.Fatal("error counter not incremented")
	}
	flip(seg0, 300) // restore

	// a bad byte in a checkpoint chunk
	ck := LatestCheckpoint(dir)
	fi, _ := os.Stat(ck)
	flip(ck, fi.Size()/2)
	e.StartScrub(0)
	r = waitScrub(t, e)
	if r.OK() || !strings.Contains(strings.Join(r.Errors, ";"), "checkpoint") {
		t.Fatalf("corrupted checkpoint not reported: %+v", r)
	}

	// a second scrub cannot start while one runs; a rate cap slows it
	flip(ck, fi.Size()/2) // restore
	if !e.StartScrub(2 << 20) {
		t.Fatal("start")
	}
	if e.StartScrub(0) {
		t.Fatal("two scrubs at once")
	}
	e.StopScrub()
	r = waitScrub(t, e)
	if !r.Cancelled {
		t.Fatalf("not cancelled: %+v", r)
	}
	e.StartScrub(0)
	if r = waitScrub(t, e); !r.OK() {
		t.Fatalf("after repairing: %+v", r)
	}
}

func TestScrubTailGarbage(t *testing.T) {
	// a damaged length field that reads as the end-of-log marker must not
	// hide the records after it
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	defer e.Close()
	e.CreateTable(kvTable())
	for i := 0; i < 4000; i++ {
		e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}})
	}
	segs := e.Log().Segments()
	path := filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", segs[0]))
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	defer f.Close()
	// zero the 4-byte length of a record far into the segment
	f.WriteAt([]byte{0, 0, 0, 0}, 500)
	e.StartScrub(0)
	r := waitScrub(t, e)
	if r.OK() {
		t.Fatalf("zeroed length not noticed: %+v", r)
	}
}

func TestScrubCompressedSegment(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	defer e.Close()
	e.CreateTable(kvTable())
	for i := 0; i < 4000; i++ {
		e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}})
	}
	segs := e.Log().Segments()
	codec, err := wal.ParseCodec("zstd")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Log().Compress(segs[0], codec); err != nil {
		t.Fatal(err)
	}
	if !e.Log().IsCompressed(segs[0]) {
		t.Fatal("not compressed")
	}
	e.StartScrub(0)
	if r := waitScrub(t, e); !r.OK() || r.Segments < 2 {
		t.Fatalf("clean compressed segment: %+v", r)
	}
	path := filepath.Join(dir, "wal", fmt.Sprintf("%08d.walz", segs[0]))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	f.ReadAt(b[:], 200)
	b[0] ^= 0xff
	f.WriteAt(b[:], 200)
	f.Close()
	e.StartScrub(0)
	if r := waitScrub(t, e); r.OK() {
		t.Fatalf("damaged compressed segment not reported: %+v", r)
	}
}
