package proj

import (
	"math/bits"

	"stask/internal/schema"
)

// count(DISTINCT x) grouped by at most one column, without materializing
// the (group, x) pairs as result rows: each scan worker routes its pairs
// into partitions by hash, as ClickHouse's two-level hash tables split
// keys into buckets by hash so that buckets merge independently; each
// partition is then deduplicated by one goroutine with an open-addressing
// set and counted per group. Other aggregates accumulate per group as on
// the single-key path.

const distinctMaxParts = 256

func mix64(g, x uint64) uint64 {
	h := g*0x9E3779B97F4A7C15 ^ x
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// pairSet is an open-addressing set of (group key, value) pairs.
type pairSet struct {
	keys [][2]uint64
	used []bool
	mask uint64
}

func newPairSet(n int) *pairSet {
	size := 1 << max(4, bits.Len(uint(n*2)))
	return &pairSet{keys: make([][2]uint64, size), used: make([]bool, size), mask: uint64(size - 1)}
}

// add reports whether (g, x) was new.
func (s *pairSet) add(g, x uint64) bool {
	for i := mix64(g, x) & s.mask; ; i = (i + 1) & s.mask {
		if !s.used[i] {
			s.used[i], s.keys[i] = true, [2]uint64{g, x}
			return true
		}
		if s.keys[i] == [2]uint64{g, x} {
			return false
		}
	}
}

func (ct *colTable) aggregateDistinctFast(gcols []int, xi int, specs []aggSpec, distinct []bool, cfs []colFilter, q *AggQuery, workers int) *Result {
	nparts := 1 << bits.Len(uint(min(workers*4, distinctMaxParts)-1))
	shift := 64 - bits.Len(uint(nparts-1))
	type partial struct {
		groups  map[uint64]*group
		null    *group
		nullX   map[uint64]struct{}
		pairs   [][]uint64 // by partition: group key, value, ...
		scanned int64
		slab    groupSlab
	}
	parts := make([]partial, workers)
	for w := range parts {
		parts[w].groups = map[uint64]*group{}
		parts[w].nullX = map[uint64]struct{}{}
		parts[w].pairs = make([][]uint64, nparts)
	}
	xv := &ct.cols[xi]
	var gv *colVec
	if len(gcols) == 1 {
		gv = &ct.cols[gcols[0]]
	}
	nchunks := (ct.n + chunkRows - 1) / chunkRows
	next := chunkQueue(nchunks)
	q.parallel(workers, func(w int) {
		p := &parts[w]
		sel := make([]uint32, 0, chunkRows)
		for ci := range next {
			var live int
			sel, live = ct.selectChunk(sel, ci, cfs)
			p.scanned += int64(live)
			for _, pos := range sel {
				var gk uint64
				isNull := false
				if gv != nil {
					switch {
					case gv.isNull(int(pos)):
						isNull = true
					case gv.t == schema.TString:
						gk = uint64(gv.codes.at(int(pos)))
					default:
						gk = uint64(gv.intVal(gv.ints.at(int(pos))))
					}
				}
				var g *group
				if isNull {
					if g = p.null; g == nil {
						g = ct.newGroup(&p.slab, gcols, specs, int(pos))
						p.null = g
					}
				} else if g = p.groups[gk]; g == nil {
					g = ct.newGroup(&p.slab, gcols, specs, int(pos))
					p.groups[gk] = g
				}
				ct.accumulate(g.accs, specs, pos)
				if xv.isNull(int(pos)) {
					continue
				}
				var x uint64
				if xv.t == schema.TString {
					x = uint64(xv.codes.at(int(pos)))
				} else {
					x = uint64(xv.ints.at(int(pos)))
				}
				if isNull {
					p.nullX[x] = struct{}{}
					continue
				}
				pi := mix64(gk, x) >> shift
				p.pairs[pi] = append(p.pairs[pi], gk, x)
			}
			if !q.keep(w) {
				break
			}
		}
	})

	// dedupe each partition in parallel
	counts := make([]map[uint64]int64, nparts)
	q.each(nparts, func(pi int) {
		n := 0
		for w := range parts {
			n += len(parts[w].pairs[pi]) / 2
		}
		set := newPairSet(n)
		c := map[uint64]int64{}
		for w := range parts {
			ps := parts[w].pairs[pi]
			for i := 0; i < len(ps); i += 2 {
				if set.add(ps[i], ps[i+1]) {
					c[ps[i]]++
				}
			}
			parts[w].pairs[pi] = nil
		}
		counts[pi] = c
	})

	var scanned int64
	merged := map[uint64]*group{}
	var null *group
	nullX := map[uint64]struct{}{}
	for w := range parts {
		p := &parts[w]
		scanned += p.scanned
		for k, g := range p.groups {
			if into := merged[k]; into != nil {
				for i := range into.accs {
					into.accs[i].merge(&g.accs[i])
				}
			} else {
				merged[k] = g
			}
		}
		if p.null != nil {
			if null == nil {
				null = p.null
			} else {
				for i := range null.accs {
					null.accs[i].merge(&p.null.accs[i])
				}
			}
			for x := range p.nullX {
				nullX[x] = struct{}{}
			}
		}
	}
	setDistinct := func(g *group, n int64) {
		for i := range specs {
			if distinct[i] {
				g.accs[i].n = n
			}
		}
	}
	for k, g := range merged {
		var n int64
		for _, c := range counts {
			n += c[k]
		}
		setDistinct(g, n)
	}
	ngroups := len(merged)
	if null != nil {
		setDistinct(null, int64(len(nullX)))
		ngroups++
	}
	return finish(q.GroupBy, specs, func(yield func(*group) bool) {
		for _, g := range merged {
			if !yield(g) {
				return
			}
		}
		if null != nil {
			yield(null)
		}
	}, ngroups, q, scanned)
}
