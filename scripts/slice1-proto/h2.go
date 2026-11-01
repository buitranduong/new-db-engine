package main

// H2 KERNEL EXPERIMENT (docs/34 H2), throw-away, kernel only: no SQL, no pgwire, no WAL, no harness.
// Three configurations of the canonical R2 (rank (p-1)*q+1..p*q per carrier of the rows that pass the filter,
// ORDER BY store_rate DESC, md5(number||seed), number):
//   cfg1  filter-major base (category, price) + zone maps  = the existing State.R2 of this package
//   cfg2  rank-major base (tel, store_rate DESC, md5(number||'42'), number): lazy filter, STOP after p*q matches
//   cfg3  cfg2 + per-row filter bitmaps (category bit per value, range-encoded price-bucket bitmaps)
// The rank-major order is valid only for seed '42'; other seeds use fallbackR2 (scan with a bounded heap).

import (
	"crypto/md5"
	"encoding/binary"
	"flag"
	"fmt"
	"math/bits"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	fH2Part = flag.String("h2part", "check", "h2 mode: check|bench|variants|merge|dump")
	fH2Reps = flag.Int("h2reps", 200, "h2 repetitions per timing")
	fH2Var  = flag.Int("h2var", 0, "generator variant (0 canonical)")
)

const NB = 20 // price buckets of the range-encoded bitmaps

// ---- generator (same formulas as genRow / r2big.sh for variant 0) ------------------------------

func genCols(g int64, v int) (cat, tel, rate uint8, price int32) {
	cat = uint8(g % 5)
	price = int32(500000 + (g*7919)%4000000)
	switch m := ((g / 5) * 13) % 10; {
	case m < 3:
		tel = 0
	case m < 8:
		tel = 1
	default:
		tel = 2
	}
	rate = uint8(5 + (g*31)%6)
	switch v {
	case 1: // 200 distinct store_rate values
		rate = uint8(5 + ((uint64(g)*2654435761)>>7)%200)
	case 2: // skew: every TAM_HOA row has the LOWEST rate (matches sit at the end of each carrier list)
		if cat == 0 {
			rate = 5
		}
	case 3: // carrier C starved: almost no TAM_HOA in C (fewer matches than quota)
		if tel == 2 && cat == 0 && g%250000 != 0 {
			cat = 1
		}
	case 4: // carrier C tiny (about N/200000 rows)
		if tel == 2 && g%200000 != 3 {
			tel = 1
		}
	}
	return
}

func numStr(g uint32) (b [11]byte) {
	b[0], b[1] = '0', '9'
	for i := 10; i >= 2; i-- {
		b[i] = byte('0' + g%10)
		g /= 10
	}
	return
}

func md5of(g uint32, seed string) [16]byte {
	var b [32]byte
	k := numStr(g)
	copy(b[:], k[:])
	n := copy(b[11:], seed)
	return md5.Sum(b[:11+n])
}
func h32of(g uint32, seed string) uint32 { h := md5of(g, seed); return binary.BigEndian.Uint32(h[:4]) }

type cols struct {
	n              int
	g              []uint32
	cat, tel, rate []uint8
	price          []int32
}

func parFor(n, workers int, f func(lo, hi int)) {
	workers = max(1, workers)
	var wg sync.WaitGroup
	step := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		lo, hi := w*step, min(n, (w+1)*step)
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func() { defer wg.Done(); f(lo, hi) }()
	}
	wg.Wait()
}

func genAll(n, v, workers int) *cols {
	c := &cols{n: n, g: make([]uint32, n), cat: make([]uint8, n), tel: make([]uint8, n), rate: make([]uint8, n), price: make([]int32, n)}
	parFor(n, workers, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			c.g[i] = uint32(i)
			c.cat[i], c.tel[i], c.rate[i], c.price[i] = genCols(int64(i), v)
		}
	})
	return c
}

func (c *cols) rows() []Row {
	rows := make([]Row, c.n)
	for i := range rows {
		rows[i] = Row{Key: numStr(c.g[i]), Cat: c.cat[i], Tel: c.tel[i], Rate: c.rate[i], Price: c.price[i]}
	}
	return rows
}

// ---- query ---------------------------------------------------------------------------------------

type q2 struct {
	cat     uint8
	lo, hi  int32
	page    int
	quota   [3]int
	seed    string
	threads int
}

func canon(page, threads int) q2 {
	return q2{cat: 0, lo: 1000000, hi: 3000000, page: page, quota: [3]int{20, 50, 30}, seed: "42", threads: threads}
}

type qstat struct {
	covered [3]int64 // rows walked/scanned per carrier (base)
	tested  int64    // rows whose exact predicate columns were read (cfg3: edge verification only)
	tailN   int64
}

func (s *qstat) total() int64 { return s.covered[0] + s.covered[1] + s.covered[2] + s.tailN }

type ent struct {
	g     uint32
	tel   uint8
	rate  uint8
	price int32
	h32   uint32 // md5(number||'42') first 4 bytes, valid when seed == "42"
}

