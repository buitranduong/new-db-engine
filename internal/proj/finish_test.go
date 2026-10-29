package proj

import (
	"fmt"
	"maps"
	"math/rand"
	"testing"
)

// With a limit, finish selects the groups a full sort would put first.
func TestFinishTopKMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	specs := []aggSpec{{fn: "sum", col: 1, name: "s"}, {fn: "count", col: -1, name: "n"}}
	for trial := 0; trial < 200; trial++ {
		groups := map[uint64]*group{}
		n := 1 + rng.Intn(300)
		for i := 0; i < n; i++ {
			g := &group{accs: newAccs(len(specs))}
			if i == 0 && rng.Intn(2) == 0 { // at most one NULL group, as in real results
				g.vals = []any{nil}
			} else {
				g.vals = []any{int64(i)}
			}
			for j := 0; j < 1+rng.Intn(3); j++ {
				g.accs[0].addFloat(float64(rng.Intn(10))) // many ties
				g.accs[1].n++
			}
			groups[uint64(i)] = g
		}
		for _, order := range []string{"s", "n", "k", ""} {
			for _, desc := range []bool{false, true} {
				q := &AggQuery{OrderBy: order, Desc: desc}
				full := finish([]string{"k"}, specs, maps.Values(groups), len(groups), q, 0)
				lim := 1 + rng.Intn(n+2)
				q.Limit = lim
				top := finish([]string{"k"}, specs, maps.Values(groups), len(groups), q, 0)
				want := full.Rows
				if len(want) > lim {
					want = want[:lim]
				}
				if fmt.Sprint(top.Rows) != fmt.Sprint(want) {
					t.Fatalf("order %q desc %v limit %d of %d:\n got %v\nwant %v", order, desc, lim, n, top.Rows, want)
				}
			}
		}
	}
}
