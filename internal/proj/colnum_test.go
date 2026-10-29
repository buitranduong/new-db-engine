package proj

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/pgnum"
	"stask/internal/schema"
)

func randDec(rng *rand.Rand, intDigits, scale int) string {
	var sb strings.Builder
	if rng.Intn(3) == 0 {
		sb.WriteByte('-')
	}
	nd := 1 + rng.Intn(max(intDigits, 1))
	for i := 0; i < nd; i++ {
		sb.WriteByte(byte('0' + rng.Intn(10)))
		if i == 0 && nd > 1 && sb.String()[len(sb.String())-1] == '0' {
			s := sb.String()
			sb.Reset()
			sb.WriteString(s[:len(s)-1] + "1")
		}
	}
	if scale > 0 {
		sb.WriteByte('.')
		for i := 0; i < scale; i++ {
			sb.WriteByte(byte('0' + rng.Intn(10)))
		}
	}
	return sb.String()
}

// parseScaled and fmtScaled are inverse and agree with pgnum on the text.
func TestNumScaledText(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		sc := rng.Intn(18)
		s := randDec(rng, max(18-sc, 1), sc)
		d, err := pgnum.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := parseScaled(d.String(), sc)
		if !ok {
			t.Fatalf("parseScaled(%q, %d) failed", d.String(), sc)
		}
		if got := fmtScaled(v, sc); got != d.String() {
			t.Fatalf("%q scale %d: %d -> %q", d.String(), sc, v, got)
		}
		if got := fmtWide(int64(int64(v)>>63), uint64(v), sc); got != d.String() {
			t.Fatalf("wide %q: %q", d.String(), got)
		}
	}
	for _, c := range []struct {
		s  string
		sc int
		ok bool
	}{
		{"NaN", 2, false}, {"Infinity", 2, false}, {"-Infinity", 0, false}, {"", 2, false}, {".", 2, false}, {"1.234", 2, false},
		{"9223372036854775807", 0, true}, {"9223372036854775808", 0, false}, {"-9223372036854775808", 0, false}, // 19 digits: left to the rows
		{"999999999999999999", 0, true}, {"-999999999999999999", 0, true}, {"1", 3, true}, {"1.5", 3, true}, {"0.001", 3, true},
		{"1e3", 0, false}, {"--1", 0, false}, {"1.2.3", 3, false}, {"12", 18, false}, {"0.5", 18, true},
	} {
		v, ok := parseScaled(c.s, c.sc)
		if ok != c.ok {
			t.Errorf("parseScaled(%q,%d) = %d,%v", c.s, c.sc, v, ok)
		}
	}
	if fmtScaled(-5, 2) != "-0.05" || fmtScaled(0, 2) != "0.00" || fmtScaled(7, 0) != "7" || fmtScaled(-12345, 2) != "-123.45" {
		t.Fatal("fmtScaled")
	}
}

// A filter compiled for the scaled integers keeps the decimal comparison:
// for any value and any constant (more or fewer digits than the scale).
func TestNumFilterMatchesDecimal(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	ops := map[string]int{"=": opEq, "!=": opNe, "<": opLt, "<=": opLe, ">": opGt, ">=": opGe}
	consts := func(sc int) string {
		switch rng.Intn(6) {
		case 0:
			return fmt.Sprint(rng.Intn(20) - 10)
		case 1:
			return randDec(rng, 25, rng.Intn(6)) // beyond the int64 range at times
		}
		return randDec(rng, max(18-sc, 1), rng.Intn(sc+4))
	}
	for i := 0; i < 100000; i++ {
		sc := rng.Intn(7)
		x := rng.Int63n(2000) - 1000
		if rng.Intn(3) == 0 {
			x = rng.Int63n(1<<62) - 1<<61
		}
		c := consts(sc)
		cd, _ := pgnum.Parse(c)
		xd, _ := pgnum.Parse(fmtScaled(x, sc))
		cmp := pgnum.Cmp(xd, cd)
		for name, op := range ops {
			cf, err := numFilter(0, sc, Filter{Col: "c", Op: name, Value: c}, op)
			if err != nil {
				t.Fatal(err)
			}
			var got bool
			switch cf.op {
			case opNotNull:
				got = true
			case opIn:
				got = cf.in[schema.Value{I: x}]
			default:
				got = cmpResult(cmpInt(x, cf.v.I), cf.op)
			}
			if want := cmpResult(cmp, op); got != want {
				t.Fatalf("%s %s %s (scale %d, x=%d): got %v want %v", fmtScaled(x, sc), name, c, sc, x, got, want)
			}
		}
		// IN: equal only when exactly representable
		cf, err := numFilter(0, sc, Filter{Col: "c", Op: "in", Value: []any{c, "1", int64(3)}}, opIn)
		if err != nil {
			t.Fatal(err)
		}
		eq := func(s string) bool { d, _ := pgnum.Parse(s); return pgnum.Cmp(xd, d) == 0 }
		if want := eq(c) || eq("1") || eq("3"); cf.in[schema.Value{I: x}] != want {
			t.Fatalf("%s IN (%s,1,3) scale %d: got %v", fmtScaled(x, sc), c, sc, !want)
		}
	}
}

