package proj

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// Property test of the segment state against the core (docs/31 9 item 1):
// random INSERT / UPDATE / DELETE, merges in between, 1 and 4 workers, the
// state at the published LSN and at older LSNs of the ring: the operator's
// answer equals a brute-force evaluation over a scan of the core at the same
// snapshot.

type bfRow struct {
	key, cat, tel string
	price, rate   int64
}

func segTestEngine(t *testing.T) (*core.Engine, *SegColumnar) {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), NoCheckpoint: true, Logf: func(string, ...any) {}, ViewSaveEvery: -1, NoViewFile: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	_, err = e.CreateTable(schema.Table{Name: "sims", Key: "number", Columnar: true, Columns: []schema.Column{
		{Name: "number", Type: "string"}, {Name: "category", Type: "text"}, {Name: "price", Type: "int"},
		{Name: "tel", Type: "text"}, {Name: "store_rate", Type: "int"}}})
	if err != nil {
		t.Fatal(err)
	}
	return e, StartSeg(e)
}

var segCats = []string{"TAM_HOA", "LOC_PHAT", "THAN_TAI", "PHONG_THUY", "THUONG"}
var segTels = []string{"A", "B", "C"}

func randSim(rng *rand.Rand, key string) schema.Row {
	return schema.Row{{S: key}, {S: segCats[rng.Intn(5)]}, {I: int64(500000 + rng.Intn(4000000))}, {S: segTels[rng.Intn(3)]}, {I: int64(5 + rng.Intn(6))}}
}

