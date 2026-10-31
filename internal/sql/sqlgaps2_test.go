package sql

import "testing"

// runS is run with a session-aware parser (user types and tables resolve at parse time, as on the wire).
func runS(t *testing.T, s *Session, q string) *Result {
	t.Helper()
	res, err := tryRunS(s, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res
}

func tryRunS(s *Session, q string) (*Result, error) {
	stmts, err := parseWith(q, s)
	if err != nil {
		return nil, err
	}
	var res *Result
	for _, st := range stmts {
		if res, err = s.Exec(st, nil); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func oneS(t *testing.T, s *Session, q string) string {
	t.Helper()
	return render(runS(t, s, q))
}

func wantErrS(t *testing.T, s *Session, q, sqlstate string) {
	t.Helper()
	_, err := tryRunS(s, q)
	if code(err) != sqlstate {
		t.Fatalf("%s: got %v (%s), want SQLSTATE %s", q, err, code(err), sqlstate)
	}
}

// ---- UPDATE SET (a, b) = (SELECT ..) ----

func TestSQLGapsUpdateMultiAssign(t *testing.T) {
	s := NewSession(newEngine(t))
	runS(t, s, `CREATE TABLE u1 (id INT PRIMARY KEY, a INT, b TEXT, c INT)`)
	runS(t, s, `CREATE TABLE u2 (id INT PRIMARY KEY, x INT, y TEXT)`)
	runS(t, s, `INSERT INTO u1 VALUES (1,1,'a',10),(2,2,'b',20),(3,3,'c',30)`)
	runS(t, s, `INSERT INTO u2 VALUES (1,100,'p'),(2,200,'q')`)
	// PostgreSQL 16: a correlated sub-SELECT per row; no row gives NULLs
	runS(t, s, `UPDATE u1 SET (a,b) = (SELECT x,y FROM u2 WHERE u2.id = u1.id)`)
	if got := oneS(t, s, `SELECT id, a, b, c FROM u1 ORDER BY id`); got != "1,100,p,10;2,200,q,20;3,NULL,NULL,30" {
		t.Fatalf("correlated: %s", got)
	}
	runS(t, s, `UPDATE u1 SET (a,b) = (7,'z') WHERE id = 1`)
	runS(t, s, `UPDATE u1 SET (a,b) = ROW(8,'zz') WHERE id = 2`)
	runS(t, s, `UPDATE u1 SET (a,b,c) = (SELECT 1,'k',5) WHERE id = 3`)
	if got := oneS(t, s, `SELECT id, a, b, c FROM u1 ORDER BY id`); got != "1,7,z,10;2,8,zz,20;3,1,k,5" {
		t.Fatalf("row forms: %s", got)
	}
	// referencing the old row on the right-hand side
	runS(t, s, `UPDATE u1 SET (a,b) = (c, b || '!') WHERE id = 1`)
	if got := oneS(t, s, `SELECT a, b FROM u1 WHERE id = 1`); got != "10,z!" {
		t.Fatalf("old row: %s", got)
	}
	// mixed with plain assignments, RETURNING, UPDATE .. FROM
	if got := render(runS(t, s, `UPDATE u1 SET c = 1, (a,b) = (SELECT x,y FROM u2 WHERE id = 2) WHERE id = 3 RETURNING id, a, b, c`)); got != "3,200,q,1" {
		t.Fatalf("returning: %s", got)
	}
	runS(t, s, `UPDATE u1 SET (a,b) = (SELECT x,y FROM u2 WHERE id = 1) FROM u2 WHERE u1.id = u2.id AND u2.id = 2`)
	if got := oneS(t, s, `SELECT a, b FROM u1 WHERE id = 2`); got != "100,p" {
		t.Fatalf("update from: %s", got)
	}
	runS(t, s, `UPDATE u1 SET (a,b) = (SELECT max(x), min(y) FROM u2) WHERE id = 1`)
	if got := oneS(t, s, `SELECT a, b FROM u1 WHERE id = 1`); got != "200,p" {
		t.Fatalf("aggregate: %s", got)
	}
	// errors (PostgreSQL 16 SQLSTATEs)
	wantErrS(t, s, `UPDATE u1 SET (a,b) = (SELECT 1) WHERE id = 1`, "42601")
	wantErrS(t, s, `UPDATE u1 SET (a,b) = (1,2,3) WHERE id = 1`, "42601")
	wantErrS(t, s, `UPDATE u1 SET (a) = (9) WHERE id = 1`, "0A000")
	wantErrS(t, s, `UPDATE u1 SET (a,b) = (SELECT x,y FROM u2) WHERE id = 1`, "21000")
	runS(t, s, `UPDATE u1 SET (a) = ROW(9) WHERE id = 1`)
	runS(t, s, `UPDATE u1 SET (a) = (SELECT 11) WHERE id = 1`)
	if got := oneS(t, s, `SELECT a FROM u1 WHERE id = 1`); got != "11" {
		t.Fatalf("single column: %s", got)
	}
}

// ---- composite types ----

func TestSQLGapsCompositeTypes(t *testing.T) {
	s := NewSession(newEngine(t))
	runS(t, s, `CREATE TYPE pt AS (x INT, y TEXT)`)
	wantErrS(t, s, `CREATE TYPE pt AS (x INT)`, "42710")
	wantErrS(t, s, `CREATE TYPE bad AS (x INT, x TEXT)`, "42701")
	runS(t, s, `CREATE TABLE ct (id INT PRIMARY KEY, p pt)`)
	runS(t, s, `INSERT INTO ct VALUES (1, ROW(1,'a')), (2, '(2,"b c")'), (3, NULL)`)
	if got := oneS(t, s, `SELECT id, (p).x, (p).y FROM ct ORDER BY id`); got != "1,1,a;2,2,b c;3,NULL,NULL" {
		t.Fatalf("columns: %s", got)
	}
	if got := oneS(t, s, `SELECT p::text FROM ct WHERE id = 1`); got != "(1,a)" {
		t.Fatalf("text form: %s", got)
	}
	wantErrS(t, s, `INSERT INTO ct VALUES (4, '(x,d)')`, "22P02")
	wantErrS(t, s, `INSERT INTO ct VALUES (5, '(5,d,6)')`, "22P02")
	wantErrS(t, s, `SELECT '(5)'::pt`, "22P02")
	if got := oneS(t, s, `SELECT ('(5,"a,b")'::pt).y`); got != "a,b" {
		t.Fatalf("quoted field: %s", got)
	}
	if got := oneS(t, s, `SELECT id FROM ct WHERE (p).x > 1 ORDER BY id`); got != "2" {
		t.Fatalf("where: %s", got)
	}
	// functions: scalar composite, SETOF composite, RETURNS TABLE
	runS(t, s, `CREATE FUNCTION mk(a INT, b TEXT) RETURNS pt AS $f$ BEGIN RETURN ROW(a, b); END $f$ LANGUAGE plpgsql`)
	if got := oneS(t, s, `SELECT mk(3,'c')::text`); got != "(3,c)" {
		t.Fatalf("mk: %s", got)
	}
	if got := oneS(t, s, `SELECT (mk(3,'c')).x, (mk(3,'c')).y`); got != "3,c" {
		t.Fatalf("field of call: %s", got)
	}
	if got := oneS(t, s, `SELECT * FROM mk(4,'d')`); got != "4,d" {
		t.Fatalf("select *: %s", got)
	}
	runS(t, s, `CREATE FUNCTION mks(n INT) RETURNS SETOF pt AS $f$ BEGIN FOR i IN 1..n LOOP RETURN NEXT ROW(i, 'r' || i); END LOOP; END $f$ LANGUAGE plpgsql`)
	if got := oneS(t, s, `SELECT x, y FROM mks(3) WHERE x > 1 ORDER BY x`); got != "2,r2;3,r3" {
		t.Fatalf("setof: %s", got)
	}
	runS(t, s, `CREATE FUNCTION rt(n INT) RETURNS TABLE (i INT, s TEXT) AS $f$ BEGIN RETURN QUERY SELECT g, 's' || g FROM generate_series(1,n) g; END $f$ LANGUAGE plpgsql`)
	if got := oneS(t, s, `SELECT s FROM rt(3) WHERE i = 2`); got != "s2" {
		t.Fatalf("returns table: %s", got)
	}
	runS(t, s, `CREATE FUNCTION sq(n INT) RETURNS pt AS $f$ SELECT n, 'q'::text $f$ LANGUAGE sql`)
	if got := oneS(t, s, `SELECT (sq(6)).y`); got != "q" {
		t.Fatalf("sql composite: %s", got)
	}
	runS(t, s, `CREATE FUNCTION nul() RETURNS pt AS $f$ BEGIN RETURN NULL; END $f$ LANGUAGE plpgsql`)
	if got := oneS(t, s, `SELECT nul() IS NULL`); got != "t" && got != "true" {
		t.Fatalf("null composite: %s", got)
	}
	// nested composite
	runS(t, s, `CREATE TYPE pt2 AS (a pt, n INT)`)
	if got := oneS(t, s, `SELECT ((ROW(ROW(1,'a'),2)::pt2).a).y`); got != "a" {
		t.Fatalf("nested: %s", got)
	}
	// renaming a composite type follows its column and functions
	runS(t, s, `DROP TYPE pt2`)
	runS(t, s, `ALTER TYPE pt RENAME TO pt3`)
	if got := oneS(t, s, `SELECT (p).y FROM ct WHERE id = 1`); got != "a" {
		t.Fatalf("after rename: %s", got)
	}
	runS(t, s, `ALTER TYPE pt3 RENAME TO pt`)
	// dependent column: restrict, then cascade
	wantErrS(t, s, `DROP TYPE pt`, "2BP01")
	runS(t, s, `DROP TYPE pt CASCADE`)
	if hasColT(t, s, "ct", "p") {
		t.Fatal("column of the dropped type is still there")
	}
}

// ---- event triggers ----

func TestSQLGapsEventTriggers(t *testing.T) {
	s := NewSession(newEngine(t))
	runS(t, s, `CREATE TABLE ev_log (id SERIAL PRIMARY KEY, ev TEXT, tag TEXT)`)
	runS(t, s, `CREATE FUNCTION ev_fn() RETURNS event_trigger AS $f$ BEGIN INSERT INTO ev_log (ev, tag) VALUES (TG_EVENT, TG_TAG); END $f$ LANGUAGE plpgsql`)
	runS(t, s, `CREATE EVENT TRIGGER ev_s ON ddl_command_start EXECUTE FUNCTION ev_fn()`)
	runS(t, s, `CREATE EVENT TRIGGER ev_e ON ddl_command_end EXECUTE FUNCTION ev_fn()`)
	wantErrS(t, s, `CREATE EVENT TRIGGER ev_e ON ddl_command_end EXECUTE FUNCTION ev_fn()`, "42710")
	log := func() string {
		r := oneS(t, s, `SELECT coalesce(string_agg(ev || ' ' || tag, ';' ORDER BY id), '') FROM ev_log`)
		runS(t, s, `DELETE FROM ev_log`)
		return r
	}
	_ = log()
	runS(t, s, `CREATE TABLE evt (id INT PRIMARY KEY, v TEXT)`)
	runS(t, s, `INSERT INTO evt VALUES (1,'a')`) // DML: no event
	runS(t, s, `TRUNCATE evt`)                   // TRUNCATE: no event
	if got := log(); got != "ddl_command_start CREATE TABLE;ddl_command_end CREATE TABLE" {
		t.Fatalf("create table: %s", got)
	}
	runS(t, s, `CREATE INDEX evt_v ON evt (v)`)
	runS(t, s, `DROP TABLE evt`)
	if got := log(); got != "ddl_command_start CREATE INDEX;ddl_command_end CREATE INDEX;ddl_command_start DROP TABLE;ddl_command_end DROP TABLE" {
		t.Fatalf("index, drop: %s", got)
	}
	// a failing command undoes the start trigger's work (one transaction)
	if _, err := tryRunS(s, `ALTER TABLE nosuch ADD COLUMN a INT`); err == nil {
		t.Fatal("expected an error")
	}
	if got := log(); got != "" {
		t.Fatalf("failed command left %q", got)
	}
	// disable, tag filter, errors
	runS(t, s, `ALTER EVENT TRIGGER ev_s DISABLE`)
	runS(t, s, `CREATE TABLE evt (a INT)`)
	if got := log(); got != "ddl_command_end CREATE TABLE" {
		t.Fatalf("disabled: %s", got)
	}
	runS(t, s, `DROP EVENT TRIGGER ev_s`)
	runS(t, s, `DROP EVENT TRIGGER ev_e`)
	wantErrS(t, s, `DROP EVENT TRIGGER ev_s`, "42704")
	runS(t, s, `DROP EVENT TRIGGER IF EXISTS ev_s`)
	wantErrS(t, s, `CREATE EVENT TRIGGER bad ON nosuchevent EXECUTE FUNCTION ev_fn()`, "42601")
	wantErrS(t, s, `CREATE EVENT TRIGGER bad ON ddl_command_end WHEN TAG IN ('NOPE') EXECUTE FUNCTION ev_fn()`, "42601")
	wantErrS(t, s, `CREATE EVENT TRIGGER bad ON ddl_command_end EXECUTE FUNCTION nosuchfn()`, "42883")
	runS(t, s, `CREATE EVENT TRIGGER ev_f ON ddl_command_end WHEN TAG IN ('CREATE TABLE', 'DROP TABLE') EXECUTE FUNCTION ev_fn()`)
	runS(t, s, `CREATE TABLE evt5 (a INT)`)
	runS(t, s, `CREATE VIEW evv5 AS SELECT 1`)
	runS(t, s, `DROP TABLE evt5`)
	if got := log(); got != "ddl_command_end CREATE TABLE;ddl_command_end DROP TABLE" {
		t.Fatalf("tag filter: %s", got)
	}
	runS(t, s, `DROP EVENT TRIGGER ev_f`)
	// sql_drop
	runS(t, s, `CREATE TABLE dl (ev TEXT, typ TEXT, nm TEXT)`)
	runS(t, s, `CREATE FUNCTION ev_drop() RETURNS event_trigger AS $f$ DECLARE r record; BEGIN FOR r IN SELECT * FROM pg_event_trigger_dropped_objects() LOOP INSERT INTO dl VALUES (TG_TAG, r.object_type, r.object_name); END LOOP; END $f$ LANGUAGE plpgsql`)
	runS(t, s, `CREATE EVENT TRIGGER ev_d ON sql_drop EXECUTE FUNCTION ev_drop()`)
	runS(t, s, `CREATE TABLE d1 (a INT)`)
	runS(t, s, `DROP TABLE d1`)
	runS(t, s, `DROP TABLE IF EXISTS d9`)
	if got := oneS(t, s, `SELECT ev, typ, nm FROM dl ORDER BY nm`); got != "DROP TABLE,table,d1" {
		t.Fatalf("sql_drop: %s", got)
	}
	wantErrS(t, s, `SELECT * FROM pg_event_trigger_dropped_objects()`, "39P03")
	// an exception in a start trigger blocks the command
	runS(t, s, `DROP EVENT TRIGGER ev_d`)
	runS(t, s, `CREATE FUNCTION ev_err() RETURNS event_trigger AS $f$ BEGIN RAISE EXCEPTION 'no ddl for you: %', TG_TAG; END $f$ LANGUAGE plpgsql`)
	runS(t, s, `CREATE EVENT TRIGGER ev_x ON ddl_command_start WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION ev_err()`)
	if _, err := tryRunS(s, `CREATE TABLE blocked (a INT)`); err == nil || hasTable(s, "blocked") {
		t.Fatalf("blocked table: err=%v exists=%v", err, hasTable(s, "blocked"))
	}
	runS(t, s, `BEGIN`)
	runS(t, s, `CREATE VIEW okv AS SELECT 1`)
	if _, err := tryRunS(s, `CREATE TABLE blocked2 (a INT)`); err == nil {
		t.Fatal("expected the trigger to block")
	}
	runS(t, s, `ROLLBACK`)
	if got := oneS(t, s, `SELECT count(*) FROM pg_event_trigger`); got != "1" {
		t.Fatalf("pg_event_trigger: %s", got)
	}
}

// ---- DDL rollback ----

func TestSQLGapsDDLRollback(t *testing.T) {
	s := NewSession(newEngine(t))
	runS(t, s, `CREATE TYPE r_mood AS ENUM ('a','b')`)
	runS(t, s, `CREATE DOMAIN r_dom AS INT CHECK (VALUE > 0)`)
	runS(t, s, `CREATE TABLE r_t (id INT PRIMARY KEY, m r_mood, d r_dom, v TEXT)`)
	runS(t, s, `CREATE INDEX r_t_v ON r_t (v)`)
	runS(t, s, `INSERT INTO r_t VALUES (1, 'a', 5, 'x')`)
	runS(t, s, `CREATE FUNCTION r_f() RETURNS INT AS $f$ SELECT 1 $f$ LANGUAGE sql`)

	// functions
	runS(t, s, `BEGIN`)
	runS(t, s, `CREATE FUNCTION r_f2() RETURNS INT AS $f$ SELECT 2 $f$ LANGUAGE sql`)
	runS(t, s, `CREATE OR REPLACE FUNCTION r_f() RETURNS INT AS $f$ SELECT 100 $f$ LANGUAGE sql`)
	if got := oneS(t, s, `SELECT r_f(), r_f2()`); got != "100,2" {
		t.Fatalf("in tx: %s", got)
	}
	runS(t, s, `ROLLBACK`)
	if got := oneS(t, s, `SELECT r_f()`); got != "1" {
		t.Fatalf("replace not undone: %s", got)
	}
	wantErrS(t, s, `SELECT r_f2()`, "42883")
	runS(t, s, `BEGIN`)
	runS(t, s, `DROP FUNCTION r_f()`)
	runS(t, s, `ROLLBACK`)
	if got := oneS(t, s, `SELECT r_f()`); got != "1" {
		t.Fatalf("drop not undone: %s", got)
	}
	runS(t, s, `BEGIN`)
	runS(t, s, `DROP FUNCTION r_f()`)
	runS(t, s, `COMMIT`)
	wantErrS(t, s, `SELECT r_f()`, "42883")

	// ALTER TYPE .. RENAME, RENAME VALUE, ALTER DOMAIN .. RENAME
	runS(t, s, `BEGIN`)
	runS(t, s, `ALTER TYPE r_mood RENAME TO r_mood2`)
	if got := oneS(t, s, `SELECT 'a'::r_mood2`); got != "a" {
		t.Fatalf("renamed type in tx: %s", got)
	}
	runS(t, s, `ROLLBACK`)
	if got := oneS(t, s, `SELECT m::text FROM r_t`); got != "a" {
		t.Fatalf("rename type: %s", got)
	}
	wantErrS(t, s, `SELECT 'a'::r_mood2`, "42704")
	runS(t, s, `INSERT INTO r_t VALUES (2, 'b', 6, 'y')`) // the column still has the type
	runS(t, s, `BEGIN`)
	runS(t, s, `ALTER TYPE r_mood RENAME VALUE 'a' TO 'z'`)
	if got := oneS(t, s, `SELECT m::text FROM r_t WHERE id = 1`); got != "z" {
		t.Fatalf("renamed value in tx: %s", got)
	}
	runS(t, s, `ROLLBACK`)
	if got := oneS(t, s, `SELECT string_agg(m::text, ',' ORDER BY id) FROM r_t`); got != "a,b" {
		t.Fatalf("rename value: %s", got)
	}
	runS(t, s, `BEGIN`)
	runS(t, s, `ALTER DOMAIN r_dom RENAME TO r_dom2`)
	runS(t, s, `ROLLBACK`)
	if got := oneS(t, s, `SELECT 5::r_dom`); got != "5" {
		t.Fatalf("rename domain: %s", got)
	}
	wantErrS(t, s, `SELECT 5::r_dom2`, "42704")
	// a committed rename stays
	runS(t, s, `BEGIN`)
	runS(t, s, `ALTER TYPE r_mood RENAME TO r_mood3`)
	runS(t, s, `COMMIT`)
	if got := oneS(t, s, `SELECT 'b'::r_mood3`); got != "b" {
		t.Fatalf("committed rename: %s", got)
	}
	runS(t, s, `ALTER TYPE r_mood3 RENAME TO r_mood`)

	// ALTER INDEX .. RENAME
	runS(t, s, `BEGIN`)
	runS(t, s, `ALTER INDEX r_t_v RENAME TO r_t_v2`)
	if !hasIdx(s, "r_t_v2") {
		t.Fatal("renamed index missing in tx")
	}
	runS(t, s, `ROLLBACK`)
	if !hasIdx(s, "r_t_v") || hasIdx(s, "r_t_v2") {
		t.Fatalf("rename index not undone: old=%v new=%v", hasIdx(s, "r_t_v"), hasIdx(s, "r_t_v2"))
	}

	// ALTER TABLE and CREATE INDEX on a TEMP table
	runS(t, s, `CREATE TEMP TABLE r_tmp (a INT)`)
	runS(t, s, `BEGIN`)
	runS(t, s, `ALTER TABLE r_tmp ADD COLUMN b INT`)
	runS(t, s, `CREATE INDEX r_tmp_a ON r_tmp (a)`)
	runS(t, s, `ROLLBACK`)
	wantErrS(t, s, `SELECT b FROM r_tmp`, "42703")
	if ts, err := s.E.Table(s.mapTemp("r_tmp")); err != nil || ts.Schema().Index("r_tmp_a") != nil {
		t.Fatalf("index on a TEMP table survived the rollback: %v", err)
	}

	// CREATE TABLE .. PARTITION OF
	runS(t, s, `CREATE TABLE r_p (id BIGINT, d DATE, k TEXT, PRIMARY KEY (id, d)) PARTITION BY RANGE (d)`)
	runS(t, s, `BEGIN`)
	runS(t, s, `CREATE TABLE r_p1 PARTITION OF r_p FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')`)
	runS(t, s, `ROLLBACK`)
	if got := oneS(t, s, `SELECT count(*) FROM r_p`); got != "0" {
		t.Fatalf("partition rows after rollback: %s", got)
	}
	wantErrS(t, s, `SELECT * FROM r_p1`, "42P01")
	runS(t, s, `CREATE TABLE r_p1 PARTITION OF r_p FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')`)
	runS(t, s, `BEGIN`)
	runS(t, s, `CREATE TABLE r_p2 PARTITION OF r_p FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')`)
	runS(t, s, `COMMIT`)
	run(t, s, `INSERT INTO r_p VALUES (2, '2025-05-05', 'x')`)
	if got := oneS(t, s, `SELECT count(*) FROM r_p2`); got != "1" {
		t.Fatalf("committed partition: %s", got)
	}
}
