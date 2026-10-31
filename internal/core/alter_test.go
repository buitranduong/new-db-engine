package core

import (
	"errors"
	"testing"

	"stask/internal/schema"
)

// TestAlterTable covers ADD COLUMN without rewriting rows (old rows read
// the missing value), RENAME TO, writes encoded before the ALTER, and the
// schema across restarts from the log and from a checkpoint.
func TestAlterTable(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	fill(t, e, "kv", 0, 200)
	tx := e.Begin(SnapshotIsolation)
	if err := tx.Put("kv", row("late", 1)); err != nil {
		t.Fatal(err)
	}
	if err := e.AlterTable("kv", func(d *schema.Table) error {
		d.Columns = append(d.Columns, schema.Column{Name: "q", Type: "int", Missing: &schema.Value{I: 7}},
			schema.Column{Name: "w", Type: "string"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// encoded with the old schema: the commit re-encodes it (alterwrites.go);
	// a row of the old width written outside a transaction is refused (an
	// autocommit retry rebuilds it)
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit of a row encoded before the ALTER: %v", err)
	}
	if r, ok, err := e.Get("kv", "late"); err != nil || ok == nil || len(r) != 5 || r[3].I != 7 {
		t.Fatalf("row re-encoded at commit: %v %v %v", r, ok, err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("old", 1)}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("write of an old-width row: %v", err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: append(row("new", 1), schema.Value{I: 1}, schema.Value{S: "w"})}}); err != nil {
		t.Fatal(err)
	}
	if err := e.AlterTable("kv", func(d *schema.Table) error { d.Name = "kv2"; return nil }); err != nil {
		t.Fatal(err)
	}
	check := func(e *Engine, when string) {
		t.Helper()
		if _, err := e.Table("kv"); !errors.Is(err, ErrNoTable) {
			t.Fatalf("%s: old name still resolves: %v", when, err)
		}
		r, _, err := e.Get("kv2", fillKey(5))
		if err != nil || len(r) != 5 || r[3].I != 7 || !r[4].Null {
			t.Fatalf("%s: old row %v %v", when, r, err)
		}
		r, _, err = e.Get("kv2", "new")
		if err != nil || len(r) != 5 || r[3].I != 1 || r[4].S != "w" {
			t.Fatalf("%s: new row %v %v", when, r, err)
		}
		n := 0
		e.ScanSnapshot("kv2", e.DurableLSN(), func(_ string, r schema.Row, _ *Version) bool {
			if len(r) != 5 || r[3].Null {
				t.Errorf("%s: scanned row %v", when, r)
			}
			n++
			return true
		})
		if n != 202 { // 200 + late (re-encoded at commit) + new
			t.Fatalf("%s: %d rows", when, n)
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
}

// fillKey is the key fill gives row i.
func fillKey(i int) string {
	k := ""
	for j := 0; j < 1+i%3; j++ {
		k += "k"
	}
	return k + string(rune('a'+i%26)) + itoa(i)
}

// TestDropColumn: rows stored before DROP COLUMN are read past the dropped
// value, also with a column added before and one after the drop, in
// transactions that span the ALTER, and across replay and checkpoint.
func TestDropColumn(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	fill(t, e, "kv", 0, 100) // id, n, s
	alter := func(f func(d *schema.Table)) {
		t.Helper()
		if err := e.AlterTable("kv", func(d *schema.Table) error { f(d); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	alter(func(d *schema.Table) {
		d.Columns = append(d.Columns, schema.Column{Name: "a", Type: "int", Missing: &schema.Value{I: 3}})
	})
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: append(row("mid", 1), schema.Value{I: 4})}}); err != nil {
		t.Fatal(err)
	}
	base, _ := e.Table("kv")
	tx := e.Begin(SnapshotIsolation)
	if err := tx.Put("kv", append(row("late", 2), schema.Value{I: 5})); err != nil {
		t.Fatal(err)
	}
	alter(func(d *schema.Table) { d.Columns = append(d.Columns[:1], d.Columns[2:]...) }) // drop n
	if r, ok, err := tx.Get("kv", "late"); err != nil || !ok || len(r) != 3 || r[2].I != 5 || r[1].S == "" {
		t.Fatalf("own write after the drop: %v %v %v", r, ok, err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit across the drop: %v", err)
	}
	if r, ok, err := e.Get("kv", "late"); err != nil || ok == nil || len(r) != 3 || r[2].I != 5 {
		t.Fatalf("row re-encoded at commit across the drop: %v %v %v", r, ok, err)
	}
	// a row of the width before the drop, and one built for that schema
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: append(row("x", 1), schema.Value{I: 4})}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("old-width row: %v", err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "y"}, {S: "s"}, {I: 1}}, Schema: base.Schema()}}); err != nil {
		t.Fatal(err) // base.Schema() is the current one: TableState follows the ALTER
	}
	alter(func(d *schema.Table) { d.Columns = append(d.Columns, schema.Column{Name: "n", Type: "string"}) }) // a new n
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: schema.Row{{S: "new"}, {S: "s"}, {I: 9}, {S: "nn"}}}}); err != nil {
		t.Fatal(err)
	}
	check := func(e *Engine, when string) {
		t.Helper()
		ts, _ := e.Table("kv")
		if sch := ts.Schema(); len(sch.Columns) != 4 || sch.Columns[1].Name != "s" || len(sch.Dropped) != 1 || sch.Dropped[0].Name != "n" {
			t.Fatalf("%s: schema %+v", when, sch)
		}
		want := map[string][2]any{fillKey(7): {int64(3), true}, "mid": {int64(4), true}, "y": {int64(1), true}, "new": {int64(9), false}}
		for k, w := range want {
			r, _, err := e.Get("kv", k)
			if err != nil || len(r) != 4 || r[2].I != w[0].(int64) || r[3].Null != w[1].(bool) || r[1].S == "" {
				t.Fatalf("%s: row %s = %v %v", when, k, r, err)
			}
		}
		n := 0
		e.ScanSnapshot("kv", e.DurableLSN(), func(k string, r schema.Row, _ *Version) bool {
			if len(r) != 4 || r[2].Null || r[1].S == "" {
				t.Errorf("%s: scanned %s %v", when, k, r)
			}
			n++
			return true
		})
		if n != 104 { // 100 + mid + late (committed across the drop) + y + new
			t.Fatalf("%s: %d rows", when, n)
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
}
