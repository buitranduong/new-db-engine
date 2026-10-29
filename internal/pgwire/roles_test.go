package pgwire

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"stask/internal/core"
)

func startRoleServer(t *testing.T, setup func(*Server)) (string, *core.Engine) {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{E: e, Logf: t.Logf}
	setup(s)
	if err := s.prepareAuth(); err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close(); e.Close() })
	return ln.Addr().String(), e
}

func rconnect(t *testing.T, user, pw, addr string, extra string) (*pgx.Conn, error) {
	t.Helper()
	return pgx.Connect(context.Background(), "postgres://"+user+":"+pw+"@"+addr+"/stask?sslmode=disable"+extra)
}

func rmustExec(t *testing.T, c *pgx.Conn, q string, args ...any) {
	t.Helper()
	if _, err := c.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestRolesOverWire(t *testing.T) {
	for _, auth := range []string{AuthSCRAM, AuthPassword} {
		t.Run(auth, func(t *testing.T) {
			addr, _ := startRoleServer(t, func(s *Server) { s.Auth = auth; s.Password = "boot" })
			ctx := context.Background()
			boot, err := rconnect(t, "app", "boot", addr, "")
			if err != nil {
				t.Fatal(err)
			}
			defer boot.Close(ctx)
			rmustExec(t, boot, `CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT)`)
			rmustExec(t, boot, `INSERT INTO t VALUES (1,'a')`)
			rmustExec(t, boot, `CREATE USER alice PASSWORD 'al1ce-pw'`)
			rmustExec(t, boot, `CREATE ROLE ghost PASSWORD 'x'`) // NOLOGIN

			if _, err := rconnect(t, "alice", "boot", addr, ""); sqlState(err) != "28P01" {
				t.Fatalf("shared password for a role: want 28P01, got %v", err)
			}
			if _, err := rconnect(t, "alice", "wrong", addr, ""); sqlState(err) != "28P01" {
				t.Fatalf("wrong password: want 28P01, got %v", err)
			}
			if _, err := rconnect(t, "ghost", "x", addr, ""); sqlState(err) != "28000" {
				t.Fatalf("NOLOGIN: want 28000, got %v", err)
			}
			a, err := rconnect(t, "alice", "al1ce-pw", addr, "")
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(ctx)

			var n int
			err = a.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n)
			if sqlState(err) != "42501" {
				t.Fatalf("select without privilege: want 42501, got %v", err)
			}
			// extended protocol: the denial comes at Parse, and the session lives on
			if err := a.QueryRow(ctx, `SELECT 1`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("session after denial: %v", err)
			}
			if _, err := a.Exec(ctx, `INSERT INTO t VALUES (2,'b')`); sqlState(err) != "42501" {
				t.Fatalf("insert: %v", err)
			}
			if _, err := a.Exec(ctx, `CREATE TABLE x (id BIGINT PRIMARY KEY)`); sqlState(err) != "42501" {
				t.Fatalf("ddl: %v", err)
			}
			rmustExec(t, boot, `GRANT SELECT, INSERT ON t TO alice`)
			if err := a.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("select after grant: %d %v", n, err)
			}
			if _, err := a.Exec(ctx, `INSERT INTO t VALUES ($1,$2)`, 2, "b"); err != nil {
				t.Fatalf("insert after grant: %v", err)
			}
			if _, err := a.Exec(ctx, `DELETE FROM t WHERE id = 2`); sqlState(err) != "42501" {
				t.Fatalf("delete: %v", err)
			}
			// COPY needs the same privileges
			if _, err := a.PgConn().CopyFrom(ctx, strings.NewReader("7\tz\n"), `COPY t FROM STDIN`); err != nil {
				t.Fatalf("copy from with INSERT: %v", err)
			}
			if _, err := a.PgConn().CopyTo(ctx, discard{}, `COPY t TO STDOUT`); err != nil {
				t.Fatalf("copy to with SELECT: %v", err)
			}
			rmustExec(t, boot, `REVOKE SELECT ON t FROM alice`)
			if _, err := a.PgConn().CopyTo(ctx, discard{}, `COPY t TO STDOUT`); sqlState(err) != "42501" {
				t.Fatalf("copy to without SELECT: %v", err)
			}

			// pg_stat_activity shows both sessions, with their users
			var users string
			if err := boot.QueryRow(ctx, `SELECT string_agg(usename, ',' ORDER BY usename) FROM pg_stat_activity`).Scan(&users); err != nil || users != "alice,app" {
				t.Fatalf("pg_stat_activity: %q %v", users, err)
			}
			var su string
			if err := a.QueryRow(ctx, `SELECT current_user`).Scan(&su); err != nil || su != "alice" {
				t.Fatalf("current_user: %q %v", su, err)
			}

			// a new password applies to the next login
			rmustExec(t, boot, `ALTER ROLE alice PASSWORD 'second'`)
			if _, err := rconnect(t, "alice", "al1ce-pw", addr, ""); sqlState(err) != "28P01" {
				t.Fatalf("old password: %v", err)
			}
			c, err := rconnect(t, "alice", "second", addr, "")
			if err != nil {
				t.Fatalf("new password: %v", err)
			}
			c.Close(ctx)
			// a role whose LOGIN is revoked cannot log in again
			rmustExec(t, boot, `ALTER ROLE alice NOLOGIN`)
			if _, err := rconnect(t, "alice", "second", addr, ""); sqlState(err) != "28000" {
				t.Fatalf("NOLOGIN: %v", err)
			}
		})
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestRolesOnlyClosesSharedPassword(t *testing.T) {
	addr, _ := startRoleServer(t, func(s *Server) { s.Auth = AuthSCRAM; s.Password = "boot"; s.RolesOnly = true })
	ctx := context.Background()
	boot, err := rconnect(t, "app", "boot", addr, "")
	if err != nil {
		t.Fatal(err) // no superuser role yet: the shared password still works
	}
	rmustExec(t, boot, `CREATE USER admin SUPERUSER PASSWORD 'adm'`)
	boot.Close(ctx)
	if _, err := rconnect(t, "app", "boot", addr, ""); sqlState(err) != "28P01" {
		t.Fatalf("shared password after a superuser role exists: %v", err)
	}
	ad, err := rconnect(t, "admin", "adm", addr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close(ctx)
	var s string
	if err := ad.QueryRow(ctx, `SHOW is_superuser`).Scan(&s); err == nil && s != "on" {
		t.Fatalf("is_superuser %q", s)
	}
}

func TestReplicationNeedsPrivilege(t *testing.T) {
	addr, _ := startRoleServer(t, func(s *Server) { s.Auth = AuthSCRAM; s.Password = "boot" })
	ctx := context.Background()
	boot, err := rconnect(t, "app", "boot", addr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close(ctx)
	rmustExec(t, boot, `CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT)`)
	rmustExec(t, boot, `CREATE PUBLICATION pub FOR TABLE t`)
	rmustExec(t, boot, `CREATE USER plain PASSWORD 'p1'`)
	rmustExec(t, boot, `CREATE USER repl REPLICATION PASSWORD 'p2'`)

	replURL := func(user, pw string) string {
		return "postgres://" + user + ":" + pw + "@" + addr + "/stask?sslmode=disable&replication=database"
	}
	connectRepl := func(user, pw string) (*pgconn.PgConn, error) {
		cfg, err := pgconn.ParseConfig(replURL(user, pw))
		if err != nil {
			t.Fatal(err)
		}
		return pgconn.ConnectConfig(ctx, cfg)
	}
	// without REPLICATION: refused at startup, as PostgreSQL does
	if _, err := connectRepl("plain", "p1"); sqlState(err) != "42501" {
		t.Fatalf("walsender without REPLICATION: want 42501, got %v", err)
	}
	// a wrong password is still an authentication failure
	if _, err := connectRepl("repl", "nope"); sqlState(err) != "28P01" {
		t.Fatalf("wrong password: %v", err)
	}
	pc, err := connectRepl("repl", "p2")
	if err != nil {
		t.Fatalf("walsender with REPLICATION: %v", err)
	}
	defer pc.Close(ctx)
	if res, err := pc.Exec(ctx, "IDENTIFY_SYSTEM").ReadAll(); err != nil || len(res) != 1 {
		t.Fatalf("IDENTIFY_SYSTEM: %v", err)
	}
	if _, err := pc.Exec(ctx, "CREATE_REPLICATION_SLOT s1 LOGICAL pgoutput").ReadAll(); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	// REPLICATION is not table access: plain SQL on the connection is checked
	if _, err := pc.Exec(ctx, "SELECT * FROM t").ReadAll(); sqlState(err) != "42501" {
		t.Fatalf("SELECT on a replication connection without privilege: %v", err)
	}
	// an ordinary connection of the REPLICATION role cannot read either
	r, err := rconnect(t, "repl", "p2", addr, "")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); sqlState(err) != "42501" {
		t.Fatalf("select by the replication role: %v", err)
	}
	r.Close(ctx)

	// the stream itself flows to a REPLICATION role that holds no table privilege
	rmustExec(t, boot, `INSERT INTO t VALUES (1,'streamed')`)
	fe := pc.Frontend()
	fe.Send(&pgproto3.Query{String: "START_REPLICATION SLOT s1 LOGICAL 0/0 (proto_version '1', publication_names 'pub')"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	sawInsert, started := false, false
	for !sawInsert && time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		msg, err := pc.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			t.Fatalf("receive: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.CopyBothResponse:
			started = true
		case *pgproto3.CopyData:
			if len(m.Data) > 25 && m.Data[0] == 'w' && m.Data[25] == 'I' {
				sawInsert = true
			}
		case *pgproto3.ErrorResponse:
			t.Fatalf("stream error: %s %s", m.Code, m.Message)
		}
	}
	if !started || !sawInsert {
		t.Fatalf("logical stream did not deliver the insert (started=%v)", started)
	}
}
