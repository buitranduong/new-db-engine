package proj

// The R2 operator over segment columns (docs/30 4.2, docs/31 6): filter on
// dictionary-coded and integer columns, bounded heap per group, a
// deterministic score (score column, then md5(key || suffix) computed only for
// rows that tie on the score and compete for a place, then key), and rank
// ranges (quota pages). The base segment is scanned without the rows the
// Changed bitmap hides; the tail contributes the newest version of the rows
// that changed; so the answer is the one a scan of the table at the state's
// LSN would give.

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"stask/internal/schema"
)

var (
	// ErrSegNullScore: a row that passes the filters has a NULL score; the
	// SQL order of NULLs is not modelled here, the caller falls back.
	ErrSegNullScore = errors.New("a candidate row has a NULL in the score column")
	// ErrSegBudget: the rank range asks for more rows per group than the budget.
	ErrSegBudget = errors.New("rank range exceeds the top-N budget")
	// ErrSegInterrupted: the statement was cancelled.
	ErrSegInterrupted = errors.New("interrupted")
)

// SegMaxTopN is the most rows kept per group (the largest rank asked for).
var SegMaxTopN = 100000

// SegFilter is one conjunct on a held column.
type SegFilter struct {
	Col  int
	Text *SegTextPred
	Int  *SegIntPred
}

// SegTextPred: value IN Vals (NOT IN with Neg); NULL never passes.
type SegTextPred struct {
	Vals []string
	Neg  bool
}

// SegIntPred: Lo <= v <= Hi, v IN In (when HasIn), v NOT IN NotIn; NULL never passes.
type SegIntPred struct {
	Lo, Hi int64
	In     []int64
	HasIn  bool
	NotIn  []int64
}

// Match applies the filter to a row value (used to re-check page rows read
// from the core).
func (f *SegFilter) Match(v schema.Value) bool {
	if v.Null {
		return false
	}
	if p := f.Text; p != nil {
		in := slices.Contains(p.Vals, v.S)
		return in != p.Neg
	}
	p := f.Int
	if v.I < p.Lo || v.I > p.Hi {
		return false
	}
	if p.HasIn && !slices.Contains(p.In, v.I) {
		return false
	}
	return !slices.Contains(p.NotIn, v.I)
}

type SegRange struct{ Lo, Hi int } // ranks, 1-based, inclusive
type SegGroup struct {
	Value  string
	Ranges []SegRange
}

// SegR2 is one top-N-per-group query on one table.
type SegR2 struct {
	Filters   []SegFilter
	Group     int
	Groups    []SegGroup
	Score     int
	Desc      bool
	Md5       bool
	Md5Suffix string
	// optional range on the primary key (bytewise), applied to the rows that pass the other filters
	KeyLo, KeyHi string
	HasLo, HasHi bool
	LoEx, HiEx   bool
	Workers      int
	Interrupted  func() bool
}

type SegHit struct {
	Key   string
	Group string
	Rank  int
	Score int32
}

// SegR2Stats are the operator's real counters (EXPLAIN ANALYZE).
type SegR2Stats struct {
	BaseRows, ChunksRead, ChunksPruned int
	BasePassed, BaseHidden             int64 // rows that passed the filters / of those hidden by the Changed bitmap
	TailRows                           int
	TailPassed, TailDead               int64
	ColBytes                           int64 // column bytes touched in base and tail
	Candidates                         int
	Md5Evals                           int64
	Workers                            int
	Nothing                            string
}

type cflt struct {
	col      int
	text     bool
	allow    []bool
	mask     uint64
	lo, hi   int32
	in       []int32
	hasIn    bool
	notIn    []int32
	byteSize int
}

func dictCode(names []string, s string) (int, bool) {
	for i := 1; i < len(names); i++ {
		if names[i] == s {
			return i, true
		}
	}
	return 0, false
}

func clampI32(v int64) (int32, bool) {
	if v < math.MinInt32+1 {
		return math.MinInt32 + 1, true
	}
	if v > math.MaxInt32 {
		return math.MaxInt32, true
	}
	return int32(v), true
}

