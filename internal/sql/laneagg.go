package sql

import (
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/schema"
)

// Aggregates and the lanes (branch aggfacet).
//
// Three pieces keep facet counts, GROUP BY reports and filtered counts
// well-behaved when -heavy-workers is on:
//
//  1. A statement the columnar view answers (viewAggPlan) is classified by
//     what it costs, not by its keyword: over a view of at most
//     ViewNormalRows live rows, and with a view that is fresh enough for the
//     statement's contract, it runs on the session's own goroutine (a few
//     tens of milliseconds, measured at 1M rows: es_facet_carrier 35 ms).
//     Over a bigger view, or a view that must first catch up, it goes to the
//     heavy lane like any scan.
//  2. The heavy lane is scheduled in user space (heavyGov): a token bucket
//     refilled at HeavyShare of the CPUs. While the node is saturated a
//     heavy worker takes the next statement only when the bucket is not in
//     debt, so heavy work keeps at least that share of the machine (never
//     starved as with nice 10) and point statements keep the rest.
//  3. The classifier's decisions are counted (LaneReport, /v1/status).

// ViewNormalRows is the most live rows of a columnar view an aggregate may
// read on the normal lane. 0 sends every aggregate to the heavy lane.
var ViewNormalRows int64 = 2_000_000

// ViewFreshLag is the view lag at or below which a strong read of the view
// is expected to be answered at once (the view reaches the statement's
// snapshot within about this long).
var ViewFreshLag = 100 * time.Millisecond

// viewInfoer is implemented by the Views of a full platform.
type viewInfoer interface {
	ViewInfo(table string) (rows int64, lag time.Duration, ok bool)
}

// viewAggPlan says whether SELECT st would be answered from the columnar
// view, and how the contract is met. ok=false: row path.
type viewAggPlan struct {
	ok        bool
	rows      int64
	lag       time.Duration
	staleness time.Duration // > 0: stask.read_staleness lets the view answer behind the snapshot
	fresh     bool          // the planner can prove the view meets the contract without a long wait
	reason    string        // when !ok
}

func (s *Session) viewAggPlan(st *Select, t *schema.Table, params []Datum) viewAggPlan {
	if s.Views == nil || !t.Columnar {
		return viewAggPlan{reason: "table has no columnar view"}
	}
	if s.explicit {
		return viewAggPlan{reason: "inside BEGIN ... COMMIT the view may include later commits"}
	}
	if st.Rel != nil || len(st.With) > 0 || len(st.Sets) > 0 || len(st.Windows) > 0 || st.ForUpd || len(st.Lock) > 0 {
		return viewAggPlan{reason: "shape not expressible on the view"}
	}
	var aggs []*Func
	grouped := len(st.GroupBy) > 0
	collect := func(x Expr) {
		walk(x, func(e Expr) {
			if f, ok := e.(*Func); ok && aggFuncs[f.Name] {
				aggs = append(aggs, f)
				grouped = true
			}
		})
	}
	for _, it := range st.Items {
		if it.Star {
			return viewAggPlan{reason: "SELECT *"}
		}
		collect(it.X)
	}
	collect(st.Having)
	for _, o := range st.OrderBy {
		collect(o.X)
	}
	if !grouped {
		return viewAggPlan{reason: "not an aggregate"}
	}
	ctx := &evalCtx{sc: &scope{t: t, alias: st.Alias}, params: params, now: nowMicros(), sess: s}
	if _, ok, err := s.buildViewQuery(st, t, ctx, aggs); err != nil || !ok {
		return viewAggPlan{reason: "grouping, aggregate or filter not expressible on the view"}
	}
	p := viewAggPlan{ok: true, staleness: s.readStaleness()}
	if vi, ok := s.Views.(viewInfoer); ok {
		var have bool
		p.rows, p.lag, have = vi.ViewInfo(t.Name)
		if !have {
			return viewAggPlan{reason: "view is bootstrapping or absent"}
		}
		p.fresh = p.lag <= ViewFreshLag || p.staleness > 0 && p.lag <= p.staleness
	} else {
		p.fresh = true // a view without metadata (tests): trust it
	}
	return p
}

// viewLaneNormal reports whether the aggregate st is cheap and fresh enough
// to run on the normal lane.
func (s *Session) viewLaneNormal(st *Select, params []Datum) bool {
	if ViewNormalRows <= 0 || s.Views == nil {
		return false
	}
	t, err := s.table(st.From)
	if err != nil {
		return false
	}
	p := s.viewAggPlan(st, t, params)
	if !p.ok || !p.fresh {
		return false
	}
	_, isInfo := s.Views.(viewInfoer)
	return !isInfo || p.rows <= ViewNormalRows
}

// ---- classifier observability ----

// Lane counters (LaneReport).
var (
	laneNormalStmts  atomic.Int64
	laneHeavyReads   atomic.Int64
	laneHeavyWrites  atomic.Int64
	laneViewAggs     atomic.Int64 // aggregates kept on the normal lane because the view answers them
	shapeMovesHeavy  atomic.Int64
	shapeMovesNormal atomic.Int64
)

// ---- user-space scheduler of the heavy lane ----

// HeavyShare is the fraction of the CPUs the heavy lane is guaranteed while
// the node is saturated, and the most it takes then. Above it only when the
// node has spare CPU. 0 turns the scheduler off (priority only).
var HeavyShare = 0.25

