package pindex

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// sortedSet returns n distinct keys of varied length with values, sorted.
func sortedSet(rng *rand.Rand, n int, big bool) ([][]byte, [][]byte, refMap) {
	m := refMap{}
	for len(m) < n {
		var k []byte
		if big {
			k = []byte(fmt.Sprintf("big/%06d/%s", rng.Intn(10*n+10), string(bytes.Repeat([]byte{'x'}, 380+rng.Intn(100)))))
		} else {
			k = mkKey(rng, 10*n+10)
		}
		if len(k) == 0 {
			continue
		}
		m[string(k)] = mkVal(rng)
	}
	ks := m.keys()
	keys := make([][]byte, len(ks))
	vals := make([][]byte, len(ks))
	for i, k := range ks {
		keys[i], vals[i] = []byte(k), m[k]
	}
	return keys, vals, m
}

func TestBulkBuildVsMap(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, tc := range []struct {
		n    int
		fill int
		big  bool
	}{{0, 100, false}, {1, 100, false}, {2, 100, false}, {60, 100, false}, {5000, 100, false}, {5000, 70, false}, {5000, 50, false}, {30000, 100, false}, {20000, 100, true}} {
		t.Run(fmt.Sprintf("n%d_fill%d_big%v", tc.n, tc.fill, tc.big), func(t *testing.T) {
			keys, vals, m := sortedSet(rng, tc.n, tc.big)
			mf := newMemFile()
			n, err := BulkBuild(mf, SliceFeed(keys, vals), BulkOptions{Fill: tc.fill, User: []byte("pos-42")})
			if err != nil || n != int64(tc.n) {
				t.Fatalf("BulkBuild = %d, %v", n, err)
			}
			tr := openMem(t, mf, 4<<20)
			if string(tr.User()) != "pos-42" || tr.Len() != int64(tc.n) {
				t.Fatalf("user %q len %d", tr.User(), tr.Len())
			}
			s, _ := tr.Snapshot()
			checkSnap(t, s, m, rng, "bulk")
			s.Close()
			// the result is an ordinary tree: mutate, checkpoint, reopen
			for i := 0; i < 3000; i++ {
				k := mkKey(rng, 10*tc.n+10)
				if len(k) == 0 {
					continue
				}
				if rng.Intn(3) == 0 {
					tr.Delete(k)
					delete(m, string(k))
				} else {
					v := mkVal(rng)
					if err := tr.Put(k, v); err != nil {
						t.Fatal(err)
					}
					m[string(k)] = v
				}
			}
			if err := tr.Checkpoint(nil); err != nil {
				t.Fatal(err)
			}
			tr.Close()
			tr = openMem(t, mf, 4<<20)
			s, _ = tr.Snapshot()
			checkSnap(t, s, m, rng, "bulk+mutate+reopen")
			s.Close()
		})
	}
}

func TestBulkLeavesAreFull(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	keys, vals, _ := sortedSet(rng, 20000, false)
	build := func(fill int) uint64 {
		mf := newMemFile()
		if _, err := BulkBuild(mf, SliceFeed(keys, vals), BulkOptions{Fill: fill}); err != nil {
			t.Fatal(err)
		}
		tr := openMem(t, mf, 4<<20)
		return tr.Stats().FilePages
	}
	full, half := build(100), build(50)
	// the same keys through Put, in order, for comparison
	tr := openMem(t, newMemFile(), 4<<20)
	for i := range keys {
		tr.Put(keys[i], vals[i])
	}
	put := tr.Stats().FilePages
	t.Logf("pages: bulk100=%d bulk50=%d sequential Put=%d", full, half, put)
	if full >= put || half <= full {
		t.Fatalf("unexpected page counts: bulk100=%d bulk50=%d put=%d", full, half, put)
	}
}

