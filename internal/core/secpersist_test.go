package core

import (
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

// diskIndexDB builds a kv table with a disk index on n and some history
// (updates leave dead entries behind).
func diskIndexDB(t *testing.T, dir string, hook func(string)) *Engine {
	t.Helper()
	e := openDisk(t, dir, true, hook)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	fillKV(t, e, 0, 1500)
	if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
		t.Fatal(err)
	}
	// updates and deletes: dead entries in the file
	for i := 0; i < 400; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%05d", i), int64((i+7)%50))}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1000; i < 1100; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Key: fmt.Sprintf("k%05d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	e.waitInstalled(e.DurableLSN())
	return e
}

func secStat(t *testing.T, e *Engine) SecStats {
	t.Helper()
	for _, s := range e.SecIndexStats() {
		if s.Name == "kv_n" {
			return s
		}
	}
	t.Fatal("no kv_n in stats")
	return SecStats{}
}

// tail makes changes after a checkpoint: they must be replayed into the
// reused file.
func tail(t *testing.T, e *Engine) {
	t.Helper()
	for i := 100; i < 300; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%05d", i), int64((i*3)%50))}}); err != nil {
			t.Fatal(err)
		}
	}
	fillKV(t, e, 1500, 1600)
	for i := 1200; i < 1230; i++ {
		e.Write([]WriteOp{{Table: "kv", Key: fmt.Sprintf("k%05d", i)}})
	}
	e.waitInstalled(e.DurableLSN())
}

// The file survives a restart: after a checkpoint the index is reused (not
// rebuilt), the log tail is replayed into it, and it answers like a scan.
func TestSecPersistAdopt(t *testing.T) {
	dir := t.TempDir()
	e := diskIndexDB(t, dir, nil)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if e.SidxCkpts.Load() != 1 {
		t.Fatalf("disk file checkpoints = %d", e.SidxCkpts.Load())
	}
	pending := secStat(t, e).Pending
	tail(t, e)
	checkAll(t, e, "before crash")
	img := t.TempDir()
	copyDir(t, dir, img) // a crash: no Close
	e.Close()

	for _, name := range []string{"crash-image", "clean-dir"} {
		src := img
		if name == "clean-dir" {
			src = dir
		}
		work := t.TempDir()
		copyDir(t, src, work)
		e2 := openDisk(t, work, true, nil)
		if e2.SidxAdopted.Load() != 1 || e2.SidxRebuilt.Load() != 0 {
			t.Fatalf("%s: adopted=%d rebuilt=%d, want 1/0", name, e2.SidxAdopted.Load(), e2.SidxRebuilt.Load())
		}
		storageIs(t, e2, "disk")
		checkAll(t, e2, name)
		if p := secStat(t, e2).Pending; p < pending {
			t.Fatalf("%s: the sweeper lost its list of dead entries: %d < %d", name, p, pending)
		}
		fillKV(t, e2, 2000, 2100)
		checkAll(t, e2, name+"+writes")
		// the dead entries are swept after the reuse like before
		e2.sweepIndexes(e2.DurableLSN())
		checkAll(t, e2, name+"+sweep")
		// and the next checkpoint/restart cycle reuses it again
		if err := e2.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		e2.Close()
		e3 := openDisk(t, work, true, nil)
		if e3.SidxAdopted.Load() != 1 {
			t.Fatalf("%s: second restart did not reuse the file", name)
		}
		checkAll(t, e3, name+" 2nd")
		e3.Close()
	}
}

