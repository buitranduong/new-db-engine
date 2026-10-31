// stask-m2: acceptance harness for the vertical slice (docs/30 §9, docs/32): multi-table update transactions, point reads,
// R2 quota pages with joins and an import running together, with correctness oracles, a crash drill and a barrier comparison
// against postgres:16. It talks to ANY PostgreSQL-wire server. All numbers it prints are measurements, not claims.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+f+"\n", a...) }

func parseFlags() *Config {
	c := &Config{}
	flag.StringVar(&c.SUT, "sut", "postgres://postgres:wm@127.0.0.1:28790/postgres?sslmode=disable", "DSN of the server under test")
	flag.StringVar(&c.RefDSN, "ref", "postgres://postgres:wm@127.0.0.1:28791/postgres?sslmode=disable", "DSN of the reference postgres:16 (journal replay target)")
	flag.BoolVar(&c.RefDocker, "ref-docker", false, "start a throw-away postgres:16 container named m2h-ref on the port of -ref and remove it at the end")
	flag.BoolVar(&c.KeepRef, "keep-ref", false, "keep the reference container")
	flag.BoolVar(&c.NoRef, "no-ref", false, "skip the barrier comparison")
	flag.StringVar(&c.Label, "label", "sut", "name of the server under test in the report")
	flag.StringVar(&c.Out, "out", "m2-report", "output directory (report.json, report.md)")
	flag.StringVar(&c.QueriesDir, "queries", "scripts/workload-multi/queries", "directory with q01..q15")
	flag.StringVar(&c.SchemaFile, "schema", "scripts/workload-multi/schema.sql", "schema file")
	flag.IntVar(&c.N, "n", 1000000, "number of sims (1M smoke, 10M real run)")
	flag.StringVar(&c.Seed, "seed", "stask1", "data seed")
	flag.IntVar(&c.HistPerSim, "hist-per-sim", 3, "history rows per sim")
	flag.DurationVar(&c.Duration, "duration", 60*time.Second, "length of the concurrent phase")
	flag.IntVar(&c.T, "t", 4, "T workers: multi-table transactions")
	flag.IntVar(&c.P, "p", 4, "P workers: R1 point reads and keyset pages")
	flag.IntVar(&c.R, "r", 2, "R workers: R2 quota pages 1-3 and join queries")
	flag.IntVar(&c.ThinkT, "think-t", 2, "ms think time of T workers")
	flag.IntVar(&c.ThinkR, "think-r", 2, "ms think time of R/P workers")
	flag.IntVar(&c.ImportEvery, "import-every", 5, "seconds between imports (1% of the sims in one transaction)")
	flag.StringVar(&c.ImportMode, "import-mode", "alternate", "alternate (second half of the run, priority probe) | always | never")
	flag.StringVar(&c.PhaseFirst, "phase-first", "noimport", "alternate: which phase comes first (noimport|import)")
	flag.IntVar(&c.Probes, "probes", 10000, "read-your-writes probes")
	flag.Float64Var(&c.HotFrac, "hot-frac", 0.3, "fraction of T sim picks taken from the hot set (deliberate conflicts)")
	flag.IntVar(&c.Hot, "hot", 200, "size of the hot set")
	flag.BoolVar(&c.SkipLoad, "skip-load", false, "reuse the data already loaded in the SUT (the SUT journal must be empty)")
	flag.BoolVar(&c.LoadOnly, "load-only", false, "load and exit")
	flag.DurationVar(&c.CrashAt, "crash-at", 0, "crash drill: run -kill-cmd this long after the start (0 = off)")
	flag.StringVar(&c.KillCmd, "kill-cmd", "", "shell command that kills -9 the server under test")
	flag.StringVar(&c.RestartCmd, "restart-cmd", "", "shell command that restarts it (must return once started)")
	flag.DurationVar(&c.RestartWait, "restart-wait", 180*time.Second, "how long to wait for the SUT to accept connections after restart")
	flag.DurationVar(&c.OracleEvery, "oracle-every", 2*time.Second, "period of the oracle worker")
	flag.BoolVar(&c.Force, "force", false, "ignore the gate and disk guards")
	flag.StringVar(&c.GateLog, "gate-log", "/tmp/gate-integ21.log", "merge gate log: refuse sizes above 100000 while it does not end with GATE_EXIT")
	flag.StringVar(&c.DiskPath, "disk-path", "/", "path whose free space is checked (>= -min-free-gb)")
	flag.Float64Var(&c.MinFreeGB, "min-free-gb", 3, "")
	flag.StringVar(&c.LoadMode, "load-mode", "auto", "copy | insert | auto")
	flag.StringVar(&c.Exec, "exec-mode", "simple", "simple | extended")
	flag.BoolVar(&c.Columnar, "columnar", false, "create sims WITH (columnar = on) in the SUT (needed for staskd -hybrid)")
	flag.IntVar(&c.Tear, "tear", 1, "tear-test readers: REPEATABLE READ transactions reading an R2 page and the multi-table invariants at one snapshot")
	flag.IntVar(&c.ProbeWorkers, "probe-workers", 1, "read-your-writes probe workers (each owns a disjoint share of the probe sims)")
	flag.DurationVar(&c.ProbeGap, "probe-gap", -1, "pause between probes (-1: spread over 90% of the duration; 0: back to back)")
	flag.StringVar(&c.RefName, "ref-name", "m2h-ref", "name of the reference container (use a unique one when several harnesses run)")
	flag.StringVar(&c.Iso, "iso", "", "transaction isolation level of T workers (default: server default)")
	flag.Parse()
	return c
}

