package core

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/schema"
)

func openDisk(t *testing.T, dir string, disk bool, hook func(string)) *Engine {
	t.Helper()
	e, err := Open(Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf,
		DiskIndexes: disk, DiskIndexCacheMB: 4, SidxHook: hook})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// eqErr compares the index with a full scan for n = v at snap.
func eqErr(e *Engine, snap uint64, v int64) error {
	ts, err := e.Table("kv")
	if err != nil {
		return err
	}
	lo := encInt(v)
	var got []string
	ok, err := e.IndexScan(ts, "kv_n", snap, lo, PrefixEnd(lo), false, func(pk string, r schema.Row) bool {
		got = append(got, pk)
		return true
	})
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("index cannot serve")
	}
	want := bruteEq(e, "kv", snap, v)
	if !sameKeys(sorted(got), want) {
		return fmt.Errorf("n=%d at %d: index %v, scan %v", v, snap, got, want)
	}
	return nil
}

func checkAll(t *testing.T, e *Engine, when string) {
	t.Helper()
	e.waitInstalled(e.DurableLSN())
	snap := e.DurableLSN()
	for v := int64(0); v < 50; v++ {
		if err := eqErr(e, snap, v); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
	}
}

func storageIs(t *testing.T, e *Engine, want string) {
	t.Helper()
	got, err := e.IndexStorage("kv", "kv_n")
	if err != nil || got != want {
		t.Fatalf("storage = %q, %v; want %q", got, err, want)
	}
	ts, _ := e.Table("kv")
	cat := ""
	if want == "disk" {
		cat = "disk"
	}
	if c := ts.Schema().Index("kv_n").Storage; c != cat {
		t.Fatalf("catalog storage = %q, want %q", c, cat)
	}
}

func fillKV(t *testing.T, e *Engine, lo, hi int) {
	t.Helper()
	for i := lo; i < hi; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%05d", i), int64(i%50))}}); err != nil {
			t.Fatal(err)
		}
	}
}

// Off by default: the option must be given.
func TestDiskIndexOffByDefault(t *testing.T) {
	e := openDisk(t, t.TempDir(), false, nil)
	defer e.Close()
	e.CreateTable(kvTable())
	fillKV(t, e, 0, 100)
	if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetIndexStorage("kv", "kv_n", "disk"); !errors.Is(err, ErrDiskIndexOff) {
		t.Fatalf("disk without the option: %v", err)
	}
	storageIs(t, e, "memory")
	if err := e.SetIndexStorage("kv", "kv_n", "memory"); err != nil {
		t.Fatalf("memory on a memory index: %v", err)
	}
	if err := e.SetIndexStorage("kv", "kv_n", "tape"); err == nil {
		t.Fatal("bad value accepted")
	}
	if _, err := os.Stat(filepath.Join(e.Dir(), "sidx")); err == nil {
		t.Fatal("sidx directory exists although no disk index was asked for")
	}
	checkAll(t, e, "default")
}