func bruteTop(e *core.Engine, snap uint64, q *SegR2, cat string, lo, hi int64, tels []string) ([]SegHit, error) {
	var rows []bfRow
	err := e.ScanSnapshot("sims", snap, func(key string, r schema.Row, _ *core.Version) bool {
		rows = append(rows, bfRow{key, r[1].S, r[3].S, r[2].I, r[4].I})
		return true
	})
	if err != nil {
		return nil, err
	}
	type cand struct {
		bfRow
		h string
	}
	var g = map[string][]cand{}
	for _, r := range rows {
		if r.cat != cat || r.price < lo || r.price > hi {
			continue
		}
		ok := false
		for _, t := range tels {
			ok = ok || t == r.tel
		}
		if !ok {
			continue
		}
		h := md5.Sum([]byte(r.key + q.Md5Suffix))
		g[r.tel] = append(g[r.tel], cand{r, hex.EncodeToString(h[:])})
	}
	less := func(a, b cand) bool {
		if a.rate != b.rate {
			if q.Desc {
				return a.rate > b.rate
			}
			return a.rate < b.rate
		}
		if q.Md5 && a.h != b.h {
			return a.h < b.h
		}
		return a.key < b.key
	}
	var all []cand
	var rk = map[string]int{}
	for _, gr := range q.Groups {
		c := g[gr.Value]
		sort.Slice(c, func(i, j int) bool { return less(c[i], c[j]) })
		for i := range c {
			for _, rg := range gr.Ranges {
				if i+1 >= rg.Lo && i+1 <= rg.Hi {
					all = append(all, c[i])
					rk[c[i].key] = i + 1
					break
				}
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	var out []SegHit
	for _, c := range all {
		out = append(out, SegHit{Key: c.key, Group: c.tel, Rank: rk[c.key], Score: int32(c.rate)})
	}
	return out, nil
}

func segQuery(tb *SegTbl, rng *rand.Rand, workers int) (q *SegR2, cat string, lo, hi int64, tels []string) {
	ci, _, _ := tb.Col("category")
	pi, _, _ := tb.Col("price")
	ti, _, _ := tb.Col("tel")
	si, _, _ := tb.Col("store_rate")
	cat = segCats[rng.Intn(5)]
	lo = int64(500000 + rng.Intn(2000000))
	hi = lo + int64(rng.Intn(3000000))
	for _, tl := range segTels {
		if rng.Intn(5) > 0 {
			tels = append(tels, tl)
		}
	}
	if len(tels) == 0 {
		tels = []string{"B"}
	}
	q = &SegR2{
		Filters: []SegFilter{{Col: ci, Text: &SegTextPred{Vals: []string{cat}}}, {Col: pi, Int: &SegIntPred{Lo: lo, Hi: hi}}, {Col: ti, Text: &SegTextPred{Vals: tels}}},
		Group:   ti, Score: si, Desc: rng.Intn(4) > 0, Md5: rng.Intn(5) > 0, Md5Suffix: "42", Workers: workers,
	}
	page := 1 + rng.Intn(3)
	for gi, tl := range segTels {
		quota := []int{20, 50, 30}[gi]
		g := SegGroup{Value: tl, Ranges: []SegRange{{(page-1)*quota + 1, page * quota}}}
		if rng.Intn(6) == 0 {
			g.Ranges = append(g.Ranges, SegRange{1, 3})
		}
		q.Groups = append(q.Groups, g)
	}
	return
}

func hitsString(h []SegHit) string {
	s := ""
	for _, x := range h {
		s += fmt.Sprintf("%s/%s/%d/%d ", x.Key, x.Group, x.Rank, x.Score)
	}
	return s
}

func waitSeg(t *testing.T, c *SegColumnar, lsn uint64) {
	t.Helper()
	if !c.Follower().WaitApplied(lsn, 30*time.Second) {
		t.Fatalf("segment follower did not reach lsn %d", lsn)
	}
}

func TestSegmentStateMatchesCore(t *testing.T) {
	oldMin, oldFrac := SegMergeMinTail, SegMergeFrac
	SegMergeMinTail, SegMergeFrac = 150, 4 // force merges between the rounds
	defer func() { SegMergeMinTail, SegMergeFrac = oldMin, oldFrac }()
	for _, seed := range []int64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			e, c := segTestEngine(t)
			keys := 0
			keyName := func(i int) string { return fmt.Sprintf("09%09d", i) }
			// initial load, in a few transactions
			var last uint64
			for b := 0; b < 4; b++ {
				tx := e.Begin(core.SnapshotIsolation)
				for i := 0; i < 500; i++ {
					if err := tx.Put("sims", randSim(rng, keyName(keys))); err != nil {
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
			type snap struct {
				lsn uint64
			}
			var olds []snap
			for round := 0; round < 40; round++ {
				tx := e.Begin(core.SnapshotIsolation)
				nops := 1 + rng.Intn(60)
				touched := map[string]bool{}
				for i := 0; i < nops; i++ {
					var k string
					switch rng.Intn(6) {
					case 0: // insert a new key
						k = keyName(keys)
						keys++
					default:
						k = keyName(rng.Intn(keys))
					}
					if touched[k] && rng.Intn(2) == 0 {
						continue
					}
					touched[k] = true
					if rng.Intn(4) == 0 {
						_ = tx.Delete("sims", k)
					} else if err := tx.Put("sims", randSim(rng, k)); err != nil {
						t.Fatal(err)
					}
				}
				l, err := tx.Commit()
				if err != nil {
					t.Fatal(err)
				}
				last = l
				waitSeg(t, c, last)
				olds = append(olds, snap{last})
				check := func(S uint64, workers int) {
					st := c.StateAt(S)
					if st == nil {
						return // older than the ring
					}
					ts, _ := e.Table("sims")
					tb := st.Tbls[ts.Schema().ID]
					if tb == nil {
						t.Fatalf("no table in state at %d", st.LSN)
					}
					q, cat, lo, hi, tels := segQuery(tb, rng, workers)
					got, _, err := tb.TopNPerGroup(q)
					if err != nil {
						t.Fatalf("round %d S=%d: %v", round, S, err)
					}
					want, err := bruteTop(e, st.LSN, q, cat, lo, hi, tels)
					if err != nil {
						return // snapshot no longer readable in the core
					}
					if hitsString(got) != hitsString(want) {
						t.Fatalf("round %d S=%d (state %d) workers=%d cat=%s [%d,%d] tels=%v desc=%v md5=%v:\n got  %s\n want %s", round, S, st.LSN, workers, cat, lo, hi, tels, q.Desc, q.Md5, hitsString(got), hitsString(want))
					}
				}
				check(last, 1)
				check(last, 4)
				if len(olds) > 3 {
					check(olds[rng.Intn(len(olds)-1)].lsn, 2) // an older LSN still in the ring
				}
			}
			if c.Merges.Load() == 0 {
				t.Fatal("no merge happened: the test did not exercise the swap")
			}
		})
	}
}
