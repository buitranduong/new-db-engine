package sql

import "testing"

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
