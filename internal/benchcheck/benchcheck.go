// Package benchcheck makes the benchmark tools check themselves. Two
// measurement errors motivated it (docs/14): a load tool silently dropped
// and re-created a columnar table as a row table, so 12 aggregate scripts
// measured the row-scan path (14 s instead of 0.18 s); and benchmarks ran
// against a bin/staskd older than the source. The tools now
//
//   - read build.revision/modified from the running staskd (/v1/status) and
//     compare it with git HEAD and the working tree (CheckBuild);
//   - verify declared preconditions before and after the measured run
//     (Preconditions): table exists, columnar attribute, row-count range,
//     and for aggregates that a probe query is answered by plan.engine =
//     columnar;
//   - put a meta block and valid/invalid_reasons into every result JSON
//     (Finish) and exit non-zero when the run is INVALID.
package benchcheck

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"stask/internal/buildinfo"
)

// ExitInvalid is the process exit code of a run that finished but whose
// result must not be used.
const ExitInvalid = 3

// Spec is one declared precondition on a table.
type Spec struct {
	Table    string
	Columnar bool  // the table must have the columnar attribute and a view
	Row      bool  // the table must NOT be columnar (row-path measurement)
	Agg      bool  // an aggregate probe must be answered by the columnar engine
	Min, Max int64 // live row range; Max < 0 = unbounded
}

// ParseSpecs parses "orders,columnar,agg,min=1000,max=5000;other,row".
func ParseSpecs(s string) ([]Spec, error) {
	var out []Spec
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.Split(part, ",")
		sp := Spec{Table: strings.TrimSpace(f[0]), Max: -1}
		if sp.Table == "" || strings.Contains(sp.Table, "=") {
			return nil, fmt.Errorf("expect %q: first item must be the table name", part)
		}
		for _, tok := range f[1:] {
			tok = strings.TrimSpace(tok)
			k, v, hasV := strings.Cut(tok, "=")
			switch {
			case tok == "columnar":
				sp.Columnar = true
			case tok == "row":
				sp.Row = true
			case tok == "agg":
				sp.Agg, sp.Columnar = true, true
			case (k == "min" || k == "max") && hasV:
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("expect %q: %v", part, err)
				}
				if k == "min" {
					sp.Min = n
				} else {
					sp.Max = n
				}
			default:
				return nil, fmt.Errorf("expect %q: unknown item %q", part, tok)
			}
		}
		if sp.Row && sp.Columnar {
			return nil, fmt.Errorf("expect %q: row and columnar/agg contradict", part)
		}
		out = append(out, sp)
	}
	return out, nil
}

// Options are the flags every benchmark tool shares.
type Options struct {
	StatusURL  string // staskd HTTP base, e.g. 127.0.0.1:17073 ("" = server not checked)
	Token      string
	Expect     string
	AllowStale bool
	Repo       string // git work tree the binaries should match
	DataDir    string // directory whose free space is recorded
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// Register adds -stask-http, -check-token, -expect, -allow-stale, -repo and
// -data-dir to fs. Each has an environment default so shell scripts set
// them once.
func Register(fs *flag.FlagSet) *Options {
	o := &Options{}
	fs.StringVar(&o.StatusURL, "stask-http", envOr("STASK_HTTP", ""), "staskd HTTP address (host:port or URL) for the self-checks: build revision, preconditions, aggregate plan (env STASK_HTTP); empty = server NOT checked")
	fs.StringVar(&o.Token, "check-token", os.Getenv("STASK_HTTP_TOKEN"), "bearer token for -stask-http")
	fs.StringVar(&o.Expect, "expect", os.Getenv("BENCH_EXPECT"), "preconditions: table[,columnar][,row][,agg][,min=N][,max=N];... (env BENCH_EXPECT)")
	fs.BoolVar(&o.AllowStale, "allow-stale", os.Getenv("BENCH_ALLOW_STALE") == "1", "do not fail when the server binary does not match git HEAD or was built from a dirty tree (env BENCH_ALLOW_STALE=1)")
	fs.StringVar(&o.Repo, "repo", envOr("BENCH_REPO", "."), "git work tree the binaries must match")
	fs.StringVar(&o.DataDir, "data-dir", envOr("DATA", "."), "directory whose free disk space is recorded in meta")
	return o
}

func (o *Options) base() string {
	u := o.StatusURL
	if u != "" && !strings.Contains(u, "://") {
		u = "http://" + u
	}
	return strings.TrimRight(u, "/")
}

// Harness accumulates the findings of one tool run.
type Harness struct {
	Tool      string
	O         *Options
	fs        *flag.FlagSet
	specs     []Spec
	start     time.Time
	cpu0      time.Duration
	reasons   []string
	warnings  []string
	server    buildinfo.Info
	srvSeen   bool
	repoRev   string
	repoDirt  bool
	checks    []string
	unchecked bool
	http      *http.Client
}

// New creates the harness; call it right after flag.Parse. A malformed
// -expect is fatal (exit 2): a mistyped check must not silently pass.
func New(tool string, o *Options, fs *flag.FlagSet) *Harness {
	h := &Harness{Tool: tool, O: o, fs: fs, start: time.Now(), cpu0: cpuTime(), http: &http.Client{Timeout: 20 * time.Second}}
	specs, err := ParseSpecs(o.Expect)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchcheck:", err)
		os.Exit(2)
	}
	h.specs = specs
	h.refreshUnchecked()
	return h
}

