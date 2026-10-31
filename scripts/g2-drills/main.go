// g2-drills runs the failure drills of gate G2 (docs/33_incident_runbook.md):
// each drill starts its own staskd processes in its own directory, injects a
// failure, and ends with PASS or FAIL decided by a correctness oracle, not by
// "the process came back". It signals only processes it started. No
// performance is measured; the restore times printed by D5 are indicative
// and labelled with the load they were taken under.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Opts struct {
	Cycles  int
	Rows    int
	Seed    int64
	Writers int
	Keep    bool
	Out     string
	LoadAvg string
}

// Result collects the checks of one drill.
type Result struct {
	Drill, Mode string
	Fails       []string
	Lines       []string
	Oracle      []string
	Skipped     string
	Start       time.Time
}

func (r *Result) Logf(f string, a ...any) {
	s := fmt.Sprintf(f, a...)
	r.Lines = append(r.Lines, s)
	fmt.Printf("%s   %s %s: %s\n", time.Now().Format("15:04:05"), r.Drill, r.Mode, s)
}

// Check records a failed oracle condition.
func (r *Result) Check(ok bool, f string, a ...any) bool {
	if !ok {
		s := fmt.Sprintf(f, a...)
		r.Fails = append(r.Fails, s)
		fmt.Printf("%s   %s %s: CHECK FAILED: %s\n", time.Now().Format("15:04:05"), r.Drill, r.Mode, s)
	}
	return ok
}

// OracleOK records what the oracle verified (printed on PASS).
func (r *Result) OracleOK(f string, a ...any) { r.Oracle = append(r.Oracle, fmt.Sprintf(f, a...)) }

type Drill struct {
	ID    string
	Desc  string
	Modes []string
	Run   func(e *Env, o *Opts, r *Result, mode string)
}

var drills = []Drill{
	{"D1", "kill -9 repeated at random points under a bank workload", []string{"single", "cluster"}, drillD1},
	{"D2", "kill -9 of the Raft leader and of a follower under load, then rejoin", []string{"cluster"}, drillD2},
	{"D3", "disk full: writes refused with a clear error, recovery, no corruption", []string{"single", "cluster"}, drillD3},
	{"D4", "corruption: flipped byte in log, checkpoint, view, index and history files", []string{"single", "cluster"}, drillD4},
	{"D5", "backup, encrypted backup, restore and PITR rehearsal on a large table", []string{"single"}, drillD5},
	{"D6", "SIGSTOP of the leader longer than the election timeout, then SIGCONT", []string{"cluster"}, drillD6},
	{"D7", "stalled and slow CDC consumers: slot lag grows, retention is bounded", []string{"single"}, drillD7},
}

var (
	cleanMu  sync.Mutex
	cleanups []func()
)

func addCleanup(f func()) {
	cleanMu.Lock()
	cleanups = append(cleanups, f)
	cleanMu.Unlock()
}

func runCleanups() {
	cleanMu.Lock()
	cs := cleanups
	cleanups = nil
	cleanMu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

func main() {
	var (
		bin     = flag.String("staskd", "", "staskd binary (built from this tree)")
		ctl     = flag.String("staskctl", "", "staskctl binary")
		work    = flag.String("work", "", "work directory (default: a new directory under $TMPDIR, removed at the end)")
		only    = flag.String("drills", "all", "comma list: D1,D2,... or all")
		modes   = flag.String("modes", "all", "single, cluster or all")
		o       = Opts{}
		outFile = flag.String("report", "", "write the report here")
	)
	flag.IntVar(&o.Cycles, "cycles", 10, "D1/D2: kill cycles")
	flag.IntVar(&o.Rows, "rows", 5_000_000, "D5: rows of the large table")
	flag.Int64Var(&o.Seed, "seed", time.Now().UnixNano()%1000000, "random seed (printed; reuse it to repeat the kill points)")
	flag.IntVar(&o.Writers, "writers", 6, "workload writers")
	flag.BoolVar(&o.Keep, "keep", false, "keep the work directory")
	flag.Parse()
	if *bin == "" || *ctl == "" {
		fmt.Fprintln(os.Stderr, "need -staskd and -staskctl")
		os.Exit(2)
	}
	if *work == "" {
		w, err := os.MkdirTemp(os.Getenv("TMPDIR"), "g2drills.")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		*work = w
	}
	os.MkdirAll(*work, 0o755)
	e := &Env{Bin: *bin, Ctl: *ctl, Work: *work, Rng: rand.New(rand.NewSource(o.Seed))}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("interrupted: stopping the nodes this program started and cleaning up")
		for _, n := range e.nodes {
			n.Kill9()
		}
		runCleanups()
		if !o.Keep {
			os.RemoveAll(*work)
		}
		os.Exit(130)
	}()
	fmt.Printf("g2-drills seed=%d work=%s\n", o.Seed, *work)
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		o.LoadAvg = strings.TrimSpace(string(b))
		fmt.Printf("load average at start: %s\n", o.LoadAvg)
	}
	want := map[string]bool{}
	for _, d := range strings.Split(*only, ",") {
		want[strings.ToUpper(strings.TrimSpace(d))] = true
	}
	wantMode := func(m string) bool { return *modes == "all" || *modes == m }
	var results []*Result
	for _, d := range drills {
		if !want["ALL"] && !want[d.ID] {
			continue
		}
		for _, m := range d.Modes {
			if !wantMode(m) {
				continue
			}
			if free := freeMB(*work); free < 3072 {
				fmt.Printf("only %d MB free: stopping (need 3 GB)\n", free)
				os.Exit(2)
			}
			r := &Result{Drill: d.ID, Mode: m, Start: time.Now()}
			fmt.Printf("=== %s (%s): %s\n", d.ID, m, d.Desc)
			func() {
				defer func() {
					if p := recover(); p != nil {
						r.Fails = append(r.Fails, fmt.Sprintf("drill panicked: %v", p))
					}
					e.Stop()
					e.nodes = nil
					runCleanups()
				}()
				d.Run(e, &o, r, m)
			}()
			results = append(results, r)
			status := "PASS"
			switch {
			case r.Skipped != "":
				status = "SKIP"
			case len(r.Fails) > 0:
				status = "FAIL"
			}
			fmt.Printf("%s %s (%s) %.0fs: %s\n", status, d.ID, m, time.Since(r.Start).Seconds(), summarize(r))
		}
	}
	// the report
	var b strings.Builder
	fmt.Fprintf(&b, "g2-drills seed=%d load-average-at-start=%q\n", o.Seed, o.LoadAvg)
	failed := 0
	sort.SliceStable(results, func(i, j int) bool { return results[i].Drill < results[j].Drill })
	for _, r := range results {
		status := "PASS"
		switch {
		case r.Skipped != "":
			status = "SKIP"
		case len(r.Fails) > 0:
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(&b, "%s %s (%s): %s\n", status, r.Drill, r.Mode, summarize(r))
		for _, l := range r.Lines {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	fmt.Print("\n" + b.String())
	if *outFile != "" {
		os.MkdirAll(filepath.Dir(*outFile), 0o755)
		os.WriteFile(*outFile, []byte(b.String()), 0o644)
	}
	if !o.Keep {
		os.RemoveAll(*work)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func summarize(r *Result) string {
	if r.Skipped != "" {
		return r.Skipped
	}
	if len(r.Fails) > 0 {
		return "FAILED CHECKS: " + strings.Join(r.Fails, " | ")
	}
	return "oracle: " + strings.Join(r.Oracle, "; ")
}

func freeMB(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 1 << 30
	}
	return int64(st.Bavail) * int64(st.Bsize) >> 20
}
