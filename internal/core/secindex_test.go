package core

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"stask/internal/schema"
)

func encInt(v int64) string { return string(schema.AppendKeyValue(nil, schema.TInt, schema.Value{I: v})) }

// scanEq returns the primary keys of the rows with n = v at snap, via the index.
func scanEq(t *testing.T, e *Engine, table, idx string, snap uint64, v int64) []string {
	t.Helper()
	ts, err := e.Table(table)
	if err != nil {
		t.Fatal(err)
	}
	lo := encInt(v)
	var out []string
	ok, err := e.IndexScan(ts, idx, snap, lo, PrefixEnd(lo), false, func(pk string, r schema.Row) bool {
		if r[1].I != v {
			t.Errorf("row %s has n=%d, scanned for %d", pk, r[1].I, v)
		}
		out = append(out, pk)
		return true
	})
	if err != nil || !ok {
		t.Fatalf("IndexScan: ok=%v err=%v", ok, err)
	}
	return out
}

// bruteEq is the same by a full scan.
func bruteEq(e *Engine, table string, snap uint64, v int64) []string {
	var out []string
	e.ScanSnapshot(table, snap, func(k string, r schema.Row, _ *Version) bool {
		if !r[1].Null && r[1].I == v {
			out = append(out, k)
		}
		return true
	})
	sort.Strings(out)
	return out
}

