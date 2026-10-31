package core

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stask/internal/schema"
	"stask/internal/seccrypt"
)

// TestArchiveEncrypted: with ArchiveKey the archived segments are sealed
// (no plaintext, authenticated), a point-in-time restore with the key gives
// the rows, and a wrong or missing key, a modified segment, or a plaintext
// segment slipped into an encrypted archive all fail instead of replaying.
func archiveName2(dir string, s uint32) string {
	return filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", s))
}

func TestArchiveEncrypted(t *testing.T) {
	root := t.TempDir()
	dir, base, arch := filepath.Join(root, "db"), filepath.Join(root, "base"), filepath.Join(root, "archive")
	key := make([]byte, 32)
	rand.Read(key)
	opts := Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, ArchiveDir: arch, ArchiveKey: key, Logf: t.Logf}
	e, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	fill(t, e, "kv", 0, 100)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	if out, err := exec.Command("cp", "-a", dir, base).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v %s", err, out)
	}
	e, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	fill(t, e, "kv", 100, 5000) // several 1 MB segments
	last := e.log.WritePos().Seg - 1
	for i := 0; e.archived.Load() < int64(last); i++ {
		if i > 100 {
			t.Fatalf("archived up to %d, want %d", e.archived.Load(), last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.Close()

	segs, _ := archivedSegs(arch)
	if len(segs) == 0 {
		t.Fatal("nothing archived")
	}
	for _, s := range segs {
		b, _ := os.ReadFile(archiveName(arch, s))
		if !seccrypt.IsEncrypted(b) {
			t.Fatalf("segment %d is not encrypted", s)
		}
		pl, _ := os.ReadFile(archiveName2(dir, s))
		if len(pl) > 4096 && bytes.Contains(b, pl[2048:2112]) {
			t.Fatalf("segment %d shows plaintext", s)
		}
	}

	open := func(name string, o Options) (int, error) {
		d := filepath.Join(root, name)
		if out, err := exec.Command("cp", "-a", base, d).CombinedOutput(); err != nil {
			t.Fatalf("copy: %v %s", err, out)
		}
		o.Dir, o.SegmentSize, o.NoCheckpoint, o.RestoreArchive, o.Logf = d, 1<<20, true, arch, t.Logf
		e, err := Open(o)
		if err != nil {
			return 0, err
		}
		defer e.Close()
		n := 0
		e.ScanSnapshot("kv", e.DurableLSN(), func(string, schema.Row, *Version) bool { n++; return true })
		return n, nil
	}
	n0, err := open("ok", Options{ArchiveKey: key})
	if err != nil || n0 < 1000 { // the sealed segments only: the open one was never archived
		t.Fatalf("restore with the key: %d rows, %v", n0, err)
	}
	wrong := make([]byte, 32)
	if _, err := open("wrong", Options{ArchiveKey: wrong}); err == nil || !strings.Contains(err.Error(), "wrong encryption key") {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := open("nokey", Options{}); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("no key: %v", err)
	}
	// modify one byte of the newest archived segment
	f := archiveName(arch, segs[len(segs)-1])
	good, _ := os.ReadFile(f)
	bad := append([]byte(nil), good...)
	bad[len(bad)/2] ^= 1
	os.WriteFile(f, bad, 0o644)
	if _, err := open("flip", Options{ArchiveKey: key}); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("modified segment: %v", err)
	}
	// a plaintext segment in an encrypted archive is refused unless allowed
	plain, _ := seccrypt.Open(key, good)
	os.WriteFile(f, plain, 0o644)
	if _, err := open("plain", Options{ArchiveKey: key}); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("plaintext segment accepted: %v", err)
	}
	if n, err := open("plainok", Options{ArchiveKey: key, ArchiveAllowPlain: true}); err != nil || n != n0 {
		t.Fatalf("allowed plaintext segment: %d, %v", n, err)
	}
}
