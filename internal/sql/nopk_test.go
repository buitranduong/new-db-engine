package sql

import (
	"fmt"
	"strings"
	"testing"
)

func cell(t *testing.T, s *Session, q string) string {
	t.Helper()
	r := run(t, s, q)
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("%s: %s", q, render(r))
	}
	return r.Rows[0][0].String()
}

func TestNoPrimaryKey(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE databasechangelog (ID VARCHAR(255) NOT NULL, AUTHOR VARCHAR(255) NOT NULL, ORDEREXECUTED INTEGER NOT NULL, MD5SUM VARCHAR(35))`)
	run(t, s, `INSERT INTO databasechangelog (ID, AUTHOR, ORDEREXECUTED) VALUES ('1', 'a', 1), ('1', 'a', 2)`) // duplicates are fine
	run(t, s, `INSERT INTO databasechangelog VALUES ('2', 'b', 3, 'x')`)
	r := run(t, s, `SELECT * FROM databasechangelog ORDER BY ORDEREXECUTED`)
	if len(r.Cols) != 4 || len(r.Rows) != 3 || len(r.Rows[0]) != 4 {
		t.Fatalf("select *: %d cols, %s", len(r.Cols), render(r))
	}
	if got := cell(t, s, `SELECT count(*) FROM databasechangelog`); got != "3" {
		t.Fatal(got)
	}
	if n := run(t, s, `UPDATE databasechangelog SET MD5SUM = 'm' WHERE ID = '1'`); n.Tag != "UPDATE 2" {
		t.Fatal(n.Tag)
	}
	if got := cell(t, s, `SELECT count(*) FROM databasechangelog WHERE MD5SUM = 'm'`); got != "2" {
		t.Fatal(got)
	}
	if n := run(t, s, `DELETE FROM databasechangelog WHERE ORDEREXECUTED = 2`); n.Tag != "DELETE 1" {
		t.Fatal(n.Tag)
	}
	if got := cell(t, s, `SELECT max(ORDEREXECUTED) FROM databasechangelog`); got != "3" {
		t.Fatal(got)
	}
	// catalogs: no primary key, no hidden column
	if got := cell(t, s, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'databasechangelog'`); got != "4" {
		t.Fatalf("information_schema.columns: %s", got)
	}
	if got := cell(t, s, `SELECT count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = 'databasechangelog' AND a.attnum > 0`); got != "4" {
		t.Fatalf("pg_attribute: %s", got)
	}
	if got := cell(t, s, `SELECT min(a.attnum) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = 'databasechangelog' AND a.attnum > 0`); got != "1" {
		t.Fatalf("attnum starts at %s", got)
	}
	if got := cell(t, s, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid WHERE c.relname = 'databasechangelog'`); got != "0" {
		t.Fatalf("pg_index: %s", got)
	}
	if got := cell(t, s, `SELECT count(*) FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid WHERE r.relname = 'databasechangelog' AND contype = 'p'`); got != "0" {
		t.Fatalf("pg_constraint: %s", got)
	}
	if got := cell(t, s, `SELECT count(*) FROM pg_class WHERE relname LIKE '%rowid%'`); got != "0" {
		t.Fatalf("hidden sequence listed: %s", got)
	}
	if got := cell(t, s, `SELECT count(*) FROM information_schema.table_constraints WHERE table_name = 'databasechangelog'`); got != "0" {
		t.Fatalf("table_constraints: %s", got)
	}
	// TRUNCATE and DROP
	run(t, s, `TRUNCATE databasechangelog`)
	run(t, s, `INSERT INTO databasechangelog VALUES ('9', 'z', 9, NULL)`)
	if got := cell(t, s, `SELECT count(*) FROM databasechangelog`); got != "1" {
		t.Fatal(got)
	}
	run(t, s, `DROP TABLE databasechangelog`)
	run(t, s, `CREATE TABLE databasechangelog (a int)`)
	run(t, s, `DROP TABLE databasechangelog`)
}

func TestCompositePrimaryKey(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ck (a INT, b TEXT, v TEXT, PRIMARY KEY (a, b))`)
	run(t, s, `INSERT INTO ck VALUES (1, 'x', 'p'), (1, 'y', 'q'), (2, 'x', 'r')`)
	if _, err := tryRun(s, `INSERT INTO ck VALUES (1, 'x', 'dup')`); code(err) != "23505" || !strings.Contains(err.Error(), "Key (a, b)=(1, x)") {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := tryRun(s, `INSERT INTO ck (a, v) VALUES (5, 'nb')`); code(err) != "23502" {
		t.Fatalf("null key column: %v", err)
	}
	r := run(t, s, `SELECT * FROM ck ORDER BY a, b`)
	if len(r.Cols) != 3 || len(r.Rows) != 3 {
		t.Fatal(render(r))
	}
	run(t, s, `INSERT INTO ck VALUES (1, 'x', 'new') ON CONFLICT (a, b) DO UPDATE SET v = excluded.v`)
	if got := cell(t, s, `SELECT v FROM ck WHERE a = 1 AND b = 'x'`); got != "new" {
		t.Fatal(got)
	}
	run(t, s, `INSERT INTO ck VALUES (1, 'x', 'z') ON CONFLICT (a, b) DO NOTHING`)
	if got := cell(t, s, `SELECT v FROM ck WHERE a = 1 AND b = 'x'`); got != "new" {
		t.Fatal(got)
	}
	// moving a key
	run(t, s, `UPDATE ck SET b = 'moved' WHERE a = 2`)
	if got := cell(t, s, `SELECT count(*) FROM ck WHERE b = 'moved' AND a = 2`); got != "1" {
		t.Fatal(got)
	}
	if _, err := tryRun(s, `UPDATE ck SET b = 'y' WHERE a = 1 AND b = 'x'`); code(err) != "23505" {
		t.Fatalf("key collision on update: %v", err)
	}
	run(t, s, `DELETE FROM ck WHERE a = 1 AND b = 'y'`)
	if got := cell(t, s, `SELECT count(*) FROM ck`); got != "2" {
		t.Fatal(got)
	}
	// catalogs
	if got := cell(t, s, `SELECT indkey FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid WHERE c.relname = 'ck' AND indisprimary`); got != "1 2" {
		t.Fatalf("indkey %q", got)
	}
	if got := cell(t, s, `SELECT conkey::text FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid WHERE r.relname = 'ck' AND contype = 'p'`); got != "{1,2}" {
		t.Fatalf("conkey %q", got)
	}
	if got := cell(t, s, `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid WHERE r.relname = 'ck' AND contype = 'p'`); got != `PRIMARY KEY (a, b)` {
		t.Fatalf("constraintdef %q", got)
	}
	if got := cell(t, s, `SELECT count(*) FROM information_schema.key_column_usage WHERE table_name = 'ck'`); got != "2" {
		t.Fatal(got)
	}
	// foreign key into the composite key
	run(t, s, `CREATE TABLE child (id INT PRIMARY KEY, ca INT, cb TEXT, FOREIGN KEY (ca, cb) REFERENCES ck (a, b))`)
	run(t, s, `INSERT INTO child VALUES (1, 1, 'x')`)
	if _, err := tryRun(s, `INSERT INTO child VALUES (2, 1, 'nope')`); code(err) != "23503" {
		t.Fatalf("fk insert: %v", err)
	}
	if _, err := tryRun(s, `DELETE FROM ck WHERE a = 1 AND b = 'x'`); code(err) != "23503" {
		t.Fatalf("fk delete: %v", err)
	}
	// drop of a key column and alter of it
	if _, err := tryRun(s, `ALTER TABLE ck DROP COLUMN a`); code(err) != codeFeature {
		t.Fatalf("drop key column: %v", err)
	}
	run(t, s, `ALTER TABLE ck ADD COLUMN w INT DEFAULT 3`)
	if got := cell(t, s, `SELECT w FROM ck WHERE a = 1 AND b = 'x'`); got != "3" {
		t.Fatal(got)
	}
}

