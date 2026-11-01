package main

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"time"
)

func runH2(n int) {
	fmt.Printf("# H2 kernel experiment part=%s N=%d GOMAXPROCS=%d load1=%s\n", *fH2Part, n, runtime.GOMAXPROCS(0), loadavg())
	switch *fH2Part {
	case "check":
		h2check(n)
	case "bench":
		h2bench(n)
	case "variants":
		h2variants(n)
	case "merge":
		h2merge(n)
	case "dump":
		h2dump(n)
	default:
		fmt.Println("unknown h2part")
	}
}

func varsToRun() []int {
	if *fH2Var < 0 {
		return []int{0, 1, 2, 3, 4}
	}
	return []int{*fH2Var}
}

func cfg1Query(w *Writer, q *q2) ([]Out, R2Stats) {
	return w.Current().R2(R2Q{Cat: q.cat, Lo: q.lo, Hi: q.hi, Page: q.page, Quota: q.quota, Workers: q.threads})
}

func cfg1Writer(c *cols) *Writer {
	return NewWriter(buildBase(c.rows(), true, 0), defaultDim())
}

func applyCfg1(w *Writer, ops []dop) {
	var lsn uint64
	for i := 0; i < len(ops); i += 1000 {
		tx := Txn{}
		lsn++
		tx.LSN = lsn
		for _, o := range ops[i:min(len(ops), i+1000)] {
			tx.Ops = append(tx.Ops, o.toOp())
		}
		w.Apply(tx)
	}
	w.Publish()
}

var pagesChk = []int{1, 2, 3, 50}

func failf(f string, a ...any) {
	fmt.Printf("FAIL "+f+"\n", a...)
	os.Exit(1)
}

// h2check: every variant of every configuration against the streaming reference, row for row.
func h2check(n int) {
	// the generator equals genRow (variant 0), and the streaming reference equals ref.go (Model.R2)
	for g := int64(0); g < 200000; g++ {
		r := genRow(g)
		cat, tel, rate, price := genCols(g, 0)
		if r.Cat != cat || r.Tel != tel || r.Rate != rate || r.Price != price || string(r.Key[:]) != string(func() []byte { k := numStr(uint32(g)); return k[:] }()) {
			failf("genCols != genRow at %d", g)
		}
	}
	fmt.Println("OK generator genCols == genRow (200000 rows)")
	if n <= 1000000 {
		c := genAll(n, 0, 8)
		m := NewModel(c.rows(), *defaultDim())
		for _, p := range pagesChk {
			q := canon(p, 1)
			want := m.R2(R2Q{Cat: 0, Lo: q.lo, Hi: q.hi, Page: p, Quota: q.quota})
			if d := sameOut(want, refR2(n, 0, nil, &q)); d != "" {
				failf("streaming ref != ref.go Model page %d: %s", p, d)
			}
		}
		fmt.Printf("OK streaming reference == ref.go Model.R2 (N=%d, pages 1,2,3,50)\n", n)
	}
	for _, v := range varsToRun() {
		c := genAll(n, v, 8)
		b := buildRank(c, 8, nil)
		b.buildBitmaps(8)
		w1 := cfg1Writer(c)
		for _, K := range []int{0, 1000, 100000} {
			if K > n/5 {
				continue
			}
			var ds *deltaState
			var last map[uint32]dop
			if K > 0 {
				ops := genDelta(n, K, v)
				last = lastOps(ops)
				ds = applyDelta(b, ops, buildPos(b))
				w1 = cfg1Writer(c)
				applyCfg1(w1, ops)
			}
			for _, p := range pagesChk {
				for _, seed := range []string{"42", "7"} {
					q := canon(p, 1)
					q.seed = seed
					want := refR2(n, v, last, &q)
					if seed == "42" && p == 1 && v == 0 && len(want) != 100 {
						failf("vacuous oracle: page 1 has %d rows", len(want))
					}
					chk := func(name string, got []Out) {
						if d := sameOut(want, got); d != "" {
							failf("variant %d K=%d page %d seed %s %s: %s", v, K, p, seed, name, d)
						}
					}
					for _, th := range []int{1, 8} {
						q.threads = th
						if seed == "42" {
							g1, _ := cfg1Query(w1, &q)
							chk(fmt.Sprintf("cfg1 th=%d", th), g1)
							g2, _ := b.R2rank(&q, ds, false)
							chk(fmt.Sprintf("cfg2 th=%d", th), g2)
							g3, _ := b.R2rank(&q, ds, true)
							chk(fmt.Sprintf("cfg3 th=%d", th), g3)
						}
						f1, _ := b.fallbackR2(&q, ds, true)
						chk(fmt.Sprintf("fallback-ratestop th=%d", th), f1)
						f2, _ := b.fallbackR2(&q, ds, false)
						chk(fmt.Sprintf("fallback-full th=%d", th), f2)
					}
					if K == 0 && seed == "42" {
						fmt.Printf("  variant %d K=%d page %d: reference rows %d, all configurations identical\n", v, K, p, len(want))
					}
				}
			}
			fmt.Printf("OK variant %d N=%d delta K=%d: cfg1 cfg2 cfg3 (1 and 8 threads, seed 42) and fallback (seed 7 and 42, both flavours) == reference on pages 1,2,3,50\n", v, n, K)
		}
	}
}

