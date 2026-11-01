package proj

import (
	"fmt"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// Loop audit (docs/31 slice1c): the merge trigger, the ring trimming and the
// directory probe loops terminate and reach a fixed point.

// A merge that finishes with no writes in between must not start another one
// (the trigger compares the residual tail, which is empty), and the ring never
// exceeds SegRingMax however many states are published.
func TestSegMergeAndRingTerminate(t *testing.T) {
	oldMin, oldFrac := SegMergeMinTail, SegMergeFrac
	SegMergeMinTail, SegMergeFrac = 50, 4
	defer func() { SegMergeMinTail, SegMergeFrac = oldMin, oldFrac }()
	e, c := segTestEngine(t)
	var last uint64
	for b := 0; b < 6; b++ { // 6 commits of 40 rows: tail crosses 50 inside the loop
		tx := e.Begin(core.SnapshotIsolation)
		for i := 0; i < 40; i++ {
			_ = tx.Put("sims", schema.Row{{S: fmt.Sprintf("09%09d", b*40+i)}, {S: "TAM_HOA"}, {I: 1000000}, {S: "A"}, {I: 7}})
		}
		l, err := tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
		last = l
	}
	waitSeg(t, c, last)
	deadline := time.Now().Add(20 * time.Second)
	for c.Merges.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		c.Follower().WaitApplied(last, 10*time.Millisecond)
		c.Flush(last)
	}
	if c.Merges.Load() == 0 {
		t.Fatal("no merge finished")
	}
	// merges already in flight may finish; with no writes the count must settle (fixed point) within a bound
	prev, stable := int64(-1), 0
	for i := 0; i < 400 && stable < 20; i++ {
		c.Flush(last)
		time.Sleep(5 * time.Millisecond)
		if m := c.Merges.Load(); m == prev {
			stable++
		} else {
			prev, stable = m, 0
		}
	}
	if stable < 20 {
		t.Fatalf("merges did not settle with no writes (count %d)", prev)
	}
	if prev > 6 {
		t.Fatalf("%d merges for 6 commits: the trigger chases itself", prev)
	}
	if n, _ := c.RingStats(); n > SegRingMax {
		t.Fatalf("ring %d exceeds the maximum %d", n, SegRingMax)
	}
}

// Probing a directory filled to just under the load limit terminates for hits
// and misses (there is always an empty slot).
func TestSegDirProbeTerminates(t *testing.T) {
	keys := make([][]byte, 5000)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("k%08d", i))
	}
	d := newSegDir(0, func(id uint32) []byte { return keys[id] })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range keys {
			d.Put(keys[i], uint32(i))
			if _, ok := d.Get([]byte(fmt.Sprintf("absent%d", i))); ok {
				t.Error("absent key found")
			}
		}
		for i := range keys {
			if id, ok := d.Get(keys[i]); !ok || int(id) != i {
				t.Errorf("key %d: %d %v", i, id, ok)
			}
		}
		if d.n*10 >= len(d.slots)*7+10 {
			t.Errorf("load factor above the limit: %d/%d", d.n, len(d.slots))
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("directory probe did not terminate")
	}
}
