package repl

import (
	"path/filepath"
	"sync"
	"testing"

	"stask/internal/core"
	"stask/internal/sql"
)

// A trigger runs on the leader only: the replica receives the rows it
// wrote as ordinary log records (so each exactly once), knows the
// definition, and does not run it.
func TestReplicaTrigger(t *testing.T) {
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
	exec(ls, `CREATE TABLE log (id BIGINT PRIMARY KEY, n BIGINT)`)
	exec(ls, `CREATE TRIGGER t1 AFTER INSERT ON emp BEGIN INSERT INTO log VALUES (NEW.id, NEW.sal); END`)
	srv, err := Serve(leader, "127.0.0.1:0", nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	rdir := filepath.Join(base, "replica")
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
	defer func() { close(stop); fw.Wait(); r.Close() }()

	exec(ls, `INSERT INTO emp VALUES (1, 10), (2, 20)`)
	waitLSN(t, r, leader.DurableLSN())
	rs := sql.NewSession(r)
	if n := exec(rs, `SELECT count(*) FROM log`).Rows[0][0].I; n != 2 {
		t.Fatalf("replica log rows: %d, want 2 (the leader's, once)", n)
	}
	if _, ok := r.Trigger("emp", "t1"); !ok {
		t.Fatal("trigger definition missing on the replica")
	}
	exec(ls, `DROP TRIGGER t1`)
	exec(ls, `INSERT INTO emp VALUES (3, 30)`)
	waitLSN(t, r, leader.DurableLSN())
	if n := exec(rs, `SELECT count(*) FROM log`).Rows[0][0].I; n != 2 {
		t.Fatalf("replica log rows after DROP: %d", n)
	}
	if _, ok := r.Trigger("emp", "t1"); ok {
		t.Fatal("dropped trigger still on the replica")
	}
}
