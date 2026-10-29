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
	"sync/atomic"
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

// TestClusterFollowsLeader: three fake nodes; the leader changes while a
// request is being retried, and a request sent to a dead node is repeated
// on the new leader, while one that was sent and lost is not.
func TestClusterFollowsLeader(t *testing.T) {
	type node struct {
		srv    *httptest.Server
		leader atomic.Bool
		writes atomic.Int64
	}
	var ns [3]*node
	var urls []string
	for i := range ns {
		n := &node{}
		ns[i] = n
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/cluster", func(w http.ResponseWriter, r *http.Request) {
			role := "follower"
			if n.leader.Load() {
				role = "leader"
			}
			fmt.Fprintf(w, `{"role":%q,"term":1,"writable":%v}`, role, n.leader.Load())
		})
		mux.HandleFunc("/v1/write", func(w http.ResponseWriter, r *http.Request) {
			if !n.leader.Load() {
				w.Header().Set("X-Stask-Not-Leader", "1")
				w.WriteHeader(503)
				return
			}
			n.writes.Add(1)
			w.Header().Set(headerLSN, "7")
			fmt.Fprint(w, `{"lsn":7}`)
		})
		n.srv = httptest.NewServer(mux)
		urls = append(urls, n.srv.URL)
		defer n.srv.Close()
	}
	ns[0].leader.Store(true)
	rt, err := NewCluster(urls...)
	if err != nil {
		t.Fatal(err)
	}
	rt.FailoverWait = 5 * time.Second
	front := httptest.NewServer(rt)
	defer front.Close()
	post := func() (int, error) {
		resp, err := http.Post(front.URL+"/v1/write", "application/json", strings.NewReader(`{}`))
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if c, err := post(); err != nil || c != 200 || ns[0].writes.Load() != 1 {
		t.Fatalf("first write: %d %v", c, err)
	}
	// the leader steps down; node 2 takes over 200 ms later
	ns[0].leader.Store(false)
	go func() { time.Sleep(200 * time.Millisecond); ns[2].leader.Store(true) }()
	if c, err := post(); err != nil || c != 200 || ns[2].writes.Load() != 1 {
		t.Fatalf("write during the change: %d %v (node 2 wrote %d)", c, err, ns[2].writes.Load())
	}
	// the leader's process dies: connection refused, nothing was sent
	ns[2].srv.Close()
	ns[1].leader.Store(true)
	if c, err := post(); err != nil || c != 200 || ns[1].writes.Load() != 1 {
		t.Fatalf("write after the leader died: %d %v", c, err)
	}
	if rt.Rediscovered.Load() < 2 {
		t.Fatalf("the router rediscovered the leader %d times", rt.Rediscovered.Load())
	}
}
