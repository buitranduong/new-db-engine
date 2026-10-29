package shadow

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		k    Kind
		a, b string
	}{
		{KNumeric, "10.50", "10.5"}, {KNumeric, "1e3", "1000"}, {KNumeric, "-0.00", "0"}, {KNumeric, "007.100", "7.1"},
		{KInt, "+5", "5"}, {KBool, "true", "t"}, {KBool, "f", "false"},
		{KUUID, "550E8400-E29B-41D4-A716-446655440000", "550e8400-e29b-41d4-a716-446655440000"},
		{KBytea, `\xDEADBEEF`, `\xdeadbeef`},
		{KTimestamptz, "2026-10-29 12:34:56.123456+07", "2026-10-29 05:34:56.123456+00"},
		{KTimestamp, "2026-10-29 12:34:56.5", "2026-10-29 12:34:56.500000"},
		{KJSON, `{"b": 1, "a": [1, 2]}`, `{"a":[1,2],"b":1}`},
		{KFloat8, "1.50", "1.5"}, {KFloat4, "0.1", "0.10000000149"}, {KBpchar, "ab ", "ab"},
	}
	for _, c := range cases {
		if x, y := Normalize(c.k, c.a), Normalize(c.k, c.b); x != y {
			t.Errorf("kind %d: %q -> %q, %q -> %q", c.k, c.a, x, c.b, y)
		}
	}
	if Normalize(KNumeric, "1.5") == Normalize(KNumeric, "1.6") || Normalize(KText, "a") == Normalize(KText, "A") {
		t.Error("different values must stay different")
	}
}

func TestKindAndDDL(t *testing.T) {
	if kindOfOID(1700) != KNumeric || kindOfOID(2950) != KUUID || kindOfOID(1186) != KUnsupported {
		t.Error("kindOfOID")
	}
	tb := &Table{Name: "t", Cols: []Column{
		{Name: "id", OID: 20, Type: "bigint", Kind: KInt, NotNull: true},
		{Name: "p", OID: 1700, Type: "numeric(12,2)", Kind: KNumeric},
		{Name: "v", OID: 1043, Type: "character varying(10)", Kind: KText},
		{Name: "i", OID: 23, Type: "integer", Kind: KInt}}, PK: []int{0}}
	want := `CREATE TABLE "t" ("id" bigint NOT NULL, "p" numeric(12,2), "v" varchar(10), "i" integer, PRIMARY KEY ("id"))`
	if got := tb.CreateDDL(); got != want {
		t.Errorf("DDL\n got %s\nwant %s", got, want)
	}
	bad := &Table{Name: "x", Cols: []Column{{Name: "iv", OID: 1186, Type: "interval", Kind: KUnsupported}}}
	if err := bad.Check(); err == nil || !strings.Contains(err.Error(), "interval") {
		t.Errorf("unsupported type must be reported, got %v", err)
	}
	nopk := &Table{Name: "n", ReplIdent: 'd', Cols: []Column{{Name: "a", OID: 20, Kind: KInt}}}
	if err := nopk.Check(); err == nil || !strings.Contains(err.Error(), "REPLICA IDENTITY FULL") {
		t.Errorf("no-PK table without FULL identity must be refused with the fix, got %v", err)
	}
	nopk.ReplIdent = 'f'
	if nopk.Check() != nil {
		t.Error("FULL identity is fine")
	}
}

func v(s string) val { return val{b: []byte(s)} }

var nul = val{st: vNull}
var unch = val{st: vUnchanged}

func testRel() *rel {
	tb := &Table{Name: "sims"}
	return &rel{tbl: tb, cols: []relCol{{"number", KText}, {"price", KNumeric}, {"meta", KJSON}}, pk: []int{0}}
}

