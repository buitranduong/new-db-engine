package sql

import (
	"testing"
	"time"
)

// Regression tests for the "wrong results or dangerous behaviour" tier of the first acceptance
// suite run (docs/25 §6 S1-S8, T1). Expected values are what PostgreSQL 16 returns.

// S1: sum(bigint) is numeric and never wraps.
func TestSuiteFixSumBigintOverflow(t *testing.T) {
	s := NewSession(newEngine(t))
	ddlWantRows(t, s, `SELECT sum(x) FROM (VALUES (9223372036854775807::bigint),(1)) t(x)`, "9223372036854775808")
	ddlWantRows(t, s, `SELECT sum(x), avg(x) FROM (VALUES (9223372036854775807::bigint),(9223372036854775807::bigint)) t(x)`,
		"18446744073709551614,9223372036854775807")
	ddlWantRows(t, s, `SELECT sum(x) FROM (VALUES (-9223372036854775807::bigint),(-9223372036854775807::bigint),(5)) t(x)`,
		"-18446744073709551609")
	ddlWantRows(t, s, `SELECT sum(x) FROM (VALUES (1::bigint),(2)) t(x)`, "3")
	mustq(t, s, `CREATE TABLE big (g int, x bigint)`)
	mustq(t, s, `INSERT INTO big VALUES (1, 9223372036854775807), (1, 9223372036854775807), (2, 7)`)
	ddlWantRows(t, s, `SELECT g, sum(x) FROM big GROUP BY g ORDER BY g`, "1,18446744073709551614;2,7")
	ddlWantRows(t, s, `SELECT sum(x) OVER (ORDER BY g, x) FROM big ORDER BY g, x`, "18446744073709551614;18446744073709551614;18446744073709551621") // the two equal rows are peers
}

// S2: a VALUES column mixing float8, numeric and integers has one type for all rows.
func TestSuiteFixValuesMixedNumeric(t *testing.T) {
	s := NewSession(newEngine(t))
	ddlWantRows(t, s, `SELECT sum(x), avg(x) FROM (VALUES (0.1::float8),(0.2),(0.3)) t(x)`, "0.6000000000000001,0.20000000000000004")
	ddlWantRows(t, s, `SELECT x FROM (VALUES (1),(2.5),(3)) t(x)`, "1;2.5;3")
	ddlWantRows(t, s, `SELECT pg_typeof(x) FROM (VALUES (1),(2.5),(3)) t(x) LIMIT 1`, "numeric")
	ddlWantRows(t, s, `SELECT sum(x) FROM (VALUES (1),(2.5),(3)) t(x)`, "6.5")
	ddlWantRows(t, s, `SELECT pg_typeof(x) FROM (VALUES (1),(0.5::float8)) t(x) LIMIT 1`, "double precision")
	ddlWantRows(t, s, `SELECT sum(x) FROM (VALUES (1),(NULL),(2.5::float8)) t(x)`, "3.5")
}

