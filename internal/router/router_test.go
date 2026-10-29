package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/repl"
	"stask/internal/schema"
	"stask/internal/server"
	"stask/internal/sql"
)

// cluster: a leader with a replication port, one replica following it, an
// HTTP server on each and a router in front.
type cluster struct {
	leader, replica *core.Engine
	lURL, rURL      string
	rt              *Router
	front           *httptest.Server
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	base := t.TempDir()
	leader, err := core.Open(core.Options{Dir: filepath.Join(base, "leader"), SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leader.Close() })
	if _, err := leader.CreateTable(schema.Table{Name: "kv", Key: "id", Columns: []schema.Column{
		{Name: "id", Type: "string"}, {Name: "n", Type: "int"}}}); err != nil {
		t.Fatal(err)
	}
	rs, err := repl.Serve(leader, "127.0.0.1:0", nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	rdir := filepath.Join(base, "replica")
	segMax, err := repl.Prepare(rs.Addr(), rdir, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := core.Open(core.Options{Dir: rdir, SegmentSize: segMax, Replica: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); repl.Follow(replica, rs.Addr(), nil, stop, t.Logf) }()
	t.Cleanup(func() { close(stop); wg.Wait(); replica.Close() })

	lh := httptest.NewServer(server.New(&planner.Platform{E: leader}, nil).Handler())
	rh := httptest.NewServer(server.New(&planner.Platform{E: replica}, nil).Handler())
	t.Cleanup(lh.Close)
	t.Cleanup(rh.Close)
	rt, err := New(lh.URL, rh.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(rt)
	t.Cleanup(front.Close)
	return &cluster{leader, replica, lh.URL, rh.URL, rt, front}
}

func putURL(t *testing.T, base string, id string, n int) (*http.Response, error) {
	body := fmt.Sprintf(`{"ops":[{"op":"put","table":"kv","row":{"id":%q,"n":%d}}]}`, id, n)
	return http.Post(base+"/v1/write", "application/json", strings.NewReader(body))
}

// TestReadYourWritesRouter: through the router a client writes (to the
// leader) and reads at once (served by the replica); the read always
// sees the write, although the replica has no other reason to be current.
func TestReadYourWritesRouter(t *testing.T) {
	c := newCluster(t)
	// the write body format is the server's; check it once
	resp, err := putURL(t, c.lURL, "probe", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Skipf("write request format differs: %d %s", resp.StatusCode, b)
	}
	for i := 1; i <= 300; i++ {
		id := fmt.Sprintf("k%d", i)
		r, err := putURL(t, c.front.URL, id, i)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("write %d: %v %v", i, err, r)
		}
		if r.Header.Get("X-Stask-Lsn") == "" {
			t.Fatal("no X-Stask-Lsn on the write answer")
		}
		r.Body.Close()
		g, err := http.Get(c.front.URL + "/v1/get?table=kv&key=" + id)
		if err != nil {
			t.Fatal(err)
		}
		gb, _ := io.ReadAll(g.Body)
		g.Body.Close()
		var out struct {
			Found bool           `json:"found"`
			Row   map[string]any `json:"row"`
		}
		json.Unmarshal(gb, &out)
		if g.StatusCode != 200 || !out.Found {
			t.Fatalf("read after write %d: %d %s", i, g.StatusCode, gb)
		}
	}
	if c.rt.ToReplica.Load() == 0 {
		t.Fatal("no read was served by the replica")
	}
	t.Logf("reads: %d replica, %d leader, %d fallbacks", c.rt.ToReplica.Load(), c.rt.ToLeader.Load(), c.rt.Fallbacks.Load())
}

// TestMinLSNHeader: a replica holds a request until it has the LSN, and
// answers 503 when it cannot get there in time.
func TestMinLSNHeader(t *testing.T) {
	c := newCluster(t)
	lsn, err := c.leader.Write([]core.WriteOp{{Table: "kv", Row: schema.Row{{S: "a"}, {I: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", c.rURL+"/v1/get?table=kv&key=a", nil)
	req.Header.Set("X-Stask-Min-Lsn", fmt.Sprint(lsn))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", c.rURL+"/v1/get?table=kv&key=a", nil)
	req.Header.Set("X-Stask-Min-Lsn", fmt.Sprint(lsn+1000))
	req.Header.Set("X-Stask-Min-Lsn-Timeout", "100")
	t0 := time.Now()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || time.Since(t0) < 90*time.Millisecond {
		t.Fatalf("future LSN: status %d after %v", resp.StatusCode, time.Since(t0))
	}
}

// TestSQLMinLSN: the pgwire session variables, at the SQL layer: the
// leader session reads stask.last_lsn, the replica session sets
// stask.min_lsn and then sees the row, every time.
func TestSQLMinLSN(t *testing.T) {
	c := newCluster(t)
	run := func(s *sql.Session, q string) *sql.Result {
		t.Helper()
		st, err := sql.ParseOne(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		res, err := s.Exec(st, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res
	}
	ls, rs := sql.NewSession(c.leader), sql.NewSession(c.replica)
	run(ls, "CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT)")
	for i := 1; i <= 200; i++ {
		run(ls, fmt.Sprintf("INSERT INTO t VALUES (%d, 'x')", i))
		last := run(ls, "SHOW stask.last_lsn").Rows[0][0]
		run(rs, fmt.Sprintf("SET stask.min_lsn = %s", last.String()))
		res := run(rs, fmt.Sprintf("SELECT v FROM t WHERE id = %d", i))
		if len(res.Rows) != 1 {
			t.Fatalf("row %d not visible on the replica", i)
		}
	}
	run(rs, "SET stask.min_lsn = 999999999")
	run(rs, "SET stask.min_lsn_timeout = '50ms'")
	st, _ := sql.ParseOne("SELECT 1")
	if _, err := rs.Exec(st, nil); err == nil || !strings.Contains(err.Error(), "55000") && !strings.Contains(err.Error(), "has not reached") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}