// Every crash point of the engine checkpoint leaves a directory that opens,
// answers correctly, and reuses or rebuilds the file as the evidence allows.
func TestSecPersistCheckpointCrashPoints(t *testing.T) {
	dir := t.TempDir()
	images := map[string]string{}
	armed := false
	hook := func(step string) {
		if !armed || (step != "ckpt-before-rename" && step != "ckpt-after-rename") {
			return
		}
		img := t.TempDir()
		copyDir(t, dir, img)
		images[step] = img
	}
	e := diskIndexDB(t, dir, hook)
	if err := e.Checkpoint(); err != nil { // checkpoint A
		t.Fatal(err)
	}
	tail(t, e)
	armed = true
	if err := e.Checkpoint(); err != nil { // checkpoint B: images inside
		t.Fatal(err)
	}
	armed = false
	e.Close()
	cases := []struct {
		step    string
		adopted int64
	}{
		{"ckpt-before-rename", 0}, // file is at B, header still A: proof fails, rebuilt
		{"ckpt-after-rename", 1},
	}
	for _, c := range cases {
		t.Run(c.step, func(t *testing.T) {
			work := t.TempDir()
			copyDir(t, images[c.step], work)
			e2 := openDisk(t, work, true, nil)
			defer e2.Close()
			if got := e2.SidxAdopted.Load(); got != c.adopted {
				t.Fatalf("adopted = %d, want %d (rebuilt %d)", got, c.adopted, e2.SidxRebuilt.Load())
			}
			if c.adopted == 0 && e2.SidxRebuilt.Load() != 1 {
				t.Fatalf("rebuilt = %d", e2.SidxRebuilt.Load())
			}
			checkAll(t, e2, c.step)
			fillKV(t, e2, 3000, 3050)
			checkAll(t, e2, c.step+"+writes")
		})
	}
}

// A damaged, missing or foreign file is never trusted.
func TestSecPersistCorruption(t *testing.T) {
	dir := t.TempDir()
	e := diskIndexDB(t, dir, nil)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	tail(t, e)
	img := t.TempDir()
	copyDir(t, dir, img)
	e.Close()
	damage := map[string]func(p string){
		"missing": func(p string) { os.Remove(p) },
		"empty":   func(p string) { os.Truncate(p, 0) },
		"half": func(p string) {
			fi, _ := os.Stat(p)
			os.Truncate(p, fi.Size()/2)
		},
		"zeroed-metas": func(p string) {
			f, _ := os.OpenFile(p, os.O_WRONLY, 0)
			f.WriteAt(make([]byte, 16384), 0)
			f.Close()
		},
		"garbage-page": func(p string) {
			f, _ := os.OpenFile(p, os.O_WRONLY, 0)
			fi, _ := f.Stat()
			junk := make([]byte, 8192)
			for i := range junk {
				junk[i] = byte(i * 7)
			}
			f.WriteAt(junk, fi.Size()/2/8192*8192)
			f.Close()
		},
		"other-file": func(p string) { // a valid but unrelated index file in its place
			os.WriteFile(p, nil, 0o644)
		},
	}
	for name, dmg := range damage {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			copyDir(t, img, work)
			m, _ := filepath.Glob(filepath.Join(work, "sidx", "*.pidx"))
			if len(m) != 1 {
				t.Fatalf("files: %v", m)
			}
			dmg(m[0])
			e2 := openDisk(t, work, true, nil)
			defer e2.Close()
			// a garbage data page inside the file is only found when read: the
			// reuse may happen (the scan at adoption reads every page, so it
			// is found there) or not, but answers are right either way
			checkAll(t, e2, name)
			storageIs(t, e2, "disk")
			if e2.SidxAdopted.Load()+e2.SidxRebuilt.Load() != 1 {
				t.Fatalf("adopted=%d rebuilt=%d", e2.SidxAdopted.Load(), e2.SidxRebuilt.Load())
			}
		})
	}
	t.Run("option-off", func(t *testing.T) {
		work := t.TempDir()
		copyDir(t, img, work)
		e2 := openDisk(t, work, false, nil)
		defer e2.Close()
		if got, _ := e2.IndexStorage("kv", "kv_n"); got != "memory" {
			t.Fatalf("storage %q with the option off", got)
		}
		checkAll(t, e2, "off")
		if m, _ := filepath.Glob(filepath.Join(work, "sidx", "*")); len(m) != 0 {
			t.Fatalf("files left with the option off: %v", m)
		}
	})
}