// compile resolves the filters against the table's dictionaries. empty=true
// when no row can pass.
func (t *SegTbl) compile(q *SegR2) (fs []cflt, empty bool, err error) {
	held := func(c int) error {
		if c < 0 || c >= len(t.Kinds) || t.Kinds[c] == SegNone || t.Absent&(1<<uint(c)) != 0 {
			return fmt.Errorf("column %d is not held by the segment", c)
		}
		return nil
	}
	for _, f := range q.Filters {
		if err := held(f.Col); err != nil {
			return nil, false, err
		}
		c := cflt{col: f.Col}
		switch {
		case f.Text != nil:
			if t.Kinds[f.Col] != SegText {
				return nil, false, fmt.Errorf("column %s is not a text column", t.ColNames[f.Col])
			}
			names := t.Dict[f.Col]
			c.text, c.byteSize = true, 2
			c.allow = make([]bool, len(names))
			n := 0
			if f.Text.Neg {
				for i := 1; i < len(names); i++ {
					c.allow[i] = true
				}
				for _, v := range f.Text.Vals {
					if i, ok := dictCode(names, v); ok {
						c.allow[i] = false
					}
				}
			} else {
				for _, v := range f.Text.Vals {
					if i, ok := dictCode(names, v); ok {
						c.allow[i] = true
					}
				}
			}
			for i, a := range c.allow {
				if a {
					c.mask |= 1 << (uint(i) & 63)
					n++
				}
			}
			if n == 0 {
				empty = true
			}
		case f.Int != nil:
			if t.Kinds[f.Col] != SegInt {
				return nil, false, fmt.Errorf("column %s is not an integer column", t.ColNames[f.Col])
			}
			p := f.Int
			c.byteSize = 4
			if p.Lo > math.MaxInt32 || p.Hi < math.MinInt32+1 || p.Lo > p.Hi {
				empty = true
			}
			c.lo, _ = clampI32(p.Lo)
			c.hi, _ = clampI32(p.Hi)
			if p.HasIn {
				c.hasIn = true
				for _, v := range p.In {
					if v >= math.MinInt32+1 && v <= math.MaxInt32 {
						c.in = append(c.in, int32(v))
					}
				}
				if len(c.in) == 0 {
					empty = true
				}
			}
			for _, v := range p.NotIn {
				if v >= math.MinInt32+1 && v <= math.MaxInt32 {
					c.notIn = append(c.notIn, int32(v))
				}
			}
		default:
			return nil, false, fmt.Errorf("empty filter")
		}
		fs = append(fs, c)
	}
	// text filters first: they are the cheapest to apply on a dense scan
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].text && !fs[j].text })
	return fs, empty, nil
}

// filterRange appends to sel the positions in [lo,hi) that pass every filter.
func filterRange(fs []cflt, text [][]uint16, ints [][]int32, lo, hi int, sel []int32) []int32 {
	sel = sel[:0]
	if len(fs) == 0 {
		for i := lo; i < hi; i++ {
			sel = append(sel, int32(i))
		}
		return sel
	}
	f := &fs[0]
	if f.text {
		col, allow := text[f.col], f.allow
		for i := lo; i < hi; i++ {
			if c := int(col[i]); c < len(allow) && allow[c] {
				sel = append(sel, int32(i))
			}
		}
	} else {
		col := ints[f.col]
		for i := lo; i < hi; i++ {
			if intPass(f, col[i]) {
				sel = append(sel, int32(i))
			}
		}
	}
	for k := 1; k < len(fs) && len(sel) > 0; k++ {
		f = &fs[k]
		out := sel[:0]
		if f.text {
			col, allow := text[f.col], f.allow
			for _, i := range sel {
				if c := int(col[i]); c < len(allow) && allow[c] {
					out = append(out, i)
				}
			}
		} else {
			col := ints[f.col]
			for _, i := range sel {
				if intPass(f, col[i]) {
					out = append(out, i)
				}
			}
		}
		sel = out
	}
	return sel
}

func intPass(f *cflt, v int32) bool {
	if v == segNullInt || v < f.lo || v > f.hi {
		return false
	}
	if f.hasIn && !slices.Contains(f.in, v) {
		return false
	}
	if len(f.notIn) > 0 && slices.Contains(f.notIn, v) {
		return false
	}
	return true
}

// ---- heap of candidates --------------------------------------------------------------

