// Package planner gives one query interface over the core and its derived
// views. It does not route by statement keyword: it routes by what the
// query needs (access pattern) and what the caller requires (freshness),
// and it reports the decision with every answer.
package planner

import (
	"fmt"
	"math"
	"sort"
	"time"

	"stask/internal/core"
	"stask/internal/proj"
	"stask/internal/schema"
)

type Platform struct {
	E    *core.Engine
	Col  *proj.Columnar
	Srch *proj.Search
	TS   *proj.TimeSeries
	Sub  *proj.Substr // substring (trigram) view, substr.go
	ColF *core.Follower
	SF   *core.Follower
	TSF  *core.Follower
	SubF *core.Follower
}

func Start(e *core.Engine) *Platform {
	p := &Platform{E: e, Col: proj.NewColumnar(), Srch: proj.NewSearch(), TS: proj.NewTimeSeries()}
	p.ColF = e.Follow(p.Col)
	p.SF = e.Follow(p.Srch)
	p.TSF = e.Follow(p.TS)
	p.Sub = proj.NewSubstr()
	if on, dir, cache, logf := e.DiskIndexConfig(); on {
		if err := p.Sub.ConfigureDisk(true, dir, cache, logf); err != nil {
			logf("trigram disk store: %v", err)
		}
	}
	p.SubF = e.Follow(p.Sub)
	e.AddIndexMemSource("trigram", func() []core.IndexMem {
		var out []core.IndexMem
		for _, st := range p.Sub.Report() {
			name := ""
			if ts := e.TableByID(st.Table); ts != nil {
				name = ts.Schema().Name
			}
			out = append(out, core.IndexMem{Kind: "trigram", Table: name, Name: st.Col, Storage: st.Storage,
				RAMBytes: st.RAMBytes, DiskBytes: st.DiskBytes, Entries: st.Postings})
		}
		return out
	})
	return p
}

// Consistency is what the caller requires from a read.
//
//	strong   – reflect every commit acknowledged before the query started
//	bounded  – may lag the core by at most max_lag_ms
//	eventual – whatever the derived view has
//	session  – reflect at least min_lsn (read-your-writes token)
type Consistency struct {
	Mode      string `json:"mode,omitempty"`
	MaxLagMs  int64  `json:"max_lag_ms,omitempty"`
	MinLSN    uint64 `json:"min_lsn,omitempty"`
	TimeoutMs int64  `json:"timeout_ms,omitempty"`
	// ForceCore answers from the core snapshot regardless of views
	// (benchmarking the row path against the derived view).
	ForceCore bool `json:"force_core,omitempty"`
}

type Plan struct {
	Engine     string  `json:"engine"`
	Mode       string  `json:"mode"`
	Reason     string  `json:"reason"`
	DurableLSN uint64  `json:"durable_lsn"`
	AppliedLSN uint64  `json:"applied_lsn,omitempty"`
	LagMs      float64 `json:"view_lag_ms"`
	WaitedMs   float64 `json:"waited_ms,omitempty"`
	ExecMs     float64 `json:"exec_ms"`
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())) / 1000 }

func (p *Platform) choose(f *core.Follower, has bool, view string, c Consistency, def string) (bool, Plan) {
	mode := c.Mode
	if mode == "" {
		mode = def
	}
	durable := p.E.DurableLSN()
	pl := Plan{Mode: mode, DurableLSN: durable, Engine: "core"}
	// a view publishes its tables when its bootstrap ends: before that, has
	// is false for every table
	if !f.Ready() {
		pl.Reason = view + " view is bootstrapping"
		return false, pl
	}
	if !has {
		pl.Reason = "table has no " + view + " representation"
		return false, pl
	}
	if c.ForceCore {
		pl.Reason = "forced core execution"
		return false, pl
	}
	t0 := time.Now()
	use := func(reason string) (bool, Plan) {
		pl.Engine, pl.Reason = view, reason
		pl.AppliedLSN, pl.LagMs, pl.WaitedMs = f.Applied(), ms(f.Lag()), ms(time.Since(t0))
		return true, pl
	}
	fallback := func(reason string) (bool, Plan) {
		pl.Reason, pl.AppliedLSN, pl.LagMs, pl.WaitedMs = reason, f.Applied(), ms(f.Lag()), ms(time.Since(t0))
		return false, pl
	}
	timeout := func(d time.Duration) time.Duration {
		if c.TimeoutMs > 0 {
			return time.Duration(c.TimeoutMs) * time.Millisecond
		}
		return d
	}
	switch mode {
	case "eventual":
		return use("eventual: view used as-is")
	case "bounded":
		bound := time.Duration(c.MaxLagMs) * time.Millisecond
		if c.MaxLagMs <= 0 {
			bound = time.Second
		}
		if f.Lag() <= bound {
			return use(fmt.Sprintf("view lag within %v bound", bound))
		}
		if f.WaitApplied(durable, timeout(bound)) {
			return use("view caught up within bound")
		}
		return fallback(fmt.Sprintf("view lag exceeds %v bound: answered from core snapshot", bound))
	case "strong":
		if f.WaitApplied(durable, timeout(2*time.Second)) {
			return use(fmt.Sprintf("view reached durable lsn %d", durable))
		}
		return fallback("view did not reach durable lsn in time: answered from core snapshot")
	case "session":
		if f.WaitApplied(c.MinLSN, timeout(500*time.Millisecond)) {
			return use(fmt.Sprintf("view reached session lsn %d", c.MinLSN))
		}
		return fallback("view did not reach session lsn in time: answered from core snapshot")
	}
	return fallback("unknown consistency mode " + mode)
}

