package wal

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// TestReadVerify: a flipped byte in one frame makes every read that touches
// that frame fail with a CorruptError naming segment and offset, and leaves
// the other frames readable; a frame verified before it rotted is checked
// again after InvalidateVerified.
func TestReadVerify(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.PrepareAppend(0, 0); err != nil {
		t.Fatal(err)
	}
	var locs []Loc
	var vals []int64
	var pos []Pos
	for i := 0; i < 20; i++ {
		ops := []Op{{Kind: OpPut, Table: 1, Key: []byte{'k', byte(i)}, Val: bytes.Repeat([]byte{byte('a' + i)}, 100)}}
		buf, valRel := AppendRecord(nil, KindTx, uint64(i+1), 1, ops, nil, nil)
		p, err := l.Append(buf)
		if err != nil {
			t.Fatal(err)
		}
		pos = append(pos, p)
		locs = append(locs, Loc{Ok: true, Seg: p.Seg, Off: uint32(p.Off)})
		vals = append(vals, p.Off+int64(valRel[0]))
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	stale := false
	read := func(i int) error {
		b := make([]byte, 100)
		if err := l.ReadValue(locs[i], vals[i], b); err != nil {
			return err
		}
		if !stale && !bytes.Equal(b, bytes.Repeat([]byte{byte('a' + i)}, 100)) {
			t.Fatalf("value %d read back wrong without an error", i)
		}
		return nil
	}
	for i := range locs {
		if err := read(i); err != nil {
			t.Fatal(err)
		}
	}
	// rot after the check: still served until the verified state is dropped
	flip := func(off int64) {
		f, err := os.OpenFile(segName(dir, 0), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var b [1]byte
		f.ReadAt(b[:], off)
		b[0] ^= 0x40
		f.WriteAt(b[:], off)
	}
	flip(vals[5] + 50)
	stale = true // frame 5 is served from the verified state until a scrub drops it
	read(5)
	stale = false
	l.InvalidateVerified(0)
	for _, f := range []func() error{
		func() error { return read(5) },
		func() error { _, err := l.ReadOpMeta(0, pos[5].Off, 0); return err },
		func() error { _, _, _, err := l.ReadHeader(0, pos[5].Off); return err },
		func() error { _, err := l.OpKindAt(0, pos[5].Off, 0); return err },
		func() error { return l.Reader().ReadValue(locs[5], vals[5], make([]byte, 100)) },
	} {
		err := f()
		var ce *CorruptError
		if !errors.Is(err, ErrCorrupt) || !errors.As(err, &ce) || ce.Seg != 0 || ce.Off != pos[5].Off {
			t.Fatalf("read of the damaged frame: %v", err)
		}
	}
	if ReadCRCErrors.Load() < 5 {
		t.Fatalf("errors counted: %d", ReadCRCErrors.Load())
	}
	for _, i := range []int{0, 4, 6, 19} {
		if err := read(i); err != nil {
			t.Fatalf("intact frame %d: %v", i, err)
		}
	}
	// a damaged length field is caught too
	flip(pos[7].Off + 1)
	l.InvalidateVerified(0)
	if err := read(7); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged length: %v", err)
	}
	l.SetReadVerify(false)
	if _, err := l.ReadOpMeta(0, pos[5].Off, 0); err != nil {
		t.Fatalf("with the check off: %v", err)
	}
}

// An empty segment has no frame at offset 0: reading its header is "torn", not corruption (no CRC alarm, no log
// line, no counter): every fresh start probes it (core restoreHistoryState).
func TestReadHeaderOfEmptySegmentIsNotCorruption(t *testing.T) {
	l, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.PrepareAppend(0, 0); err != nil {
		t.Fatal(err)
	}
	before := ReadCRCErrors.Load()
	if _, _, _, err := l.ReadHeader(0, 0); !errors.Is(err, ErrTorn) {
		t.Fatalf("ReadHeader of an empty segment: %v, want ErrTorn", err)
	}
	if ReadCRCErrors.Load() != before {
		t.Fatal("an empty segment raised the read CRC counter")
	}
}
