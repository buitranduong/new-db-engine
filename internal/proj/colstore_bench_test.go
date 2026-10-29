package proj

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// TestViewBench measures the saved view on an `orders` table shaped like
// docs/07 §1 (id ascending, customer_id random, amount random float, status
// of 4 values, note free text, created every second). Run with
// STASK_VIEWBENCH=5000000 go test -run TestViewBench -v ./internal/proj
func TestViewBench(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("STASK_VIEWBENCH"))
	if n == 0 {
		t.Skip("set STASK_VIEWBENCH=<rows>")
	}
	dir := t.TempDir()
	e, err := core.Open(core.Options{Dir: dir, NoCheckpoint: true, Logf: func(string, ...any) {}, ViewSaveEvery: -1, NoViewFile: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	tb := schema.Table{Name: "orders", Key: "id", Columnar: true, Columns: []schema.Column{
		{Name: "id", Type: "int"}, {Name: "customer_id", Type: "int"}, {Name: "amount", Type: "float"},
		{Name: "status", Type: "text"}, {Name: "note", Type: "text"}, {Name: "created", Type: "time"},
	}}
	if _, err := e.CreateTable(tb); err != nil {
		t.Fatal(err)
	}
	ts, _ := e.Table("orders")
	c := NewColumnar()
	ct := c.ensure(ts.Schema())
	statuses := []string{"new", "paid", "shipped", "cancelled"}
	t0 := time.Now()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	for i := 0; i < n; i++ {
		r := rand.New(rand.NewPCG(uint64(i), 42))
		note := fmt.Sprintf("Đơn hàng số %d của khách %d, giao tới quận %d", i, r.IntN(1_000_000), r.IntN(24)+1)
		ct.put(strconv.Itoa(i), schema.Row{{I: int64(i)}, {I: int64(r.IntN(1_000_000))}, {F: math.Round(r.Float64()*1e6) / 100},
			{S: statuses[r.IntN(4)]}, {S: note}, {I: base + int64(i)*1000}})
	}
	t.Logf("build %d rows: %v (note stored: %v)", n, time.Since(t0), ct.cols[4].stored)
	t.Logf("memory of the view: %.1f MB", float64(c.MemBytes(ts.Schema().ID))/1e6)
	sizes := map[string]int{}
	viewStat = func(tbl, col string, b int) { sizes[col] = b }
	defer func() { viewStat = nil }()
	t0 = time.Now()
	if err := c.Save(e, 1, e.Log().WritePos()); err != nil {
		t.Fatal(err)
	}
	t.Logf("save: %v", time.Since(t0))
	fi, _ := os.Stat(filepath.Join(dir, viewFile))
	t.Logf("file: %.2f MB (%.2f bytes/row)", float64(fi.Size())/1e6, float64(fi.Size())/float64(n))
	for _, col := range []string{"id", "customer_id", "amount", "status", "note", "created"} {
		t.Logf("  %-12s %10.1f KB", col, float64(sizes[col])/1e3)
	}
	queries := []AggQuery{
		{Table: "orders", GroupBy: []string{"status"}, Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "amount"}}},
		{Table: "orders", Aggs: []Agg{{Fn: "sum", Col: "customer_id"}, {Fn: "avg", Col: "amount"}, {Fn: "max", Col: "created"}}},
		{Table: "orders", Filters: []Filter{{Col: "amount", Op: ">", Value: 9000.0}}, GroupBy: []string{"status"}, Aggs: []Agg{{Fn: "count"}}},
	}
	run := func(c *Columnar, label string) [][][]any {
		var out [][][]any
		for i := range queries {
			best := time.Hour
			var res *Result
			for k := 0; k < 7; k++ {
				q := queries[i]
				t1 := time.Now()
				res, err = c.Aggregate(ts.Schema().ID, &q)
				if err != nil {
					t.Fatal(err)
				}
				best = min(best, time.Since(t1))
			}
			t.Logf("%s q%d best of 7: %v", label, i, best)
			rows := res.Rows
			out = append(out, rows)
		}
		return out
	}
	before := run(c, "built ")
	c2 := NewColumnar()
	t0 = time.Now()
	if err := c2.Load(e); err != nil {
		t.Fatal(err)
	}
	t.Logf("load: %v", time.Since(t0))
	after := run(c2, "loaded")
	for i := range before {
		if len(before[i]) != len(after[i]) {
			t.Fatalf("q%d row count differs", i)
		}
		// group order is not defined: compare as sets of formatted rows
		a, b := map[string]bool{}, map[string]bool{}
		for j := range before[i] {
			a[fmtRow(before[i][j])], b[fmtRow(after[i][j])] = true, true
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("q%d differs:\n%v\n%v", i, before[i], after[i])
		}
	}
}

// fmtRow formats floats to 9 significant digits: parallel partial sums
// add in another order and differ in the last bits.
func fmtRow(r []any) string {
	s := ""
	for _, x := range r {
		if f, ok := x.(float64); ok {
			s += fmt.Sprintf("%.9g|", f)
		} else {
			s += fmt.Sprint(x, "|")
		}
	}
	return s
}