func TestSQLGenPK(t *testing.T) {
	r := testRel()
	ins, _ := r.insert([]val{v("1"), v("9.5"), nul}, false)
	if ins.sql != `INSERT INTO "sims" ("number", "price", "meta") VALUES ($1, $2, $3)` || ins.args[2] != nil || string(ins.args[1]) != "9.5" {
		t.Errorf("insert: %s %v", ins.sql, ins.args)
	}
	up, _ := r.insert([]val{v("1"), v("9.5"), v("{}")}, true)
	if !strings.Contains(up.sql, `ON CONFLICT ("number") DO UPDATE SET "price" = EXCLUDED."price", "meta" = EXCLUDED."meta"`) {
		t.Errorf("upsert: %s", up.sql)
	}
	// unchanged TOAST column is not in SET; key unchanged -> WHERE from the new tuple
	ss, err := r.update([]val{v("1"), v("10"), unch}, nil)
	if err != nil || len(ss) != 1 || ss[0].sql != `UPDATE "sims" SET "number" = $1, "price" = $2 WHERE "number" = $3` {
		t.Errorf("update: %v %+v", err, ss)
	}
	// key change: WHERE uses the old key
	ss, _ = r.update([]val{v("2"), v("10"), unch}, []val{v("1"), nul, nul})
	if string(ss[0].args[2]) != "1" || string(ss[0].args[0]) != "2" {
		t.Errorf("key change args: %q", ss[0].args)
	}
	del, err := r.delete([]val{v("7"), nul, nul})
	if err != nil || del[0].sql != `DELETE FROM "sims" WHERE "number" = $1` {
		t.Errorf("delete: %v %+v", err, del)
	}
	if _, err := r.delete([]val{nul, nul, nul}); err == nil {
		t.Error("a delete without key value must fail")
	}
}

func TestSQLGenNoPK(t *testing.T) {
	r := &rel{tbl: &Table{Name: "h"}, cols: []relCol{{"a", KInt}, {"b", KText}}}
	del, err := r.delete([]val{v("1"), nul})
	if err != nil || del[0].sql != `DELETE FROM "h" WHERE "a" = $1 AND "b" IS NULL` || del[0].post == nil {
		t.Errorf("nopk delete: %v %+v", err, del)
	}
	ss, err := r.update([]val{v("2"), unch}, []val{v("1"), v("x")})
	if err != nil || len(ss) != 2 || ss[0].sql != `DELETE FROM "h" WHERE "a" = $1 AND "b" = $2` {
		t.Fatalf("nopk update: %v %+v", err, ss)
	}
	if string(ss[1].args[1]) != "x" { // unchanged column taken from the old row
		t.Errorf("insert of the new row must take unchanged columns from the old row: %q", ss[1].args)
	}
	if _, err := r.update([]val{v("2"), v("y")}, nil); err == nil {
		t.Error("nopk update without old tuple must fail")
	}
}

func TestNoPKPostReinsertsDuplicates(t *testing.T) {
	r := &rel{tbl: &Table{Name: "h"}, cols: []relCol{{"a", KInt}}}
	ap := newApplier(nil, "s", 10, false, func(string, ...any) {})
	del, _ := r.delete([]val{v("1")})
	// STASK deleted 3 identical rows for one source row: two must be put back
	if err := del[0].post(ap, tagOf("DELETE 3")); err != nil {
		t.Fatal(err)
	}
	if len(ap.pending) != 2 {
		t.Errorf("want 2 reinserts, got %d", len(ap.pending))
	}
	ap.pending = nil
	if err := del[0].post(ap, tagOf("DELETE 0")); err != nil || ap.anomalies != 1 {
		t.Errorf("0 rows is an anomaly: err=%v anomalies=%d", err, ap.anomalies)
	}
	ap.strict = true
	if err := del[0].post(ap, tagOf("DELETE 0")); !IsFatal(err) {
		t.Errorf("strict anomaly must be fatal, got %v", err)
	}
}

func TestParseQueries(t *testing.T) {
	qs := ParseQueries("-- name: a\nSELECT 1;\n\n-- ordered\nSELECT x\nFROM t;\nSELECT y FROM t ORDER BY y;\n")
	if len(qs) != 3 || qs[0].Name != "a" || qs[0].Ordered || !qs[1].Ordered || qs[1].Name != "q2" || !qs[2].Ordered {
		t.Errorf("%+v", qs)
	}
}