func (h *Harness) refreshUnchecked() {
	h.unchecked = h.O.base() == ""
	if h.unchecked {
		if len(h.specs) > 0 {
			h.invalid("-expect given without -stask-http: preconditions cannot be checked")
		}
		h.warn("%s: -stask-http not set, server build and preconditions are NOT verified", h.Tool)
	}
}

// SetStatusURLDefault supplies the status address for tools whose own -url
// already is the staskd HTTP address, when -stask-http was not given.
func (h *Harness) SetStatusURLDefault(u string) {
	if h.O.StatusURL == "" && u != "" {
		h.O.StatusURL = u
		h.reasons, h.warnings = nil, nil
		h.refreshUnchecked()
	}
}

func (h *Harness) invalid(format string, a ...any) {
	h.reasons = append(h.reasons, fmt.Sprintf(format, a...))
}
func (h *Harness) warn(format string, a ...any) {
	h.warnings = append(h.warnings, fmt.Sprintf(format, a...))
	fmt.Fprintf(os.Stderr, "benchcheck WARNING: %s\n", h.warnings[len(h.warnings)-1])
}

// Status fetches /v1/status.
func (h *Harness) Status() (map[string]any, error) {
	req, _ := http.NewRequest("GET", h.O.base()+"/v1/status", nil)
	if h.O.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.O.Token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET /v1/status: HTTP %d: %.200s", resp.StatusCode, b)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return st, nil
}

func serverBuild(st map[string]any) (buildinfo.Info, bool) {
	raw, ok := st["build"]
	if !ok {
		return buildinfo.Info{}, false
	}
	b, _ := json.Marshal(raw)
	var in buildinfo.Info
	json.Unmarshal(b, &in)
	return in, true
}

