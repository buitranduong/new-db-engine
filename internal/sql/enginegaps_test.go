package sql

import (
	"testing"

	"stask/internal/core"
	"stask/internal/schema"
)

func one(t *testing.T, s *Session, q string) string {
	t.Helper()
	return render(run(t, s, q))
}

// A transaction that wrote rows and then ALTERs the table commits, and the
// rows it wrote read as if written after the ALTER.
func TestGapsWriteThenAlter(t *testing.T) {
	cases := []struct{ alter, probe, want string }{
		{`ALTER TABLE g ADD COLUMN c INT DEFAULT 5`, `SELECT sum(c) FROM g`, "10"},
		{`ALTER TABLE g RENAME COLUMN v TO w`, `SELECT string_agg(w, ',' ORDER BY id) FROM g`, "b,c"},
		{`ALTER TABLE g ADD COLUMN c TEXT`, `SELECT count(*) - count(c) FROM g`, "2"},
		{`ALTER TABLE g ALTER COLUMN n TYPE TEXT`, `SELECT string_agg(n, ',' ORDER BY id) FROM g`, "20,30"},
		{`ALTER TABLE g DROP COLUMN v`, `SELECT sum(n) FROM g`, "50"},
		{`CREATE INDEX g_v ON g (v)`, `SELECT count(*) FROM g WHERE v = 'b'`, "1"},
		{`ALTER TABLE g ADD CONSTRAINT g_u UNIQUE (v)`, `SELECT count(*) FROM g WHERE v = 'c'`, "1"},
	}
	for _, a := range cases {
		s := NewSession(newEngine(t))
		run(t, s, `CREATE TABLE g (id INT PRIMARY KEY, v TEXT, n INT)`)
		run(t, s, `INSERT INTO g VALUES (1,'a',10)`)
		run(t, s, `BEGIN`)
		run(t, s, `INSERT INTO g VALUES (2,'b',20)`)
		run(t, s, `UPDATE g SET v = 'z' WHERE id = 1`)
		run(t, s, `DELETE FROM g WHERE id = 1`)
		run(t, s, `INSERT INTO g VALUES (3,'c',30)`)
		run(t, s, a.alter)
		if got := one(t, s, a.probe); got != a.want {
			t.Fatalf("%s: in tx %q want %q", a.alter, got, a.want)
		}
		run(t, s, `COMMIT`)
		if got := one(t, s, a.probe); got != a.want {
			t.Fatalf("%s: after commit %q want %q", a.alter, got, a.want)
		}
	}
}

// Unique violations against the new index are still detected.
func TestGapsAlterUniqueViolation(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE g (id INT PRIMARY KEY, v TEXT)`)
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO g VALUES (1,'a'),(2,'a')`)
	_, err := tryRun(s, `ALTER TABLE g ADD CONSTRAINT g_u UNIQUE (v)`)
	if err == nil {
		_, err = tryRun(s, `COMMIT`)
	}
	if err == nil {
		t.Fatal("duplicates under a new unique constraint were committed")
	}
	t.Log(err)
	tryRun(s, `ROLLBACK`)
	if got := one(t, s, `SELECT count(*) FROM g`); got != "0" {
		t.Fatal(got)
	}
}

