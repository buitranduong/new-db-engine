package proj

import (
	"fmt"
	"sync/atomic"
	"testing"

	"stask/internal/schema"
)

// An interrupted aggregate stops after the chunk in progress (every path:
// hash, dense, DISTINCT).
func TestColumnarAggregateInterrupt(t *testing.T) {
	tbl := &schema.Table{ID: 1, Name: "t", Key: "id", Columnar: true, Columns: []schema.Column{
		{Name: "id", Type: "int"}, {Name: "g", Type: "int"}, {Name: "v", Type: "int"},
	}}
	if err := tbl.Init(); err != nil {
		t.Fatal(err)
	}
	c := NewColumnar()
	ct := c.ensure(tbl)
	const n = 8 * chunkRows
	for i := 0; i < n; i++ {
		ct.put(fmt.Sprint(i), schema.Row{{I: int64(i)}, {I: int64(i % 1000 * 7919)}, {I: int64(i % 13)}})
	}
	for _, q := range []AggQuery{
		{GroupBy: []string{"g"}}, // sparse keys: hash
		{GroupBy: []string{"v"}}, // dense
		{GroupBy: []string{"v"}, Aggs: []Agg{{Fn: "count", Col: "g", Distinct: true}}}, // DISTINCT
	} {
		q.Table, q.Workers = "t", 1
		if q.Aggs == nil {
			q.Aggs = []Agg{{Fn: "count"}}
		}
		var polls atomic.Int32
		q.Interrupted = func() bool { return polls.Add(1) > 2 }
		res, err := c.Aggregate(1, &q)
		if err != nil {
			t.Fatal(err)
		}
		if res.Scanned >= n || polls.Load() > 4 {
			t.Fatalf("%v: scanned %d of %d rows after %d polls", q.GroupBy, res.Scanned, n, polls.Load())
		}
	}
}
