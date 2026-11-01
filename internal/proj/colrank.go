package proj

// Rank-ordered access path for the canonical R2 (docs/34 H2, integration h2int).
//
// The rank index is a sidecar of an immutable base segment: the base positions
// sorted by (partition value, score in the query's direction, md5(key || seed),
// key), i.e. exactly the order the R2 operator ranks by, plus lazily built
// per-column bitmaps in that order. A query then walks each partition's list
// from the top, 64 rows per step through the filter bitmaps, and stops after
// `need` qualifying rows (the largest rank asked for), instead of scanning the
// whole base. The rows the Changed bitmap hides are skipped during the walk;
// the tail (rows changed since the base) is filtered by the existing operator
// code into per-partition heaps and merged linearly with the walk.
//
// The index is DERIVED, in memory only, rebuilt on demand and carried over to
// the next base by the merge: no on-disk byte is touched (docs/PROCESS D33).
// A base without an index (not built yet, declined, merge in progress) simply
// uses the scan operator: the order tag is optional.

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"math/bits"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
)

// RankSeedCanonical is the one tie-break seed for which the rank order is
// valid: md5(number || '42'). The planner recognises it; every other seed or
// ORDER BY shape uses the scan operator.
const RankSeedCanonical = "42"

const (
	rankVersion   = 1 // layout version of the sidecar (in memory; the tag of the order)
	rankMaxPairs  = 1 << 16
	rankMaxCodes  = 32 // largest dictionary a text column may have to get exact bitmaps
	rankIntBucket = 16 // price-style buckets of the range-encoded integer bitmaps
)

// SegRankTailMax is the largest tail the rank path merges per query; above it
// the scan operator is used (the reason is in EXPLAIN).
var SegRankTailMax = 1 << 17

// SegRankSync builds the index inside the first query that needs it (tests);
// by default the build runs in the background and queries use the scan
// operator until it is ready.
var SegRankSync = false

type rankSpec struct {
	group, score int
	desc         bool
	suffix       string
}

type rankSeg struct{ lo, hi int } // rank positions [lo,hi) of one partition value

type rankInt struct {
	min   int32
	width int32
	max   int32
	rb    [][]uint64 // rb[k]: bit set when the value bucket is <= k (k = 0..rankIntBucket-2)
}

type rankIdx struct {
	ver    int
	spec   rankSpec
	n      int
	ord    []uint32           // rank position -> base position
	gseg   map[int32]rankSeg  // partition key (dictionary code or integer value) -> range
	mu     sync.Mutex         // guards the lazily built bitmaps
	tbm    map[int][][]uint64 // text column -> bitmap per code (nil entry: column not eligible)
	ibm    map[int]*rankInt
	bmByte atomic.Int64
}

func (ix *rankIdx) bytes() int64 { return int64(len(ix.ord))*4 + ix.bmByte.Load() }

type rankEntry struct {
	ix       atomic.Pointer[rankIdx]
	building atomic.Bool
	why      atomic.Pointer[string] // build declined
}

// rankSet is the optional order tag of a base: one entry per rank spec.
type rankSet struct {
	mu sync.Mutex
	m  map[rankSpec]*rankEntry
}

func (rs *rankSet) entry(sp rankSpec) *rankEntry {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.m == nil {
		rs.m = map[rankSpec]*rankEntry{}
	}
	e := rs.m[sp]
	if e == nil {
		e = &rankEntry{}
		rs.m[sp] = e
	}
	return e
}

// specs lists the specs that have a ready index, with the bitmap columns they hold.
func (rs *rankSet) ready() (sp []rankSpec, ix []*rankIdx) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for k, e := range rs.m {
		if i := e.ix.Load(); i != nil {
			sp, ix = append(sp, k), append(ix, i)
		}
	}
	return
}

// RankBytes is the resident size of the rank sidecars of this base.
func (b *SegBase) RankBytes() int64 {
	var n int64
	_, ixs := b.rank.ready()
	for _, ix := range ixs {
		n += ix.bytes()
	}
	return n
}

func rankHash32(key []byte, suffix string, buf *[]byte) uint32 {
	*buf = append(append((*buf)[:0], key...), suffix...)
	s := md5.Sum(*buf)
	return binary.BigEndian.Uint32(s[:4])
}

