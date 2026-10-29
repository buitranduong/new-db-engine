package sql

import (
	"strings"
	"testing"
)

func TestSQLDropTruncate(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE t1 (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `CREATE TABLE t2 (id BIGINT PRIMARY KEY, v TEXT) WITH (columnar = true)`)
	run(t, s, `CREATE TABLE t3 (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO t1 VALUES (1, 'a'), (2, 'b')`)
	run(t, s, `INSERT INTO t2 VALUES (1, 'a'), (2, 'b'), (3, 'c')`)
	run(t, s, `INSERT INTO t3 VALUES (1, 'a')`)
	run(t, s, `CREATE PUBLICATION p FOR TABLE t1, t2`)
	run(t, s, `ALTER TABLE t1 REPLICA IDENTITY FULL`)
	run(t, s, `SELECT pg_create_logical_replication_slot('td', 'test_decoding')`)
	run(t, s, `SELECT pg_create_logical_replication_slot('w2', 'wal2json')`)

	// every table must exist before any is dropped
	if _, err := tryRun(s, `DROP TABLE t1, nope`); code(err) != codeUndefinedTable {
		t.Fatalf("drop with a missing table: %v", err)
	}
	if r := run(t, s, `SELECT count(*) FROM t1`); r.Rows[0][0].I != 2 {
		t.Fatal("t1 dropped by a failed DROP")
	}
	r := run(t, s, `DROP TABLE IF EXISTS t1, nope CASCADE`)
	if r.Tag != "DROP TABLE" || !strings.Contains(r.Notice, "nope") {
		t.Fatalf("drop if exists: %q %q", r.Tag, r.Notice)
	}
	if _, err := tryRun(s, `SELECT * FROM t1`); code(err) != codeUndefinedTable {
		t.Fatalf("select dropped table: %v", err)
	}
	// dropped from the publication and from REPLICA IDENTITY FULL
	if r := run(t, s, `SELECT tablename FROM pg_publication_tables ORDER BY tablename`); len(r.Rows) != 1 || r.Rows[0][0].S != "t2" {
		t.Fatalf("publication tables after drop: %s", render(r))
	}
	if e.ReplicaIdentityFull("t1") {
		t.Fatal("t1 still REPLICA IDENTITY FULL")
	}

	if r := run(t, s, `TRUNCATE TABLE ONLY t2, t3 RESTART IDENTITY`); r.Tag != "TRUNCATE TABLE" {
		t.Fatalf("truncate tag %q", r.Tag)
	}
	for _, q := range []string{`SELECT count(*) FROM t2`, `SELECT count(*) FROM t3`} {
		if r := run(t, s, q); r.Rows[0][0].I != 0 {
			t.Fatalf("%s after truncate: %s", q, render(r))
		}
	}
	run(t, s, `INSERT INTO t2 VALUES (7, 'z')`)
	// the columnar view of the new table answers aggregates
	if r := run(t, s, `SELECT v, count(*) FROM t2 GROUP BY v`); len(r.Rows) != 1 || r.Rows[0][1].I != 1 {
		t.Fatalf("aggregate after truncate: %s", render(r))
	}
	// the name is free again after DROP
	run(t, s, `CREATE TABLE t1 (id BIGINT PRIMARY KEY, w BIGINT)`)
	run(t, s, `INSERT INTO t1 VALUES (5, 50)`)
	if r := run(t, s, `SELECT w FROM t1`); len(r.Rows) != 1 || r.Rows[0][0].I != 50 {
		t.Fatalf("recreated t1: %s", render(r))
	}
	if _, err := tryRun(s, `TRUNCATE nope`); code(err) != codeUndefinedTable {
		t.Fatalf("truncate missing: %v", err)
	}

	td := strings.Join(data(run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('td', NULL, NULL)`)), "\n")
	for _, want := range []string{"table public.t2: TRUNCATE: (no-flags)", "table public.t3: TRUNCATE: (no-flags)"} {
		if !strings.Contains(td, want) {
			t.Fatalf("test_decoding missing %q:\n%s", want, td)
		}
	}
	w2 := strings.Join(data(run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('w2', NULL, NULL, 'format-version', '2')`)), "\n")
	if !strings.Contains(w2, `{"action":"T","schema":"public","table":"t2"}`) {
		t.Fatalf("wal2json 2 truncate missing:\n%s", w2)
	}
	w1 := strings.Join(data(run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('w2', NULL, NULL, 'format-version', '1')`)), "\n")
	if strings.Contains(w1, "truncate") {
		t.Fatalf("wal2json 1 has no truncate:\n%s", w1)
	}
}
