package core

import (
	"testing"
)

// The first table of an engine was table 0, and the DDL record of its
// TRUNCATE (Replaces 0, omitted) read as a CREATE: after a restart the
// truncated table was back, rows included, next to the new one, and CDC
// did not deliver the TRUNCATE (docs/14 ZEROID).
func TestTruncateFirstTable(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	if id := mustTable(t, e, "kv").Schema().ID; id == 0 {
		t.Fatal("the first table has ID 0")
	}
	if _, err := e.CreateSlot("s", "wal2json"); err != nil {
		t.Fatal(err)
	}
	truncateAndRestart(t, e, dir, func(e *Engine) {
		n := 0
		if _, err := e.SlotChanges("s", ChangeOpts{}, func(c *Change) error {
			if c.Kind == ChangeTruncate && c.Table.Name == "kv" {
				n++
			}
			return nil
		}); err != nil || n != 1 {
			t.Fatalf("truncate changes %d, %v", n, err)
		}
	})
}

// A log written before IDs started at 1 holds a table 0 and its TRUNCATE
// as a record without Replaces: replay recognizes it.
func TestTruncateTableZeroLegacyLog(t *testing.T) {
	dir := t.TempDir()
	firstTableID = 0 // reset once the engines are closed (their committers read it)
	defer func() { firstTableID = 1 }()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	if id := mustTable(t, e, "kv").Schema().ID; id != 0 {
		t.Fatalf("legacy first table has ID %d", id)
	}
	truncateAndRestart(t, e, dir, nil)
}

func truncateAndRestart(t *testing.T, e *Engine, dir string, after func(*Engine)) {
	t.Helper()
	for _, k := range []string{"a", "b", "c"} {
		if _, err := e.Write([]WriteOp{{Table: "kv", Row: row(k, 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.TruncateTable("kv"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("z", 2)}}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	if ts := e.UserTables(); len(ts) != 1 || ts[0].Schema().Name != "kv" {
		var names []string
		for _, s := range ts {
			names = append(names, s.Schema().Name)
		}
		t.Fatalf("tables after restart: %v", names)
	}
	if n := countRows(t, e, "kv"); n != 1 {
		t.Fatalf("rows after restart: %d, want 1", n)
	}
	if after != nil {
		after(e)
	}
}

func mustTable(t *testing.T, e *Engine, name string) *TableState {
	t.Helper()
	tb, err := e.Table(name)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}