// S3: after an error inside a transaction block COMMIT does not commit (tag ROLLBACK).
func TestSuiteFixFailedTxCommitRollsBack(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE ft (a int)`)
	mustq(t, s, `BEGIN`)
	mustq(t, s, `SAVEPOINT s`)
	mustq(t, s, `RELEASE s`)
	wantErr(t, s, `ROLLBACK TO s`, "3B001")
	if s.TxStatus() != 'E' {
		t.Fatalf("transaction status after the error = %c, want E", s.TxStatus())
	}
	wantErr(t, s, `INSERT INTO ft VALUES (1)`, "25P02")
	if r := mustq(t, s, `COMMIT`); r.Tag != "ROLLBACK" {
		t.Fatalf("COMMIT of a failed transaction returned tag %q, want ROLLBACK", r.Tag)
	}
	ddlWantRows(t, s, `SELECT count(*) FROM ft`, "0")

	// the same through RELEASE of an unknown savepoint, and a data change before the error
	mustq(t, s, `BEGIN`)
	mustq(t, s, `INSERT INTO ft VALUES (2)`)
	wantErr(t, s, `RELEASE nosuch`, "3B001")
	if r := mustq(t, s, `COMMIT`); r.Tag != "ROLLBACK" {
		t.Fatalf("tag %q, want ROLLBACK", r.Tag)
	}
	ddlWantRows(t, s, `SELECT count(*) FROM ft`, "0")

	// ROLLBACK TO an existing savepoint still revives the transaction
	mustq(t, s, `BEGIN`)
	mustq(t, s, `SAVEPOINT a`)
	wantErr(t, s, `SELECT 1/0`, "22012")
	mustq(t, s, `ROLLBACK TO a`)
	mustq(t, s, `INSERT INTO ft VALUES (3)`)
	if r := mustq(t, s, `COMMIT`); r.Tag != "COMMIT" {
		t.Fatalf("tag %q, want COMMIT", r.Tag)
	}
	ddlWantRows(t, s, `SELECT a FROM ft`, "3")
}

// S4: LIMIT 0 does not run the FROM list, LIMIT n over a series asks for OFFSET+n rows only.
func TestSuiteFixGenerateSeriesLimit(t *testing.T) {
	s := NewSession(newEngine(t))
	start := time.Now()
	ddlWantRows(t, s, `SELECT * FROM generate_series(1, 1000000000000) LIMIT 0`, "")
	ddlWantRows(t, s, `SELECT * FROM generate_series(1, 1000000000000) LIMIT 3`, "1;2;3")
	ddlWantRows(t, s, `SELECT * FROM generate_series(1, 1000000000000) LIMIT 2 OFFSET 5`, "6;7")
	ddlWantRows(t, s, `SELECT generate_series * 2 AS d FROM generate_series(10, 1000000000000) LIMIT 2`, "20;22")
	ddlWantRows(t, s, `SELECT count(*) FROM (SELECT * FROM generate_series(1, 1000000000000) LIMIT 4) q`, "4")
	ddlWantRows(t, s, `SELECT * FROM generate_series(1, 3) LIMIT 10`, "1;2;3")
	ddlWantRows(t, s, `SELECT * FROM generate_series(5, 1, -2) LIMIT 2`, "5;3")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("LIMIT over a huge series took %v", d)
	}
}

// S5: tables of a non-public schema are referred to by their bare name or alias in UPDATE .. FROM,
// DELETE .. USING, INSERT .. ON CONFLICT and MERGE.
func TestSuiteFixSchemaQualifiedAliases(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE SCHEMA s5`)
	mustq(t, s, `SET search_path = s5`)
	mustq(t, s, `CREATE TABLE t (id int PRIMARY KEY, v int, hits int DEFAULT 0)`)
	mustq(t, s, `INSERT INTO t VALUES (1, 1, 0), (2, 2, 0)`)
	ddlWantRows(t, s, `UPDATE t SET v = s.v FROM (VALUES (1, 10)) AS s(id, v) WHERE t.id = s.id RETURNING t.id, t.v`, "1,10")
	ddlWantRows(t, s, `INSERT INTO t VALUES (1, 5, 0) ON CONFLICT (id) DO UPDATE SET hits = t.hits + 1 RETURNING t.hits, hits`, "1,1")
	ddlWantRows(t, s, `INSERT INTO t AS u VALUES (1, 5, 0) ON CONFLICT (id) DO UPDATE SET hits = u.hits + 1 RETURNING u.hits`, "2")
	mustq(t, s, `CREATE TABLE tgt (id int, n int)`)
	mustq(t, s, `INSERT INTO tgt VALUES (1, 1)`)
	mustq(t, s, `MERGE INTO tgt USING (VALUES (1, 2)) s(id, n) ON tgt.id = s.id WHEN MATCHED THEN UPDATE SET n = tgt.n + s.n`)
	ddlWantRows(t, s, `SELECT n FROM tgt`, "3")
	ddlWantRows(t, s, `DELETE FROM t USING tgt WHERE t.id = tgt.id RETURNING t.id`, "1")
	ddlWantRows(t, s, `UPDATE s5.t SET v = 7 WHERE t.id = 2 RETURNING t.v`, "7")
	// a prepared statement with parameters over the aliased table
	r, err := qs(t, s, `PREPARE p5(int) AS UPDATE t SET v = $1 WHERE t.id = 2 RETURNING t.v`)
	if err != nil {
		t.Fatalf("prepare: %v (%v)", err, r)
	}
	ddlWantRows(t, s, `EXECUTE p5(9)`, "9")
	// the public schema keeps working
	mustq(t, s, `CREATE TABLE public.pp (id int PRIMARY KEY, h int)`)
	ddlWantRows(t, s, `INSERT INTO public.pp AS z VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET h = z.h + 1 RETURNING z.h`, "1")
}

