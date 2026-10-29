package proj

import (
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"

	"stask/internal/schema"
)

// The columnar aggregate (dense, many-key dense and hash paths) must give
// the row aggregator's answer, with NULL keys, deletes and updates.
func TestColumnarAggregateMatchesRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(rng *rand.Rand) int64
	}{
		{"few-keys", func(rng *rand.Rand) int64 { return int64(rng.Intn(50)) - 20 }},
		{"many-keys", func(rng *rand.Rand) int64 { return int64(rng.Intn(60000)) + 1_000_000 }},
		{"sparse-keys", func(rng *rand.Rand) int64 { return int64(rng.Intn(3000)) * 1_000_000_007 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := &schema.Table{ID: 1, Name: "t", Key: "id", Columnar: true, Columns: []schema.Column{
				{Name: "id", Type: "int"}, {Name: "g", Type: "int"}, {Name: "c", Type: "string"},
				{Name: "b", Type: "bool"}, {Name: "v", Type: "float"}, {Name: "i", Type: "int"},
				{Name: "ts", Type: "time"},
			}}
			if err := tbl.Init(); err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewSource(3))
			c := NewColumnar()
			ct := c.ensure(tbl)
			rows := map[string]schema.Row{}
			const n = 150_000 // three chunks
			for i := 0; i < n+40_000; i++ {
				id := int64(rng.Intn(n))
				if rng.Intn(10) == 0 {
					ct.del(fmt.Sprint(id))
					delete(rows, fmt.Sprint(id))
					continue
				}
				r := schema.Row{{I: id}, {I: tc.key(rng)}, {S: fmt.Sprint("c", rng.Intn(7))},
					{I: int64(rng.Intn(2))}, {F: float64(rng.Intn(4000)) / 4}, {I: int64(rng.Intn(1000)) - 500},
					{I: 1_790_000_000_000 + rng.Int63n(400*msDay) - 200*msDay}} // a year and more around 2026
				for col := 1; col < len(r); col++ {
					if rng.Intn(40) == 0 {
						r[col] = schema.Value{Null: true}
					}
				}
				ct.put(fmt.Sprint(id), r)
				rows[fmt.Sprint(id)] = r
			}
			aggs := []Agg{{Fn: "count"}, {Fn: "sum", Col: "v", As: "s"}, {Fn: "min", Col: "i"},
				{Fn: "max", Col: "i"}, {Fn: "avg", Col: "v"}, {Fn: "count", Col: "i"}}
			for _, q := range []AggQuery{
				{GroupBy: []string{"g"}},
				{GroupBy: []string{"g"}, OrderBy: "s", Desc: true, Limit: 10},
				{GroupBy: []string{"g"}, Filters: []Filter{{Col: "v", Op: ">", Value: 700.0}}},
				{GroupBy: []string{"c"}},
				{GroupBy: []string{"b"}, Filters: []Filter{{Col: "c", Op: "=", Value: "c3"}}},
				{GroupBy: []string{"g", "c"}, OrderBy: "s", Limit: 25},
				{},
				{GroupBy: []string{"g"}, Filters: []Filter{{Col: "v", Op: ">", Value: 1e9}}},
				{GroupBy: []string{"c"}, Filters: []Filter{{Op: "or", Any: [][]Filter{
					{{Col: "c", Op: "in", Value: []any{"c1", "c4"}}, {Col: "i", Op: ">", Value: 0.0}},
					{{Col: "v", Op: "is null"}},
					{{Col: "g", Op: "not in", Value: []any{1.0, 2.0, 3.0}}, {Col: "b", Op: "=", Value: true}},
				}}}},
				{GroupBy: []string{"b"}, Filters: []Filter{{Col: "c", Op: "not in", Value: []any{"c0", "c2"}}}},
				{GroupBy: []string{"ts"}, GroupTrunc: []string{"day"}, OrderBy: "s", Desc: true, Limit: 7},
				{GroupBy: []string{"ts", "c"}, GroupTrunc: []string{"month"}},
				{GroupBy: []string{"c", "ts"}, GroupTrunc: []string{"", "week"}, Filters: []Filter{{Col: "v", Op: "<", Value: 300.0}}},
				{GroupBy: []string{"ts"}, GroupTrunc: []string{"hour"}},
			} {
				for _, variant := range []int{0, 1, 2, 3} {
					limited := variant == 1
					q.Table, q.Aggs = "t", aggs
					switch variant { // count(DISTINCT) of an int, a string, a group column
					case 2:
						q.Aggs = append([]Agg{{Fn: "count", Col: "i", Distinct: true, As: "d"}}, aggs...)
					case 3:
						q.Aggs = []Agg{{Fn: "count", Col: "c", Distinct: true}, {Fn: "sum", Col: "v", As: "s"},
							{Fn: "count", Col: "c", Distinct: true, As: "dc2"}}
						if len(q.GroupBy) > 0 && q.GroupBy[0] == "g" {
							q.Aggs = []Agg{{Fn: "count", Col: "g", Distinct: true}, {Fn: "sum", Col: "v", As: "s"}}
						}
					}
					if limited { // as SQL calls it: some helpers refused, others told to stop
						var started atomic.Int32
						q.SQLNulls, q.Workers = true, 5
						q.Spawn = func(f func()) bool {
							if started.Add(1)%2 == 0 {
								return false
							}
							go f()
							return true
						}
						q.Keep = func(w int) bool { return w < 3 }
					}
					got, err := c.Aggregate(tbl.ID, &q)
					if err != nil {
						t.Fatal(err)
					}
					ra, err := NewRowAggregator(tbl, &q)
					if err != nil {
						t.Fatal(err)
					}
					for _, r := range rows {
						ra.Add(r)
					}
					want := ra.Result()
					if g, w := fmt.Sprint(got.Rows), fmt.Sprint(want.Rows); g != w {
						t.Fatalf("%+v: %d groups, want %d\n got %.300s\nwant %.300s", q, len(got.Rows), len(want.Rows), g, w)
					}
					if got.Scanned != int64(len(rows)) {
						t.Fatalf("scanned %d, want %d", got.Scanned, len(rows))
					}
				}
			}
		})
	}
}

