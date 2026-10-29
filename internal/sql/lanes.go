package sql

import (
	"fmt"
	"math"
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"syscall"
	"time"
)

// Work lanes: each statement is classified by cost before it runs. Heavy
// ones (multi-row writes, scans, long key lists) execute on a pool of OS
// threads with a lower scheduling priority; point statements run on the
// session's own goroutine at normal priority. When the machine has spare
// CPU the heavy lane uses all of it; when it is saturated the kernel runs
// point reads first, so their latency does not depend on bulk load.
// GOMAXPROCS must exceed the CPU count by the heavy workers (staskd does
// this) so that heavy threads never hold every P.

// heavyRows is the row count above which a statement is heavy.
const heavyRows = 8

// heavyReadKeys is the most key lookups a SELECT may need and stay off the
// heavy lane.
const heavyReadKeys = 256

var heavyQ chan func()

// ScanWorkers is the most goroutines one full-scan aggregate uses (1 =
// sequential). Helpers beyond the first only run on idle heavy-lane threads.
var ScanWorkers = runtime.NumCPU()

// StartHeavyLane starts workers OS threads at the given nice value (Linux;
// elsewhere the threads keep normal priority). workers <= 0 disables the
// lane: every statement runs on its session.
func StartHeavyLane(workers, nice int) {
	if workers <= 0 {
		heavyQ = nil
		return
	}
	heavyQ = make(chan func())
	for i := 0; i < workers; i++ {
		go func() {
			runtime.LockOSThread() // never unlocked: the thread keeps its priority
			if !joinHeavyGroup() && nice > 0 {
				setThreadNice(nice)
			}
			for f := range heavyQ {
				f()
			}
		}()
	}
}

func setThreadNice(nice int) {
	tid := syscall.Gettid()
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, nice); err != nil {
		fmt.Printf("heavy lane: setpriority: %v\n", err)
	}
}

// onHeavyLane runs f on a heavy-lane thread and waits for it.
func onHeavyLane(f func()) {
	done := make(chan any, 1)
	heavyQ <- func() {
		defer func() { done <- recover() }()
		f()
	}
	if p := <-done; p != nil {
		panic(p)
	}
}

// heavy classifies a statement before it runs.
func (s *Session) heavy(st Stmt, params []Datum) bool {
	var table string
	var where Expr
	switch st := st.(type) {
	case *Insert:
		return len(st.Rows) > heavyRows
	case *Update:
		table, where = st.Table, st.Where
	case *Delete:
		table, where = st.Table, st.Where
	case *Select:
		if st.From == "" {
			return false
		}
		table, where = st.From, st.Where
	default:
		return false
	}
	t, err := s.table(table)
	if err != nil {
		return false // fails fast either way
	}
	keys, point, err := keyLookups(t, where, &evalCtx{sc: &scope{t: t}, params: params})
	limit := heavyRows
	if _, ok := st.(*Select); ok {
		limit = heavyReadKeys // reads come first: short ranges stay on the fast lane
	}
	return err == nil && (!point || len(keys) > limit)
}

// spawnHelper runs f as a helper of a statement already on the heavy lane:
// on an idle heavy thread if there is one (never waiting for one: the
// statement keeps working by itself), or on a new goroutine when lanes are
// off.
func spawnHelper(f func()) bool {
	if heavyQ == nil {
		go f()
		return true
	}
	select {
	case heavyQ <- f:
		return true
	default:
		return false
	}
}

// Elastic scan helpers. A parallel aggregate may use cores only while they
// are spare: every 100ms the controller reads the Go scheduler's latency
// histogram (how long runnable goroutines waited for a CPU). Above
// schedTarget at the 99th percentile, the helpers allowed are halved; below
// it, one more is allowed. Helpers check between chunks and the ones
// numbered above the allowance leave, so reads, writes and transactions
// keep their latency and the aggregate gets whatever is left. (CockroachDB
// throttles background work on the same signal.)
var scanHelpers atomic.Int32