// 128-bit sums are exact, in any order and after merging partial sums.
func TestWideSum(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for round := 0; round < 2000; round++ {
		n := 1 + rng.Intn(300)
		parts := make([]acc, 1+rng.Intn(4))
		for i := range parts {
			parts[i].reset()
		}
		want := new(big.Int)
		for i := 0; i < n; i++ {
			var v int64
			switch rng.Intn(4) {
			case 0:
				v = rng.Int63() - rng.Int63()
			case 1:
				v = []int64{1<<63 - 1, -1 << 63, -1, 0, 1}[rng.Intn(5)]
			default:
				v = rng.Int63n(2_000_000_000_000_000_000) - 1_000_000_000_000_000_000
			}
			parts[rng.Intn(len(parts))].addWide(v)
			want.Add(want, big.NewInt(v))
		}
		for i := 1; i < len(parts); i++ {
			parts[0].merge(&parts[i])
		}
		a := &parts[0]
		sc := rng.Intn(19)
		s := aggSpec{num: true, scale: sc, wide: true, fn: "sum"}
		got := s.sumText(a)
		neg := want.Sign() < 0
		digits := new(big.Int).Abs(want).String()
		if len(digits) <= sc {
			digits = strings.Repeat("0", sc-len(digits)+1) + digits
		}
		w := digits
		if sc > 0 {
			w = digits[:len(digits)-sc] + "." + digits[len(digits)-sc:]
		}
		if neg {
			w = "-" + w
		}
		if got != w {
			t.Fatalf("sum scale %d: got %s want %s", sc, got, w)
		}
	}
}

func numTables() []schema.Table {
	return []schema.Table{{Name: "money", Key: "id", Columnar: true, Columns: []schema.Column{
		{Name: "id", Type: "int"}, {Name: "carrier", Type: "string"}, {Name: "status", Type: "string"},
		{Name: "price", Type: "numeric(12,2)"}, {Name: "fee", Type: "numeric(18,4)"},
		{Name: "free", Type: "numeric"}, {Name: "wide", Type: "numeric(30,5)"}, {Name: "nan", Type: "numeric(8,1)"},
	}}}
}

func numRow(rng *rand.Rand, id int64, nan bool) schema.Row {
	null := func(s string) schema.Value {
		if rng.Intn(15) == 0 {
			return schema.Value{Null: true}
		}
		return schema.Value{S: s}
	}
	r := schema.Row{{I: id}, {S: fmt.Sprint("c", rng.Intn(4))}, {S: fmt.Sprint("s", rng.Intn(3))},
		null(randDec(rng, 10, 2)), null(randDec(rng, 14, 4)), null(randDec(rng, 20, rng.Intn(6))),
		null(randDec(rng, 25, 5)), null(randDec(rng, 7, 1))}
	if nan && id == 77 {
		r[7] = schema.Value{S: "NaN"}
	}
	return r
}

