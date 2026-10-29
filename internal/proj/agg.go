// Package proj holds the derived representations. Each one is a pure
// function of the authoritative log, can be dropped and rebuilt, and
// publishes the LSN it reflects.
package proj

import (
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"stask/internal/schema"
)

type Filter struct {
	Col   string `json:"col"`
	Op    string `json:"op"`
	Value any    `json:"value"`
	// Op "or": a row passes when it passes every filter of one of Any
	Any [][]Filter `json:"any,omitempty"`
}

type Agg struct {
	Fn  string `json:"fn"`
	Col string `json:"col,omitempty"`
	As  string `json:"as,omitempty"`
	// Distinct: count(DISTINCT col); all distinct aggregates of a query
	// must be on the same column
	Distinct bool `json:"distinct,omitempty"`
}

type AggQuery struct {
	Table   string   `json:"table"`
	Filters []Filter `json:"filters,omitempty"`
	GroupBy []string `json:"group_by,omitempty"`
	// GroupTrunc[i], when set, groups by date_trunc(GroupTrunc[i], GroupBy[i])
	GroupTrunc []string `json:"group_trunc,omitempty"`
	Aggs       []Agg    `json:"aggs"`
	OrderBy    string   `json:"order_by,omitempty"`
	Desc       bool     `json:"desc,omitempty"`
	Limit      int      `json:"limit,omitempty"`

	// Set by in-process callers (SQL), not by the HTTP API.
	SQLNulls bool              `json:"-"` // sum over no values is NULL, as in SQL
	Workers  int               `json:"-"` // scan goroutines (0 = GOMAXPROCS)
	Spawn    func(func()) bool `json:"-"` // starts a helper; false = none may start now
	Keep     func(w int) bool  `json:"-"` // helper w may take another chunk
	// Interrupted, polled once per chunk by every scan worker, stops the
	// scan when it returns true (statement cancel or timeout); the result
	// is then partial and the caller must discard it.
	Interrupted func() bool `json:"-"`
}

type Result struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Scanned int64    `json:"scanned_rows"`
}

const (
	opEq = iota
	opNe
	opLt
	opLe
	opGt
	opGe
	opIn      // value is a list
	opNotIn   // value is a list; NULL does not pass
	opIsNull  // no value
	opNotNull // no value
	opOr      // any of the conjunctions in or
)

type cfilter struct {
	col int
	t   schema.ColType
	op  int
	v   schema.Value
	in  map[schema.Value]bool // opIn, opNotIn
	or  [][]cfilter           // opOr
}

func compileFilters(t *schema.Table, fs []Filter) ([]cfilter, error) {
	out := make([]cfilter, 0, len(fs))
	for _, f := range fs {
		if strings.EqualFold(f.Op, "or") {
			cf := cfilter{op: opOr, col: -1}
			for _, conj := range f.Any {
				b, err := compileFilters(t, conj)
				if err != nil {
					return nil, err
				}
				cf.or = append(cf.or, b)
			}
			out = append(out, cf)
			continue
		}
		ci, ok := t.Col(f.Col)
		if !ok {
			return nil, fmt.Errorf("unknown column %q", f.Col)
		}
		ct := t.Columns[ci].T.Store()
		op := map[string]int{"=": opEq, "==": opEq, "!=": opNe, "<": opLt, "<=": opLe, ">": opGt, ">=": opGe,
			"in": opIn, "not in": opNotIn, "is null": opIsNull, "is not null": opNotNull}
		o, ok := op[strings.ToLower(f.Op)]
		if !ok {
			return nil, fmt.Errorf("unknown operator %q", f.Op)
		}
		cf := cfilter{col: ci, t: ct, op: o}
		switch o {
		case opIsNull, opNotNull:
		case opIn, opNotIn:
			list, ok := f.Value.([]any)
			if !ok {
				return nil, fmt.Errorf("filter %s: in needs a list", f.Col)
			}
			cf.in = make(map[schema.Value]bool, len(list))
			for _, x := range list {
				v, err := schema.ValueOf(ct, x)
				if err != nil {
					return nil, fmt.Errorf("filter %s: %w", f.Col, err)
				}
				cf.in[v] = true
			}
		default:
			v, err := schema.ValueOf(ct, f.Value)
			if err != nil {
				return nil, fmt.Errorf("filter %s: %w", f.Col, err)
			}
			cf.v = v
		}
		out = append(out, cf)
	}
	return out, nil
}

