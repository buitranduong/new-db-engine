package sql

// Hybrid path, step 1 of slice 1: choice by contract, execution on the
// segment state, page fetch from the core (docs/30 4.1, docs/31 5-7).

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/core"
	"stask/internal/proj"
	"stask/internal/schema"
)

// Hybrid is the platform side of the hybrid path (staskd -hybrid): the
// segment consumer and its budgets. A Session with Hybrid == nil never
// takes the path.
type Hybrid struct {
	E          *core.Engine
	Seg        *proj.SegColumnar
	DimMaxRows int           // most rows a joined dimension may have
	Wait       time.Duration // longest wait for the state to reach a snapshot

	Used, Fallbacks atomic.Int64
	seen            sync.Map // fallback reason -> logged once
	Logf            func(string, ...any)
}

// NewHybrid registers the segment consumer with the engine.
func NewHybrid(e *core.Engine) *Hybrid {
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	// test/oracle knob: merge the tail early, so that merges happen at small scale
	if v, err := strconv.Atoi(os.Getenv("STASK_SEG_MERGE_MIN_TAIL")); err == nil && v > 0 {
		proj.SegMergeMinTail = v
	}
	c := proj.NewSegColumnar()
	c.Logf = logf
	c.Start(e)
	return &Hybrid{E: e, Seg: c, DimMaxRows: 4096, Wait: 2 * time.Second, Logf: logf}
}

// hySess is the session's hybrid state: the last decision, and the segment
// state an explicit transaction pinned (its snapshot is older than the ring
// of retained states after a few seconds).
type hySess struct {
	last     *hyReport
	pinSnap  uint64
	pinState *proj.SegState
}

// hyReport is what one decision learned; EXPLAIN prints it.
type hyReport struct {
	used     bool
	why      string // reason for not using the path
	plan     *hyPlan
	contract string
	snap     uint64 // statement snapshot S
	readSnap uint64 // snapshot the page and the dimension were read at
	state    uint64 // LSN of the state used
	applied  uint64
	durable  uint64
	lag      time.Duration
	waited   time.Duration
	info     proj.SegInfo
	stats    proj.SegR2Stats
	dimRows  int
	dimPass  int
	hits     int
	coreRows int
	coreByte int
	memBytes int64
	elapsed  time.Duration
	baseRows int
	tailRows int
	rejected []string
	groupsN  int
}

func (h *Hybrid) note(why string) {
	h.Fallbacks.Add(1)
	if _, seen := h.seen.LoadOrStore(why, true); !seen && h.Logf != nil {
		h.Logf("hybrid: path not used (logged once per reason): %s", why)
	}
}

// tryHybrid is the hook in runHere: handled=false sends the statement to the
// existing executors; the reason is in s.hy.last either way.
func (s *Session) tryHybrid(tx *core.Tx, st *Select, params []Datum) (*Result, bool, error) {
	h := s.Hybrid
	if h == nil {
		return nil, false, nil
	}
	s.hy.last = nil
	p, why, cand := s.hyRecognize(st, params)
	if !cand {
		return nil, false, nil
	}
	rep := &hyReport{plan: p}
	s.hy.last = rep
	if why == "" {
		why = s.hyTxReason(tx)
	}
	if why != "" {
		rep.why = why
		h.note(why)
		return nil, false, nil
	}
	t0 := time.Now()
	res, why, err := s.hyRun(tx, p, params, rep)
	rep.elapsed = time.Since(t0)
	if err != nil {
		return nil, true, err
	}
	if why != "" {
		rep.why = why
		h.note(why)
		return nil, false, nil
	}
	rep.used = true
	h.Used.Add(1)
	return res, true, nil
}

// hyTxReason: an explicit transaction may use the path while it has written
// nothing (the state has no uncommitted rows) and is not SERIALIZABLE (whose
// reads the core validates and whose range scans it refuses).
func (s *Session) hyTxReason(tx *core.Tx) string {
	if !s.explicit {
		return ""
	}
	if tx.NumWrites() > 0 {
		return "the explicit transaction has uncommitted writes (the segment state has none of them)"
	}
	if strings.EqualFold(s.txLevel, "serializable") {
		return "SERIALIZABLE transaction (reads are validated by the core)"
	}
	return ""
}

func hyWaitApplied(f *core.Follower, lsn uint64, max time.Duration) bool {
	deadline := time.Now().Add(max)
	last := f.Applied()
	for !f.WaitApplied(lsn, 200*time.Millisecond) {
		cur := f.Applied()
		if cur == last || time.Now().After(deadline) {
			return false
		}
		last = cur
	}
	return true
}

