package repl

import (
	"path/filepath"
	"sync"
	"testing"

	"stask/internal/core"
	"stask/internal/sql"
)

// A SQL view created on the leader is shipped to the replica as an
// ordinary log record: the replica answers queries through it, follows
// REPLACE and DROP, and keeps it across a restart.
func TestReplicaSQLView(t *testing.T) {
	base := t.TempDir()
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	ls := sql.NewSession(leader)
	exec := func(s *sql.Session, q string) *sql.Result {
		t.Helper()
		st, err := sql.ParseOne(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		r, err := s.Exec(st, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	exec(ls, `CREATE TABLE emp (id BIGINT PRIMARY KEY, sal BIGINT)`)
	exec(ls, `INSERT INTO emp VALUES (1, 10), (2, 20), (3, 30)`)
	exec(ls, `CREATE VIEW big AS SELECT id FROM emp WHERE sal >= 20`)
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
	count := func(e *core.Engine, q string) int64 {
		t.Helper()
		waitLSN(t, e, leader.DurableLSN())
		return exec(sql.NewSession(e), q).Rows[0][0].I
	}
	replica, closeR := openReplica()
	if n := count(replica, `SELECT count(*) FROM big`); n != 2 {
		t.Fatalf("replica view rows: %d", n)
	}
	exec(ls, `CREATE OR REPLACE VIEW big AS SELECT id FROM emp WHERE sal >= 30`)
	exec(ls, `CREATE VIEW bigger AS SELECT id FROM big`)
	if n := count(replica, `SELECT count(*) FROM bigger`); n != 1 {
		t.Fatalf("replica nested view rows: %d", n)
	}
	closeR()
	// restart (the replica rebuilds from its own log)
	replica, closeR = openReplica()
	if n := count(replica, `SELECT count(*) FROM bigger`); n != 1 {
		t.Fatalf("replica view rows after restart: %d", n)
	}
	exec(ls, `DROP VIEW bigger`)
	waitLSN(t, replica, leader.DurableLSN())
	if _, ok, _ := replica.View(nil, "bigger"); ok {
		t.Fatal("dropped view still on the replica")
	}
	if _, ok, _ := replica.View(nil, "big"); !ok {
		t.Fatal("view lost on the replica")
	}
	closeR()
}
