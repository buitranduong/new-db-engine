package main

// rank-major base (cfg2) + bitmaps (cfg3), tail, walk, fallback.

import (
	"crypto/md5"
	"encoding/binary"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type rmBase struct {
	n     int
	g     []uint32
	cat   []uint8
	rate  []uint8
	price []int32
	h32   []uint32 // the stable rank column: first 4 bytes of md5(number||'42')
	seg   [4]int   // carrier t covers [seg[t], seg[t+1])
	// cfg3 (nil when not built)
	catB     [5][]uint64
	rb       [][]uint64 // rb[k]: bit set when price bucket <= k (k = 0..NB-2)
	pmin, pw int32
	pmax     int32
}

type BuildStats struct{ Gen, Hash, Sort, Mat, Bitmaps time.Duration }

// buildRank sorts the rows by (tel, rate DESC, h32, full md5, number). Input must have c.g[i] == i.
func buildRank(c *cols, workers int, st *BuildStats) *rmBase {
	n := c.n
	t0 := time.Now()
	hh := make([]uint32, n)
	parFor(n, workers, func(lo, hi int) {
		var buf [13]byte
		buf[11], buf[12] = '4', '2'
		for i := lo; i < hi; i++ {
			k := numStr(c.g[i])
			copy(buf[:], k[:])
			s := md5sum13(buf[:])
			hh[i] = s
		}
	})
	t1 := time.Now()
	// counting sort by bucket = tel*256 + (255-rate)
	var cnt [3*256 + 1]int
	for i := 0; i < n; i++ {
		cnt[int(c.tel[i])*256+255-int(c.rate[i])+1]++
	}
	for i := 1; i < len(cnt); i++ {
		cnt[i] += cnt[i-1]
	}
	start := cnt
	pk := make([]uint64, n)
	fill := cnt
	for i := 0; i < n; i++ {
		b := int(c.tel[i])*256 + 255 - int(c.rate[i])
		pk[fill[b]] = uint64(hh[i])<<32 | uint64(c.g[i])
		fill[b]++
	}
	// sort every bucket by (h32, g) in parallel, then fix runs of equal h32 with the full md5
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < max(1, workers); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				b := int(next.Add(1) - 1)
				if b >= 3*256 {
					return
				}
				s := pk[start[b]:start[b+1]]
				if len(s) < 2 {
					continue
				}
				slices.Sort(s)
				for i := 0; i < len(s); {
					j := i + 1
					for j < len(s) && s[j]>>32 == s[i]>>32 {
						j++
					}
					if j-i > 1 {
						slices.SortFunc(s[i:j], func(a, b uint64) int {
							x, y := md5of(uint32(a), "42"), md5of(uint32(b), "42")
							for k := range x {
								if x[k] != y[k] {
									return int(x[k]) - int(y[k])
								}
							}
							return int(uint32(a)) - int(uint32(b))
						})
					}
					i = j
				}
			}
		}()
	}
	wg.Wait()
	t2 := time.Now()
	b := &rmBase{n: n, g: make([]uint32, n), cat: make([]uint8, n), rate: make([]uint8, n), price: make([]int32, n), h32: make([]uint32, n)}
	parFor(n, workers, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			g := uint32(pk[i])
			b.g[i], b.h32[i] = g, uint32(pk[i]>>32)
			b.cat[i], b.rate[i], b.price[i] = c.cat[g], c.rate[g], c.price[g]
		}
	})
	for t := 0; t < 3; t++ {
		b.seg[t] = start[t*256]
	}
	b.seg[3] = n
	if st != nil {
		st.Hash, st.Sort, st.Mat = t1.Sub(t0), t2.Sub(t1), time.Since(t2)
	}
	return b
}

func md5sum13(b []byte) uint32 {
	s := md5.Sum(b)
	return binary.BigEndian.Uint32(s[:4])
}

func (b *rmBase) buildBitmaps(workers int) time.Duration {
	t0 := time.Now()
	n := b.n
	nw := (n + 63) / 64
	mn, mx := int32(1<<30), int32(-1)
	for _, p := range b.price {
		mn, mx = min(mn, p), max(mx, p)
	}
	b.pmin, b.pmax = mn, mx
	b.pw = (mx-mn)/NB + 1
	for c := 0; c < 5; c++ {
		b.catB[c] = make([]uint64, nw)
	}
	b.rb = make([][]uint64, NB-1)
	for k := range b.rb {
		b.rb[k] = make([]uint64, nw)
	}
	parFor(nw, workers, func(lo, hi int) {
		for w := lo; w < hi; w++ {
			for i := w * 64; i < min(n, w*64+64); i++ {
				bit := uint64(1) << (i & 63)
				b.catB[b.cat[i]][w] |= bit
				bk := int((b.price[i] - mn) / b.pw)
				for k := bk; k < NB-1; k++ {
					b.rb[k][w] |= bit
				}
			}
		}
	})
	return time.Since(t0)
}