func TestBulkRejectsBadInput(t *testing.T) {
	k := [][]byte{[]byte("a"), []byte("c"), []byte("b")}
	if _, err := BulkBuild(newMemFile(), SliceFeed(k, nil), BulkOptions{}); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("unsorted: %v", err)
	}
	k = [][]byte{[]byte("a"), []byte("a")}
	if _, err := BulkBuild(newMemFile(), SliceFeed(k, nil), BulkOptions{}); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := BulkBuild(newMemFile(), SliceFeed([][]byte{{}}, nil), BulkOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("empty key: %v", err)
	}
	mf := newMemFile()
	mf.WriteAt([]byte{1}, 0)
	if _, err := BulkBuild(mf, SliceFeed(nil, nil), BulkOptions{}); err == nil {
		t.Fatal("non-empty file accepted")
	}
	boom := errors.New("boom")
	n := 0
	_, err := BulkBuild(newMemFile(), func() ([]byte, []byte, bool, error) {
		if n++; n > 3 {
			return nil, nil, false, boom
		}
		return []byte{byte(n)}, nil, true, nil
	}, BulkOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("feed error: %v", err)
	}
}

// A crash at any I/O step of BulkBuild leaves a file that is either not a
// valid index yet (never visible: it has a temporary name) or the complete
// index. Half-built indexes do not exist.
func TestBulkBuildCrashImages(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	keys, vals, m := sortedSet(rng, 4000, false)
	for fail := 0; fail < 30; fail++ {
		mf := newMemFile()
		mf.failAt = fail
		_, err := BulkBuild(mf, SliceFeed(keys, vals), BulkOptions{})
		for rep := 0; rep < 5; rep++ {
			img := mf.crash(rng)
			tr, oerr := Open("", Options{File: img, CacheBytes: 4 << 20})
			if oerr != nil {
				continue // no valid meta: not an index
			}
			if tr.Len() == 0 && err != nil {
				continue // an empty file Open initialised
			}
			s, _ := tr.Snapshot()
			checkSnap(t, s, m, rng, fmt.Sprintf("crash image failAt=%d", fail))
			s.Close()
		}
		if err == nil {
			break
		}
	}
}

func TestBulkLoadCrashPoints(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	kA, vA, mA := sortedSet(rng, 3000, false)
	kB, vB, mB := sortedSet(rng, 5000, false)
	crash := errors.New("crash")
	read := func(path string, want refMap, what string) {
		t.Helper()
		tr, err := Open(path, Options{CacheBytes: 4 << 20})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		defer tr.Close()
		s, _ := tr.Snapshot()
		defer s.Close()
		checkSnap(t, s, want, rng, what)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	dir := t.TempDir()
	path := filepath.Join(dir, "idx.pidx")
	tr, err := BulkLoad(path, SliceFeed(kA, vA), BulkOptions{}, Options{CacheBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()
	read(path, mA, "initial")

	// crash before the rename: the old file is intact, a stale temp remains
	_, err = BulkLoad(path, SliceFeed(kB, vB), BulkOptions{BeforeRename: func() error { return crash }}, Options{})
	if !errors.Is(err, crash) || !exists(BulkTemp(path)) {
		t.Fatalf("before rename: err %v, temp exists %v", err, exists(BulkTemp(path)))
	}
	read(path, mA, "after crash before rename")

	// crash after the rename: the new file is complete, no temp remains
	_, err = BulkLoad(path, SliceFeed(kB, vB), BulkOptions{AfterRename: func() error { return crash }}, Options{})
	if !errors.Is(err, crash) || exists(BulkTemp(path)) {
		t.Fatalf("after rename: err %v, temp exists %v", err, exists(BulkTemp(path)))
	}
	read(path, mB, "after crash after rename")

	// a failed feed leaves the target alone and removes its temp
	_, err = BulkLoad(path, SliceFeed([][]byte{[]byte("b"), []byte("a")}, nil), BulkOptions{}, Options{})
	if !errors.Is(err, ErrNotSorted) || exists(BulkTemp(path)) {
		t.Fatalf("bad feed: %v, temp exists %v", err, exists(BulkTemp(path)))
	}
	read(path, mB, "after failed feed")

	// a stale temp from an earlier crash does not stop the next load
	os.WriteFile(BulkTemp(path), []byte("garbage"), 0o644)
	tr, err = BulkLoad(path, SliceFeed(kA, vA), BulkOptions{}, Options{CacheBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()
	read(path, mA, "after load over stale temp")
	_ = sort.Strings
}