// S6: catalog output names objects relative to the search path, without the internal schema prefix.
func TestSuiteFixCatalogSchemaNames(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE SCHEMA s6`)
	mustq(t, s, `SET search_path = s6`)
	mustq(t, s, `CREATE TYPE color AS ENUM ('r', 'g')`)
	mustq(t, s, `CREATE TABLE parent (id serial PRIMARY KEY, code text UNIQUE, c color)`)
	mustq(t, s, `CREATE TABLE child (id serial PRIMARY KEY, pid int NOT NULL REFERENCES parent(id) ON DELETE CASCADE, note text)`)
	mustq(t, s, `CREATE INDEX child_note ON child (note)`)

	ddlWantRows(t, s, `SELECT 's6.parent'::regclass::text, 'parent'::regclass::text, to_regclass('s6.child')::text`, "parent,parent,child")
	ddlWantRows(t, s, `SELECT pg_get_serial_sequence('s6.child', 'id')`, "s6.child_id_seq")
	ddlWantRows(t, s, `SELECT indexrelid::regclass::text FROM pg_index WHERE indrelid = 's6.child'::regclass ORDER BY 1`, "child_note;child_pkey")
	ddlWantRows(t, s, `SELECT pg_get_indexdef(indexrelid) FROM pg_index WHERE indrelid = 's6.child'::regclass AND indisprimary`,
		"CREATE UNIQUE INDEX child_pkey ON s6.child USING btree (id)")
	ddlWantRows(t, s, `SELECT pg_get_indexdef(indexrelid, 0, true) FROM pg_index WHERE indrelid = 's6.parent'::regclass AND indisprimary`,
		"CREATE UNIQUE INDEX parent_pkey ON parent USING btree (id)")
	ddlWantRows(t, s, `SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 's6.child'::regclass AND contype = 'f'`,
		"child_pid_fkey,FOREIGN KEY (pid) REFERENCES parent(id) ON DELETE CASCADE")
	ddlWantRows(t, s, `SELECT relname FROM pg_class WHERE relname LIKE '%code_key'`, "parent_code_key")
	ddlWantRows(t, s, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a WHERE a.attrelid = 's6.parent'::regclass AND a.attname = 'c'`, "color")
	ddlWantRows(t, s, `SELECT column_default FROM information_schema.columns WHERE table_schema = 's6' AND table_name = 'child' AND column_name = 'id'`,
		"nextval('child_id_seq'::regclass)")
	// from another schema on the path the names are qualified
	mustq(t, s, `SET search_path = public`)
	ddlWantRows(t, s, `SELECT 's6.parent'::regclass::text`, "s6.parent")
	ddlWantRows(t, s, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 's6.child'::regclass AND contype = 'f'`,
		"FOREIGN KEY (pid) REFERENCES s6.parent(id) ON DELETE CASCADE")
	ddlWantRows(t, s, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a WHERE a.attrelid = 's6.parent'::regclass AND a.attname = 'c'`, "s6.color")
}

