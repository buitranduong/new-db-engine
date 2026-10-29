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
	"stask/internal/wal"
)

func kvTable() schema.Table {
	return schema.Table{Name: "kv", Key: "id", Columns: []schema.Column{
		{Name: "id", Type: "string"}, {Name: "n", Type: "int"}, {Name: "s", Type: "string"},
	}}
}

func open(t *testing.T, dir string, seg int64) *Engine {
	t.Helper()
	e, err := Open(Options{Dir: dir, SegmentSize: seg, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func row(id string, n int64) schema.Row {
	return schema.Row{{S: id}, {I: n}, {S: fmt.Sprintf("v-%s-%d-%0300d", id, n, n)}}
}

func TestWriteReadRecover(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20) // small segments to exercise rolling
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	lsnBeforeUpd := e.DurableLSN()
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("7", 700)}, {Table: "kv", Key: "8"}}); err != nil {
		t.Fatal(err)
	}
	check := func(e *Engine) {
		r, _, err := e.Get("kv", "7")
		if err != nil || r == nil || r[1].I != 700 {
			t.Fatalf("get 7: %v %v", r, err)
		}
		if r, _, _ := e.Get("kv", "8"); r != nil {
			t.Fatalf("8 should be deleted")
		}
		if r, _, _ := e.GetAt("kv", "7", lsnBeforeUpd); r == nil || r[1].I != 7 {
			t.Fatalf("snapshot read of 7: %v", r)
		}
		if r, _, _ := e.GetAt("kv", "8", lsnBeforeUpd); r == nil {
			t.Fatalf("snapshot read of 8 should exist")
		}
		h, err := e.History("kv", "7", 10)
		if err != nil || len(h) != 2 || h[0].Row[1].I != 700 || h[1].Row[1].I != 7 {
			t.Fatalf("history: %+v %v", h, err)
		}
		// the count follows the index, which the installer updates after
		// the commit is acknowledged
		e.waitInstalled(e.DurableLSN())
		tb, _ := e.Table("kv")
		if tb.LiveRows() != 4999 {
			t.Fatalf("live rows = %d", tb.LiveRows())
		}
		rows, err := e.ScanRange("kv", "10", "11", 1000, e.DurableLSN())
		if err != nil || len(rows) != 111 { // "10", "100".."109", "1000".."1099"
			t.Fatalf("scan: %d %v", len(rows), err)
		}
	}
	check(e)
	if len(e.Log().Segments()) < 2 {
		t.Fatalf("expected segment rolls, got %v", e.Log().Segments())
	}
	e.Close()

	e = open(t, dir, 1<<20)
	check(e)
	// checkpoint + more writes + recover from checkpoint
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("7", 701)}}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	if e.Stats.Recovery.CheckpointKeys != 5000 || e.Stats.Recovery.ReplayRecords != 1 {
		t.Fatalf("recovery stats %+v", e.Stats.Recovery)
	}
	r, _, _ := e.Get("kv", "7")
	if r[1].I != 701 {
		t.Fatalf("after ckpt recovery: %v", r)
	}
	h, _ := e.History("kv", "7", 10)
	if len(h) != 3 {
		t.Fatalf("history after ckpt: %d", len(h))
	}
}

func TestTornTail(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	e.CreateTable(kvTable())
	for i := 0; i < 100; i++ {
		e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}})
	}
	end := e.Log().WritePos()
	e.Close()
	// simulate a torn final write: garbage half-record after the end
	f, _ := os.OpenFile(filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", end.Seg)), os.O_RDWR, 0)
	f.WriteAt([]byte{200, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7}, end.Off)
	f.Close()
	e = open(t, dir, 1<<20)
	if !e.Stats.Recovery.TornTail || e.DurableLSN() != 101 {
		t.Fatalf("recovery %+v lsn=%d", e.Stats.Recovery, e.DurableLSN())
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("x", 1)}}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	if r, _, _ := e.Get("kv", "x"); r == nil || e.Stats.Recovery.TornTail {
		t.Fatalf("x missing or tail still torn: %+v", e.Stats.Recovery)
	}
}