func TestGapsRollbackToAfterAlter(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE g (id INT PRIMARY KEY, v TEXT)`)
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO g VALUES (1,'a')`)
	run(t, s, `SAVEPOINT sp`)
	run(t, s, `INSERT INTO g VALUES (2,'b')`)
	run(t, s, `ALTER TABLE g ADD COLUMN c INT DEFAULT 9`)
	run(t, s, `INSERT INTO g VALUES (3,'c',4)`)
	run(t, s, `ROLLBACK TO sp`)
	run(t, s, `INSERT INTO g VALUES (4,'d')`)
	run(t, s, `COMMIT`)
	if got := one(t, s, `SELECT string_agg(id::text || v, ',' ORDER BY id) FROM g`); got != "1a,4d" {
		t.Fatal(got)
	}
	// ALTER before the savepoint, rollback to it
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO g VALUES (5,'e')`)
	run(t, s, `SAVEPOINT s2`)
	run(t, s, `ALTER TABLE g ADD COLUMN c INT DEFAULT 9`)
	run(t, s, `INSERT INTO g VALUES (6,'f',1)`)
	run(t, s, `RELEASE s2`)
	run(t, s, `COMMIT`)
	if got := one(t, s, `SELECT string_agg(id::text || ':' || c::text, ',' ORDER BY id) FROM g`); got != "1:9,4:9,5:9,6:1" {
		t.Fatal(got)
	}
}

func TestGapsReadCommitted(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	run(t, a, `CREATE TABLE rc (id INT PRIMARY KEY, v INT)`)
	run(t, a, `INSERT INTO rc VALUES (1, 10), (2, 20)`)
	if got := one(t, a, `SHOW transaction_isolation`); got != "read committed" {
		t.Fatal(got)
	}
	// default level: a statement sees what committed since the previous one
	run(t, a, `BEGIN`)
	if got := one(t, a, `SHOW transaction_isolation`); got != "read committed" {
		t.Fatal(got)
	}
	if got := one(t, a, `SELECT sum(v) FROM rc`); got != "30" {
		t.Fatal(got)
	}
	run(t, b, `UPDATE rc SET v = 11 WHERE id = 1`)
	run(t, b, `INSERT INTO rc VALUES (3, 30)`)
	if got := one(t, a, `SELECT sum(v) FROM rc`); got != "61" {
		t.Fatalf("read committed statement did not see committed rows: %s", got)
	}
	// own writes stay visible; a row written by this transaction that
	// another session changed since fails the commit
	run(t, a, `UPDATE rc SET v = 99 WHERE id = 2`)
	run(t, b, `INSERT INTO rc VALUES (2, 21) ON CONFLICT (id) DO UPDATE SET v = excluded.v`) // an UPDATE would wait for a
	if _, err := tryRun(a, `COMMIT`); err == nil {
		t.Fatal("lost update committed")
	}
	// a write after the other session committed (new statement snapshot) succeeds
	run(t, a, `BEGIN`)
	run(t, b, `UPDATE rc SET v = 22 WHERE id = 2`)
	run(t, a, `UPDATE rc SET v = v + 1 WHERE id = 2`)
	run(t, a, `COMMIT`)
	if got := one(t, a, `SELECT v FROM rc WHERE id = 2`); got != "23" {
		t.Fatal(got)
	}
	// repeatable read keeps the transaction snapshot
	run(t, a, `BEGIN ISOLATION LEVEL REPEATABLE READ`)
	if got := one(t, a, `SHOW transaction_isolation`); got != "repeatable read" {
		t.Fatal(got)
	}
	run(t, a, `SELECT 1`)
	run(t, b, `UPDATE rc SET v = 0 WHERE id = 1`)
	if got := one(t, a, `SELECT v FROM rc WHERE id = 1`); got != "11" {
		t.Fatalf("repeatable read saw a later commit: %s", got)
	}
	run(t, a, `ROLLBACK`)
	run(t, a, `BEGIN`)
	run(t, a, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`)
	run(t, a, `SELECT 1`)
	run(t, b, `UPDATE rc SET v = 5 WHERE id = 1`)
	if got := one(t, a, `SELECT v FROM rc WHERE id = 1`); got != "0" {
		t.Fatalf("SET TRANSACTION repeatable read: %s", got)
	}
	run(t, a, `ROLLBACK`)
	run(t, a, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL REPEATABLE READ`)
	if got := one(t, a, `SHOW transaction_isolation`); got != "repeatable read" {
		t.Fatal(got)
	}
}

func TestGapsDMLAliasAndValuesSubquery(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE a (id INT PRIMARY KEY, v TEXT)`)
	run(t, s, `CREATE TABLE b (id INT PRIMARY KEY, v TEXT, w INT)`)
	run(t, s, `INSERT INTO a VALUES (1,'a'),(2,'b')`)
	run(t, s, `UPDATE a x SET v = 'z' WHERE x.id = 1`)
	run(t, s, `UPDATE a AS x SET v = x.v || '!' WHERE x.id = 1 RETURNING x.id`)
	run(t, s, `DELETE FROM a y WHERE y.id = 2`)
	if got := one(t, s, `SELECT string_agg(id::text || v, ',') FROM a`); got != "1z!" {
		t.Fatal(got)
	}
	run(t, s, `INSERT INTO b VALUES (1, (SELECT v FROM a WHERE id = 1), (SELECT count(*) FROM a))`)
	run(t, s, `INSERT INTO b VALUES (2, (SELECT v FROM a WHERE id = 99), 1 + (SELECT max(id) FROM a)), (3, 'k', 5)`)
	if got := one(t, s, `SELECT string_agg(id::text || ':' || coalesce(v,'~') || ':' || w, ',' ORDER BY id) FROM b`); got != "1:z!:1,2:~:2,3:k:5" {
		t.Fatal(got)
	}
	if _, err := tryRun(s, `INSERT INTO b VALUES (4, (SELECT v FROM a UNION ALL SELECT v FROM a), 1)`); err == nil {
		t.Fatal("expected 21000")
	} else if e := AsError(err); e.Code != "21000" {
		t.Fatal(e.Code, e.Msg)
	}
	// ON CONFLICT DO UPDATE SET with a subquery: a proper error or a value, never XX000
	_, err := tryRun(s, `INSERT INTO b VALUES (1,'q',1) ON CONFLICT (id) DO UPDATE SET w = (SELECT count(*) FROM a)`)
	if err != nil && AsError(err).Code == "XX000" {
		t.Fatal(err)
	}
}

