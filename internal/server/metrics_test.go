package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMetricsAndScrubEndpoints(t *testing.T) {
	srv, _ := authServer(t, SecurityConfig{Tokens: "full-tok", ROTokens: "ro-tok"})
	hc := srv.Client()
	do := func(t *testing.T, hc *http.Client, method, url, tok, body string) *resp {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		r, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return &resp{r.StatusCode, r.Header, b}
	}
	// /metrics needs a token, a read-only one is enough
	if r := do(t, hc, "GET", srv.URL+"/metrics", "", ""); r.StatusCode != 401 {
		t.Fatalf("anonymous /metrics: %d", r.StatusCode)
	}
	r := do(t, hc, "GET", srv.URL+"/metrics", "ro-tok", "")
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("/metrics: %d %q", r.StatusCode, r.Header.Get("Content-Type"))
	}
	b := r.body
	for _, want := range []string{"# TYPE stask_commits_total counter", "stask_up 1", "stask_sql_statement_duration_seconds_bucket{le=\"+Inf\"}",
		"stask_scrub_running 0", "stask_pg_auth_failures_total"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in\n%s", want, b)
		}
	}
	// every non-comment line is "name[{labels}] value"
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(ln, "#") {
			continue
		}
		if f := strings.Fields(ln); len(f) != 2 {
			t.Fatalf("malformed sample %q", ln)
		}
	}

	// starting a scrub is a write: a read-only token is refused
	if r := do(t, hc, "POST", srv.URL+"/v1/admin/scrub", "ro-tok", ""); r.StatusCode != 403 {
		t.Fatalf("RO scrub start: %d", r.StatusCode)
	}
	r = do(t, hc, "POST", srv.URL+"/v1/admin/scrub?wait=1&rate_mb=0", "full-tok", "")
	var rep struct {
		Running  bool
		Errors   []string
		Segments int
	}
	json.Unmarshal(r.body, &rep)
	if r.StatusCode != 200 || rep.Running || len(rep.Errors) != 0 {
		t.Fatalf("scrub: %d %+v", r.StatusCode, rep)
	}
	if r := do(t, hc, "GET", srv.URL+"/v1/admin/scrub", "ro-tok", ""); r.StatusCode != 200 {
		t.Fatalf("scrub status: %d", r.StatusCode)
	}
	if r := do(t, hc, "POST", srv.URL+"/v1/admin/scrub?rate_mb=x", "full-tok", ""); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad rate: %d", r.StatusCode)
	}
}

type resp struct {
	StatusCode int
	Header     http.Header
	body       []byte
}
