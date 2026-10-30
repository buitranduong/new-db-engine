package sql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// dbState dumps the catalog (tables, columns, constraints, indexes, views,
// sequences, object catalog) and every user row, without ids, LSNs and
// RelIDs, so two databases can be compared.
func dbState(t *testing.T, s *Session) string {
	t.Helper()
	var out []string
	for _, ts := range s.E.UserTables() {
		d := ts.Schema()
		var cols []string
		for _, c := range d.Columns {
			cols = append(cols, fmt.Sprintf("%s %s nn=%v def=%q id=%s", c.Name, c.Type, c.NotNull, c.Default, c.Identity))
		}
		var ixs []string
		for _, ix := range d.Indexes {
			ixs = append(ixs, fmt.Sprintf("%s(%s)u=%v c=%s i=%v", ix.Name, strings.Join(ix.Columns, ","), ix.Unique, ix.Constraint, ix.Internal))
		}
		var cks []string
		for _, c := range d.Checks {
			cks = append(cks, c.Name+":"+c.Expr)
		}
		var fks []string
		for _, f := range d.ForeignKeys {
			fks = append(fks, fmt.Sprintf("%s(%s)->%s(%s)", f.Name, strings.Join(f.Columns, ","), f.RefTable, strings.Join(f.RefCols, ",")))
		}
		sort.Strings(ixs)
		out = append(out, fmt.Sprintf("TABLE %s cols=[%s] idx=[%s] ck=[%s] fk=[%s]", d.Name, strings.Join(cols, "; "), strings.Join(ixs, "; "), strings.Join(cks, "; "), strings.Join(fks, "; ")))
		if r, err := tryRun(s, `SELECT * FROM `+d.Name+` ORDER BY 1`); err == nil {
			out = append(out, "ROWS "+d.Name+" "+render(r))
		}
	}
	for _, v := range s.E.Views() {
		b, _ := json.Marshal(v)
		out = append(out, "VIEW "+string(b))
	}
	for _, q := range s.E.Sequences() {
		last, called, _ := s.E.SequenceState(q.Name)
		out = append(out, fmt.Sprintf("SEQ %+v last=%d called=%v", q, last, called))
	}
	for _, k := range []string{"enum", "domain", "schema", "ext", "comment"} {
		for _, o := range s.E.Objs(k) {
			out = append(out, fmt.Sprintf("OBJ %s/%s %s", o.Kind, o.Name, o.Data))
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// crashState is dbState without the sequences' last values: a sequence logs
// ahead, so after a crash it resumes past the values it reserved.
func crashState(t *testing.T, s *Session) string {
	t.Helper()
	return seqLast.ReplaceAllString(dbState(t, s), "last=_")
}

var seqLast = regexp.MustCompile(`last=\d+`)

var txdSetup2 = []string{
	`CREATE TABLE p (id BIGINT PRIMARY KEY, name TEXT, qty INT, note TEXT)`,
	`INSERT INTO p VALUES (1,'a',5,'n1'),(2,'b',6,'n2')`,
	`CREATE TABLE ch (id BIGINT PRIMARY KEY, pid BIGINT, v TEXT)`,
	`CREATE INDEX ch_v ON ch (v)`,
	`CREATE TABLE tr (id BIGSERIAL PRIMARY KEY, x TEXT)`,
	`INSERT INTO tr (x) VALUES ('q'),('r')`,
	`CREATE TABLE dz (id BIGSERIAL PRIMARY KEY, z TEXT)`,
	`INSERT INTO dz (z) VALUES ('old1'),('old2')`,
	`CREATE SEQUENCE sq0`,
	`CREATE SEQUENCE sq1`,
	`CREATE TABLE vt (id BIGINT PRIMARY KEY, name TEXT, qty INT)`,
	`CREATE VIEW vp AS SELECT id, name FROM vt`,
	`CREATE VIEW vold AS SELECT id FROM vt`,
	`CREATE TYPE mood AS ENUM ('a','b')`,
	`CREATE DOMAIN pos AS BIGINT`,
	`CREATE SCHEMA sch1`,
	`COMMENT ON TABLE p IS 'ptable'`,
}

// first half: structural changes of tables
var txdScriptA = []string{
	`ALTER TABLE p ADD CONSTRAINT p_chk CHECK (qty > 0)`,
	`ALTER TABLE p ADD CONSTRAINT p_name_key UNIQUE (name)`,
	`ALTER TABLE ch ADD CONSTRAINT ch_fk FOREIGN KEY (pid) REFERENCES p(id)`,
	`ALTER TABLE p RENAME COLUMN note TO memo`,
	`ALTER TABLE p ALTER COLUMN qty SET DEFAULT 7`,
	`ALTER TABLE p ALTER COLUMN memo SET NOT NULL`,
	`ALTER TABLE p ALTER COLUMN qty TYPE BIGINT`,
	`ALTER TABLE p ADD COLUMN extra TEXT`,
	`CREATE TABLE fresh (id BIGINT PRIMARY KEY, w TEXT)`,
	`DROP TABLE dz`,
	`CREATE TABLE dz (id BIGSERIAL PRIMARY KEY, z2 TEXT)`,
	`INSERT INTO dz (z2) VALUES ('new1')`,
	`DROP INDEX ch_v`,
	`ALTER TABLE ch RENAME TO ch2`,
}

// second half: views, sequences, types, schemas, comments, truncate
var txdScriptB = []string{
	`CREATE VIEW vq AS SELECT id FROM vt`,
	`CREATE OR REPLACE VIEW vp AS SELECT id, name, qty FROM vt`,
	`DROP VIEW vold`,
	`CREATE SEQUENCE sq2 START 10`,
	`ALTER SEQUENCE sq1 RESTART WITH 50`,
	`DROP SEQUENCE sq0`,
	`CREATE TYPE mood2 AS ENUM ('x','y')`,
	`DROP TYPE mood`,
	`CREATE DOMAIN pos2 AS INT`,
	`ALTER DOMAIN pos SET DEFAULT 1`,
	`CREATE SCHEMA sch2`,
	`DROP SCHEMA sch1`,
	`COMMENT ON TABLE p IS 'changed'`,
	`COMMENT ON COLUMN p.name IS 'cn'`,
	`TRUNCATE tr RESTART IDENTITY`,
	`INSERT INTO tr (x) VALUES ('after')`,
	`ALTER TABLE p DROP COLUMN extra`,
}

func runAll(t *testing.T, s *Session, qs ...[]string) {
	t.Helper()
	for _, l := range qs {
		for _, q := range l {
			run(t, s, q)
		}
	}
}

func oracle(t *testing.T, scripts ...[]string) string {
	t.Helper()
	s := NewSession(newEngine(t))
	runAll(t, s, txdSetup2)
	runAll(t, s, scripts...)
	return dbState(t, s)
}

func TestTxDDLOracleCommit(t *testing.T) {
	want := oracle(t, txdScriptA, txdScriptB)
	s := NewSession(newEngine(t))
	runAll(t, s, txdSetup2)
	run(t, s, `BEGIN`)
	runAll(t, s, txdScriptA, txdScriptB)
	run(t, s, `COMMIT`)
	if got := dbState(t, s); got != want {
		t.Fatalf("committed state differs from autocommit state\n--- want\n%s\n--- got\n%s", want, got)
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 || hiddenLeft(s) != 0 {
		t.Fatalf("journal rows %d, hidden objects %d", n, hiddenLeft(s))
	}
}

func TestTxDDLOracleRollback(t *testing.T) {
	want := oracle(t)
	s := NewSession(newEngine(t))
	runAll(t, s, txdSetup2)
	run(t, s, `BEGIN`)
	runAll(t, s, txdScriptA, txdScriptB)
	run(t, s, `ROLLBACK`)
	if got := dbState(t, s); got != want {
		t.Fatalf("rolled back state differs\n--- want\n%s\n--- got\n%s", want, got)
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 || hiddenLeft(s) != 0 {
		t.Fatalf("journal rows %d, hidden objects %d", n, hiddenLeft(s))
	}
}

func TestTxDDLOracleCrash(t *testing.T) {
	for _, ckpt := range []bool{false, true} {
		e, dir := openDir(t)
		s := NewSession(e)
		runAll(t, s, txdSetup2)
		want := crashState(t, s)
		run(t, s, `BEGIN`)
		runAll(t, s, txdScriptA, txdScriptB)
		if ckpt {
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		s2 := crashReopen(t, e, dir)
		if got := crashState(t, s2); got != want {
			t.Fatalf("ckpt=%v: state after crash differs\n--- want\n%s\n--- got\n%s", ckpt, want, got)
		}
		if n := len(s2.E.Objs(ddlKind)); n != 0 || hiddenLeft(s2) != 0 {
			t.Fatalf("journal rows %d, hidden objects %d", n, hiddenLeft(s2))
		}
		s2.E.Close()
	}
}

func TestTxDDLOracleCrashAfterCommit(t *testing.T) {
	want := seqLast.ReplaceAllString(oracle(t, txdScriptA, txdScriptB), "last=_")
	for _, ckpt := range []bool{false, true} {
		e, dir := openDir(t)
		s := NewSession(e)
		runAll(t, s, txdSetup2)
		run(t, s, `BEGIN`)
		runAll(t, s, txdScriptA, txdScriptB)
		ddlCrashAfterCommit = func() bool { return true }
		run(t, s, `COMMIT`)
		ddlCrashAfterCommit = nil
		if ckpt {
			if err := e.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		s2 := crashReopen(t, e, dir)
		if got := crashState(t, s2); got != want {
			t.Fatalf("ckpt=%v: state after crash differs\n--- want\n%s\n--- got\n%s", ckpt, want, got)
		}
		if n := len(s2.E.Objs(ddlKind)); n != 0 || hiddenLeft(s2) != 0 {
			t.Fatalf("journal rows %d, hidden objects %d", n, hiddenLeft(s2))
		}
		s2.E.Close()
	}
}

// ROLLBACK TO SAVEPOINT undoes the DDL after it (also what a hidden drop,
// a deferred drop and TRUNCATE did), RELEASE keeps it.
func TestTxDDLSavepoint(t *testing.T) {
	wantAB := oracle(t, txdScriptA, txdScriptB)
	s := NewSession(newEngine(t))
	runAll(t, s, txdSetup2)
	run(t, s, `BEGIN`)
	runAll(t, s, txdScriptA)
	run(t, s, `SAVEPOINT sp`)
	wantA := dbState(t, s) // hidden drops and deferred DROP INDEX are still in it
	runAll(t, s, txdScriptB)
	run(t, s, `ROLLBACK TO sp`)
	if got := dbState(t, s); got != wantA {
		t.Fatalf("after ROLLBACK TO\n--- want\n%s\n--- got\n%s", wantA, got)
	}
	// the savepoint is still there: repeat
	runAll(t, s, txdScriptB)
	run(t, s, `ROLLBACK TO sp`)
	if got := dbState(t, s); got != wantA {
		t.Fatalf("after second ROLLBACK TO\n--- want\n%s\n--- got\n%s", wantA, got)
	}
	// RELEASE keeps the DDL
	runAll(t, s, txdScriptB)
	run(t, s, `RELEASE sp`)
	_, vq, _ := s.E.View(nil, "vq")
	_, vold, _ := s.E.View(nil, "vold")
	if !vq || vold || hasColT(t, s, "p", "extra") {
		t.Fatal("RELEASE lost the DDL after the savepoint")
	}
	run(t, s, `COMMIT`)
	if got := dbState(t, s); got != wantAB {
		t.Fatalf("after COMMIT\n--- want\n%s\n--- got\n%s", wantAB, got)
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 || hiddenLeft(s) != 0 {
		t.Fatalf("journal rows %d, hidden objects %d", n, hiddenLeft(s))
	}
}

// Nested savepoints, a release inside a rolled-back one, and an error that
// is recovered with ROLLBACK TO.
func TestTxDDLSavepointNested(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE keep (id BIGINT PRIMARY KEY)`)
	run(t, s, `BEGIN`)
	run(t, s, `CREATE TABLE t1 (id BIGINT PRIMARY KEY)`)
	run(t, s, `SAVEPOINT a`)
	run(t, s, `CREATE TABLE t2 (id BIGINT PRIMARY KEY)`)
	run(t, s, `SAVEPOINT b`)
	run(t, s, `CREATE TABLE t3 (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE INDEX t3_id ON t3 (id)`)
	run(t, s, `RELEASE b`)
	run(t, s, `DROP TABLE keep`)
	if _, err := tryRun(s, `SELECT * FROM keep`); code(err) != codeUndefinedTable {
		t.Fatalf("dropped table readable: %v", err)
	}
	// a failing statement, then ROLLBACK TO makes the transaction usable
	if _, err := tryRun(s, `ALTER TABLE t1 ADD COLUMN c BIGINT, ADD COLUMN c BIGINT`); err == nil {
		t.Fatal("want duplicate column error")
	}
	if _, err := tryRun(s, `SELECT 1`); code(err) != codeInFailedTx {
		t.Fatalf("want failed transaction: %v", err)
	}
	run(t, s, `ROLLBACK TO a`)
	if hasTable(s, "t2") || hasTable(s, "t3") || !hasTable(s, "t1") || !hasTable(s, "keep") {
		t.Fatalf("tables after ROLLBACK TO a: t1=%v t2=%v t3=%v keep=%v", hasTable(s, "t1"), hasTable(s, "t2"), hasTable(s, "t3"), hasTable(s, "keep"))
	}
	if hasColT(t, s, "t1", "c") {
		t.Fatal("column c of the failed ALTER survived")
	}
	run(t, s, `COMMIT`)
	if !hasTable(s, "t1") || hasTable(s, "t2") || !hasTable(s, "keep") || hasIdx(s, "t3_id") {
		t.Fatal("state after COMMIT wrong")
	}
	if n := len(s.E.Objs(ddlKind)); n != 0 || hiddenLeft(s) != 0 {
		t.Fatalf("journal rows %d, hidden objects %d", n, hiddenLeft(s))
	}
}

// A crash after ROLLBACK TO leaves the journal in step with the database.
func TestTxDDLSavepointCrash(t *testing.T) {
	e, dir := openDir(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE gone (id BIGINT PRIMARY KEY)`)
	want := crashState(t, s)
	run(t, s, `BEGIN`)
	run(t, s, `CREATE TABLE t1 (id BIGINT PRIMARY KEY)`)
	run(t, s, `SAVEPOINT a`)
	run(t, s, `CREATE TABLE t2 (id BIGINT PRIMARY KEY)`)
	run(t, s, `DROP TABLE gone`)
	run(t, s, `ROLLBACK TO a`)
	s2 := crashReopen(t, e, dir)
	if got := crashState(t, s2); got != want {
		t.Fatalf("state after crash\n--- want\n%s\n--- got\n%s", want, got)
	}
	if n := len(s2.E.Objs(ddlKind)); n != 0 || hiddenLeft(s2) != 0 {
		t.Fatalf("journal rows %d, hidden objects %d", n, hiddenLeft(s2))
	}
	s2.E.Close()
}

// DROP TABLE a; CREATE TABLE a in one transaction.
func TestTxDDLDropCreateSameName(t *testing.T) {
	setup := func(s *Session) {
		run(t, s, `CREATE TABLE a (id BIGSERIAL PRIMARY KEY, v TEXT)`)
		run(t, s, `CREATE INDEX a_v ON a (v)`)
		run(t, s, `INSERT INTO a (v) VALUES ('old1'),('old2')`)
	}
	t.Run("commit", func(t *testing.T) {
		s := NewSession(newEngine(t))
		setup(s)
		run(t, s, `BEGIN`)
		run(t, s, `DROP TABLE a`)
		if _, err := tryRun(s, `SELECT * FROM a`); code(err) != codeUndefinedTable {
			t.Fatalf("dropped table readable: %v", err)
		}
		run(t, s, `ROLLBACK`)
		run(t, s, `BEGIN`)
		run(t, s, `DROP TABLE a`)
		if _, err := tryRun(s, `DROP TABLE a`); code(err) != codeUndefinedTable {
			t.Fatalf("second DROP: %v", err)
		}
		run(t, s, `ROLLBACK`)
		run(t, s, `BEGIN`)
		run(t, s, `DROP TABLE a`)
		run(t, s, `CREATE TABLE a (id BIGSERIAL PRIMARY KEY, w TEXT)`)
		run(t, s, `CREATE INDEX a_v ON a (w)`)
		run(t, s, `INSERT INTO a (w) VALUES ('new1')`)
		if r := run(t, s, `SELECT id, w FROM a`); render(r) != "1,new1" {
			t.Fatalf("new table: %s", render(r))
		}
		run(t, s, `COMMIT`)
		if r := run(t, s, `SELECT id, w FROM a`); render(r) != "1,new1" {
			t.Fatalf("after COMMIT: %s", render(r))
		}
		if hiddenLeft(s) != 0 || len(s.E.Objs(ddlKind)) != 0 {
			t.Fatalf("leftovers: hidden %d journal %d", hiddenLeft(s), len(s.E.Objs(ddlKind)))
		}
		run(t, s, `INSERT INTO a (w) VALUES ('new2')`)
		if r := run(t, s, `SELECT count(*) FROM a`); r.Rows[0][0].I != 2 {
			t.Fatalf("count: %s", render(r))
		}
	})
	t.Run("rollback", func(t *testing.T) {
		s := NewSession(newEngine(t))
		setup(s)
		want := dbState(t, s)
		run(t, s, `BEGIN`)
		run(t, s, `DROP TABLE a`)
		run(t, s, `CREATE TABLE a (id BIGSERIAL PRIMARY KEY, w TEXT)`)
		run(t, s, `INSERT INTO a (w) VALUES ('new1')`)
		run(t, s, `ROLLBACK`)
		if got := dbState(t, s); got != want {
			t.Fatalf("--- want\n%s\n--- got\n%s", want, got)
		}
		run(t, s, `INSERT INTO a (v) VALUES ('old3')`)
		if r := run(t, s, `SELECT max(id) FROM a`); r.Rows[0][0].I != 3 {
			t.Fatalf("sequence of the restored table: %s", render(r))
		}
	})
	t.Run("crash", func(t *testing.T) {
		for _, after := range []bool{false, true} {
			e, dir := openDir(t)
			s := NewSession(e)
			setup(s)
			before := crashState(t, s)
			run(t, s, `BEGIN`)
			run(t, s, `DROP TABLE a`)
			run(t, s, `CREATE TABLE a (id BIGSERIAL PRIMARY KEY, w TEXT)`)
			run(t, s, `INSERT INTO a (w) VALUES ('new1')`)
			if after {
				ddlCrashAfterCommit = func() bool { return true }
				run(t, s, `COMMIT`)
				ddlCrashAfterCommit = nil
			}
			s2 := crashReopen(t, e, dir)
			if after {
				if r := run(t, s2, `SELECT id, w FROM a`); render(r) != "1,new1" {
					t.Fatalf("after-commit crash: %s", render(r))
				}
			} else if got := crashState(t, s2); got != before {
				t.Fatalf("before-commit crash\n--- want\n%s\n--- got\n%s", before, got)
			}
			if hiddenLeft(s2) != 0 || len(s2.E.Objs(ddlKind)) != 0 {
				t.Fatalf("leftovers: hidden %d journal %d", hiddenLeft(s2), len(s2.E.Objs(ddlKind)))
			}
			s2.E.Close()
		}
	})
}

// A dropped table cannot gain dependents from other sessions (it is gone
// by name); the transaction's own DROP of an object with rows written in the
// transaction falls back to a deferred drop.
func TestTxDDLDropNoDependents(t *testing.T) {
	e := newEngine(t)
	s1, s2 := NewSession(e), NewSession(e)
	run(t, s1, `CREATE TABLE a (id BIGINT PRIMARY KEY)`)
	run(t, s1, `BEGIN`)
	run(t, s1, `DROP TABLE a`)
	if _, err := tryRun(s2, `CREATE VIEW va AS SELECT * FROM a`); err == nil {
		t.Fatal("a view on a dropped table was created")
	}
	run(t, s1, `COMMIT`)
	if hasTable(s1, "a") {
		t.Fatal("a survived")
	}
	// rows written by the transaction: deferred drop, table visible until COMMIT
	run(t, s1, `CREATE TABLE b (id BIGINT PRIMARY KEY)`)
	run(t, s1, `BEGIN`)
	run(t, s1, `INSERT INTO b VALUES (1)`)
	run(t, s1, `DROP TABLE b`)
	if _, err := tryRun(s1, `CREATE TABLE b (id BIGINT)`); code(err) != codeDuplicateTable {
		t.Fatalf("deferred drop, same name: %v", err)
	}
	run(t, s1, `ROLLBACK`)
	run(t, s1, `BEGIN`)
	run(t, s1, `INSERT INTO b VALUES (2)`)
	run(t, s1, `DROP TABLE b`)
	run(t, s1, `COMMIT`)
	if hasTable(s1, "b") {
		t.Fatal("deferred drop did not happen at COMMIT")
	}
}

// A redo step that fails after the transaction is durable: COMMIT reports a
// warning, the journal stays, and the next start finishes the job.
func TestTxDDLRedoFailure(t *testing.T) {
	e, dir := openDir(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE a (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE TABLE keep (id BIGINT PRIMARY KEY)`)
	run(t, s, `BEGIN`)
	run(t, s, `DROP TABLE a`)
	run(t, s, `INSERT INTO keep VALUES (1)`)
	ddlRedoFault = func(st ddlStep) error { return fmt.Errorf("injected failure in %s", st.Op) }
	res, err := tryRun(s, `COMMIT`)
	ddlRedoFault = nil
	if err != nil {
		t.Fatalf("a durable commit must not fail: %v", err)
	}
	if !strings.Contains(res.Notice, "WARNING") || !strings.Contains(res.Notice, "injected failure") {
		t.Fatalf("COMMIT hid the failure: notice %q", res.Notice)
	}
	if r := run(t, s, `SELECT count(*) FROM keep`); r.Rows[0][0].I != 1 {
		t.Fatal("the transaction's rows are not durable")
	}
	if n := len(s.E.Objs(ddlKind)); n != 2 {
		t.Fatalf("journal rows kept: %d, want 2", n)
	}
	s2 := crashReopen(t, e, dir) // restart: the redo runs
	if hiddenLeft(s2) != 0 || hasTable(s2, "a") || len(s2.E.Objs(ddlKind)) != 0 {
		t.Fatalf("restart did not finish the drop: hidden %d a=%v journal %d", hiddenLeft(s2), hasTable(s2, "a"), len(s2.E.Objs(ddlKind)))
	}
	s2.E.Close()
}

// A deferred drop (DROP TABLE .. CASCADE of a referenced table) wins over a
// view another session created meanwhile; the notice says what went with it.
func TestTxDDLRedoCascades(t *testing.T) {
	e := newEngine(t)
	s1, s2 := NewSession(e), NewSession(e)
	run(t, s1, `CREATE TABLE par (id BIGINT PRIMARY KEY)`)
	run(t, s1, `CREATE TABLE chi (id BIGINT PRIMARY KEY, p BIGINT REFERENCES par(id))`)
	run(t, s1, `BEGIN`)
	run(t, s1, `DROP TABLE par CASCADE`)
	run(t, s2, `CREATE VIEW vpar AS SELECT * FROM par`) // par is still visible: deferred
	res, err := tryRun(s1, `COMMIT`)
	if err != nil {
		t.Fatal(err)
	}
	if hasTable(s1, "par") || s1.isView("vpar") {
		t.Fatal("par or its view survived the committed drop")
	}
	if !strings.Contains(res.Notice, "vpar") {
		t.Fatalf("notice does not name the dropped view: %q", res.Notice)
	}
	if len(e.Objs(ddlKind)) != 0 {
		t.Fatal("journal rows left")
	}
}

// The undo of CREATE TABLE drops only the table it created, even if the
// name was taken meanwhile or the CREATE never happened.
func TestTxDDLCreateUndoIdentity(t *testing.T) {
	e := newEngine(t)
	s1, s2 := NewSession(e), NewSession(e)
	run(t, s1, `BEGIN`)
	run(t, s1, `CREATE TABLE mine (id BIGINT PRIMARY KEY)`)
	ts, _ := e.Table("mine")
	if ts.Schema().OID() < 0x40000000 {
		t.Fatalf("table not marked: oid %d", ts.Schema().OID())
	}
	run(t, s1, `ROLLBACK`)
	if hasTable(s1, "mine") {
		t.Fatal("rollback left the table")
	}
	// the CREATE failed because s2 got the name first: the undo step stays
	// journaled (written before the CREATE) but must not drop s2's table
	run(t, s1, `BEGIN`)
	if _, err := s1.djUndo(ddlStep{Op: "drop_table", Table: "taken", Rel: 0x41234567}); err != nil {
		t.Fatal(err)
	}
	run(t, s2, `CREATE TABLE taken (id BIGINT PRIMARY KEY)`)
	run(t, s1, `ROLLBACK`)
	if !hasTable(s1, "taken") {
		t.Fatal("the undo dropped a table it did not create")
	}
	// a crash between the journal write and the CREATE
	e2, dir := openDir(t)
	s3 := NewSession(e2)
	run(t, s3, `BEGIN`)
	if _, err := s3.djUndo(ddlStep{Op: "drop_table", Table: "later", Rel: 0x41234568}); err != nil {
		t.Fatal(err)
	}
	other := NewSession(e2)
	run(t, other, `CREATE TABLE later (id BIGINT PRIMARY KEY)`)
	s4 := crashReopen(t, e2, dir)
	if !hasTable(s4, "later") {
		t.Fatal("recovery dropped a table created by another session")
	}
	if len(s4.E.Objs(ddlKind)) != 0 {
		t.Fatal("journal rows left")
	}
	s4.E.Close()
}

// A view created in the transaction is readable in it (views are applied at
// once, outside the snapshot), and a dropped one is gone.
func TestTxDDLViewVisibleInTx(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE k (id BIGINT PRIMARY KEY, a TEXT)`)
	run(t, s, `INSERT INTO k VALUES (1,'x')`)
	run(t, s, `CREATE VIEW old AS SELECT id FROM k`)
	run(t, s, `BEGIN`)
	run(t, s, `CREATE VIEW kv AS SELECT id, a FROM k`)
	if r := run(t, s, `SELECT a FROM kv`); render(r) != "x" {
		t.Fatalf("view in tx: %s", render(r))
	}
	run(t, s, `DROP VIEW old`)
	if _, err := tryRun(s, `SELECT * FROM old`); code(err) != codeUndefinedTable {
		t.Fatalf("dropped view readable: %v", err)
	}
	run(t, s, `ROLLBACK`)
	_, kv, _ := s.E.View(nil, "kv")
	_, old, _ := s.E.View(nil, "old")
	if kv || !old {
		t.Fatal("views after ROLLBACK wrong")
	}
	if r := run(t, s, `SELECT id FROM old`); render(r) != "1" {
		t.Fatalf("restored view: %s", render(r))
	}
}

// TRUNCATE in a transaction: rows, identity, referencing tables, rollback.
func TestTxDDLTruncate(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE par (id BIGSERIAL PRIMARY KEY, n TEXT)`)
	run(t, s, `CREATE TABLE kid (id BIGINT PRIMARY KEY, p BIGINT REFERENCES par(id))`)
	run(t, s, `INSERT INTO par (n) VALUES ('a'),('b')`)
	run(t, s, `INSERT INTO kid VALUES (10,1),(11,2)`)
	want := dbState(t, s)
	run(t, s, `BEGIN`)
	if _, err := tryRun(s, `TRUNCATE par`); err == nil {
		t.Fatal("TRUNCATE of a referenced table must fail without CASCADE")
	}
	run(t, s, `ROLLBACK`)
	run(t, s, `BEGIN`)
	run(t, s, `TRUNCATE par RESTART IDENTITY CASCADE`)
	if r := run(t, s, `SELECT count(*) FROM par`); r.Rows[0][0].I != 0 {
		t.Fatal("par not empty in the transaction")
	}
	if r := run(t, s, `SELECT count(*) FROM kid`); r.Rows[0][0].I != 0 {
		t.Fatal("kid not emptied by CASCADE")
	}
	run(t, s, `INSERT INTO par (n) VALUES ('z')`)
	if r := run(t, s, `SELECT id FROM par`); render(r) != "1" {
		t.Fatalf("identity not restarted: %s", render(r))
	}
	run(t, s, `ROLLBACK`)
	if got := dbState(t, s); got != want {
		t.Fatalf("--- want\n%s\n--- got\n%s", want, got)
	}
	run(t, s, `BEGIN`)
	run(t, s, `TRUNCATE par, kid`)
	run(t, s, `COMMIT`)
	if r := run(t, s, `SELECT count(*) FROM par`); r.Rows[0][0].I != 0 {
		t.Fatal("committed TRUNCATE left rows")
	}
}

// ALTER COLUMN TYPE with a conversion is undone: old rows read as before.
func TestTxDDLAlterTypeRollback(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE ty (id BIGINT PRIMARY KEY, n INT, d TEXT DEFAULT 'x')`)
	run(t, s, `INSERT INTO ty VALUES (1, 5, 'a'),(2, 6, 'b')`)
	want := dbState(t, s)
	run(t, s, `BEGIN`)
	run(t, s, `ALTER TABLE ty ALTER COLUMN n TYPE TEXT USING n::text`)
	run(t, s, `ALTER TABLE ty ALTER COLUMN d DROP DEFAULT`)
	run(t, s, `ALTER TABLE ty ALTER COLUMN d SET NOT NULL`)
	if r := run(t, s, `SELECT n FROM ty ORDER BY id`); render(r) != "5;6" {
		t.Fatalf("converted: %s", render(r))
	}
	run(t, s, `ROLLBACK`)
	if got := dbState(t, s); got != want {
		t.Fatalf("--- want\n%s\n--- got\n%s", want, got)
	}
	run(t, s, `INSERT INTO ty (id, n) VALUES (3, 7)`)
	if r := run(t, s, `SELECT n + 1, d FROM ty WHERE id = 3`); render(r) != "8,x" {
		t.Fatalf("after rollback: %s", render(r))
	}
}

// A hidden table gets its indexes back, built again, on ROLLBACK.
func TestTxDDLHiddenTableIndexes(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE ix (id BIGINT PRIMARY KEY, u TEXT, v TEXT)`)
	run(t, s, `CREATE UNIQUE INDEX ix_u ON ix (u)`)
	run(t, s, `CREATE INDEX ix_v ON ix (v)`)
	run(t, s, `INSERT INTO ix VALUES (1,'a','x'),(2,'b','x')`)
	run(t, s, `BEGIN`)
	run(t, s, `DROP TABLE ix`)
	run(t, s, `CREATE TABLE ix (id BIGINT PRIMARY KEY, u TEXT)`)
	run(t, s, `CREATE UNIQUE INDEX ix_u ON ix (u)`) // the name is free
	run(t, s, `ROLLBACK`)
	if !hasIdx(s, "ix_u") || !hasIdx(s, "ix_v") {
		t.Fatal("indexes of the restored table are missing")
	}
	if _, err := tryRun(s, `INSERT INTO ix VALUES (3,'a','y')`); code(err) != codeUniqueViolation {
		t.Fatalf("unique index not rebuilt: %v", err)
	}
}
