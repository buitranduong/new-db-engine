package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------- latency recording

type Rec struct {
	class, phase string
	d            time.Duration
	err          string // "" = ok
}

type Recorder struct{ recs []Rec }

func (r *Recorder) Add(class, phase string, d time.Duration, err string) {
	r.recs = append(r.recs, Rec{class, phase, d, err})
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
		switch {
		case res.Unknown:
			r.unknown.Store(txid, p.Kind)
			r.Count("t_unknown_commit", 1)
			rc.Add(cls, r.Phase(), d, "unknown_commit")
		case ctx.Err() != nil && res.Err != nil:
			// run over: not an error of the SUT
		case res.Err != nil:
			if strings.Contains(res.Err.Error(), "anomaly") {
				r.Check("txn_anomaly", false, "%v", res.Err)
			}
			rc.Add(cls, r.Phase(), d, ErrCode(res.Err))
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
		rc.Add(class, r.Phase(), d, ErrCode(err))
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
		if r.cfg.ImportMode == "never" || (r.cfg.ImportMode == "alternate" && r.Phase() != "import") {
			sleepCtx(ctx, 200*time.Millisecond)
			continue
		}
		run := r.impRun.Add(1)
		slice := int(run % 100)
		a, b := int64(slice*n/100), int64((slice+1)*n/100)
		if slice == 99 {
			b = int64(n)
		}
		p := &Payload{Kind: "import", TS: fmtTS(time.Now()), Slice: slice, Nums: []string{simNum(a), simNum(b)}, OrderID: impHistBase + run*100_000_000}
		txid := r.txid.Add(1)
		r.gate.RLock()
		t0 := time.Now()
		txc, txcancel := txContext(ctx)
		res := s.RunTx(txc, 30, func(ctx context.Context, tx pgxTx) (bool, error) { return Apply(ctx, tx, p, true, txid) })
		txcancel()
		d := time.Since(t0)
		r.gate.RUnlock()
		switch {
		case res.Unknown:
			r.unknown.Store(txid, "import")
			rc.Add("import", r.Phase(), d, "unknown_commit")
		case ctx.Err() != nil && res.Err != nil:
		case res.Err != nil:
			rc.Add("import", r.Phase(), d, ErrCode(res.Err))
		default:
			r.acked.Store(txid, time.Now())
			r.ackedKind.Store(txid, "import")
			rc.Add("import", r.Phase(), d, "")
		}
		r.Count("import_retries", int64(res.Retries))
		sleepCtx(ctx, time.Duration(r.cfg.ImportEvery)*time.Second)
	}
}