func freeGB(path string) float64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 1e9
	}
	return float64(st.Bavail) * float64(st.Bsize) / 1e9
}

func gateBusy(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	t := string(b)
	if len(t) > 400 {
		t = t[len(t)-400:]
	}
	return !strings.Contains(t, "GATE_EXIT")
}

func load1() float64 {
	b, _ := os.ReadFile("/proc/loadavg")
	f, _ := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
	return f
}

// busyProcs lists build/test/gate processes (the idea of scripts/perf-run.sh).
func busyProcs() []string {
	out, err := exec.Command("ps", "-eo", "comm=").Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l == "go" || strings.HasSuffix(l, ".test") || l == "claimcheck" || l == "pgdiff" || l == "vet" || l == "compile" || l == "link" {
			seen[l] = true
		}
	}
	var r []string
	for k := range seen {
		r = append(r, k)
	}
	sort.Strings(r)
	return r
}

type Machine struct {
	CPUs      int      `json:"cpus"`
	Load1At0  float64  `json:"load1_at_start"`
	Load1Max  float64  `json:"load1_max"`
	Load1End  float64  `json:"load1_end"`
	Busy      []string `json:"other_builds_seen,omitempty"`
	Dirty     bool     `json:"dirty"`
	DirtyWhy  []string `json:"dirty_reasons,omitempty"`
	FreeGBEnd float64  `json:"disk_free_gb_end"`
}

var cleanup = func() {}