// Bank transfers under SI: the total must be conserved.
func TestSnapshotIsolationBank(t *testing.T) {
	e := open(t, t.TempDir(), 64<<20)
	defer e.Close()
	e.CreateTable(kvTable())
	const accts = 20
	for i := 0; i < accts; i++ {
		e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), 1000)}})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	conflicts := 0
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				a, b := fmt.Sprint((w+i)%accts), fmt.Sprint((w*7+i*3+1)%accts)
				if a == b {
					continue
				}
				for {
					tx := e.Begin(SnapshotIsolation)
					ra, _, _ := tx.Get("kv", a)
					rb, _, _ := tx.Get("kv", b)
					ra[1].I -= 5
					rb[1].I += 5
					tx.Put("kv", ra)
					tx.Put("kv", rb)
					_, err := tx.Commit()
					if errors.Is(err, ErrConflict) {
						mu.Lock()
						conflicts++
						mu.Unlock()
						continue
					}
					if err != nil {
						t.Error(err)
					}
					break
				}
			}
		}(w)
	}
	wg.Wait()
	sum := int64(0)
	e.ScanSnapshot("kv", e.DurableLSN(), func(k string, r schema.Row, v *Version) bool {
		sum += r[1].I
		return true
	})
	if sum != accts*1000 {
		t.Fatalf("sum %d", sum)
	}
	t.Logf("conflicts retried: %d", conflicts)
}

// Write skew is allowed by SI and must be prevented by Serializable.
func TestSerializablePreventsWriteSkew(t *testing.T) {
	e := open(t, t.TempDir(), 64<<20)
	defer e.Close()
	e.CreateTable(kvTable())
	e.Write([]WriteOp{{Table: "kv", Row: row("a", 1)}, {Table: "kv", Row: row("b", 1)}})
	run := func(iso Isolation) (error, error) {
		t1, t2 := e.Begin(iso), e.Begin(iso)
		// each reads both, writes the other's key to 0 if sum >= 2
		ra, _, _ := t1.Get("kv", "a")
		rb, _, _ := t1.Get("kv", "b")
		_ = ra
		rb2, _, _ := t2.Get("kv", "b")
		ra2, _, _ := t2.Get("kv", "a")
		_ = rb2
		rb[1].I = 0
		t1.Put("kv", rb)
		ra2[1].I = 0
		t2.Put("kv", ra2)
		_, e1 := t1.Commit()
		_, e2 := t2.Commit()
		return e1, e2
	}
	e1, e2 := run(SnapshotIsolation)
	if e1 != nil || e2 != nil {
		t.Fatalf("SI should allow write skew: %v %v", e1, e2)
	}
	e.Write([]WriteOp{{Table: "kv", Row: row("a", 1)}, {Table: "kv", Row: row("b", 1)}})
	e1, e2 = run(Serializable)
	if e1 != nil || !errors.Is(e2, ErrConflict) {
		t.Fatalf("serializable must reject the second: %v %v", e1, e2)
	}
}

func TestInsertIfAbsent(t *testing.T) {
	e := open(t, t.TempDir(), 64<<20)
	defer e.Close()
	e.CreateTable(kvTable())
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("a", 1), IfAbsent: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("a", 2), IfAbsent: true}}); !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
}

// powerCut fails the n-th fdatasync. The test then rewrites everything
// after the last durable position with a random mix of kept bytes, zeros
// and garbage, i.e. any subset of the unsynced write may or may not have
// reached the disk.
type powerCut struct {
	syncs  atomic.Int64
	failAt int64
}

func (p *powerCut) BeforeWrite(n int) (int, error) { return n, nil }
func (p *powerCut) BeforeCreate() error            { return nil }
func (p *powerCut) BeforeSync() error {
	if p.syncs.Add(1) == p.failAt {
		return errors.New("simulated power cut")
	}
	return nil
}

func TestPowerLossSimulation(t *testing.T) { powerLossTrials(t, false) }

// Same, while log reclamation keeps relocating versions and deleting
// segments; every transaction also overwrites a per-writer hot key.
func TestPowerLossDuringGC(t *testing.T) { powerLossTrials(t, true) }

