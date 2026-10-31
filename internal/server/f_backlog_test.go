package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/sql"
)

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

// F7: /v1/health answered 200 while commits failed because no log segment
// could be allocated. It now says 503 with writes_blocked, and goes back to
// 200 once the disk has room again, with no write needed to find out.
func TestHealthReportsWritesBlockedByFullDisk(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer(New(planner.Start(e), nil).Handler())
	defer srv.Close()
	free := int64(0)
	old := core.DiskFreeFunc
	core.DiskFreeFunc = func(string) (int64, error) { return free, nil }
	defer func() { core.DiskFreeFunc = old }()

	if c, b := get(t, srv.URL+"/v1/health"); c != 200 {
		t.Fatalf("healthy node: %d %s", c, b)
	}
	e.NoteSegmentAllocFailure(errors.New("cannot allocate log segment (disk full?): no space left on device"))
	if c, b := get(t, srv.URL+"/v1/health"); c != 503 || !strings.Contains(b, "writes_blocked") || !strings.Contains(b, `"ok":false`) {
		t.Fatalf("health with writes refused for lack of space: %d %s", c, b)
	}
	if c, b := get(t, srv.URL+"/v1/ready?write=1"); c != 503 || !strings.Contains(b, "no space left") {
		t.Fatalf("ready?write=1 with writes refused: %d %s", c, b)
	}
	if c, _ := get(t, srv.URL+"/v1/ready"); c != 200 {
		t.Fatalf("plain readiness (reads work) should stay 200, got %d", c)
	}
	free = 1 << 40
	if c, b := get(t, srv.URL+"/v1/health"); c != 200 {
		t.Fatalf("health after space came back: %d %s", c, b)
	}
}

// F4: a leader frozen and resumed is writable in the engine until it handles
// a message of the new term; ready?write=1 must not say 200 unless the
// cluster layer says writable with a valid lease.
func TestReadyWriteNeedsLeaseAndTerm(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := New(planner.Start(e), nil)
	m := map[string]float64{"raft_leader_known": 1, "raft_writable": 1, "raft_lease": 1}
	s.ClusterMetrics = func() map[string]float64 { return m }
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if c, b := get(t, srv.URL+"/v1/ready?write=1"); c != 200 {
		t.Fatalf("leader with lease: %d %s", c, b)
	}
	m["raft_lease"] = 0
	if c, b := get(t, srv.URL+"/v1/ready?write=1"); c != 503 || !strings.Contains(b, "lease") {
		t.Fatalf("leader without lease: %d %s", c, b)
	}
	m["raft_lease"], m["raft_writable"] = 1, 0
	if c, _ := get(t, srv.URL+"/v1/ready?write=1"); c != 503 {
		t.Fatalf("not writable: %d", c)
	}
	if c, _ := get(t, srv.URL+"/v1/ready"); c != 200 {
		t.Fatalf("plain readiness of a follower: %d", c)
	}
}

// F11: the lag of an invalidated CDC slot is 0 and stask_cdc_slot_invalid is 1.
func TestInvalidatedSlotLagStopsGrowing(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, SlotMaxLagBytes: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer(New(planner.Start(e), nil).Handler())
	defer srv.Close()
	sess := sql.NewSession(e)
	exec := func(q string) {
		t.Helper()
		st, err := sql.ParseOne(q)
		if err == nil {
			_, err = sess.Exec(st, nil)
		}
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT)`)
	if _, err := e.CreateSlot("s", "test_decoding"); err != nil {
		t.Fatal(err)
	}
	if _, m := get(t, srv.URL+"/metrics"); !strings.Contains(m, `stask_cdc_slot_invalid{slot="s"} 0`) {
		t.Fatalf("healthy slot not reported as valid:\n%s", m)
	}
	pad := strings.Repeat("x", 500)
	for i := 0; i < 20; i++ {
		exec(`INSERT INTO t VALUES (` + string(rune('0'+i/10)) + string(rune('0'+i%10)) + `, '` + pad + `')`)
	}
	if _, err := e.GCRound(); err != nil {
		t.Fatal(err)
	}
	_, m := get(t, srv.URL+"/metrics")
	if !strings.Contains(m, `stask_cdc_slot_invalid{slot="s"} 1`) || !strings.Contains(m, `stask_cdc_slot_lag_bytes{slot="s"} 0`) {
		t.Fatalf("invalidated slot:\n%s", grepLines(m, "cdc_slot"))
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
