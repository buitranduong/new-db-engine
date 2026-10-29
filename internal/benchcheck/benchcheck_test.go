package benchcheck

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestParseSpecs(t *testing.T) {
	s, err := ParseSpecs("orders,columnar,agg,min=10,max=20;t2,row")
	if err != nil || len(s) != 2 {
		t.Fatal(s, err)
	}
	if !s[0].Columnar || !s[0].Agg || s[0].Min != 10 || s[0].Max != 20 || !s[1].Row || s[1].Max != -1 {
		t.Fatalf("%+v", s)
	}
	for _, bad := range []string{"orders,colunar", "min=3", "t,row,columnar", "t,min=x"} {
		if _, err := ParseSpecs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

type fake struct {
	status map[string]any
	engine string
}

func (f *fake) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(f.status) })
	m.HandleFunc("POST /v1/query", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"plan": map[string]any{"engine": f.engine, "reason": "test"}})
	})
	return m
}

func table(columnar, view bool, live float64) map[string]any {
	t := map[string]any{"live_rows": live, "columnar": columnar}
	if view {
		t["columnar_rows"] = live
	}
	return t
}

// gitHead makes the test independent of the checkout: it uses this
// repository's own HEAD (or skips when git is not usable).
func gitHead(t *testing.T) string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skip("no git")
	}
	return strings.TrimSpace(string(out))
}

func harness(t *testing.T, f *fake, expect string, allowStale bool) *Harness {
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o := Register(fs)
	o.StatusURL, o.Expect, o.AllowStale, o.Repo = srv.URL, expect, allowStale, "."
	return New("test", o, fs)
}

func TestColumnarPreconditions(t *testing.T) {
	good := &fake{status: map[string]any{"tables": map[string]any{"orders": table(true, true, 100)}}, engine: "columnar"}
	h := harness(t, good, "orders,agg,min=50,max=200", false)
	h.Preconditions("before")
	if !h.Valid() {
		t.Fatal(h.Reasons())
	}

	// the staskmix bug: the table was re-created as a row table
	row := &fake{status: map[string]any{"tables": map[string]any{"orders": table(false, false, 100)}}, engine: "core"}
	h = harness(t, row, "orders,agg", false)
	h.Preconditions("after")
	if h.Valid() || len(h.Reasons()) < 2 {
		t.Fatalf("row table accepted as columnar: %v", h.Reasons())
	}
	res := map[string]any{}
	if h.Finish(res) != ExitInvalid || res["valid"] != false || res["invalid_reasons"] == nil || res["meta"] == nil {
		t.Fatalf("%v", res)
	}

	// columnar attribute present but aggregates fall back to the core
	fb := &fake{status: good.status, engine: "core"}
	h = harness(t, fb, "orders,agg", false)
	h.Preconditions("after")
	if h.Valid() || !strings.Contains(strings.Join(h.Reasons(), ";"), "not columnar") {
		t.Fatalf("%v", h.Reasons())
	}

	// missing table, row count range
	h = harness(t, good, "nope", false)
	h.Preconditions("before")
	if h.Valid() {
		t.Fatal("missing table accepted")
	}
	h = harness(t, good, "orders,min=1000", false)
	h.Preconditions("before")
	if h.Valid() {
		t.Fatal("row count below min accepted")
	}
	h = harness(t, good, "orders,row", false)
	h.Preconditions("before")
	if h.Valid() {
		t.Fatal("columnar table accepted for a row-path run")
	}
	h = harness(t, row, "orders,row", false)
	h.Preconditions("before")
	if !h.Valid() {
		t.Fatal(h.Reasons())
	}
}

func TestBuildCheck(t *testing.T) {
	head := gitHead(t)
	mk := func(b any) *fake { return &fake{status: map[string]any{"build": b, "tables": map[string]any{}}} }
	cases := []struct {
		name  string
		build any
		ok    bool
	}{
		{"matching", map[string]any{"revision": head}, true},
		{"old commit", map[string]any{"revision": "0000000000000000000000000000000000000000"}, false},
		{"dirty", map[string]any{"revision": head, "modified": true}, false},
		{"no revision", map[string]any{"revision": ""}, false},
		{"no build key (old binary)", nil, false},
	}
	for _, c := range cases {
		f := mk(c.build)
		if c.build == nil {
			delete(f.status, "build")
		}
		h := harness(t, f, "", false)
		h.CheckBuild()
		// a dirty work tree of this repo is reported too; only assert the
		// negative cases and the clean positive when the tree is clean
		if !c.ok && h.Valid() {
			t.Errorf("%s: accepted", c.name)
		}
		if c.ok && !h.Valid() && !h.repoDirt {
			t.Errorf("%s: rejected: %v", c.name, h.Reasons())
		}
		// -allow-stale turns every failure into a warning
		h = harness(t, f, "", true)
		h.CheckBuild()
		if !h.Valid() || (!c.ok && len(h.warnings) == 0) {
			t.Errorf("%s: allow-stale: valid=%v warnings=%v", c.name, h.Valid(), h.warnings)
		}
	}
}

func TestUncheckedAndExpectWithoutServer(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o := Register(fs)
	o.StatusURL, o.Expect = "", "orders,columnar"
	h := New("test", o, fs)
	if h.Valid() {
		t.Fatal("-expect without a server must be invalid")
	}
	o.Expect = ""
	h = New("test", o, fs)
	res := map[string]any{}
	if h.Finish(res) != 0 || res["meta"].(map[string]any)["server_checked"] != false {
		t.Fatalf("%v", res)
	}
}