func init() { scanHelpers.Store(math.MaxInt32) } // no controller: no limit

// SchedTarget is the scheduler latency (p99 over 100ms) above which scan
// helpers are cut; ScanMinHelpers are kept regardless.
var (
	SchedTarget    = time.Millisecond
	ScanMinHelpers = 0
)

// keepHelper reports whether scan helper w may take another chunk. Helpers
// run only on CPUs nothing else wants: before each chunk (about a
// millisecond of scanning) a helper reads how many goroutines wait for a
// CPU, and while any does it sleeps, which hands its processor to them,
// and looks again. Go has no thread priorities, and a helper otherwise
// keeps its processor until preempted (10ms) while woken reads, commits
// and the committer itself queue behind six scanning goroutines: at 10^8
// rows that held point statements for most of each 400ms aggregate.
//
// The count is read by the helper itself, which holds a processor: a
// separate sampling goroutine (runnableControl) waits for a CPU like the
// others, so it runs, and samples, mostly when the queue has just drained,
// and saw ~0 under exactly this load. CockroachDB likewise has elastic work
// check the scheduler from inside its loop
// (https://www.cockroachlabs.com/blog/goroutine-scheduling-elastic-admission-control-cockroachdb).
func keepHelper(w int) bool {
	if int32(w) > scanHelpers.Load() {
		return false
	}
	if !runnableOK || HelperBackoff <= 0 {
		return true
	}
	s := []metrics.Sample{{Name: runnableMetric}}
	for {
		metrics.Read(s)
		if s[0].Value.Uint64() == 0 {
			return true
		}
		time.Sleep(HelperBackoff)
	}
}

// HelperBackoff is how long a scan helper sleeps when goroutines wait for
// a CPU before it looks again; 0 = helpers never yield.
var HelperBackoff = 100 * time.Microsecond

var runnableOK = runnableMetricOK()

// StartScanControl starts the controller; until then helpers are capped
// only by ScanWorkers.
func StartScanControl() {
	limit := int32(ScanWorkers - 1)
	scanHelpers.Store(limit)
	if runnableOK {
		// helpers yield to waiting goroutines themselves (keepHelper); the
		// sampler only still runs for its other job, capping the heavy lane
		if RunnableThreshold > 0 && HeavyCapCores > 0 {
			go runnableControl(limit)
		}
		return
	}
	go func() {
		s := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
		var prev []uint64
		for range time.Tick(100 * time.Millisecond) {
			metrics.Read(s)
			h := s[0].Value.Float64Histogram()
			p99 := histP99(h, prev)
			prev = append(prev[:0], h.Counts...)
			if a := adm.Load(); a != nil {
				a.adjust(p99)
			}
			cur := scanHelpers.Load()
			switch {
			case p99 > SchedTarget.Seconds():
				scanHelpers.Store(max(cur/2, int32(min(ScanMinHelpers, int(limit)))))
			case cur < limit:
				scanHelpers.Store(cur + 1)
			}
		}
	}()
}

const runnableMetric = "/sched/goroutines/runnable:goroutines" // Go 1.26+

// RunnableThreshold is the average number of runnable goroutines per CPU
// at or above which scan helpers are cut by one; at or below half of it
// they grow by one. 0 uses the scheduler latency controller instead.
var RunnableThreshold = 4.0

// HeavyCapCores is the CPU limit of the heavy group (with CPU groups)
// while runnable goroutines per CPU stay at or above RunnableThreshold; it
// is lifted when they fall to half of it. 0 = never capped.
var HeavyCapCores = 0.0

// HeavyCapPeriod is the cgroup bandwidth period of that cap.
var HeavyCapPeriod = 10 * time.Millisecond

func runnableMetricOK() bool {
	for _, d := range metrics.All() {
		if d.Name == runnableMetric {
			return true
		}
	}
	return false
}