// buildRank sorts the base positions into rank order. why != "" declines.
func buildRank(b *SegBase, sp rankSpec, workers int) (*rankIdx, string) {
	n := b.n
	if sp.group < 0 || sp.group >= len(b.text) || sp.score < 0 || sp.score >= len(b.ints) || b.ints[sp.score] == nil || (b.text[sp.group] == nil && b.ints[sp.group] == nil) {
		return nil, "partition or score column not held by the base"
	}
	if int64(n) >= 1<<32-1 {
		return nil, "base too large for 32-bit positions"
	}
	gint := b.ints[sp.group] != nil
	gk := func(i int) int32 {
		if gint {
			return b.ints[sp.group][i]
		}
		return int32(b.text[sp.group][i])
	}
	sc := b.ints[sp.score]
	// distinct (partition, score) pairs, in rank order
	type pair struct{ g, s int32 }
	idx := map[pair]int{}
	var pairs []pair
	var last pair
	lastOK := false
	for i := 0; i < n; i++ {
		if sc[i] == segNullInt {
			return nil, "the score column has NULL values in the base"
		}
		p := pair{gk(i), sc[i]}
		if lastOK && p == last {
			continue
		}
		last, lastOK = p, true
		if _, ok := idx[p]; !ok {
			idx[p] = 0
			pairs = append(pairs, p)
			if len(pairs) > rankMaxPairs {
				return nil, fmt.Sprintf("more than %d distinct (partition, score) pairs", rankMaxPairs)
			}
		}
	}
	slices.SortFunc(pairs, func(a, c pair) int {
		if a.g != c.g {
			if a.g < c.g {
				return -1
			}
			return 1
		}
		if a.s == c.s {
			return 0
		}
		if (a.s > c.s) == sp.desc {
			return -1
		}
		return 1
	})
	for i, p := range pairs {
		idx[p] = i
	}
	nb := len(pairs)
	// hash32 of every row
	if workers < 1 {
		workers = 1
	}
	hh := make([]uint32, n)
	parRange(n, workers, func(lo, hi int) {
		var buf []byte
		for i := lo; i < hi; i++ {
			hh[i] = rankHash32(b.key(i), sp.suffix, &buf)
		}
	})
	// counting sort into buckets
	start := make([]int, nb+1)
	bk := make([]uint16, n)
	last, lastOK = pair{}, false
	lb := 0
	for i := 0; i < n; i++ {
		p := pair{gk(i), sc[i]}
		if !lastOK || p != last {
			last, lastOK, lb = p, true, idx[p]
		}
		bk[i] = uint16(lb)
		start[lb+1]++
	}
	for i := 1; i <= nb; i++ {
		start[i] += start[i-1]
	}
	pk := make([]uint64, n)
	fill := slices.Clone(start[:nb])
	for i := 0; i < n; i++ {
		pk[fill[bk[i]]] = uint64(hh[i])<<32 | uint64(i)
		fill[bk[i]]++
	}
	hh, bk = nil, nil
	// sort every bucket by (h32, position); equal h32 runs are ordered by the full md5, then the key
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var b1, b2 []byte
			for {
				k := int(next.Add(1) - 1)
				if k >= nb {
					return
				}
				s := pk[start[k]:start[k+1]]
				slices.Sort(s)
				for i := 0; i < len(s); {
					j := i + 1
					for j < len(s) && s[j]>>32 == s[i]>>32 {
						j++
					}
					if j-i > 1 {
						slices.SortFunc(s[i:j], func(x, y uint64) int {
							kx, ky := b.key(int(uint32(x))), b.key(int(uint32(y)))
							b1 = append(append(b1[:0], kx...), sp.suffix...)
							b2 = append(append(b2[:0], ky...), sp.suffix...)
							hx, hy := md5.Sum(b1), md5.Sum(b2)
							if c := bytes.Compare(hx[:], hy[:]); c != 0 {
								return c
							}
							return bytes.Compare(kx, ky)
						})
					}
					i = j
				}
			}
		}()
	}
	wg.Wait()
	ix := &rankIdx{ver: rankVersion, spec: sp, n: n, ord: make([]uint32, n), gseg: map[int32]rankSeg{},
		tbm: map[int][][]uint64{}, ibm: map[int]*rankInt{}}
	parRange(n, workers, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			ix.ord[i] = uint32(pk[i])
		}
	})
	for k, p := range pairs {
		sg := ix.gseg[p.g]
		if sg.hi == sg.lo && sg.hi == 0 {
			sg.lo = start[k]
		}
		sg.hi = start[k+1]
		ix.gseg[p.g] = sg
	}
	return ix, ""
}

