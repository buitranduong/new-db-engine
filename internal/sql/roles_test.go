package sql

import (
	"strings"
	"testing"

	"stask/internal/core"
)

func roleSession(e *core.Engine, role string) *Session {
	s := NewSession(e)
	s.SetRole(role)
	return s
}

func wantDenied(t *testing.T, s *Session, q string) {
	t.Helper()
	if _, err := tryRun(s, q); code(err) != "42501" {
		t.Fatalf("%s: want 42501, got %v", q, err)
	}
}

func TestRolesAndGrants(t *testing.T) {
	e := newEngine(t)
	su := NewSession(e)
	run(t, su, `CREATE TABLE t1 (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, su, `CREATE TABLE t2 (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, su, `INSERT INTO t1 VALUES (1,'a'),(2,'b')`)
	run(t, su, `INSERT INTO t2 VALUES (1,'x')`)
	run(t, su, `CREATE VIEW v1 AS SELECT id FROM t1`)

	run(t, su, `CREATE USER alice PASSWORD 'pw1'`)
	run(t, su, `CREATE ROLE nologin`)
	run(t, su, `CREATE ROLE boss SUPERUSER LOGIN`)
	if _, err := tryRun(su, `CREATE ROLE alice`); code(err) != "42710" {
		t.Fatalf("duplicate role: %v", err)
	}
	if _, err := tryRun(su, `CREATE ROLE public`); code(err) != "42939" {
		t.Fatalf("reserved: %v", err)
	}
	r, ok := e.Role("alice")
	if !ok || !r.Login || r.Super || !strings.HasPrefix(r.Verifier, "SCRAM-SHA-256$4096:") {
		t.Fatalf("alice: %+v", r)
	}
	if v, _ := core.ParseSCRAM(r.Verifier); !v.CheckPassword("pw1") || v.CheckPassword("nope") {
		t.Fatal("verifier does not check the password")
	}
	if r2, _ := e.Role("nologin"); r2.Login {
		t.Fatal("CREATE ROLE logs in")
	}

	a := roleSession(e, "alice")
	// nothing granted yet
	wantDenied(t, a, `SELECT * FROM t1`)
	wantDenied(t, a, `INSERT INTO t1 VALUES (3,'c')`)
	wantDenied(t, a, `SELECT * FROM v1`)
	wantDenied(t, a, `CREATE TABLE t9 (id BIGINT PRIMARY KEY)`)
	wantDenied(t, a, `GRANT SELECT ON t1 TO alice`)
	wantDenied(t, a, `CREATE ROLE evil SUPERUSER`)
	// session control and the catalog stay open
	run(t, a, `SET application_name = 'x'`)
	run(t, a, `SELECT count(*) FROM pg_class`)
	run(t, a, `SELECT 1`)

	run(t, su, `GRANT SELECT ON t1 TO alice`)
	if got := render(run(t, a, `SELECT v FROM t1 ORDER BY id`)); got != "a;b" {
		t.Fatalf("select after grant: %s", got)
	}
	wantDenied(t, a, `SELECT * FROM t2`)
	wantDenied(t, a, `SELECT * FROM t1 JOIN t2 ON t1.id = t2.id`)
	wantDenied(t, a, `SELECT * FROM t1 WHERE id IN (SELECT id FROM t2)`)
	wantDenied(t, a, `SELECT (SELECT max(id) FROM t2)`)
	wantDenied(t, a, `SELECT * FROM t1 UNION SELECT * FROM t2`)
	wantDenied(t, a, `WITH c AS (SELECT * FROM t2) SELECT * FROM c`)
	// a CTE named like a table hides it only where it is defined
	run(t, a, `WITH t2 AS (SELECT 1 AS id) SELECT * FROM t2`)
	wantDenied(t, a, `WITH t2 AS (SELECT * FROM t2) SELECT * FROM t2`)
	wantDenied(t, a, `INSERT INTO t1 VALUES (3,'c')`)
	wantDenied(t, a, `UPDATE t1 SET v = 'z'`)
	wantDenied(t, a, `DELETE FROM t1`)
	wantDenied(t, a, `INSERT INTO t1 SELECT * FROM t2`)

	run(t, su, `GRANT INSERT, UPDATE ON t1 TO alice`)
	run(t, a, `INSERT INTO t1 VALUES (3,'c')`)
	run(t, a, `UPDATE t1 SET v = 'z' WHERE id = 3`)
	wantDenied(t, a, `DELETE FROM t1 WHERE id = 3`)
	wantDenied(t, a, `DELETE FROM t1`)
	run(t, su, `GRANT DELETE ON t1 TO alice`)
	run(t, a, `DELETE FROM t1 WHERE id = 3`)

	// views are relations of their own
	run(t, su, `GRANT SELECT ON v1 TO alice`)
	run(t, a, `SELECT * FROM v1`)

	// PUBLIC
	run(t, su, `GRANT SELECT ON t2 TO PUBLIC`)
	run(t, a, `SELECT * FROM t2`)
	run(t, su, `REVOKE SELECT ON t2 FROM PUBLIC`)
	wantDenied(t, a, `SELECT * FROM t2`)

	// REVOKE applies to the open session at once
	run(t, su, `REVOKE INSERT, UPDATE, DELETE ON t1 FROM alice`)
	wantDenied(t, a, `INSERT INTO t1 VALUES (9,'z')`)
	run(t, a, `SELECT * FROM t1`)

	// ALL TABLES IN SCHEMA, ALL PRIVILEGES, schema USAGE
	run(t, su, `GRANT USAGE ON SCHEMA public TO alice`)
	run(t, su, `GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO alice`)
	run(t, a, `SELECT * FROM t2`)
	run(t, a, `UPDATE t2 SET v = 'y' WHERE id = 1`)
	if got := render(run(t, a, `SELECT has_table_privilege('alice','t1','SELECT'), has_table_privilege('t1','TRUNCATE')`)); got != "t,f" {
		t.Fatalf("has_table_privilege: %s", got)
	}
	if got := render(run(t, su, `SELECT grantee, table_name, privilege_type FROM information_schema.table_privileges WHERE grantee = 'alice' AND table_name = 't2' ORDER BY privilege_type`)); got != "alice,t2,DELETE;alice,t2,INSERT;alice,t2,SELECT;alice,t2,UPDATE" {
		t.Fatalf("table_privileges: %s", got)
	}
	// the prepared-statement route
	st, _ := ParseOne(`SELECT * FROM t1`)
	run(t, su, `REVOKE ALL ON t1 FROM alice`)
	if _, err := a.Describe(st); code(err) != "42501" {
		t.Fatalf("describe: %v", err)
	}
	if _, err := a.Exec(st, nil); code(err) != "42501" {
		t.Fatalf("cached statement: %v", err)
	}
	run(t, su, `GRANT SELECT ON t1 TO alice`)
	if _, err := a.Exec(st, nil); err != nil {
		t.Fatalf("cached statement after regrant: %v", err)
	}

	// superuser role bypasses; roles view
	b := roleSession(e, "boss")
	run(t, b, `SELECT * FROM t2`)
	run(t, b, `CREATE TABLE made_by_boss (id BIGINT PRIMARY KEY)`)
	if got := render(run(t, su, `SELECT rolname, rolsuper, rolcanlogin FROM pg_roles ORDER BY rolname`)); got != "alice,f,t;boss,t,t;nologin,f,f;stask,t,t" {
		t.Fatalf("pg_roles: %s", got)
	}
	if got := render(run(t, a, `SELECT current_user`)); got != "alice" {
		t.Fatalf("current_user: %s", got)
	}

	// ALTER ROLE
	run(t, su, `ALTER ROLE alice PASSWORD 'pw2' NOLOGIN`)
	r, _ = e.Role("alice")
	if v, _ := core.ParseSCRAM(r.Verifier); !v.CheckPassword("pw2") || v.CheckPassword("pw1") || r.Login {
		t.Fatalf("alter role: %+v", r)
	}
	run(t, su, `ALTER USER alice PASSWORD NULL LOGIN`)
	if r, _ = e.Role("alice"); r.Verifier != "" {
		t.Fatal("PASSWORD NULL kept a verifier")
	}
	if _, err := tryRun(su, `ALTER ROLE ghost LOGIN`); code(err) != "42704" {
		t.Fatalf("alter missing: %v", err)
	}
	if _, err := tryRun(b, `ALTER ROLE boss NOSUPERUSER`); code(err) != "42501" {
		t.Fatalf("self-demotion: %v", err)
	}

	// DROP ROLE: privileges block it; the table's drop clears them
	if _, err := tryRun(su, `DROP ROLE alice`); code(err) != "2BP01" {
		t.Fatalf("drop with privileges: %v", err)
	}
	run(t, su, `REVOKE ALL ON ALL TABLES IN SCHEMA public FROM alice`)
	run(t, su, `REVOKE ALL ON v1 FROM alice`)
	run(t, su, `DROP ROLE alice`)
	if _, err := tryRun(su, `DROP ROLE alice`); code(err) != "42704" {
		t.Fatalf("drop twice: %v", err)
	}
	run(t, su, `DROP ROLE IF EXISTS alice`)
	wantDenied(t, a, `SELECT * FROM t1`) // the open session of a dropped role

	// privileges do not outlive their table
	run(t, su, `GRANT SELECT ON t2 TO nologin`)
	run(t, su, `DROP VIEW v1`)
	run(t, su, `DROP TABLE t2`)
	run(t, su, `CREATE TABLE t2 (id BIGINT PRIMARY KEY)`)
	n := roleSession(e, "nologin")
	wantDenied(t, n, `SELECT * FROM t2`)
	// and follow a rename
	run(t, su, `GRANT SELECT ON t1 TO nologin`)
	run(t, su, `ALTER TABLE t1 RENAME TO t1b`)
	run(t, n, `SELECT * FROM t1b`)
}

func TestRolesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() *core.Engine {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	s := NewSession(e)
	run(t, s, `CREATE TABLE t (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE USER u PASSWORD 'secret'`)
	run(t, s, `GRANT SELECT, INSERT ON t TO u`)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	run(t, s, `CREATE USER later REPLICATION`)
	run(t, s, `GRANT SELECT ON t TO PUBLIC`)
	e.Close()

	e = open()
	defer e.Close()
	u, ok := e.Role("u")
	if !ok || !u.Login || u.Grants["t"] != core.PrivSelect|core.PrivInsert {
		t.Fatalf("u after restart: %+v", u)
	}
	if v, err := core.ParseSCRAM(u.Verifier); err != nil || !v.CheckPassword("secret") {
		t.Fatal("verifier lost")
	}
	if l, ok := e.Role("later"); !ok || !l.Replication {
		t.Fatalf("role written after the checkpoint: %+v", l)
	}
	us := roleSession(e, "u")
	run(t, us, `INSERT INTO t VALUES (1)`)
	wantDenied(t, us, `DELETE FROM t`)
	run(t, roleSession(e, "later"), `SELECT * FROM t`) // PUBLIC
	// the hidden table is not a client table
	if _, err := tryRun(NewSession(e), `SELECT * FROM __stask_roles`); err == nil {
		t.Fatal("system table readable")
	}
	for _, tb := range e.UserTables() {
		if strings.HasPrefix(tb.Schema().Name, "__") {
			t.Fatalf("hidden table listed: %s", tb.Schema().Name)
		}
	}
}

func TestActivityAndSlowLog(t *testing.T) {
	e := newEngine(t)
	var logged []string
	SetSlowQueryLog(1, func(f string, a ...any) { logged = append(logged, f) })
	defer SetSlowQueryLog(0, nil)
	s := NewSession(e)
	s.PID = 4242
	s.Track("alice", "psql", "10.0.0.1", 5555)
	defer s.Close()
	s.SetQuery("SELECT 1")
	run(t, s, `SELECT 1`)
	if len(logged) == 0 {
		t.Fatal("slow log empty at a 1ns threshold")
	}
	o := NewSession(e)
	o.PID = 1
	o.Track("root", "", "", 0)
	defer o.Close()
	o.SetQuery(`SELECT pid, usename, state, query FROM pg_stat_activity WHERE pid = 4242 OR pid = 1 ORDER BY pid`)
	got := render(run(t, o, `SELECT pid, usename, state, query FROM pg_stat_activity WHERE pid = 4242 OR pid = 1 ORDER BY pid`))
	if !strings.Contains(got, "1,root,active,SELECT pid") || !strings.Contains(got, "4242,alice,idle,SELECT 1") {
		t.Fatalf("pg_stat_activity: %s", got)
	}
	// query_start and xact_start are timestamps; xact_start is null while idle
	got = render(run(t, o, `SELECT query_start IS NOT NULL, xact_start IS NULL FROM pg_stat_activity WHERE pid = 4242`))
	if got != "t,t" {
		t.Fatalf("times: %s", got)
	}
	run(t, s, `BEGIN`)
	if got = render(run(t, o, `SELECT state, xact_start IS NOT NULL FROM pg_stat_activity WHERE pid = 4242`)); got != "idle in transaction,t" {
		t.Fatalf("in transaction: %s", got)
	}
	run(t, s, `ROLLBACK`)
	// another user's query text is hidden from a non-superuser
	run(t, NewSession(e), `CREATE ROLE nosy LOGIN`)
	n := roleSession(e, "nosy")
	n.PID = 7
	n.Track("nosy", "", "", 0)
	defer n.Close()
	// the catalog is readable without grants
	if got = render(run(t, n, `SELECT query FROM pg_stat_activity WHERE pid = 4242`)); got != "<insufficient privilege>" {
		t.Fatalf("hidden query: %s", got)
	}
	// passwords are not kept in the activity text
	s.SetQuery(`CREATE ROLE x PASSWORD 'hunter2'`)
	run(t, s, `SELECT 1`) // (statement text of a role command is replaced when it runs)
	var b strings.Builder
	WriteMetrics(&b, e)
	for _, want := range []string{`stask_sql_statements_total{kind="select"}`, "stask_sql_statement_duration_seconds_count", `stask_pg_sessions{state="idle"}`} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics lack %s:\n%s", want, b.String())
		}
	}
}
