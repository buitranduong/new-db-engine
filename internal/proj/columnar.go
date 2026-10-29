package proj

import (
	"encoding/binary"
	"fmt"
	"iter"
	"maps"
	"math"
	"math/bits"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

// Columnar is a column-oriented copy of the latest state of every table
// declared `columnar`. Updates append a new row version and mark the old
// position dead; a compaction rewrites the vectors when too many are dead.
type Columnar struct {
	mu      sync.RWMutex
	tables  map[uint32]*colTable
	pending []colChange
}

type colChange struct {
	table uint32
	key   string
	row   schema.Row    // nil = delete
	sch   *schema.Table // the schema row was decoded with
}

type dict struct {
	m    map[string]uint32
	vals []string
}

func (d *dict) code(s string) uint32 {
	if c, ok := d.m[s]; ok {
		return c
	}
	c := uint32(len(d.vals))
	d.m[s] = c
	d.vals = append(d.vals, s)
	return c
}

type colVec struct {
	t      schema.ColType
	ints   pvec[int64]
	floats vec[float64]
	codes  pvec[uint32]
	dict   *dict
	nulls  []uint64
	stored bool
	text   bool // a TEXT column, kept dictionary-encoded while it has few distinct values
	// derive maps a stored integer to the group value (date_trunc buckets
	// to their first millisecond); nil: the integer itself
	derive func(int64) int64
}

// intVal is the group value of integer v.
func (cv *colVec) intVal(v int64) int64 {
	if cv.derive != nil {
		return cv.derive(v)
	}
	return v
}

// textDictMax is the most distinct values a TEXT column may have in the
// columnar view. TEXT is what most schemas use for any string (statuses,
// categories as well as free text), so it is dictionary-encoded like a
// string column until it proves to be free text, then dropped from the view
// (as ClickHouse's LowCardinality falls back to plain strings).
const textDictMax = 1 << 16

type colTable struct {
	mu     sync.RWMutex
	schema *schema.Table
	cols   []colVec
	n      int
	live   []uint64
	dead   int
	keyPos map[string]uint32 // row position by key (non-integer keys)
	// integer primary keys: position index over the key column instead
	// of keyPos (see keyindex.go); kcol is that column, -1 if unused
	kcol int
	kidx *intPosIndex
}

func NewColumnar() *Columnar { return &Columnar{tables: map[uint32]*colTable{}} }

func (c *Columnar) Name() string { return "columnar" }

func newColTable(t *schema.Table) *colTable {
	ct := &colTable{schema: t, keyPos: map[string]uint32{}, cols: make([]colVec, len(t.Columns)), kcol: -1}
	if k := t.KeyIndex(); t.Columns[k].T.Store() == schema.TInt {
		ct.kcol, ct.kidx = k, &intPosIndex{}
	}
	for i, c := range t.Columns {
		ct.cols[i].t = c.T.Store()
		ct.cols[i].stored = true
		if c.T.Store() == schema.TText {
			ct.cols[i].t, ct.cols[i].text = schema.TString, true
		}
		if ct.cols[i].t == schema.TString {
			ct.cols[i].dict = &dict{m: map[string]uint32{}}
		}
	}
	return ct
}

// alter follows ALTER TABLE: added columns are not stored (queries on
// them run on rows) until the view is rebuilt at the next start; dropped
// ones are released. Columns are matched by number.
func (ct *colTable) alter(t *schema.Table) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.alterLocked(t)
}

func (ct *colTable) alterLocked(t *schema.Table) {
	if ct.schema == t {
		return
	}
	cols := make([]colVec, len(t.Columns))
	for i, c := range t.Columns {
		j := slices.IndexFunc(ct.schema.Columns, func(o schema.Column) bool { return o.Num == c.Num })
		if j >= 0 && j < len(ct.cols) {
			cols[i] = ct.cols[j]
		} else {
			cols[i] = colVec{t: c.T.Store()}
		}
	}
	ct.cols = cols
	if ct.kcol >= 0 {
		ct.kcol = t.KeyIndex()
	}
	ct.schema = t
}

// rowOf decodes the row of a put with the newest schema, which it returns
// (rows stored before an ALTER TABLE have other columns).
func rowOf(e *core.Engine, op *wal.Op, rec *wal.Record) (schema.Row, *schema.Table, error) {
	t := e.TableByID(op.Table)
	if t == nil {
		return nil, nil, core.ErrNoTable
	}
	lsn := rec.LSN
	if rec.Kind == wal.KindMove {
		lsn = op.OrigLSN
	}
	sch := t.Schema()
	r, err := sch.DecodeAt(op.Val, lsn)
	return r, sch, err
}

func (c *Columnar) table(id uint32) *colTable {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tables[id]
}

