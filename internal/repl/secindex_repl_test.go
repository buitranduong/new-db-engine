package repl

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"stask/internal/core"
	"stask/internal/sql"
)

// A secondary index created on the leader reaches the replica as a DDL
// record; the replica builds it from its rows, maintains it while applying
// the leader's writes, answers queries through it, and rebuilds it when it
// restarts.
func TestReplicaSecondaryIndex(t *testing.T) {
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
	exec(ls, `CREATE TABLE emp (id BIGINT PRIMARY KEY, dept BIGINT, mail TEXT)`)
	for i := 0; i < 300; i++ {
		exec(ls, fmt.Sprintf(`INSERT INTO emp VALUES (%d, %d, 'm%d')`, i, i%10, i))
	}
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
	scans := func(e *core.Engine, name string) int64 {
		for _, st := range e.SecIndexStats() {
			if st.Name == name {
				return st.Scans
			}
		}
		return -1
	}
	check := func(e *core.Engine, when string, dept, want int64) {
		t.Helper()
		waitLSN(t, e, leader.DurableLSN())
		before := scans(e, "emp_dept")
		got := exec(sql.NewSession(e), fmt.Sprintf(`SELECT count(*) FROM emp WHERE dept = %d`, dept)).Rows[0][0].I
		if got != want {
			t.Fatalf("%s: dept %d has %d rows on the replica, want %d", when, dept, got, want)
		}
		if scans(e, "emp_dept") <= before {
			t.Fatalf("%s: the replica did not read its index", when)
		}
	}
	replica, closeR := openReplica()
	// created after the replica started following
	exec(ls, `CREATE INDEX emp_dept ON emp (dept)`)
	exec(ls, `ALTER TABLE emp ADD CONSTRAINT emp_mail_uq UNIQUE (mail)`)
	check(replica, "built from the rows", 3, 30)
	exec(ls, `UPDATE emp SET dept = 99 WHERE id % 10 = 3 AND id < 100`) // moves 10 rows
	exec(ls, `DELETE FROM emp WHERE id % 10 = 4`)
	exec(ls, `INSERT INTO emp VALUES (1000, 3, 'new')`)
	check(replica, "after updates", 3, 21)
	check(replica, "after updates", 99, 10)
	check(replica, "after delete", 4, 0)
	closeR()
	replica, closeR = openReplica()
	exec(ls, `INSERT INTO emp VALUES (1001, 3, 'new2')`)
	check(replica, "after restart", 3, 22)
	closeR()
}