// S7: DROP SCHEMA .. CASCADE removes the schema although it holds partitioned tables.
func TestSuiteFixDropSchemaWithPartitions(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE SCHEMA s7`)
	mustq(t, s, `SET search_path = s7`)
	mustq(t, s, `CREATE TABLE pr (id int, d date) PARTITION BY RANGE (d)`)
	mustq(t, s, `CREATE TABLE pr_2024q1 PARTITION OF pr FOR VALUES FROM ('2024-01-01') TO ('2024-04-01')`)
	mustq(t, s, `CREATE TABLE pr_2024q2 PARTITION OF pr FOR VALUES FROM ('2024-04-01') TO ('2024-07-01')`)
	mustq(t, s, `INSERT INTO pr VALUES (1, '2024-02-01'), (2, '2024-05-01')`)
	mustq(t, s, `CREATE TABLE plain (id int PRIMARY KEY)`)
	mustq(t, s, `DROP SCHEMA s7 CASCADE`)
	mustq(t, s, `SET search_path = public`)
	ddlWantRows(t, s, `SELECT count(*) FROM information_schema.schemata WHERE schema_name = 's7'`, "0")
	ddlWantRows(t, s, `SELECT count(*) FROM pg_class WHERE relname LIKE 'pr%'`, "0")
	// the schema can be created again and used
	mustq(t, s, `CREATE SCHEMA s7`)
	mustq(t, s, `CREATE TABLE s7.pr (id int, d date) PARTITION BY RANGE (d)`)
	mustq(t, s, `DROP SCHEMA s7 CASCADE`)
}

// S8: timestamp - timestamp is an interval also next to window functions.
func TestSuiteFixTimestampDiffWindow(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE w8 (ts timestamp)`)
	mustq(t, s, `INSERT INTO w8 VALUES ('2024-01-01 10:00'), ('2024-01-01 10:30'), ('2024-01-02 09:00')`)
	ddlWantRows(t, s, `SELECT pg_typeof(ts - lag(ts) OVER (ORDER BY ts)) FROM w8 LIMIT 1`, "interval")
	res := mustq(t, s, `SELECT ts - lag(ts) OVER (ORDER BY ts) AS d FROM w8 ORDER BY ts`)
	if c := res.Cols[0]; c.Kind != KInterval || c.OID != 0 && c.OID != 1186 {
		t.Fatalf("column kind %v oid %d, want interval", c.Kind, c.OID)
	}
	ddlWantRows(t, s, `SELECT ts - min(ts) OVER () FROM w8 ORDER BY ts`, "00:00:00;00:30:00;23:00:00")
	ddlWantRows(t, s, `SELECT pg_typeof(ts + interval '1 day') FROM w8 LIMIT 1`, "timestamp without time zone")
}

// T1: integer VALUES, set operations, GROUPING(), mode() and percentile_disc() are int4 as in
// PostgreSQL (pgjdbc getObject gives Integer, not Long).
func TestSuiteFixInt4Resolution(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE i4 (g int, a int, b bigint)`)
	mustq(t, s, `INSERT INTO i4 VALUES (1, 5, 5), (1, 5, 6), (2, 7, 7)`)
	cases := []struct{ q, want string }{
		{`SELECT x FROM (VALUES (1), (2)) t(x)`, "23/-1"},
		{`SELECT x FROM (VALUES (1), (NULL)) a(x) UNION SELECT NULL`, "23/-1"},
		{`SELECT x FROM (VALUES (1), (NULL)) a(x) INTERSECT SELECT x FROM (VALUES (NULL), (2)) b(x)`, "23/-1"},
		{`SELECT 1 UNION ALL SELECT 2`, "23/-1"},
		{`SELECT 1 UNION SELECT 2147483648`, "20/0"},
		{`SELECT a FROM i4 UNION SELECT b FROM i4`, "20/0"},
		{`SELECT NULL UNION ALL SELECT 1`, "23/-1"},
		{`SELECT g, grouping(g) FROM i4 GROUP BY ROLLUP (g)`, "23/-1 23/-1"},
		{`SELECT mode() WITHIN GROUP (ORDER BY a) FROM i4`, "23/-1"},
		{`SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY a) FROM i4`, "23/-1"},
		{`SELECT mode() WITHIN GROUP (ORDER BY b) FROM i4`, "20/0"},
	}
	for _, c := range cases {
		if got := colOIDs(t, s, c.q); got != c.want {
			t.Errorf("%s\n got: %s\nwant: %s", c.q, got, c.want)
		}
	}
	ddlWantRows(t, s, `SELECT g, grouping(g) FROM i4 GROUP BY ROLLUP (g) ORDER BY 2, 1`, "1,0;2,0;NULL,1")
}