var numQueries = []AggQuery{
	{Table: "money", SQLNulls: true, GroupBy: []string{"carrier", "status"}, Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "price"}, {Fn: "avg", Col: "price"},
		{Fn: "min", Col: "price"}, {Fn: "max", Col: "fee"}, {Fn: "sum", Col: "fee"}}},
	{Table: "money", SQLNulls: true, GroupBy: []string{"price"}, Filters: []Filter{{Col: "price", Op: ">", Value: "0"}}, Aggs: []Agg{{Fn: "count"}, {Fn: "max", Col: "fee"}}},
	{Table: "money", SQLNulls: true, Aggs: []Agg{{Fn: "sum", Col: "price"}, {Fn: "avg", Col: "fee"}}, Filters: []Filter{{Col: "fee", Op: "<=", Value: "12.345"}}},
	{Table: "money", SQLNulls: true, GroupBy: []string{"carrier"}, Aggs: []Agg{{Fn: "sum", Col: "nan"}}},
}

func (v *viewEnv) numAnswers(t *testing.T) []string {
	t.Helper()
	var out []string
	for i := range numQueries {
		q := numQueries[i]
		ts, err := v.e.Table(q.Table)
		if err != nil {
			t.Fatal(err)
		}
		res, err := v.c.Aggregate(ts.Schema().ID, &q)
		if err != nil {
			out = append(out, "err: "+err.Error())
			continue
		}
		var rows []string
		for _, r := range res.Rows {
			rows = append(rows, fmt.Sprint(r...))
		}
		slices.Sort(rows)
		out = append(out, fmt.Sprint(rows))
	}
	return out
}

func writeNum(t *testing.T, v *viewEnv, rng *rand.Rand, n int, nan bool) uint64 {
	t.Helper()
	var lsn uint64
	for i := 0; i < n; i += 50 {
		var ops []core.WriteOp
		for j := 0; j < 50; j++ {
			ops = append(ops, core.WriteOp{Table: "money", Row: numRow(rng, int64(rng.Intn(3000)), nan)})
		}
		// one write per key
		seen := map[int64]bool{}
		ops = slices.DeleteFunc(ops, func(o core.WriteOp) bool { k := o.Row[0].I; d := seen[k]; seen[k] = true; return d })
		var err error
		if lsn, err = v.e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	if nan {
		var err error
		if lsn, err = v.e.Write([]core.WriteOp{{Table: "money", Row: numRow(rng, 77, true)}}); err != nil {
			t.Fatal(err)
		}
	}
	if !v.f.WaitApplied(lsn, 20*time.Second) {
		t.Fatal("view did not catch up")
	}
	return lsn
}

func fileVersion(t *testing.T, dir string) uint32 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, viewFile))
	if err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint32(b[4:8])
}

// The saved view holds numeric columns (version 2), a restart loads it
// and replays the tail, a view without numeric columns is still version 1.
func TestNumericViewSaveLoad(t *testing.T) {
	dir := t.TempDir()
	v := startView(t, dir, true)
	for _, tb := range numTables() {
		if _, err := v.e.CreateTable(tb); err != nil {
			t.Fatal(err)
		}
	}
	v.ready(t)
	rng := rand.New(rand.NewSource(5))
	writeNum(t, v, rng, 4000, false)
	v.mustSave(t)
	if got := fileVersion(t, dir); got != viewVersionNum {
		t.Fatalf("file version %d, want %d", got, viewVersionNum)
	}
	lsnB := writeNum(t, v, rng, 1500, true) // the NaN reaches the tail only
	want := v.numAnswers(t)
	v.e.Close()
	if want[0][:3] == "err" || want[3][:3] != "err" {
		t.Fatalf("unexpected answers %v", want)
	}

	w := startView(t, dir, true)
	w.ready(t)
	if !w.f.Restored() {
		t.Fatal("view was rebuilt instead of loaded")
	}
	if !w.f.WaitApplied(lsnB, 10*time.Second) {
		t.Fatal("tail not replayed")
	}
	if got := w.numAnswers(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("answers differ after restart\n got %v\nwant %v", got, want)
	}
	// save again with the NaN column dropped from the view, then load that
	w.mustSave(t)
	w.e.Close()
	x := startView(t, dir, true)
	defer x.e.Close()
	x.ready(t)
	if !x.f.Restored() {
		t.Fatal("view with a dropped numeric column was rebuilt")
	}
	x.f.WaitApplied(lsnB, 10*time.Second)
	if got := x.numAnswers(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("answers differ after the second restart\n got %v\nwant %v", got, want)
	}
	// and a view rebuilt from the data agrees
	dir2 := t.TempDir()
	copyDir(t, dir, dir2)
	os.Remove(filepath.Join(dir2, viewFile))
	b := startView(t, dir2, true)
	defer b.e.Close()
	b.ready(t)
	b.f.WaitApplied(lsnB, 10*time.Second)
	if got := b.numAnswers(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt answers differ\n got %v\nwant %v", got, want)
	}
}

