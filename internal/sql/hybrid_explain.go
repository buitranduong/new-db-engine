package sql

// EXPLAIN of the hybrid path: the contract of docs/30 4.5 as extended by
// docs/31 7. EXPLAIN prints the decision with estimates; EXPLAIN ANALYZE
// replaces the plan block with the real counters of the run.

import (
	"fmt"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/proj"
)

func hyMS(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }

func hyBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// hyLines renders the plan block of a decision. analyze: real counters.
func (s *Session) hyLines(rep *hyReport, q *Select, analyze bool) []string {
	y := &explainer{s: s}
	p := rep.plan
	if !rep.used && rep.why != "" && rep.contract == "" && rep.state == 0 && !analyze {
		// not eligible before any state was looked at
		return []string{"Hybrid not used: " + rep.why}
	}
	gaps := uint64(0)
	if rep.snap > rep.applied {
		gaps = rep.snap - rep.applied
	}
	delta := fmt.Sprintf("%d tail rows + %d base rows hidden", rep.info.TailRows, rep.info.Changed)
	y.node(0, "Hybrid Plan  (contract: %s; snapshot lsn=%d; state lsn=%d; applied lsn=%d; view lag=%s; delta size: %s; gap: %d lsn)",
		rep.contract, rep.snap, rep.state, rep.applied, hyMS(rep.lag), delta, gaps)
	var kept []string
	for _, g := range p.groups {
		var rs []string
		for _, r := range g.Ranges {
			rs = append(rs, fmt.Sprintf("%d-%d", r.Lo, r.Hi))
		}
		kept = append(kept, fmt.Sprintf("%s: %s", g.Value, strings.Join(rs, ",")))
	}
	y.node(1, "Top-N per Partition on %s  [ranks kept per partition: %s]", p.fact.t.Name, strings.Join(kept, " / "))
	y.detail(1, "Partition: %s    Order: %s", p.fact.t.Columns[p.groupCol].Name, p.orderText)
	if len(p.filterText) > 0 {
		y.detail(1, "Filter (on dictionary and integer columns): %s", strings.Join(p.filterText, " AND "))
	}
	path := "SEGMENT(base)+TAIL"
	if p.dim != nil {
		path += "+DIM-BROADCAST"
	}
	y.detail(1, "Path: %s", path)
	if rep.used || (!analyze && rep.why == "") {
		y.detail(1, "Why: row_number() OVER (PARTITION BY) with outer rank filter recognised; every column read is held by the segment state; %s met by state lsn=%d at snapshot lsn=%d (state is immutable: no lock is taken, the merge never blocks the read)", rep.contract, rep.state, rep.snap)
	} else {
		y.detail(1, "Why not: %s", rep.why)
	}
	y.detail(1, "Rejected: ROW-EXECUTOR: materialises every row that passes the filter into Datums and sorts them (no top-N operator); COLUMNAR VIEW (old): answers GROUP BY aggregates only, returns no rows; LOG-GAP (lemma A): not part of step 1 (the read waits for the state instead, up to %v)", s.Hybrid.Wait)
	st := rep.stats
	if analyze && rep.used {
		y.detail(1, "Read base:  %d rows scanned in %d chunks (%d chunks pruned by zone maps), %d passed the filter, %d of those hidden by the changed-rows bitmap; %s of columns touched; %d workers",
			st.BaseRows, st.ChunksRead, st.ChunksPruned, st.BasePassed, st.BaseHidden, hyBytes(st.ColBytes), st.Workers)
		y.detail(1, "Read tail:  %d rows scanned, %d passed the filter, %d superseded or deleted rows skipped", st.TailRows, st.TailPassed, st.TailDead)
		y.detail(1, "Read gap:   0 keys from the log (state lsn=%d covers the snapshot)", rep.state)
		y.detail(1, "Order keys: %d candidates ranked, %d md5 evaluated", st.Candidates, st.Md5Evals)
	} else {
		chunks := (rep.baseRows + 65535) / 65536
		y.detail(1, "Read base:  up to %d rows in %d chunks (zone maps prune at run time), %d dictionary/int columns; estimate", rep.baseRows, chunks, len(p.filters)+2)
		y.detail(1, "Read tail:  %d rows (estimate: whole tail)", rep.tailRows)
		y.detail(1, "Read gap:   %d lsn behind the snapshot (a read waits up to %v for the state; step 1 has no log-gap path)", gaps, s.Hybrid.Wait)
	}
	if p.dim != nil {
		if analyze && rep.used {
			y.node(1, "Hash Join (DENSE-BROADCAST build=%s %d rows read at snapshot %d, %d pass %s)", p.dim.name, rep.dimRows, rep.readSnap, rep.dimPass, strings.Join(p.dimOnConj, " AND "))
		} else {
			y.node(1, "Hash Join (DENSE-BROADCAST build=%s, at most %d rows, read at snapshot %d; %s)", p.dim.name, s.Hybrid.DimMaxRows, rep.readSnap, strings.Join(p.dimOnConj, " AND "))
		}
	}
	if analyze && rep.used {
		y.detail(1, "Read core:  %d rows fetched by key at snapshot %d (%s), each checked against the state", rep.coreRows, rep.readSnap, hyBytes(int64(rep.coreByte)))
	} else {
		maxRows := 0
		for _, g := range p.groups {
			for _, r := range g.Ranges {
				maxRows += r.Hi - r.Lo + 1
			}
		}
		y.detail(1, "Read core:  at most %d rows fetched by key (the page)", maxRows)
	}
	lane := "normal (R is never demoted to the heavy lane)"
	if s.heavy(q, nil) {
		lane = "heavy"
	}
	mem := rep.memBytes
	if mem == 0 {
		mem = 40 * 100 * 8
	}
	y.detail(1, "Memory: %s of heaps and candidates; lane: %s", hyBytes(mem), lane)
	y.detail(0, "Planning: shape=R2 top-N per partition, contract=%s", rep.contract)
	if analyze && rep.used {
		y.detail(0, "Execution: %s, %d rows returned", hyMS(rep.elapsed), rep.hits)
	}
	return y.lines
}