// hyContract picks the state per the consistency contract. The reason is
// non-empty when the path cannot be used.
func (s *Session) hyContract(tx *core.Tx, rep *hyReport) (*proj.SegState, string) {
	h := s.Hybrid
	f := h.Seg.Follower()
	if !h.Seg.Ready() {
		return nil, "the segment state is still being built"
	}
	mode := strings.ToLower(s.vars[SearchConsistencyVar])
	if mode == "" {
		mode = DefaultConsistency
	}
	bound := time.Second
	if v, err := parseStaleness(s.vars[SearchMaxLagVar]); err == nil && v > 0 {
		bound = v
	}
	if st := s.readStaleness(); st > 0 {
		mode, bound = "bounded", st
	}
	S := tx.Snapshot()
	rep.snap = S
	rep.durable = h.E.DurableLSN()
	t0 := time.Now()
	defer func() {
		rep.waited = time.Since(t0)
		rep.applied, rep.lag = f.Applied(), f.Lag()
	}()
	switch mode {
	case "strong", "session":
		// session: the snapshot S already holds every commit of this session,
		// so the exact snapshot answers it (stronger than required)
		rep.contract = "strong"
		if mode == "session" {
			rep.contract = "strong (session)"
		}
		if s.explicit && s.hy.pinState != nil && s.hy.pinSnap == S {
			rep.readSnap = S
			return s.hy.pinState, ""
		}
		if !hyWaitApplied(f, S, h.Wait) {
			return nil, fmt.Sprintf("the segment state did not reach snapshot lsn %d within %v (applied %d); the log-gap path is not part of step 1", S, h.Wait, f.Applied())
		}
		st := h.Seg.StateAt(S)
		if st == nil {
			return nil, fmt.Sprintf("snapshot lsn %d is older than the retained segment states", S)
		}
		rep.readSnap = S
		if s.explicit {
			s.hy.pinSnap, s.hy.pinState = S, st
		}
		return st, ""
	case "bounded", "eventual":
		rep.contract = fmt.Sprintf("bounded (<= %v)", bound)
		if mode == "eventual" {
			rep.contract = "eventual"
		}
		if mode == "bounded" && f.Lag() > bound {
			hyWaitApplied(f, rep.durable, bound)
			if f.Lag() > bound {
				return nil, fmt.Sprintf("segment state lags %v, more than the bound %v", f.Lag(), bound)
			}
		}
		st := h.Seg.Current()
		if st == nil {
			return nil, "no segment state"
		}
		rep.readSnap = st.LSN
		return st, ""
	}
	return nil, fmt.Sprintf("unknown consistency mode %q", mode)
}

func hyCols(p *hyPlan) []Column {
	cols := make([]Column, len(p.outs))
	for i, o := range p.outs {
		switch o.kind {
		case hyRank:
			cols[i] = Column{Name: o.name, Kind: KInt, OID: 20, Mod: -1}
		default:
			t := p.fact.t
			if o.kind == hyDim {
				t = p.dim.t
			}
			cols[i] = itemCol(SelItem{X: &ColRef{Name: t.Columns[o.col].Name}}, &scope{t: t}, nil)
			cols[i].Name = o.name
			cols[i].TableOID, cols[i].AttNum = 0, 0 // a derived table's columns have no source table
		}
	}
	return cols
}