func h2dump(n int) {
	dir := "/tmp/h2out"
	if len(os.Args) > 0 {
		for i, a := range os.Args {
			if a == "-out" && i+1 < len(os.Args) {
				dir = os.Args[i+1]
			}
		}
	}
	os.MkdirAll(dir, 0o755)
	c := genAll(n, 0, 8)
	b := buildRank(c, 8, nil)
	b.buildBitmaps(8)
	for _, seed := range []string{"42", "7"} {
		for _, p := range pagesChk {
			q := canon(p, 1)
			q.seed = seed
			ref := refR2(n, 0, nil, &q)
			dumpPage(fmt.Sprintf("%s/ref_s%s_p%d.tsv", dir, seed, p), ref)
			var got []Out
			if seed == "42" {
				got, _ = b.R2rank(&q, nil, true)
			} else {
				got, _ = b.fallbackR2(&q, nil, true)
			}
			dumpPage(fmt.Sprintf("%s/kernel_s%s_p%d.tsv", dir, seed, p), got)
		}
	}
	fmt.Println("dumped to", dir)
}

// ---- bench ---------------------------------------------------------------------------------------------

type cfgs struct {
	c  *cols
	b  *rmBase
	w1 *Writer
}

func line(tag string, f string, a ...any) { fmt.Printf("H2 %s "+f+"\n", append([]any{tag}, a...)...) }

func benchQuery(tag string, x *cfgs, q q2, reps int, only int) {
	run := func(name string, f func() (int64, int64)) {
		ins, tst := f()
		p50, p99 := timeIt(reps, func() { f() })
		line(tag, "%s page=%d thr=%d inspected=%d tested=%d p50=%s p99=%s", name, q.page, q.threads, ins, tst, dur(p50), dur(p99))
	}
	if only == 0 || only == 1 {
		run("cfg1-filter-major", func() (int64, int64) {
			_, s := cfg1Query(x.w1, &q)
			return s.ScannedBase + s.ScannedTail, s.ScannedBase
		})
	}
	if x.b == nil {
		return
	}
	if only == 0 || only == 2 {
		run("cfg2-rank-lazy", func() (int64, int64) {
			_, s := x.b.R2rank(&q, nil, false)
			return s.total(), s.tested
		})
	}
	if (only == 0 || only == 3) && x.b.catB[0] != nil {
		run("cfg3-rank-bitmap", func() (int64, int64) {
			_, s := x.b.R2rank(&q, nil, true)
			return s.total(), s.tested
		})
	}
}

