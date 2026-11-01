package proj

import (
	"fmt"
	"math/rand"
	"testing"

	"stask/internal/core"
	"stask/internal/schema"
)

// The rank path against the scan operator (itself checked against a brute force over the core in
// TestSegmentStateMatchesCore): same rows, same ranks, over data that makes an early stop fail if it is wrong:
// partitions with fewer qualifying rows than the quota, ties on the score (6 values), rows changed after the base
// (hidden base rows, a tail), deep pages, rare filters, a dictionary too large for exact bitmaps.

func rankRow(rng *rand.Rand, key string, ncat int) schema.Row {
	cat := segCats[rng.Intn(5)]
	if ncat > 5 {
		cat = fmt.Sprintf("X%02d", rng.Intn(ncat)) // many codes: no exact text bitmap for this column
		if rng.Intn(8) == 0 {
			cat = "TAM_HOA"
		}
	}
	tel := segTels[rng.Intn(3)]
	if rng.Intn(40) == 0 {
		tel = "C" // keep C rare in the cheap way below; the weights are fixed per test by the caller
	}
	return schema.Row{{S: key}, {S: cat}, {I: int64(500000 + rng.Intn(4000000))}, {S: tel}, {I: int64(5 + rng.Intn(6))}}
}

func rankQuery(tb *SegTbl, rng *rand.Rand, cat string, lo, hi int64, tels []string, page int) *SegR2 {
	ci, _, _ := tb.Col("category")
	pi, _, _ := tb.Col("price")
	ti, _, _ := tb.Col("tel")
	si, _, _ := tb.Col("store_rate")
	q := &SegR2{
		Filters: []SegFilter{{Col: ci, Text: &SegTextPred{Vals: []string{cat}}}, {Col: pi, Int: &SegIntPred{Lo: lo, Hi: hi}}, {Col: ti, Text: &SegTextPred{Vals: tels}}},
		Group:   ti, Score: si, Desc: true, Md5: true, Md5Suffix: RankSeedCanonical, Workers: 2,
	}
	for gi, tl := range segTels {
		quota := []int{20, 50, 30}[gi]
		q.Groups = append(q.Groups, SegGroup{Value: tl, Ranges: []SegRange{{(page-1)*quota + 1, page * quota}}})
	}
	return q
}

func TestRankMatchesScan(t *testing.T) {
	oldMin, oldFrac, oldSync := SegMergeMinTail, SegMergeFrac, SegRankSync
	SegMergeMinTail, SegMergeFrac, SegRankSync = 400, 3, true
	defer func() { SegMergeMinTail, SegMergeFrac, SegRankSync = oldMin, oldFrac, oldSync }()
	for _, ncat := range []int{5, 40} {
		t.Run(fmt.Sprintf("ncat%d", ncat), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(ncat)))
			e, c := segTestEngine(t)
			keyName := func(i int) string { return fmt.Sprintf("09%09d", i) }
			keys := 0
			var last uint64
			for b := 0; b < 6; b++ {
				tx := e.Begin(core.SnapshotIsolation)
				for i := 0; i < 2000; i++ {
					if err := tx.Put("sims", rankRow(rng, keyName(keys), ncat)); err != nil {
						t.Fatal(err)
					}
					keys++
				}
				l, err := tx.Commit()
				if err != nil {
					t.Fatal(err)
				}
				last = l
			}
			waitSeg(t, c, last)
			used, short := 0, 0
			for round := 0; round < 60; round++ {
				tx := e.Begin(core.SnapshotIsolation)
				for i, n := 0, 1+rng.Intn(80); i < n; i++ {
					k := keyName(rng.Intn(keys))
					switch rng.Intn(5) {
					case 0:
						k = keyName(keys)
						keys++
						_ = tx.Put("sims", rankRow(rng, k, ncat))
					case 1:
						_ = tx.Delete("sims", k)
					default:
						_ = tx.Put("sims", rankRow(rng, k, ncat))
					}
				}
				l, err := tx.Commit()
				if err != nil {
					t.Fatal(err)
				}
				waitSeg(t, c, l)
				tb := c.Current().Tbls[mustTableID(t, e)]
				for _, page := range []int{1, 2, 3, 7, 40} {
					cat := "TAM_HOA"
					lo := int64(500000 + rng.Intn(2000000))
					hi := lo + int64(1000+rng.Intn(3000000))
					if rng.Intn(4) == 0 {
						hi = lo + int64(rng.Intn(20000)) // rare filter
					}
					tels := []string{"A", "B", "C"}
					if rng.Intn(3) == 0 {
						tels = []string{"C"}
					}
					q := rankQuery(tb, rng, cat, lo, hi, tels, page)
					scan, _, err := tb.TopNPerGroup(q)
					if err != nil {
						t.Fatal(err)
					}
					q.Rank = true
					got, st, err := tb.TopNPerGroup(q)
					if err != nil {
						t.Fatal(err)
					}
					if !st.RankUsed {
						t.Fatalf("rank path not used: %s", st.RankWhy)
					}
					used++
					if hitsString(got) != hitsString(scan) {
						t.Fatalf("round %d page %d cat=%s [%d,%d] tels=%v (base %d tail %d hidden %d):\n rank %s\n scan %s", round, page, cat, lo, hi, tels, tb.Base.n, tb.Tail.n, tb.NChanged, hitsString(got), hitsString(scan))
					}
					if len(scan) < len(segTels)*page*10 {
						short++ // a partition ran out of rows before its quota
					}
				}
			}
			if c.Merges.Load() == 0 {
				t.Fatal("no merge")
			}
			if used == 0 || short == 0 {
				t.Fatalf("used=%d short=%d: the test did not cover the cases", used, short)
			}
		})
	}
}

