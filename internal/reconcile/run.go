package reconcile

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/proj"
	"stask/internal/schema"
)

// HistoryView is the history store as the reconciler sees it (wired by
// staskd when -hist-store is on).
type HistoryView struct {
	Tables   []string
	Follower *core.Follower
	// Scan calls fn for every stored version of table with LSN <= maxLSN
	// (any order across keys; the versions of one key may come in any order).
	Scan func(table uint32, maxLSN uint64, fn func(key string, val []byte, lsn uint64, deleted bool)) error
	// InGap reports whether the store dropped history (disk full) and has
	// a hole.
	InGap func() bool
}

// Env is what a run needs from the server.
type Env struct {
	P       *planner.Platform
	History *HistoryView
	Node    string
}

const nb = 1024 // buckets of the checksum

func bucketOf(kh uint64) int { return int((kh >> 24) % nb) }

type sums struct {
	cnt [nb]int64
	sum [nb]uint64
}

func (s *sums) add(b int, h uint64) { s.cnt[b]++; s.sum[b] += h }

func (s *sums) total() (n int64, x uint64) {
	for i := range s.cnt {
		n += s.cnt[i]
		x += s.sum[i] * uint64(2*i+1) // a row moved between buckets changes the total
	}
	return
}

// target is one structure being compared with the core.
type target struct {
	name  string
	table *core.TableState
	rep   *StructReport
	fol   *core.Follower // nil: a core structure (secondary index) or history
	cols  []proj.RecCol
	idx   []int
	keyed bool
	walk  func(cols []proj.RecCol, cur *proj.RecCursor, max int, div uint64, fn func(key []byte, kh, rh uint64)) (bool, error)
	live  func() int64
	kind  string // "view", "index", "history"
	index string
}

type runner struct {
	ctx   context.Context
	env   Env
	e     *core.Engine
	o     Options
	start time.Time
}

// Run compares the core with its derived structures.
func Run(ctx context.Context, env Env, o Options) (*Report, error) {
	if o.Retries <= 0 {
		o.Retries = 3
	}
	if o.Chunk <= 0 {
		o.Chunk = 2048
	}
	if o.Limit <= 0 {
		o.Limit = 20
	}
	r := &runner{ctx: ctx, env: env, e: env.P.E, o: o, start: time.Now()}
	rep := &Report{Node: env.Node, At: r.start.UTC().Format(time.RFC3339), DurableLSN: r.e.DurableLSN(), Sample: o.Sample, Verdict: StInSync}
	want := map[string]bool{}
	for _, t := range o.Tables {
		want[t] = true
	}
	var tabs []*core.TableState
	for _, ts := range r.e.UserTables() {
		if len(want) == 0 || want[ts.Schema().Name] {
			tabs = append(tabs, ts)
			delete(want, ts.Schema().Name)
		}
	}
	for t := range want {
		return nil, fmt.Errorf("no such table: %s", t)
	}
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].Schema().Name < tabs[j].Schema().Name })
	for _, ts := range tabs {
		tr := r.table(ts)
		for _, s := range tr.Structures {
			rep.Verdict = Worse(rep.Verdict, s.Status)
		}
		rep.Tables = append(rep.Tables, tr)
	}
	if o.Slot != "" {
		sr, err := r.slot(o.Slot, tabs)
		if err != nil {
			return nil, err
		}
		rep.Slot = sr
		rep.Verdict = Worse(rep.Verdict, sr.Status)
		for _, st := range sr.Tables {
			if st.Status == StBehind {
				rep.Verdict = Worse(rep.Verdict, StBehind)
			}
		}
	}
	if o.Sample == 0 {
		rep.Notes = append(rep.Notes, "sample=0: only LSNs and row counts were compared; use sample=N (1/N of the keys) or sample=1 (every key) to compare content")
	}
	rep.DurationMs = time.Since(r.start).Milliseconds()
	return rep, nil
}

func (r *runner) div() uint64 {
	if r.o.Sample <= 1 {
		return 1
	}
	return uint64(r.o.Sample)
}

