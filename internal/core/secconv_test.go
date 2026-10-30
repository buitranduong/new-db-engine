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

func waitStorage(t *testing.T, e *Engine, want string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if got, err := e.IndexStorage("kv", "kv_n"); err == nil && got == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	got, err := e.IndexStorage("kv", "kv_n")
	t.Fatalf("storage never became %s: %q, %v", want, got, err)
}

// DROP INDEX while a conversion is in each of its phases: the conversion
// ends with an error, nothing is left behind, and an index of the same name
// can be created and converted afterwards.
func TestDiskIndexDropRacesConversion(t *testing.T) {
	for _, phase := range []string{"bulk-before-rename", "bulk-after-rename", "bulk-loaded"} {
		for _, dir := range []string{"to-disk", "to-memory"} {
			if dir == "to-memory" && phase != "bulk-loaded" {
				continue
			}
			t.Run(phase+"/"+dir, func(t *testing.T) {
				wd := t.TempDir()
				var armed atomic.Bool
				reached, release := make(chan struct{}, 1), make(chan struct{})
				hook := func(step string) {
					want := phase
					if dir == "to-memory" {
						want = "memory-built"
					}
					if step == want && armed.CompareAndSwap(true, false) {
						reached <- struct{}{}
						<-release
					}
				}
				e := openDisk(t, wd, true, hook)
				defer e.Close()
				e.CreateTable(kvTable())
				fillKV(t, e, 0, 1500)
				if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
					t.Fatal(err)
				}
				if dir == "to-memory" {
					if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
						t.Fatal(err)
					}
				}
				armed.Store(true)
				to := "disk"
				if dir == "to-memory" {
					to = "memory"
				}
				errc := make(chan error, 1)
				go func() { errc <- e.SetIndexStorage("kv", "kv_n", to) }()
				<-reached
				dropped := make(chan error, 1)
				go func() { dropped <- e.DropIndex("kv", "kv_n") }()
				time.Sleep(60 * time.Millisecond) // the drop is now waiting for the conversion to stop
				close(release)
				if err := <-errc; err == nil {
					t.Fatal("the conversion of a dropped index reported success")
				}
				if err := <-dropped; err != nil {
					t.Fatal(err)
				}
				if m, _ := filepath.Glob(filepath.Join(wd, "sidx", "*")); len(m) != 0 {
					t.Fatalf("files left after the drop: %v", m)
				}
				// same name again
				if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
					t.Fatal(err)
				}
				if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
					t.Fatal(err)
				}
				checkAll(t, e, "recreated")
			})
		}
	}
}

// The ALTER logged by a run that was killed is converted in the background
// when the log is replayed: Open returns while the index is still being
// built, the index answers (from RAM) and takes writes, and ends on disk.
func TestDiskIndexReplayConvertsOnline(t *testing.T) {
	dir := t.TempDir()
	e := openDisk(t, dir, true, nil)
	e.CreateTable(kvTable())
	fillKV(t, e, 0, 1500)
	e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}})
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil { // logged after the checkpoint
		t.Fatal(err)
	}
	fillKV(t, e, 1500, 1600)
	e.waitInstalled(e.DurableLSN())
	img := t.TempDir()
	copyDir(t, dir, img)
	e.Close()

	reached, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	hook := func(step string) {
		if step == "bulk-loaded" {
			once.Do(func() { reached <- struct{}{}; <-release })
		}
	}
	e2 := openDisk(t, img, true, hook) // would never return if replay waited for the conversion
	<-reached
	if got, _ := e2.IndexStorage("kv", "kv_n"); got != "memory" {
		t.Fatalf("storage %q while converting", got)
	}
	checkAll(t, e2, "while converting")
	fillKV(t, e2, 2000, 2200) // writes during the conversion are carried over
	close(release)
	waitStorage(t, e2, "disk")
	checkAll(t, e2, "after")
	if e2.SidxAsync.Load() != 1 {
		t.Fatalf("async conversions = %d", e2.SidxAsync.Load())
	}
	if st := secStat(t, e2); st.ConvCatchUp+st.ConvFinal < 200 {
		t.Fatalf("writes during the conversion were not carried over: %+v", st)
	}
	e2.Close()

	// Close during a conversion does not hang and leaves nothing that matters
	img2 := t.TempDir()
	copyDir(t, img, img2)
	reached2, release2 := make(chan struct{}, 1), make(chan struct{})
	var once2 sync.Once
	e3 := openDisk(t, img2, true, func(step string) {
		if step == "bulk-loaded" {
			once2.Do(func() { reached2 <- struct{}{}; <-release2 })
		}
	})
	<-reached2
	closed := make(chan struct{})
	go func() { e3.Close(); close(closed) }()
	time.Sleep(50 * time.Millisecond)
	close(release2)
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("Close hangs while a conversion runs")
	}
	e4 := openDisk(t, img2, true, nil)
	defer e4.Close()
	waitStorage(t, e4, "disk")
	checkAll(t, e4, "after aborted conversion")
}

// The conversion log is bounded in RAM: with a small limit the ops spill to a
// temp file, the result is the same, and the temp file is gone afterwards.
func TestDiskIndexConversionLogSpills(t *testing.T) {
	for _, dir := range []string{"to-disk", "to-memory"} {
		t.Run(dir, func(t *testing.T) {
			wd := t.TempDir()
			var e *Engine
			var armed atomic.Bool
			hook := func(step string) {
				want := "bulk-loaded"
				if dir == "to-memory" {
					want = "memory-built"
				}
				if step == want && armed.CompareAndSwap(true, false) {
					// ops piled up in the log during the build; more arrive now
					rng := rand.New(rand.NewSource(5))
					for i := 0; i < 1500; i++ {
						id := fmt.Sprintf("k%05d", rng.Intn(2500))
						var op WriteOp
						if rng.Intn(5) == 0 {
							op = WriteOp{Table: "kv", Key: id}
						} else {
							op = WriteOp{Table: "kv", Row: row(id, int64(rng.Intn(50)))}
						}
						if _, err := e.Write([]WriteOp{op}); err != nil && !errors.Is(err, ErrConflict) {
							t.Error(err)
							return
						}
					}
					if m, _ := filepath.Glob(filepath.Join(wd, "sidx", "mig-*.tmp")); len(m) == 0 {
						t.Error("no spill file although the log exceeds the limit")
					}
				}
			}
			var err error
			e, err = Open(Options{Dir: wd, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf,
				DiskIndexes: true, DiskIndexCacheMB: 4, SidxHook: hook, SidxMigMemOps: 40})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			e.CreateTable(kvTable())
			fillKV(t, e, 0, 2000)
			e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}})
			if dir == "to-memory" {
				if err := e.SetIndexStorage("kv", "kv_n", "disk"); err != nil {
					t.Fatal(err)
				}
			}
			armed.Store(true)
			to := "disk"
			if dir == "to-memory" {
				to = "memory"
			}
			if err := e.SetIndexStorage("kv", "kv_n", to); err != nil {
				t.Fatal(err)
			}
			if st := secStat(t, e); st.ConvSpilled < 1000 {
				t.Fatalf("spilled = %d", st.ConvSpilled)
			}
			checkAll(t, e, "after spill")
			if m, _ := filepath.Glob(filepath.Join(wd, "sidx", "mig-*.tmp")); len(m) != 0 {
				t.Fatalf("temp files left: %v", m)
			}
			_ = os.Remove
		})
	}
}
