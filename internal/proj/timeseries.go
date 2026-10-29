package proj

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"runtime"
	"sort"
	"sync"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

// TimeSeries keeps append-only tables with a time column as compressed,
// time-pruned blocks: timestamps delta-of-delta + zigzag varint, float
// values XOR-with-previous (Gorilla-style, byte aligned), series ids varint.
type TimeSeries struct {
	mu      sync.RWMutex
	tables  map[uint32]*tsTable
	pending []tsRow
}

type tsRow struct {
	table  uint32
	t      int64
	series string
	vals   []float64
}

const blockRows = 8192

type tsBlock struct {
	minT, maxT int64
	n          int
	times      []byte
	series     []byte
	vals       [][]byte
	seriesSet  map[uint32]struct{}
}

type tsTable struct {
	mu        sync.RWMutex
	schema    *schema.Table
	timeCol   int
	seriesCol int
	valCols   []int
	valNames  []string
	blocks    []*tsBlock
	at        []int64
	as        []uint32
	av        [][]float64
	dict      dict
	rows      int64
	rawBytes  int64
	compBytes int64
}

func NewTimeSeries() *TimeSeries { return &TimeSeries{tables: map[uint32]*tsTable{}} }

func (ts *TimeSeries) Name() string { return "timeseries" }

func (ts *TimeSeries) table(id uint32) *tsTable {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.tables[id]
}

func (ts *TimeSeries) Has(id uint32) bool { return ts.table(id) != nil }

func (ts *TimeSeries) ensure(t *schema.Table) *tsTable {
	if t.TimeColumn == "" {
		return nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tt := ts.tables[t.ID]
	if tt == nil {
		tt = &tsTable{schema: t, seriesCol: -1, dict: dict{m: map[string]uint32{}}}
		tt.timeCol, _ = t.Col(t.TimeColumn)
		if t.SeriesColumn != "" {
			tt.seriesCol, _ = t.Col(t.SeriesColumn)
		}
		for i, c := range t.Columns {
			if (c.T == schema.TFloat || c.T == schema.TInt) && i != t.KeyIndex() {
				tt.valCols = append(tt.valCols, i)
				tt.valNames = append(tt.valNames, c.Name)
			}
		}
		tt.av = make([][]float64, len(tt.valCols))
		ts.tables[t.ID] = tt
	}
	return tt
}

func (tt *tsTable) toRow(table uint32, r schema.Row) tsRow {
	x := tsRow{table: table, t: r[tt.timeCol].I, vals: make([]float64, len(tt.valCols))}
	if tt.seriesCol >= 0 {
		x.series = r[tt.seriesCol].S
	}
	for i, c := range tt.valCols {
		v := r[c]
		switch {
		case v.Null:
			x.vals[i] = math.NaN()
		case tt.schema.Columns[c].T == schema.TInt:
			x.vals[i] = float64(v.I)
		default:
			x.vals[i] = v.F
		}
	}
	return x
}

func (ts *TimeSeries) Bootstrap(e *core.Engine, snap uint64) error {
	for _, s := range e.Tables() {
		tt := ts.ensure(s.Schema())
		if tt == nil {
			continue
		}
		var rows []tsRow
		err := e.ScanSnapshot(s.Schema().Name, snap, func(_ string, r schema.Row, _ *core.Version) bool {
			rows = append(rows, tt.toRow(s.Schema().ID, r))
			return true
		})
		if err != nil {
			return err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].t < rows[j].t })
		tt.mu.Lock()
		for _, r := range rows {
			tt.append(r)
		}
		tt.mu.Unlock()
	}
	return nil
}