type r2entry struct {
	sk   int64 // score key: smaller is better
	id   uint32
	g    uint8
	hh   bool
	hash [16]byte
}

type r2ctx struct {
	t        *SegTbl
	md5      bool
	suffix   string
	md5Evals *atomic.Int64
}

func (c *r2ctx) key(id uint32) []byte {
	if int(id) < c.t.Base.n {
		return c.t.Base.key(int(id))
	}
	return c.t.Tail.key(int(id) - c.t.Base.n)
}

func (c *r2ctx) hashOf(x *r2entry) *[16]byte {
	if !x.hh {
		k := c.key(x.id)
		b := make([]byte, 0, len(k)+len(c.suffix))
		b = append(append(b, k...), c.suffix...)
		x.hash, x.hh = md5.Sum(b), true
		c.md5Evals.Add(1)
	}
	return &x.hash
}

// worse: a sorts after b.
func (c *r2ctx) worse(a, b *r2entry) bool {
	if a.sk != b.sk {
		return a.sk > b.sk
	}
	if c.md5 {
		if d := bytes.Compare(c.hashOf(a)[:], c.hashOf(b)[:]); d != 0 {
			return d > 0
		}
	}
	return bytes.Compare(c.key(a.id), c.key(b.id)) > 0
}

type r2heap struct {
	c   *r2ctx
	cap int
	e   []r2entry
}

func (h *r2heap) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h.c.worse(&h.e[i], &h.e[p]) {
			return
		}
		h.e[i], h.e[p] = h.e[p], h.e[i]
		i = p
	}
}
func (h *r2heap) down(i int) {
	n := len(h.e)
	for {
		l, r, w := 2*i+1, 2*i+2, i
		if l < n && h.c.worse(&h.e[l], &h.e[w]) {
			w = l
		}
		if r < n && h.c.worse(&h.e[r], &h.e[w]) {
			w = r
		}
		if w == i {
			return
		}
		h.e[i], h.e[w] = h.e[w], h.e[i]
		i = w
	}
}

// offer: rows worse than a full heap's root on the leading key are rejected
// before any md5 is computed.
func (h *r2heap) offer(sk int64, id uint32) {
	if h.cap == 0 {
		return
	}
	if len(h.e) < h.cap {
		h.e = append(h.e, r2entry{sk: sk, id: id})
		h.up(len(h.e) - 1)
		return
	}
	if sk > h.e[0].sk {
		return
	}
	c := r2entry{sk: sk, id: id}
	if sk == h.e[0].sk && !h.c.worse(&h.e[0], &c) {
		return
	}
	h.e[0] = c
	h.down(0)
}