func (c *Columnar) ensure(t *schema.Table) *colTable {
	if !t.Columnar {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ct := c.tables[t.ID]
	if ct == nil {
		ct = newColTable(t)
		c.tables[t.ID] = ct
	}
	return ct
}

func (c *Columnar) Has(tableID uint32) bool { return c.table(tableID) != nil }

// Bootstrap fills each table before publishing it: no lock is held during
// the scan (minutes at 10^8 rows), which would block readers of the view's
// stats, such as /v1/status, for as long. Queries do not read the view
// before it is Ready.
func (c *Columnar) Bootstrap(e *core.Engine, snap uint64) error {
	for _, ts := range e.Tables() {
		if !ts.Schema().Columnar {
			continue
		}
		ct := newColTable(ts.Schema())
		err := e.ScanSnapshot(ts.Schema().Name, snap, func(key string, r schema.Row, _ *core.Version) bool {
			ct.put(key, r)
			return true
		})
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.tables[ts.Schema().ID] = ct
		c.mu.Unlock()
	}
	return nil
}

func (c *Columnar) Apply(e *core.Engine, rec *wal.Record) {
	if rec.Kind == wal.KindDDL {
		// changes so far are of whole transactions: apply them before a
		// dropped (or truncated) table's view is released
		c.Flush(rec.LSN)
		live := map[uint32]bool{}
		for _, ts := range e.Tables() {
			live[ts.Schema().ID] = true
			if ct := c.table(ts.Schema().ID); ct == nil {
				c.ensure(ts.Schema())
			} else {
				ct.alter(ts.Schema())
			}
		}
		c.mu.Lock()
		for id := range c.tables {
			if !live[id] {
				delete(c.tables, id)
			}
		}
		c.mu.Unlock()
		return
	}
	for _, op := range rec.Ops {
		ct := c.table(op.Table)
		if ct == nil {
			continue
		}
		ch := colChange{table: op.Table}
		if op.Kind != wal.OpPut || ct.kcol < 0 {
			ch.key = string(op.Key) // integer keys of puts come from the row
		}
		if op.Kind == wal.OpPut {
			r, sch, err := rowOf(e, &op, rec)
			if err != nil {
				continue
			}
			ch.row, ch.sch = r, sch
		}
		c.pending = append(c.pending, ch)
	}
}

func (c *Columnar) Flush(uint64) {
	if len(c.pending) == 0 {
		return
	}
	// apply per table under one lock so a multi-op transaction becomes
	// visible atomically within each table view
	sort.SliceStable(c.pending, func(i, j int) bool { return c.pending[i].table < c.pending[j].table })
	var cur *colTable
	var curID uint32
	for _, ch := range c.pending {
		if cur == nil || ch.table != curID {
			if cur != nil {
				cur.maybeCompact()
				cur.mu.Unlock()
			}
			cur, curID = c.table(ch.table), ch.table
			cur.mu.Lock()
		}
		if ch.row == nil {
			cur.del(ch.key)
		} else {
			// decoded with a schema an ALTER TABLE published before its
			// DDL record reached this view
			cur.alterLocked(ch.sch)
			cur.put(ch.key, ch.row)
		}
	}
	cur.maybeCompact()
	cur.mu.Unlock()
	clear(c.pending)
	c.pending = c.pending[:0]
}

func setBit(b []uint64, i int) []uint64 {
	for i/64 >= len(b) {
		b = append(b, 0)
	}
	b[i/64] |= 1 << (i % 64)
	return b
}

func (ct *colTable) del(key string) {
	if ct.kidx != nil {
		k, err := strconv.ParseInt(key, 10, 64)
		if err == nil {
			ct.delPos(k)
		}
		return
	}
	if p, ok := ct.keyPos[key]; ok {
		ct.live[p/64] &^= 1 << (p % 64)
		delete(ct.keyPos, key)
		ct.dead++
	}
}

func (ct *colTable) delPos(k int64) {
	if p, ok := ct.kidx.del(k, &ct.cols[ct.kcol].ints); ok {
		ct.live[p/64] &^= 1 << (p % 64)
		ct.dead++
	}
}

func (ct *colTable) put(key string, r schema.Row) {
	if ct.kidx != nil {
		ct.delPos(r[ct.kcol].I)
	} else {
		ct.del(key)
	}
	pos := ct.n
	for i := range ct.cols {
		cv := &ct.cols[i]
		if !cv.stored {
			continue
		}
		v := r[i]
		if v.Null {
			cv.nulls = setBit(cv.nulls, pos)
		}
		switch cv.t {
		case schema.TInt, schema.TTime, schema.TBool:
			cv.ints.push(v.I)
		case schema.TFloat:
			cv.floats.push(v.F)
		case schema.TString:
			cv.codes.push(cv.dict.code(v.S))
			if cv.text && len(cv.dict.vals) > textDictMax {
				*cv = colVec{t: cv.t, text: true} // free text: not stored
			}
		}
	}
	ct.live = setBit(ct.live, pos)
	if ct.kidx != nil {
		ct.kidx.set(r[ct.kcol].I, uint32(pos), &ct.cols[ct.kcol].ints)
	} else {
		ct.keyPos[key] = uint32(pos)
	}
	ct.n++
}

func (ct *colTable) maybeCompact() {
	if ct.dead < 1<<16 || ct.dead*2 < ct.n {
		return
	}
	nt := newColTable(ct.schema)
	for i := range nt.cols {
		nt.cols[i].dict = ct.cols[i].dict // keep dictionaries
		nt.cols[i].stored = ct.cols[i].stored
	}
	var keyOf []string
	if ct.kidx == nil {
		keyOf = make([]string, ct.n)
		for k, p := range ct.keyPos {
			keyOf[p] = k
		}
	}
	for p := 0; p < ct.n; p++ {
		if ct.live[p/64]&(1<<(p%64)) == 0 {
			continue
		}
		np := nt.n
		for i := range nt.cols {
			cv, ov := &nt.cols[i], &ct.cols[i]
			if !cv.stored {
				continue
			}
			if len(ov.nulls) > p/64 && ov.nulls[p/64]&(1<<(p%64)) != 0 {
				cv.nulls = setBit(cv.nulls, np)
			}
			switch cv.t {
			case schema.TInt, schema.TTime, schema.TBool:
				cv.ints.push(ov.ints.at(p))
			case schema.TFloat:
				cv.floats.push(ov.floats.at(p))
			case schema.TString:
				cv.codes.push(ov.codes.at(p))
			}
		}
		nt.live = setBit(nt.live, np)
		if nt.kidx != nil {
			nt.kidx.set(nt.cols[nt.kcol].ints.at(np), uint32(np), &nt.cols[nt.kcol].ints)
		} else {
			nt.keyPos[keyOf[p]] = uint32(np)
		}
		nt.n++
	}
	ct.cols, ct.n, ct.live, ct.dead, ct.keyPos, ct.kidx = nt.cols, nt.n, nt.live, 0, nt.keyPos, nt.kidx
}

// MemBytes is the memory a table's columns take (values, NULL and live
// bitmaps; dictionaries and the key index not included).
func (c *Columnar) MemBytes(tableID uint32) int64 {
	ct := c.table(tableID)
	if ct == nil {
		return 0
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	n := int64(len(ct.live) * 8)
	for i := range ct.cols {
		cv := &ct.cols[i]
		n += cv.ints.bytes() + cv.codes.bytes() + int64(len(cv.floats.ch))*chunkRows*8 + int64(len(cv.nulls)*8)
	}
	return n
}

// Stats: rows stored (incl. dead) and live rows.
func (c *Columnar) Stats(tableID uint32) (int, int) {
	ct := c.table(tableID)
	if ct == nil {
		return 0, 0
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return ct.n, ct.n - ct.dead
}

// ---- vectorized execution ----

type colFilter struct {
	cfilter
	code  uint32
	known bool          // for string equality: whether the literal exists in the dict
	codes []bool        // for string IN / NOT IN: the dictionary codes in the list
	or    [][]colFilter // opOr
}

// prepFilter resolves f against the columns (dictionary codes of string
// constants). Called with ct.mu held.
func (ct *colTable) prepFilter(f cfilter, notStored func(int) error) (colFilter, error) {
	cf := colFilter{cfilter: f}
	if f.op == opOr {
		for _, conj := range f.or {
			var b []colFilter
			for _, x := range conj {
				pf, err := ct.prepFilter(x, notStored)
				if err != nil {
					return cf, err
				}
				b = append(b, pf)
			}
			cf.or = append(cf.or, b)
		}
		return cf, nil
	}
	if err := notStored(f.col); err != nil {
		return cf, err
	}
	cv := &ct.cols[f.col]
	if cv.t == schema.TString && (f.op == opIn || f.op == opNotIn) {
		cf.codes = make([]bool, len(cv.dict.vals))
		for v := range f.in {
			if c, ok := cv.dict.m[v.S]; ok {
				cf.codes[c] = true
			}
		}
	}
	if cv.t == schema.TString && (f.op == opEq || f.op == opNe) {
		cf.code, cf.known = cv.dict.m[f.v.S]
	}
	return cf, nil
}

// Aggregate runs q over the table, chunk-parallel. Each worker evaluates
// filters column-at-a-time into a selection vector and aggregates the
// selected positions into a private hash table; partials are merged.
func (c *Columnar) Aggregate(tableID uint32, q *AggQuery) (*Result, error) {
	ct := c.table(tableID)
	if ct == nil {
		return nil, fmt.Errorf("table is not columnar")
	}
	ct.mu.RLock()
	t, colTypes := ct.schema, make([]schema.ColType, len(ct.cols)) // ALTER TABLE replaces both
	for i := range ct.cols {
		colTypes[i] = ct.cols[i].t
	}
	ct.mu.RUnlock()
	distinctX := -1
	if hasDistinct(q.Aggs) {
		if len(q.GroupBy) > 1 {
			return aggregateDistinct(t, q, func(inner *AggQuery) (*Result, error) { return c.Aggregate(tableID, inner) })
		}
		xi, err := distinctCol(t, q)
		if err != nil {
			return nil, err
		}
		distinctX = xi
	}
	fs, err := compileFilters(t, q.Filters)
	if err != nil {
		return nil, err
	}
	specs, err := compileAggs(t, q.Aggs)
	if err != nil {
		return nil, err
	}
	var gcols []int
	for _, g := range q.GroupBy {
		ci, ok := t.Col(g)
		if !ok {
			return nil, fmt.Errorf("unknown group column %q", g)
		}
		if colTypes[ci] == schema.TFloat {
			return nil, fmt.Errorf("cannot group by %s column %q", t.Columns[ci].Type, g)
		}
		gcols = append(gcols, ci)
	}
	if len(q.GroupTrunc) > len(q.GroupBy) {
		return nil, fmt.Errorf("group_trunc has more entries than group_by")
	}

	ct.mu.RLock()
	defer ct.mu.RUnlock()

	// date_trunc group columns: aggregate over a view of the table with a
	// bucket column appended for each
	var extra []colVec
	for gi, unit := range q.GroupTrunc {
		if unit == "" {
			continue
		}
		u, err := lookupUnitFor(unit, ct.schema.Columns[gcols[gi]].T)
		if err != nil {
			return nil, err
		}
		if !ct.schema.Columns[gcols[gi]].T.IsTime() {
			return nil, fmt.Errorf("date_trunc(%s): %q is not a timestamp column", unit, q.GroupBy[gi])
		}
		cv, release := ct.truncColumn(&ct.cols[gcols[gi]], u, q)
		defer release()
		gcols[gi] = len(ct.cols) + len(extra)
		extra = append(extra, cv)
	}
	if len(extra) > 0 {
		ct = &colTable{schema: ct.schema, cols: append(slices.Clip(ct.cols), extra...), n: ct.n,
			live: ct.live, dead: ct.dead, keyPos: ct.keyPos, kcol: ct.kcol, kidx: ct.kidx}
	}

	notStored := func(ci int) error {
		if ct.cols[ci].stored {
			return nil
		}
		if !ct.cols[ci].text {
			return fmt.Errorf("column %q was added by ALTER TABLE and is not in the columnar view until a restart", t.Columns[ci].Name)
		}
		return fmt.Errorf("column %q has more than %d distinct values and is not in the columnar view; use search", t.Columns[ci].Name, textDictMax)
	}
	for _, ci := range gcols {
		if err := notStored(ci); err != nil {
			return nil, err
		}
	}
	for _, s := range specs {
		if s.col >= 0 {
			if err := notStored(s.col); err != nil {
				return nil, err
			}
		}
	}
	cfs := make([]colFilter, len(fs))
	for i, f := range fs {
		if cfs[i], err = ct.prepFilter(f, notStored); err != nil {
			return nil, err
		}
	}

	n := ct.n
	nchunks := (n + chunkRows - 1) / chunkRows
	workers := runtime.GOMAXPROCS(0)
	if q.Workers > 0 {
		workers = q.Workers
	}
	workers = min(workers, max(nchunks, 1))
	for i := range specs {
		specs[i].nullEmpty = q.SQLNulls
	}
	if distinctX >= 0 {
		if err := notStored(distinctX); err != nil {
			return nil, err
		}
		distinct := make([]bool, len(specs))
		for i, a := range q.Aggs {
			distinct[i] = a.Distinct
		}
		return ct.aggregateDistinctFast(gcols, distinctX, specs, distinct, cfs, q, workers), nil
	}
	if len(gcols) == 1 {
		if lo, r, ok := ct.denseRange(gcols[0], len(specs)); ok {
			return ct.aggregateDense(gcols[0], lo, r, specs, cfs, q, workers), nil
		}
	}
	// Two-level aggregation: each worker splits its single-key groups into
	// nparts partitions by key hash; partition i of every worker is then
	// merged by one goroutine, so merging runs in parallel without locks.
	nparts := workers
	type partial struct {
		groups  []map[uint64]*group // by partition
		sgroups map[string]*group   // NULL key or several group columns
		scanned int64
		slab    groupSlab
	}
	parts := make([]partial, workers)
	for w := range parts {
		p := &parts[w]
		p.groups = make([]map[uint64]*group, nparts)
		for i := range p.groups {
			p.groups[i] = map[uint64]*group{}
		}
		p.sgroups = map[string]*group{}
	}
	next := chunkQueue(nchunks)
	singleKey := len(gcols) <= 1
	q.parallel(workers, func(w int) {
		p := &parts[w]
		sel := make([]uint32, 0, chunkRows)
		var kb []byte
		for ci := range next {
			var live int
			sel, live = ct.selectChunk(sel, ci, cfs)
			p.scanned += int64(live)
			for _, pos := range sel {
				var g *group
				if singleKey {
					var k uint64
					isNull := false
					if len(gcols) == 1 {
						cv := &ct.cols[gcols[0]]
						if cv.isNull(int(pos)) {
							isNull = true
						} else if cv.t == schema.TString {
							k = uint64(cv.codes.at(int(pos)))
						} else {
							k = uint64(cv.ints.at(int(pos)))
						}
					}
					if isNull {
						g = p.sgroups["\x00null"]
						if g == nil {
							g = ct.newGroup(&p.slab, gcols, specs, int(pos))
							p.sgroups["\x00null"] = g
						}
					} else {
						m := p.groups[partOf(k, nparts)]
						if g = m[k]; g == nil {
							g = ct.newGroup(&p.slab, gcols, specs, int(pos))
							m[k] = g
						}
					}
				} else {
					kb = kb[:0]
					for _, gc := range gcols {
						kb = ct.cols[gc].appendKey(kb, int(pos))
					}
					g = p.sgroups[string(kb)]
					if g == nil {
						g = ct.newGroup(&p.slab, gcols, specs, int(pos))
						p.sgroups[string(kb)] = g
					}
				}
				ct.accumulate(g.accs, specs, pos)
			}
			if !q.keep(w) {
				break
			}
		}
	})

	// Merge: partition i of every worker into the first worker's, one
	// goroutine per partition. Keys are the same in every worker.
	var scanned int64
	for _, p := range parts {
		scanned += p.scanned
	}
	merged := make([]map[uint64]*group, nparts)
	q.each(nparts, func(pi int) {
		m := parts[0].groups[pi]
		for _, p := range parts[1:] {
			for k, g := range p.groups[pi] {
				if into := m[k]; into != nil {
					for i := range into.accs {
						into.accs[i].merge(&g.accs[i])
					}
				} else {
					m[k] = g
				}
			}
		}
		merged[pi] = m
	})
	smerged := map[string]*group{}
	for _, p := range parts {
		for k, g := range p.sgroups {
			if into := smerged[k]; into != nil {
				for i := range into.accs {
					into.accs[i].merge(&g.accs[i])
				}
			} else {
				smerged[k] = g
			}
		}
	}
	if len(smerged) > 0 {
		// rare: NULL keys or several group columns. A row's group is in
		// exactly one of the two kinds of maps, so the keys never collide.
		all := map[string]*group{}
		var kb []byte
		for _, m := range merged {
			for k, g := range m {
				kb = binary.LittleEndian.AppendUint64(append(kb[:0], 'u'), k)
				all[string(kb)] = g
			}
		}
		for k, g := range smerged {
			all["s"+k] = g
		}
		return finish(q.GroupBy, specs, maps.Values(all), len(all), q, scanned), nil
	}
	ngroups := 0
	for _, m := range merged {
		ngroups += len(m)
	}
	return finish(q.GroupBy, specs, func(yield func(*group) bool) {
		for _, m := range merged {
			for _, g := range m {
				if !yield(g) {
					return
				}
			}
		}
	}, ngroups, q, scanned), nil
}

// mergeUnion returns the sorted union of a and b (both sorted, without
// duplicates), reusing nothing of b.
func mergeUnion(a, b []uint32) []uint32 {
	if len(a) == 0 {
		return append([]uint32(nil), b...)
	}
	out := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i, j = i+1, j+1
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}

// selectChunk returns the live rows of chunk ci that pass the filters, and
// how many live rows the chunk has.
func (ct *colTable) selectChunk(sel []uint32, ci int, cfs []colFilter) ([]uint32, int) {
	lo, hi := ci*chunkRows, min((ci+1)*chunkRows, ct.n)
	sel = sel[:0]
	for i := lo; i < hi; {
		word := ct.live[i/64]
		if i%64 == 0 && hi-i >= 64 {
			for word != 0 {
				b := bits.TrailingZeros64(word)
				sel = append(sel, uint32(i+b))
				word &= word - 1
			}
			i += 64
			continue
		}
		if word&(1<<(i%64)) != 0 {
			sel = append(sel, uint32(i))
		}
		i++
	}
	live := len(sel)
	for fi := range cfs {
		sel = ct.filter(&cfs[fi], sel)
	}
	return sel, live
}

// accumulate adds row pos to a group's accumulators.
func (ct *colTable) accumulate(accs []acc, specs []aggSpec, pos uint32) {
	for ai := range specs {
		ct.add(&accs[ai], &specs[ai], pos)
	}
}

// accumulateBatch adds rows sel to their groups: row sel[i] goes to group
// keys[i], whose accumulators are accs[keys[i]*len(specs):]. It runs one
// loop per aggregate, with the common cases (count, sum over a column
// without NULLs) as tight loops: vectorized execution instead of
// interpreting every aggregate for every row.
func (ct *colTable) accumulateBatch(accs []acc, specs []aggSpec, keys []int32, sel []uint32) {
	na := len(specs)
	for ai := range specs {
		s := &specs[ai]
		if s.col < 0 {
			for _, k := range keys {
				accs[int(k)*na+ai].n++
			}
			continue
		}
		cv := &ct.cols[s.col]
		switch {
		case len(cv.nulls) > 0 || s.minmax:
			for i, k := range keys {
				ct.add(&accs[int(k)*na+ai], s, sel[i])
			}
		// sel may span chunks (rows routed to a partition): one run of
		// positions in the same chunk at a time
		case cv.t == schema.TFloat:
			for i := 0; i < len(sel); {
				ci := sel[i] >> chunkShift
				vals := cv.floats.ch[ci]
				for ; i < len(sel) && sel[i]>>chunkShift == ci; i++ {
					a := &accs[int(keys[i])*na+ai]
					a.n++
					a.fsum += vals[sel[i]&chunkMask]
				}
			}
		case cv.t == schema.TInt || cv.t == schema.TTime || cv.t == schema.TBool:
			for i := 0; i < len(sel); {
				ci := sel[i] >> chunkShift
				vals := cv.ints.arr(int(ci))
				for ; i < len(sel) && sel[i]>>chunkShift == ci; i++ {
					a := &accs[int(keys[i])*na+ai]
					a.n++
					a.isum += vals[sel[i]&chunkMask]
				}
				cv.ints.release(int(ci), vals)
			}
		default:
			for _, k := range keys {
				accs[int(k)*na+ai].n++
			}
		}
	}
}

// add adds row pos to one aggregate's accumulator.
func (ct *colTable) add(a *acc, s *aggSpec, pos uint32) {
	if s.col < 0 {
		a.n++
		return
	}
	cv := &ct.cols[s.col]
	if cv.isNull(int(pos)) {
		return
	}
	switch cv.t {
	case schema.TInt, schema.TTime, schema.TBool:
		if s.minmax {
			a.addInt(cv.ints.at(int(pos)))
		} else {
			a.n++
			a.isum += cv.ints.at(int(pos))
		}
	case schema.TFloat:
		if s.minmax {
			a.addFloat(cv.floats.at(int(pos)))
		} else {
			a.n++
			a.fsum += cv.floats.at(int(pos))
		}
	default:
		a.n++
	}
}

// Dense aggregation. When the group column's values fall in a range that
// is not much larger than the table (dictionary codes always do; integer
// keys such as customer ids usually do), the accumulators live in one
// flat array indexed by key - offset: no hashing, no per-group
// allocation. This is what ClickHouse does for small integer keys
// (FixedHashMap); here it applies to any range up to about the row count.
const (
	denseSmall    = 1 << 12   // up to this many keys every worker aggregates privately
	denseMaxBytes = 256 << 20 // accumulator memory the dense path may use
	// many keys: accumulator bytes per partition (about half an L2 cache)
	// and the most partitions (fan-out of the routing pass)
	densePartBytes = 512 << 10
	denseMaxParts  = 1024
)

// denseRange reports whether the group column can be aggregated densely:
// its smallest value and the number of possible keys.
func (ct *colTable) denseRange(gc, naccs int) (lo int64, r int, ok bool) {
	cv := &ct.cols[gc]
	switch cv.t {
	case schema.TString:
		r = len(cv.dict.vals)
	case schema.TInt, schema.TTime, schema.TBool:
		if ct.n == 0 {
			return 0, 0, false
		}
		lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
		for ci := 0; ci < cv.ints.chunks(); ci++ { // dead and NULL rows only widen the range
			l, h := cv.ints.minMax(ci)
			lo, hi = min(lo, l), max(hi, h)
		}
		if uint64(hi-lo) >= uint64(max(denseSmall, 2*ct.n)) {
			return 0, 0, false
		}
		r = int(hi-lo) + 1
		return lo, r, r*naccs*int(unsafe.Sizeof(acc{})) <= denseMaxBytes
	default:
		return 0, 0, false
	}
	return 0, r, r > 0 && r <= max(denseSmall, 2*ct.n) && r*naccs*int(unsafe.Sizeof(acc{})) <= denseMaxBytes
}

// aggregateDense groups by one column whose keys are in [lo, lo+r).
// Few keys: every worker aggregates its chunks into a private array and the
// arrays are added up. Many keys: workers only route the selected row
// positions to key-range partitions (aligned to 64 keys), then one
// goroutine per partition aggregates its range of the shared array, so no
// two goroutines write the same accumulator or bitmap word.
func (ct *colTable) aggregateDense(gc int, lo int64, r int, specs []aggSpec, cfs []colFilter, q *AggQuery, workers int) *Result {
	cv := &ct.cols[gc]
	na := len(specs)
	// keysOf appends the group index of each row of sel to keys
	keysOf := func(keys []int32, sel []uint32) []int32 { // sel: one chunk
		if len(sel) == 0 {
			return keys
		}
		ci := int(sel[0] >> chunkShift)
		if cv.t == schema.TString {
			return appendKeys(&cv.codes, ci, sel, 0, keys)
		}
		return appendKeys(&cv.ints, ci, sel, lo, keys)
	}
	small := r <= denseSmall
	hasNulls := len(cv.nulls) > 0
	// Many keys: route rows to key-range partitions small enough that one
	// partition's accumulators stay in the CPU cache while it is aggregated
	// (radix partitioning: Müller et al., "Cache-Efficient Aggregation:
	// Hashing Is Sorting", SIGMOD 2015; ClickHouse's two-level tables split
	// into 256 buckets). Partitions are aligned to 64 keys, so no two
	// goroutines share a word of the seen bitmap.
	nparts := 1
	if !small {
		nparts = min(max(workers, r*na*int(unsafe.Sizeof(acc{}))/densePartBytes), denseMaxParts)
	}
	width := ((r+nparts-1)/nparts + 63) &^ 63
	nparts = (r + width - 1) / width
	type partial struct {
		accs, null []acc
		seen       []uint64
		hasNull    bool
		route      *routeBuf // many keys: rows and their keys by partition
		scanned    int64
	}
	parts := make([]partial, workers)
	for w := range parts {
		p := &parts[w]
		p.null = newAccs(na)
		if small {
			p.accs = newAccs(r * na)
			p.seen = make([]uint64, (r+63)/64)
		} else {
			p.route = getRouteBuf(nparts)
			defer routeBufs.Put(p.route)
		}
	}
	next := chunkQueue((ct.n + chunkRows - 1) / chunkRows)
	q.parallel(workers, func(w int) {
		p := &parts[w]
		sel := make([]uint32, 0, chunkRows)
		keys := make([]int32, 0, chunkRows)
		for ci := range next {
			var live int
			sel, live = ct.selectChunk(sel, ci, cfs)
			p.scanned += int64(live)
			if hasNulls { // NULL keys aside, then the rest as usual
				rest := sel[:0]
				for _, pos := range sel {
					if cv.isNull(int(pos)) {
						p.hasNull = true
						ct.accumulate(p.null, specs, pos)
					} else {
						rest = append(rest, pos)
					}
				}
				sel = rest
			}
			keys = keysOf(keys[:0], sel)
			if small {
				for _, k := range keys {
					p.seen[k/64] |= 1 << (k % 64)
				}
				ct.accumulateBatch(p.accs, specs, keys, sel)
			} else {
				rb := p.route
				for i, k := range keys {
					pi := int(k) / width
					rb.pos[pi] = append(rb.pos[pi], sel[i])
					rb.keys[pi] = append(rb.keys[pi], k)
				}
			}
			if !q.keep(w) {
				break
			}
		}
	})

	var accs []acc
	var seen []uint64
	if small {
		accs, seen = parts[0].accs, parts[0].seen
		for _, p := range parts[1:] {
			for i := range accs {
				accs[i].merge(&p.accs[i])
			}
			for i := range seen {
				seen[i] |= p.seen[i]
			}
		}
	} else {
		ab := getAccBuf(r * na)
		defer accBufs.Put(ab)
		accs = ab.accs
		seen = make([]uint64, (r+63)/64)
		pq := chunkQueue(nparts)
		q.parallel(workers, func(int) {
			for pi := range pq {
				part := accs[pi*width*na : min((pi+1)*width, r)*na]
				for i := range part {
					part[i].reset()
				}
				for _, p := range parts {
					keys := p.route.keys[pi]
					for _, k := range keys {
						seen[k/64] |= 1 << (k % 64)
					}
					ct.accumulateBatch(accs, specs, keys, p.route.pos[pi])
				}
			}
		})
	}

	var scanned int64
	null := parts[0].null
	hasNull := false
	for i := range parts {
		scanned += parts[i].scanned
		hasNull = hasNull || parts[i].hasNull
		if i > 0 {
			for j := range null {
				null[j].merge(&parts[i].null[j])
			}
		}
	}
	ngroups := 0
	for _, w := range seen {
		ngroups += bits.OnesCount64(w)
	}
	if hasNull {
		ngroups++
	}
	dval := func(k int) any {
		if cv.t == schema.TString {
			return cv.dict.vals[k]
		}
		return cv.intVal(lo + int64(k))
	}
	// the groups in up to 4 parts per worker (by ranges of the seen
	// bitmap), so that a top-k is selected in parallel
	nsplit := min(4*workers, len(seen))
	if small || q.Limit <= 0 {
		nsplit = 1
	}
	parts2 := make([]iter.Seq[*group], 0, nsplit+1)
	if hasNull {
		parts2 = append(parts2, func(yield func(*group) bool) {
			yield(&group{vals: []any{nil}, accs: null})
		})
	}
	for i := 0; i < nsplit; i++ {
		wlo, whi := i*len(seen)/nsplit, (i+1)*len(seen)/nsplit
		parts2 = append(parts2, func(yield func(*group) bool) {
			g := &group{dval: dval} // reused: finish copies what it keeps
			for wi := wlo; wi < whi; wi++ {
				for w := seen[wi]; w != 0; w &= w - 1 {
					k := wi*64 + bits.TrailingZeros64(w)
					g.dkey, g.accs = k, accs[k*na:k*na+na]
					if !yield(g) {
						return
					}
				}
			}
		})
	}
	return finishParts(q.GroupBy, specs, parts2, ngroups, q, scanned)
}

func (cv *colVec) isNull(pos int) bool {
	return len(cv.nulls) > pos/64 && cv.nulls[pos/64]&(1<<(pos%64)) != 0
}

func (cv *colVec) appendKey(kb []byte, pos int) []byte {
	if cv.isNull(pos) {
		return append(kb, 0, 0xff)
	}
	if cv.t == schema.TString {
		c := cv.codes.at(int(pos))
		return append(kb, 1, byte(c), byte(c>>8), byte(c>>16), byte(c>>24), 0xff)
	}
	v := uint64(cv.ints.at(int(pos)))
	return append(kb, 2, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56), 0xff)
}

// Buffers of the many-keys dense path, reused across queries: an aggregate
// over many groups would otherwise allocate tens of MB per query, and every
// collection that this triggers scans the whole heap (the table's
// columns and key map), which cost about a fifth of the query time.
type routeBuf struct {
	pos  [][]uint32
	keys [][]int32
}

var routeBufs, accBufs sync.Pool

func getRouteBuf(nparts int) *routeBuf {
	rb, _ := routeBufs.Get().(*routeBuf)
	if rb == nil {
		rb = &routeBuf{}
	}
	for len(rb.pos) < nparts {
		rb.pos, rb.keys = append(rb.pos, nil), append(rb.keys, nil)
	}
	rb.pos, rb.keys = rb.pos[:nparts], rb.keys[:nparts]
	for i := range rb.pos {
		rb.pos[i], rb.keys[i] = rb.pos[i][:0], rb.keys[i][:0]
	}
	return rb
}

type accBuf struct{ accs []acc }

// getAccBuf returns n accumulators, not reset.
func getAccBuf(n int) *accBuf {
	ab, _ := accBufs.Get().(*accBuf)
	if ab == nil {
		ab = &accBuf{}
	}
	if cap(ab.accs) < n {
		ab.accs = make([]acc, n)
	}
	ab.accs = ab.accs[:n]
	return ab
}

// chunkQueue hands out chunk numbers to the workers of one aggregate.
func chunkQueue(n int) chan int {
	next := make(chan int, n)
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	return next
}

// parallel runs fn(0..n-1): 0 on the calling goroutine, the others on
// helpers started through q.Spawn (plain goroutines when it is nil). A
// helper that cannot start runs on the calling goroutine afterwards; in a
// scan it then finds the chunks taken and returns at once.
func (q *AggQuery) parallel(n int, fn func(w int)) {
	var wg sync.WaitGroup
	var inline []int
	for w := 1; w < n; w++ {
		wg.Add(1)
		f := func() { defer wg.Done(); fn(w) }
		if q.Spawn == nil {
			go f()
		} else if !q.Spawn(f) {
			wg.Done()
			inline = append(inline, w)
		}
	}
	fn(0)
	for _, w := range inline {
		fn(w)
	}
	wg.Wait()
}

// each runs fn(i) for i in [0, n) on at most q.Workers goroutines (all
// cores when unset), each taking the next i when done with one: tasks
// that could not get a helper are then still spread over the ones that
// did, instead of running one after another on the caller.
func (q *AggQuery) each(n int, fn func(i int)) {
	workers := q.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	var next atomic.Int64
	q.parallel(min(workers, n), func(int) {
		for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
			fn(i)
		}
	})
}

// keep reports whether scan worker w may take another chunk: not once the
// query is interrupted (every worker), else helpers as q.Keep says.
func (q *AggQuery) keep(w int) bool {
	if q.Interrupted != nil && q.Interrupted() {
		return false
	}
	return w == 0 || q.Keep == nil || q.Keep(w)
}

// partOf spreads keys over n partitions (Fibonacci hashing, so that
// consecutive keys do not land together).
func partOf(k uint64, n int) int {
	return int((k * 0x9E3779B97F4A7C15 >> 32) % uint64(n))
}

// groupSlab hands out groups and their accumulators from large blocks: an
// aggregate over many groups otherwise makes several allocations per group.
type groupSlab struct {
	gs   []group
	accs []acc
	vals []any
}

func (s *groupSlab) new(naccs, nvals int) *group {
	const block = 4096
	if len(s.gs) == 0 {
		s.gs = make([]group, block)
	}
	if len(s.accs) < naccs {
		s.accs = make([]acc, max(block*naccs, naccs))
	}
	if len(s.vals) < nvals {
		s.vals = make([]any, max(block*nvals, nvals))
	}
	g := &s.gs[0]
	s.gs = s.gs[1:]
	g.accs = s.accs[:naccs:naccs]
	s.accs = s.accs[naccs:]
	for i := range g.accs {
		g.accs[i].reset()
	}
	g.vals = s.vals[:0:nvals]
	s.vals = s.vals[nvals:]
	return g
}

func (ct *colTable) newGroup(slab *groupSlab, gcols []int, specs []aggSpec, pos int) *group {
	g := slab.new(len(specs), len(gcols))
	for _, gc := range gcols {
		cv := &ct.cols[gc]
		switch {
		case cv.isNull(pos):
			g.vals = append(g.vals, nil)
		case cv.t == schema.TString:
			g.vals = append(g.vals, cv.dict.vals[cv.codes.at(int(pos))])
		default:
			g.vals = append(g.vals, cv.intVal(cv.ints.at(int(pos))))
		}
	}
	return g
}

// filter narrows sel in place, reading one column.
func (ct *colTable) filter(f *colFilter, sel []uint32) []uint32 {
	if f.op == opOr {
		// union of the branches, kept in row order
		var union, b []uint32
		for _, conj := range f.or {
			b = append(b[:0], sel...)
			for i := range conj {
				b = ct.filter(&conj[i], b)
			}
			union = mergeUnion(union, b)
		}
		return append(sel[:0], union...)
	}
	cv := &ct.cols[f.col]
	out := sel[:0]
	if len(sel) == 0 {
		return out
	}
	// sel is one chunk (selectChunk): index that chunk's arrays directly
	var ints *[chunkRows]int64
	var floats *[chunkRows]float64
	var codes *[chunkRows]uint32
	ci := int(sel[0] >> chunkShift)
	switch cv.t {
	case schema.TInt, schema.TTime, schema.TBool:
		ints = cv.ints.arr(ci)
		defer cv.ints.release(ci, ints)
	case schema.TFloat:
		floats = cv.floats.arr(sel[0])
	case schema.TString:
		codes = cv.codes.arr(ci)
		defer cv.codes.release(ci, codes)
	}
	switch f.op {
	case opIsNull, opNotNull:
		want := f.op == opIsNull
		for _, p := range sel {
			if cv.isNull(int(p)) == want {
				out = append(out, p)
			}
		}
		return out
	case opIn, opNotIn:
		want := f.op == opIn
		for _, p := range sel {
			if cv.isNull(int(p)) {
				continue
			}
			var ok bool
			switch cv.t {
			case schema.TString:
				ok = f.codes[codes[p&chunkMask]]
			case schema.TFloat:
				ok = f.in[schema.Value{F: floats[p&chunkMask]}]
			default:
				ok = f.in[schema.Value{I: ints[p&chunkMask]}]
			}
			if ok == want {
				out = append(out, p)
			}
		}
		return out
	}
	switch cv.t {
	case schema.TInt, schema.TTime, schema.TBool:
		x := f.v.I
		for _, p := range sel {
			if !cv.isNull(int(p)) && cmpResult(cmpInt(ints[p&chunkMask], x), f.op) {
				out = append(out, p)
			}
		}
	case schema.TFloat:
		x := f.v.F
		for _, p := range sel {
			if !cv.isNull(int(p)) && cmpResult(cmpFloat(floats[p&chunkMask], x), f.op) {
				out = append(out, p)
			}
		}
	case schema.TString:
		switch {
		case f.op == opEq && !f.known:
			return out
		case f.op == opEq:
			for _, p := range sel {
				if codes[p&chunkMask] == f.code && !cv.isNull(int(p)) {
					out = append(out, p)
				}
			}
		case f.op == opNe && !f.known:
			for _, p := range sel {
				if !cv.isNull(int(p)) {
					out = append(out, p)
				}
			}
		case f.op == opNe:
			for _, p := range sel {
				if codes[p&chunkMask] != f.code && !cv.isNull(int(p)) {
					out = append(out, p)
				}
			}
		default:
			for _, p := range sel {
				if !cv.isNull(int(p)) && cmpResult(compareStr(cv.dict.vals[codes[p&chunkMask]], f.v.S), f.op) {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
