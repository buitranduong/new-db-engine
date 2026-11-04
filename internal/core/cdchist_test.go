package core

import (
	"fmt"
	"slices"
	"testing"

	"stask/internal/schema"
	"stask/internal/wal"
)

// published is how pgoutput decides (internal/sql): a publication of the
// change's time covers its table.
func published(c *Change, pub string) bool {
	for _, p := range c.Pubs {
		if p.Name == pub && p.Has(c.Table.Name) {
			return true
		}
	}
	return false
}

type histSeen struct {
	key   string
	table string
	pub   bool
	full  bool
	end   wal.Pos // end of its transaction
}

func histCollect(t *testing.T, e *Engine, slot string, o ChangeOpts) []histSeen {
	t.Helper()
	var out []histSeen
	_, err := e.SlotChanges(slot, o, func(c *Change) error {
		switch c.Kind {
		case ChangeInsert, ChangeUpdate, ChangeDelete, ChangeTruncate:
			out = append(out, histSeen{key: c.Key, table: c.Table.Name, pub: published(c, "p"), full: c.IdentityFull})
		case ChangeCommit:
			if len(out) > 0 {
				out[len(out)-1].end = c.End
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A consumer that lags across RENAME and ALTER PUBLICATION gets every
// change under the name and publications of the time it was written
// (docs/28 A0): before, the change written while kv was published arrived
// as unpublished (lost) and under the new name. The catalog is kept with
// the slot: decoding resumes right after a restart, and through
// ConfirmSlot as well as through SlotChanges.
func TestCDCHistoricCatalog(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1<<20)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	if err := e.CreatePublication(Publication{Name: "p", Tables: []string{"kv"}, Insert: true, Update: true, Delete: true}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"s1", "s2", "s3"} {
		if _, err := e.CreateSlot(s, "pgoutput"); err != nil {
			t.Fatal(err)
		}
	}
	w := func(k string, n int64, table string) {
		t.Helper()
		if _, err := e.Write([]WriteOp{{Table: table, Row: row(k, n)}}); err != nil {
			t.Fatal(err)
		}
	}
	w("a", 1, "kv")
	if err := e.SetReplicaIdentityFull("kv", true); err != nil {
		t.Fatal(err)
	}
	if err := e.AlterTable("kv", func(d *schema.Table) error { d.Name = "kv2"; return nil }); err != nil {
		t.Fatal(err)
	}
	w("c", 3, "kv2") // renamed, still published (the publication follows the table)
	if err := e.AlterPublication("p", func(p *Publication) error {
		p.Tables = slices.DeleteFunc(p.Tables, func(s string) bool { return s == "kv2" })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w("b", 2, "kv2")                                             // no longer published
	if _, err := e.CreateTable(kvTableNamed("kv")); err != nil { // a new table takes the old name
		t.Fatal(err)
	}
	w("d", 4, "kv") // the new kv is not in the publication

	want := []histSeen{
		{key: "a", table: "kv", pub: true},
		{key: "c", table: "kv2", pub: true, full: true},
		{key: "b", table: "kv2", pub: false, full: true},
		{key: "d", table: "kv", pub: false},
	}
	check := func(what string, got, want []histSeen) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d changes %+v, want %d", what, len(got), got, len(want))
		}
		for i := range want {
			g := got[i]
			g.end = wal.Pos{}
			if g != want[i] {
				t.Fatalf("%s: change %d is %+v, want %+v", what, i, g, want[i])
			}
		}
	}

	// s1: everything at once, without consuming
	all := histCollect(t, e, "s1", ChangeOpts{})
	check("s1", all, want)

	// s2: consume the first two transactions (across the RENAME), restart,
	// then the rest from the catalog saved with the slot
	check("s2 first", histCollect(t, e, "s2", ChangeOpts{MaxChanges: 2, Consume: true}), want[:2])
	// s3: the consumer confirms up to the end of the second transaction
	if _, err := e.ConfirmSlot("s3", all[1].end); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open(t, dir, 1<<20)
	defer e.Close()
	check("s2 after restart", histCollect(t, e, "s2", ChangeOpts{}), want[2:])
	check("s3 after confirm and restart", histCollect(t, e, "s3", ChangeOpts{}), want[2:])
	for _, si := range e.Slots() {
		if si.Name != "s1" && (si.Catalog == nil || si.Catalog.Names[e.mustOID(t, "kv2")] != "kv2") {
			t.Fatalf("slot %s saved catalog %+v", si.Name, si.Catalog)
		}
	}
}

// TRUNCATE gives the table a new ID with the same relation OID: the table
// stays in the publication for the changes after it.
func TestCDCHistoricTruncate(t *testing.T) {
	e := open(t, t.TempDir(), 1<<20)
	defer e.Close()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	if err := e.CreatePublication(Publication{Name: "p", Tables: []string{"kv"}, Insert: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateSlot("s", "pgoutput"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("a", 1)}}); err != nil {
		t.Fatal(err)
	}
	if err := e.TruncateTable("kv"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]WriteOp{{Table: "kv", Row: row("b", 2)}}); err != nil {
		t.Fatal(err)
	}
	got := histCollect(t, e, "s", ChangeOpts{})
	var s []string
	for _, g := range got {
		s = append(s, fmt.Sprintf("%s:%s:%v", g.key, g.table, g.pub))
	}
	if fmt.Sprint(s) != "[a:kv:true :kv:true b:kv:true]" {
		t.Fatalf("changes %v", s)
	}
}

func kvTableNamed(name string) schema.Table {
	s := kvTable()
	s.Name = name
	return s
}

func (e *Engine) mustOID(t *testing.T, name string) uint32 {
	t.Helper()
	tb, err := e.Table(name)
	if err != nil {
		t.Fatal(err)
	}
	return tb.Schema().OID()
}
