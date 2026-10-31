// Command suite-cdc exercises STASK's change-capture and event mechanisms
// (pgoutput over the walsender, the HTTP CDC stream, LISTEN/NOTIFY) together
// with concurrent DML and DDL, and checks that they are correct. It is a
// correctness suite: volumes are small and deterministic, and no
// performance number is measured or asserted (a liveness timeout is not a
// performance claim). See docs/26_cdc_events_suite.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// T is the handle of one scenario.
type T struct {
	Name     string
	Env      *Env
	Seed     int64
	mu       sync.Mutex
	logs     []string
	fails    []string
	gaps     []string
	cleanups []func()
	t0       time.Time
}

type failNow struct{}

func (t *T) Helper() {}
func (t *T) Logf(f string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logs = append(t.logs, fmt.Sprintf("%s  %s", time.Since(t.t0).Truncate(time.Millisecond), fmt.Sprintf(f, a...)))
}
func (t *T) Errorf(f string, a ...any) {
	t.mu.Lock()
	t.fails = append(t.fails, fmt.Sprintf(f, a...))
	t.mu.Unlock()
	t.Logf("FAIL: "+f, a...)
}
func (t *T) Fatalf(f string, a ...any) {
	t.Errorf(f, a...)
	panic(failNow{})
}

// Check records a failure (and goes on) when cond is false.
func (t *T) Check(cond bool, f string, a ...any) bool {
	if !cond {
		t.Errorf(f, a...)
	}
	return cond
}

// Require stops the scenario when cond is false.
func (t *T) Require(cond bool, f string, a ...any) {
	if !cond {
		t.Fatalf(f, a...)
	}
}

// Gap records a known difference from PostgreSQL that does not fail the run.
func (t *T) Gap(f string, a ...any) {
	t.mu.Lock()
	t.gaps = append(t.gaps, fmt.Sprintf(f, a...))
	t.mu.Unlock()
	t.Logf("GAP: "+f, a...)
}

func (t *T) OnCleanup(f func()) { t.cleanups = append(t.cleanups, f) }

type scenario struct {
	id   string
	desc string
	fn   func(*T)
}

var scenarios []scenario

func register(id, desc string, fn func(*T)) { scenarios = append(scenarios, scenario{id, desc, fn}) }

func (s scenario) run(env *Env, seed int64, limit time.Duration) (t *T) {
	t = &T{Name: s.id, Env: env, Seed: seed, t0: time.Now()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(failNow); !ok {
					t.Errorf("panic: %v\n%s", r, debug.Stack())
				}
			}
			for i := len(t.cleanups) - 1; i >= 0; i-- {
				t.cleanups[i]()
			}
		}()
		s.fn(t)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Logf("goroutines at the timeout:\n%s", buf)
		t.Errorf("scenario did not finish within %v", limit)
		t.Env.StopAll() // a stuck scenario must not leave its servers behind
	}
	return t
}

func main() {
	bin := flag.String("staskd", "staskd", "staskd binary built from this tree")
	work := flag.String("work", "", "work directory (default: a new directory under $TMPDIR; removed at the end unless -keep)")
	out := flag.String("out", "", "report directory (default: ./cdc-suite-out)")
	only := flag.String("only", "", "regexp of scenario ids to run")
	port := flag.Int("base-port", 29400, "first TCP port used")
	seed := flag.Int64("seed", 1, "seed of the workload generators")
	keep := flag.Bool("keep", false, "keep the work directory")
	limit := flag.Duration("limit", 6*time.Minute, "time limit per scenario")
	flag.Parse()
	if *out == "" {
		*out = "cdc-suite-out"
	}
	os.MkdirAll(*out, 0o755)
	if *work == "" {
		d, err := os.MkdirTemp("", "cdcsuite-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		*work = d
	}
	absBin, _ := filepath.Abs(*bin)
	env := newEnv(absBin, *work, *port)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; env.StopAll(); os.Exit(130) }()
	defer func() {
		env.StopAll()
		if !*keep {
			os.RemoveAll(*work)
		}
	}()

	re := regexp.MustCompile(".*")
	if *only != "" {
		re = regexp.MustCompile(*only)
	}
	sort.Slice(scenarios, func(i, j int) bool { return scenarios[i].id < scenarios[j].id })
	var rep strings.Builder
	fmt.Fprintf(&rep, "CDC and events suite, %s, seed %d, staskd %s\n\n", time.Now().Format(time.RFC3339), *seed, absBin)
	failed := 0
	type row struct{ id, res, dur, desc string }
	var rows []row
	for _, s := range scenarios {
		if !re.MatchString(s.id) {
			continue
		}
		t := s.run(env, *seed, *limit)
		res := "PASS"
		if len(t.fails) > 0 {
			res = "FAIL"
			failed++
		}
		d := time.Since(t.t0).Truncate(100 * time.Millisecond)
		fmt.Printf("%s %-22s %6s  %s\n", res, s.id, d, s.desc)
		for _, f := range t.fails {
			fmt.Printf("     - %s\n", strings.ReplaceAll(strings.TrimSpace(f), "\n", "\n       "))
		}
		rows = append(rows, row{s.id, res, d.String(), s.desc})
		fmt.Fprintf(&rep, "=== %s %s (%s): %s\n", res, s.id, d, s.desc)
		for _, l := range t.logs {
			fmt.Fprintf(&rep, "    %s\n", l)
		}
		rep.WriteString("\n")
	}
	var sum strings.Builder
	sum.WriteString("SUMMARY\n")
	for _, r := range rows {
		fmt.Fprintf(&sum, "%s %-22s %8s  %s\n", r.res, r.id, r.dur, r.desc)
	}
	fmt.Fprintf(&sum, "\n%d scenarios, %d failed\n", len(rows), failed)
	fmt.Print("\n" + sum.String())
	os.WriteFile(filepath.Join(*out, "report.txt"), []byte(sum.String()+"\n"+rep.String()), 0o644)
	fmt.Printf("report: %s\n", filepath.Join(*out, "report.txt"))
	env.StopAll()
	if !*keep {
		os.RemoveAll(*work)
	}
	if failed > 0 {
		os.Exit(1)
	}
}