func TestAddPrimaryKey(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ap (a INT NOT NULL, b INT NOT NULL, v TEXT)`)
	run(t, s, `ALTER TABLE ap ADD CONSTRAINT pk_ap PRIMARY KEY (a, b)`)
	run(t, s, `INSERT INTO ap VALUES (1, 1, 'x')`)
	if _, err := tryRun(s, `INSERT INTO ap VALUES (1, 1, 'y')`); code(err) != "23505" {
		t.Fatalf("%v", err)
	}
	if _, err := tryRun(s, `ALTER TABLE ap ADD PRIMARY KEY (v)`); code(err) != "42P16" {
		t.Fatalf("second pk: %v", err)
	}
	run(t, s, `CREATE TABLE ap1 (id BIGINT NOT NULL, v TEXT)`)
	run(t, s, `ALTER TABLE ap1 ADD PRIMARY KEY (id)`)
	run(t, s, `INSERT INTO ap1 VALUES (7, 'x')`)
	if _, err := tryRun(s, `INSERT INTO ap1 VALUES (7, 'y')`); code(err) != "23505" {
		t.Fatalf("%v", err)
	}
	if got := cell(t, s, `SELECT count(*) FROM pg_class WHERE relname LIKE '%rowid%'`); got != "0" {
		t.Fatal(got)
	}
	run(t, s, `CREATE TABLE ap2 (id BIGINT NOT NULL)`)
	run(t, s, `INSERT INTO ap2 VALUES (1)`)
	if _, err := tryRun(s, `ALTER TABLE ap2 ADD PRIMARY KEY (id)`); code(err) != codeFeature {
		t.Fatalf("pk on a table with rows: %v", err)
	}
	_ = fmt.Sprint()
}

// CDC: the hidden key column never appears; a table without a visible
// one-column key carries the whole old row (REPLICA IDENTITY FULL).
func TestNoPKChangeDataCapture(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE np (x INT, y TEXT)`)
	run(t, s, `CREATE TABLE ck (a INT, b INT, v TEXT, PRIMARY KEY (a, b))`)
	run(t, s, `SELECT pg_create_logical_replication_slot('td', 'test_decoding')`)
	run(t, s, `SELECT pg_create_logical_replication_slot('w2', 'wal2json')`)
	run(t, s, `INSERT INTO np VALUES (1, 'a'), (1, 'a')`)
	run(t, s, `UPDATE np SET y = 'b' WHERE x = 1`)
	run(t, s, `DELETE FROM np WHERE x = 1`)
	run(t, s, `INSERT INTO ck VALUES (1, 2, 'p')`)
	run(t, s, `UPDATE ck SET b = 3 WHERE a = 1`)
	run(t, s, `DELETE FROM ck WHERE a = 1`)
	got := strings.Join(data(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('td', NULL, NULL, 'include-xids', '0')`)), "\n")
	for _, bad := range []string{"__rowid", "__pk"} {
		if strings.Contains(got, bad) {
			t.Fatalf("the hidden column %s is in the stream:\n%s", bad, got)
		}
	}
	for _, want := range []string{
		"table public.np: INSERT: x[integer]:1 y[text]:'a'",
		"table public.np: UPDATE: old-key: x[integer]:1 y[text]:'a' new-tuple: x[integer]:1 y[text]:'b'",
		"table public.np: DELETE: x[integer]:1 y[text]:'b'",
		"table public.ck: DELETE: a[integer]:1 b[integer]:2 v[text]:'p'", // a key change is a delete and an insert
		"table public.ck: INSERT: a[integer]:1 b[integer]:3 v[text]:'p'",
		"table public.ck: DELETE: a[integer]:1 b[integer]:3 v[text]:'p'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	w := strings.Join(data(run(t, s, `SELECT * FROM pg_logical_slot_get_changes('w2', NULL, NULL, 'format-version', '2', 'include-pk', '1')`)), "\n")
	if strings.Contains(w, "__") {
		t.Fatalf("the hidden column is in wal2json:\n%s", w)
	}
	if !strings.Contains(w, `"pk":[{"name":"a","type":"integer"},{"name":"b","type":"integer"}]`) {
		t.Fatalf("wal2json pk of the composite key:\n%s", w)
	}
	if got := cell(t, s, `SELECT relreplident FROM pg_class WHERE relname = 'np'`); got != "f" {
		t.Fatalf("relreplident %s", got)
	}
}

// The hidden key must not leak through other features that read columns.
func TestNoPKWithOtherFeatures(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ev (host TEXT, n DOUBLE PRECISION) WITH (columnar = true)`)
	run(t, s, `INSERT INTO ev VALUES ('a', 1.5), ('a', 2.5), ('b', 4)`)
	if got := cell(t, s, `SELECT sum(n) FROM ev WHERE host = 'a'`); got != "4" {
		t.Fatalf("columnar aggregate: %s", got)
	}
	run(t, s, `CREATE VIEW evv AS SELECT * FROM ev WHERE host = 'a'`)
	if r := run(t, s, `SELECT * FROM evv`); len(r.Rows) != 2 || len(r.Cols) != 2 {
		t.Fatalf("view over a table without key: %d cols %s", len(r.Cols), render(r))
	}
	run(t, s, `DROP VIEW evv`)
	run(t, s, `ALTER TABLE ev RENAME TO ev2`)
	run(t, s, `INSERT INTO ev2 VALUES ('c', 9)`)
	run(t, s, `ALTER TABLE ev2 ADD COLUMN note TEXT DEFAULT 'n'`)
	if r := run(t, s, `SELECT * FROM ev2 WHERE host = 'c'`); render(r) != "c,9,n" {
		t.Fatalf("after rename and add column: %s", render(r))
	}
	if r := run(t, s, `SELECT a.host, b.host FROM ev2 a JOIN ev2 b ON a.host = b.host WHERE a.host = 'c'`); render(r) != "c,c" {
		t.Fatalf("self join: %s", render(r))
	}
	if r := run(t, s, `SELECT * FROM ev2 a NATURAL JOIN ev2 b WHERE a.host = 'c'`); len(r.Cols) != 3 {
		t.Fatalf("natural join must not join on the hidden key: %d cols", len(r.Cols))
	}
	// the hidden sequence goes with the table
	run(t, s, `DROP TABLE ev2`)
	if n := len(e.Sequences()); n != 0 {
		t.Fatalf("%d sequences left after DROP TABLE", n)
	}
	if _, err := tryRun(s, `CREATE TABLE bad (__rowid INT)`); code(err) != "42939" {
		t.Fatalf("reserved name: %v", err)
	}
	// text key columns of a composite key keep their order across the NUL byte
	run(t, s, `CREATE TABLE tk (a TEXT, b TEXT, PRIMARY KEY (a, b))`)
	run(t, s, `INSERT INTO tk VALUES ('a', 'z'), ('a' || chr(1), 'a'), ('ab', 'a')`)
	if got := cell(t, s, `SELECT count(*) FROM tk`); got != "3" {
		t.Fatal(got)
	}
}