// hybrid is called by EXPLAIN for a SELECT: done=true when the plan was
// printed; otherwise note (when set) is appended after the ordinary plan.
func (x *explainer) hybrid(depth int, q *Select) (done bool, note string) {
	s := x.s
	if s.Hybrid == nil {
		return false, ""
	}
	p, why, cand := s.hyRecognize(q, x.params)
	if !cand {
		return false, ""
	}
	rep := &hyReport{plan: p}
	if why != "" {
		return false, "Hybrid not used: " + why
	}
	// the decision without waiting
	tx := x.tx
	if tx == nil {
		return false, "Hybrid not used: no snapshot"
	}
	state, w := s.hyContractDry(tx, rep)
	if w != "" {
		return false, "Hybrid not used: " + w
	}
	if state != nil {
		rep.state = state.LSN
		rep.info, _ = s.Hybrid.Seg.Info(p.fact.t.ID)
		if tb := state.Tbls[p.fact.t.ID]; tb != nil {
			rep.baseRows, rep.tailRows = tb.RowsBase(), tb.RowsTail()
		} else {
			return false, "Hybrid not used: table " + p.fact.name + ": " + s.Hybrid.Seg.Reason(s.E, p.fact.t.Name)
		}
	}
	for _, l := range s.hyLines(rep, q, false) {
		x.lines = append(x.lines, l)
	}
	return true, ""
}

// hyContractDry is hyContract without waiting: a state that has not reached
// the snapshot is reported as a wait, not as a failure.
func (s *Session) hyContractDry(tx *core.Tx, rep *hyReport) (*proj.SegState, string) {
	h := s.Hybrid
	f := h.Seg.Follower()
	if !h.Seg.Ready() {
		return nil, "the segment state is still being built"
	}
	mode := strings.ToLower(s.vars[SearchConsistencyVar])
	if mode == "" {
		mode = DefaultConsistency
	}
	if st := s.readStaleness(); st > 0 {
		mode = "bounded"
	}
	S := tx.Snapshot()
	rep.snap, rep.durable = S, h.E.DurableLSN()
	rep.applied, rep.lag = f.Applied(), f.Lag()
	switch mode {
	case "strong", "session":
		rep.contract = "strong"
		if mode == "session" {
			rep.contract = "strong (session)"
		}
		st := h.Seg.StateAt(S)
		if st == nil {
			return nil, fmt.Sprintf("snapshot lsn %d is older than the retained segment states", S)
		}
		rep.readSnap = S
		return st, ""
	case "bounded", "eventual":
		rep.contract = mode
		st := h.Seg.Current()
		if st == nil {
			return nil, "no segment state"
		}
		rep.readSnap = st.LSN
		return st, ""
	}
	return nil, fmt.Sprintf("unknown consistency mode %q", mode)
}

// hybridAnalyzed replaces the estimated block by the real counters after
// EXPLAIN ANALYZE ran the statement.
func (x *explainer) hybridAnalyzed(q Stmt) {
	s := x.s
	rep := s.hyLast
	sel, ok := q.(*Select)
	if s.Hybrid == nil || rep == nil || !ok {
		return
	}
	i := len(x.lines)
	for j, l := range x.lines {
		if strings.HasPrefix(l, "Hybrid Plan") {
			i = j
			break
		}
	}
	if !rep.used {
		// the dry decision printed a plan but the run fell back: show the plan that really ran
		x.lines = x.lines[:i]
		_ = x.general(0, sel)
		x.detail(0, "Hybrid not used: %s", rep.why)
		return
	}
	x.lines = append(x.lines[:i], s.hyLines(rep, sel, true)...)
}