func (s *Session) hyRun(tx *core.Tx, p *hyPlan, params []Datum, rep *hyReport) (*Result, string, error) {
	h := s.Hybrid
	e := h.E
	state, why := s.hyContract(tx, rep)
	if why != "" {
		return nil, why, nil
	}
	rep.state = state.LSN
	tb := state.Tbls[p.fact.t.ID]
	if tb == nil {
		return nil, "table " + p.fact.name + ": " + h.Seg.Reason(e, p.fact.t.Name), nil
	}
	rep.info, _ = h.Seg.Info(p.fact.t.ID)
	rep.baseRows, rep.tailRows = tb.RowsBase(), tb.RowsTail()
	R := rep.readSnap

	// columns must be held
	need := []int{p.groupCol, p.scoreCol}
	for _, f := range p.filters {
		need = append(need, f.Col)
	}
	for _, c := range need {
		if _, _, ok := tb.Col(p.fact.t.Columns[c].Name); !ok {
			return nil, fmt.Sprintf("column %s is not held by the segment (type, cardinality or NULL-sentinel overflow)", p.fact.t.Columns[c].Name), nil
		}
	}

	// the dimension, read from the core at the same snapshot
	groups := p.groups
	var dimRows map[string]schema.Row
	if p.dim != nil {
		dimRows = map[string]schema.Row{}
		ctx := &evalCtx{sc: &scope{t: p.dim.t, alias: p.dim.alias}, params: params, now: nowMicros(), sess: s}
		var perr error
		over := false
		err := e.ScanSnapshot(p.dim.t.Name, R, func(key string, r schema.Row, _ *core.Version) bool {
			rep.dimRows++
			if rep.dimRows > h.DimMaxRows {
				over = true
				return false
			}
			if r[p.dimJoinCol].Null {
				return true
			}
			ctx.row = r
			for _, pe := range p.dimPreds {
				d, err := ctx.evalBool(pe)
				if err != nil {
					perr = err
					return false
				}
				if !d.Bool() {
					return true
				}
			}
			dimRows[r[p.dimJoinCol].S] = r
			return true
		})
		switch {
		case perr != nil:
			return nil, "", perr
		case errors.Is(err, core.ErrSnapshotTooOld) || errors.Is(err, core.ErrInterrupted):
			if errors.Is(err, core.ErrInterrupted) {
				return nil, "", err
			}
			return nil, fmt.Sprintf("dimension %s cannot be read at snapshot %d (too old)", p.dim.name, R), nil
		case err != nil:
			return nil, "", err
		case over:
			return nil, fmt.Sprintf("dimension %s has more than %d rows (broadcast join budget)", p.dim.name, h.DimMaxRows), nil
		}
		rep.dimPass = len(dimRows)
		groups = nil
		for _, g := range p.groups {
			if _, ok := dimRows[g.Value]; ok {
				groups = append(groups, g)
			}
		}
	}
	rep.groupsN = len(groups)

	workers := 1 + max(0, min(ScanWorkers-1, int(scanHelpers.Load())))
	q := &proj.SegR2{Filters: p.filters, Group: p.groupCol, Groups: groups, Score: p.scoreCol, Desc: p.desc, Md5: p.md5,
		Md5Suffix: p.suffix, Workers: workers, Interrupted: s.interrupted}
	hits, stats, err := tb.TopNPerGroup(q)
	rep.stats = stats
	maxCap := 0
	for _, g := range groups {
		for _, r := range g.Ranges {
			maxCap = max(maxCap, r.Hi)
		}
	}
	rep.memBytes = int64(len(groups)) * int64(maxCap) * 40 * int64(max(1, stats.Workers+1))
	switch {
	case errors.Is(err, proj.ErrSegInterrupted):
		return nil, "", core.ErrInterrupted
	case errors.Is(err, proj.ErrSegNullScore), errors.Is(err, proj.ErrSegBudget):
		return nil, err.Error(), nil
	case err != nil:
		return nil, "segment operator: " + err.Error(), nil
	}
	rep.hits = len(hits)

	// page rows from the core at the read snapshot; the state is checked
	// against them: a mismatch is a broken invariant, never an answer
	cols := hyCols(p)
	res := &Result{HasRows: true, Cols: cols, Rows: make([][]Datum, 0, len(hits))}
	ft := p.fact.t
	for _, hit := range hits {
		row, _, err := e.GetAt(ft.Name, hit.Key, R)
		if err != nil {
			if errors.Is(err, core.ErrSnapshotTooOld) {
				return nil, fmt.Sprintf("snapshot %d can no longer be read from the core", R), nil
			}
			return nil, "", err
		}
		rep.coreRows++
		if row == nil {
			return nil, "", errf("XX000", "hybrid invariant violated: key %q of segment state lsn %d is not in %s at snapshot %d", hit.Key, state.LSN, ft.Name, R)
		}
		rep.coreByte += rowSize(row)
		if row[p.groupCol].Null || row[p.groupCol].S != hit.Group || row[p.scoreCol].Null || row[p.scoreCol].I != int64(hit.Score) {
			return nil, "", errf("XX000", "hybrid invariant violated: key %q differs between segment state lsn %d and %s at snapshot %d", hit.Key, state.LSN, ft.Name, R)
		}
		for i := range p.filters {
			if !p.filters[i].Match(row[p.filters[i].Col]) {
				return nil, "", errf("XX000", "hybrid invariant violated: key %q of segment state lsn %d does not satisfy %s in the core at snapshot %d", hit.Key, state.LSN, p.filterText[i], R)
			}
		}
		var drow schema.Row
		if p.dim != nil {
			drow = dimRows[hit.Group]
			if drow == nil {
				return nil, "", errf("XX000", "hybrid invariant violated: no dimension row for partition %q", hit.Group)
			}
		}
		out := make([]Datum, len(p.outs))
		for i, o := range p.outs {
			switch o.kind {
			case hyFact:
				out[i] = fromCol(&ft.Columns[o.col], row[o.col])
			case hyDim:
				out[i] = fromCol(&p.dim.t.Columns[o.col], drow[o.col])
			default:
				out[i] = IntD(int64(hit.Rank))
			}
		}
		res.Rows = append(res.Rows, conform(out, cols))
	}
	res.Tag = fmt.Sprintf("SELECT %d", len(res.Rows))
	if err := s.checkInterrupt(); err != nil {
		return nil, "", err
	}
	return res, "", nil
}

func rowSize(r schema.Row) int {
	n := 0
	for _, v := range r {
		n += 8 + len(v.S)
	}
	return n
}