func (ts *TimeSeries) Apply(e *core.Engine, rec *wal.Record) {
	if rec.Kind == wal.KindDDL {
		// changes so far are of whole transactions: apply them before a
		// dropped (or truncated) table's view is released
		ts.Flush(rec.LSN)
		live := map[uint32]bool{}
		for _, s := range e.Tables() {
			live[s.Schema().ID] = true
			if ts.table(s.Schema().ID) == nil {
				ts.ensure(s.Schema())
			}
		}
		ts.mu.Lock()
		for id := range ts.tables {
			if !live[id] {
				delete(ts.tables, id)
			}
		}
		ts.mu.Unlock()
		return
	}
	for _, op := range rec.Ops {
		tt := ts.table(op.Table)
		if tt == nil || op.Kind != wal.OpPut {
			continue // append-only tables never carry deletes
		}
		r, _, err := rowOf(e, &op, rec)
		if err != nil {
			continue
		}
		ts.pending = append(ts.pending, tt.toRow(op.Table, r))
	}
}

func (ts *TimeSeries) Flush(uint64) {
	if len(ts.pending) == 0 {
		return
	}
	// apply per table under one lock so a multi-op transaction becomes
	// visible atomically within each table view
	sort.SliceStable(ts.pending, func(i, j int) bool { return ts.pending[i].table < ts.pending[j].table })
	var cur *tsTable
	var curID uint32
	for _, r := range ts.pending {
		if cur == nil || r.table != curID {
			if cur != nil {
				cur.mu.Unlock()
			}
			cur, curID = ts.table(r.table), r.table
			cur.mu.Lock()
		}
		cur.append(r)
	}
	cur.mu.Unlock()
	clear(ts.pending)
	ts.pending = ts.pending[:0]
}

func (tt *tsTable) append(r tsRow) {
	tt.at = append(tt.at, r.t)
	tt.as = append(tt.as, tt.dict.code(r.series))
	for i, v := range r.vals {
		tt.av[i] = append(tt.av[i], v)
	}
	tt.rows++
	tt.rawBytes += int64(8 + 4 + 8*len(r.vals))
	if len(tt.at) >= blockRows {
		tt.seal()
	}
}

func zigzag(v int64) uint64   { return uint64((v << 1) ^ (v >> 63)) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

func (tt *tsTable) seal() {
	n := len(tt.at)
	b := &tsBlock{n: n, minT: math.MaxInt64, maxT: math.MinInt64, seriesSet: map[uint32]struct{}{}}
	var prevT, prevD int64
	for i, t := range tt.at {
		b.minT, b.maxT = min(b.minT, t), max(b.maxT, t)
		if i == 0 {
			b.times = binary.AppendVarint(b.times, t)
		} else {
			d := t - prevT
			b.times = binary.AppendUvarint(b.times, zigzag(d-prevD))
			prevD = d
		}
		prevT = t
	}
	for _, s := range tt.as {
		b.series = binary.AppendUvarint(b.series, uint64(s))
		b.seriesSet[s] = struct{}{}
	}
	for _, col := range tt.av {
		b.vals = append(b.vals, encodeFloats(col))
	}
	tt.blocks = append(tt.blocks, b)
	tt.compBytes += int64(len(b.times) + len(b.series))
	for _, v := range b.vals {
		tt.compBytes += int64(len(v))
	}
	tt.at, tt.as = tt.at[:0:0], tt.as[:0:0]
	for i := range tt.av {
		tt.av[i] = nil
	}
}

func (b *tsBlock) decode(cols []int) (ts []int64, series []uint32, vals [][]float64) {
	ts = make([]int64, b.n)
	p := 0
	var prevT, prevD int64
	for i := 0; i < b.n; i++ {
		if i == 0 {
			v, k := binary.Varint(b.times)
			p += k
			prevT = v
		} else {
			u, k := binary.Uvarint(b.times[p:])
			p += k
			prevD += unzigzag(u)
			prevT += prevD
		}
		ts[i] = prevT
	}
	series = make([]uint32, b.n)
	p = 0
	for i := range series {
		u, k := binary.Uvarint(b.series[p:])
		p += k
		series[i] = uint32(u)
	}
	vals = make([][]float64, len(b.vals))
	for _, c := range cols {
		vals[c] = decodeFloats(b.vals[c], b.n)
	}
	return
}

// encodeFloats picks, per block and column, the smaller of two lossless
// encodings: decimal (values that round-trip exactly as k/10^d, stored as
// zigzag deltas of k) or XOR-with-previous. First byte: 0 = XOR, 1+d = decimal.
func encodeFloats(col []float64) []byte {
	if d, ok := decimalDigits(col); ok {
		enc := []byte{byte(1 + d)}
		scale := math.Pow10(d)
		var prev int64
		for _, v := range col {
			k := int64(math.Round(v * scale))
			enc = binary.AppendUvarint(enc, zigzag(k-prev))
			prev = k
		}
		return enc
	}
	enc := []byte{0}
	var prev uint64
	for _, v := range col {
		x := math.Float64bits(v) ^ prev
		prev = math.Float64bits(v)
		if x == 0 {
			enc = append(enc, 0)
			continue
		}
		tz := bits.TrailingZeros64(x)
		enc = append(enc, byte(tz+1))
		enc = binary.AppendUvarint(enc, x>>tz)
	}
	return enc
}

// decimalDigits finds the smallest d <= 6 such that every value equals
// round(v*10^d)/10^d exactly (so decoding is bit-identical).
func decimalDigits(col []float64) (int, bool) {
	for d := 0; d <= 6; d++ {
		scale := math.Pow10(d)
		ok := true
		for _, v := range col {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v*scale) > 1<<52 {
				return 0, false
			}
			if k := math.Round(v * scale); k/scale != v {
				ok = false
				break
			}
		}
		if ok {
			return d, true
		}
	}
	return 0, false
}