func cmpResult(c int, op int) bool {
	switch op {
	case opEq:
		return c == 0
	case opNe:
		return c != 0
	case opLt:
		return c < 0
	case opLe:
		return c <= 0
	case opGt:
		return c > 0
	default:
		return c >= 0
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f *cfilter) matchRow(r schema.Row) bool {
	if f.op == opOr {
	branches:
		for _, conj := range f.or {
			for i := range conj {
				if !conj[i].matchRow(r) {
					continue branches
				}
			}
			return true
		}
		return false
	}
	v := r[f.col]
	switch {
	case f.op == opIsNull:
		return v.Null
	case f.op == opNotNull:
		return !v.Null
	case v.Null:
		return false
	case f.op == opIn:
		return f.in[valueKey(f.t, v)]
	case f.op == opNotIn:
		return !f.in[valueKey(f.t, v)]
	}
	switch f.t {
	case schema.TInt, schema.TTime, schema.TBool:
		return cmpResult(cmpInt(v.I, f.v.I), f.op)
	case schema.TFloat:
		return cmpResult(cmpFloat(v.F, f.v.F), f.op)
	default:
		return cmpResult(strings.Compare(v.S, f.v.S), f.op)
	}
}

// ---- accumulators shared by every execution path ----

type aggSpec struct {
	fn     string
	col    int // -1 for count(*)
	isInt  bool
	minmax bool // min or max: the only functions that need the extremes
	// nullEmpty: sum over no values is NULL (SQL) rather than 0
	nullEmpty bool
	name      string
}

func compileAggs(t *schema.Table, as []Agg) ([]aggSpec, error) {
	if len(as) == 0 {
		return nil, fmt.Errorf("at least one aggregate required")
	}
	out := make([]aggSpec, 0, len(as))
	for _, a := range as {
		s := aggSpec{fn: strings.ToLower(a.Fn), col: -1, name: a.As}
		s.minmax = s.fn == "min" || s.fn == "max"
		switch s.fn {
		case "count", "sum", "avg", "min", "max":
		default:
			return nil, fmt.Errorf("unknown aggregate %q", a.Fn)
		}
		if a.Col != "" {
			ci, ok := t.Col(a.Col)
			if !ok {
				return nil, fmt.Errorf("unknown column %q", a.Col)
			}
			ct := t.Columns[ci].T.Store()
			if s.fn != "count" && ct != schema.TInt && ct != schema.TFloat && ct != schema.TTime {
				return nil, fmt.Errorf("%s(%s): column is not numeric", a.Fn, a.Col)
			}
			s.col, s.isInt = ci, ct == schema.TInt || ct == schema.TTime
		} else if s.fn != "count" {
			return nil, fmt.Errorf("%s needs a column", a.Fn)
		}
		if s.name == "" {
			s.name = s.fn
			if a.Col != "" {
				s.name += "_" + a.Col
			}
		}
		out = append(out, s)
	}
	return out, nil
}

type acc struct {
	n          int64
	isum       int64
	fsum       float64
	min, max   float64 // float columns
	imin, imax int64   // integer columns: exact beyond 2^53
}

func newAccs(n int) []acc {
	a := make([]acc, n)
	for i := range a {
		a[i].reset()
	}
	return a
}

func (a *acc) reset() {
	*a = acc{min: math.Inf(1), max: math.Inf(-1), imin: math.MaxInt64, imax: math.MinInt64}
}

func (a *acc) addInt(v int64) {
	a.n++
	a.isum += v
	a.imin, a.imax = min(a.imin, v), max(a.imax, v)
}

func (a *acc) addFloat(v float64) {
	a.n++
	a.fsum += v
	a.min = math.Min(a.min, v)
	a.max = math.Max(a.max, v)
}

func (a *acc) merge(b *acc) {
	a.n += b.n
	a.isum += b.isum
	a.fsum += b.fsum
	a.min = math.Min(a.min, b.min)
	a.max = math.Max(a.max, b.max)
	a.imin, a.imax = min(a.imin, b.imin), max(a.imax, b.imax)
}

func (s *aggSpec) value(a *acc) any {
	switch s.fn {
	case "count":
		return a.n
	case "sum":
		if a.n == 0 && s.nullEmpty {
			return nil
		}
		if s.isInt {
			return a.isum
		}
		return a.fsum
	case "avg":
		if a.n == 0 {
			return nil
		}
		if s.isInt {
			return float64(a.isum) / float64(a.n)
		}
		return a.fsum / float64(a.n)
	case "min":
		if a.n == 0 {
			return nil
		}
		if s.isInt {
			return a.imin
		}
		return a.min
	default:
		if a.n == 0 {
			return nil
		}
		if s.isInt {
			return a.imax
		}
		return a.max
	}
}

// group is the per-group state: the group-by values and one acc per agg.
type group struct {
	vals []any
	accs []acc
	// A dense group (one group column) may carry its key index instead of
	// vals: dval(dkey) is its value, boxed only for the groups finish keeps.
	dkey int
	dval func(int) any
	// RowAggregator: the values seen by each count(DISTINCT) aggregate
	sets []map[schema.Value]struct{}
}

func (g *group) val(i int) any {
	if g.dval != nil {
		return g.dval(g.dkey)
	}
	return g.vals[i]
}

func (g *group) clone() *group {
	if g.dval != nil {
		return &group{vals: []any{g.dval(g.dkey)}, accs: g.accs}
	}
	return &group{vals: slices.Clone(g.vals), accs: g.accs}
}

// sortKey is a value of the order column without boxing it: ordering
// hundreds of thousands of groups otherwise allocates for each of them.
type sortKey struct {
	kind uint8 // 0 NULL, 1 int, 2 float, 3 other (v)
	i    int64
	f    float64
	v    any
}

func keyOfAny(v any) sortKey {
	switch x := v.(type) {
	case nil:
		return sortKey{}
	case int64:
		return sortKey{kind: 1, i: x}
	case float64:
		return sortKey{kind: 2, f: x}
	}
	return sortKey{kind: 3, v: v}
}

func (k sortKey) any() any {
	switch k.kind {
	case 1:
		return k.i
	case 2:
		return k.f
	}
	return k.v
}

// cmpKey orders like cmpAny (NULL last).
func cmpKey(a, b sortKey) int {
	if a.kind == b.kind {
		switch a.kind {
		case 0:
			return 0
		case 1:
			return cmpInt(a.i, b.i)
		case 2:
			return cmpFloat(a.f, b.f)
		}
	}
	return cmpAny(a.any(), b.any())
}

// key is value(a) as a sortKey.
func (s *aggSpec) key(a *acc) sortKey {
	switch {
	case s.fn == "count":
		return sortKey{kind: 1, i: a.n}
	case a.n == 0 && (s.fn != "sum" || s.nullEmpty):
		return sortKey{}
	case s.fn == "avg":
		if s.isInt {
			return sortKey{kind: 2, f: float64(a.isum) / float64(a.n)}
		}
		return sortKey{kind: 2, f: a.fsum / float64(a.n)}
	case !s.isInt:
		switch s.fn {
		case "sum":
			return sortKey{kind: 2, f: a.fsum}
		case "min":
			return sortKey{kind: 2, f: a.min}
		}
		return sortKey{kind: 2, f: a.max}
	}
	switch s.fn {
	case "sum":
		return sortKey{kind: 1, i: a.isum}
	case "min":
		return sortKey{kind: 1, i: a.imin}
	}
	return sortKey{kind: 1, i: a.imax}
}

// finish orders the groups and turns them into result rows. groups may
// reuse the *group it yields (and its vals) for the next one: finish copies
// the groups it keeps. The order is
// total (the order column, then the group columns, which identify a
// group), so no stable sort is needed; with a limit only the first Limit
// groups are selected (a bounded heap) and turned into rows.
func finish(groupCols []string, specs []aggSpec, groups iter.Seq[*group], ngroups int, q *AggQuery, scanned int64) *Result {
	return finishParts(groupCols, specs, []iter.Seq[*group]{groups}, ngroups, q, scanned)
}

// finishParts is finish over groups given in parts. With a limit, each part
// selects its own best Limit groups in parallel (q.parallel) and only those
// are merged.
func finishParts(groupCols []string, specs []aggSpec, parts []iter.Seq[*group], ngroups int, q *AggQuery, scanned int64) *Result {
	res := &Result{Scanned: scanned}
	res.Columns = append(res.Columns, groupCols...)
	for _, s := range specs {
		res.Columns = append(res.Columns, s.name)
	}
	ord := -1
	if q.OrderBy != "" {
		for i, c := range res.Columns {
			if c == q.OrderBy {
				ord = i
			}
		}
	}
	keyOf := func(g *group) sortKey {
		switch {
		case ord < 0:
			return sortKey{}
		case ord < len(groupCols):
			return keyOfAny(g.val(ord))
		}
		i := ord - len(groupCols)
		return specs[i].key(&g.accs[i])
	}
	cmp := func(a, b cand) int {
		if ord >= 0 {
			if c := cmpKey(a.key, b.key); c != 0 {
				if q.Desc {
					return -c
				}
				return c
			}
		}
		for k := range groupCols {
			if c := cmpAny(a.g.val(k), b.g.val(k)); c != 0 {
				return c
			}
		}
		return 0
	}
	var cs []cand
	if len(groupCols) == 0 && ngroups == 0 {
		cs = []cand{{g: &group{accs: newAccs(len(specs))}}}
	} else if q.Limit > 0 && q.Limit < ngroups {
		best := make([][]cand, len(parts))
		q.each(len(parts), func(i int) { best[i] = topK(parts[i], q.Limit, keyOf, cmp) })
		cs = topK(func(yield func(*group) bool) {
			for _, b := range best {
				for _, c := range b {
					if !yield(c.g) {
						return
					}
				}
			}
		}, q.Limit, keyOf, cmp)
	} else {
		cs = make([]cand, 0, ngroups)
		for _, groups := range parts {
			for g := range groups {
				g = g.clone()
				cs = append(cs, cand{g, keyOf(g)})
			}
		}
	}
	slices.SortFunc(cs, cmp)
	if q.Limit > 0 && len(cs) > q.Limit {
		cs = cs[:q.Limit]
	}
	res.Rows = make([][]any, 0, len(cs))
	for _, c := range cs {
		row := append(make([]any, 0, len(c.g.vals)+len(specs)), c.g.vals...)
		for i := range specs {
			row = append(row, specs[i].value(&c.g.accs[i]))
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

type cand struct {
	g   *group
	key sortKey // value of the order column
}

// topK returns the best k groups (copies) by cmp, in no particular order:
// a max-heap whose top is the worst one kept.
func topK(groups iter.Seq[*group], k int, keyOf func(*group) sortKey, cmp func(a, b cand) int) []cand {
	h := make([]cand, 0, k)
	down := func(i int) {
		for {
			l, w := 2*i+1, i
			if l < len(h) && cmp(h[l], h[w]) > 0 {
				w = l
			}
			if r := l + 1; r < len(h) && cmp(h[r], h[w]) > 0 {
				w = r
			}
			if w == i {
				return
			}
			h[i], h[w] = h[w], h[i]
			i = w
		}
	}
	for g := range groups {
		c := cand{g, keyOf(g)}
		if len(h) < k {
			c.g = g.clone()
			h = append(h, c)
			for i := len(h) - 1; i > 0 && cmp(h[i], h[(i-1)/2]) > 0; i = (i - 1) / 2 {
				h[i], h[(i-1)/2] = h[(i-1)/2], h[i]
			}
		} else if cmp(c, h[0]) < 0 {
			c.g = g.clone()
			h[0] = c
			down(0)
		}
	}
	return h
}

func cmpAny(a, b any) int {
	switch x := a.(type) {
	case int64:
		if y, ok := b.(int64); ok {
			return cmpInt(x, y)
		}
	case float64:
		if y, ok := b.(float64); ok {
			return cmpFloat(x, y)
		}
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y)
		}
	}
	// NULL sorts after every value, as in PostgreSQL (NULLS LAST ascending,
	// FIRST descending)
	if a == nil && b != nil {
		return 1
	}
	if b == nil && a != nil {
		return -1
	}
	return 0
}

// RowAggregator evaluates an AggQuery row by row. It is the execution path
// used when a query must run against the authoritative core (no derived
// view, or the view is too stale), and the oracle for testing the columnar
// engine.
type RowAggregator struct {
	q       *AggQuery
	t       *schema.Table
	filters []cfilter
	specs   []aggSpec
	gcols   []int
	groups  map[string]*group
	scanned int64
	kb      []byte
	truncs  []truncUnit // by group column; bucket nil: none
}

func NewRowAggregator(t *schema.Table, q *AggQuery) (*RowAggregator, error) {
	if hasDistinct(q.Aggs) {
		if _, err := distinctCol(t, q); err != nil {
			return nil, err
		}
	}
	fs, err := compileFilters(t, q.Filters)
	if err != nil {
		return nil, err
	}
	specs, err := compileAggs(t, q.Aggs)
	if err != nil {
		return nil, err
	}
	for i := range specs {
		specs[i].nullEmpty = q.SQLNulls
	}
	ra := &RowAggregator{q: q, t: t, filters: fs, specs: specs, groups: map[string]*group{}}
	for _, g := range q.GroupBy {
		ci, ok := t.Col(g)
		if !ok {
			return nil, fmt.Errorf("unknown group column %q", g)
		}
		if t.Columns[ci].T == schema.TFloat {
			return nil, fmt.Errorf("cannot group by %s column %q", t.Columns[ci].Type, g)
		}
		ra.gcols = append(ra.gcols, ci)
	}
	ra.truncs = make([]truncUnit, len(ra.gcols))
	for i, unit := range q.GroupTrunc {
		if unit == "" {
			continue
		}
		if i >= len(ra.gcols) || !t.Columns[ra.gcols[i]].T.IsTime() {
			return nil, fmt.Errorf("date_trunc(%s) needs a timestamp group column", unit)
		}
		u, err := lookupUnitFor(unit, t.Columns[ra.gcols[i]].T)
		if err != nil {
			return nil, err
		}
		ra.truncs[i] = u
	}
	return ra, nil
}

func (ra *RowAggregator) Add(r schema.Row) {
	ra.scanned++
	for i := range ra.filters {
		if !ra.filters[i].matchRow(r) {
			return
		}
	}
	ra.kb = ra.kb[:0]
	gv := func(i int) schema.Value {
		v := r[ra.gcols[i]]
		if u := ra.truncs[i]; u.bucket != nil && !v.Null {
			v.I = u.start(u.bucket(v.I))
		}
		return v
	}
	for i, c := range ra.gcols {
		v := gv(i)
		if v.Null {
			ra.kb = append(ra.kb, 0)
		} else if ct := ra.t.Columns[c].T.Store(); ct == schema.TString || ct == schema.TText {
			ra.kb = append(ra.kb, 1)
			ra.kb = append(ra.kb, v.S...)
		} else {
			ra.kb = append(ra.kb, 2)
			ra.kb = strconv.AppendInt(ra.kb, v.I, 10)
		}
		ra.kb = append(ra.kb, 0xff)
	}
	g := ra.groups[string(ra.kb)]
	if g == nil {
		g = &group{accs: newAccs(len(ra.specs)), sets: make([]map[schema.Value]struct{}, len(ra.specs))}
		for i, c := range ra.gcols {
			g.vals = append(g.vals, jsonValue(ra.t.Columns[c].T.Store(), gv(i)))
		}
		ra.groups[string(ra.kb)] = g
	}
	for i, s := range ra.specs {
		if s.col < 0 {
			g.accs[i].n++
			continue
		}
		v := r[s.col]
		if v.Null {
			continue
		}
		if ra.q.Aggs[i].Distinct {
			if g.sets[i] == nil {
				g.sets[i] = map[schema.Value]struct{}{}
			}
			g.sets[i][valueKey(ra.t.Columns[s.col].T.Store(), v)] = struct{}{}
			g.accs[i].n = int64(len(g.sets[i]))
			continue
		}
		if s.isInt {
			g.accs[i].addInt(v.I)
		} else {
			g.accs[i].addFloat(v.F)
		}
	}
}

func (ra *RowAggregator) Result() *Result {
	return finish(ra.q.GroupBy, ra.specs, maps.Values(ra.groups), len(ra.groups), ra.q, ra.scanned)
}

// valueKey is v with only the field its type uses, for set lookups.
func valueKey(t schema.ColType, v schema.Value) schema.Value {
	switch t {
	case schema.TFloat:
		return schema.Value{F: v.F}
	case schema.TString, schema.TText:
		return schema.Value{S: v.S}
	}
	return schema.Value{I: v.I}
}

func jsonValue(t schema.ColType, v schema.Value) any {
	if v.Null {
		return nil
	}
	switch t {
	case schema.TInt, schema.TTime, schema.TBool:
		return v.I
	case schema.TFloat:
		return v.F
	}
	return v.S
}