// An adopted file with an entry from beyond the recovered log (a commit that
// was staged into the index and lost with the crash) is rebuilt.
func TestSecPersistLostTail(t *testing.T) {
	dir := t.TempDir()
	e := diskIndexDB(t, dir, nil)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	img := t.TempDir()
	copyDir(t, dir, img)
	e.Close()
	work := t.TempDir()
	copyDir(t, img, work)
	e2 := openDisk(t, work, true, nil)
	defer e2.Close()
	if e2.SidxAdopted.Load() != 1 {
		t.Fatalf("not adopted")
	}
	// pretend the file held an entry past the log: verifyAdopted must rebuild
	ts, _ := e2.Table("kv")
	si := ts.secNamed("kv_n")
	si.apply(func(w *secW) { w.put(string(encInt(7))+"phantom", e2.DurableLSN()+5, 0) })
	e2.verifyAdopted(e2.DurableLSN())
	if e2.SidxRebuilt.Load() != 1 || e2.SidxAdopted.Load() != 0 {
		t.Fatalf("rebuilt=%d adopted=%d", e2.SidxRebuilt.Load(), e2.SidxAdopted.Load())
	}
	si2 := ts.secNamed("kv_n")
	if si2 == si {
		t.Fatal("same index")
	}
	rd, _, _ := si2.openRead()
	defer rd.close()
	rd.ascend("", func(k string, _ bool) bool {
		if len(k) >= 7 && k[len(k)-7:] == "phantom" {
			t.Fatal("phantom entry survived")
		}
		return true
	})
	checkAll(t, e2, "after rebuild")
}

// DROP INDEX and a move back to memory after the checkpoint: the file is not
// reused, nothing is left behind.
func TestSecPersistDDLAfterCheckpoint(t *testing.T) {
	for _, what := range []string{"drop", "to-memory", "drop-recreate"} {
		t.Run(what, func(t *testing.T) {
			dir := t.TempDir()
			e := diskIndexDB(t, dir, nil)
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			switch what {
			case "drop":
				if err := e.DropIndex("kv", "kv_n"); err != nil {
					t.Fatal(err)
				}
			case "to-memory":
				if err := e.SetIndexStorage("kv", "kv_n", "memory"); err != nil {
					t.Fatal(err)
				}
			case "drop-recreate":
				if err := e.DropIndex("kv", "kv_n"); err != nil {
					t.Fatal(err)
				}
				if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
					t.Fatal(err)
				}
			}
			fillKV(t, e, 1500, 1550)
			e.waitInstalled(e.DurableLSN())
			img := t.TempDir()
			copyDir(t, dir, img)
			e.Close()
			e2 := openDisk(t, img, true, nil)
			defer e2.Close()
			// a move back to memory that arrives by log replay is made in the
			// background (convertAsync), so the file goes a moment later
			var files []string
			for i := 0; i < 800; i++ {
				if files, _ = filepath.Glob(filepath.Join(img, "sidx", "*")); len(files) == 0 {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if len(files) != 0 {
				t.Fatalf("files left: %v", files)
			}
			if what == "drop" {
				ts, _ := e2.Table("kv")
				if ts.secNamed("kv_n") != nil {
					t.Fatal("dropped index is back")
				}
				return
			}
			storageIs(t, e2, "memory")
			checkAll(t, e2, what)
		})
	}
}

// Writers, checkpoints, conversions and crashes (directory copies taken
// while all of it runs, at engine checkpoint hooks) together.
func TestSecPersistStress(t *testing.T) {
	dir := t.TempDir()
	var imgMu sync.Mutex
	var images []string
	hook := func(step string) {
		if step != "ckpt-before-rename" && step != "ckpt-after-rename" {
			return
		}
		imgMu.Lock()
		defer imgMu.Unlock()
		if len(images) >= 6 {
			return
		}
		img := t.TempDir()
		copyDir(t, dir, img)
		images = append(images, img)
	}
	e := diskIndexDB(t, dir, hook)
	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w) + 11))
			for !stop.Load() {
				id := fmt.Sprintf("k%05d", rng.Intn(3000))
				op := WriteOp{Table: "kv", Row: row(id, int64(rng.Intn(50)))}
				if rng.Intn(7) == 0 {
					op = WriteOp{Table: "kv", Key: id}
				}
				e.Write([]WriteOp{op})
			}
		}(w)
	}
	for i := 0; i < 8; i++ {
		if err := e.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if i%3 == 2 {
			e.sweepIndexes(e.DurableLSN())
		}
	}
	stop.Store(true)
	wg.Wait()
	e.Close()
	imgMu.Lock()
	defer imgMu.Unlock()
	if len(images) == 0 {
		t.Fatal("no images")
	}
	for i, img := range images {
		work := t.TempDir()
		copyDir(t, img, work)
		e2 := openDisk(t, work, true, nil)
		checkAll(t, e2, fmt.Sprintf("image %d", i))
		fillKV(t, e2, 4000, 4040)
		checkAll(t, e2, fmt.Sprintf("image %d +writes", i))
		e2.Close()
	}
}