func (b *rmBase) bytesPerRow() float64 { return 4 + 1 + 1 + 4 + 4 } // g, cat, rate, price, h32

// ---- changed rows: dead bitmap over base positions + sorted-on-demand tail ----------------------------

type tailT struct {
	g              []uint32
	cat, tel, rate []uint8
	price          []int32
	h32            []uint32
}
type deltaState struct {
	dead []uint64
	tail tailT
	N    int
}

// applyDelta marks superseded base rows and keeps the latest version of every touched key in the tail.
// pos is the key -> base position directory (dense numeric ids, 4 B/row), built here (cost reported by the caller).
func applyDelta(b *rmBase, ops []dop, pos []uint32) *deltaState {
	ds := &deltaState{dead: make([]uint64, (b.n+63)/64), N: b.n}
	last := lastOps(ops)
	keys := make([]uint32, 0, len(last))
	for g := range last {
		keys = append(keys, g)
	}
	slices.Sort(keys)
	for _, g := range keys {
		o := last[g]
		if int(g) < b.n {
			p := pos[g]
			ds.dead[p>>6] |= 1 << (p & 63)
		}
		if o.del {
			continue
		}
		t := &ds.tail
		t.g = append(t.g, g)
		t.cat, t.tel, t.rate = append(t.cat, o.cat), append(t.tel, o.tel), append(t.rate, o.rate)
		t.price = append(t.price, o.price)
		t.h32 = append(t.h32, h32of(g, "42"))
	}
	return ds
}

func buildPos(b *rmBase) []uint32 {
	pos := make([]uint32, b.n)
	for i, g := range b.g {
		pos[g] = uint32(i)
	}
	return pos
}

// tailTop returns the top `need` matching tail rows of carrier t in (rate DESC, md5, number) order.
func (ds *deltaState) tailTop(t uint8, q *q2, need int) []ent {
	tl := &ds.tail
	var cnt [256]int
	for i := range tl.g {
		if tl.tel[i] == t && tl.cat[i] == q.cat && tl.price[i] >= q.lo && tl.price[i] <= q.hi {
			cnt[tl.rate[i]]++
		}
	}
	thr, acc := 0, 0
	for r := 255; r >= 0; r-- {
		acc += cnt[r]
		thr = r
		if acc >= need {
			break
		}
	}
	var out []ent
	for i := range tl.g {
		if tl.tel[i] == t && int(tl.rate[i]) >= thr && tl.cat[i] == q.cat && tl.price[i] >= q.lo && tl.price[i] <= q.hi {
			out = append(out, ent{g: tl.g[i], tel: t, rate: tl.rate[i], price: tl.price[i], h32: tl.h32[i]})
		}
	}
	slices.SortFunc(out, func(a, b ent) int { return cmpEnt(&a, &b, q.seed) })
	if len(out) > need {
		out = out[:need]
	}
	return out
}

// ---- cfg2 / cfg3 query (seed '42') ---------------------------------------------------------------------

func (b *rmBase) ent(i int, t uint8) ent {
	return ent{g: b.g[i], tel: t, rate: b.rate[i], price: b.price[i], h32: b.h32[i]}
}

// walk2: lazy filter from the top of carrier t's list, stop after `need` matches. Returns positions of ranks skip..need-1.
func (b *rmBase) walk2(t int, q *q2, dead []uint64, skip, need int, st *qstat) []int32 {
	s, e := b.seg[t], b.seg[t+1]
	cat, price := b.cat, b.price
	var out []int32
	cnt := 0
	i := s
	for ; i < e; i++ {
		if cat[i] != q.cat {
			continue
		}
		p := price[i]
		if p < q.lo || p > q.hi {
			continue
		}
		if dead != nil && dead[i>>6]&(1<<(i&63)) != 0 {
			continue
		}
		cnt++
		if cnt > skip {
			out = append(out, int32(i))
		}
		if cnt == need {
			i++
			break
		}
	}
	st.covered[t] = int64(i - s)
	st.tested = st.tested + int64(i-s) // cfg2 reads category (and price when category matches) of every row walked
	return out
}

func (b *rmBase) rw(k, w int) uint64 {
	if k < 0 {
		return 0
	}
	if k >= NB-1 {
		return ^uint64(0)
	}
	return b.rb[k][w]
}

