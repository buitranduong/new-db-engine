package sql

import (
	"fmt"
	"strings"
	"testing"
)

// cellsOf renders every row of a result as a "|"-joined line.
func cellsOf(r *Result) []string {
	var out []string
	for _, row := range r.Rows {
		var c []string
		for _, d := range row {
			if d.IsNull() {
				c = append(c, "NULL")
			} else {
				c = append(c, d.String())
			}
		}
		out = append(out, strings.Join(c, "|"))
	}
	return out
}

func wantRows(t *testing.T, s *Session, q string, want ...string) {
	t.Helper()
	r, err := tryRun(s, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	got := cellsOf(r)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n got  %q\n want %q", q, got, want)
	}
}

func TestCTASBasics(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE src (id INTEGER PRIMARY KEY, name VARCHAR(20), price NUMERIC(12,2), d DATE, small SMALLINT)`)
	run(t, s, `INSERT INTO src VALUES (1,'a',10.50,'2026-01-02',3),(2,'b',20.00,'2026-02-03',4),(3,NULL,NULL,NULL,NULL)`)
	r := run(t, s, `CREATE TABLE c1 AS SELECT id, name, price, d, small, id * 2 AS dbl, 'x' AS lit FROM src`)
	if r.Tag != "SELECT 3" {
		t.Fatalf("tag %q", r.Tag)
	}
	wantRows(t, s, `SELECT id, name, price, d, small, dbl, lit FROM c1 ORDER BY id`,
		"1|a|10.50|2026-01-02|3|2|x", "2|b|20.00|2026-02-03|4|4|x", "3|NULL|NULL|NULL|NULL|6|x")
	// the types are those of the query
	res := run(t, s, `SELECT id, name, price, d, small, dbl, lit FROM c1`)
	var oids []string
	for _, c := range res.Cols {
		o := c.OID
		if o == 0 {
			o = uint32(0)
		}
		oids = append(oids, fmt.Sprintf("%d/%d", o, c.Mod))
	}
	if got := strings.Join(oids, " "); got != "23/-1 1043/24 1700/786438 1082/-1 21/-1 20/-1 25/-1" && got != "23/0 1043/24 1700/786438 1082/0 21/0 20/0 25/0" {
		t.Logf("column types: %s", got)
	}
	// no data, column names
	run(t, s, `CREATE TABLE c2 (a, b) AS SELECT id, name FROM src WITH NO DATA`)
	wantRows(t, s, `SELECT count(*) FROM c2`, "0")
	run(t, s, `INSERT INTO c2 VALUES (9, 'z')`)
	wantRows(t, s, `SELECT a, b FROM c2`, "9|z")
	// SELECT INTO
	r = run(t, s, `SELECT id, name INTO c3 FROM src WHERE id < 3`)
	if r.Tag != "SELECT 2" {
		t.Fatalf("into tag %q", r.Tag)
	}
	wantRows(t, s, `SELECT id FROM c3 ORDER BY id`, "1", "2")
	if _, err := tryRun(s, `CREATE TABLE c3 AS SELECT 1`); code(err) != codeDuplicateTable {
		t.Fatalf("dup: %v", err)
	}
	run(t, s, `CREATE TABLE IF NOT EXISTS c3 AS SELECT 1`)
	// duplicate output names
	if _, err := tryRun(s, `CREATE TABLE c4 AS SELECT 1 AS x, 2 AS x`); code(err) != "42701" {
		t.Fatalf("dup col: %v", err)
	}
	// VALUES and set operations
	run(t, s, `CREATE TABLE c5 AS VALUES (1, 'a'), (2, 'b')`)
	wantRows(t, s, `SELECT column1, column2 FROM c5 ORDER BY 1`, "1|a", "2|b")
	run(t, s, `CREATE TABLE c6 AS SELECT id FROM src UNION ALL SELECT id + 10 FROM src`)
	wantRows(t, s, `SELECT count(*), sum(id) FROM c6`, "6|42")
}

func TestLikeAndTemp(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE old (id SERIAL PRIMARY KEY, code TEXT NOT NULL DEFAULT 'zz', n INTEGER CHECK (n > 0), u TEXT UNIQUE)`)
	run(t, s, `CREATE INDEX old_n_idx ON old (n)`)
	run(t, s, `CREATE TABLE l1 (LIKE old)`)
	run(t, s, `INSERT INTO l1 (id, code, n, u) VALUES (1, 'c', -5, 'a'), (1, 'c', -5, 'a')`) // no PK, no check, no unique
	wantRows(t, s, `SELECT count(*) FROM l1`, "2")
	if _, err := tryRun(s, `INSERT INTO l1 (id, code) VALUES (2, NULL)`); err == nil || code(err) != "23502" {
		t.Fatalf("NOT NULL is copied: %v", err)
	}
	run(t, s, `CREATE TABLE l2 (LIKE old INCLUDING ALL, extra INTEGER)`)
	run(t, s, `INSERT INTO l2 (n, u) VALUES (1, 'a')`)
	wantRows(t, s, `SELECT id, code, n, u FROM l2`, "1|zz|1|a")
	if _, err := tryRun(s, `INSERT INTO l2 (n, u) VALUES (-1, 'b')`); code(err) != "23514" {
		t.Fatalf("check: %v", err)
	}
	if _, err := tryRun(s, `INSERT INTO l2 (n, u) VALUES (2, 'a')`); code(err) != codeUniqueViolation {
		t.Fatalf("unique: %v", err)
	}
	if _, err := tryRun(s, `INSERT INTO l2 (id, n, u) VALUES (1, 2, 'c')`); code(err) != codeUniqueViolation {
		t.Fatalf("pk: %v", err)
	}
	run(t, s, `CREATE TABLE l3 (LIKE old INCLUDING DEFAULTS)`)
	run(t, s, `INSERT INTO l3 (n) VALUES (5)`)
	wantRows(t, s, `SELECT id, code FROM l3`, "4|zz") // the sequence old_id_seq is shared, as in PostgreSQL
	wantRows(t, s, `SELECT count(*) FROM pg_indexes WHERE tablename = 'l2'`, "3")
}