func h2bench(n int) {
	v := *fH2Var
	reps := *fH2Reps
	m0 := heapMB()
	t := time.Now()
	c := genAll(n, v, 8)
	line("build", "N=%d variant=%d generate (8 workers) %s", n, v, ms(time.Since(t)))
	m1 := heapMB()
	// rank-major build, 1 worker and 8 workers
	var bs BuildStats
	t = time.Now()
	b := buildRank(c, 1, &bs)
	line("build", "rank-major 1 worker total %s (md5 %s, sort %s, materialize %s)", ms(time.Since(t)), ms(bs.Hash), ms(bs.Sort), ms(bs.Mat))
	b = nil
	t = time.Now()
	b = buildRank(c, 8, &bs)
	line("build", "rank-major 8 workers total %s (md5 %s, sort %s, materialize %s)", ms(time.Since(t)), ms(bs.Hash), ms(bs.Sort), ms(bs.Mat))
	m2 := heapMB()
	line("mem", "rank-major cfg2 columns (g,cat,rate,price,h32) +%.0f MB = %.1f B/row (build input cols excluded: %.1f B/row)", m2-m1, (m2-m1)*1e6/float64(n), (m1-m0)*1e6/float64(n))
	bt := b.buildBitmaps(8)
	m3 := heapMB()
	line("build", "cfg3 bitmaps (5 category + %d range-encoded price bitmaps) 8 workers %s", NB-1, ms(bt))
	line("mem", "cfg3 bitmaps +%.0f MB = %.1f B/row", m3-m2, (m3-m2)*1e6/float64(n))
	t = time.Now()
	w1 := cfg1Writer(c)
	line("build", "cfg1 filter-major (category,price) base+zonemaps+row directory (1 thread, existing code) %s", ms(time.Since(t)))
	m4 := heapMB()
	line("mem", "cfg1 State (base cols+11B keys+zone maps+directory) +%.0f MB = %.1f B/row", m4-m3, (m4-m3)*1e6/float64(n))
	x := &cfgs{c, b, w1}
	for _, p := range pagesChk {
		for _, th := range []int{1, 8} {
			benchQuery("canon", x, canon(p, th), reps, 0)
		}
	}
	// low selectivity: about 0.1% of the rows pass
	for _, p := range []int{1, 50} {
		for _, th := range []int{1, 8} {
			q := canon(p, th)
			q.hi = 1020000
			benchQuery("lowsel0.1pct", x, q, reps, 0)
		}
	}
	// fallback: arbitrary seed
	for _, p := range []int{1, 50} {
		for _, th := range []int{1, 8} {
			q := canon(p, th)
			q.seed = "7"
			for _, stop := range []bool{true, false} {
				name := "fallback-seed7-scan-full"
				if stop {
					name = "fallback-seed7-ratestop"
				}
				var cov int64
				f := func() { _, s := b.fallbackR2(&q, nil, stop); cov = s.total() }
				f()
				p50, p99 := timeIt(max(10, reps/4), f)
				line("fallback", "%s page=%d thr=%d inspected=%d p50=%s p99=%s", name, p, th, cov, dur(p50), dur(p99))
			}
		}
	}
	runtime.KeepAlive(x)
}

func h2variants(n int) {
	reps := *fH2Reps
	for _, v := range []int{1, 2, 3, 4} {
		c := genAll(n, v, 8)
		b := buildRank(c, 8, nil)
		b.buildBitmaps(8)
		w1 := cfg1Writer(c)
		x := &cfgs{c, b, w1}
		cnt := [3]int{}
		for t := 0; t < 3; t++ {
			cnt[t] = b.seg[t+1] - b.seg[t]
		}
		line("variant", "variant=%d N=%d rows per carrier A/B/C = %d/%d/%d", v, n, cnt[0], cnt[1], cnt[2])
		for _, p := range []int{1, 50} {
			for _, th := range []int{1, 8} {
				benchQuery(fmt.Sprintf("var%d", v), x, canon(p, th), reps, 0)
			}
		}
		x, w1, b, c = nil, nil, nil, nil
		runtime.GC()
	}
}

// ---- merge ---------------------------------------------------------------------------------------------

func bitsetOf(n int, ops []dop) []uint64 {
	t := make([]uint64, (n+63)/64)
	for _, o := range ops {
		if int(o.g) < n {
			t[o.g>>6] |= 1 << (o.g & 63)
		}
	}
	return t
}

func allocMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.TotalAlloc) / 1e6
}