func checkMode(c Consistency) error {
	switch c.Mode {
	case "", "strong", "bounded", "eventual":
		return nil
	case "session":
		if c.MinLSN == 0 {
			return fmt.Errorf("session consistency needs min_lsn")
		}
		return nil
	}
	return fmt.Errorf("unknown consistency mode %q", c.Mode)
}

// Aggregate: columnar view by default with bounded staleness (WL-2 matrix).
func (p *Platform) Aggregate(q *proj.AggQuery, c Consistency) (*proj.Result, Plan, error) {
	t, err := p.E.UserTable(q.Table)
	if err != nil {
		return nil, Plan{}, err
	}
	if err := checkMode(c); err != nil {
		return nil, Plan{}, err
	}
	useView, pl := p.choose(p.ColF, p.Col.Has(t.Schema().ID), "columnar", c, "bounded")
	t0 := time.Now()
	var res *proj.Result
	if useView {
		res, err = p.Col.Aggregate(t.Schema().ID, q)
	} else {
		var ra *proj.RowAggregator
		ra, err = proj.NewRowAggregator(t.Schema(), q)
		if err == nil {
			err = p.E.ScanSnapshot(q.Table, pl.DurableLSN, func(_ string, r schema.Row, _ *core.Version) bool {
				ra.Add(r)
				return true
			})
			res = ra.Result()
		}
	}
	pl.ExecMs = ms(time.Since(t0))
	return res, pl, err
}

// ViewAggregate answers q from the columnar view: SQL aggregates. By
// default the view must reflect lsn, the statement's snapshot. With
// staleness > 0 (the session accepts reading that far in the past, like
// TiDB's tidb_read_staleness) a view lagging less than that answers as it
// is. ok=false, with no answer, when the table is not columnar, the view is
// bootstrapping or stalled (see waitView), or q needs a column the view does
// not hold; SQL then scans the rows.
func (p *Platform) ViewAggregate(q *proj.AggQuery, lsn uint64, staleness time.Duration) (*proj.Result, bool) {
	t, err := p.E.UserTable(q.Table)
	if err != nil || !p.Col.Has(t.Schema().ID) || !p.ColF.Ready() {
		return nil, false
	}
	if (staleness <= 0 || p.ColF.Lag() > staleness) && !waitView(p.ColF, lsn) {
		return nil, false
	}
	res, err := p.Col.Aggregate(t.Schema().ID, q)
	return res, err == nil
}

// A view read waits the way a TiFlash learner read does: until the view
// has applied the read's snapshot, for as long as the view keeps advancing.
// After a burst (a bulk load, a replica catching up) the answer is delayed
// by the time the view needs to apply it, which is much less than scanning
// the rows instead; the rows are scanned only when the view stalls.
var (
	ViewStall   = 500 * time.Millisecond // no progress for this long: stalled
	ViewMaxWait = 10 * time.Second
)

func waitView(f *core.Follower, lsn uint64) bool {
	deadline := time.Now().Add(ViewMaxWait)
	last := f.Applied()
	for !f.WaitApplied(lsn, ViewStall) {
		cur := f.Applied()
		if cur == last || time.Now().After(deadline) {
			return false
		}
		last = cur
	}
	return true
}