func decodeFloats(enc []byte, n int) []float64 {
	out := make([]float64, n)
	mode := enc[0]
	p := 1
	if mode > 0 {
		scale := math.Pow10(int(mode - 1))
		var k int64
		for i := range out {
			u, w := binary.Uvarint(enc[p:])
			p += w
			k += unzigzag(u)
			out[i] = float64(k) / scale
		}
		return out
	}
	var prev uint64
	for i := range out {
		tag := enc[p]
		p++
		if tag != 0 {
			u, w := binary.Uvarint(enc[p:])
			p += w
			prev ^= u << (tag - 1)
		}
		out[i] = math.Float64frombits(prev)
	}
	return out
}

// ---- query ----

type TSQuery struct {
	Table    string `json:"table"`
	From     int64  `json:"from"` // unix ms, inclusive
	To       int64  `json:"to"`   // unix ms, exclusive
	Series   string `json:"series,omitempty"`
	BySeries bool   `json:"by_series,omitempty"`
	BucketMs int64  `json:"bucket_ms,omitempty"`
	Aggs     []Agg  `json:"aggs"`
}

type tsSpec struct {
	fn   string
	col  int // index into valCols, -1 = count(*)
	name string
}

type TSExec struct {
	q       *TSQuery
	specs   []tsSpec
	cols    []int
	groups  map[[2]int64]*group
	scanned int64
	names   map[int64]string
}

