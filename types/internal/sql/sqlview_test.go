package sql

import (
	"strings"
	"testing"

	"stask/internal/core"
)

func viewSetup(t *testing.T, s *Session) {
	t.Helper()
	run(t, s, `CREATE TABLE emp (id BIGINT PRIMARY KEY, name TEXT, dept TEXT, sal BIGINT)`)
	run(t, s, `INSERT INTO emp VALUES (1,'ann','eng',100),(2,'bob','eng',200),(3,'cy','ops',50),(4,'di','ops',70)`)
}

func TestSQLView(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	viewSetup(t, s)

	if r := run(t, s, `CREATE VIEW rich AS SELECT id, name, sal FROM emp WHERE sal >= 70`); r.Tag != "CREATE VIEW" {
		t.Fatalf("tag %q", r.Tag)
	}
	if got := render(run(t, s, `SELECT name FROM rich ORDER BY id`)); got != "ann;bob;di" {
		t.Fatalf("select from view: %s", got)
	}
	// the view follows the data
	run(t, s, `INSERT INTO emp VALUES (5,'ed','eng',999)`)
	if got := render(run(t, s, `SELECT count(*) FROM rich`)); got != "4" {
		t.Fatalf("count: %s", got)
	}
	// aliases, joins, where, aggregate over the view
	if got := render(run(t, s, `SELECT r.name, e.dept FROM rich r JOIN emp e ON e.id = r.id WHERE r.sal > 150 ORDER BY r.id`)); got != "bob,eng;ed,eng" {
		t.Fatalf("join: %s", got)
	}
	// column list
	run(t, s, `CREATE VIEW dept_tot (d, total) AS SELECT dept, sum(sal) FROM emp GROUP BY dept`)
	if got := render(run(t, s, `SELECT d, total FROM dept_tot ORDER BY d`)); got != "eng,1299;ops,120" {
		t.Fatalf("column list: %s", got)
	}
	if _, err := tryRun(s, `SELECT dept FROM dept_tot`); code(err) != codeUndefinedColumn {
		t.Fatalf("hidden underlying name: %v", err)
	}
	// SELECT * is expanded at creation: later columns do not show up
	run(t, s, `CREATE VIEW all_emp AS SELECT * FROM emp`)
	run(t, s, `ALTER TABLE emp ADD COLUMN extra BIGINT`)
	if r := run(t, s, `SELECT * FROM all_emp WHERE id = 1`); len(r.Cols) != 4 {
		t.Fatalf("star view gained columns: %d", len(r.Cols))
	}
	// set operations, CTE, subquery in the view
	run(t, s, `CREATE VIEW names AS WITH x AS (SELECT name FROM emp WHERE dept='eng') SELECT name FROM x UNION ALL SELECT 'zed'`)
	if got := render(run(t, s, `SELECT count(*) FROM names`)); got != "4" {
		t.Fatalf("cte view: %s", got)
	}
	// a view does not see the CTEs of the query that uses it
	if got := render(run(t, s, `WITH emp AS (SELECT 1 AS id) SELECT count(*) FROM rich`)); got != "4" {
		t.Fatalf("view saw outer cte: %s", got)
	}
	// view in a subquery
	if got := render(run(t, s, `SELECT name FROM emp WHERE id IN (SELECT id FROM rich) AND dept = 'ops'`)); got != "di" {
		t.Fatalf("view in subquery: %s", got)
	}

	// duplicates and name clashes
	if _, err := tryRun(s, `CREATE VIEW rich AS SELECT 1 AS x`); code(err) != codeDuplicateTable {
		t.Fatalf("duplicate view: %v", err)
	}
	if _, err := tryRun(s, `CREATE VIEW emp AS SELECT 1 AS x`); code(err) != codeDuplicateTable {
		t.Fatalf("view named as a table: %v", err)
	}
	if _, err := tryRun(s, `CREATE TABLE rich (id BIGINT PRIMARY KEY)`); code(err) != codeDuplicateTable {
		t.Fatalf("table named as a view: %v", err)
	}
	if _, err := tryRun(s, `CREATE VIEW bad AS SELECT * FROM nope`); code(err) != codeUndefinedTable {
		t.Fatalf("view of a missing table: %v", err)
	}
	if _, err := tryRun(s, `CREATE VIEW bad AS SELECT 1, 1`); code(err) != "42701" {
		t.Fatalf("duplicate column names: %v", err)
	}
	if _, err := tryRun(s, `CREATE VIEW bad (a, b, c) AS SELECT 1`); code(err) != codeSyntax {
		t.Fatalf("too many column names: %v", err)
	}
	if _, err := tryRun(s, `CREATE MATERIALIZED VIEW bad AS SELECT 1`); code(err) != codeFeature {
		t.Fatalf("materialized: %v", err)
	}
	// a view is read-only and is not a table
	for _, q := range []string{`INSERT INTO rich VALUES (9,'x',1)`, `UPDATE rich SET sal = 1`, `DELETE FROM rich`} {
		if _, err := tryRun(s, q); code(err) != codeObjectNotInState {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := tryRun(s, `DROP TABLE rich`); code(err) != codeWrongObjectType {
		t.Fatalf("drop table on a view: %v", err)
	}
	if _, err := tryRun(s, `DROP VIEW emp`); code(err) != codeWrongObjectType {
		t.Fatalf("drop view on a table: %v", err)
	}

	// OR REPLACE: same columns (more at the end allowed), other query
	run(t, s, `CREATE OR REPLACE VIEW rich AS SELECT id, name, sal FROM emp WHERE sal >= 200`)
	if got := render(run(t, s, `SELECT name FROM rich ORDER BY id`)); got != "bob;ed" {
		t.Fatalf("replaced: %s", got)
	}
	run(t, s, `CREATE OR REPLACE VIEW rich AS SELECT id, name, sal, dept FROM emp WHERE sal >= 200`)
	if r := run(t, s, `SELECT * FROM rich`); len(r.Cols) != 4 {
		t.Fatalf("added column: %d", len(r.Cols))
	}
	for _, q := range []string{
		`CREATE OR REPLACE VIEW rich AS SELECT id, name FROM emp`,            // drops columns
		`CREATE OR REPLACE VIEW rich AS SELECT name, id, sal, dept FROM emp`, // renames/reorders
		`CREATE OR REPLACE VIEW rich AS SELECT id, name, 'x', dept FROM emp`, // type change of sal
	} {
		if _, err := tryRun(s, q); code(err) != "42P16" {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run(t, s, `CREATE OR REPLACE VIEW fresh AS SELECT 1 AS one`)
	if got := render(run(t, s, `SELECT one FROM fresh`)); got != "1" {
		t.Fatalf("or replace creating: %s", got)
	}

	// ALTER TABLE that would break the stored query is refused
	for _, q := range []string{`ALTER TABLE emp DROP COLUMN sal`, `ALTER TABLE emp RENAME TO emp2`, `ALTER TABLE emp RENAME COLUMN sal TO pay`} {
		if _, err := tryRun(s, q); code(err) != "2BP01" {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run(t, s, `ALTER TABLE emp ADD COLUMN more BIGINT`) // harmless

	// dependencies
	if _, err := tryRun(s, `DROP TABLE emp`); code(err) != "2BP01" {
		t.Fatalf("drop table with views: %v", err)
	}
	if got := render(run(t, s, `SELECT count(*) FROM emp`)); got != "5" {
		t.Fatalf("table lost: %s", got)
	}
	r := run(t, s, `DROP VIEW IF EXISTS nope, fresh`)
	if r.Tag != "DROP VIEW" || !strings.Contains(r.Notice, "nope") {
		t.Fatalf("drop if exists: %q %q", r.Tag, r.Notice)
	}
	if _, err := tryRun(s, `DROP VIEW nope`); code(err) != codeUndefinedTable {
		t.Fatalf("drop missing: %v", err)
	}
	if _, err := tryRun(s, `SELECT * FROM fresh`); code(err) != codeUndefinedTable {
		t.Fatalf("select dropped view: %v", err)
	}
}

func TestSQLViewNestedAndCycles(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	viewSetup(t, s)
	run(t, s, `CREATE VIEW v1 AS SELECT id, dept, sal FROM emp`)
	run(t, s, `CREATE VIEW v2 AS SELECT dept, sum(sal) AS total FROM v1 GROUP BY dept`)
	run(t, s, `CREATE VIEW v3 AS SELECT dept FROM v2 WHERE total > 200`)
	if got := render(run(t, s, `SELECT * FROM v3`)); got != "eng" {
		t.Fatalf("nested: %s", got)
	}
	if got := render(run(t, s, `SELECT v2.dept, v3.dept FROM v2 LEFT JOIN v3 ON v2.dept = v3.dept ORDER BY 1`)); got != "eng,eng;ops,NULL" && got != "eng,eng;ops," {
		t.Fatalf("join of views: %s", got)
	}

	// cycles: a view cannot come to depend on itself
	if _, err := tryRun(s, `CREATE OR REPLACE VIEW v1 AS SELECT id, dept, sal FROM v3, emp WHERE false`); code(err) != codeInvalidRecursion {
		t.Fatalf("indirect cycle: %v", err)
	}
	if _, err := tryRun(s, `CREATE OR REPLACE VIEW v2 AS SELECT dept, 1 AS total FROM v2`); code(err) != codeInvalidRecursion {
		t.Fatalf("self reference: %v", err)
	}
	if _, err := tryRun(s, `CREATE VIEW selfish AS SELECT * FROM selfish`); code(err) != codeUndefinedTable {
		t.Fatalf("new view naming itself: %v", err)
	}
	// the refused replacements changed nothing
	if got := render(run(t, s, `SELECT * FROM v3`)); got != "eng" {
		t.Fatalf("after refused cycles: %s", got)
	}

	// dependent views block the drop, with the 2BP01 of PostgreSQL
	for _, q := range []string{`DROP VIEW v1`, `DROP VIEW v2`, `DROP VIEW v1 RESTRICT`, `DROP TABLE emp`} {
		if _, err := tryRun(s, q); code(err) != "2BP01" {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// several at once is fine when the dependents go too
	run(t, s, `DROP VIEW v3, v2`)
	if _, err := tryRun(s, `SELECT * FROM v2`); code(err) != codeUndefinedTable {
		t.Fatalf("v2 still there: %v", err)
	}
	run(t, s, `CREATE VIEW v2 AS SELECT dept FROM v1`)
	run(t, s, `CREATE VIEW v3 AS SELECT dept FROM v2`)
	r := run(t, s, `DROP VIEW v1 CASCADE`)
	if !strings.Contains(r.Notice, "v2") || !strings.Contains(r.Notice, "v3") {
		t.Fatalf("cascade notice: %q", r.Notice)
	}
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := tryRun(s, `SELECT * FROM `+v); code(err) != codeUndefinedTable {
			t.Fatalf("%s survived the cascade: %v", v, err)
		}
	}
	// DROP TABLE ... CASCADE takes the views along
	run(t, s, `CREATE VIEW a1 AS SELECT id FROM emp`)
	run(t, s, `CREATE VIEW a2 AS SELECT id FROM a1`)
	run(t, s, `DROP TABLE emp CASCADE`)
	if len(e.Views()) != 0 {
		t.Fatalf("views left: %v", e.Views())
	}
	// the names are free again
	run(t, s, `CREATE TABLE a1 (id BIGINT PRIMARY KEY)`)
}

// A transaction sees the view definitions of its snapshot.
func TestSQLViewSnapshot(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	viewSetup(t, a)
	run(t, a, `BEGIN ISOLATION LEVEL REPEATABLE READ`)
	run(t, a, `SELECT count(*) FROM emp`)
	run(t, b, `CREATE VIEW late AS SELECT id FROM emp`)
	if _, err := tryRun(a, `SELECT * FROM late`); code(err) != codeUndefinedTable {
		t.Fatalf("view created after the snapshot: %v", err)
	}
	run(t, a, `ROLLBACK`)
	if got := render(run(t, a, `SELECT count(*) FROM late`)); got != "4" {
		t.Fatalf("new transaction: %s", got)
	}

	run(t, a, `BEGIN ISOLATION LEVEL REPEATABLE READ`)
	run(t, a, `SELECT count(*) FROM emp`)
	run(t, b, `CREATE OR REPLACE VIEW late AS SELECT id FROM emp WHERE id > 2`)
	if got := render(run(t, a, `SELECT count(*) FROM late`)); got != "4" {
		t.Fatalf("replaced after the snapshot: %s", got)
	}
	run(t, b, `DROP VIEW late`)
	if got := render(run(t, a, `SELECT count(*) FROM late`)); got != "4" {
		t.Fatalf("dropped after the snapshot: %s", got)
	}
	run(t, a, `ROLLBACK`)
	if _, err := tryRun(a, `SELECT count(*) FROM late`); code(err) != codeUndefinedTable {
		t.Fatalf("dropped: %v", err)
	}
}

// Views survive a restart: replay of the log alone, then a checkpoint with
// more DDL after it.
func TestSQLViewRecovery(t *testing.T) {
	dir := t.TempDir()
	open := func() *core.Engine {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	defer func() { e.Close() }()
	s := NewSession(e)
	viewSetup(t, s)
	run(t, s, `CREATE VIEW v1 AS SELECT id, dept, sal FROM emp`)
	run(t, s, `CREATE VIEW v2 (d, total) AS SELECT dept, sum(sal) FROM v1 GROUP BY dept`)
	run(t, s, `CREATE VIEW gone AS SELECT 1 AS x`)
	run(t, s, `DROP VIEW gone`)
	run(t, s, `CREATE OR REPLACE VIEW v1 AS SELECT id, dept, sal, name FROM emp`)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	check := func(e *core.Engine, want string) *Session {
		t.Helper()
		s := NewSession(e)
		if got := render(run(t, s, `SELECT d, total FROM v2 ORDER BY d`)); got != want {
			t.Fatalf("v2: %s, want %s", got, want)
		}
		if r := run(t, s, `SELECT * FROM v1`); len(r.Cols) != 4 {
			t.Fatalf("v1 has %d columns", len(r.Cols))
		}
		if _, err := tryRun(s, `SELECT * FROM gone`); code(err) != codeUndefinedTable {
			t.Fatalf("dropped view back: %v", err)
		}
		if _, err := tryRun(s, `DROP TABLE emp`); code(err) != "2BP01" {
			t.Fatalf("dependencies lost: %v", err)
		}
		return s
	}
	// replay
	e = open()
	s = check(e, "eng,300;ops,120")
	run(t, s, `INSERT INTO emp VALUES (9,'zz','eng',1)`)
	// checkpoint, then more DDL after it
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	run(t, s, `CREATE VIEW after_ckpt AS SELECT id FROM emp`)
	run(t, s, `DROP VIEW v2`)
	run(t, s, `CREATE VIEW v2 (d, total) AS SELECT dept, count(*) FROM v1 GROUP BY dept`)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = open()
	s = check(e, "eng,3;ops,2")
	if got := render(run(t, s, `SELECT count(*) FROM after_ckpt`)); got != "5" {
		t.Fatalf("view created after the checkpoint: %s", got)
	}
	// and the checkpoint alone
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open()
	check(e, "eng,3;ops,2")
}

// Views are hidden from CDC and publications, and the DDL does not
// produce changes.
func TestSQLViewCDC(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	viewSetup(t, s)
	run(t, s, `CREATE PUBLICATION p FOR ALL TABLES`)
	run(t, s, `SELECT pg_create_logical_replication_slot('vs', 'test_decoding')`)
	run(t, s, `CREATE VIEW cv AS SELECT id FROM emp`)
	run(t, s, `INSERT INTO emp VALUES (7,'g','eng',1)`)
	run(t, s, `DROP VIEW cv`)
	out := strings.Join(data(run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('vs', NULL, NULL)`)), "\n")
	if strings.Contains(out, "__stask") || strings.Contains(out, "cv") {
		t.Fatalf("view leaked into CDC:\n%s", out)
	}
	if !strings.Contains(out, "table public.emp: INSERT") {
		t.Fatalf("data change missing:\n%s", out)
	}
	if r := run(t, s, `SELECT tablename FROM pg_publication_tables ORDER BY tablename`); strings.Contains(render(r), "__stask") {
		t.Fatalf("view table published: %s", render(r))
	}
}

// Extended protocol: Describe of a statement over a view.
func TestSQLViewDescribe(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	viewSetup(t, s)
	run(t, s, `CREATE VIEW dv AS SELECT id, name FROM emp`)
	st, err := ParseOne(`SELECT name FROM dv WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Describe(st)
	if err != nil || len(d.Cols) != 1 || d.Cols[0].Name != "name" {
		t.Fatalf("describe: %+v %v", d, err)
	}
}
