package planner

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/proj"
	"stask/internal/schema"
)

func start(t *testing.T) (*Platform, func()) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 8 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return Start(e), func() { e.Close() }
}

func salesTable() schema.Table {
	return schema.Table{Name: "sales", Key: "id", Columnar: true, Columns: []schema.Column{
		{Name: "id", Type: "int"}, {Name: "region", Type: "string"}, {Name: "qty", Type: "int"},
		{Name: "price", Type: "float"}, {Name: "note", Type: "text"},
	}}
}

func round(res *proj.Result) [][]any {
	out := make([][]any, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = make([]any, len(r))
		for j, v := range r {
			if f, ok := v.(float64); ok {
				v = math.Round(f*1e6) / 1e6
			}
			out[i][j] = v
		}
	}
	return out
}

// The columnar view must give the same answer as the row-wise evaluation
// over the authoritative core, after inserts, updates and deletes.
func TestColumnarMatchesCore(t *testing.T) {
	p, stop := start(t)
	defer stop()
	if _, err := p.E.CreateTable(salesTable()); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	regions := []string{"north", "south", "east", "west"}
	var lsn uint64
	for i := 0; i < 3000; i++ {
		var ops []core.WriteOp
		for j := 0; j < 10; j++ {
			id := int64(rng.Intn(20000))
			if rng.Intn(10) == 0 {
				ops = append(ops, core.WriteOp{Table: "sales", Key: fmt.Sprint(id)})
				continue
			}
			r := schema.Row{{I: id}, {S: regions[rng.Intn(4)]}, {I: int64(rng.Intn(100))}, {F: rng.Float64() * 50}, {S: "x"}}
			if rng.Intn(20) == 0 {
				r[3].Null = true
			}
			ops = append(ops, core.WriteOp{Table: "sales", Row: r})
		}
		var err error
		if lsn, err = p.E.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	queries := []proj.AggQuery{
		{Table: "sales", Aggs: []proj.Agg{{Fn: "count"}, {Fn: "sum", Col: "qty"}, {Fn: "avg", Col: "price"}, {Fn: "max", Col: "price"}}},
		{Table: "sales", GroupBy: []string{"region"}, Aggs: []proj.Agg{{Fn: "count"}, {Fn: "sum", Col: "qty"}, {Fn: "min", Col: "price"}}},
		{Table: "sales", GroupBy: []string{"region", "qty"}, Filters: []proj.Filter{{Col: "qty", Op: ">=", Value: 90}}, Aggs: []proj.Agg{{Fn: "count"}}},
		{Table: "sales", Filters: []proj.Filter{{Col: "region", Op: "=", Value: "east"}, {Col: "price", Op: "<", Value: 10.5}}, Aggs: []proj.Agg{{Fn: "sum", Col: "price"}}},
		{Table: "sales", Filters: []proj.Filter{{Col: "region", Op: "=", Value: "nowhere"}}, Aggs: []proj.Agg{{Fn: "count"}}},
	}
	for i, q := range queries {
		q := q
		view, pl, err := p.Aggregate(&q, Consistency{Mode: "session", MinLSN: lsn})
		if err != nil || pl.Engine != "columnar" {
			t.Fatalf("q%d view: %v %+v", i, err, pl)
		}
		ra, _ := proj.NewRowAggregator(mustTable(t, p, "sales"), &q)
		p.E.ScanSnapshot("sales", p.E.DurableLSN(), func(_ string, r schema.Row, _ *core.Version) bool { ra.Add(r); return true })
		want := ra.Result()
		if !reflect.DeepEqual(round(view), round(want)) {
			t.Fatalf("q%d mismatch\nview=%v\ncore=%v", i, view.Rows, want.Rows)
		}
	}
}

func mustTable(t *testing.T, p *Platform, name string) *schema.Table {
	ts, err := p.E.Table(name)
	if err != nil {
		t.Fatal(err)
	}
	return ts.Schema()
}

func TestSearchVietnameseAndFuzzy(t *testing.T) {
	p, stop := start(t)
	defer stop()
	p.E.CreateTable(schema.Table{Name: "docs", Key: "id", Search: []string{"title", "body"}, Columns: []schema.Column{
		{Name: "id", Type: "string"}, {Name: "title", Type: "text"}, {Name: "body", Type: "text"}}})
	put := func(id, title, body string) uint64 {
		lsn, err := p.E.Write([]core.WriteOp{{Table: "docs", Row: schema.Row{{S: id}, {S: title}, {S: body}}}})
		if err != nil {
			t.Fatal(err)
		}
		return lsn
	}
	put("1", "Sim số đẹp Hà Nội", "Kho sim Viettel tại Hà Nội")
	put("2", "Điện thoại Samsung", "Bảo hành chính hãng")
	lsn := put("3", "Sim Mobifone", "Giao hàng toàn quốc, thanh toán khi nhận")
	c := Consistency{Mode: "session", MinLSN: lsn}

	hits, _, pl, err := p.Search(&proj.SearchQuery{Table: "docs", Query: "ha noi"}, c, true)
	if err != nil || len(hits) == 0 || hits[0].Key != "1" || pl.Engine != "search" {
		t.Fatalf("diacritics folding: %+v %v %+v", hits, err, pl)
	}
	// an accented query matches that exact word only: "hạ" must not match "Hà"
	if hits, _, _, _ := p.Search(&proj.SearchQuery{Table: "docs", Query: "hạ"}, c, false); len(hits) != 0 {
		t.Fatalf("accented query matched a different word: %+v", hits)
	}
	if hits, _, _, _ := p.Search(&proj.SearchQuery{Table: "docs", Query: "Hà"}, c, false); len(hits) != 1 {
		t.Fatalf("exact accented query: %+v", hits)
	}
	hits, exp, _, _ := p.Search(&proj.SearchQuery{Table: "docs", Query: "samsnug", Fuzzy: true}, c, false)
	if len(hits) != 1 || hits[0].Key != "2" || exp["samsung"] != "samsnug" {
		t.Fatalf("fuzzy: %+v %v", hits, exp)
	}
	hits, _, _, _ = p.Search(&proj.SearchQuery{Table: "docs", Query: "sim viettel", All: true}, c, false)
	if len(hits) != 1 || hits[0].Key != "1" {
		t.Fatalf("AND: %+v", hits)
	}
	// update: old terms must disappear, new appear
	lsn = put("1", "Sim thường", "không còn")
	hits, _, _, _ = p.Search(&proj.SearchQuery{Table: "docs", Query: "viettel"}, Consistency{Mode: "session", MinLSN: lsn}, false)
	if len(hits) != 0 {
		t.Fatalf("stale postings: %+v", hits)
	}
	// delete
	lsn, _ = p.E.Write([]core.WriteOp{{Table: "docs", Key: "2"}})
	hits, _, _, _ = p.Search(&proj.SearchQuery{Table: "docs", Query: "samsung"}, Consistency{Mode: "session", MinLSN: lsn}, false)
	if len(hits) != 0 {
		t.Fatalf("deleted doc returned: %+v", hits)
	}
}

func TestTimeSeriesMatchesCoreAndRYW(t *testing.T) {
	p, stop := start(t)
	defer stop()
	_, err := p.E.CreateTable(schema.Table{Name: "metrics", Key: "id", AppendOnly: true, TimeColumn: "ts", SeriesColumn: "device",
		Columns: []schema.Column{{Name: "id", Type: "int"}, {Name: "device", Type: "string"}, {Name: "ts", Type: "time"},
			{Name: "temp", Type: "float"}, {Name: "count", Type: "int"}}})
	if err != nil {
		t.Fatal(err)
	}
	base := int64(1_700_000_000_000)
	var lsn uint64
	id := int64(0)
	for i := 0; i < 400; i++ {
		var ops []core.WriteOp
		for d := 0; d < 50; d++ {
			id++
			ops = append(ops, core.WriteOp{Table: "metrics", Row: schema.Row{{I: id}, {S: fmt.Sprintf("dev-%d", d)},
				{I: base + int64(i)*1000 + int64(d)}, {F: 20 + float64(i%17)/4}, {I: int64(d)}}})
		}
		lsn, _ = p.E.Write(ops)
	}
	if _, err := p.E.Write([]core.WriteOp{{Table: "metrics", Key: "1"}}); err == nil {
		t.Fatal("delete on append-only table must fail")
	}
	q := proj.TSQuery{Table: "metrics", From: base + 50_000, To: base + 350_000, BucketMs: 60_000, BySeries: true,
		Aggs: []proj.Agg{{Fn: "count"}, {Fn: "avg", Col: "temp"}, {Fn: "max", Col: "count"}}}
	view, pl, err := p.TimeSeries(&q, Consistency{MinLSN: lsn})
	if err != nil || pl.Engine != "timeseries" || pl.Mode != "session" {
		t.Fatalf("view: %v %+v", err, pl)
	}
	core, pl2, err := p.TimeSeries(&q, Consistency{Mode: "session", MinLSN: lsn + 1000, TimeoutMs: 10})
	if err != nil || pl2.Engine != "core" {
		t.Fatalf("expected core fallback for an unreachable token: %v %+v", err, pl2)
	}
	if !reflect.DeepEqual(round(view), round(core)) || len(view.Rows) != 5*50 {
		t.Fatalf("mismatch: %d vs %d rows\n%v\n%v", len(view.Rows), len(core.Rows), view.Rows[:3], core.Rows[:3])
	}
	st := p.TS.Stats(mustTable(t, p, "metrics").ID)
	t.Logf("timeseries stats %+v", st)
	if st.Rows != 20000 || st.Blocks != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestBoundedFallsBackWhenStale(t *testing.T) {
	p, stop := start(t)
	defer stop()
	p.E.CreateTable(salesTable())
	lsn, _ := p.E.Write([]core.WriteOp{{Table: "sales", Row: schema.Row{{I: 1}, {S: "n"}, {I: 1}, {F: 1}, {S: ""}}}})
	p.ColF.WaitApplied(lsn, time.Second)
	q := proj.AggQuery{Table: "sales", Aggs: []proj.Agg{{Fn: "count"}}}
	_, pl, _ := p.Aggregate(&q, Consistency{Mode: "bounded", MaxLagMs: 1000})
	if pl.Engine != "columnar" {
		t.Fatalf("%+v", pl)
	}
	if _, _, err := p.Aggregate(&q, Consistency{Mode: "bogus"}); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