// NewTSExec validates q against the table schema; it is shared by the
// projection path and the core-fallback path.
func NewTSExec(t *schema.Table, q *TSQuery) (*TSExec, error) {
	if t.TimeColumn == "" {
		return nil, fmt.Errorf("table %s has no time_column", t.Name)
	}
	if q.To <= q.From {
		return nil, fmt.Errorf("empty time range")
	}
	if q.BucketMs < 0 {
		return nil, fmt.Errorf("bucket_ms must be >= 0")
	}
	if q.BucketMs > 0 && (q.To-q.From)/q.BucketMs > 1_000_000 {
		return nil, fmt.Errorf("too many buckets")
	}
	var valNames []string
	for i, c := range t.Columns {
		if (c.T == schema.TFloat || c.T == schema.TInt) && i != t.KeyIndex() {
			valNames = append(valNames, c.Name)
		}
	}
	x := &TSExec{q: q, groups: map[[2]int64]*group{}, names: map[int64]string{}}
	used := map[int]bool{}
	for _, a := range q.Aggs {
		s := tsSpec{fn: a.Fn, col: -1, name: a.As}
		switch a.Fn {
		case "count", "sum", "avg", "min", "max":
		default:
			return nil, fmt.Errorf("unknown aggregate %q", a.Fn)
		}
		if a.Col != "" {
			found := false
			for i, n := range valNames {
				if n == a.Col {
					s.col, found = i, true
				}
			}
			if !found {
				return nil, fmt.Errorf("%q is not a numeric value column", a.Col)
			}
			if !used[s.col] {
				used[s.col] = true
				x.cols = append(x.cols, s.col)
			}
		} else if a.Fn != "count" {
			return nil, fmt.Errorf("%s needs a column", a.Fn)
		}
		if s.name == "" {
			s.name = a.Fn
			if a.Col != "" {
				s.name += "_" + a.Col
			}
		}
		x.specs = append(x.specs, s)
	}
	if len(x.specs) == 0 {
		return nil, fmt.Errorf("at least one aggregate required")
	}
	return x, nil
}

func (x *TSExec) add(t int64, sid int64, series string, vals func(c int) float64) {
	if t < x.q.From || t >= x.q.To {
		return
	}
	x.scanned++
	b := int64(0)
	if x.q.BucketMs > 0 {
		b = x.q.From + (t-x.q.From)/x.q.BucketMs*x.q.BucketMs
	}
	if !x.q.BySeries {
		sid = 0
	}
	k := [2]int64{b, sid}
	g := x.groups[k]
	if g == nil {
		g = &group{accs: newAccs(len(x.specs))}
		x.groups[k] = g
		if x.q.BySeries {
			x.names[sid] = series
		}
	}
	for i, s := range x.specs {
		if s.col < 0 {
			g.accs[i].n++
			continue
		}
		if v := vals(s.col); !math.IsNaN(v) {
			g.accs[i].addFloat(v)
		}
	}
}

// AddRow feeds one row from the authoritative core (fallback path).
func (x *TSExec) AddRow(t *schema.Table, r schema.Row, seriesID func(string) int64) {
	tc, _ := t.Col(t.TimeColumn)
	series := ""
	if t.SeriesColumn != "" {
		sc, _ := t.Col(t.SeriesColumn)
		series = r[sc].S
	}
	if x.q.Series != "" && series != x.q.Series {
		return
	}
	var valIdx []int
	for i, c := range t.Columns {
		if (c.T == schema.TFloat || c.T == schema.TInt) && i != t.KeyIndex() {
			valIdx = append(valIdx, i)
		}
	}
	x.add(r[tc].I, seriesID(series), series, func(c int) float64 {
		v := r[valIdx[c]]
		if v.Null {
			return math.NaN()
		}
		if t.Columns[valIdx[c]].T == schema.TInt {
			return float64(v.I)
		}
		return v.F
	})
}

func (x *TSExec) merge(o *TSExec) {
	x.scanned += o.scanned
	for k, g := range o.groups {
		if m := x.groups[k]; m != nil {
			for i := range m.accs {
				m.accs[i].merge(&g.accs[i])
			}
		} else {
			x.groups[k] = g
		}
	}
	for k, v := range o.names {
		x.names[k] = v
	}
}

