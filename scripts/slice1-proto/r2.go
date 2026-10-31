package main

import (
	"bytes"
	"crypto/md5"
	"slices"
	"sync"
	"sync/atomic"
)

// R2 operator: top-K per carrier over columns (PROTOTYPE).

type entry struct {
	rate uint8
	id   uint32 // row id in state space (base then tail)
	hh   bool   // hash computed
	hash [16]byte
}

type heap struct {
	cap  int
	e    []entry
	st   *State
}

func (h *heap) hashOf(x *entry) *[16]byte {
	if !x.hh {
		k := h.st.KeyAt(x.id)
		var b [13]byte
		copy(b[:], k)
		b[11], b[12] = '4', '2'
		x.hash, x.hh = md5.Sum(b[:]), true
	}
	return &x.hash
}

// worse reports whether a sorts after b in (rate DESC, md5(number||'42'), number).
func (h *heap) worse(a, b *entry) bool {
	if a.rate != b.rate {
		return a.rate < b.rate
	}
	if c := bytes.Compare(h.hashOf(a)[:], h.hashOf(b)[:]); c != 0 {
		return c > 0
	}
	return bytes.Compare(h.st.KeyAt(a.id), h.st.KeyAt(b.id)) > 0
}

// worst-at-root heap
func (h *heap) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h.worse(&h.e[i], &h.e[p]) {
			return
		}
		h.e[i], h.e[p] = h.e[p], h.e[i]
		i = p
	}
}
func (h *heap) down(i int) {
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

// offer: cheap reject on the leading key (rate) before any hash.
func (h *heap) offer(rate uint8, id uint32) {
	if len(h.e) < h.cap {
		h.e = append(h.e, entry{rate: rate, id: id})
		h.up(len(h.e) - 1)
		return
	}
	if rate < h.e[0].rate {
		return
	}
	c := entry{rate: rate, id: id}
	if rate == h.e[0].rate && !h.worse(&h.e[0], &c) {
		return
	}
	h.e[0] = c
	h.down(0)
}

type Out struct {
	Key   string
	Tel   string
	Name  string
	Price int32
	Rate  uint8
}

type R2Stats struct {
	ScannedBase, ScannedTail, Passed int64
	ChunksRead, ChunksPruned           int
}

type R2Q struct {
	Cat      uint8
	Lo, Hi   int32
	Page     int
	Quota    [3]int
	Workers  int
}

// R2 runs the query on one immutable State.
func (st *State) R2(q R2Q) ([]Out, R2Stats) {
	var want [3]bool
	var quota [3]int
	for t := 0; t < 3; t++ {
		want[t] = st.Dim.car[t].Active // broadcast join with the dimension at this snapshot
		quota[t] = q.Quota[t]
	}
	b := st.Base
	nz := len(b.zmin)
	var next atomic.Int64
	var stats R2Stats
	var mu sync.Mutex
	var all [][3]*heap
	workers := max(1, q.Workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var hs [3]*heap
			for t := 0; t < 3; t++ {
				hs[t] = &heap{cap: q.Page * quota[t], st: st}
			}
			var s R2Stats
			for {
				z := int(next.Add(1) - 1)
				if z >= nz {
					break
				}
				if b.zcat[z]&(1<<q.Cat) == 0 || b.zmax[z] < q.Lo || b.zmin[z] > q.Hi {
					s.ChunksPruned++
					continue
				}
				s.ChunksRead++
				lo, hi := z*ChunkRows, min(b.n, (z+1)*ChunkRows)
				cat, price, tel, rate := b.cat[lo:hi], b.price[lo:hi], b.tel[lo:hi], b.rate[lo:hi]
				s.ScannedBase += int64(hi - lo)
				for i := range cat {
					if cat[i] != q.Cat {
						continue
					}
					p := price[i]
					if p < q.Lo || p > q.Hi {
						continue
					}
					id := lo + i
					if st.Changed.Get(id) {
						continue
					}
					t := tel[i]
					if !want[t] {
						continue
					}
					s.Passed++
					hs[t].offer(rate[i], uint32(id))
				}
			}
			mu.Lock()
			all = append(all, hs)
			stats.ScannedBase += s.ScannedBase
			stats.Passed += s.Passed
			stats.ChunksRead += s.ChunksRead
			stats.ChunksPruned += s.ChunksPruned
			mu.Unlock()
		}()
	}
	wg.Wait()
	// tail (single thread: it is small by construction)
	tl := &st.Tail
	var th [3]*heap
	for t := 0; t < 3; t++ {
		th[t] = &heap{cap: q.Page * quota[t], st: st}
	}
	for i := 0; i < tl.n; i++ {
		if tl.cat[i] != q.Cat || tl.tomb[i] || tl.price[i] < q.Lo || tl.price[i] > q.Hi || st.TailDead.Get(i) || !want[tl.tel[i]] {
			continue
		}
		stats.Passed++
		th[tl.tel[i]].offer(tl.rate[i], uint32(b.n+i))
	}
	stats.ScannedTail = int64(tl.n)
	all = append(all, th)
	// merge candidates per group, take the page
	var res []entry
	var tels []uint8
	for t := 0; t < 3; t++ {
		var cand []entry
		for _, hs := range all {
			cand = append(cand, hs[t].e...)
		}
		h := &heap{st: st}
		slices.SortFunc(cand, func(a, b entry) int {
			switch {
			case h.worse(&a, &b):
				return 1
			case h.worse(&b, &a):
				return -1
			}
			return 0
		})
		lo, hi := (q.Page-1)*quota[t], min(len(cand), q.Page*quota[t])
		if lo < hi {
			for _, c := range cand[lo:hi] {
				res = append(res, c)
				tels = append(tels, uint8(t))
			}
		}
	}
	idx := make([]int, len(res))
	for i := range idx {
		idx[i] = i
	}
	h := &heap{st: st}
	slices.SortFunc(idx, func(i, j int) int {
		a, b := &res[i], &res[j]
		switch {
		case h.worse(a, b):
			return 1
		case h.worse(b, a):
			return -1
		}
		return 0
	})
	out := make([]Out, 0, len(res))
	for _, i := range idx {
		e := res[i]
		id := int(e.id)
		var price int32
		if id < b.n {
			price = b.price[id]
		} else {
			price = tl.price[id-b.n]
		}
		out = append(out, Out{Key: string(st.KeyAt(e.id)), Tel: telNames[tels[i]], Name: st.Dim.car[tels[i]].Name, Price: price, Rate: e.rate})
	}
	return out, stats
}