// walk3: same walk, 64 rows per step through the bitmaps. Price is verified only for the two edge buckets.
// skip>0 is honoured by popcount (no row read) only when called without a tail.
func (b *rmBase) walk3(t int, q *q2, dead []uint64, skip, need int, st *qstat) []int32 {
	s, e := b.seg[t], b.seg[t+1]
	lo, hi := max(q.lo, b.pmin), min(q.hi, b.pmax)
	if lo > hi || e <= s {
		return nil
	}
	bl, bh := int((lo-b.pmin)/b.pw), int((hi-b.pmin)/b.pw)
	cat := b.catB[q.cat]
	var out []int32
	cnt := 0
	w0, w1 := s>>6, (e-1)>>6
	var tested int64
	endw := w1
	for w := w0; w <= w1; w++ {
		cw := cat[w]
		if w == w0 {
			cw &= ^uint64(0) << (s & 63)
		}
		if w == w1 && e&63 != 0 {
			cw &= (uint64(1) << (e & 63)) - 1
		}
		if dead != nil {
			cw &^= dead[w]
		}
		if cw == 0 {
			continue
		}
		full := cw & (b.rw(bh-1, w) &^ b.rw(bl, w))
		edge := cw & ((b.rw(bl, w) &^ b.rw(bl-1, w)) | (b.rw(bh, w) &^ b.rw(bh-1, w))) &^ full
		for x := edge; x != 0; x &= x - 1 {
			j := bits.TrailingZeros64(x)
			p := b.price[w*64+j]
			tested++
			if p >= q.lo && p <= q.hi {
				full |= 1 << j
			}
		}
		pc := bits.OnesCount64(full)
		if pc == 0 {
			continue
		}
		if cnt+pc <= skip {
			cnt += pc
			continue
		}
		for x := full; x != 0; x &= x - 1 {
			j := bits.TrailingZeros64(x)
			cnt++
			tested++
			if cnt > skip {
				out = append(out, int32(w*64+j))
			}
			if cnt == need {
				endw = w
				goto done
			}
		}
	}
done:
	st.covered[t] = int64(min(e, (endw+1)*64) - s)
	st.tested += tested
	return out
}

// R2rank answers the canonical query (seed '42') from the rank-major base. ds may be nil (no changed rows).
func (b *rmBase) R2rank(q *q2, ds *deltaState, bm bool) ([]Out, qstat) {
	var st [3]qstat
	var cands [3][]ent
	var dead []uint64
	hasTail := false
	if ds != nil {
		dead = ds.dead
		hasTail = len(ds.tail.g) > 0
	}
	run := func(t int) {
		need := q.page * q.quota[t]
		skip := (q.page - 1) * q.quota[t]
		if hasTail {
			skip = 0 // the tail may displace base rows: keep every candidate of the first `need` ranks
		}
		var pos []int32
		if bm {
			pos = b.walk3(t, q, dead, skip, need, &st[t])
		} else {
			pos = b.walk2(t, q, dead, skip, need, &st[t])
		}
		base := make([]ent, len(pos))
		for i, p := range pos {
			base[i] = b.ent(int(p), uint8(t))
		}
		if !hasTail {
			cands[t] = base
			return
		}
		tl := ds.tailTop(uint8(t), q, need)
		m := make([]ent, 0, need)
		i, j := 0, 0
		for len(m) < need && (i < len(base) || j < len(tl)) {
			if j >= len(tl) || (i < len(base) && cmpEnt(&base[i], &tl[j], q.seed) < 0) {
				m = append(m, base[i])
				i++
			} else {
				m = append(m, tl[j])
				j++
			}
		}
		cands[t] = m
	}
	if q.threads <= 1 {
		for t := 0; t < 3; t++ {
			run(t)
		}
	} else {
		var wg sync.WaitGroup
		for t := 0; t < 3; t++ {
			wg.Add(1)
			go func() { defer wg.Done(); run(t) }()
		}
		wg.Wait()
	}
	var tot qstat
	for t := 0; t < 3; t++ {
		tot.covered[t] = st[t].covered[t]
		tot.tested += st[t].tested
	}
	if hasTail {
		tot.tailN = int64(len(ds.tail.g))
	}
	// cands hold ranks skip..need-1 of each carrier unless a tail was merged (then ranks 0..need-1)
	for t := 0; t < 3; t++ {
		if hasTail {
			lo := (q.page - 1) * q.quota[t]
			if lo < len(cands[t]) {
				cands[t] = cands[t][lo:]
			} else {
				cands[t] = nil
			}
		}
	}
	return assembleRange(cands, q), tot
}

// assembleRange: candidate lists already hold exactly ranks skip..need-1 of each carrier.
func assembleRange(c [3][]ent, q *q2) []Out {
	var all []ent
	for t := 0; t < 3; t++ {
		all = append(all, c[t]...)
	}
	slices.SortFunc(all, func(a, b ent) int { return cmpEnt(&a, &b, q.seed) })
	out := make([]Out, len(all))
	for i, e := range all {
		k := numStr(e.g)
		out[i] = Out{Key: string(k[:]), Tel: telNames[e.tel], Name: telNames[e.tel], Price: e.price, Rate: e.rate}
	}
	return out
}