func (r *runner) base(name string, fol *core.Follower) StructReport {
	d := r.e.DurableLSN()
	s := StructReport{Name: name, DurableLSN: d, Status: StInSync, Checked: "lsn"}
	if fol != nil {
		s.AppliedLSN = fol.Applied()
		if d > s.AppliedLSN {
			s.BehindRecords = d - s.AppliedLSN
			s.BehindMs = fol.Lag().Milliseconds()
		}
	}
	return s
}

func (r *runner) table(ts *core.TableState) TableReport {
	sch := ts.Schema()
	id := sch.ID
	p := r.env.P
	tr := TableReport{Table: sch.Name, CoreRows: ts.LiveRows()}
	var ts_ []*target

	if p.Col.Has(id) && p.ColF != nil {
		if cols, keyed, _, ok := p.Col.RecCols(id); ok {
			rep := r.base("columnar", p.ColF)
			ts_ = append(ts_, &target{name: "columnar", table: ts, rep: &rep, fol: p.ColF, cols: cols, keyed: keyed, kind: "view",
				walk: func(c []proj.RecCol, cur *proj.RecCursor, max int, div uint64, fn func([]byte, uint64, uint64)) (bool, error) {
					return p.Col.RecWalk(id, c, cur, max, div, fn)
				},
				live: func() int64 { _, l := p.Col.Stats(id); return int64(l) }})
			if !keyed {
				rep.Detail = "string primary key: the view cannot list keys, so the checksum covers counts and total content only (a difference cannot be narrowed to a key range)"
			}
		}
	}
	if p.Sub != nil && p.SubF != nil {
		if cols, _, ok := p.Sub.RecCols(id); ok && len(cols) > 0 {
			rep := r.base("trigram("+colNames(cols)+")", p.SubF)
			ts_ = append(ts_, &target{name: rep.Name, table: ts, rep: &rep, fol: p.SubF, cols: cols, keyed: true, kind: "view",
				walk: func(c []proj.RecCol, cur *proj.RecCursor, max int, div uint64, fn func([]byte, uint64, uint64)) (bool, error) {
					return p.Sub.RecWalk(id, c, cur, max, div, fn)
				},
				live: func() int64 { l, _ := p.Sub.Stats(id); return int64(l) }})
		}
	}
	var lagOnly []*StructReport
	if p.Srch != nil && p.Srch.Has(id) {
		rep := r.base("search", p.SF)
		d, t := p.Srch.Stats(id)
		rep.ViewRows, rep.CoreRows = int64(d), ts.LiveRows()
		rep.Detail = fmt.Sprintf("%d documents, %d terms; content is not compared (lag only)", d, t)
		rep.Checked = "lsn"
		lagOnly = append(lagOnly, &rep)
		ts_ = append(ts_, &target{name: "search", rep: &rep, fol: p.SF, kind: "lagonly"})
	}
	if p.TS != nil && p.TS.Has(id) {
		rep := r.base("timeseries", p.TSF)
		rep.Detail = "content is not compared (lag only)"
		ts_ = append(ts_, &target{name: "timeseries", rep: &rep, fol: p.TSF, kind: "lagonly"})
	}
	for _, ix := range r.e.SecIndexNames(sch.Name) {
		rep := r.base("index:"+ix, nil)
		rep.AppliedLSN = rep.DurableLSN
		rep.Checked = "lsn"
		ts_ = append(ts_, &target{name: rep.Name, table: ts, rep: &rep, kind: "index", index: ix})
	}
	if h := r.env.History; h != nil && contains(h.Tables, sch.Name) && h.Follower != nil {
		rep := r.base("history", h.Follower)
		ts_ = append(ts_, &target{name: "history", table: ts, rep: &rep, fol: h.Follower, kind: "history"})
	}

	// status from LSNs and counts
	for _, t := range ts_ {
		r.quick(t, &tr)
	}
	// content
	if r.o.Sample > 0 {
		for _, t := range ts_ {
			if r.ctx.Err() != nil {
				t.rep.Detail = join(t.rep.Detail, "cancelled")
				break
			}
			if t.kind == "lagonly" || t.rep.Status == StNotReady {
				continue
			}
			r.content(t)
		}
	}
	for _, t := range ts_ {
		tr.Structures = append(tr.Structures, *t.rep)
	}
	return tr
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func colNames(cols []proj.RecCol) string {
	s := ""
	for i, c := range cols {
		if i > 0 {
			s += ","
		}
		s += c.Name
	}
	return s
}

// quick fills the status from the applied and durable LSN and the row counts.
func (r *runner) quick(t *target, tr *TableReport) {
	rep := t.rep
	if t.fol != nil && !t.fol.Ready() {
		rep.Status, rep.Detail = StNotReady, join(rep.Detail, "bootstrapping")
		return
	}
	if rep.BehindRecords > 0 {
		rep.Status = StBehind
	}
	if t.kind == "lagonly" || t.live == nil {
		return
	}
	if rep.BehindRecords > 0 {
		rep.Detail = join(rep.Detail, "counts are not compared while behind")
		return
	}
	// caught up: the counts must agree. The core counter is bumped by the
	// installer shortly after a commit is acknowledged, so look again a few
	// times before saying no.
	for i := 0; ; i++ {
		d0 := r.e.DurableLSN()
		cr, vr := t.table.LiveRows(), t.live()
		a := t.fol.Applied()
		d1 := r.e.DurableLSN()
		rep.CoreRows, rep.ViewRows, rep.Checked = cr, vr, "lsn+count"
		if cr == vr && a >= d0 {
			return
		}
		if i >= r.o.Retries+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
		if d1 != d0 {
			continue
		}
	}
	if r.e.DurableLSN() != rep.DurableLSN || t.fol.Applied() < r.e.DurableLSN() {
		rep.Status = Worse(rep.Status, StBehind)
		rep.Detail = join(rep.Detail, "counts differ while writes are in progress")
		return
	}
	rep.Status = StDiverged
	rep.Detail = join(rep.Detail, fmt.Sprintf("row count differs at a quiet point: core %d, structure %d", rep.CoreRows, rep.ViewRows))
}

func (r *runner) sleepChunk() {
	if r.o.Pause > 0 {
		time.Sleep(r.o.Pause)
	} else {
		runtime.Gosched()
	}
}

// corePass reads the table at snapshot S in chunks and gives each chunk to fn.
func (r *runner) corePass(table string, S uint64, fn func(keys []string, rows []schema.Row)) error {
	after := ""
	for {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		keys, rows, last, done, err := r.e.RecChunk(table, after, r.o.Chunk, S)
		if err != nil {
			return err
		}
		fn(keys, rows)
		if done {
			return nil
		}
		after = last
		r.sleepChunk()
	}
}

// viewPass walks a view in chunks.
func (r *runner) viewPass(t *target, fn func(key []byte, kh, rh uint64)) error {
	var cur proj.RecCursor
	div := r.div()
	for {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		done, err := t.walk(t.cols, &cur, 4096, div, fn)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		r.sleepChunk()
	}
}

var errTooMany = errors.New("too many differing keys to list")

func (r *runner) content(t *target) {
	switch t.kind {
	case "index":
		r.contentIndex(t)
	case "history":
		r.contentHistory(t)
	default:
		r.contentView(t)
	}
}

func (r *runner) finishLag(t *target) {
	// final status when what was compared is equal: IN_SYNC or BEHIND
	rep := t.rep
	d := r.e.DurableLSN()
	rep.DurableLSN = d
	if t.fol != nil {
		a := t.fol.Applied()
		rep.AppliedLSN = a
		rep.BehindRecords, rep.BehindMs = 0, 0
		if d > a {
			rep.BehindRecords, rep.BehindMs = d-a, t.fol.Lag().Milliseconds()
		}
	}
	if rep.Status == StDiverged { // the count check found a difference at a quiet point: a sample that happens to be equal does not clear it
		return
	}
	if rep.BehindRecords > 0 {
		rep.Status = StBehind
	} else {
		rep.Status = StInSync
	}
}

func hexs(x uint64) string { return fmt.Sprintf("%016x", x) }

func (r *runner) contentView(t *target) {
	rep := t.rep
	sch := t.table.Schema()
	div := r.div()
	rep.Checked = sampleLabel(r.o.Sample)
	if !t.keyed {
		rep.Checked = "all keys, total content only"
	}
	var cand map[string]string // keys that differed in every unstable pass
	for attempt := 0; ; attempt++ {
		if r.ctx.Err() != nil {
			return
		}
		idx, ok := proj.RecColIdx(sch, t.cols)
		if !ok {
			rep.Status, rep.Detail = StInconclusive, join(rep.Detail, "a column of the structure is not in the table schema (ALTER TABLE in progress?)")
			return
		}
		t.idx = idx
		S := t.fol.Applied()
		release := r.e.PinLSN(S)
		var cs, vs sums
		err := r.corePass(sch.Name, S, func(keys []string, rows []schema.Row) {
			for i, row := range rows {
				var kh uint64
				b := 0
				if t.keyed {
					kh = proj.RecKeyHash(keys[i])
					if div > 1 && kh%div != 0 {
						continue
					}
					b = bucketOf(kh)
				}
				cs.add(b, proj.RecRowHash(t.cols, t.idx, row))
			}
		})
		if err != nil {
			release()
			if errors.Is(err, core.ErrSnapshotTooOld) && attempt < r.o.Retries+2 {
				continue
			}
			rep.Status, rep.Detail = StInconclusive, join(rep.Detail, "reading the core: "+err.Error())
			return
		}
		a1 := t.fol.Applied()
		err = r.viewPass(t, func(key []byte, kh, rh uint64) {
			b := 0
			if t.keyed {
				b = bucketOf(kh)
			}
			vs.add(b, rh)
		})
		a2 := t.fol.Applied()
		release()
		if errors.Is(err, proj.ErrRecRestart) && attempt < r.o.Retries+2 {
			continue
		}
		if err != nil {
			rep.Status, rep.Detail = StInconclusive, join(rep.Detail, "reading the structure: "+err.Error())
			return
		}
		stable := a1 == S && a2 == S
		rep.CompareLSN = S
		cn, cx := cs.total()
		vn, vx := vs.total()
		rep.KeysChecked, rep.CoreSum, rep.ViewSum = cn, hexs(cx), hexs(vx)
		if div <= 1 || !t.keyed {
			rep.CoreRows, rep.ViewRows = cn, vn
		}
		var mism []int
		for b := 0; b < nb; b++ {
			if cs.cnt[b] != vs.cnt[b] || cs.sum[b] != vs.sum[b] {
				mism = append(mism, b)
			}
		}
		if len(mism) == 0 {
			if !stable {
				rep.Detail = join(rep.Detail, "equal, although the structure moved during the pass")
			}
			rep.Diff = nil
			r.finishLag(t)
			return
		}
		if !t.keyed {
			if stable {
				rep.Status = StDiverged
				rep.Detail = join(rep.Detail, fmt.Sprintf("content differs at LSN %d (core %d rows, structure %d rows); key range unknown", S, cn, vn))
				return
			}
			if attempt >= r.o.Retries {
				rep.Status = StInconclusive
				rep.Detail = join(rep.Detail, "differs while the structure keeps moving")
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// second pass: the exact keys of the differing buckets
		diffs, stable2, err := r.diffKeys(t, mism)
		if err != nil {
			if errors.Is(err, proj.ErrRecRestart) && attempt < r.o.Retries+2 {
				continue
			}
			if errors.Is(err, errTooMany) {
				rep.Status = StDiverged
				rep.Detail = join(rep.Detail, fmt.Sprintf("%d of %d buckets differ (too many keys to list)", len(mism), nb))
				rep.Diff = &Diff{Buckets: len(mism)}
				return
			}
			rep.Status, rep.Detail = StInconclusive, join(rep.Detail, err.Error())
			return
		}
		if len(diffs) == 0 {
			rep.Detail = join(rep.Detail, "a difference seen in the first pass was gone in the second (the structure was moving)")
			r.finishLag(t)
			return
		}
		if stable2 {
			rep.Status = StDiverged
			rep.Diff = r.mkDiff(diffs, len(mism))
			rep.Detail = join(rep.Detail, fmt.Sprintf("differs from the core at LSN %d", S))
			return
		}
		if cand == nil {
			cand = diffs
		} else {
			for k := range cand {
				if _, ok := diffs[k]; !ok {
					delete(cand, k)
				}
			}
		}
		if attempt >= r.o.Retries {
			if len(cand) > 0 {
				rep.Status = StSuspect
				rep.Diff = r.mkDiff(cand, len(mism))
				rep.Detail = join(rep.Detail, fmt.Sprintf("these keys differed in all %d passes while the structure moved", attempt+1))
			} else {
				rep.Detail = join(rep.Detail, "differences seen were transient (the structure was moving)")
				r.finishLag(t)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sampleLabel(n int) string {
	if n <= 1 {
		return "all keys"
	}
	return fmt.Sprintf("1/%d of the keys", n)
}

// diffKeys re-reads the buckets in mism on both sides and returns the keys
// that differ (key -> kind), and whether the structure stayed at the LSN the
// core was read at during its pass.
func (r *runner) diffKeys(t *target, mism []int) (map[string]string, bool, error) {
	const maxKeys = 400000
	in := map[int]bool{}
	for _, b := range mism {
		in[b] = true
	}
	div := r.div()
	sch := t.table.Schema()
	S := t.fol.Applied()
	release := r.e.PinLSN(S)
	defer release()
	cm := map[string]uint64{}
	err := r.corePass(sch.Name, S, func(keys []string, rows []schema.Row) {
		for i, row := range rows {
			kh := proj.RecKeyHash(keys[i])
			if div > 1 && kh%div != 0 || !in[bucketOf(kh)] || len(cm) > maxKeys {
				continue
			}
			cm[keys[i]] = proj.RecRowHash(t.cols, t.idx, row)
		}
	})
	if err != nil {
		return nil, false, err
	}
	if len(cm) > maxKeys {
		return nil, false, errTooMany
	}
	vm := map[string]uint64{}
	a1 := t.fol.Applied()
	err = r.viewPass(t, func(key []byte, kh, rh uint64) {
		if in[bucketOf(kh)] && len(vm) <= maxKeys {
			vm[string(key)] = rh
		}
	})
	a2 := t.fol.Applied()
	if err != nil {
		return nil, false, err
	}
	if len(vm) > maxKeys {
		return nil, false, errTooMany
	}
	out := map[string]string{}
	for k, h := range cm {
		if vh, ok := vm[k]; !ok {
			out[k] = "missing_in_view"
		} else if vh != h {
			out[k] = "changed"
		}
	}
	for k := range vm {
		if _, ok := cm[k]; !ok {
			out[k] = "extra_in_view"
		}
	}
	return out, a1 == S && a2 == S, nil
}

func (r *runner) mkDiff(m map[string]string, buckets int) *Diff {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := &Diff{Count: len(keys), Buckets: buckets}
	if len(keys) > 0 {
		d.FirstKey, d.LastKey = keys[0], keys[len(keys)-1]
	}
	for _, k := range keys {
		switch m[k] {
		case "missing_in_view":
			d.MissingInView++
		case "extra_in_view":
			d.ExtraInView++
		default:
			d.Changed++
		}
	}
	for _, k := range keys {
		if len(d.Examples) >= r.o.Limit {
			break
		}
		d.Examples = append(d.Examples, DiffEx{Key: k, Kind: m[k]})
	}
	return d
}

// contentIndex checks that every row has its entry in the secondary index.
func (r *runner) contentIndex(t *target) {
	rep := t.rep
	sch := t.table.Schema()
	div := r.div()
	rep.Checked = sampleLabel(r.o.Sample) + " (every row has its entry)"
	for attempt := 0; ; attempt++ {
		S := r.e.DurableLSN()
		release := r.e.PinLSN(S)
		var n int64
		missing := map[string]string{}
		unusable := ""
		err := r.corePass(sch.Name, S, func(keys []string, rows []schema.Row) {
			var ks []string
			var rs []schema.Row
			for i := range rows {
				if div > 1 && proj.RecKeyHash(keys[i])%div != 0 {
					continue
				}
				ks, rs = append(ks, keys[i]), append(rs, rows[i])
			}
			n += int64(len(ks))
			m, un, err := r.e.SecIndexMissing(sch.Name, ks, rs)
			if err != nil {
				unusable = err.Error()
				return
			}
			for ix, bad := range un {
				if ix == t.index {
					unusable = bad
				}
			}
			for _, k := range m[t.index] {
				if len(missing) < 400000 {
					missing[k] = "missing_in_view"
				}
			}
		})
		release()
		if err != nil {
			if errors.Is(err, core.ErrSnapshotTooOld) && attempt < r.o.Retries {
				continue
			}
			rep.Status, rep.Detail = StInconclusive, join(rep.Detail, err.Error())
			return
		}
		rep.KeysChecked, rep.CompareLSN, rep.CoreRows = n, S, n
		if unusable != "" {
			if attempt < r.o.Retries { // a storage conversion swaps the stores for a moment
				time.Sleep(50 * time.Millisecond)
				continue
			}
			rep.Status, rep.Detail = StDiverged, join(rep.Detail, "index is unusable: "+unusable)
			return
		}
		if len(missing) == 0 {
			rep.Status = StInSync
			return
		}
		if attempt < r.o.Retries {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		rep.Status = StDiverged
		rep.Diff = r.mkDiff(missing, 0)
		rep.Detail = join(rep.Detail, "rows whose index entry is missing: a lookup through this index would not find them")
		return
	}
}

// contentHistory compares the newest stored version of each key with the core.
func (r *runner) contentHistory(t *target) {
	rep := t.rep
	h := r.env.History
	sch := t.table.Schema()
	div := r.div()
	rep.Checked = sampleLabel(r.o.Sample) + " (newest stored version equals the row)"
	if h.InGap != nil && h.InGap() {
		rep.Detail = join(rep.Detail, "the store is in a gap (history was dropped while its disk was full)")
	}
	var all []proj.RecCol
	for _, c := range sch.Columns {
		all = append(all, proj.RecCol{Num: c.Num, Name: c.Name, Kind: 'a'})
	}
	idx, _ := proj.RecColIdx(sch, all)
	type hent struct {
		lsn uint64
		h   uint64
		del bool
	}
	for attempt := 0; ; attempt++ {
		S := h.Follower.Applied()
		release := r.e.PinLSN(S)
		newest := map[string]hent{}
		serr := h.Scan(sch.ID, S, func(key string, val []byte, lsn uint64, del bool) {
			if div > 1 && proj.RecKeyHash(key)%div != 0 {
				return
			}
			if cur, ok := newest[key]; ok && cur.lsn >= lsn {
				return
			}
			en := hent{lsn: lsn, del: del}
			if !del {
				if hr, err := sch.DecodeAt(val, lsn); err == nil {
					en.h = proj.RecRowHash(all, idx, hr)
				} else {
					en.h = 1
				}
			}
			newest[key] = en
		})
		if serr != nil {
			release()
			rep.Status, rep.Detail = StInconclusive, join(rep.Detail, "reading the history store: "+serr.Error())
			return
		}
		diffs := map[string]string{}
		var n, absent int64
		err := r.corePass(sch.Name, S, func(keys []string, rows []schema.Row) {
			for i, row := range rows {
				if div > 1 && proj.RecKeyHash(keys[i])%div != 0 {
					continue
				}
				n++
				en, found := newest[keys[i]]
				switch {
				case !found:
					absent++
				case en.del:
					diffs[keys[i]] = "extra_in_view"
				case en.h != proj.RecRowHash(all, idx, row):
					diffs[keys[i]] = "changed"
				}
			}
		})
		release()
		if err != nil {
			if errors.Is(err, core.ErrSnapshotTooOld) && attempt < r.o.Retries {
				continue
			}
			rep.Status, rep.Detail = StInconclusive, join(rep.Detail, err.Error())
			return
		}
		rep.KeysChecked, rep.CompareLSN = n, S
		if absent > 0 {
			rep.Detail = join(rep.Detail, fmt.Sprintf("%d sampled rows have no stored version (normal when the row was last written before history was switched on or older than the retention)", absent))
		}
		if len(diffs) == 0 {
			r.finishLag(t)
			return
		}
		if attempt < r.o.Retries && h.Follower.Applied() != S {
			continue
		}
		rep.Status = StDiverged
		if h.Follower.Applied() != S {
			rep.Status = StSuspect
		}
		rep.Diff = r.mkDiff(diffs, 0)
		return
	}
}
