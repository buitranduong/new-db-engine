package repl

import (
	"path/filepath"
	"sync"
	"testing"

	"stask/internal/core"
	"stask/internal/sql"
)

// Roles and grants are rows of a hidden system table: they reach the
// replica through the log, so a replica enforces the leader's privileges
// (read-only sessions of a role) and keeps them across a restart.
func TestReplicaRoles(t *testing.T) {
	base := t.TempDir()
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	exec := func(s *sql.Session, q string) (*sql.Result, error) {
		t.Helper()
		st, err := sql.ParseOne(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s.Exec(st, nil)
	}
	must := func(s *sql.Session, q string) *sql.Result {
		t.Helper()
		r, err := exec(s, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	ls := sql.NewSession(leader)
	must(ls, `CREATE TABLE a (id BIGINT PRIMARY KEY)`)
	must(ls, `CREATE TABLE b (id BIGINT PRIMARY KEY)`)
	must(ls, `INSERT INTO a VALUES (1)`)
	must(ls, `INSERT INTO b VALUES (1)`)
	must(ls, `CREATE USER reader PASSWORD 'pw'`)
	must(ls, `GRANT SELECT ON a TO reader`)
	srv, err := Serve(leader, "127.0.0.1:0", nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	rdir := filepath.Join(base, "replica")
	openReplica := func() (*core.Engine, func()) {
		segMax, err := Prepare(srv.Addr(), rdir, nil, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		r, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		var fw sync.WaitGroup
		fw.Add(1)
		go func() { defer fw.Done(); Follow(r, srv.Addr(), nil, stop, t.Logf) }()
		return r, func() { close(stop); fw.Wait(); r.Close() }
	}
	check := func(e *core.Engine, what string) {
		t.Helper()
		waitLSN(t, e, leader.DurableLSN())
		rs := sql.NewSession(e)
		rs.SetRole("reader")
		if _, err := exec(rs, `SELECT count(*) FROM a`); err != nil {
			t.Fatalf("%s: granted table: %v", what, err)
		}
		if _, err := exec(rs, `SELECT count(*) FROM b`); sql.AsError(err).Code != "42501" {
			t.Fatalf("%s: ungranted table: %v", what, err)
		}
		r, ok := e.Role("reader")
		if !ok || !r.Login {
			t.Fatalf("%s: role missing: %+v", what, r)
		}
		if v, err := core.ParseSCRAM(r.Verifier); err != nil || !v.CheckPassword("pw") {
			t.Fatalf("%s: verifier", what)
		}
	}
	replica, closeR := openReplica()
	check(replica, "replica")
	// a later REVOKE reaches the running replica
	must(ls, `REVOKE SELECT ON a FROM reader`)
	waitLSN(t, replica, leader.DurableLSN())
	rs := sql.NewSession(replica)
	rs.SetRole("reader")
	if _, err := exec(rs, `SELECT count(*) FROM a`); sql.AsError(err).Code != "42501" {
		t.Fatalf("revoke did not reach the replica: %v", err)
	}
	must(ls, `GRANT SELECT ON a TO reader`)
	closeR()
	replica, closeR = openReplica()
	defer closeR()
	check(replica, "restarted replica")
}