func parRange(n, workers int, f func(lo, hi int)) {
	if workers <= 1 || n < 4096 {
		f(0, n)
		return
	}
	step := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += step {
		hi := min(n, lo+step)
		wg.Add(1)
		go func() { defer wg.Done(); f(lo, hi) }()
	}
	wg.Wait()
}

func rankWorkers() int { return max(1, min(4, runtime.GOMAXPROCS(0)/2)) }

// text returns the exact bitmap per dictionary code of a text column in rank order, or nil when the
// column has more than rankMaxCodes codes.
func (ix *rankIdx) text(b *SegBase, col int) [][]uint64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if v, ok := ix.tbm[col]; ok {
		return v
	}
	c := b.text[col]
	mx := 0
	for _, v := range c {
		if int(v) > mx {
			mx = int(v)
		}
	}
	if mx >= rankMaxCodes {
		ix.tbm[col] = nil
		return nil
	}
	nw := (ix.n + 63) / 64
	bm := make([][]uint64, mx+1)
	for k := range bm {
		bm[k] = make([]uint64, nw)
	}
	parRange(nw, rankWorkers(), func(lo, hi int) {
		for w := lo; w < hi; w++ {
			for i := w * 64; i < min(ix.n, w*64+64); i++ {
				bm[c[ix.ord[i]]][w] |= 1 << (i & 63)
			}
		}
	})
	ix.bmByte.Add(int64(len(bm)) * int64(nw) * 8)
	ix.tbm[col] = bm
	return bm
}

// intBM returns the range-encoded bucket bitmaps of an integer column (a superset filter: the walk
// verifies the exact predicate on the column), or nil when the column holds no value.
func (ix *rankIdx) intBM(b *SegBase, col int) *rankInt {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if v, ok := ix.ibm[col]; ok {
		return v
	}
	c := b.ints[col]
	mn, mx := int32(1<<30), int32(-1<<31+1)
	any := false
	for _, v := range c {
		if v == segNullInt {
			continue
		}
		any = true
		mn, mx = min(mn, v), max(mx, v)
	}
	if !any {
		ix.ibm[col] = nil
		return nil
	}
	ri := &rankInt{min: mn, max: mx, width: int32((int64(mx)-int64(mn))/rankIntBucket + 1)}
	nw := (ix.n + 63) / 64
	ri.rb = make([][]uint64, rankIntBucket-1)
	for k := range ri.rb {
		ri.rb[k] = make([]uint64, nw)
	}
	parRange(nw, rankWorkers(), func(lo, hi int) {
		for w := lo; w < hi; w++ {
			for i := w * 64; i < min(ix.n, w*64+64); i++ {
				v := c[ix.ord[i]]
				bkt := 0 // NULL is never matched: the exact check rejects it
				if v != segNullInt {
					bkt = int((int64(v) - int64(mn)) / int64(ri.width))
				}
				for k := bkt; k < rankIntBucket-1; k++ {
					ri.rb[k][w] |= 1 << (i & 63)
				}
			}
		}
	})
	ix.bmByte.Add(int64(len(ri.rb)) * int64(nw) * 8)
	ix.ibm[col] = ri
	return ri
}

// word returns the bucket-range superset word w for values in [lo,hi].
func (ri *rankInt) word(bl, bh, w int) uint64 {
	rw := func(k int) uint64 {
		if k < 0 {
			return 0
		}
		if k >= rankIntBucket-1 {
			return ^uint64(0)
		}
		return ri.rb[k][w]
	}
	return rw(bh) &^ rw(bl-1)
}

// rankGet returns the ready index for sp, or "" and a reason. Without SegRankSync the build is started
// in the background and the reason says so.
func (b *SegBase) rankGet(sp rankSpec) (*rankIdx, string) {
	e := b.rank.entry(sp)
	if ix := e.ix.Load(); ix != nil {
		return ix, ""
	}
	if w := e.why.Load(); w != nil {
		return nil, "rank index declined: " + *w
	}
	if SegRankSync {
		b.rankBuild(e, sp)
		if ix := e.ix.Load(); ix != nil {
			return ix, ""
		}
		if w := e.why.Load(); w != nil {
			return nil, "rank index declined: " + *w
		}
	}
	if e.building.CompareAndSwap(false, true) {
		go b.rankBuild(e, sp)
	}
	return nil, "rank index for this base is being built in the background (the scan operator answers meanwhile)"
}

