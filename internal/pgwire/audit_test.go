package pgwire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stask/internal/audit"
)

func readAll(t *testing.T, dir string) string {
	var sb strings.Builder
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		sb.Write(b)
	}
	return sb.String()
}

func recs(t *testing.T, dir string, f audit.Filter) []audit.Record {
	t.Helper()
	rs, err := audit.Query(dir, f)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// TestAuditTrailOverWire drives a real client and checks what the audit
// trail recorded: connections (success and each kind of failure), role and
// GRANT changes, DDL, denials, reads and writes (session logging), that no
// password reaches the files, and that the chain verifies.
func TestAuditTrailOverWire(t *testing.T) {
	dir := t.TempDir()
	key := []byte("0123456789abcdef-audit-key")
	l, err := audit.Open(audit.Config{Dir: dir, Key: key, Classes: audit.ClassConnection | audit.ClassRole | audit.ClassDDL |
		audit.ClassRead | audit.ClassWrite | audit.ClassDenied | audit.ClassAdmin})
	if err != nil {
		t.Fatal(err)
	}
	audit.SetDefault(l)
	t.Cleanup(func() { audit.SetDefault(nil) })

	addr, _ := startRoleServer(t, func(s *Server) { s.Auth = AuthSCRAM; s.Password = "boot" })
	ctx := context.Background()
	boot, err := rconnect(t, "app", "boot", addr, "&application_name=admin-tool")
	if err != nil {
		t.Fatal(err)
	}
	rmustExec(t, boot, `CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT)`)
	rmustExec(t, boot, `INSERT INTO t VALUES (1,'a')`)
	rmustExec(t, boot, `CREATE USER alice PASSWORD 'al1ce-pw'`)
	rmustExec(t, boot, `CREATE ROLE ghost PASSWORD 'ghost-pw'`)
	rmustExec(t, boot, `GRANT SELECT ON t TO alice`)
	// a multi-statement query that mentions a password: the DDL record must not carry the text
	if _, err := boot.PgConn().Exec(ctx, `CREATE TABLE z (id BIGINT PRIMARY KEY); CREATE ROLE bob PASSWORD 'topsecret-bob'`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := rconnect(t, "alice", "wrong-pw", addr, ""); sqlState(err) != "28P01" {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := rconnect(t, "ghost", "ghost-pw", addr, ""); sqlState(err) != "28000" {
		t.Fatalf("NOLOGIN: %v", err)
	}
	a, err := rconnect(t, "alice", "al1ce-pw", addr, "")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := a.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO t VALUES (2,'b')`); sqlState(err) != "42501" {
		t.Fatalf("insert: %v", err)
	}
	if _, err := a.Exec(ctx, `CREATE TABLE x (id BIGINT PRIMARY KEY)`); sqlState(err) != "42501" {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := a.PgConn().CopyTo(ctx, discard{}, `COPY t TO STDOUT`); err != nil {
		t.Fatal(err)
	}
	if _, err := boot.PgConn().CopyFrom(ctx, strings.NewReader("7\tz\n"), `COPY t FROM STDIN`); err != nil {
		t.Fatal(err)
	}
	rmustExec(t, boot, `ALTER ROLE alice PASSWORD 'second-pw'`)
	rmustExec(t, boot, `REVOKE SELECT ON t FROM alice`)
	rmustExec(t, boot, `DELETE FROM t WHERE id = 7`)
	rmustExec(t, boot, `DROP ROLE ghost`)
	a.Close(ctx)
	boot.Close(ctx)
	// the disconnect records are written when the server notices the close
	for i := 0; i < 100 && len(recs(t, dir, audit.Filter{Ev: "disconnect"})) < 2; i++ {
		l.Log(audit.Record{Ev: "wait", OK: true}, false)
		time.Sleep(20 * time.Millisecond)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	audit.SetDefault(nil)

	if res, err := audit.Verify(dir, key); err != nil || !res.HeadOK {
		t.Fatalf("verify: %+v %v", res, err)
	}
	all := readAll(t, dir)
	for _, secret := range []string{"al1ce-pw", "second-pw", "ghost-pw", "topsecret-bob", "wrong-pw", "boot\""} {
		if strings.Contains(all, secret) {
			t.Fatalf("%q reached the audit files", secret)
		}
	}
	want := func(name string, f audit.Filter, ok bool, sub string) {
		t.Helper()
		for _, r := range recs(t, dir, f) {
			if r.OK == ok && strings.Contains(r.Detail+r.App+r.Addr, sub) {
				return
			}
		}
		t.Errorf("%s: no record for %+v (ok=%v, %q)\n%s", name, f, ok, sub, all)
	}
	want("login", audit.Filter{Ev: "connect.ok", User: "app"}, true, "admin-tool")
	want("login alice", audit.Filter{Ev: "connect.ok", User: "alice"}, true, "")
	want("wrong password", audit.Filter{Ev: "connect.fail", User: "alice"}, false, "password authentication failed")
	want("nologin", audit.Filter{Ev: "connect.fail", User: "ghost"}, false, "not permitted to log in")
	want("disconnect", audit.Filter{Ev: "disconnect", User: "alice"}, true, "")
	want("create role", audit.Filter{Ev: "role", Cmd: "CREATE ROLE", Obj: "alice"}, true, "password=set")
	want("grant", audit.Filter{Ev: "role", Cmd: "GRANT", Obj: "t"}, true, "privileges=SELECT roles=alice")
	want("revoke", audit.Filter{Ev: "role", Cmd: "REVOKE", Obj: "t"}, true, "roles=alice")
	want("alter role", audit.Filter{Ev: "role", Cmd: "ALTER ROLE", Obj: "alice"}, true, "password=set")
	want("drop role", audit.Filter{Ev: "role", Cmd: "DROP ROLE", Obj: "ghost"}, true, "")
	want("create table", audit.Filter{Ev: "ddl", Obj: "t"}, true, "CREATE TABLE t")
	want("withheld text", audit.Filter{Ev: "ddl", Obj: "z"}, true, "withheld")
	want("denied insert", audit.Filter{Ev: "sql.denied", User: "alice"}, false, "permission denied for table t")
	want("denied ddl", audit.Filter{Ev: "sql.denied", User: "alice", Cmd: "CREATE TABLE"}, false, "42501")
	want("read", audit.Filter{Ev: "sql.read", Obj: "t", User: "alice"}, true, "")
	want("copy to", audit.Filter{Ev: "sql.read", Cmd: "COPY TO", Obj: "t"}, true, "")
	want("write", audit.Filter{Ev: "sql.write", Cmd: "INSERT", Obj: "t", User: "app"}, true, "")
	want("copy from", audit.Filter{Ev: "sql.write", Cmd: "COPY FROM", Obj: "t"}, true, "")
	want("delete", audit.Filter{Ev: "sql.write", Cmd: "DELETE", Obj: "t"}, true, "")
}

// TestAuditObjectLogging: with an object list, only those tables and only
// the listed operations are recorded (pgaudit object logging); with the
// classes off nothing else is.
func TestAuditObjectLogging(t *testing.T) {
	dir := t.TempDir()
	tabs, _ := audit.ParseTables("secret:read,pii")
	l, err := audit.Open(audit.Config{Dir: dir, Classes: audit.ClassConnection, Tables: tabs})
	if err != nil {
		t.Fatal(err)
	}
	audit.SetDefault(l)
	t.Cleanup(func() { audit.SetDefault(nil) })
	addr, _ := startRoleServer(t, func(s *Server) { s.Auth = AuthSCRAM; s.Password = "boot" })
	ctx := context.Background()
	c, err := rconnect(t, "app", "boot", addr, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE secret (id BIGINT PRIMARY KEY, v TEXT)`, `CREATE TABLE pii (id BIGINT PRIMARY KEY, v TEXT)`, `CREATE TABLE other (id BIGINT PRIMARY KEY, v TEXT)`,
		`INSERT INTO secret VALUES (1,'x')`, `INSERT INTO pii VALUES (1,'x')`, `INSERT INTO other VALUES (1,'x')`,
		`SELECT * FROM secret`, `SELECT * FROM pii`, `SELECT * FROM other`, `UPDATE pii SET v='y'`,
	} {
		rmustExec(t, c, q)
	}
	c.Close(ctx)
	l.Close()
	audit.SetDefault(nil)
	got := map[string]bool{}
	for _, r := range recs(t, dir, audit.Filter{Ev: "sql."}) {
		got[r.Ev+" "+r.Cmd+" "+r.Obj] = true
	}
	for _, w := range []string{"sql.read SELECT secret", "sql.read SELECT pii", "sql.write INSERT pii", "sql.write UPDATE pii"} {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	for _, nw := range []string{"sql.write INSERT secret", "sql.write INSERT other", "sql.read SELECT other"} {
		if got[nw] {
			t.Errorf("unexpected %q", nw)
		}
	}
	if len(recs(t, dir, audit.Filter{Ev: "ddl"})) != 0 {
		t.Error("ddl logged though the class is off")
	}
}