func powerLossTrials(t *testing.T, gc bool) {
	rng := rand.New(rand.NewSource(3))
	opts := func(f wal.Fault) Options {
		o := Options{Dir: "", SegmentSize: 256 << 10, NoCheckpoint: true, Fault: f, Logf: func(string, ...any) {}}
		if gc {
			o.GCRetention, o.GCMaxLiveRatio, o.GCRateBytes = -1, 0.9, -1
		}
		return o
	}
	for trial := 0; trial < 40; trial++ {
		dir := t.TempDir()
		pc := &powerCut{failAt: int64(5 + rng.Intn(150))}
		if gc {
			pc.failAt += 150
		}
		o := opts(pc)
		o.Dir = dir
		e, err := Open(o)
		if err != nil {
			t.Fatal(err)
		}
		e.CreateTable(kvTable())
		var mu sync.Mutex
		acked := map[string]int64{}
		tried := map[string]int64{}
		var wg sync.WaitGroup
		gcDone := make(chan struct{})
		if gc {
			go func() {
				defer close(gcDone)
				for e.Failed() == nil {
					e.GCRound()
				}
			}()
		} else {
			close(gcDone)
		}
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 100000; i++ {
					id := fmt.Sprintf("%d-%d", w, i)
					v := int64(w*1_000_000 + i)
					mu.Lock()
					tried[id] = v
					mu.Unlock()
					ops := []WriteOp{{Table: "kv", Row: row(id+"a", v)}, {Table: "kv", Row: row(id+"b", v)}}
					if gc {
						ops = append(ops, WriteOp{Table: "kv", Row: row(fmt.Sprintf("hot-%d", w), v)})
					}
					_, err := e.Write(ops)
					if err != nil {
						return
					}
					mu.Lock()
					acked[id] = v
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		<-gcDone
		if e.Failed() == nil {
			t.Fatalf("trial %d: engine still accepts writes after a failed fsync", trial)
		}
		_, durablePos, _ := e.DurablePoint()
		end := e.Log().WritePos()
		e.Close()
		// mangle the unsynced suffix [durablePos, end). Roll fsyncs a segment
		// before the first write to the next one, so only the last segment
		// can hold unsynced data, even if the durable point lags behind it.
		if durablePos.Seg < end.Seg {
			durablePos = wal.Pos{Seg: end.Seg}
		}
		for seg := durablePos.Seg; seg <= end.Seg; seg++ {
			f, err := os.OpenFile(filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", seg)), os.O_RDWR, 0)
			if err != nil {
				continue
			}
			from, to := int64(0), int64(256<<10)
			if seg == durablePos.Seg {
				from = durablePos.Off
			}
			if seg == end.Seg {
				to = end.Off
			}
			for off := from; off < to; off += 512 {
				blk := make([]byte, min(512, to-off))
				switch rng.Intn(3) {
				case 0: // persisted
					continue
				case 1: // never reached the disk
				case 2: // garbage
					rng.Read(blk)
				}
				f.WriteAt(blk, off)
			}
			f.Close()
		}
		o2 := opts(nil)
		o2.Dir = dir
		e2, err := Open(o2)
		if err != nil {
			t.Fatalf("trial %d: reopen: %v", trial, err)
		}
		for id, v := range acked {
			ra, _, _ := e2.Get("kv", id+"a")
			rb, _, _ := e2.Get("kv", id+"b")
			if ra == nil || rb == nil || ra[1].I != v || rb[1].I != v {
				t.Fatalf("trial %d: acked tx %s lost or wrong", trial, id)
			}
		}
		for id := range tried {
			ra, _, _ := e2.Get("kv", id+"a")
			rb, _, _ := e2.Get("kv", id+"b")
			if (ra == nil) != (rb == nil) {
				t.Fatalf("trial %d: tx %s partially visible", trial, id)
			}
		}
		if gc {
			for w := 0; w < 8; w++ {
				h, _, _ := e2.Get("kv", fmt.Sprintf("hot-%d", w))
				var last int64 = -1
				for id, v := range acked {
					var ww, i int
					fmt.Sscanf(id, "%d-%d", &ww, &i)
					if ww == w && v > last {
						last = v
					}
				}
				if last >= 0 && (h == nil || h[1].I < last) {
					t.Fatalf("trial %d: hot key of writer %d regressed: %v < %d", trial, w, h, last)
				}
				if h != nil {
					id := fmt.Sprintf("%d-%d", w, h[1].I-int64(w*1_000_000))
					if ra, _, _ := e2.Get("kv", id+"a"); ra == nil {
						t.Fatalf("trial %d: hot key %v survived without its transaction", trial, h)
					}
				}
			}
		}
		// the recovered log must accept new commits and survive another restart
		if _, err := e2.Write([]WriteOp{{Table: "kv", Row: row("after", 1)}}); err != nil {
			t.Fatalf("trial %d: write after recovery: %v", trial, err)
		}
		e2.Close()
		e3, err := Open(o2)
		if err != nil {
			t.Fatalf("trial %d: second reopen: %v", trial, err)
		}
		if r, _, _ := e3.Get("kv", "after"); r == nil {
			t.Fatalf("trial %d: post-recovery write lost", trial)
		}
		e3.Close()
		t.Logf("trial %d: acked=%d tried=%d torn=%v reclaimed=%d moved=%d", trial, len(acked), len(tried),
			e2.Stats.Recovery.TornTail, e.Stats.GCReclaimedSegs.Load(), e.Stats.GCMovedVersions.Load())
	}
}

func TestPreZeroRolls(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, PreZero: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	e.CreateTable(kvTable())
	for i := 0; i < 8000; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	if e.Log().ZeroBytes() == 0 {
		t.Fatal("no segment was pre-zeroed")
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	if tb, _ := e.Table("kv"); tb.LiveRows() != 8000 {
		t.Fatalf("live rows %d", tb.LiveRows())
	}
}

type failNextSync struct{ armed atomic.Bool }

func (f *failNextSync) BeforeWrite(n int) (int, error) { return n, nil }
func (f *failNextSync) BeforeCreate() error            { return nil }
func (f *failNextSync) BeforeSync() error {
	if f.armed.Load() {
		return errors.New("EIO (injected)")
	}
	return nil
}

// A failed fsync while rolling to a new segment must stop all writes; it
// must never be followed by a "successful" fsync and an acknowledgement.
func TestSyncFailureDuringRollIsFatal(t *testing.T) {
	f := &failNextSync{}
	e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 64 << 10, NoCheckpoint: true, Fault: f, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.CreateTable(kvTable())
	for e.Log().Remaining() > 4<<10 {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("a", 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	f.armed.Store(true)
	big := schema.Row{{S: "big"}, {I: 1}, {S: string(make([]byte, 8<<10))}} // does not fit: forces Roll
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: big}}); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("want ErrUnknownOutcome, got %v", err)
	}
	f.armed.Store(false)
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("b", 1)}}); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("writes must stay disabled after a failed fsync, got %v", err)
	}
}