// A TEXT column with few distinct values is grouped and filtered like a
// string column; once it has too many it leaves the view, and queries on it
// say so while the rest of the table keeps working.
func TestColumnarTextDictionary(t *testing.T) {
	tbl := &schema.Table{ID: 1, Name: "t", Key: "id", Columnar: true, Columns: []schema.Column{
		{Name: "id", Type: "int"}, {Name: "status", Type: "text"}, {Name: "note", Type: "text"}, {Name: "v", Type: "int"},
	}}
	if err := tbl.Init(); err != nil {
		t.Fatal(err)
	}
	c := NewColumnar()
	ct := c.ensure(tbl)
	rows := map[string]schema.Row{}
	for i := 0; i < textDictMax+5000; i++ {
		r := schema.Row{{I: int64(i)}, {S: fmt.Sprint("s", i%4)}, {S: fmt.Sprint("note ", i)}, {I: int64(i % 97)}}
		ct.put(fmt.Sprint(i), r)
		rows[fmt.Sprint(i)] = r
	}
	q := AggQuery{Table: "t", GroupBy: []string{"status"}, Filters: []Filter{{Col: "status", Op: "!=", Value: "s2"}},
		Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "v"}}}
	got, err := c.Aggregate(tbl.ID, &q)
	if err != nil {
		t.Fatal(err)
	}
	ra, err := NewRowAggregator(tbl, &q)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		ra.Add(r)
	}
	if g, w := fmt.Sprint(got.Rows), fmt.Sprint(ra.Result().Rows); g != w || len(got.Rows) != 3 {
		t.Fatalf("group by text: got %s, want %s", g, w)
	}
	q = AggQuery{Table: "t", GroupBy: []string{"note"}, Aggs: []Agg{{Fn: "count"}}}
	if _, err := c.Aggregate(tbl.ID, &q); err == nil || !strings.Contains(err.Error(), "distinct values") {
		t.Fatalf("group by free text: %v, want a not-in-view error", err)
	}
	for k := range rows { // compaction keeps the dropped column dropped
		if k != "1" {
			ct.del(k)
		}
	}
	if ct.maybeCompact(); ct.dead != 0 {
		t.Fatalf("not compacted: %d dead", ct.dead)
	}
	ct.put("x", schema.Row{{I: 1 << 40}, {S: "s9"}, {S: "x"}, {I: 1}})
	q = AggQuery{Table: "t", GroupBy: []string{"status"}, Aggs: []Agg{{Fn: "count"}}}
	if got, err = c.Aggregate(tbl.ID, &q); err != nil || fmt.Sprint(got.Rows) != "[[s1 1] [s9 1]]" {
		t.Fatalf("after deletes: %v %v", got, err)
	}
}