func (b *SegBase) rankBuild(e *rankEntry, sp rankSpec) {
	e.building.Store(true)
	ix, why := buildRank(b, sp, rankWorkers())
	if why != "" {
		e.why.Store(&why)
	} else {
		e.ix.Store(ix)
	}
	e.building.Store(false)
}

// rankCarry builds on the new base the indexes (and bitmap columns) the old base had, inside the merge
// goroutine, so the swap does not leave the rank path cold.
func rankCarry(old, nb *SegBase) {
	sps, ixs := old.rank.ready()
	for k, sp := range sps {
		e := nb.rank.entry(sp)
		ix, why := buildRank(nb, sp, rankWorkers())
		if why != "" {
			e.why.Store(&why)
			continue
		}
		oix := ixs[k]
		oix.mu.Lock()
		var tc, ic []int
		for c, v := range oix.tbm {
			if v != nil {
				tc = append(tc, c)
			}
		}
		for c, v := range oix.ibm {
			if v != nil {
				ic = append(ic, c)
			}
		}
		oix.mu.Unlock()
		for _, c := range tc {
			if nb.text[c] != nil {
				ix.text(nb, c)
			}
		}
		for _, c := range ic {
			if nb.ints[c] != nil {
				ix.intBM(nb, c)
			}
		}
		e.ix.Store(ix)
	}
}

// ---- the walk ----------------------------------------------------------------------------------

type rkFilter struct {
	f     *cflt
	words [][]uint64 // exact text: bitmaps of the allowed codes (nil: verify on the column)
	exact bool       // words decide the predicate (no column check needed)
	ri    *rankInt   // integer superset
	bl    int
	bh    int
	none  bool // no base row can pass
}

func (ix *rankIdx) prepare(b *SegBase, fs []cflt) []rkFilter {
	out := make([]rkFilter, len(fs))
	for i := range fs {
		f := &fs[i]
		r := &out[i]
		r.f = f
		if f.text {
			bm := ix.text(b, f.col)
			if bm == nil {
				continue
			}
			for c, ok := range f.allow {
				if ok && c < len(bm) {
					r.words = append(r.words, bm[c])
				}
			}
			r.exact = true
			if len(r.words) == 0 {
				r.none = true
			}
			continue
		}
		ri := ix.intBM(b, f.col)
		if ri == nil {
			r.none = true
			continue
		}
		lo, hi := max(f.lo, ri.min), min(f.hi, ri.max)
		if lo > hi {
			r.none = true
			continue
		}
		r.ri = ri
		r.bl = int((int64(lo) - int64(ri.min)) / int64(ri.width))
		r.bh = int((int64(hi) - int64(ri.min)) / int64(ri.width))
	}
	return out
}

type rankWalkStats struct {
	words, passed, hidden, verified int64
}

// walk returns the first `need` base positions of the segment [s,e) that pass every filter and are not hidden,
// in rank order.
func (t *SegTbl) rankWalk(ix *rankIdx, rf []rkFilter, s, e, need int, interrupted func() bool, st *rankWalkStats) ([]uint32, bool) {
	b := t.Base
	var out []uint32
	if s >= e || need <= 0 {
		return nil, true
	}
	for i := range rf {
		if rf[i].none {
			return nil, true
		}
	}
	w0, w1 := s>>6, (e-1)>>6
	for w := w0; w <= w1; w++ {
		if interrupted != nil && w&1023 == 0 && interrupted() {
			return nil, false
		}
		m := ^uint64(0)
		if w == w0 {
			m &= ^uint64(0) << (s & 63)
		}
		if w == w1 && e&63 != 0 {
			m &= (uint64(1) << (e & 63)) - 1
		}
		for i := range rf {
			r := &rf[i]
			switch {
			case r.words != nil:
				var u uint64
				for _, bm := range r.words {
					u |= bm[w]
				}
				m &= u
			case r.ri != nil:
				m &= r.ri.word(r.bl, r.bh, w)
			}
			if m == 0 {
				break
			}
		}
		st.words++
		for x := m; x != 0; x &= x - 1 {
			j := bits.TrailingZeros64(x)
			p := ix.ord[w*64+j]
			ok := true
			for i := range rf {
				r := &rf[i]
				if r.exact {
					continue
				}
				if r.f.text {
					if c := int(b.text[r.f.col][p]); !(c < len(r.f.allow) && r.f.allow[c]) {
						ok = false
						break
					}
				} else if !intPass(r.f, b.ints[r.f.col][p]) {
					ok = false
					break
				}
			}
			st.verified++
			if !ok {
				continue
			}
			st.passed++
			if t.Changed.Get(int(p)) {
				st.hidden++
				continue
			}
			out = append(out, p)
			if len(out) == need {
				return out, true
			}
		}
	}
	return out, true
}