// heavyGov is a token bucket in CPU-nanoseconds. It is refilled at
// share*cpus per second of wall time; a heavy statement charges the wall time
// of its run; a worker may start another statement while the bucket is not
// in debt, or at once when the node is not saturated. Debt is capped at
// maxDebt, so a worker never waits longer than maxDebt/rate: heavy work is
// delayed, never starved.
type heavyGov struct {
	mu      sync.Mutex
	rate    float64 // ns of CPU per ns of wall time
	tokens  float64 // ns; negative = debt
	burst   float64
	maxDebt float64
	last    time.Time
	now     func() time.Time
	sleep   func(time.Duration)
	sat     func() bool

	waits   atomic.Int64
	waitNs  atomic.Int64
	charged atomic.Int64
}

func newHeavyGov(share float64, cpus int, sat func() bool) *heavyGov {
	g := &heavyGov{rate: share * float64(cpus), now: time.Now, sleep: time.Sleep, sat: sat}
	g.burst = 100e6 * g.rate // 100 ms of the share
	g.maxDebt = 2e9 * g.rate // 2 s of the share
	g.tokens = g.burst
	g.last = g.now()
	return g
}

func (g *heavyGov) refill() {
	n := g.now()
	g.tokens = min(g.burst, g.tokens+float64(n.Sub(g.last))*g.rate)
	g.last = n
}

// wait blocks the calling heavy worker while the node is saturated and the
// bucket is in debt.
func (g *heavyGov) wait() {
	var t0 time.Time
	for {
		g.mu.Lock()
		g.refill()
		debt := -g.tokens
		g.mu.Unlock()
		if debt <= 0 || !g.sat() {
			break
		}
		if t0.IsZero() {
			t0 = g.now()
			g.waits.Add(1)
		}
		d := time.Duration(debt / g.rate)
		g.sleep(min(max(d, 200*time.Microsecond), 20*time.Millisecond))
	}
	if !t0.IsZero() {
		g.waitNs.Add(int64(g.now().Sub(t0)))
	}
}

// charge records ran of heavy execution.
func (g *heavyGov) charge(ran time.Duration) {
	g.mu.Lock()
	// what the share earned while the statement ran offsets its cost, so a
	// statement longer than the burst is not charged for time it had paid for
	n := g.now()
	g.tokens = max(-g.maxDebt, min(g.burst, g.tokens+float64(n.Sub(g.last))*g.rate-float64(ran)))
	g.last = n
	g.mu.Unlock()
	g.charged.Add(int64(ran))
}

var govP atomic.Pointer[heavyGov]

// fgInFlight counts statements running on their session's goroutine.
var fgInFlight atomic.Int64

func nodeSaturated() bool {
	return fgInFlight.Load() >= int64(max(2, runtime.NumCPU()))
}

// AutoHeavyWorkers is the heavy-lane size chosen when -heavy-workers is not
// given: none below 4 CPUs (a lane cannot help there), else a quarter of the
// CPUs, between 1 and 4. UNMEASURED end to end (docs/14 Phụ lục AF).
func AutoHeavyWorkers(cpus int) int {
	if cpus < 4 {
		return 0
	}
	return min(4, max(1, cpus/4))
}

func startGovernor() {
	if HeavyShare > 0 {
		govP.Store(newHeavyGov(HeavyShare, runtime.NumCPU(), nodeSaturated))
	} else {
		govP.Store(nil)
	}
}

// ---- LaneReport ----

// LaneReport is the /v1/status view of the lanes: counters of the
// classifier, the scheduler and the shapes currently on the heavy lane.
func LaneReport() map[string]any {
	var heavy []map[string]any
	slowShapes.Range(func(k, v any) bool {
		ss := v.(*shapeStat)
		if ss.heavy.Load() {
			heavy = append(heavy, map[string]any{"shape": k, "avg_ms": float64(ss.ewma.Load()) / 1e6})
		}
		return true
	})
	sort.Slice(heavy, func(i, j int) bool { return heavy[i]["shape"].(string) < heavy[j]["shape"].(string) })
	r := map[string]any{
		"enabled":              heavyQ != nil,
		"statements":           map[string]any{"normal": laneNormalStmts.Load(), "heavy_read": laneHeavyReads.Load(), "heavy_write": laneHeavyWrites.Load()},
		"view_aggregate_lane":  laneViewAggs.Load(),
		"view_normal_rows_max": ViewNormalRows,
		"shape_moves":          map[string]any{"to_heavy": shapeMovesHeavy.Load(), "to_normal": shapeMovesNormal.Load()},
		"heavy_shapes":         heavy,
		"view_aggregates":      ViewAggregates.Load(),
	}
	if g := govP.Load(); g != nil {
		g.mu.Lock()
		g.refill()
		tok := g.tokens
		g.mu.Unlock()
		r["scheduler"] = map[string]any{
			"share": HeavyShare, "cores": g.rate, "tokens_ms": tok / 1e6,
			"waits": g.waits.Load(), "wait_ms": float64(g.waitNs.Load()) / 1e6,
			"charged_ms": float64(g.charged.Load()) / 1e6, "saturated": nodeSaturated(),
		}
	}
	return r
}
