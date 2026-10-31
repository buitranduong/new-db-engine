package repl

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/sql"
)

// The leader moves an index to disk and back with ALTER INDEX; the replica,
// which applies that DDL record while it keeps applying the leader's writes,
// converts online (it never stops following), ends on the same storage, and
// answers through the index the same as the leader.
func TestReplicaIndexStorageOnline(t *testing.T) {
	base := t.TempDir()
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf,
		DiskIndexes: true, DiskIndexCacheMB: 2})
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
	for i := 0; i < 600; i++ {
		exec(ls, fmt.Sprintf(`INSERT INTO emp VALUES (%d, %d, 'm%d')`, i, i%10, i))
	}
	exec(ls, `CREATE INDEX emp_dept ON emp (dept)`)
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
	replica, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf,
		DiskIndexes: true, DiskIndexCacheMB: 2})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var fw sync.WaitGroup
	fw.Add(1)
	go func() { defer fw.Done(); Follow(replica, srv.Addr(), nil, stop, t.Logf) }()
	defer func() { close(stop); fw.Wait(); replica.Close() }()

	count := func(e *core.Engine, dept int64) int64 {
		return exec(sql.NewSession(e), fmt.Sprintf(`SELECT count(*) FROM emp WHERE dept = %d`, dept)).Rows[0][0].I
	}
	waitStorage := func(want string) {
		t.Helper()
		for i := 0; i < 600; i++ {
			if got, err := replica.IndexStorage("emp", "emp_dept"); err == nil && got == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		got, err := replica.IndexStorage("emp", "emp_dept")
		t.Fatalf("replica storage %q, %v; want %q", got, err, want)
	}
	check := func(when string) {
		t.Helper()
		waitLSN(t, replica, leader.DurableLSN())
		for d := int64(0); d < 12; d++ {
			if l, r := count(leader, d), count(replica, d); l != r {
				t.Fatalf("%s: dept %d: leader %d, replica %d", when, d, l, r)
			}
		}
	}
	check("before")
	var stopW atomic.Bool
	var ww sync.WaitGroup
	ws := sql.NewSession(leader)
	ww.Add(1)
	go func() {
		defer ww.Done()
		for i := 1000; !stopW.Load(); i++ {
			st, _ := sql.ParseOne(fmt.Sprintf(`INSERT INTO emp VALUES (%d, %d, 'w%d')`, i, i%10, i))
			ws.Exec(st, nil)
		}
	}()
	exec(ls, `ALTER INDEX emp_dept SET (index_storage = 'disk')`)
	waitStorage("disk")
	exec(ls, `ALTER INDEX emp_dept SET (index_storage = 'memory')`)
	waitStorage("memory")
	exec(ls, `ALTER INDEX emp_dept SET (index_storage = 'disk')`)
	stopW.Store(true)
	ww.Wait()
	waitStorage("disk")
	check("after")
	for _, st := range replica.SecIndexStats() {
		if st.Name == "emp_dept" && st.Storage != "disk" {
			t.Fatalf("stats say %s", st.Storage)
		}
	}
}