// TopNPerGroup runs the query on this immutable table state.
func (t *SegTbl) TopNPerGroup(q *SegR2) ([]SegHit, SegR2Stats, error) {
	var st SegR2Stats
	if q.Group < 0 || q.Group >= len(t.Kinds) || (t.Kinds[q.Group] != SegText && t.Kinds[q.Group] != SegInt) || t.Absent&(1<<uint(q.Group)) != 0 {
		return nil, st, fmt.Errorf("group column is not held as a dictionary or integer column")
	}
	intGroup := t.Kinds[q.Group] == SegInt
	if q.Score < 0 || q.Score >= len(t.Kinds) || t.Kinds[q.Score] != SegInt || t.Absent&(1<<uint(q.Score)) != 0 {
		return nil, st, fmt.Errorf("score column is not held as an integer column")
	}
	fs, empty, err := t.compile(q)
	if err != nil {
		return nil, st, err
	}
	// groups: dictionary code -> group index
	var gnames []string
	if !intGroup {
		gnames = t.Dict[q.Group]
	}
	// integer group column: gidx is indexed by value - gmin (values of the asked groups only; NULL never matches)
	var gmin int64
	if intGroup {
		gmax := int64(0)
		for i, g := range q.Groups {
			v, err := strconv.ParseInt(g.Value, 10, 32)
			if err != nil {
				return nil, st, fmt.Errorf("group value %q is not an integer", g.Value)
			}
			if i == 0 || v < gmin {
				gmin = v
			}
			if i == 0 || v > gmax {
				gmax = v
			}
		}
		if len(q.Groups) > 0 && gmax-gmin >= 1<<16 {
			return nil, st, fmt.Errorf("integer group values span more than 65536")
		}
		gnames = make([]string, 0)
		gidxLen := 0
		if len(q.Groups) > 0 {
			gidxLen = int(gmax-gmin) + 1
		}
		gnames = make([]string, gidxLen)
	}
	gidx := make([]int8, len(gnames))
	for i := range gidx {
		gidx[i] = -1
	}
	G := len(q.Groups)
	if G > 100 {
		return nil, st, fmt.Errorf("more than 100 groups")
	}
	caps := make([]int, G)
	for gi, g := range q.Groups {
		for _, r := range g.Ranges {
			caps[gi] = max(caps[gi], r.Hi)
		}
		if caps[gi] > SegMaxTopN {
			return nil, st, fmt.Errorf("%w: %d > %d", ErrSegBudget, caps[gi], SegMaxTopN)
		}
		if intGroup {
			v, _ := strconv.ParseInt(g.Value, 10, 32)
			gidx[v-gmin] = int8(gi)
		} else if code, ok := dictCode(gnames, g.Value); ok {
			gidx[code] = int8(gi)
		}
	}
	var md5Evals atomic.Int64
	rc := &r2ctx{t: t, md5: q.Md5, suffix: q.Md5Suffix, md5Evals: &md5Evals}
	keyOK := func(id uint32) bool {
		if !q.HasLo && !q.HasHi {
			return true
		}
		k := rc.key(id)
		if q.HasLo {
			if c := bytes.Compare(k, []byte(q.KeyLo)); c < 0 || (c == 0 && q.LoEx) {
				return false
			}
		}
		if q.HasHi {
			if c := bytes.Compare(k, []byte(q.KeyHi)); c > 0 || (c == 0 && q.HiEx) {
				return false
			}
		}
		return true
	}
	b := t.Base
	nz := 0
	if b.n > 0 {
		nz = (b.n + SegChunkRows - 1) / SegChunkRows
	}
	scoreSign := int64(1)
	if q.Desc {
		scoreSign = -1
	}
	// bytes per row read: filter columns + group + score
	rowBytes := 2 + 4
	for _, f := range fs {
		rowBytes += f.byteSize
	}
	st.BaseRows, st.TailRows = b.n, t.Tail.n
	workers := max(1, q.Workers)
	if empty || G == 0 {
		st.Nothing = "no row can pass the filters"
		return nil, st, nil
	}
	var next atomic.Int64
	var mu sync.Mutex
	var all [][]*r2heap
	var nullScore atomic.Bool
	var interrupted atomic.Bool
	var wg sync.WaitGroup
	nw := min(workers, max(1, nz))
	st.Workers = nw
	for w := 0; w < nw; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hs := make([]*r2heap, G)
			for g := range hs {
				hs[g] = &r2heap{c: rc, cap: caps[g]}
			}
			var read, pruned int
			var passed, hidden int64
			var bytesTouched int64
			sel := make([]int32, 0, SegChunkRows)
			scoreCol := b.ints[q.Score]
			var groupCol []uint16
			var groupInt []int32
			if intGroup {
				groupInt = b.ints[q.Group]
			} else {
				groupCol = b.text[q.Group]
			}
			for {
				z := int(next.Add(1) - 1)
				if z >= nz {
					break
				}
				if q.Interrupted != nil && q.Interrupted() {
					interrupted.Store(true)
					break
				}
				skip := false
				for i := range fs {
					f := &fs[i]
					if f.text {
						if b.ztext[f.col][z]&f.mask == 0 {
							skip = true
							break
						}
					} else if b.zmax[f.col][z] < f.lo || b.zmin[f.col][z] > f.hi {
						skip = true
						break
					}
				}
				if skip {
					pruned++
					continue
				}
				read++
				lo, hi := z*SegChunkRows, min(b.n, (z+1)*SegChunkRows)
				bytesTouched += int64(hi-lo) * int64(rowBytes)
				sel = filterRange(fs, b.text, b.ints, lo, hi, sel)
				for _, i := range sel {
					passed++
					if t.Changed.Get(int(i)) {
						hidden++
						continue
					}
					var gi int8
					if intGroup {
						gi = gIntIdx(gidx, groupInt[i], gmin)
					} else {
						gi = gidx[groupCol[i]]
					}
					if gi < 0 || !keyOK(uint32(i)) {
						continue
					}
					sc := scoreCol[i]
					if sc == segNullInt {
						nullScore.Store(true)
						continue
					}
					hs[gi].offer(scoreSign*int64(sc), uint32(i))
				}
			}
			mu.Lock()
			all = append(all, hs)
			st.ChunksRead += read
			st.ChunksPruned += pruned
			st.BasePassed += passed
			st.BaseHidden += hidden
			st.ColBytes += bytesTouched
			mu.Unlock()
		}()
	}
	wg.Wait()
	if interrupted.Load() {
		return nil, st, ErrSegInterrupted
	}
	// tail: single thread, small by construction (merged in the background)
	tl := &t.Tail
	if tl.n > 0 {
		th := make([]*r2heap, G)
		for g := range th {
			th[g] = &r2heap{c: rc, cap: caps[g]}
		}
		sel := filterRange(fs, tl.text, tl.ints, 0, tl.n, make([]int32, 0, min(tl.n, SegChunkRows)))
		st.ColBytes += int64(tl.n) * int64(rowBytes)
		scoreCol := tl.ints[q.Score]
		var groupCol []uint16
		var groupInt []int32
		if intGroup {
			groupInt = tl.ints[q.Group]
		} else {
			groupCol = tl.text[q.Group]
		}
		for _, i := range sel {
			if tl.tomb[i] || t.TailDead.Get(int(i)) {
				st.TailDead++
				continue
			}
			st.TailPassed++
			var gi int8
			if intGroup {
				gi = gIntIdx(gidx, groupInt[i], gmin)
			} else {
				gi = gidx[groupCol[i]]
			}
			if gi < 0 || !keyOK(uint32(b.n+int(i))) {
				continue
			}
			sc := scoreCol[i]
			if sc == segNullInt {
				nullScore.Store(true)
				continue
			}
			th[gi].offer(scoreSign*int64(sc), uint32(b.n+int(i)))
		}
		all = append(all, th)
	}
	if nullScore.Load() {
		return nil, st, ErrSegNullScore
	}
	// merge the candidates of each group, rank them, keep the asked ranges
	var out []r2entry
	var ranks []int
	for g := 0; g < G; g++ {
		var cand []r2entry
		for _, hs := range all {
			cand = append(cand, hs[g].e...)
		}
		st.Candidates += len(cand)
		for i := range cand {
			cand[i].g = uint8(g)
		}
		sortEntries(rc, cand)
		for i := range cand {
			r := i + 1
			for _, rg := range q.Groups[g].Ranges {
				if r >= rg.Lo && r <= rg.Hi {
					out = append(out, cand[i])
					ranks = append(ranks, r)
					break
				}
			}
		}
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(i, j int) int { return cmpEntries(rc, &out[i], &out[j]) })
	hits := make([]SegHit, 0, len(out))
	for _, i := range idx {
		e := &out[i]
		var sc int32
		if int(e.id) < b.n {
			sc = b.ints[q.Score][e.id]
		} else {
			sc = tl.ints[q.Score][int(e.id)-b.n]
		}
		hits = append(hits, SegHit{Key: string(rc.key(e.id)), Group: q.Groups[e.g].Value, Rank: ranks[i], Score: sc})
	}
	st.Md5Evals = md5Evals.Load()
	return hits, st, nil
}

func cmpEntries(c *r2ctx, a, b *r2entry) int {
	switch {
	case c.worse(a, b):
		return 1
	case c.worse(b, a):
		return -1
	}
	return 0
}

func sortEntries(c *r2ctx, e []r2entry) {
	slices.SortFunc(e, func(a, b r2entry) int { return cmpEntries(c, &a, &b) })
}

// gIntIdx maps an integer group value to its group index (-1: not asked, or NULL).
func gIntIdx(gidx []int8, v int32, gmin int64) int8 {
	if v == segNullInt {
		return -1
	}
	d := int64(v) - gmin
	if d < 0 || d >= int64(len(gidx)) {
		return -1
	}
	return gidx[d]
}