// Writes continue while an index moves to disk and back, repeatedly; the
// index answers like a full scan before, during and after, and across
// restarts from the log and from a checkpoint.
func TestDiskIndexOnline(t *testing.T) {
	dir := t.TempDir()
	e := openDisk(t, dir, true, nil)
	e.CreateTable(kvTable())
	fillKV(t, e, 0, 3000)
	if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var writes atomic.Int64
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for !stop.Load() {
				id := fmt.Sprintf("k%05d", rng.Intn(5000))
				var op WriteOp
				if rng.Intn(6) == 0 {
					op = WriteOp{Table: "kv", Key: id}
				} else {
					op = WriteOp{Table: "kv", Row: row(id, int64(rng.Intn(50)))}
				}
				if _, err := e.Write([]WriteOp{op}); err != nil && !errors.Is(err, ErrConflict) {
					t.Errorf("write: %v", err)
					return
				}
				writes.Add(1)
			}
		}(w)
	}
	// a reader that checks the index against scans all the time
	wg.Add(1)
	var reads atomic.Int64
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(99))
		for !stop.Load() {
			e.waitInstalled(e.DurableLSN())
			if err := eqErr(e, e.DurableLSN(), int64(rng.Intn(50))); err != nil {
				t.Errorf("concurrent check: %v", err)
				return
			}
			reads.Add(1)
		}
	}()
	// the sweeper, as the GC runs it
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			e.sweepIndexes(e.DurableLSN())
			time.Sleep(time.Millisecond)
		}
	}()
	settle := func() {
		w := writes.Load()
		for writes.Load() < w+300 {
			time.Sleep(time.Millisecond)
		}
	}
	for round := 0; round < 3; round++ {
		settle()
		if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
			t.Fatal(err)
		}
		storageIs(t, e, "disk")
		settle()
		if err := e.SetIndexStorage("kv", "kv_n", "memory"); err != nil {
			t.Fatal(err)
		}
		storageIs(t, e, "memory")
	}
	settle()
	if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
		t.Fatal(err)
	}
	settle()
	stop.Store(true)
	wg.Wait()
	t.Logf("writes %d, concurrent index checks %d", writes.Load(), reads.Load())
	if t.Failed() {
		return
	}
	storageIs(t, e, "disk")
	checkAll(t, e, "disk, quiet")
	st := e.SecIndexStats()[0]
	if st.Storage != "disk" || st.Failed != "" || st.Entries == 0 {
		t.Fatalf("stats %+v", st)
	}
	// ordered scans, both ways, through the disk store
	ts, _ := e.Table("kv")
	snap := e.DurableLSN()
	var fwd, back []int64
	e.IndexScan(ts, "kv_n", snap, "", "", false, func(_ string, r schema.Row) bool { fwd = append(fwd, r[1].I); return true })
	e.IndexScan(ts, "kv_n", snap, "", "", true, func(_ string, r schema.Row) bool { back = append(back, r[1].I); return true })
	if len(fwd) == 0 || len(fwd) != len(back) {
		t.Fatalf("scan sizes %d/%d", len(fwd), len(back))
	}
	for i := range fwd {
		if i > 0 && fwd[i] < fwd[i-1] {
			t.Fatalf("forward scan not ordered at %d", i)
		}
		if fwd[i] != back[len(back)-1-i] {
			t.Fatalf("forward and backward scans disagree at %d", i)
		}
	}

	// restart from the log: replay creates the index in RAM, then the ALTER
	// moves it to disk
	e.Close()
	e = openDisk(t, dir, true, nil)
	storageIs(t, e, "disk")
	checkAll(t, e, "after replay")
	fillKV(t, e, 6000, 6100)
	checkAll(t, e, "after replay and writes")
	// restart from a checkpoint: the index is rebuilt as a disk index
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	fillKV(t, e, 6100, 6200)
	e.Close()
	e = openDisk(t, dir, true, nil)
	storageIs(t, e, "disk")
	checkAll(t, e, "after checkpoint")
	// the same data directory without the option: the index is kept in RAM
	e.Close()
	e = openDisk(t, dir, false, nil)
	if got, _ := e.IndexStorage("kv", "kv_n"); got != "memory" {
		t.Fatalf("without the option the index is %q", got)
	}
	checkAll(t, e, "option off")
	// DROP INDEX removes the file
	e.Close()
	e = openDisk(t, dir, true, nil)
	defer e.Close()
	if _, err := os.Stat(e.sidxPath(ts, "kv_n")); err != nil {
		t.Fatalf("disk index file missing: %v", err)
	}
	if err := e.DropIndex("kv", "kv_n"); err != nil {
		t.Fatal(err)
	}
	waitGone := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(e.sidxPath(ts, "kv_n")); err != nil {
			break
		}
		if time.Now().After(waitGone) {
			t.Fatal("disk index file survived DROP INDEX")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A unique index on disk still enforces uniqueness.
func TestDiskIndexUnique(t *testing.T) {
	e := openDisk(t, t.TempDir(), true, nil)
	defer e.Close()
	e.CreateTable(kvTable())
	for i := 0; i < 200; i++ {
		e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%03d", i), int64(i))}})
	}
	if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}, Unique: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
		t.Fatal(err)
	}
	var ue *UniqueViolationError
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("dup", 7)}}); !errors.As(err, &ue) {
		t.Fatalf("duplicate on a disk unique index: %v", err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("k007", 7)}}); err != nil {
		t.Fatalf("rewrite own value: %v", err)
	}
	// move a row away, then another may take its value
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("k007", 1007)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("fresh", 7)}}); err != nil {
		t.Fatalf("value freed by an update: %v", err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("again", 7)}}); !errors.As(err, &ue) {
		t.Fatalf("second holder: %v", err)
	}
}

