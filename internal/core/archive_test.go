package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"stask/internal/schema"
)

// TestPointInTimeRecovery: a base backup plus the archived log restores
// the database as of a time and as of an LSN after the backup.
func TestPointInTimeRecovery(t *testing.T) {
	root := t.TempDir()
	dir, base, arch := filepath.Join(root, "db"), filepath.Join(root, "base"), filepath.Join(root, "archive")
	opts := Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, ArchiveDir: arch, GC: true,
		GCInterval: 50 * time.Millisecond, Logf: t.Logf}
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
	fill(t, e, "kv", 100, 1000)
	time.Sleep(10 * time.Millisecond)
	target := time.Now().UnixNano()
	targetLSN := e.DurableLSN()
	time.Sleep(10 * time.Millisecond)
	fill(t, e, "kv", 1000, 8000) // rolls several 1 MB segments
	// rewrite the first rows: GC may now reclaim old segments, but only
	// once they are archived
	fill(t, e, "kv", 0, 1000)
	last := e.log.WritePos().Seg - 1
	for i := 0; e.archived.Load() < int64(last); i++ {
		if i > 100 {
			t.Fatalf("archived up to %d, want %d", e.archived.Load(), last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.Close()

	count := func(dir string, o Options) int {
		t.Helper()
		if out, err := exec.Command("cp", "-a", base, dir).CombinedOutput(); err != nil {
			t.Fatalf("copy: %v %s", err, out)
		}
		o.Dir, o.SegmentSize, o.NoCheckpoint, o.RestoreArchive, o.Logf = dir, 1<<20, true, arch, t.Logf
		e, err := Open(o)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		if !e.Stats.Recovery.TargetReached {
			t.Fatal("target not reached")
		}
		n := 0
		e.ScanSnapshot("kv", e.DurableLSN(), func(string, schema.Row, *Version) bool { n++; return true })
		// the restored database takes writes
		fill(t, e, "kv", 9000, 9001)
		return n
	}
	if n := count(filepath.Join(root, "r1"), Options{RecoveryTargetTS: target}); n != 1000 {
		t.Fatalf("as of the target time: %d rows, want 1000", n)
	}
	if n := count(filepath.Join(root, "r2"), Options{RecoveryTargetLSN: targetLSN}); n != 1000 {
		t.Fatalf("as of the target LSN: %d rows, want 1000", n)
	}
	// a target past the archive fails rather than silently stopping short
	if out, err := exec.Command("cp", "-a", base, filepath.Join(root, "r3")).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v %s", err, out)
	}
	if _, err := Open(Options{Dir: filepath.Join(root, "r3"), SegmentSize: 1 << 20, NoCheckpoint: true, RestoreArchive: arch,
		RecoveryTargetTS: time.Now().Add(time.Hour).UnixNano(), Logf: t.Logf}); err == nil {
		t.Fatal("recovery past the archive succeeded")
	}
	os.RemoveAll(root)
}