// mergeRank folds the tail into a new rank-major base: only the tail is sorted; the base order is kept (one linear pass).
func mergeRank(b *rmBase, touched []uint64, ds *deltaState) *rmBase {
	tl := &ds.tail
	idx := make([]int32, len(tl.g))
	for i := range idx {
		idx[i] = int32(i)
	}
	te := func(i int32) ent {
		return ent{g: tl.g[i], tel: tl.tel[i], rate: tl.rate[i], price: tl.price[i], h32: tl.h32[i]}
	}
	slices.SortFunc(idx, func(a, c int32) int {
		if tl.tel[a] != tl.tel[c] {
			return int(tl.tel[a]) - int(tl.tel[c])
		}
		x, y := te(a), te(c)
		return cmpEnt(&x, &y, "42")
	})
	live := b.n
	for i := 0; i < b.n; i++ {
		if touched[b.g[i]>>6]&(1<<(b.g[i]&63)) != 0 {
			live--
		}
	}
	total := live + len(idx)
	nb := &rmBase{n: total, g: make([]uint32, 0, total), cat: make([]uint8, 0, total), rate: make([]uint8, 0, total), price: make([]int32, 0, total), h32: make([]uint32, 0, total)}
	j := 0
	for t := 0; t < 3; t++ {
		nb.seg[t] = len(nb.g)
		for i := b.seg[t]; i < b.seg[t+1]; i++ {
			g := b.g[i]
			if touched[g>>6]&(1<<(g&63)) != 0 {
				continue
			}
			be := ent{g: g, rate: b.rate[i], h32: b.h32[i]}
			for j < len(idx) && int(tl.tel[idx[j]]) == t {
				x := te(idx[j])
				if cmpEnt(&x, &be, "42") >= 0 {
					break
				}
				nb.g, nb.cat, nb.rate, nb.price, nb.h32 = append(nb.g, x.g), append(nb.cat, tl.cat[idx[j]]), append(nb.rate, x.rate), append(nb.price, x.price), append(nb.h32, x.h32)
				j++
			}
			nb.g, nb.cat, nb.rate, nb.price, nb.h32 = append(nb.g, g), append(nb.cat, b.cat[i]), append(nb.rate, b.rate[i]), append(nb.price, b.price[i]), append(nb.h32, b.h32[i])
		}
		for j < len(idx) && int(tl.tel[idx[j]]) == t {
			x := te(idx[j])
			nb.g, nb.cat, nb.rate, nb.price, nb.h32 = append(nb.g, x.g), append(nb.cat, tl.cat[idx[j]]), append(nb.rate, x.rate), append(nb.price, x.price), append(nb.h32, x.h32)
			j++
		}
	}
	nb.seg[3] = len(nb.g)
	return nb
}

// filter-major (category, price, number) arrays: used only to measure the merge cost of the other layout.
type fmBase struct {
	n              int
	g              []uint32
	cat, tel, rate []uint8
	price          []int32
}

func buildFM(c *cols) *fmBase {
	pk := make([]uint64, c.n)
	for i := range pk {
		pk[i] = uint64(c.cat[i])<<56 | uint64(c.price[i])<<32 | uint64(c.g[i])
	}
	slices.Sort(pk)
	f := &fmBase{n: c.n, g: make([]uint32, c.n), cat: make([]uint8, c.n), tel: make([]uint8, c.n), rate: make([]uint8, c.n), price: make([]int32, c.n)}
	for i, k := range pk {
		g := uint32(k)
		f.g[i], f.cat[i], f.tel[i], f.rate[i], f.price[i] = g, c.cat[g], c.tel[g], c.rate[g], c.price[g]
	}
	return f
}

func mergeFM(f *fmBase, touched []uint64, tl *tailT) *fmBase {
	idx := make([]int32, len(tl.g))
	for i := range idx {
		idx[i] = int32(i)
	}
	key := func(i int32) uint64 { return uint64(tl.cat[i])<<56 | uint64(tl.price[i])<<32 | uint64(tl.g[i]) }
	sort.Slice(idx, func(a, b int) bool { return key(idx[a]) < key(idx[b]) })
	nf := &fmBase{g: make([]uint32, 0, f.n+len(idx)), cat: make([]uint8, 0, f.n+len(idx)), tel: make([]uint8, 0, f.n+len(idx)), rate: make([]uint8, 0, f.n+len(idx)), price: make([]int32, 0, f.n+len(idx))}
	putT := func(i int32) {
		nf.g, nf.cat, nf.tel, nf.rate, nf.price = append(nf.g, tl.g[i]), append(nf.cat, tl.cat[i]), append(nf.tel, tl.tel[i]), append(nf.rate, tl.rate[i]), append(nf.price, tl.price[i])
	}
	j := 0
	for i := 0; i < f.n; i++ {
		g := f.g[i]
		if touched[g>>6]&(1<<(g&63)) != 0 {
			continue
		}
		k := uint64(f.cat[i])<<56 | uint64(f.price[i])<<32 | uint64(g)
		for j < len(idx) && key(idx[j]) < k {
			putT(idx[j])
			j++
		}
		nf.g, nf.cat, nf.tel, nf.rate, nf.price = append(nf.g, g), append(nf.cat, f.cat[i]), append(nf.tel, f.tel[i]), append(nf.rate, f.rate[i]), append(nf.price, f.price[i])
	}
	for ; j < len(idx); j++ {
		putT(idx[j])
	}
	nf.n = len(nf.g)
	return nf
}