func sorted(s []string) []string { sort.Strings(s); return s }

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSecIndex covers CREATE INDEX on a populated table, maintenance by
// inserts, updates and deletes, snapshots, the sweeper, and the index
// across restarts from the log and from a checkpoint.
func TestSecIndex(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	fill := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%04d", i), int64(i%50))}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	fill(0, 1000)
	if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_n", Columns: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	fill(1000, 2000)
	snap0 := e.DurableLSN()
	// move some rows, delete some, re-set some to a value they had before
	for i := 0; i < 2000; i += 3 {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%04d", i), int64((i+1)%50))}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < 2000; i += 7 {
		if _, err := e.Write([]WriteOp{{Table: "kv", Key: fmt.Sprintf("k%04d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2000; i += 6 { // back to the original value
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%04d", i), int64(i%50))}}); err != nil {
			t.Fatal(err)
		}
	}
	// NULL values sort last
	nr := row("knull", 0)
	nr[1].Null = true
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: nr}}); err != nil {
		t.Fatal(err)
	}
	check := func(e *Engine, when string) {
		t.Helper()
		e.waitInstalled(e.DurableLSN())
		now := e.DurableLSN()
		for v := int64(0); v < 50; v++ {
			if got, want := sorted(scanEq(t, e, "kv", "kv_n", now, v)), bruteEq(e, "kv", now, v); !sameKeys(got, want) {
				t.Fatalf("%s: n=%d: index %v, scan %v", when, v, got, want)
			}
		}
	}
	check(e, "live")
	// the snapshot before the updates still reads its own values through the index
	for v := int64(0); v < 50; v++ {
		if got, want := sorted(scanEq(t, e, "kv", "kv_n", snap0, v)), bruteEq(e, "kv", snap0, v); !sameKeys(got, want) {
			t.Fatalf("snapshot: n=%d: index %v, scan %v", v, got, want)
		}
	}
	// full order, forwards and backwards, NULL last
	ts, _ := e.Table("kv")
	var fwd, back []int64
	nulls := 0
	e.IndexScan(ts, "kv_n", e.DurableLSN(), "", "", false, func(pk string, r schema.Row) bool {
		if r[1].Null {
			nulls++
			fwd = append(fwd, 1<<40)
		} else {
			fwd = append(fwd, r[1].I)
		}
		return true
	})
	e.IndexScan(ts, "kv_n", e.DurableLSN(), "", "", true, func(pk string, r schema.Row) bool {
		if r[1].Null {
			back = append(back, 1<<40)
		} else {
			back = append(back, r[1].I)
		}
		return true
	})
	if nulls != 1 || !sort.SliceIsSorted(fwd, func(i, j int) bool { return fwd[i] < fwd[j] }) || fwd[len(fwd)-1] != 1<<40 {
		t.Fatalf("forward order: %d nulls, sorted=%v", nulls, sort.SliceIsSorted(fwd, func(i, j int) bool { return fwd[i] < fwd[j] }))
	}
	if len(back) != len(fwd) || back[0] != 1<<40 || !sort.SliceIsSorted(back, func(i, j int) bool { return back[i] > back[j] }) {
		t.Fatalf("backward order wrong")
	}
	// range [10, 20)
	n := 0
	e.IndexScan(ts, "kv_n", e.DurableLSN(), encInt(10), encInt(20), false, func(pk string, r schema.Row) bool {
		if r[1].I < 10 || r[1].I >= 20 {
			t.Errorf("range: %d", r[1].I)
		}
		n++
		return true
	})
	want := 0
	e.ScanSnapshot("kv", e.DurableLSN(), func(_ string, r schema.Row, _ *Version) bool {
		if !r[1].Null && r[1].I >= 10 && r[1].I < 20 {
			want++
		}
		return true
	})
	if n != want {
		t.Fatalf("range count %d, want %d", n, want)
	}
	// dead entries are removed once the horizon passes them
	before := e.SecIndexStats()[0]
	if before.Dead == 0 {
		t.Fatalf("no entry was marked dead: %+v", before)
	}
	e.sweepIndexes(e.DurableLSN())
	after := e.SecIndexStats()[0]
	if after.Swept == 0 || after.Entries >= before.Entries {
		t.Fatalf("sweep removed nothing: before %+v after %+v", before, after)
	}
	check(e, "after sweep")
	if _, ok := e.IndexNameTaken("kv_n"); !ok {
		t.Fatal("index name not found")
	}

	e.Close()
	e = open(t, dir, 1<<20)
	check(e, "after replay")
	fill(3000, 3100)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	fill(3100, 3200)
	e.Close()
	e = open(t, dir, 1<<20)
	check(e, "after checkpoint")
	if err := e.DropIndex("kv", "kv_n"); err != nil {
		t.Fatal(err)
	}
	ts, _ = e.Table("kv")
	if ok, _ := e.IndexScan(ts, "kv_n", e.DurableLSN(), "", "", false, nil); ok {
		t.Fatal("dropped index still scans")
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	if tb, _ := e.Table("kv"); len(tb.Schema().Indexes) != 0 || len(tb.secList()) != 0 {
		t.Fatalf("index back after restart")
	}
}

// TestUniqueIndex: a unique index refuses duplicates at build time and at
// write time, lets NULLs repeat, allows moving a value between rows in one
// transaction, and holds under concurrent inserts of the same value.
func TestUniqueIndex(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	defer e.Close()
	e.CreateTable(kvTable())
	for i := 0; i < 20; i++ {
		e.Write([]WriteOp{{Table: "kv", Row: row(fmt.Sprintf("k%02d", i), int64(i%10))}})
	}
	err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_u", Columns: []string{"n"}, Unique: true})
	var ue *UniqueViolationError
	if !errors.As(err, &ue) {
		t.Fatalf("building a unique index over duplicates: %v", err)
	}
	if tb, _ := e.Table("kv"); len(tb.secList()) != 0 || len(tb.Schema().Indexes) != 0 {
		t.Fatal("failed build left an index")
	}
	for i := 10; i < 20; i++ {
		e.Write([]WriteOp{{Table: "kv", Key: fmt.Sprintf("k%02d", i)}})
	}
	if err := e.CreateIndex("kv", schema.IndexDef{Name: "kv_u", Columns: []string{"n"}, Unique: true}); err != nil {
		t.Fatal(err)
	}
	_, err = e.Write([]WriteOp{{Table: "kv", Row: row("dup", 3)}})
	if !errors.As(err, &ue) || ue.Index != "kv_u" {
		t.Fatalf("duplicate insert: %v", err)
	}
	// updating a row to its own value is fine
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("k03", 3)}}); err != nil {
		t.Fatal(err)
	}
	// two rows with the same new value in one transaction
	_, err = e.Write([]WriteOp{{Table: "kv", Row: row("a", 100)}, {Table: "kv", Row: row("b", 100)}})
	if !errors.As(err, &ue) {
		t.Fatalf("duplicates inside one transaction: %v", err)
	}
	// swap: delete the holder and insert the value anew in one transaction
	if _, err := e.Write([]WriteOp{{Table: "kv", Key: "k04"}, {Table: "kv", Row: row("k04b", 4)}}); err != nil {
		t.Fatalf("replace holder: %v", err)
	}
	// NULLs do not conflict
	n1, n2 := row("n1", 0), row("n2", 0)
	n1[1].Null, n2[1].Null = true, true
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: n1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: n2}}); err != nil {
		t.Fatalf("second NULL: %v", err)
	}
	// concurrent inserts of one value: exactly one wins, in every round
	for round := 0; round < 50; round++ {
		v := int64(1000 + round)
		var wins, viol atomic.Int32
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				tx := e.Begin(SnapshotIsolation)
				if err := tx.Put("kv", row(fmt.Sprintf("r%d-%d", round, g), v)); err != nil {
					t.Error(err)
					return
				}
				_, err := tx.Commit()
				switch {
				case err == nil:
					wins.Add(1)
				case errors.As(err, new(*UniqueViolationError)):
					viol.Add(1)
				default:
					t.Errorf("commit: %v", err)
				}
			}(g)
		}
		wg.Wait()
		if wins.Load() != 1 || viol.Load() != 7 {
			t.Fatalf("round %d: %d winners, %d violations", round, wins.Load(), viol.Load())
		}
	}
	// concurrent churn: rows move between values; the unique value never has two holders
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 300; i++ {
				k := fmt.Sprintf("c%d", rnd.Intn(8))
				if rnd.Intn(3) == 0 {
					e.Write([]WriteOp{{Table: "kv", Key: k}})
					continue
				}
				e.Write([]WriteOp{{Table: "kv", Row: row(k, int64(5000+rnd.Intn(8)))}}) // unique violations are expected and ignored
			}
		}(g)
	}
	wg.Wait()
	e.waitInstalled(e.DurableLSN())
	seen := map[int64]string{}
	e.ScanSnapshot("kv", e.DurableLSN(), func(k string, r schema.Row, _ *Version) bool {
		if r[1].Null {
			return true
		}
		if o, dup := seen[r[1].I]; dup {
			t.Errorf("value %d held by %s and %s", r[1].I, o, k)
		}
		seen[r[1].I] = k
		return true
	})
}