// ---- fallback: any seed. Scan with a bounded heap per carrier (the engine's current operator shape) -----

type fent struct {
	g     uint32
	rate  uint8
	price int32
	hh    bool
	h     [16]byte
}
type fheap struct {
	cap  int
	seed string
	e    []fent
}

func (h *fheap) hash(x *fent) *[16]byte {
	if !x.hh {
		x.h, x.hh = md5of(x.g, h.seed), true
	}
	return &x.h
}
func (h *fheap) worse(a, b *fent) bool {
	if a.rate != b.rate {
		return a.rate < b.rate
	}
	if a.g == b.g {
		return false
	}
	x, y := h.hash(a), h.hash(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return a.g > b.g
}
func (h *fheap) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h.worse(&h.e[i], &h.e[p]) {
			return
		}
		h.e[i], h.e[p] = h.e[p], h.e[i]
		i = p
	}
}
func (h *fheap) down(i int) {
	n := len(h.e)
	for {
		l, r, w := 2*i+1, 2*i+2, i
		if l < n && h.worse(&h.e[l], &h.e[w]) {
			w = l
		}
		if r < n && h.worse(&h.e[r], &h.e[w]) {
			w = r
		}
		if w == i {
			return
		}
		h.e[i], h.e[w] = h.e[w], h.e[i]
		i = w
	}
}
func (h *fheap) offer(rate uint8, g uint32, price int32) {
	if len(h.e) < h.cap {
		h.e = append(h.e, fent{g: g, rate: rate, price: price})
		h.up(len(h.e) - 1)
		return
	}
	if rate < h.e[0].rate {
		return
	}
	c := fent{g: g, rate: rate, price: price}
	if rate == h.e[0].rate && !h.worse(&h.e[0], &c) {
		return
	}
	h.e[0] = c
	h.down(0)
}

// fallbackR2 answers any seed from the rank-major base. rateStop: the list of a carrier is sorted by rate DESC,
// so the scan of a carrier ends when its heap is full and the next row's rate is below the heap root's rate.
func (b *rmBase) fallbackR2(q *q2, ds *deltaState, rateStop bool) ([]Out, qstat) {
	var dead []uint64
	if ds != nil {
		dead = ds.dead
	}
	type job struct{ t, lo, hi int }
	var jobs []job
	step := 1 << 16
	if rateStop {
		step = 1 << 40
	}
	for t := 0; t < 3; t++ {
		for lo := b.seg[t]; lo < b.seg[t+1]; lo += step {
			jobs = append(jobs, job{t, lo, min(b.seg[t+1], lo+step)})
		}
	}
	nw := max(1, q.threads)
	hs := make([][3]*fheap, nw)
	var covered [3]atomic.Int64
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < nw; w++ {
		for t := 0; t < 3; t++ {
			hs[w][t] = &fheap{cap: q.page * q.quota[t], seed: q.seed}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ji := int(next.Add(1) - 1)
				if ji >= len(jobs) {
					return
				}
				j := jobs[ji]
				h := hs[w][j.t]
				i := j.lo
				for ; i < j.hi; i++ {
					if rateStop && len(h.e) == h.cap && b.rate[i] < h.e[0].rate {
						break
					}
					if b.cat[i] != q.cat || b.price[i] < q.lo || b.price[i] > q.hi {
						continue
					}
					if dead != nil && dead[i>>6]&(1<<(i&63)) != 0 {
						continue
					}
					h.offer(b.rate[i], b.g[i], b.price[i])
				}
				covered[j.t].Add(int64(i - j.lo))
			}
		}()
	}
	wg.Wait()
	var tot qstat
	var cands [3][]ent
	for t := 0; t < 3; t++ {
		tot.covered[t] = covered[t].Load()
		var all []fent
		for w := 0; w < nw; w++ {
			all = append(all, hs[w][t].e...)
		}
		for _, e := range all {
			x := ent{g: e.g, tel: uint8(t), rate: e.rate, price: e.price}
			if q.seed == "42" {
				x.h32 = h32of(e.g, "42")
			}
			cands[t] = append(cands[t], x)
		}
		if ds != nil {
			tl := &ds.tail
			tot.tailN = int64(len(tl.g))
			for i := range tl.g {
				if int(tl.tel[i]) == t && tl.cat[i] == q.cat && tl.price[i] >= q.lo && tl.price[i] <= q.hi {
					x := ent{g: tl.g[i], tel: uint8(t), rate: tl.rate[i], price: tl.price[i]}
					if q.seed == "42" {
						x.h32 = tl.h32[i]
					}
					cands[t] = append(cands[t], x)
				}
			}
		}
		slices.SortFunc(cands[t], func(a, b ent) int { return cmpEnt(&a, &b, q.seed) })
	}
	return assemble(cands, q), tot
}