func (h *Harness) git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", h.O.Repo}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// CheckBuild fails (or, with -allow-stale, warns) when the running staskd
// was not built from the current git HEAD, was built from a dirty tree, or
// when Go sources have uncommitted changes the binary cannot be proven to
// contain.
func (h *Harness) CheckBuild() {
	if h.unchecked {
		return
	}
	stale := func(format string, a ...any) {
		if h.O.AllowStale {
			h.warn("stale allowed: "+format, a...)
		} else {
			h.invalid(format+" (rebuild with scripts/build-bench.sh, or -allow-stale)", a...)
		}
	}
	st, err := h.Status()
	if err != nil {
		h.invalid("server %s unreachable: %v", h.O.base(), err)
		return
	}
	var ok bool
	h.server, ok = serverBuild(st)
	h.srvSeen = true
	rev, err := h.git("rev-parse", "HEAD")
	if err != nil || rev == "" {
		rev = buildinfo.Read().Revision
		h.warn("git HEAD not readable in %s (%v); comparing the server with this tool's own revision %.12s", h.O.Repo, err, rev)
	} else if d, err := h.git("status", "--porcelain", "--", "*.go", "go.mod", "go.sum"); err == nil && d != "" {
		h.repoDirt = true
		stale("work tree has uncommitted Go source changes the binaries cannot be proven to contain: %.120s", strings.ReplaceAll(d, "\n", "; "))
	}
	h.repoRev = rev
	switch {
	case !ok:
		stale("server has no build info in /v1/status: binary predates this harness")
	case h.server.Revision == "":
		stale("server binary has no vcs.revision (built with go run/test or outside git)")
	case rev != "" && h.server.Revision != rev:
		stale("server binary built from %.12s but source is at %.12s", h.server.Revision, rev)
	case h.server.Modified:
		stale("server binary was built from a dirty tree (vcs.modified=true, revision %.12s)", h.server.Revision)
	default:
		h.checks = append(h.checks, fmt.Sprintf("build: server %.12s = HEAD, clean", h.server.Revision))
	}
	if own := buildinfo.Read(); own.Revision != "" && rev != "" && own.Revision != rev {
		stale("%s itself was built from %.12s, source is at %.12s", h.Tool, own.Revision, rev)
	}
}

// Preconditions checks every declared Spec; phase labels the findings
// ("before"/"after"). Call it before and after the measured part.
func (h *Harness) Preconditions(phase string) {
	if h.unchecked || len(h.specs) == 0 {
		return
	}
	st, err := h.Status()
	if err != nil {
		h.invalid("%s: status unreachable: %v", phase, err)
		return
	}
	tables, _ := st["tables"].(map[string]any)
	for _, sp := range h.specs {
		t, ok := tables[sp.Table].(map[string]any)
		if !ok {
			h.invalid("%s: table %q does not exist", phase, sp.Table)
			continue
		}
		_, hasView := t["columnar_rows"]
		declared, hasDecl := t["columnar"].(bool)
		if !hasDecl {
			declared = hasView // server without the explicit attribute
		}
		switch {
		case sp.Columnar && !(declared && hasView):
			h.invalid("%s: table %q is not columnar (attribute=%v, view present=%v): aggregates would scan rows", phase, sp.Table, declared, hasView)
		case sp.Row && declared:
			h.invalid("%s: table %q is columnar but the run expects the row path", phase, sp.Table)
		}
		live := int64(num(t["live_rows"]))
		if live < sp.Min || (sp.Max >= 0 && live > sp.Max) {
			h.invalid("%s: table %q has %d live rows, expected %d..%s", phase, sp.Table, live, sp.Min, maxStr(sp.Max))
		}
		if sp.Agg {
			h.probeAgg(phase, sp)
		}
		h.checks = append(h.checks, fmt.Sprintf("%s: %s live_rows=%d columnar=%v view_rows=%v", phase, sp.Table, live, declared, t["columnar_rows"]))
	}
}

func maxStr(m int64) string {
	if m < 0 {
		return "inf"
	}
	return strconv.FormatInt(m, 10)
}

