package histstore

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"stask/internal/planner"
	"stask/internal/server"
)

// The HTTP API uses the store transparently: /v1/get?as_of_time= beyond the
// log, /v1/history across the log/store boundary, and the health lines in
// /metrics.
func TestHTTPUsesTheStore(t *testing.T) {
	dir := t.TempDir()
	ck := newClock()
	e := openEngine(t, dir, ck)
	r := attachStore(t, e, dir, ck, nil)
	defer r.close(t)
	loadVersions(t, e, ck, 40)
	r.wait(t)
	gcAll(t, e)
	p := planner.Start(e)
	hs := server.New(p, nil)
	hs.HistMetrics = r.c.Metrics
	ts := httptest.NewServer(hs.Handler())
	defer ts.Close()
	key := fmt.Sprintf("090%07d", 3)
	get := func(path string) (int, map[string]any) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	// version 4 was current 5.5 hours after the start
	at := t0.Add(5*time.Hour + 30*time.Minute).Format(time.RFC3339)
	code, m := get("/v1/get?table=sims&key=" + key + "&as_of_time=" + at)
	if code != 200 || m["found"] != true {
		t.Fatalf("as_of_time beyond the log: %d %v", code, m)
	}
	if row, _ := m["row"].(map[string]any); fmt.Sprint(row["price"]) != "4" {
		t.Fatalf("row %v", m["row"])
	}
	// before the key existed: a clean 404, not an error
	if code, _ := get("/v1/get?table=sims&key=" + key + "&as_of_time=" + t0.Add(10*time.Minute).Format(time.RFC3339)); code != 404 {
		t.Fatalf("before the first version: %d", code)
	}
	code, m = get("/v1/history?table=sims&key=" + key + "&limit=100")
	if vs, _ := m["versions"].([]any); code != 200 || len(vs) != 40 {
		t.Fatalf("history through HTTP: %d, %d versions", code, len(vs))
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "stask_histstore_broken 0") || !strings.Contains(string(b), "stask_histstore_segments") {
		t.Fatalf("metrics lack the history store lines:\n%s", b)
	}
}
