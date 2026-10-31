package sql

import (
	"encoding/json"
	"testing"

	"stask/internal/core"
	"stask/internal/schema"
)

// COLLATE in column and index definitions, CREATE COLLATION, across a restart.
func TestCollationDDLRestart(t *testing.T) {
	if err := SetDefaultCollation("en_US.utf8"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetDefaultCollation("C") })
	dir := t.TempDir()
	e := plfOpen(t, dir)
	s := NewSession(e)
	mustq(t, s, `CREATE COLLATION bytes_only (provider = libc, locale = 'C')`)
	mustq(t, s, `CREATE COLLATION loc FROM "en_US"`)
	wantErr(t, s, `CREATE COLLATION bytes_only (locale = 'C')`, "42710")
	wantErr(t, s, `CREATE COLLATION nd (provider = icu, locale = 'und', deterministic = false)`, "0A000")
	wantErr(t, s, `CREATE COLLATION bad (locale = 'zz_nowhere')`, "22023")
	mustq(t, s, `CREATE TABLE ct (id int PRIMARY KEY, a text COLLATE "C", b varchar(10) COLLATE loc, c text COLLATE bytes_only, d text)`)
	wantErr(t, s, `CREATE TABLE ct2 (x text COLLATE nonesuch)`, "42704")
	mustq(t, s, `INSERT INTO ct VALUES (1,'b','b','b','b'),(2,'A','A','A','A'),(3,'a','a','a','a'),(4,'B','B','B','B')`)
	ddlWantRows(t, s, `SELECT a FROM ct ORDER BY a`, "A;B;a;b")             // "C": bytes
	ddlWantRows(t, s, `SELECT b FROM ct ORDER BY b`, "a;A;b;B")             // en_US
	ddlWantRows(t, s, `SELECT c FROM ct ORDER BY c DESC`, "b;a;B;A")        // a user collation made from C
	ddlWantRows(t, s, `SELECT d FROM ct ORDER BY d`, "a;A;b;B")             // the database default is en_US
	ddlWantRows(t, s, `SELECT a FROM ct WHERE a > 'a' ORDER BY a`, "b")     // bytes: 'b' > 'a' only
	ddlWantRows(t, s, `SELECT b FROM ct WHERE b > 'a' ORDER BY b`, "A;b;B") // en_US: A > a
	ddlWantRows(t, s, `SELECT a FROM ct WHERE a BETWEEN 'A' AND 'a' ORDER BY a`, "A;B;a")
	ddlWantRows(t, s, `SELECT a FROM ct ORDER BY a COLLATE "en_US"`, "a;A;b;B") // an explicit COLLATE wins
	mustq(t, s, `CREATE INDEX ct_a ON ct (a COLLATE "C", id)`)
	mustq(t, s, `CREATE INDEX ct_lb ON ct (lower(b) COLLATE "C")`)
	ddlWantRows(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'ct_a'`, `CREATE INDEX ct_a ON public.ct USING btree (a COLLATE "C", id)`)
	ddlWantRows(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'ct_lb'`, `CREATE INDEX ct_lb ON public.ct USING btree (lower((b)::text) COLLATE "C")`)
	// an index is byte-ordered: ORDER BY ... LIMIT may read it only where the collation in effect is bytewise
	mustq(t, s, `CREATE INDEX ct_b ON ct (b)`)
	mustq(t, s, `CREATE INDEX ct_d ON ct (d)`)
	ddlWantRows(t, s, `SELECT a FROM ct ORDER BY a LIMIT 2`, "A;B")
	ddlWantRows(t, s, `SELECT b FROM ct ORDER BY b LIMIT 2`, "a;A")
	ddlWantRows(t, s, `SELECT d FROM ct ORDER BY d LIMIT 2`, "a;A")
	wantErr(t, s, `CREATE INDEX ct_bad ON ct (a COLLATE nonesuch)`, "42704")
	ddlWantRows(t, s, `SELECT column_name, collation_name FROM information_schema.columns WHERE table_name = 'ct' ORDER BY ordinal_position`,
		"id,NULL;a,C;b,loc;c,bytes_only;d,NULL")
	ddlWantRows(t, s, `SELECT collname FROM pg_collation WHERE collname IN ('bytes_only','loc') ORDER BY 1`, "bytes_only;loc")
	wantErr(t, s, `DROP COLLATION loc`, "2BP01")
	mustq(t, s, `ALTER TABLE ct ALTER COLUMN d TYPE text COLLATE "C"`)
	ddlWantRows(t, s, `SELECT d FROM ct ORDER BY d`, "A;B;a;b")
	mustq(t, s, `ALTER TABLE ct ADD COLUMN e text COLLATE "C"`)
	// the JSON of the schema carries the new fields, and old schemas (without them) read as before
	tbl, _ := e.UserTable("ct")
	if c, _ := tbl.Schema().Col("b"); tbl.Schema().Columns[c].Collation != "loc" {
		t.Fatalf("column collation not kept: %+v", tbl.Schema().Columns[c])
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = plfOpen(t, dir)
	defer e.Close()
	s = NewSession(e)
	ddlWantRows(t, s, `SELECT a FROM ct ORDER BY a`, "A;B;a;b")
	ddlWantRows(t, s, `SELECT b FROM ct ORDER BY b`, "a;A;b;B")
	ddlWantRows(t, s, `SELECT c FROM ct ORDER BY c DESC`, "b;a;B;A")
	ddlWantRows(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = 'ct_a'`, `CREATE INDEX ct_a ON public.ct USING btree (a COLLATE "C", id)`)
	mustq(t, s, `DROP TABLE ct`)
	mustq(t, s, `DROP COLLATION loc`)
	mustq(t, s, `DROP COLLATION IF EXISTS loc`)
	ddlWantRows(t, s, `SELECT count(*) FROM pg_collation WHERE collname IN ('bytes_only','loc')`, "1")
	var _ schema.Column
	var _ core.Obj
}

// a table stored before the collation fields existed reads without a collation
func TestCollationFieldsAdditive(t *testing.T) {
	var c schema.Column
	if err := json.Unmarshal([]byte(`{"name":"a","type":"text"}`), &c); err != nil || c.Collation != "" {
		t.Fatalf("old column: %+v %v", c, err)
	}
}