// Crash at every step of a conversion: the data directory as it is at that
// step must open, serve correct index scans, and end with the index where
// the catalog says.
func TestDiskIndexCrashPoints(t *testing.T) {
	dir := t.TempDir()
	images := map[string]string{}
	var e *Engine
	hook := func(step string) {
		switch step {
		case "bulk-before-rename", "bulk-after-rename", "before-catalog", "after-catalog", "swapped-to-memory":
			if _, seen := images[step]; seen {
				return // the first conversion (to disk) is the one under test
			}
			img := t.TempDir()
			copyDir(t, dir, img)
			images[step] = img
		}
	}
	e = openDisk(t, dir, true, hook)
	e.CreateTable(kvTable())
	fillKV(t, e, 0, 2000)
	if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
		t.Fatal(err)
	}
	fillKV(t, e, 2000, 2100)
	if err := e.SetIndexStorage("kv", "kv_n", "memory"); err != nil {
		t.Fatal(err)
	}
	e.Close()

	want := map[string]string{ // catalog at the step, so the storage after restart
		"bulk-before-rename": "memory",
		"bulk-after-rename":  "memory",
		"before-catalog":     "memory", // the swap is done, the catalog is not: memory after a crash
		"after-catalog":      "disk",
	}
	for step, storage := range want {
		img := images[step]
		if img == "" {
			t.Fatalf("no crash image for %s", step)
		}
		switch step {
		case "bulk-before-rename":
			if m, _ := filepath.Glob(filepath.Join(img, "sidx", "*.bulk")); len(m) != 1 {
				t.Fatalf("%s: expected a stale temp file in the image, got %v", step, m)
			}
		case "bulk-after-rename", "before-catalog":
			if m, _ := filepath.Glob(filepath.Join(img, "sidx", "*.pidx")); len(m) != 1 {
				t.Fatalf("%s: expected an orphan index file in the image, got %v", step, m)
			}
		}
		for _, optOn := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/option=%v", step, optOn), func(t *testing.T) {
				work := t.TempDir()
				copyDir(t, img, work)
				e2 := openDisk(t, work, optOn, nil)
				defer e2.Close()
				exp := storage
				if !optOn {
					exp = "memory"
				}
				got, err := e2.IndexStorage("kv", "kv_n")
				if err != nil || got != exp {
					t.Fatalf("after restart: storage %q, %v; want %q", got, err, exp)
				}
				checkAll(t, e2, "restart "+step)
				// no junk is left: only the index the catalog asks for
				files, _ := filepath.Glob(filepath.Join(work, "sidx", "*"))
				if exp == "disk" && len(files) != 1 || exp == "memory" && len(files) != 0 {
					t.Fatalf("sidx after restart: %v", files)
				}
				// and it is writable and still correct
				fillKV(t, e2, 3000, 3100)
				checkAll(t, e2, "restart+writes "+step)
				// the conversion can be redone
				if optOn {
					if err := e2.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
						t.Fatal(err)
					}
					storageIs(t, e2, "disk")
					checkAll(t, e2, "redo "+step)
				}
			})
		}
	}
	if images["swapped-to-memory"] == "" {
		t.Fatal("no image after the swap back to memory")
	}
}