// Close must return while writers and GC are still submitting commits
// (regression: a request queued as the committer exited hung Close forever).
func TestCloseUnderLoad(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		e, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, NoCheckpoint: true, GC: true,
			GCInterval: time.Millisecond, GCRetention: -1, GCMaxLiveRatio: 0.9, Logf: func(string, ...any) {}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.CreateTable(kvTable()); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for w := 0; w < 16; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; ; i++ {
					if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(w*50+i%50), int64(i))}}); err != nil {
						return
					}
				}
			}(w)
		}
		time.Sleep(time.Duration(20+trial*5) * time.Millisecond)
		done := make(chan struct{})
		go func() { e.Close(); wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: Close or writers hung", trial)
		}
	}
}

// A transaction whose log record exceeds the reader's frame limit must stay
// durable: acknowledged means recoverable, together with everything after it.
func TestLargeTransactionRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~90MB")
	}
	dir := t.TempDir()
	e := open(t, dir, 256<<20)
	e.CreateTable(kvTable())
	const n = 250_000 // ~85MB in one transaction
	ops := make([]WriteOp, n)
	for i := range ops {
		ops[i] = WriteOp{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}
	}
	if _, err := e.Write(ops); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("after", 1)}}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 256<<20)
	defer e.Close()
	if tb, _ := e.Table("kv"); tb.LiveRows() != n+1 {
		t.Fatalf("live rows after restart %d, want %d", tb.LiveRows(), n+1)
	}
}

