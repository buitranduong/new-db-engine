package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/sql"
)

// TestCDCHTTP: create a slot, stream while committing, ack, reconnect and
// get only what was not acknowledged; a second consumer is refused.
func TestCDCHTTP(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := httptest.NewServer(New(&planner.Platform{E: e}, nil).Handler())
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
	post := func(path, body string) int {
		t.Helper()
		r, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	exec(`CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT)`)
	if c := post("/v1/cdc", `{"name":"s"}`); c != http.StatusCreated {
		t.Fatalf("create: %d", c)
	}
	if c := post("/v1/cdc", `{"name":"s"}`); c != http.StatusConflict {
		t.Fatalf("duplicate: %d", c)
	}
	if r, _ := http.Get(srv.URL + "/v1/cdc/nope/stream"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing slot: %d", r.StatusCode)
	}

	type line struct {
		LSN    string
		XID    uint64
		Data   json.RawMessage
		Commit bool
		Error  string
	}
	// stream reads lines until n commits were seen
	stream := func(ctx context.Context, n int) ([]line, *http.Response) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/cdc/s/stream?include-transaction=0", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out []line
		sc := bufio.NewScanner(resp.Body)
		for n > 0 && sc.Scan() {
			var l line
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				t.Fatalf("%q: %v", sc.Text(), err)
			}
			out = append(out, l)
			if l.Commit || l.Error != "" {
				n--
			}
		}
		return out, resp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		exec(`INSERT INTO t VALUES (1, 'a')`)
		exec(`UPDATE t SET v = 'b' WHERE id = 1`)
		exec(`DELETE FROM t WHERE id = 1`)
	}()
	c1, cancel1 := context.WithCancel(ctx)
	got, resp := stream(c1, 3)
	// with include-transaction=0 the commit flag rides on the last change
	var actions []string
	for _, l := range got {
		var d struct{ Action string }
		json.Unmarshal(l.Data, &d)
		actions = append(actions, d.Action)
	}
	if strings.Join(actions, "") != "IUD" && strings.Join(actions, "") != "BICBUCBDC" {
		t.Fatalf("actions %v: %v", actions, got)
	}
	// a second consumer of the same slot is refused while the first streams
	refused, busy := stream(ctx, 1)
	busy.Body.Close()
	if len(refused) != 1 || refused[0].Error != core.ErrSlotBusy.Error() {
		t.Fatalf("second consumer: %v", refused)
	}
	cancel1()
	resp.Body.Close()

	// acknowledge the first transaction; a new stream starts after it
	var first string
	for _, l := range got {
		if l.Commit {
			first = l.LSN
			break
		}
	}
	if c := post("/v1/cdc/s/ack?lsn="+first, ""); c != http.StatusOK {
		t.Fatalf("ack: %d", c)
	}
	var again []line
	for i := 0; i < 100; i++ { // the cancelled stream releases the slot asynchronously
		c2, cancel2 := context.WithCancel(ctx)
		again, resp = stream(c2, 2)
		cancel2()
		resp.Body.Close()
		if len(again) > 0 && again[0].Error == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var commits int
	for _, l := range again {
		if l.Error != "" {
			t.Fatalf("stream after ack: %s", l.Error)
		}
		if l.Commit {
			commits++
		}
	}
	if commits != 2 || again[0].XID == got[0].XID {
		t.Fatalf("after ack: %v", again)
	}
	// The slot is released when the cancelled stream notices it, a moment
	// after the client hung up (the handler runs on its own goroutine), so
	// a DELETE right after the last stream can still find the slot active:
	// 409, as PostgreSQL's pg_drop_replication_slot on an active slot. Once
	// released, the drop must succeed; a slot that is never released fails
	// the test.
	var code int
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest("DELETE", srv.URL+"/v1/cdc/s", nil)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("drop: %v", err)
		}
		r.Body.Close()
		code = r.StatusCode
		if code != http.StatusConflict || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if code != http.StatusOK {
		t.Fatalf("drop: %d (the slot stayed active 5 s after the stream was cancelled)", code)
	}
}