func main() {
	cfg := parseFlags()
	if cfg.Exec == "extended" {
		execMode = pgx.QueryExecModeExec
	}
	if cfg.Iso != "" {
		isoLevel = pgx.TxIsoLevel(cfg.Iso)
	}
	ctx := context.Background()
	os.MkdirAll(cfg.Out, 0o755)
	g := NewGen(cfg.N, cfg.Seed, cfg.HistPerSim)

	// guards
	if freeGB(cfg.DiskPath) < cfg.MinFreeGB && !cfg.Force {
		logf("refuse: %.1f GB free on %s (< %.1f)", freeGB(cfg.DiskPath), cfg.DiskPath, cfg.MinFreeGB)
		cleanup()
		os.Exit(2)
	}
	if gateBusy(cfg.GateLog) && cfg.N > 100000 && !cfg.Force {
		logf("refuse: %s does not end with GATE_EXIT (a merge gate is running); use n <= 100000 or -force", cfg.GateLog)
		cleanup()
		os.Exit(2)
	}
	if g.TotalRows() > 2_000_000 {
		f, err := os.OpenFile("/tmp/stask-bigdata.lock", os.O_CREATE|os.O_RDWR, 0o644)
		if err == nil {
			logf("%d rows > 2M: taking flock /tmp/stask-bigdata.lock (released at exit)", g.TotalRows())
			syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
			defer f.Close()
		}
	}
	mach := Machine{CPUs: numCPU(), Load1At0: load1()}
	mach.Busy = busyProcs()
	if mach.Load1At0 > 1.0 {
		mach.DirtyWhy = append(mach.DirtyWhy, fmt.Sprintf("load1 at start %.2f > 1.0", mach.Load1At0))
	}
	if len(mach.Busy) > 0 {
		mach.DirtyWhy = append(mach.DirtyWhy, "other builds/tests running at start: "+strings.Join(mach.Busy, ","))
	}
	mach.Load1Max = mach.Load1At0

	ColumnarSims = cfg.Columnar
	// reference
	if !cfg.NoRef && cfg.RefDocker {
		port := "28791"
		if i := strings.LastIndex(strings.SplitN(cfg.RefDSN, "/", 4)[2], ":"); i >= 0 {
			port = strings.SplitN(cfg.RefDSN, "/", 4)[2][i+1:]
		}
		exec.Command("docker", "rm", "-f", cfg.RefName).Run()
		if out, err := exec.Command("docker", "run", "-d", "--name", cfg.RefName, "-e", "POSTGRES_PASSWORD=wm", "-p", "127.0.0.1:"+port+":5432", "postgres:16").CombinedOutput(); err != nil {
			logf("cannot start reference: %v %s", err, out)
			cleanup()
			os.Exit(2)
		}
		if !cfg.KeepRef {
			cleanup = func() { exec.Command("docker", "rm", "-f", "-v", cfg.RefName).Run() }
			defer cleanup()
		}
		waitUp(ctx, cfg.RefDSN, 90*time.Second)
	}

	if !waitUp(ctx, cfg.SUT, 60*time.Second) {
		logf("SUT does not accept connections at %s", cfg.SUT)
		cleanup()
		os.Exit(2)
	}
	// load
	if !cfg.SkipLoad {
		t0 := time.Now()
		logf("loading %d sims (%d rows) into the SUT", cfg.N, g.TotalRows())
		if err := Load(ctx, cfg.SUT, g, cfg.SchemaFile, cfg.LoadMode, logf); err != nil {
			logf("load SUT failed: %v", err)
			cleanup()
			os.Exit(2)
		}
		logf("SUT loaded in %.1fs", time.Since(t0).Seconds())
	}
	if !cfg.NoRef {
		t0 := time.Now()
		ColumnarSims = false
		if err := Load(ctx, cfg.RefDSN, g, cfg.SchemaFile, "copy", logf); err != nil {
			logf("load reference failed: %v", err)
			cleanup()
			os.Exit(2)
		}
		logf("reference loaded in %.1fs", time.Since(t0).Seconds())
	}
	if cfg.LoadOnly {
		return
	}

	run := &Run{cfg: cfg, g: g, sut: cfg.SUT, counters: map[string]int64{}, oracle: map[string]*OracleRes{}}
	run.phase.Store("all")
	// baseline of stock.reserved before any T transaction
	var baseline int64
	{
		c, err := Connect(ctx, cfg.SUT)
		if err != nil {
			logf("connect: %v", err)
			cleanup()
			os.Exit(2)
		}
		v, err := one(ctx, c, "SELECT sum(reserved) FROM stock")
		if err != nil {
			logf("baseline: %v", err)
			cleanup()
			os.Exit(2)
		}
		baseline, _ = strconv.ParseInt(strings.Split(v[0], ".")[0], 10, 64)
		c.Close(ctx)
	}
	run.baseline = baseline
	probes := g.ProbeSims(2000)
	logf("starting: T=%d P=%d R=%d import=%s duration=%s probes=%d (probe sims %d)", cfg.T, cfg.P, cfg.R, cfg.ImportMode, cfg.Duration, cfg.Probes, len(probes))

	wctx, stop := context.WithTimeout(ctx, cfg.Duration)
	defer stop()
	run.start = time.Now()
	var wg sync.WaitGroup
	spawn := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	for i := 0; i < cfg.T; i++ {
		i := i
		spawn(func() { run.workerT(wctx, i) })
	}
	for i := 0; i < cfg.P; i++ {
		i := i
		spawn(func() { run.workerP(wctx, i) })
	}
	for i := 0; i < cfg.R; i++ {
		i := i
		spawn(func() { run.workerR(wctx, i) })
	}
	spawn(func() { run.workerImport(wctx) })
	spawn(func() { run.workerOracle(wctx, baseline) })
	for i := 0; i < cfg.Tear; i++ {
		i := i
		spawn(func() { run.workerTear(wctx, i, baseline) })
	}
	for i := 0; i < max(1, cfg.ProbeWorkers); i++ {
		i := i
		spawn(func() { run.workerProbe(wctx, probes, i, max(1, cfg.ProbeWorkers)) })
	}

	// phases
	type PhaseSpan struct {
		Name   string  `json:"name"`
		StartS float64 `json:"start_s"`
		EndS   float64 `json:"end_s"`
	}
	var spans []PhaseSpan
	switch cfg.ImportMode {
	case "alternate":
		a, b := "noimport", "import"
		if cfg.PhaseFirst == "import" {
			a, b = b, a
		}
		h := cfg.Duration.Seconds() / 2
		run.phase.Store(a)
		spans = []PhaseSpan{{a, 0, h}, {b, h, cfg.Duration.Seconds()}}
		time.AfterFunc(cfg.Duration/2, func() { run.phase.Store(b) })
	case "always":
		run.phase.Store("import")
		spans = []PhaseSpan{{"import", 0, cfg.Duration.Seconds()}}
	default:
		run.phase.Store("noimport")
		spans = []PhaseSpan{{"noimport", 0, cfg.Duration.Seconds()}}
	}

	// machine sampler
	spawn(func() {
		for sleepCtx(wctx, 2*time.Second) {
			l := load1()
			if l > mach.Load1Max {
				mach.Load1Max = l
			}
			for _, b := range busyProcs() {
				found := false
				for _, x := range mach.Busy {
					found = found || x == b
				}
				if !found {
					mach.Busy = append(mach.Busy, b)
					mach.DirtyWhy = append(mach.DirtyWhy, "other build/test process started during the run: "+b)
				}
			}
		}
	})

	// crash drill
	var crash *CrashInfo
	if cfg.CrashAt > 0 {
		spawn(func() {
			if !sleepCtx(wctx, cfg.CrashAt) {
				return
			}
			crash = run.crashDrill(ctx, wctx)
		})
	}
	wg.Wait()
	elapsed := time.Since(run.start)
	mach.Load1End = load1()
	mach.FreeGBEnd = freeGB(cfg.DiskPath)
	logf("run finished after %.1fs; final checks", elapsed.Seconds())

	// final checks on the quiescent SUT
	jr, err := readJournal(ctx, cfg.SUT)
	if err != nil {
		run.Check("journal_readable_final", false, "%v", err)
	}
	finalAcked := map[int64]string{}
	if crash == nil {
		run.acked.Range(func(k, v any) bool { finalAcked[k.(int64)] = "x"; return true })
	} else {
		for k, v := range crash.Acked {
			finalAcked[k] = v
		}
	}
	run.JournalChecks(ctx, jr, "final", finalAcked)
	if c, err := Connect(ctx, cfg.SUT); err == nil {
		s := &Sess{dsn: cfg.SUT, conn: c}
		run.oraclePass(ctx, s, baseline, "final")
		s.Drop()
	} else {
		run.Check("final_connect", false, "%v", err)
	}

	var barrier []QCompare
	var replayInfo map[string]any
	if !cfg.NoRef {
		logf("replaying %d journal transactions on the reference", len(jr))
		t0 := time.Now()
		ap, dv, first, err := Replay(ctx, cfg.RefDSN, jr)
		replayInfo = map[string]any{"journal_txns": len(jr), "applied": ap, "outcome_divergences": dv, "first_divergence": first, "seconds": time.Since(t0).Seconds()}
		if err != nil {
			replayInfo["error"] = err.Error()
			run.Check("replay_ok", false, "%v", err)
		}
		run.Check("replay_outcomes_equal_journal", dv == 0 && err == nil, "%d replayed txns had another outcome than on the SUT; first: %s", dv, first)
		barrier = Barrier(ctx, cfg, cfg.SUT, cfg.RefDSN)
		for _, q := range barrier {
			run.Check("barrier_"+q.Query, q.Equal, "%s %s%s", q.Diff, q.ErrSUT, q.ErrRef)
		}
	}
	writeReport(cfg, run, mach, spans, elapsed, crash, barrier, replayInfo, len(jr))
	fails := 0
	for _, o := range run.oracle {
		fails += o.Fails
	}
	if fails > 0 {
		logf("ORACLE VIOLATIONS: %d (see %s/report.md)", fails, cfg.Out)
		cleanup()
		os.Exit(1)
	}
	logf("all oracles held")
}