// cmpEnt orders (rate DESC, md5(number||seed), number): negative if a first.
func cmpEnt(a, b *ent, seed string) int {
	if a.rate != b.rate {
		if a.rate > b.rate {
			return -1
		}
		return 1
	}
	if a.g == b.g {
		return 0
	}
	if seed == "42" {
		if a.h32 != b.h32 {
			if a.h32 < b.h32 {
				return -1
			}
			return 1
		}
	}
	x, y := md5of(a.g, seed), md5of(b.g, seed)
	for i := range x {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	if a.g < b.g {
		return -1
	}
	return 1
}

func assemble(c [3][]ent, q *q2) []Out {
	var all []ent
	for t := 0; t < 3; t++ {
		lo, hi := (q.page-1)*q.quota[t], min(len(c[t]), q.page*q.quota[t])
		if lo < hi {
			all = append(all, c[t][lo:hi]...)
		}
	}
	slices.SortFunc(all, func(a, b ent) int { return cmpEnt(&a, &b, q.seed) })
	out := make([]Out, len(all))
	for i, e := range all {
		k := numStr(e.g)
		out[i] = Out{Key: string(k[:]), Tel: telNames[e.tel], Name: telNames[e.tel], Price: e.price, Rate: e.rate}
	}
	return out
}

func sameOut(a, b []Out) string {
	if len(a) != len(b) {
		return fmt.Sprintf("len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Tel != b[i].Tel || a[i].Price != b[i].Price || a[i].Rate != b[i].Rate {
			return fmt.Sprintf("row %d: %v vs %v", i, a[i], b[i])
		}
	}
	return ""
}

// ---- delta (changed rows) -------------------------------------------------------------------------

type dop struct {
	g              uint32
	del            bool
	cat, tel, rate uint8
	price          int32
}

// genDelta: K ops; 45% update of an existing key, 10% delete, 45% insert of a new key.
func genDelta(n, K, v int) []dop {
	ops := make([]dop, 0, K)
	x := uint64(88172645463325252)
	rnd := func() uint64 { x ^= x << 13; x ^= x >> 7; x ^= x << 17; return x }
	for i := 0; i < K; i++ {
		m := rnd() % 20
		switch {
		case m < 9: // update
			g := uint32(rnd() % uint64(n))
			o := dop{g: g}
			o.cat, o.tel, o.rate, o.price = genCols(int64(g), v)
			if rnd()%2 == 0 {
				o.cat = 0
				o.price = int32(1000000 + rnd()%2000001)
			}
			if rnd()%2 == 0 {
				o.rate = 10
			}
			ops = append(ops, o)
		case m < 11:
			ops = append(ops, dop{g: uint32(rnd() % uint64(n)), del: true})
		default:
			g := uint32(n + i)
			o := dop{g: g}
			o.cat, o.tel, o.rate, o.price = genCols(int64(g), v)
			if rnd()%2 == 0 {
				o.cat = 0
				o.price = int32(1000000 + rnd()%2000001)
			}
			if rnd()%2 == 0 {
				o.rate = 10
			}
			ops = append(ops, o)
		}
	}
	return ops
}

func lastOps(ops []dop) map[uint32]dop {
	m := make(map[uint32]dop, len(ops))
	for _, o := range ops {
		m[o.g] = o
	}
	return m
}

func (o dop) toOp() Op {
	r := Row{Key: numStr(o.g), Cat: o.cat, Tel: o.tel, Rate: o.rate, Price: o.price}
	if o.del {
		return Op{Kind: OpDel, Row: r}
	}
	return Op{Kind: OpPut, Row: r}
}

// ---- reference (streaming brute force, same semantics as ref.go Model.R2, any seed) ---------------

func refR2(n, v int, last map[uint32]dop, q *q2) []Out {
	var g [3][]ent
	add := func(id uint32, cat, tel, rate uint8, price int32) {
		if cat != q.cat || price < q.lo || price > q.hi {
			return
		}
		g[tel] = append(g[tel], ent{g: id, tel: tel, rate: rate, price: price, h32: h32of(id, "42")})
	}
	for i := 0; i < n; i++ {
		id := uint32(i)
		if o, ok := last[id]; ok {
			if !o.del {
				add(id, o.cat, o.tel, o.rate, o.price)
			}
			continue
		}
		cat, tel, rate, price := genCols(int64(i), v)
		add(id, cat, tel, rate, price)
	}
	for id, o := range last {
		if int(id) >= n && !o.del {
			add(id, o.cat, o.tel, o.rate, o.price)
		}
	}
	for t := 0; t < 3; t++ {
		slices.SortFunc(g[t], func(a, b ent) int { return cmpEnt(&a, &b, q.seed) })
	}
	return assemble(g, q)
}

// ---- timing helper ---------------------------------------------------------------------------------

func timeIt(reps int, f func()) (p50, p99 time.Duration) {
	for i := 0; i < 3; i++ {
		f()
	}
	ds := make([]time.Duration, reps)
	for i := range ds {
		t := time.Now()
		f()
		ds[i] = time.Since(t)
	}
	return med(ds), pct(ds, 0.99)
}
func us(d time.Duration) string { return fmt.Sprintf("%.1fus", float64(d.Nanoseconds())/1000) }
func dur(d time.Duration) string {
	if d < time.Millisecond {
		return us(d)
	}
	return ms(d)
}

var _ = sort.Ints
var _ = os.Exit
var _ = runtime.GC
var _ = bits.Len
var _ atomic.Int64
