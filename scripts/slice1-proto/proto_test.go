package main

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
)

func randTxn(rng *rand.Rand, lsn uint64, n int, maxG int64) Txn {
	tx := Txn{LSN: lsn}
	for i := 0; i < n; i++ {
		g := rng.Int64N(maxG)
		r := genRow(g)
		switch rng.IntN(10) {
		case 0:
			tx.Ops = append(tx.Ops, Op{Kind: OpDel, Row: r})
		default:
			r.Price = int32(500000 + rng.IntN(4000000))
			r.Rate = uint8(5 + rng.IntN(6))
			r.Cat = uint8(rng.IntN(5))
			r.Tel = uint8(rng.IntN(3))
			tx.Ops = append(tx.Ops, Op{Kind: OpPut, Row: r})
		}
	}
	if rng.IntN(20) == 0 {
		d := Dim{car: [3]Carrier{{"A", true}, {"B", rng.IntN(3) != 0}, {fmt.Sprint("C", lsn), true}}}
		tx.Ops = append(tx.Ops, Op{Kind: OpDim, Dim: &d})
	}
	return tx
}

func TestRandomAgainstModel(t *testing.T) {
	for _, sorted := range []bool{false, true} {
		rng := rand.New(rand.NewPCG(7, uint64(len2(sorted))))
		const N = 300000
		rows := make([]Row, N)
		for g := range rows {
			rows[g] = genRow(int64(g))
		}
		dim := Dim{car: [3]Carrier{{"A", true}, {"B", true}, {"C", true}}}
		w := NewWriter(buildBase(rows, sorted, 0), &dim)
		m := NewModel(rows, dim)
		hist := map[uint64]*Model{}
		var lsn uint64
		merges := 0
		for round := 0; round < 60; round++ {
			for k := 0; k < 5; k++ {
				lsn++
				tx := randTxn(rng, lsn, 1+rng.IntN(3000), N+2000)
				w.Apply(tx)
				m.Apply(tx)
			}
			w.Publish()
			hist[lsn] = m.Clone()
			if round%9 == 4 {
				for !w.merging.Load() {
					w.StartMerge(sorted)
				}
			}
			for q := 0; q < 2; q++ {
				for _, workers := range []int{1, 4} {
					for page := 1; page <= 3; page++ {
						qq := R2Q{Cat: uint8(rng.IntN(5)), Lo: 1000000, Hi: 3000000, Page: page, Quota: [3]int{20, 50, 30}, Workers: workers}
						got, _ := w.Current().R2(qq)
						want := m.R2(qq)
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("sorted=%v round %d page %d: mismatch\n got %v\nwant %v", sorted, round, page, got[:min(3, len(got))], want[:min(3, len(want))])
						}
					}
				}
			}
			if w.merging.Load() && w.mergeRes.Load() != nil {
				merges++
			}
		}
		// drain a last merge and check again
		for w.merging.Load() {
			if w.mergeRes.Load() != nil {
				lsn++
				tx := randTxn(rng, lsn, 100, N)
				w.Apply(tx)
				m.Apply(tx)
				w.Publish()
			}
		}
		qq := R2Q{Cat: 0, Lo: 1000000, Hi: 3000000, Page: 1, Quota: [3]int{20, 50, 30}, Workers: 2}
		got, _ := w.Current().R2(qq)
		if !reflect.DeepEqual(got, m.R2(qq)) {
			t.Fatalf("after merge mismatch")
		}
		t.Logf("sorted=%v ok; merges=%d published=%d", sorted, w.Stats.Merges, w.Stats.Publishes)
	}
}

func len2(b bool) int {
	if b {
		return 1
	}
	return 0
}