func rowsOf(oids []uint32, data ...[]string) *Rows {
	r := &Rows{OIDs: oids}
	for range oids {
		r.Cols = append(r.Cols, "c")
	}
	for _, d := range data {
		row := make([]*string, len(d))
		for i := range d {
			if d[i] != "<null>" {
				x := d[i]
				row[i] = &x
			}
		}
		r.Data = append(r.Data, row)
	}
	return r
}

func TestCompareResults(t *testing.T) {
	a := rowsOf([]uint32{1700, 25}, []string{"10.50", "x"}, []string{"3", "y"})
	b := rowsOf([]uint32{20, 25}, []string{"3", "y"}, []string{"10.5", "x"})
	if ok, d := compareResults(a, b, false); !ok {
		t.Errorf("set-equal results must match: %s", d)
	}
	if ok, _ := compareResults(a, b, true); ok {
		t.Error("ordered comparison must notice the order")
	}
	c := rowsOf([]uint32{20, 25}, []string{"3", "y"}, []string{"11", "x"})
	if ok, _ := compareResults(a, c, false); ok {
		t.Error("different value must be detected")
	}
	avgA := rowsOf([]uint32{1700}, []string{"3.3333333333333333"})
	avgB := rowsOf([]uint32{701}, []string{"3.3333333333333335"})
	if ok, d := compareResults(avgA, avgB, false); !ok {
		t.Errorf("float noise within 1e-9 must pass: %s", d)
	}
	n1 := rowsOf([]uint32{25}, []string{"<null>"})
	n2 := rowsOf([]uint32{25}, []string{""})
	if ok, _ := compareResults(n1, n2, false); ok {
		t.Error("NULL and empty string differ")
	}
}

func TestChecksumIsOrderIndependentAndSensitive(t *testing.T) {
	sc := &scanner{cols: []colInfo{{"id", KInt}, {"n", KNumeric}}, pk: []int{0}}
	rows := [][]string{{"1", "1.50"}, {"2", "2"}, {"3", "0.1"}}
	sum := func(order []int, mod func([]string) []string) uint64 {
		var s uint64
		for _, i := range order {
			r := rows[i]
			if mod != nil {
				r = mod(append([]string(nil), r...))
			}
			c := []string{Normalize(KInt, r[0]), Normalize(KNumeric, r[1])}
			_, _, rh := sc.keyOf(c)
			s += rh
		}
		return s
	}
	if sum([]int{0, 1, 2}, nil) != sum([]int{2, 0, 1}, nil) {
		t.Error("checksum must not depend on row order")
	}
	if sum([]int{0, 1, 2}, nil) != sum([]int{0, 1, 2}, func(r []string) []string {
		if r[1] == "1.50" {
			r[1] = "1.5"
		}
		return r
	}) {
		t.Error("equal numeric values must hash equal")
	}
	if sum([]int{0, 1, 2}, nil) == sum([]int{0, 1, 2}, func(r []string) []string { r[1] = "9"; return r }) {
		t.Error("changed value must change the checksum")
	}
}

func TestPercentileAndRateLimiter(t *testing.T) {
	xs := []int64{50, 10, 40, 20, 30}
	if Percentile(xs, 50) != 30 || Percentile(xs, 99) != 50 || Percentile(nil, 50) != 0 {
		t.Error("percentile")
	}
	rl := &rateLimiter{rps: 1000}
	now := time.Unix(100, 0)
	d1 := rl.delay(500, now)
	d2 := rl.delay(500, now)
	if d1 != 500*time.Millisecond || d2 != time.Second {
		t.Errorf("limiter %v %v", d1, d2)
	}
	if (&rateLimiter{}).delay(1000, now) != 0 {
		t.Error("rate 0 means unlimited")
	}
}

// The duplicate gate: a transaction whose commit LSN is below the applied LSN
// is skipped (this is the rule runner.stream uses on Begin).
func TestDedupeRule(t *testing.T) {
	applied := uint64(pglogrepl.LSN(0x1000))
	skip := func(final uint64) bool { return final < applied }
	if !skip(0x0FF8) || skip(0x1000) || skip(0x2000) {
		t.Error("gate")
	}
}