// probeAgg runs a cheap aggregate through /v1/query and requires
// plan.engine = columnar.
func (h *Harness) probeAgg(phase string, sp Spec) {
	body, _ := json.Marshal(map[string]any{"kind": "aggregate",
		"aggregate":   map[string]any{"table": sp.Table, "aggs": []map[string]string{{"fn": "count"}}},
		"consistency": map[string]any{"mode": "strong"}})
	req, _ := http.NewRequest("POST", h.O.base()+"/v1/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if h.O.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.O.Token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.invalid("%s: aggregate probe on %q failed: %v", phase, sp.Table, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		h.invalid("%s: aggregate probe on %q: HTTP %d %.160s", phase, sp.Table, resp.StatusCode, b)
		return
	}
	var r struct {
		Plan struct {
			Engine string  `json:"engine"`
			Reason string  `json:"reason"`
			ExecMs float64 `json:"exec_ms"`
		} `json:"plan"`
	}
	json.Unmarshal(b, &r)
	if r.Plan.Engine != "columnar" {
		h.invalid("%s: aggregate on %q was answered by engine %q (%s), not columnar", phase, sp.Table, r.Plan.Engine, r.Plan.Reason)
		return
	}
	h.checks = append(h.checks, fmt.Sprintf("%s: agg probe %s engine=columnar exec_ms=%.2f", phase, sp.Table, r.Plan.ExecMs))
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// Valid reports whether no check has failed so far.
func (h *Harness) Valid() bool { return len(h.reasons) == 0 }

// Reasons returns the failures so far.
func (h *Harness) Reasons() []string { return h.reasons }

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func freeDisk(dir string) int64 {
	var s syscall.Statfs_t
	if syscall.Statfs(dir, &s) != nil {
		return -1
	}
	return int64(uint64(s.Bavail) * uint64(s.Bsize))
}

func loadavg() []float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	f := strings.Fields(string(b))
	var out []float64
	for _, x := range f[:min(3, len(f))] {
		v, _ := strconv.ParseFloat(x, 64)
		out = append(out, v)
	}
	return out
}

// Meta is the measurement context stored with the result.
func (h *Harness) Meta() map[string]any {
	flags := map[string]string{}
	if h.fs != nil {
		h.fs.VisitAll(func(f *flag.Flag) { flags[f.Name] = f.Value.String() })
	}
	wall := time.Since(h.start)
	cpu := cpuTime() - h.cpu0
	host, _ := os.Hostname()
	m := map[string]any{
		"tool":               h.Tool,
		"time":               time.Now().Format(time.RFC3339),
		"host":               host,
		"commit":             h.repoRev,
		"tool_build":         buildinfo.Read(),
		"flags":              flags,
		"num_cpu":            runtime.NumCPU(),
		"client_cpu_cores":   float64(int(cpu.Seconds()/max(wall.Seconds(), 1e-9)*100+0.5)) / 100,
		"load_avg":           loadavg(),
		"free_disk_bytes":    freeDisk(h.O.DataDir),
		"allow_stale":        h.O.AllowStale,
		"server_checked":     !h.unchecked,
		"working_tree_dirty": h.repoDirt,
		"checks_passed":      h.checks,
	}
	if h.srvSeen {
		m["server_build"] = h.server
	}
	return m
}

// Finish stores meta, valid, invalid_reasons and warnings into res, prints
// a loud banner when INVALID, and returns the exit code the tool must use
// after it has written res (0 or ExitInvalid).
func (h *Harness) Finish(res map[string]any) int {
	res["meta"] = h.Meta()
	res["valid"] = h.Valid()
	if len(h.warnings) > 0 {
		res["warnings"] = h.warnings
	}
	if h.Valid() {
		return 0
	}
	res["invalid_reasons"] = h.reasons
	fmt.Fprintf(os.Stderr, "\n*** %s: RUN INVALID - do not use these numbers ***\n", h.Tool)
	for _, r := range h.reasons {
		fmt.Fprintln(os.Stderr, "  - "+r)
	}
	fmt.Fprintln(os.Stderr)
	return ExitInvalid
}

// DefaultExpect installs spec as the preconditions when -expect was not
// given (tools know what their own workload needs).
func (h *Harness) DefaultExpect(spec string) {
	if h.O.Expect != "" {
		return
	}
	specs, err := ParseSpecs(spec)
	if err != nil {
		panic(err)
	}
	h.specs = specs
	h.O.Expect = spec
}

// Skip marks the run as not against a staskd (PostgreSQL, ClickHouse,
// Redis, embedded): no server checks apply; meta says so.
func (h *Harness) Skip(why string) {
	h.unchecked = true
	h.specs = nil
	h.reasons, h.warnings = nil, nil
	h.checks = append(h.checks, "server checks skipped: "+why)
}

// Abort prints a minimal INVALID result and exits when a check failed;
// tools call it after the early checks so a doomed run stops early.
func (h *Harness) Abort(res map[string]any) {
	if h.Valid() {
		return
	}
	code := h.Finish(res)
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	os.Exit(code)
}