// runnableControl sizes scan helpers as CockroachDB sizes its KV admission
// slots (kvSlotAdjuster): goroutines that are ready to run but wait for a
// processor show CPU overload directly, without the delay of a latency
// percentile. Sampled every millisecond and averaged over 10ms, the count
// per CPU cuts a helper when at or above RunnableThreshold and adds one
// when at or below half of it; in between nothing changes, so the helper
// count does not oscillate.
func runnableControl(limit int32) {
	s := []metrics.Sample{{Name: runnableMetric}}
	cpus := float64(runtime.NumCPU())
	floor := int32(min(ScanMinHelpers, int(limit)))
	var sum float64
	n := 0
	capped := false
	for range time.Tick(time.Millisecond) {
		metrics.Read(s)
		sum += float64(s[0].Value.Uint64())
		if n++; n < 10 {
			continue
		}
		perCPU := sum / float64(n) / cpus
		sum, n = 0, 0
		lastRunnable.Store(math.Float64bits(perCPU))
		cur := scanHelpers.Load()
		switch {
		case perCPU >= RunnableThreshold && cur > floor:
			scanHelpers.Store(cur - 1)
		case perCPU <= RunnableThreshold/2 && cur < limit:
			scanHelpers.Store(cur + 1)
		}
		if HeavyCapCores > 0 {
			switch {
			case !capped && perCPU >= RunnableThreshold:
				capHeavy(HeavyCapCores)
				capped = true
				heavyCapped.Add(1)
			case capped && perCPU <= RunnableThreshold/2:
				capHeavy(0)
				capped = false
			}
		}
	}
}

var heavyCapped atomic.Int64 // times the heavy group was capped

var lastRunnable atomic.Uint64 // float64 bits: runnable goroutines per CPU, last 10ms

// RuntimeStats logs, every interval, what the Go runtime cost: the share of
// CPU time spent in garbage collection, the p99 of GC stop-the-world
// pauses and of scheduler latency (time runnable goroutines waited), and
// the helpers a full-scan aggregate may use. These are the costs a
// runtime without GC or with priority scheduling would not have.
func RuntimeStats(interval time.Duration, logf func(string, ...any)) {
	s := []metrics.Sample{
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/cpu/classes/total:cpu-seconds"},
		{Name: "/sched/pauses/total/gc:seconds"},
		{Name: "/sched/latencies:seconds"},
	}
	var gc0, tot0 float64
	var pPrev, lPrev []uint64
	for range time.Tick(interval) {
		metrics.Read(s)
		gc, tot := s[0].Value.Float64(), s[1].Value.Float64()
		ph, lh := s[2].Value.Float64Histogram(), s[3].Value.Float64Histogram()
		share := 0.0
		if tot > tot0 {
			share = 100 * (gc - gc0) / (tot - tot0)
		}
		logf("runtime: gc cpu %.1f%%  gc pause p99 %.3f ms  sched latency p99 %.3f ms  scan helpers %d  runnable/cpu %.2f  heavy caps %d",
			share, 1000*histP99(ph, pPrev), 1000*histP99(lh, lPrev), scanHelpers.Load(), math.Float64frombits(lastRunnable.Load()), heavyCapped.Load())
		gc0, tot0 = gc, tot
		pPrev, lPrev = append(pPrev[:0], ph.Counts...), append(lPrev[:0], lh.Counts...)
	}
}

// histP99 is the 99th percentile of the counts added to h since prev.
func histP99(h *metrics.Float64Histogram, prev []uint64) float64 {
	var n uint64
	for i, c := range h.Counts {
		if i < len(prev) {
			c -= prev[i]
		}
		n += c
	}
	if n == 0 {
		return 0
	}
	t := uint64(math.Ceil(0.99 * float64(n)))
	var acc uint64
	for i, c := range h.Counts {
		if i < len(prev) {
			c -= prev[i]
		}
		if acc += c; acc >= t {
			return h.Buckets[i+1]
		}
	}
	return 0
}
