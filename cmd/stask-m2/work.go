package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------- latency recording

var warmupDur time.Duration // the measured window starts this long after runStart

type Rec struct {
	class, phase string
	d            time.Duration
	err          string        // "" = ok
	at           time.Duration // end of the operation, since runStart (series, merge overlap)
	msg          string        // full text of the error (up to 3 distinct per class are kept in the report)
}

// runStart is the start of the run (set by main before the workers start; warm-up included).
var runStart time.Time

type Recorder struct{ recs []Rec }

func (r *Recorder) Add(class, phase string, d time.Duration, err string) {
	if phase == "warmup" {
		return
	}
	r.recs = append(r.recs, Rec{class: class, phase: phase, d: d, err: err, at: time.Since(runStart)})
}

// AddE records a failed operation with the full error text.
func (r *Recorder) AddE(class, phase string, d time.Duration, code string, e error) {
	if phase == "warmup" {
		return
	}
	m := ""
	if e != nil {
		m = e.Error()
	}
	r.recs = append(r.recs, Rec{class: class, phase: phase, d: d, err: code, at: time.Since(runStart), msg: m})
}

type Stat struct {
	Count      int            `json:"count"`
	OK         int            `json:"ok"`
	Errors     map[string]int `json:"errors,omitempty"`
	PerSec     float64        `json:"per_sec"`
	P50        float64        `json:"p50_ms"`
	P95        float64        `json:"p95_ms"`
	P99        float64        `json:"p99_ms"`
	Max        float64        `json:"max_ms"`
	Retries    int            `json:"retries,omitempty"`
	Noop       int            `json:"noop,omitempty"`
	Unknown    int            `json:"unknown_commit,omitempty"`
	WindowSecs float64        `json:"window_s"`
	// up to 3 distinct error messages (full text, SQLSTATE or class, first time seen in the measured window)
	ErrorSamples []ErrSample `json:"error_samples,omitempty"`
}

type ErrSample struct {
	Code   string  `json:"code"`
	Msg    string  `json:"msg"`
	FirstS float64 `json:"first_s"`
	Count  int     `json:"count"`
}

