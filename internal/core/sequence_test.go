package core

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func newSeq(t *testing.T, e *Engine, s Sequence, hasMin, hasMax, hasStart bool) {
	t.Helper()
	if s.Inc == 0 {
		s.Inc = 1
	}
	if err := SequenceDefaults(&s, math.MinInt64, math.MaxInt64, hasMin, hasMax, hasStart); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateSequence(s); err != nil {
		t.Fatal(err)
	}
}

func next(t *testing.T, e *Engine, name string, want int64) {
	t.Helper()
	if v, err := e.NextVal(name); err != nil || v != want {
		t.Fatalf("nextval(%s) = %d, %v; want %d", name, v, err, want)
	}
}

// TestSequence covers nextval/setval, bounds and cycling, the values
// logged ahead (resumed after a restart from the log or a checkpoint),
// concurrent nextval, and that the system table stays hidden.
func TestSequence(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	newSeq(t, e, Sequence{Name: "s"}, false, false, false)
	if err := e.CreateSequence(Sequence{Name: "s", Inc: 1, Max: 10}); !errors.Is(err, ErrSequenceExists) {
		t.Fatalf("create twice: %v", err)
	}
	for i := int64(1); i <= 3; i++ {
		next(t, e, "s", i)
	}
	if err := e.SetVal("s", 100, true); err != nil {
		t.Fatal(err)
	}
	next(t, e, "s", 101) // reserves 101..132
	e.Close()
	e = open(t, dir, 1<<20)
	next(t, e, "s", 133) // reserves 133..164
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	next(t, e, "s", 165)

	newSeq(t, e, Sequence{Name: "cyc", Min: 1, Max: 3, Cycle: true}, true, true, false)
	for _, v := range []int64{1, 2, 3, 1, 2} {
		next(t, e, "cyc", v)
	}
	newSeq(t, e, Sequence{Name: "lim", Min: 1, Max: 2}, true, true, false)
	next(t, e, "lim", 1)
	next(t, e, "lim", 2)
	if _, err := e.NextVal("lim"); !errors.Is(err, ErrSequenceLimit) {
		t.Fatalf("past the maximum: %v", err)
	}
	newSeq(t, e, Sequence{Name: "down", Inc: -2}, false, false, false)
	next(t, e, "down", -1)
	next(t, e, "down", -3)
	if err := e.SetVal("down", 5, true); !errors.Is(err, ErrSequenceBounds) {
		t.Fatalf("setval out of bounds: %v", err)
	}

	// concurrent nextval never hands out a value twice
	newSeq(t, e, Sequence{Name: "par"}, false, false, false)
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				v, err := e.NextVal("par")
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[v] {
					t.Errorf("value %d handed out twice", v)
				}
				seen[v] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 4000 {
		t.Fatalf("%d distinct values, want 4000", len(seen))
	}

	if err := e.AlterSequence("s", func(d *Sequence) (*int64, error) { d.Inc = 10; return &d.Start, nil }); err != nil {
		t.Fatal(err)
	}
	next(t, e, "s", 1)
	next(t, e, "s", 11)
	if err := e.DropSequence("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NextVal("s"); !errors.Is(err, ErrNoSequence) {
		t.Fatalf("nextval of a dropped sequence: %v", err)
	}
	if _, err := e.UserTable(seqTableName); !errors.Is(err, ErrNoTable) {
		t.Fatalf("system table visible: %v", err)
	}
	for _, ts := range e.UserTables() {
		if ts.Schema().System {
			t.Fatal("UserTables lists the system table")
		}
	}
	if got := len(e.Sequences()); got != 4 {
		t.Fatalf("%d sequences, want 4", got)
	}
}