// TimeSeries: time-series view, read-your-writes by default when a token is
// given (WL-4 matrix), bounded otherwise.
func (p *Platform) TimeSeries(q *proj.TSQuery, c Consistency) (*proj.Result, Plan, error) {
	t, err := p.E.UserTable(q.Table)
	if err != nil {
		return nil, Plan{}, err
	}
	if err := checkMode(c); err != nil {
		return nil, Plan{}, err
	}
	def := "bounded"
	if c.MinLSN > 0 {
		def = "session"
	}
	if c.Mode == "bounded" && c.MaxLagMs == 0 {
		c.MaxLagMs = 500
	}
	useView, pl := p.choose(p.TSF, p.TS.Has(t.Schema().ID), "timeseries", c, def)
	t0 := time.Now()
	var res *proj.Result
	if useView {
		res, err = p.TS.Query(t.Schema().ID, q)
	} else {
		var x *proj.TSExec
		x, err = proj.NewTSExec(t.Schema(), q)
		if err == nil {
			ids := map[string]int64{}
			sid := func(s string) int64 {
				if v, ok := ids[s]; ok {
					return v
				}
				ids[s] = int64(len(ids))
				return ids[s]
			}
			err = p.E.ScanSnapshot(q.Table, pl.DurableLSN, func(_ string, r schema.Row, _ *core.Version) bool {
				x.AddRow(t.Schema(), r, sid)
				return true
			})
			res = x.Result()
		}
	}
	pl.ExecMs = ms(time.Since(t0))
	return res, pl, err
}

type SearchHit struct {
	proj.Hit
	Row        map[string]any `json:"row,omitempty"`
	CurrentLSN uint64         `json:"current_lsn,omitempty"`
	Stale      bool           `json:"stale,omitempty"`
}

// Search: inverted index, eventual by default (WL-3 matrix). With fetch,
// each hit's row is read from the core and flagged if it changed after it
// was indexed, so a caller never mistakes index content for current truth.
func (p *Platform) Search(q *proj.SearchQuery, c Consistency, fetch bool) ([]SearchHit, map[string]string, Plan, error) {
	t, err := p.E.UserTable(q.Table)
	if err != nil {
		return nil, nil, Plan{}, err
	}
	if err := checkMode(c); err != nil {
		return nil, nil, Plan{}, err
	}
	useView, pl := p.choose(p.SF, p.Srch.Has(t.Schema().ID), "search", c, "eventual")
	t0 := time.Now()
	var hits []proj.Hit
	var expanded map[string]string
	if useView {
		r, err := p.Srch.Query(t.Schema().ID, q)
		if err != nil {
			return nil, nil, pl, err
		}
		hits, expanded = r.Hits, r.Expanded
	} else {
		if len(t.Schema().Search) == 0 {
			return nil, nil, pl, fmt.Errorf("table %s has no search columns", q.Table)
		}
		hits, err = p.bruteForceSearch(t, q, pl.DurableLSN)
		if err != nil {
			return nil, nil, pl, err
		}
	}
	out := make([]SearchHit, len(hits))
	for i, h := range hits {
		out[i].Hit = h
		if fetch {
			r, v, err := p.E.Get(q.Table, h.Key)
			if err != nil {
				return nil, nil, pl, err
			}
			if r == nil {
				out[i].Stale = true
				if v != nil {
					out[i].CurrentLSN = v.LSN
				}
				continue
			}
			out[i].Row = t.Schema().ToJSON(r)
			out[i].CurrentLSN = v.LSN
			out[i].Stale = v.LSN != h.LSN
		}
	}
	pl.ExecMs = ms(time.Since(t0))
	return out, expanded, pl, nil
}

// bruteForceSearch scans the core snapshot (no index): correct, slow.
func (p *Platform) bruteForceSearch(t *core.TableState, q *proj.SearchQuery, snap uint64) ([]proj.Hit, error) {
	var terms []string
	proj.Tokenize(q.Query, func(s string) { terms = append(terms, s) })
	var cols []int
	for _, c := range t.Schema().Search {
		i, _ := t.Schema().Col(c)
		cols = append(cols, i)
	}
	var hits []proj.Hit
	err := p.E.ScanSnapshot(q.Table, snap, func(key string, r schema.Row, v *core.Version) bool {
		tf := map[string]int{}
		for _, c := range cols {
			proj.Tokenize(r[c].S, func(s string) { tf[s]++ })
		}
		score, matched := 0.0, 0
		for _, qt := range terms {
			if n := tf[qt]; n > 0 {
				score += float64(n)
				matched++
			}
		}
		if matched > 0 && (!q.All || matched == len(terms)) {
			hits = append(hits, proj.Hit{Key: key, Score: score, LSN: v.LSN})
		}
		return true
	})
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, err
}

type ViewStatus struct {
	Name       string  `json:"name"`
	Ready      bool    `json:"ready"`
	AppliedLSN uint64  `json:"applied_lsn"`
	LagMs      float64 `json:"lag_ms"`
	BootMs     float64 `json:"bootstrap_ms"`
}

func (p *Platform) Views() []ViewStatus {
	var out []ViewStatus
	for _, f := range []*core.Follower{p.ColF, p.SF, p.TSF, p.SubF} {
		out = append(out, ViewStatus{f.Name(), f.Ready(), f.Applied(), ms(f.Lag()), ms(f.BootTime())})
	}
	return out
}