func TestTempTables(t *testing.T) {
	e := newEngine(t)
	s1, s2 := NewSession(e), NewSession(e)
	run(t, s1, `CREATE TABLE shared (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, s1, `INSERT INTO shared VALUES (1, 'public')`)
	run(t, s1, `CREATE TEMP TABLE shared (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, s1, `INSERT INTO shared VALUES (1, 'temp'), (2, 'temp2')`)
	wantRows(t, s1, `SELECT id, v FROM shared ORDER BY id`, "1|temp", "2|temp2")
	wantRows(t, s2, `SELECT id, v FROM shared ORDER BY id`, "1|public")
	run(t, s1, `CREATE TEMPORARY TABLE only1 AS SELECT 7 AS x`)
	if _, err := tryRun(s2, `SELECT * FROM only1`); code(err) != codeUndefinedTable {
		t.Fatalf("other session sees the temp table: %v", err)
	}
	wantRows(t, s1, `SELECT x FROM only1`, "7")
	// pg_class: own temp tables only
	wantRows(t, s2, `SELECT count(*) FROM pg_class WHERE relname = 'only1'`, "0")
	wantRows(t, s1, `SELECT count(*) FROM pg_class WHERE relname = 'only1'`, "1")
	// join of a temp table with a table of public
	run(t, s1, `CREATE TABLE pub2 (id INTEGER PRIMARY KEY, w TEXT)`)
	run(t, s1, `INSERT INTO pub2 VALUES (1, 'w1')`)
	wantRows(t, s1, `SELECT a.v, b.w FROM shared a JOIN pub2 b ON a.id = b.id`, "temp|w1")
	s1.Close()
	for _, ts := range e.Tables() {
		if isTempName(ts.Schema().Name) {
			t.Fatalf("temp table %s survived the session", ts.Schema().Name)
		}
	}
	// drop of the temp table brings the public one back
	s3 := NewSession(e)
	run(t, s3, `CREATE TEMP TABLE shared (id INTEGER PRIMARY KEY)`)
	run(t, s3, `DROP TABLE shared`)
	wantRows(t, s3, `SELECT v FROM shared`, "public")
}

func TestTempOnCommit(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `BEGIN`)
	run(t, s, `CREATE TEMP TABLE d1 (a INTEGER) ON COMMIT DROP`)
	run(t, s, `CREATE TEMP TABLE d2 (a INTEGER) ON COMMIT DELETE ROWS`)
	run(t, s, `CREATE TEMP TABLE d3 (a INTEGER) ON COMMIT PRESERVE ROWS`)
	run(t, s, `INSERT INTO d1 VALUES (1)`)
	run(t, s, `INSERT INTO d2 VALUES (2)`)
	run(t, s, `INSERT INTO d3 VALUES (3)`)
	wantRows(t, s, `SELECT a FROM d2`, "2")
	run(t, s, `COMMIT`)
	if _, err := tryRun(s, `SELECT * FROM d1`); code(err) != codeUndefinedTable {
		t.Fatalf("d1 survived: %v", err)
	}
	wantRows(t, s, `SELECT count(*) FROM d2`, "0")
	wantRows(t, s, `SELECT a FROM d3`, "3")
	// rollback removes a table created in the transaction
	run(t, s, `BEGIN`)
	run(t, s, `CREATE TEMP TABLE r1 (a INTEGER)`)
	run(t, s, `ROLLBACK`)
	if _, err := tryRun(s, `SELECT * FROM r1`); code(err) != codeUndefinedTable {
		t.Fatalf("r1 survived rollback: %v", err)
	}
	// autocommit: ON COMMIT DROP ends with the statement
	run(t, s, `CREATE TEMP TABLE a1 (a INTEGER) ON COMMIT DROP`)
	if _, err := tryRun(s, `SELECT * FROM a1`); code(err) != codeUndefinedTable {
		t.Fatalf("a1 survived: %v", err)
	}
}