func numCPU() int {
	b, _ := os.ReadFile("/proc/cpuinfo")
	return strings.Count(string(b), "processor\t:")
}

func waitUp(ctx context.Context, dsn string, d time.Duration) bool {
	dl := time.Now().Add(d)
	for time.Now().Before(dl) {
		if c, err := Connect(ctx, dsn); err == nil {
			_, err = c.Exec(ctx, "SELECT 1")
			c.Close(ctx)
			if err == nil {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------- crash drill

type CrashInfo struct {
	AtS         float64          `json:"at_s"`
	KillOut     string           `json:"kill_output"`
	RestartMs   int64            `json:"recovery_ms"`
	Recovered   bool             `json:"recovered"`
	AckedBefore int              `json:"acked_before_kill"`
	Unknown     int              `json:"unknown_outcome_in_flight"`
	Present     int              `json:"unknown_that_are_present"`
	Acked       map[int64]string `json:"-"`
}

func (r *Run) crashDrill(ctx, wctx context.Context) *CrashInfo {
	ci := &CrashInfo{Acked: map[int64]string{}}
	if r.cfg.KillCmd == "" || r.cfg.RestartCmd == "" {
		logf("crash drill: -kill-cmd and -restart-cmd are required")
		return nil
	}
	mark := time.Now()
	ci.AtS = mark.Sub(r.start).Seconds()
	r.acked.Range(func(k, v any) bool {
		if v.(time.Time).Before(mark) {
			kind, _ := r.ackedKind.Load(k)
			ci.Acked[k.(int64)], _ = kind.(string)
		}
		return true
	})
	ci.AckedBefore = len(ci.Acked)
	logf("CRASH: kill -9 after %.1fs with %d acknowledged transactions", ci.AtS, ci.AckedBefore)
	out, _ := exec.Command("sh", "-c", r.cfg.KillCmd).CombinedOutput()
	ci.KillOut = strings.TrimSpace(string(out))
	t0 := time.Now()
	out, err := exec.Command("sh", "-c", r.cfg.RestartCmd).CombinedOutput()
	if err != nil {
		logf("restart command failed: %v %s", err, out)
	}
	ci.Recovered = waitUp(ctx, r.sut, r.cfg.RestartWait)
	ci.RestartMs = time.Since(t0).Milliseconds()
	logf("recovered=%v after %d ms", ci.Recovered, ci.RestartMs)
	r.Check("server_recovers_after_kill9", ci.Recovered, "SUT did not accept connections within %s", r.cfg.RestartWait)
	if ci.Recovered {
		r.gate.Lock() // no transaction may start while the journal and the tables are compared
		defer r.gate.Unlock()
		jr, err := readJournal(ctx, r.sut)
		if err != nil {
			r.Check("journal_readable_after_crash", false, "%v", err)
		} else {
			r.JournalChecks(ctx, jr, "after_crash", ci.Acked)
			inJ := map[int64]bool{}
			for _, j := range jr {
				inJ[j.TxID] = true
			}
			r.unknown.Range(func(k, v any) bool {
				ci.Unknown++
				if inJ[k.(int64)] {
					ci.Present++
				}
				return true
			})
		}
		if c, err := Connect(ctx, r.sut); err == nil {
			s := &Sess{dsn: r.sut, conn: c}
			r.oraclePass(ctx, s, r.baselineFor(), "after_crash")
			s.Drop()
		}
	}
	return ci
}

func (r *Run) baselineFor() int64 { return r.baseline }

var _ = filepath.Join
var _ = json.Marshal