// A crash after some frames of a split transaction reached the disk, but
// before its final frame did, must lose exactly that transaction, and the
// discarded frames must not come back after later writes and restarts.
func TestIncompleteFrameChainDiscarded(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 64<<20)
	e.CreateTable(kvTable())
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("before", 1)}}); err != nil {
		t.Fatal(err)
	}
	const n = 3*wal.MaxFrameOps + 10
	ops := make([]WriteOp, n)
	for i := range ops {
		ops[i] = WriteOp{Table: "kv", Row: row(fmt.Sprint(i), int64(i))}
	}
	if _, err := e.Write(ops); err != nil {
		t.Fatal(err)
	}
	e.Close()

	// cut the log at the final frame of the large transaction
	var final wal.Pos
	parts := 0
	l, err := wal.Open(filepath.Join(dir, "wal"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	l.Scan(0, 0, -1, func(r *wal.Record) error {
		if r.Kind == wal.KindTxPart {
			parts++
		}
		if r.Kind == wal.KindTx && len(r.Ops) > 1 {
			final = r.Pos
		}
		return nil
	})
	l.Close()
	if parts != 3 || final.Off == 0 {
		t.Fatalf("expected 3 part frames before the final frame, got %d (final at %v)", parts, final)
	}
	f, err := os.OpenFile(filepath.Join(dir, "wal", "00000000.wal"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt(make([]byte, 1<<20), final.Off)
	f.Close()

	e = open(t, dir, 64<<20)
	if tb, _ := e.Table("kv"); tb.LiveRows() != 1 || !e.Stats.Recovery.TornTail {
		t.Fatalf("live rows %d torn %v; want only the row written before", tb.LiveRows(), e.Stats.Recovery.TornTail)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("later", 2)}}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 64<<20)
	defer e.Close()
	if tb, _ := e.Table("kv"); tb.LiveRows() != 2 {
		t.Fatalf("live rows %d after second restart, want 2", tb.LiveRows())
	}
}

// Commits are acknowledged before they are installed in the index: a write
// must still be visible to point reads and scans right after it returns,
// while large batches are being installed, and checkpoints taken meanwhile
// must not lose it.
func TestReadYourWritesBeforeInstall(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Options{Dir: dir, SegmentSize: 64 << 20, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	e.CreateTable(kvTable())
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { // large transactions keep the installer busy
		defer bg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ops := make([]WriteOp, 2000)
			for j := range ops {
				ops[j] = WriteOp{Table: "kv", Row: row(fmt.Sprintf("bulk-%d-%d", i%5, j), int64(i))}
			}
			if _, err := e.Write(ops); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if err := e.Checkpoint(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				k := fmt.Sprintf("w%d-%04d", w, i)
				if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(k, int64(i))}}); err != nil {
					t.Error(err)
					return
				}
				r, _, err := e.Get("kv", k)
				if err != nil || r == nil || r[1].I != int64(i) {
					t.Errorf("point read after commit: %v %v", r, err)
					return
				}
				rows, err := e.ScanRange("kv", k, k+"\x00", 10, e.durable.Load())
				if err != nil || len(rows) != 1 {
					t.Errorf("scan after commit: %d rows, %v", len(rows), err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	bg.Wait()
	e.Close()
	e = open(t, dir, 64<<20)
	defer e.Close()
	for w := 0; w < 8; w++ {
		for i := 0; i < 300; i++ {
			if r, _, err := e.Get("kv", fmt.Sprintf("w%d-%04d", w, i)); err != nil || r == nil {
				t.Fatalf("w%d-%04d lost after restart: %v", w, i, err)
			}
		}
	}
}

// A pre-zeroed spare segment without records must not move where the log
// continues after a restart: a replica asks the leader for the bytes after
// its last record, at the same position.
func TestRecoveryEndIgnoresEmptySpare(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, PreZero: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	e.CreateTable(kvTable())
	for i := 0; i < 100; i++ {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprint(i), 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	end := e.log.WritePos()
	spare := filepath.Join(dir, "wal", fmt.Sprintf("%08d.wal", end.Seg+1))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(spare); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no spare segment was prepared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.Close()
	e2, err := Open(Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, PreZero: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if got := e2.log.WritePos(); got != end {
		t.Fatalf("log continues at %d:%d after restart, want %d:%d (end of the last record)", got.Seg, got.Off, end.Seg, end.Off)
	}
	if _, err := e2.Write([]WriteOp{{Table: "kv", Row: row("x", 1)}}); err != nil {
		t.Fatal(err)
	}
}