func mustTableID(t *testing.T, e *core.Engine) uint32 {
	t.Helper()
	ts, err := e.Table("sims")
	if err != nil {
		t.Fatal(err)
	}
	return ts.Schema().ID
}

// A tail above the limit, another seed, no md5, a key range: the scan operator answers, with a reason.
func TestRankDeclinesWithReason(t *testing.T) {
	oldSync, oldTail := SegRankSync, SegRankTailMax
	SegRankSync = true
	defer func() { SegRankSync, SegRankTailMax = oldSync, oldTail }()
	rng := rand.New(rand.NewSource(9))
	e, c := segTestEngine(t)
	tx := e.Begin(core.SnapshotIsolation)
	for i := 0; i < 3000; i++ {
		_ = tx.Put("sims", rankRow(rng, fmt.Sprintf("09%09d", i), 5))
	}
	l, _ := tx.Commit()
	waitSeg(t, c, l)
	tx = e.Begin(core.SnapshotIsolation)
	for i := 0; i < 50; i++ {
		_ = tx.Put("sims", rankRow(rng, fmt.Sprintf("09%09d", i), 5))
	}
	l, _ = tx.Commit()
	waitSeg(t, c, l)
	tb := c.Current().Tbls[mustTableID(t, e)]
	if tb.Tail.n == 0 {
		t.Fatal("no tail")
	}
	run := func(mod func(*SegR2)) SegR2Stats {
		q := rankQuery(tb, rng, "TAM_HOA", 500000, 4000000, segTels, 1)
		q.Rank = true
		if mod != nil {
			mod(q)
		}
		_, st, err := tb.TopNPerGroup(q)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := run(nil); !st.RankUsed {
		t.Fatalf("canonical query: %s", st.RankWhy)
	}
	for name, mod := range map[string]func(*SegR2){
		"seed":     func(q *SegR2) { q.Md5Suffix = "7" },
		"no md5":   func(q *SegR2) { q.Md5 = false },
		"key lo":   func(q *SegR2) { q.HasLo, q.KeyLo = true, "09000000100" },
		"tail max": func(q *SegR2) { SegRankTailMax = 10 },
	} {
		SegRankTailMax = 1 << 17
		st := run(mod)
		if st.RankUsed || st.RankWhy == "" {
			t.Fatalf("%s: used=%v why=%q", name, st.RankUsed, st.RankWhy)
		}
	}
}
