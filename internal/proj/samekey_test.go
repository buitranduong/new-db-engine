package proj

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// A batch with several operations on one key (put then delete, put then put)
// must leave the columnar view equal to the table.
func TestColumnarFollowsSameKeyBatches(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 1 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	tbl := schema.Table{Name: "m", Key: "id", Columnar: true, Columns: []schema.Column{
		{Name: "id", Type: "int"}, {Name: "v", Type: "int"},
	}}
	st, err := e.CreateTable(tbl)
	if err != nil {
		t.Fatal(err)
	}
	c := NewColumnar()
	f := e.Follow(c)
	rng := rand.New(rand.NewSource(7))
	model := map[int64]int64{}
	var last uint64
	for i := 0; i < 200; i++ {
		var ops []core.WriteOp
		for j, n := 0, 1+rng.Intn(10); j < n; j++ {
			id := int64(rng.Intn(12))
			if rng.Intn(3) == 0 {
				ops = append(ops, core.WriteOp{Table: "m", Key: fmt.Sprint(id)})
				delete(model, id)
			} else {
				v := int64(rng.Intn(1000))
				ops = append(ops, core.WriteOp{Table: "m", Row: schema.Row{{I: id}, {I: v}}})
				model[id] = v
			}
		}
		if last, err = e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	if !f.WaitApplied(last, 10*time.Second) {
		t.Fatal("follower did not catch up")
	}
	var sum int64
	for _, v := range model {
		sum += v
	}
	res, err := c.Aggregate(st.ID, &AggQuery{Table: "m", Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "v"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(res.Rows), fmt.Sprint([][]any{{float64(len(model)), float64(sum)}}); got != want {
		// numeric types of the result are not the point: compare as text
		if fmt.Sprint(res.Rows[0][0]) != fmt.Sprint(len(model)) || fmt.Sprint(res.Rows[0][1]) != fmt.Sprint(sum) {
			t.Fatalf("columnar %v, table has %d rows summing %d", res.Rows, len(model), sum)
		}
	}
}
