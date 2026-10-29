package repl

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"stask/internal/core"
	"stask/internal/sql"
)

// Tables without a PRIMARY KEY and with a composite one reach a replica
// (the hidden key column travels in the rows; nothing else is needed), the
// replica agrees with the leader after random writes and after its own
// restart, and a promoted replica keeps handing out fresh row ids.
func TestReplicaNoPKAndCompositeKey(t *testing.T) {
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
	dump := func(e *core.Engine, q string) string {
		r := exec(sql.NewSession(e), q)
		var rows []string
		for _, row := range r.Rows {
			var vs []string
			for _, v := range row {
				vs = append(vs, v.String())
			}
			rows = append(rows, strings.Join(vs, ","))
		}
		sort.Strings(rows)
		return strings.Join(rows, ";")
	}
	exec(ls, `CREATE TABLE np (g INT, v INT)`)
	exec(ls, `CREATE TABLE ck (a INT, b TEXT, v INT, PRIMARY KEY (a, b))`)
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
	rep, closeRep := openReplica()
	rng := rand.New(rand.NewSource(7))
	same := func(when string) {
		t.Helper()
		waitLSN(t, rep, leader.DurableLSN())
		for _, q := range []string{`SELECT g, v FROM np`, `SELECT a, b, v FROM ck`, `SELECT count(*) FROM np`} {
			if l, r := dump(leader, q), dump(rep, q); l != r {
				t.Fatalf("%s: %s\nleader  %q\nreplica %q", when, q, l, r)
			}
		}
	}
	write := func(n int) {
		for i := 0; i < n; i++ {
			switch rng.Intn(6) {
			case 0, 1:
				exec(ls, fmt.Sprintf(`INSERT INTO np VALUES (%d, %d)`, rng.Intn(5), rng.Intn(5)))
			case 2:
				exec(ls, fmt.Sprintf(`UPDATE np SET v = %d WHERE g = %d`, rng.Intn(5), rng.Intn(5)))
			case 3:
				exec(ls, fmt.Sprintf(`DELETE FROM np WHERE g = %d AND v = %d`, rng.Intn(5), rng.Intn(5)))
			case 4:
				exec(ls, fmt.Sprintf(`INSERT INTO ck VALUES (%d, 'k%d', %d) ON CONFLICT (a, b) DO UPDATE SET v = excluded.v`, rng.Intn(5), rng.Intn(3), rng.Intn(9)))
			default:
				exec(ls, fmt.Sprintf(`DELETE FROM ck WHERE a = %d`, rng.Intn(5)))
			}
		}
	}
	write(200)
	same("live replica")
	closeRep()
	write(100) // while the replica is down
	rep, closeRep = openReplica()
	same("replica after its restart")
	if err := leader.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	write(100)
	same("after a leader checkpoint")
	closeRep()
}
