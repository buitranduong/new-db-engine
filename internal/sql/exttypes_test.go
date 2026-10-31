package sql

import (
	"math/rand"
	"sort"
	"testing"
)

// Expected values below were read from PostgreSQL 16.15 (select '...'::interval etc.), see scripts/suite/15_types_more.sql.

func TestIntervalInputOutput(t *testing.T) {
	for in, want := range map[string]string{
		"1-2": "1 year 2 mons", "-1-2": "-1 years -2 mons", "1 2:03:04": "1 day 02:03:04", "1 -2:03:04": "1 day -02:03:04",
		"3:04": "03:04:00", "04:05.5": "00:04:05.5", "5": "00:00:05", "1.5 days": "1 day 12:00:00", "1.5 months": "1 mon 15 days",
		".5 years": "6 mons", "1 year 2 months ago": "-1 years -2 mons", "@ 1 day 2 hours ago": "-1 days -02:00:00",
		"-1 day +2 hours": "-1 days +02:00:00", "P1Y2M3DT4H5M6S": "1 year 2 mons 3 days 04:05:06", "P0001-02-03T04:05:06": "1 year 2 mons 3 days 04:05:06",
		"PT1.5S": "00:00:01.5", "P1W": "7 days", "P-1Y2M": "-10 mons", "1h 2m 3s": "01:02:03", "2 weeks": "14 days", "1 decade": "10 years",
		"1.2345678 sec": "00:00:01.234568", "100:00:00": "100:00:00", "1 day 24:00:00": "1 day 24:00:00", "0.1 day": "02:24:00",
		"1.3 years": "1 year 4 mons", "0.7 week": "4 days 21:36:00", "2.7 months": "2 mons 21 days", "-1.5 days": "-1 days -12:00:00",
		"1 hour 2": "01:00:02", "2147483647 months": "178956970 years 7 mons", "1 day ago ago": "-1 days", "1:30 ago": "-01:30:00",
	} {
		d, err := decodeIntervalPG(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := formatInterval(d); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "1 2", "infinity", "1 fortnight", "1:2:3:4", "1e2 days", "1 second 2 seconds", "1 dow", "2147483648 months", "1 hour 02:00:00"} {
		if _, err := decodeIntervalPG(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestIntervalStyles(t *testing.T) {
	d := IntervalFull(14, 3, 4*usPerHour+5*usPerMin+6*usPerSec+700000)
	n := IntervalFull(0, -1, -2*usPerHour)
	for _, tc := range []struct {
		d    Datum
		st   ivStyle
		want string
	}{
		{d, ivPostgres, "1 year 2 mons 3 days 04:05:06.7"}, {d, ivVerbose, "@ 1 year 2 mons 3 days 4 hours 5 mins 6.7 secs"},
		{d, ivISO, "P1Y2M3DT4H5M6.7S"}, {d, ivSQL, "+1-2 +3 +4:05:06.7"},
		{n, ivPostgres, "-1 days -02:00:00"}, {n, ivVerbose, "@ 1 day 2 hours ago"}, {n, ivISO, "P-1DT-2H"}, {n, ivSQL, "-1 2:00:00"},
		{IntervalFull(0, 0, 0), ivPostgres, "00:00:00"}, {IntervalFull(0, 0, 0), ivVerbose, "@ 0"}, {IntervalFull(0, 0, 0), ivISO, "PT0S"}, {IntervalFull(0, 0, 0), ivSQL, "0"},
	} {
		if got := formatIntervalStyle(tc.d, tc.st); got != tc.want {
			t.Errorf("style %d: %q, want %q", tc.st, got, tc.want)
		}
	}
}

func TestIntervalArithmetic(t *testing.T) {
	iv := func(s string) Datum {
		d, err := decodeIntervalPG(s)
		if err != nil {
			t.Fatal(s, err)
		}
		return d
	}
	for _, tc := range []struct {
		f    func() (Datum, error)
		want string
	}{
		{func() (Datum, error) { return ivCheckedMul(iv("1 year"), 1.5, false) }, "1 year 6 mons"},
		{func() (Datum, error) { return ivCheckedMul(iv("1 mon"), 0.5, false) }, "15 days"},
		{func() (Datum, error) { return ivCheckedMul(iv("1 mon"), 4, true) }, "7 days 12:00:00"},
		{func() (Datum, error) { return ivCheckedMul(iv("5 days"), 1.5, true) }, "3 days 08:00:00"},
		{func() (Datum, error) { return justifyHours(iv("50 hours")) }, "2 days 02:00:00"},
		{func() (Datum, error) { return justifyDays(iv("65 days")) }, "2 mons 5 days"},
		{func() (Datum, error) { return justifyInterval(iv("1 month -1 hour")) }, "29 days 23:00:00"},
		{func() (Datum, error) { return justifyInterval(iv("-1 month 1 hour")) }, "-29 days -23:00:00"},
		{func() (Datum, error) { return makeInterval(1, 2, 3, 4, 5, 6, 7.5) }, "1 year 2 mons 25 days 05:06:07.5"},
	} {
		d, err := tc.f()
		if err != nil {
			t.Error(tc.want, err)
			continue
		}
		if got := formatInterval(d); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
	if _, err := ivCheckedMul(iv("2147483647 days"), 2, false); err == nil {
		t.Error("overflow accepted")
	}
	if got, _ := intervalPart("epoch", iv("1 year 2 months 3 days 04:05:06.789")); got != "37015506.789000" {
		t.Errorf("epoch = %s", got)
	}
}

// The stored form of an interval orders like interval_cmp and keeps all three fields.
func TestIntervalStoredOrder(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var ds []Datum
	for i := 0; i < 400; i++ {
		ds = append(ds, IntervalFull(int64(r.Intn(40)-20), int64(r.Intn(80)-40), int64(r.Intn(1e12))-5e11))
	}
	enc := make([]string, len(ds))
	for i, d := range ds {
		enc[i] = ivEncode(d)
		back, ok := ivDecode(enc[i])
		if !ok || back != d {
			t.Fatalf("round trip %v -> %q -> %v", d, enc[i], back)
		}
	}
	idx := make([]int, len(ds))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return enc[idx[a]] < enc[idx[b]] })
	for i := 1; i < len(idx); i++ {
		if ivCmp(ds[idx[i-1]], ds[idx[i]]) > 0 {
			t.Fatalf("stored order differs from interval order: %v before %v", ds[idx[i-1]], ds[idx[i]])
		}
	}
}

func TestRangeTypes(t *testing.T) {
	parse := func(tag int32, s string) rng {
		r, err := rngParse(tag, s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		return r
	}
	i4 := rngTagOfName("int4range")
	for in, want := range map[string]string{
		"[1,5)": "[1,5)", "(1,5]": "[2,6)", "[1,5]": "[1,6)", "(1,5)": "[2,5)", "[1,1)": "empty", "(1,2)": "empty", " EMPTY ": "empty",
		"[,5)": "(,5)", "[1,)": "[1,)", "[,]": "(,)", "[ 1 , 5 )": "[1,5)",
	} {
		if got := parse(i4, in).text(); got != want {
			t.Errorf("int4range %q = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"[a,b)", "[5,1)", "[1,2", "1,2)", "[1;2)", "[1,2) x", "[1,2147483647]"} {
		if _, err := rngParse(i4, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	a, b := parse(i4, "[1,5)"), parse(i4, "[4,8)")
	if !rngOverlaps(a, b) || rngAdjacent(a, b) || !rngAdjacent(a, parse(i4, "[5,8)")) {
		t.Error("overlap/adjacent")
	}
	if u, err := rngUnion(a, b, true); err != nil || u.text() != "[1,8)" {
		t.Errorf("union %v %v", u.text(), err)
	}
	if _, err := rngUnion(a, parse(i4, "[7,8)"), true); err == nil {
		t.Error("non-contiguous union accepted")
	}
	if x, _ := rngIntersect(a, b); x.text() != "[4,5)" {
		t.Errorf("intersection %s", x.text())
	}
	if _, err := rngMinus(parse(i4, "[1,10)"), parse(i4, "[3,5)")); err == nil {
		t.Error("non-contiguous difference accepted")
	}
	// stored order: empty, (,5), [1,3), [1,4), [5,)
	var enc []string
	for _, s := range []string{"empty", "(,5)", "[1,3)", "[1,4)", "[5,)"} {
		enc = append(enc, rngEncode(i4, Datum{K: KText, D: i4, S: s}))
	}
	if !sort.StringsAreSorted(enc) {
		t.Errorf("stored order: %q", enc)
	}
	for i, s := range []string{"empty", "(,5)", "[1,3)", "[1,4)", "[5,)"} {
		if got := rngDecode(i4, enc[i]); got.S != s || got.D != i4 {
			t.Errorf("decode %q = %+v", enc[i], got)
		}
	}
	nr := rngTagOfName("numrange")
	if parse(nr, "[1.0,2)").text() != "[1.0,2)" || rngCompare(parse(nr, "[1.0,2)"), parse(nr, "[1,2)")) != 0 {
		t.Error("numrange equality keeps the scale but compares equal")
	}
}

func TestBitAndMacaddr(t *testing.T) {
	d, _ := bitApply("bit(8)", IntD(5), true)
	if d.S != "00000101" || d.D != tagBit {
		t.Errorf("5::bit(8) = %+v", d)
	}
	if _, err := bitApply("bit(3)", TextD("10"), false); err == nil {
		t.Error("short bit string assigned to bit(3)")
	}
	if x, _ := bitApply("bit(4)", TextD("10"), true); x.S != "1000" {
		t.Errorf("explicit pad: %q", x.S)
	}
	l, r := Datum{K: KText, D: tagBit, S: "1100"}, Datum{K: KText, D: tagBit, S: "1010"}
	for op, want := range map[string]string{"&": "1000", "|": "1110", "#": "0110", "||": "11001010"} {
		got, ok, err := bitBinary(op, l, r)
		if !ok || err != nil || got.S != want {
			t.Errorf("%s = %+v %v", op, got, err)
		}
	}
	if got, _, _ := bitBinary("<<", l, IntD(1)); got.S != "1000" {
		t.Errorf("shift %q", got.S)
	}
	if bin := bitBinaryOut(Datum{S: "10110"}); len(bin) != 5 || bin[4] != 0b10110000 {
		t.Errorf("bit send %v", bin)
	}
	if back, err := bitFromBinary(1560, bitBinaryOut(Datum{S: "10110"})); err != nil || back.S != "10110" {
		t.Errorf("bit recv %v %v", back, err)
	}
	m, err := mac8Parse("08:00:2b:01:02:03")
	if err != nil || mac8Text(m) != "08:00:2b:ff:fe:01:02:03" {
		t.Errorf("macaddr8 from 6 octets: %v %v", mac8Text(m), err)
	}
	for _, in := range []string{"08-00-2b-01-02-03-04-05", "08002b:0102030405", "0800.2b01.0203.0405", "08002b01:02030405", "08002b0102030405"} {
		if m, err := mac8Parse(in); err != nil || mac8Text(m) != "08:00:2b:01:02:03:04:05" {
			t.Errorf("macaddr8 %q: %v %v", in, mac8Text(m), err)
		}
	}
}

func TestTSVectorTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		"b a  a": "'a' 'b'", "a:3,1,2B,1C": "'a':1C,2B,3", "a:1D": "'a':1", "Foo bar": "'Foo' 'bar'", "a:16384": "'a':16383",
		"'a':1B,1A,1D": "'a':1A", "foo:5 foo:3": "'foo':3,5", "": "",
	} {
		v, err := tsvParse(in)
		if err != nil || v.text() != want {
			t.Errorf("tsvector %q = %q (%v), want %q", in, v.text(), err, want)
		}
	}
	for in, want := range map[string]string{
		"a | b & c": "'a' | 'b' & 'c'", "(a | b) & c": "( 'a' | 'b' ) & 'c'", "!(a & b)": "!( 'a' & 'b' )", "a <3> b": "'a' <3> 'b'",
		"a:*B": "'a':*B", "a & (b & c)": "'a' & 'b' & 'c'", "a <-> (b <-> c)": "'a' <-> ( 'b' <-> 'c' )", "(a <-> b) <-> c": "'a' <-> 'b' <-> 'c'",
		"a & b <-> c": "'a' & 'b' <-> 'c'", "!!a": "!!'a'",
	} {
		q, err := tsqParse(in)
		if err != nil || q.text() != want {
			t.Errorf("tsquery %q = %q (%v), want %q", in, q.text(), err, want)
		}
	}
	for _, bad := range []string{"a & ", "(a", "a b", "a)", "& a"} {
		if _, err := tsqParse(bad); err == nil {
			t.Errorf("tsquery %q accepted", bad)
		}
	}
	v, _ := tsvParse("a:1 b:2 c:3")
	for q, want := range map[string]bool{"a & b": true, "a <-> b": true, "a <-> c": false, "a <2> c": true, "(a | b) <-> c": true, "(a & b) <-> c": false, "!a": false, "d | b": true, "a:B": false} {
		pq, _ := tsqParse(q)
		if got := v.eval(pq).ok; got != want {
			t.Errorf("%q @@ %q = %v", v.text(), q, got)
		}
	}
	// binary round trip in PostgreSQL's layout
	v2, _ := tsvParse("a:1A,3 bb:2 ccc")
	if back, ok := tsvFromBinary(tsvBinaryOut(v2)); !ok || back.text() != v2.text() {
		t.Errorf("tsvector binary: %v %v", back.text(), ok)
	}
	q2, _ := tsqParse("a <3> (b & c) | !d:*B")
	if back, ok := tsqFromBinary(tsqBinaryOut(q2)); !ok || back.text() != q2.text() {
		t.Errorf("tsquery binary: %v %v", back.text(), ok)
	}
	// 'a & b' in PostgreSQL's wire layout: size 3, AND, then the right operand, then the left one
	ab, _ := tsqParse("a & b")
	want := []byte{0, 0, 0, 3, 2, 2, 1, 0, 0, 'b', 0, 1, 0, 0, 'a', 0}
	if got := tsqBinaryOut(ab); string(got) != string(want) {
		t.Errorf("tsquery wire layout %v, want %v", got, want)
	}
}