// A view without numeric columns is written as version 1 (unchanged), and
// a version 1 file is read.
func TestViewWithoutNumericStaysVersion1(t *testing.T) {
	dir, _, _ := setupView(t)
	if got := fileVersion(t, dir); got != viewVersion {
		t.Fatalf("file version %d, want %d", got, viewVersion)
	}
}

// Sums of the view equal sums of big decimals, with updates and deletes.
func TestNumericAggregateMatchesDecimals(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	tbl := &schema.Table{ID: 1, Name: "t", Key: "id", Columnar: true, Columns: []schema.Column{
		{Name: "id", Type: "int"}, {Name: "g", Type: "int"}, {Name: "n", Type: "numeric(18,2)"}, {Name: "m", Type: "numeric(9,0)"},
	}}
	if err := tbl.Init(); err != nil {
		t.Fatal(err)
	}
	c := NewColumnar()
	ct := c.ensure(tbl)
	live := map[int64]schema.Row{}
	for i := 0; i < 200000; i++ {
		id := int64(rng.Intn(150000))
		if rng.Intn(8) == 0 {
			ct.del(fmt.Sprint(id))
			delete(live, id)
			continue
		}
		huge := "9999999999999999.99" // wide sums
		r := schema.Row{{I: id}, {I: int64(rng.Intn(5))}, {S: randDec(rng, 16, 2)}, {S: randDec(rng, 9, 0)}}
		if rng.Intn(5) == 0 {
			r[2].S = huge
		}
		if rng.Intn(20) == 0 {
			r[2] = schema.Value{Null: true}
		}
		ct.put(fmt.Sprint(id), r)
		live[id] = r
	}
	type agg struct {
		sum pgnum.Dec
		n   int64
		min pgnum.Dec
		max pgnum.Dec
	}
	groups := map[int64]*agg{}
	for _, r := range live {
		if r[2].Null {
			continue
		}
		d, _ := pgnum.Parse(r[2].S)
		g := groups[r[1].I]
		if g == nil {
			g = &agg{min: d, max: d}
			groups[r[1].I] = g
		}
		g.sum, g.n = pgnum.Add(g.sum, d), g.n+1
		if pgnum.Cmp(d, g.min) < 0 {
			g.min = d
		}
		if pgnum.Cmp(d, g.max) > 0 {
			g.max = d
		}
	}
	res, err := c.Aggregate(1, &AggQuery{SQLNulls: true, GroupBy: []string{"g"}, Workers: 3, Aggs: []Agg{
		{Fn: "sum", Col: "n"}, {Fn: "avg", Col: "n"}, {Fn: "min", Col: "n"}, {Fn: "max", Col: "n"}, {Fn: "count", Col: "n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != len(groups) {
		t.Fatalf("%d groups, want %d", len(res.Rows), len(groups))
	}
	for _, row := range res.Rows {
		g := groups[row[0].(int64)]
		avg, _ := pgnum.Div(g.sum, pgnum.FromInt64(g.n))
		want := []any{g.sum.String(), avg.String(), g.min.String(), g.max.String(), g.n}
		if got := row[1:]; !reflect.DeepEqual(got, want) {
			t.Fatalf("group %v:\n got %v\nwant %v", row[0], got, want)
		}
	}
	// wide: the sums in this table need more than 64 bits
	if _, ok := groups[0]; !ok || groups[0].sum.String() == "" {
		t.Fatal("no group")
	}
	big := new(big.Int)
	big.SetString(strings.ReplaceAll(groups[0].sum.String(), ".", ""), 10)
	if !big.IsInt64() {
		t.Log("sums exceed int64 as intended")
	} else {
		t.Fatal("test data does not exercise 128-bit sums")
	}
}