func h2merge(n int) {
	reps := min(*fH2Reps, 50)
	c := genAll(n, 0, 8)
	b := buildRank(c, 8, nil)
	b.buildBitmaps(8)
	t := time.Now()
	fm := buildFM(c)
	line("build", "filter-major sort (category,price,number) of arrays, 1 thread: %s (rank-major: see bench part)", ms(time.Since(t)))
	m0 := heapMB()
	t = time.Now()
	pos := buildPos(b)
	line("merge", "key->base position directory (dense ids, 4 B/row) build %s", ms(time.Since(t)))
	for _, K := range []int{1000, 100000, 1000000} {
		ops := genDelta(n, K, 0)
		last := lastOps(ops)
		line("merge", "---- K=%d ops (%d distinct keys; 45%% update, 10%% delete, 45%% insert)", K, len(last))
		mb := heapMB()
		t = time.Now()
		ds := applyDelta(b, ops, pos)
		ta := time.Since(t)
		line("merge", "cfg2/3 apply: dead bitmap + tail (%d live tail rows) %s, heap +%.1f MB", len(ds.tail.g), ms(ta), heapMB()-mb)
		x := &cfgs{c, b, nil}
		_ = x
		// correctness with the unmerged tail
		for _, p := range pagesChk {
			q := canon(p, 1)
			want := refR2(n, 0, last, &q)
			for _, bm := range []bool{false, true} {
				got, _ := b.R2rank(&q, ds, bm)
				if d := sameOut(want, got); d != "" {
					failf("merge K=%d unmerged tail page %d bm=%v: %s", K, p, bm, d)
				}
			}
		}
		line("merge", "unmerged tail K=%d: cfg2 and cfg3 pages 1,2,3,50 == reference", K)
		for _, p := range []int{1, 50} {
			for _, th := range []int{1, 8} {
				q := canon(p, th)
				for _, bm := range []bool{false, true} {
					var s qstat
					f := func() { _, s = b.R2rank(&q, ds, bm) }
					f()
					p50, p99 := timeIt(reps, f)
					name := "cfg2-rank-lazy"
					if bm {
						name = "cfg3-rank-bitmap"
					}
					line("merge", "unmerged-tail K=%d %s page=%d thr=%d inspected=%d (tail rows scanned %d) p50=%s p99=%s", K, name, p, th, s.total(), s.tailN, dur(p50), dur(p99))
				}
			}
		}
		// merge: only the tail is sorted, then one linear pass over the base
		touched := bitsetOf(n, ops)
		a0 := allocMB()
		t = time.Now()
		nb := mergeRank(b, touched, ds)
		tm := time.Since(t)
		a1 := allocMB()
		bt := nb.buildBitmaps(8)
		line("merge", "cfg2 merge K=%d: sort tail + linear merge of %d rows %s, allocated %.0f MB; cfg3 bitmaps rebuild %s", K, nb.n, ms(tm), a1-a0, ms(bt))
		for _, p := range pagesChk {
			q := canon(p, 1)
			want := refR2(n, 0, last, &q)
			for _, bm := range []bool{false, true} {
				got, _ := nb.R2rank(&q, nil, bm)
				if d := sameOut(want, got); d != "" {
					failf("merge K=%d merged base page %d bm=%v: %s", K, p, bm, d)
				}
			}
		}
		line("merge", "merged base K=%d: cfg2 and cfg3 pages 1,2,3,50 == reference", K)
		a0 = allocMB()
		t = time.Now()
		nf := mergeFM(fm, touched, &ds.tail)
		line("merge", "cfg1 filter-major merge K=%d (same tail, order (category,price,number)): %s for %d rows, allocated %.0f MB", K, ms(time.Since(t)), nf.n, allocMB()-a0)
		nb, nf = nil, nil
		// cfg1 existing engine path: tail in the State
		w1 := cfg1Writer(c)
		t = time.Now()
		applyCfg1(w1, ops)
		line("merge", "cfg1 apply+publish K=%d ops (existing writer) %s", K, ms(time.Since(t)))
		for _, p := range []int{1, 50} {
			for _, th := range []int{1, 8} {
				q := canon(p, th)
				var s R2Stats
				f := func() { _, s = cfg1Query(w1, &q) }
				f()
				p50, p99 := timeIt(reps, f)
				line("merge", "unmerged-tail K=%d cfg1-filter-major page=%d thr=%d inspected=%d p50=%s p99=%s", K, p, th, s.ScannedBase+s.ScannedTail, dur(p50), dur(p99))
			}
		}
		w1 = nil
		runtime.GC()
	}
	_ = m0
}