func (x *TSExec) Result() *Result {
	res := &Result{Scanned: x.scanned, Columns: []string{"bucket"}}
	if x.q.BySeries {
		res.Columns = append(res.Columns, "series")
	}
	for _, s := range x.specs {
		res.Columns = append(res.Columns, s.name)
	}
	keys := make([][2]int64, 0, len(x.groups))
	for k := range x.groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return x.names[keys[i][1]] < x.names[keys[j][1]]
	})
	for _, k := range keys {
		g := x.groups[k]
		row := []any{k[0]}
		if x.q.BySeries {
			row = append(row, x.names[k[1]])
		}
		for i, s := range x.specs {
			sp := aggSpec{fn: s.fn}
			row = append(row, sp.value(&g.accs[i]))
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

type TSStats struct {
	Rows      int64   `json:"rows"`
	Blocks    int     `json:"blocks"`
	RawBytes  int64   `json:"raw_bytes"`
	CompBytes int64   `json:"compressed_bytes"`
	Ratio     float64 `json:"compression_ratio"`
}

func (ts *TimeSeries) Stats(id uint32) TSStats {
	tt := ts.table(id)
	if tt == nil {
		return TSStats{}
	}
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	sealedRaw := tt.rawBytes - int64(len(tt.at))*int64(8+4+8*len(tt.valCols))
	s := TSStats{Rows: tt.rows, Blocks: len(tt.blocks), RawBytes: tt.rawBytes, CompBytes: tt.compBytes}
	if tt.compBytes > 0 {
		s.Ratio = float64(sealedRaw) / float64(tt.compBytes)
	}
	return s
}

// Query prunes blocks by [minT, maxT] and series membership, decodes the
// survivors in parallel and aggregates into time buckets.
func (ts *TimeSeries) Query(id uint32, q *TSQuery) (*Result, error) {
	tt := ts.table(id)
	if tt == nil {
		return nil, fmt.Errorf("table is not a time-series table")
	}
	x, err := NewTSExec(tt.schema, q)
	if err != nil {
		return nil, err
	}
	// sealed blocks are immutable: copy the list and the active tail, then
	// release the lock so ingestion is not blocked by the query
	tt.mu.RLock()
	blocks := append([]*tsBlock(nil), tt.blocks...)
	at := append([]int64(nil), tt.at...)
	as := append([]uint32(nil), tt.as...)
	av := make([][]float64, len(tt.av))
	for i := range tt.av {
		av[i] = append([]float64(nil), tt.av[i]...)
	}
	sid := int64(-1)
	if q.Series != "" {
		if c, ok := tt.dict.m[q.Series]; ok {
			sid = int64(c)
		} else {
			tt.mu.RUnlock()
			return x.Result(), nil
		}
	}
	names := append([]string(nil), tt.dict.vals...)
	tt.mu.RUnlock()

	var cand []*tsBlock
	for _, b := range blocks {
		if b.maxT < q.From || b.minT >= q.To {
			continue
		}
		if sid >= 0 {
			if _, ok := b.seriesSet[uint32(sid)]; !ok {
				continue
			}
		}
		cand = append(cand, b)
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, len(cand)))
	parts := make([]*TSExec, workers)
	var wg sync.WaitGroup
	next := make(chan *tsBlock, len(cand))
	for _, b := range cand {
		next <- b
	}
	close(next)
	for w := 0; w < workers; w++ {
		px, _ := NewTSExec(tt.schema, q)
		parts[w] = px
		wg.Add(1)
		go func(px *TSExec) {
			defer wg.Done()
			for b := range next {
				times, series, vals := b.decode(px.cols)
				for i, t := range times {
					if sid >= 0 && int64(series[i]) != sid {
						continue
					}
					i := i
					px.add(t, int64(series[i]), names[series[i]], func(c int) float64 { return vals[c][i] })
				}
			}
		}(px)
	}
	wg.Wait()
	for _, px := range parts {
		x.merge(px)
	}
	for i, t := range at {
		if sid >= 0 && int64(as[i]) != sid {
			continue
		}
		i := i
		x.add(t, int64(as[i]), names[as[i]], func(c int) float64 { return av[c][i] })
	}
	return x.Result(), nil
}