func TestGapsReturningSubquery(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE a (id INT PRIMARY KEY, n INT)`)
	run(t, s, `INSERT INTO a VALUES (1,10),(2,20)`)
	if got := one(t, s, `INSERT INTO a VALUES (3,30) RETURNING id, (SELECT count(*) FROM a) AS c`); got != "3,2" {
		t.Fatal(got)
	}
	if got := one(t, s, `UPDATE a SET n = 0 WHERE id = 1 RETURNING (SELECT max(n) FROM a)`); got != "30" {
		t.Fatal(got)
	}
	if got := one(t, s, `DELETE FROM a WHERE id = 3 RETURNING id, (SELECT 7)`); got != "3,7" {
		t.Fatal(got)
	}
	_, err := tryRun(s, `UPDATE a SET n = 1 WHERE id = 2 RETURNING (SELECT x.n FROM a x WHERE x.id = a.id)`)
	if err != nil && AsError(err).Code == "XX000" {
		t.Fatal(err)
	}
}

func TestGapsSerializableScan(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	run(t, a, `CREATE TABLE sc (id INT PRIMARY KEY, v INT)`)
	run(t, a, `CREATE TABLE other (id INT PRIMARY KEY)`)
	run(t, a, `INSERT INTO sc VALUES (1, 10), (2, 20)`)
	// a scan works; nobody wrote the table: commits
	run(t, a, `BEGIN ISOLATION LEVEL SERIALIZABLE`)
	if got := one(t, a, `SELECT sum(v) FROM sc`); got != "30" {
		t.Fatal(got)
	}
	run(t, a, `INSERT INTO other VALUES (1)`)
	run(t, a, `COMMIT`)
	// write skew through a scan: both read the sum, each writes a row
	run(t, a, `BEGIN ISOLATION LEVEL SERIALIZABLE`)
	run(t, b, `BEGIN ISOLATION LEVEL SERIALIZABLE`)
	one(t, a, `SELECT sum(v) FROM sc`)
	one(t, b, `SELECT sum(v) FROM sc`)
	run(t, a, `INSERT INTO sc VALUES (3, 1)`)
	run(t, b, `INSERT INTO sc VALUES (4, 1)`)
	run(t, a, `COMMIT`)
	_, err := tryRun(b, `COMMIT`)
	if err == nil || AsError(err).Code != "40001" {
		t.Fatalf("write skew committed: %v", err)
	}
	// phantom: a scan, another session inserts a row, the scanner writes elsewhere
	run(t, a, `BEGIN ISOLATION LEVEL SERIALIZABLE`)
	one(t, a, `SELECT count(*) FROM sc WHERE v > 0`)
	run(t, b, `INSERT INTO sc VALUES (9, 9)`)
	run(t, a, `INSERT INTO other VALUES (2)`)
	if _, err := tryRun(a, `COMMIT`); err == nil || AsError(err).Code != "40001" {
		t.Fatalf("phantom committed: %v", err)
	}
	// a read-only serializable transaction is not failed by later writes
	run(t, a, `BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY`)
	one(t, a, `SELECT count(*) FROM sc`)
	run(t, b, `INSERT INTO sc VALUES (10, 9)`)
	run(t, a, `COMMIT`)
}

func TestGapsCodedEngineErrors(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ce (id INT PRIMARY KEY, v TEXT)`)
	run(t, s, `CREATE INDEX ce_v ON ce (v)`)
	if err := e.CreateIndex("ce", schema.IndexDef{Name: "ce_v", Columns: []string{"v"}}); err == nil || AsError(err).Code != "42P07" {
		t.Fatalf("duplicate index: %v", err)
	}
	if err := e.DropIndex("ce", "nope"); err == nil || AsError(err).Code != "42704" {
		t.Fatalf("missing index: %v", err)
	}
	if AsError(core.ErrClosed).Code != "57P01" {
		t.Fatal("ErrClosed")
	}
}
