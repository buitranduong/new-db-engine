package core

import (
	"errors"
	"strings"
	"testing"

	"stask/internal/schema"
)

func countRows(t *testing.T, e *Engine, table string) int {
	t.Helper()
	n := 0
	if err := e.ScanSnapshot(table, e.DurableLSN(), func(string, schema.Row, *Version) bool { n++; return true }); err != nil {
		t.Fatal(err)
	}
	return n
}

func fill(t *testing.T, e *Engine, table string, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if _, err := e.Write([]WriteOp{{Table: table, Row: row(strings.Repeat("k", 1+i%3)+string(rune('a'+i%26))+itoa(i), int64(i))}}); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(i int) string {
	b := []byte{}
	for {
		b = append([]byte{byte('0' + i%10)}, b...)
		if i /= 10; i == 0 {
			return string(b)
		}
	}
}

func named(name string) schema.Table {
	s := kvTable()
	s.Name = name
	return s
}

// TestDropTruncate covers DROP TABLE and TRUNCATE across restarts (from
// the log only, then from a checkpoint), name reuse, writes prepared
// before the drop, GC of the dropped rows, and CDC of a truncate.
func TestDropTruncate(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := e.CreateTable(named(n)); err != nil {
			t.Fatal(err)
		}
		fill(t, e, n, 0, 300)
	}
	if _, err := e.CreateSlot("td", "test_decoding"); err != nil {
		t.Fatal(err)
	}
	a, _ := e.Table("a")
	b, _ := e.Table("b")
	tx := e.Begin(SnapshotIsolation)
	if err := tx.Put("b", row("late", 1)); err != nil {
		t.Fatal(err)
	}

	if err := e.DropTable("a"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropTable("a"); !errors.Is(err, ErrNoTable) {
		t.Fatalf("drop twice: %v", err)
	}
	if err := e.TruncateTable("b"); err != nil {
		t.Fatal(err)
	}
	// a write prepared against the replaced table must not land in it
	if _, err := tx.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("commit to truncated table: %v", err)
	}
	if _, err := e.Write([]WriteOp{{Table: "a", Row: row("x", 1)}}); !errors.Is(err, ErrNoTable) {
		t.Fatalf("write to dropped table: %v", err)
	}
	b2, err := e.Table("b")
	if err != nil {
		t.Fatal(err)
	}
	if b2 == b || b2.Schema().OID() != b.Schema().ID || b2.Schema().Replaces != b.Schema().ID {
		t.Fatalf("truncate: new table id=%d oid=%d replaces=%d, old id=%d", b2.Schema().ID, b2.Schema().OID(), b2.Schema().Replaces, b.Schema().ID)
	}
	if n := countRows(t, e, "b"); n != 0 {
		t.Fatalf("truncated table has %d rows", n)
	}
	fill(t, e, "b", 0, 10)
	// the name of a dropped table can be used again
	if _, err := e.CreateTable(named("a")); err != nil {
		t.Fatal(err)
	}
	fill(t, e, "a", 0, 5)
	if a2, _ := e.Table("a"); a2 == a {
		t.Fatal("recreated table is the dropped one")
	}

	// CDC: the truncate is a transaction of one change on the old table
	var td []string
	_, err = e.SlotChanges("td", ChangeOpts{}, func(c *Change) error {
		if c.Kind == ChangeTruncate {
			td = append(td, c.Table.Name)
			if c.Table.ID != b.Schema().ID {
				t.Errorf("truncate change on table %d, want %d", c.Table.ID, b.Schema().ID)
			}
		}
		return nil
	})
	if err != nil || len(td) != 1 || td[0] != "b" {
		t.Fatalf("cdc truncate: %v %v", td, err)
	}

	check := func(e *Engine, when string) {
		t.Helper()
		got := map[string]int{}
		for _, ts := range e.UserTables() {
			got[ts.Schema().Name] = countRows(t, e, ts.Schema().Name)
		}
		if len(got) != 3 || got["a"] != 5 || got["b"] != 10 || got["c"] != 300 {
			t.Fatalf("%s: tables %v", when, got)
		}
		if old := e.TableByID(a.Schema().ID); old == nil || old.droppedLSN.Load() == 0 {
			t.Fatalf("%s: dropped table not kept as dropped", when)
		}
		nb, _ := e.Table("b")
		if nb.Schema().OID() != b.Schema().ID {
			t.Fatalf("%s: truncated table oid %d, want %d", when, nb.Schema().OID(), b.Schema().ID)
		}
	}
	check(e, "before restart")
	e.Close()
	e = open(t, dir, 1<<20)
	check(e, "after replay")
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	check(e, "after checkpoint")

	// once no snapshot can read them, the dropped rows are released
	e.releaseDropped(e.DurableLSN())
	for _, id := range []uint32{a.Schema().ID, b.Schema().ID} {
		if n := e.TableByID(id).index().Len(); n != 0 {
			t.Fatalf("dropped table %d still indexes %d keys", id, n)
		}
	}
	if _, err := e.GCRound(); err != nil {
		t.Fatal(err)
	}
	check(e, "after gc")
}