func pct(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	i := int(float64(len(s))*p+0.9999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func statOf(recs []Rec, window float64) Stat {
	st := Stat{Errors: map[string]int{}, WindowSecs: window}
	var ms []float64
	for _, r := range recs {
		st.Count++
		if r.err == "" {
			st.OK++
			ms = append(ms, float64(r.d.Microseconds())/1000)
		} else {
			st.Errors[r.err]++
			m := r.msg
			if m == "" {
				m = "(no text: " + r.err + ")"
			}
			if len(m) > 600 {
				m = m[:600] + "..."
			}
			found := false
			for i := range st.ErrorSamples {
				if st.ErrorSamples[i].Code == r.err && st.ErrorSamples[i].Msg == m {
					st.ErrorSamples[i].Count++
					found = true
				}
			}
			if !found && len(st.ErrorSamples) < 3 {
				st.ErrorSamples = append(st.ErrorSamples, ErrSample{r.err, m, (r.at - warmupDur).Seconds(), 1})
			}
		}
	}
	sort.Float64s(ms)
	st.P50, st.P95, st.P99 = pct(ms, .5), pct(ms, .95), pct(ms, .99)
	if len(ms) > 0 {
		st.Max = ms[len(ms)-1]
	}
	if window > 0 {
		st.PerSec = float64(st.OK) / window
	}
	if len(st.Errors) == 0 {
		st.Errors = nil
	}
	return st
}

// ---------------------------------------------------------------- shared run state

type Run struct {
	cfg    *Config
	g      *Gen
	sut    string
	start  time.Time
	phase  atomic.Value // string
	txid   atomic.Int64
	impRun atomic.Int64

	mu       sync.Mutex
	recs     []*Recorder
	counters map[string]int64

	pool      sync.Mutex
	open      []int64  // acked open T orders
	acked     sync.Map // txid -> time.Time of the ack
	ackedKind sync.Map // txid -> kind
	unknown   sync.Map // txid -> kind (commit outcome unknown)

	viol   sync.Mutex
	oracle map[string]*OracleRes

	killed   atomic.Bool
	baseline int64
	suspect  sync.Map     // order id -> what happened to attempts that failed (forensics for violations)
	gate     sync.RWMutex // workers hold RLock per transaction; the recovery checks take Lock
}

type OracleRes struct {
	Name   string   `json:"name"`
	Checks int      `json:"checks"`
	Fails  int      `json:"violations"`
	Detail []string `json:"examples,omitempty"`
}

func (r *Run) Check(name string, ok bool, detail string, args ...any) {
	r.viol.Lock()
	defer r.viol.Unlock()
	o := r.oracle[name]
	if o == nil {
		o = &OracleRes{Name: name}
		r.oracle[name] = o
	}
	o.Checks++
	if !ok {
		o.Fails++
		if len(o.Detail) < 5 {
			o.Detail = append(o.Detail, fmt.Sprintf("t+%.1fs ", time.Since(r.start).Seconds())+fmt.Sprintf(detail, args...))
		}
	}
}

func (r *Run) Count(k string, d int64) { r.mu.Lock(); r.counters[k] += d; r.mu.Unlock() }
func (r *Run) NewRecorder() *Recorder {
	rc := &Recorder{}
	r.mu.Lock()
	r.recs = append(r.recs, rc)
	r.mu.Unlock()
	return rc
}
func (r *Run) Phase() string { return r.phase.Load().(string) }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---------------------------------------------------------------- T workers

func (r *Run) popOrder(rng *rand.Rand) (int64, bool) {
	r.pool.Lock()
	defer r.pool.Unlock()
	if len(r.open) == 0 {
		return 0, false
	}
	return r.open[rng.Intn(len(r.open))], true // peek: two workers may race on the same order (deliberate conflict)
}
func (r *Run) removeOrder(id int64) {
	r.pool.Lock()
	defer r.pool.Unlock()
	for i, v := range r.open {
		if v == id {
			r.open[i] = r.open[len(r.open)-1]
			r.open = r.open[:len(r.open)-1]
			return
		}
	}
}

func (r *Run) pickSim(rng *rand.Rand) string {
	cfg := r.cfg
	if rng.Float64() < cfg.HotFrac {
		return simNum(int64(rng.Intn(cfg.Hot)))
	}
	return simNum(int64(rng.Intn(r.g.TEnd)))
}

func (r *Run) workerT(ctx context.Context, id int) {
	rc := r.NewRecorder()
	s := &Sess{dsn: r.sut}
	defer s.Drop()
	rng := rand.New(rand.NewSource(int64(r.g.Seed) + int64(id)*7919))
	for ctx.Err() == nil {
		txid := r.txid.Add(1)
		p := &Payload{TS: fmtTS(time.Now())}
		roll := rng.Intn(100)
		var oid int64
		if roll >= 50 {
			if o, ok := r.popOrder(rng); ok {
				oid = o
				if roll < 75 {
					p.Kind = "pay"
				} else {
					p.Kind = "cancel"
				}
				p.OrderID = o
			}
		}
		if p.Kind == "" {
			p.Kind = "order"
			p.OrderID = tOrderBase + txid
			p.Cust = 1 + rng.Intn(r.g.N/20)
			p.Store = 1 + rng.Intn(40)
			k := 1 + rng.Intn(3)
			seen := map[string]bool{}
			for i := 0; i < k; i++ {
				n := r.pickSim(rng)
				if !seen[n] {
					seen[n] = true
					p.Nums = append(p.Nums, n)
				}
			}
		}
		r.gate.RLock()
		t0 := time.Now()
		txc, txcancel := txContext(ctx)
		res := s.RunTx(txc, 30, func(ctx context.Context, tx pgxTx) (bool, error) { return Apply(ctx, tx, p, true, txid) })
		txcancel()
		d := time.Since(t0)
		r.gate.RUnlock()
		cls := "t_" + p.Kind
		if r.Phase() != "warmup" {
			slowEvent(cls, t0, d, res.Err == nil, fmt.Sprintf("txid=%d retries=%d", txid, res.Retries))
		}
		switch {
		case res.Unknown:
			r.unknown.Store(txid, p.Kind)
			r.Count("t_unknown_commit", 1)
			rc.AddE(cls, r.Phase(), d, "unknown_commit", res.Err)
		case ctx.Err() != nil && res.Err != nil:
			// run over: not an error of the SUT
		case res.Err != nil:
			if strings.Contains(res.Err.Error(), "anomaly") {
				r.Check("txn_anomaly", false, "%v", res.Err)
			}
			rc.AddE(cls, r.Phase(), d, ErrCode(res.Err), res.Err)
		case res.Noop:
			r.Count("t_noop_"+p.Kind, 1)
			if p.Kind != "order" {
				r.removeOrder(oid) // somebody else already closed it
			}
		default:
			r.acked.Store(txid, time.Now())
			r.ackedKind.Store(txid, p.Kind)
			rc.Add(cls, r.Phase(), d, "")
			if p.Kind == "order" {
				r.pool.Lock()
				r.open = append(r.open, p.OrderID)
				r.pool.Unlock()
			} else {
				r.removeOrder(oid)
			}
		}
		if (len(res.Codes) > 0 || res.Err != nil || ctx.Err() != nil) && p.OrderID != 0 {
			r.suspect.Store(p.OrderID, fmt.Sprintf("%s txid=%d attempts=%d codes=%v unknown=%v noop=%v ctxdone=%v err=%v", p.Kind, txid, res.Attempts, res.Codes, res.Unknown, res.Noop, ctx.Err() != nil, res.Err))
		}
		r.Count("t_retries", int64(res.Retries))
		for c, n := range res.Codes {
			r.Count("t_attempt_err_"+c, int64(n))
		}
		sleepCtx(ctx, time.Duration(r.cfg.ThinkT)*time.Millisecond)
	}
}

// ---------------------------------------------------------------- R1 / R2 / join readers

func (r *Run) timedQuery(ctx context.Context, s *Sess, rc *Recorder, class, sql string) (rows [][]string, ok bool) {
	t0 := time.Now()
	c, err := s.Conn(ctx)
	if err == nil {
		rows, err = QueryText(ctx, c, sql)
	}
	d := time.Since(t0)
	if ctx.Err() != nil && err != nil {
		return nil, false
	}
	if err != nil {
		if ErrCode(err) == "conn" {
			s.Drop()
		}
		rc.AddE(class, r.Phase(), d, ErrCode(err), err)
		if ErrCode(err) == "other" || ErrCode(err) == "42601" {
			r.Count("query_err_"+class+": "+trunc(err.Error(), 90), 1)
		}
		return nil, false
	}
	rc.Add(class, r.Phase(), d, "")
	return rows, true
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (r *Run) workerP(ctx context.Context, id int) {
	rc := r.NewRecorder()
	s := &Sess{dsn: r.sut}
	defer s.Drop()
	rng := rand.New(rand.NewSource(int64(r.g.Seed) + 1000 + int64(id)))
	for ctx.Err() == nil {
		if rng.Intn(2) == 0 {
			r.timedQuery(ctx, s, rc, "r1_point", fmt.Sprintf(`SELECT s.number, s.price, s.status, c.code, st.name, st.region
FROM sims s JOIN carriers c ON c.id = s.carrier_id JOIN stores st ON st.id = s.store_id WHERE s.number = '%s'`, simNum(int64(rng.Intn(r.g.N)))))
		} else {
			r.timedQuery(ctx, s, rc, "r1_keyset", fmt.Sprintf(`SELECT number, price, store_rate FROM sims
WHERE carrier_id = %d AND status = 'available' AND category = '%s' AND number > '%s' ORDER BY number LIMIT 50`,
				1+rng.Intn(3), cats[rng.Intn(5)], simNum(int64(rng.Intn(r.g.N)))))
		}
		sleepCtx(ctx, time.Duration(r.cfg.ThinkR)*time.Millisecond)
	}
}

// R2Query is the R2 quota query with the join to carriers and stores. mult scales the page size (for the prefix check);
// frozenLo != "" restricts the inner scan to the frozen sub-population.
func R2Query(page, mult int, frozenLo string) string {
	extra := ""
	if frozenLo != "" {
		extra = " AND number >= '" + frozenLo + "'"
	}
	lo := func(q int) string { return fmt.Sprintf("t.rn BETWEEN %d AND %d", (page-1)*q*mult+1, page*q*mult) }
	return fmt.Sprintf(`SELECT t.number, c.code, st.name, t.price, t.store_rate FROM (
  SELECT number, carrier_id, store_id, price, store_rate,
         row_number() OVER (PARTITION BY carrier_id ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3)%s) t
JOIN carriers c ON c.id = t.carrier_id JOIN stores st ON st.id = t.store_id
WHERE (t.carrier_id = 1 AND %s) OR (t.carrier_id = 2 AND %s) OR (t.carrier_id = 3 AND %s)
ORDER BY t.store_rate DESC, md5(t.number || '42'), t.number`, extra, lo(30), lo(50), lo(20))
}

func (r *Run) workerR(ctx context.Context, id int) {
	rc := r.NewRecorder()
	s := &Sess{dsn: r.sut}
	defer s.Drop()
	rng := rand.New(rand.NewSource(int64(r.g.Seed) + 2000 + int64(id)))
	joinQ := []string{"q05_revenue_by_carrier", "q06_top_customers", "q07_stock_by_region", "q09_price_churn", "q11_order_list_total", "q12_daily_orders"}
	for ctx.Err() == nil {
		for page := 1; page <= 3 && ctx.Err() == nil; page++ {
			r.timedQuery(ctx, s, rc, "r2_page", R2Query(page, 1, ""))
			sleepCtx(ctx, time.Duration(r.cfg.ThinkR)*time.Millisecond)
		}
		if rng.Intn(3) == 0 {
			q := joinQ[rng.Intn(len(joinQ))]
			r.timedQuery(ctx, s, rc, "r_join", r.cfg.Query(q, 0))
		}
	}
}

// ---------------------------------------------------------------- import worker

func (r *Run) workerImport(ctx context.Context) {
	rc := r.NewRecorder()
	s := &Sess{dsn: r.sut}
	defer s.Drop()
	n := r.g.TEnd
	for ctx.Err() == nil {
		if r.cfg.ImportMode == "never" || r.Phase() == "warmup" || (r.cfg.ImportMode == "alternate" && r.Phase() != "import") {
			sleepCtx(ctx, 200*time.Millisecond)
			continue
		}
		run := r.impRun.Add(1)
		slice := int(run % 100)
		a, b := int64(slice*n/100), int64((slice+1)*n/100)
		if slice == 99 {
			b = int64(n)
		}
		// default shape: batches of ImportBatch rows, each its own transaction (and its own journal row),
		// one after another with ImportPause between them, as staskimport commits. ImportBatch 0 = the whole
		// slice in ONE transaction (worst case, measured separately).
		step := int64(r.cfg.ImportBatch)
		if step <= 0 || step > b-a {
			step = b - a
		}
		for lo := a; lo < b && ctx.Err() == nil; lo += step {
			hi := min(lo+step, b)
			p := &Payload{Kind: "import", TS: fmtTS(time.Now()), Slice: slice, Nums: []string{simNum(lo), simNum(hi)}, OrderID: impHistBase + run*100_000_000}
			txid := r.txid.Add(1)
			r.gate.RLock()
			t0 := time.Now()
			txc, txcancel := txContext(ctx)
			res := s.RunTx(txc, 30, func(ctx context.Context, tx pgxTx) (bool, error) { return Apply(ctx, tx, p, true, txid) })
			txcancel()
			d := time.Since(t0)
			r.gate.RUnlock()
			if r.Phase() != "warmup" {
				importEvent(txid, t0, d, p.Slice, lo, hi, res.Err == nil)
			}
			switch {
			case res.Unknown:
				r.unknown.Store(txid, "import")
				rc.AddE("import", r.Phase(), d, "unknown_commit", res.Err)
			case ctx.Err() != nil && res.Err != nil:
			case res.Err != nil:
				rc.AddE("import", r.Phase(), d, ErrCode(res.Err), res.Err)
			default:
				r.acked.Store(txid, time.Now())
				r.ackedKind.Store(txid, "import")
				rc.Add("import", r.Phase(), d, "")
				r.Count("import_batches", 1)
				r.Count("import_rows", p.Rows)
			}
			r.Count("import_retries", int64(res.Retries))
			if hi < b && r.cfg.ImportPause > 0 {
				sleepCtx(ctx, r.cfg.ImportPause)
			}
		}
		sleepCtx(ctx, time.Duration(r.cfg.ImportEvery)*time.Second)
	}
}

// ---------------------------------------------------------------- slow-transaction log (-slow-log)

// The slow-log file correlates the client view with the server span log (staskd STASK_SPANS): one line per import batch
// ("I <txid> <start unix ms> <dur ms> slice range ok") and one per T transaction slower than -slow-min ("T <class> <start> <dur> ok info").
var (
	slowMu   sync.Mutex
	slowFile *os.File
	slowMin  time.Duration
)

func openSlowLog(path string, min time.Duration) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	slowFile, slowMin = f, min
	return nil
}

func slowEvent(class string, t0 time.Time, d time.Duration, ok bool, info string) {
	if slowFile == nil || d < slowMin {
		return
	}
	slowMu.Lock()
	fmt.Fprintf(slowFile, "T %s %d %.1f %v %s\n", class, t0.UnixMilli(), float64(d.Microseconds())/1000, ok, info)
	slowMu.Unlock()
}

func importEvent(txid int64, t0 time.Time, d time.Duration, slice int, a, b int64, ok bool) {
	if slowFile == nil {
		return
	}
	slowMu.Lock()
	fmt.Fprintf(slowFile, "I %d %d %.1f slice=%d range=%d-%d ok=%v\n", txid, t0.UnixMilli(), float64(d.Microseconds())/1000, slice, a, b, ok)
	slowMu.Unlock()
}