// topRank answers the query from the rank index. why != "" means the path is not usable (nothing was
// done; the scan operator runs); with why == "" the hits (or err) are the answer.
func (t *SegTbl) topRank(q *SegR2, fs []cflt, rc *r2ctx, caps []int, gidx []int8, gmin int64, intGroup bool, scoreSign int64,
	keyOK func(uint32) bool, rowBytes int, st *SegR2Stats) ([]SegHit, string, error) {
	switch {
	case !q.Md5:
		return nil, "the ORDER BY has no md5(key || seed) tie-break (the rank order is defined with it)", nil
	case q.Md5Suffix != RankSeedCanonical:
		return nil, fmt.Sprintf("tie-break seed %q is not the canonical seed %q (the rank order is valid only for it)", q.Md5Suffix, RankSeedCanonical), nil
	case q.HasLo || q.HasHi:
		return nil, "a key range is part of the statement (keyset pagination uses the scan operator)", nil
	case t.Tail.n > SegRankTailMax:
		return nil, fmt.Sprintf("the tail has %d rows, more than the rank path merges per query (%d); the merge into a new base is pending", t.Tail.n, SegRankTailMax), nil
	}
	b := t.Base
	sp := rankSpec{group: q.Group, score: q.Score, desc: q.Desc, suffix: q.Md5Suffix}
	ix, why := b.rankGet(sp)
	if why != "" {
		return nil, why, nil
	}
	G := len(q.Groups)
	rf := ix.prepare(b, fs)
	var ws rankWalkStats
	base := make([][]r2entry, G)
	for g, gr := range q.Groups {
		var sg rankSeg
		if intGroup {
			v, _ := strconv.ParseInt(gr.Value, 10, 32)
			sg = ix.gseg[int32(v)]
		} else if code, ok := dictCode(t.Dict[q.Group], gr.Value); ok {
			sg = ix.gseg[int32(code)]
		}
		ids, ok := t.rankWalk(ix, rf, sg.lo, sg.hi, caps[g], q.Interrupted, &ws)
		if !ok {
			return nil, "", ErrSegInterrupted
		}
		es := make([]r2entry, len(ids))
		for i, p := range ids {
			es[i] = r2entry{sk: scoreSign * int64(b.ints[q.Score][p]), id: p, g: uint8(g)}
		}
		base[g] = es
	}
	th, nullScore := t.tailHeaps(q, fs, rc, caps, gidx, gmin, intGroup, scoreSign, keyOK, rowBytes, st)
	if nullScore {
		return nil, "", ErrSegNullScore
	}
	cands := make([][]r2entry, G)
	for g := range cands {
		cands[g] = base[g]
		if th != nil && len(th[g].e) > 0 {
			te := th[g].e
			for i := range te {
				te[i].g = uint8(g)
			}
			sortEntries(rc, te)
			cands[g] = mergeRank(rc, base[g], te, caps[g])
		}
		st.Candidates += len(cands[g])
	}
	st.RankUsed, st.Workers = true, 1
	st.RankWords, st.RankVerified, st.RankBytes = ws.words, ws.verified, b.RankBytes()
	st.BasePassed, st.BaseHidden = ws.passed, ws.hidden
	st.ColBytes += ws.words*int64(8*len(fs)) + ws.verified*int64(rowBytes)
	return t.emit(q, rc, cands), "", nil
}

// mergeRank merges two lists, each sorted best first, and keeps the first n.
func mergeRank(rc *r2ctx, a, b []r2entry, n int) []r2entry {
	out := make([]r2entry, 0, min(n, len(a)+len(b)))
	i, j := 0, 0
	for len(out) < n && (i < len(a) || j < len(b)) {
		if j >= len(b) || (i < len(a) && !rc.worse(&a[i], &b[j])) {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return out
}
