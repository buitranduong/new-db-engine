package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"stask/internal/wal"
)

// Sealed segments rewritten compressed must read back the same: point
// reads, snapshot reads through version chains, scans, and recovery that
// replays the whole log from compressed segments.
func TestCompressedSegments(t *testing.T) {
	for _, codec := range []uint8{wal.CodecS2, wal.CodecZstd} {
		t.Run(fmt.Sprint(codec), func(t *testing.T) {
			old := wal.ZCacheBytes
			wal.ZCacheBytes = 256 << 10
			defer func() { wal.ZCacheBytes = old }()
			dir := t.TempDir()
			e := open(t, dir, 1<<20)
			if _, err := e.CreateTable(kvTable()); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5000; i++ {
				if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}}); err != nil {
					t.Fatal(err)
				}
			}
			before := e.DurableLSN()
			if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("7", 700)}, {Table: "kv", Key: "8"}}); err != nil {
				t.Fatal(err)
			}
			check := func(e *Engine) {
				t.Helper()
				for _, i := range []int{0, 1, 999, 2500, 4999} {
					r, _, err := e.Get("kv", fmt.Sprint(i))
					if err != nil || r == nil || r[2].S != row(fmt.Sprint(i), int64(i))[2].S {
						t.Fatalf("get %d: %v %v", i, r, err)
					}
				}
				if r, _, _ := e.Get("kv", "7"); r == nil || r[1].I != 700 {
					t.Fatalf("get 7: %v", r)
				}
				if r, _, err := e.GetAt("kv", "7", before); r == nil || r[1].I != 7 {
					t.Fatalf("snapshot read of 7: %v %v", r, err)
				}
				if r, _, _ := e.GetAt("kv", "8", before); r == nil {
					t.Fatalf("snapshot read of 8 should exist")
				}
				rows, err := e.ScanRange("kv", "10", "11", 1000, e.DurableLSN())
				if err != nil || len(rows) != 111 {
					t.Fatalf("scan: %d %v", len(rows), err)
				}
			}
			segs := e.Log().Segments()
			if len(segs) < 2 {
				t.Fatalf("expected several segments, got %v", segs)
			}
			var raw, comp int64
			for _, s := range segs[:len(segs)-1] {
				r, c, err := e.Log().Compress(s, codec)
				if err != nil {
					t.Fatal(err)
				}
				raw, comp = raw+r, comp+c
				if !e.Log().IsCompressed(s) {
					t.Fatalf("segment %d not compressed", s)
				}
				if _, err := os.Stat(filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", s))); !os.IsNotExist(err) {
					t.Fatalf("raw segment %d still present: %v", s, err)
				}
			}
			t.Logf("compressed %d -> %d bytes (%.2fx)", raw, comp, float64(raw)/float64(comp))
			if comp*2 > raw {
				t.Fatalf("poor ratio %d -> %d", raw, comp)
			}
			check(e)
			// concurrent point reads share the block cache (a small one,
			// see above, so blocks are replaced while others read)
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := g; i < 5000; i += 8 {
						if i == 7 || i == 8 {
							continue
						}
						r, _, err := e.Get("kv", fmt.Sprint(i))
						if err != nil || r == nil || r[2].S != row(fmt.Sprint(i), int64(i))[2].S {
							t.Errorf("concurrent get %d: %v %v", i, r, err)
							return
						}
					}
				}(g)
			}
			wg.Wait()
			if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("9", 900)}}); err != nil {
				t.Fatal(err)
			}
			e.Close()

			// no checkpoint: recovery replays every record, most of them
			// from compressed segments
			e = open(t, dir, 1<<20)
			defer e.Close()
			check(e)
			if r, _, _ := e.Get("kv", "9"); r == nil || r[1].I != 900 {
				t.Fatalf("get 9 after recovery: %v", r)
			}
			if e.Stats.Recovery.ReplayRecords < 5000 {
				t.Fatalf("replayed %d records", e.Stats.Recovery.ReplayRecords)
			}
		})
	}
}

// A crash between installing the compressed copy and removing the raw one
// leaves both; opening keeps the compressed copy.
func TestCompressedSegmentCrashLeftovers(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	walDir := filepath.Join(dir, "wal")
	first := e.Log().Segments()[0]
	rawCopy, err := os.ReadFile(filepath.Join(walDir, fmt.Sprintf("%08d.wal", first)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Log().Compress(first, wal.CodecZstd); err != nil {
		t.Fatal(err)
	}
	e.Close()
	os.WriteFile(filepath.Join(walDir, fmt.Sprintf("%08d.wal", first)), rawCopy, 0o644)
	os.WriteFile(filepath.Join(walDir, fmt.Sprintf("%08d.walz.tmp", first+1)), []byte("partial"), 0o644)
	e = open(t, dir, 1<<20)
	defer e.Close()
	if !e.Log().IsCompressed(first) {
		t.Fatal("compressed copy not used")
	}
	for _, name := range []string{fmt.Sprintf("%08d.wal", first), fmt.Sprintf("%08d.walz.tmp", first+1)} {
		if _, err := os.Stat(filepath.Join(walDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind", name)
		}
	}
	for i := 0; i < 3000; i += 97 {
		if r, _, err := e.Get("kv", fmt.Sprint(i)); err != nil || r == nil || r[1].I != int64(i) {
			t.Fatalf("get %d: %v %v", i, r, err)
		}
	}
}
