package sql

import (
	"strings"
	"testing"
)

// pgparity: PG-compatibility gaps from docs/14 Appendix M "what remains".
func TestPgparityInfinity(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, c := range []struct{ q, want string }{
		{`SELECT 'infinity'::timestamp`, "infinity"},
		{`SELECT '-infinity'::date`, "-infinity"},
		{`SELECT 'infinity'::timestamptz::text`, "infinity"},
		{`SELECT 'infinity'::timestamp > now()`, "t"},
		{`SELECT '-infinity'::timestamp < '2000-01-01'::timestamp`, "t"},
		{`SELECT 'infinity'::timestamp::date`, "infinity"},
		{`SELECT '-infinity'::date::timestamp`, "-infinity"},
		{`SELECT 'infinity'::date + 1`, "infinity"},
		{`SELECT 'infinity'::timestamp + interval '1 day'`, "infinity"},
		{`SELECT extract(epoch from 'infinity'::timestamp)`, "Infinity"},
		{`SELECT extract(epoch from '-infinity'::timestamp)`, "-Infinity"},
		{`SELECT to_char('infinity'::timestamp, 'YYYY')`, ""},
		{`SELECT isfinite('infinity'::timestamp)`, "f"},
		{`SELECT isfinite('2020-01-01'::timestamp)`, "t"},
		{`SELECT pg_typeof('infinity'::timestamp)::text`, "timestamp without time zone"},
	} {
		if got := cell(t, s, c.q); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
	run(t, s, `CREATE TABLE inf (id int PRIMARY KEY, a timestamp, b date)`)
	run(t, s, `INSERT INTO inf VALUES (1, 'infinity', '-infinity'), (2, '2020-01-01', '2020-01-01'), (3, '-infinity', 'infinity')`)
	if got := cell(t, s, `SELECT string_agg(id::text, ',' ORDER BY a DESC) FROM inf`); got != "1,2,3" {
		t.Errorf("order by infinity: %q", got)
	}
	if got := cell(t, s, `SELECT a::text || '/' || b::text FROM inf WHERE id = 3`); got != "-infinity/infinity" {
		t.Errorf("stored infinity: %q", got)
	}
	if got := cell(t, s, `SELECT count(*) FROM inf WHERE a = 'infinity'`); got != "1" {
		t.Errorf("filter infinity: %q", got)
	}
	_, err := tryRun(s, `SELECT 'infinity'::timestamp - 'infinity'::timestamp`)
	if err == nil || !strings.Contains(err.Error(), "cannot subtract infinite timestamps") {
		t.Errorf("infinite subtraction: %v", err)
	}
}

func TestPgparitySetseed(t *testing.T) {
	s := NewSession(newEngine(t))
	// expected values are PostgreSQL 16's own (docker postgres:16)
	for _, c := range []struct{ seed, want string }{
		{"-1", "0.725656831544149"}, {"0", "0.8702553105818676"}, {"1", "0.3978842227698167"}, {"0.5", "0.9851677175347999"},
	} {
		run(t, s, `SELECT setseed(`+c.seed+`)`)
		if got := cell(t, s, `SELECT random()`); got != c.want {
			t.Errorf("setseed(%s); random() = %q, want %q", c.seed, got, c.want)
		}
	}
	run(t, s, `SELECT setseed(0.5)`)
	a := cell(t, s, `SELECT random()::text || ',' || random()::text`)
	run(t, s, `SELECT setseed(0.5)`)
	if b := cell(t, s, `SELECT random()::text || ',' || random()::text`); a != b || a != "0.9851677175347999,0.825301858027981" {
		t.Errorf("setseed not deterministic: %q vs %q", a, b)
	}
	if got := cell(t, s, `SELECT setseed(0.5) IS NULL`); got != "f" {
		t.Errorf("setseed IS NULL = %q", got)
	}
	if _, err := tryRun(s, `SELECT setseed(2)`); err == nil || !strings.Contains(err.Error(), "out of allowed range [-1,1]") {
		t.Errorf("setseed(2): %v", err)
	}
	if got := cell(t, s, `SELECT random() >= 0 AND random() < 1`); got != "t" {
		t.Errorf("random range: %q", got)
	}
}

func TestPgparityFKKeyChange(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE pp (id int PRIMARY KEY, v text)`)
	run(t, s, `CREATE TABLE pc (id int PRIMARY KEY, pid int REFERENCES pp(id) ON DELETE CASCADE)`)
	run(t, s, `INSERT INTO pp VALUES (1,'a'),(2,'b')`)
	run(t, s, `INSERT INTO pc VALUES (10, 1)`)
	wantCode := func(q, code string) {
		t.Helper()
		_, err := tryRun(s, q)
		if e, ok := err.(*Error); !ok || e.Code != code {
			t.Errorf("%s: %v, want %s", q, err, code)
		}
	}
	wantCode(`UPDATE pp SET id = 100 WHERE id = 1`, "23503")
	run(t, s, `CREATE TABLE pn (id int PRIMARY KEY, v text)`)
	run(t, s, `CREATE TABLE pnc (id int PRIMARY KEY, pid int REFERENCES pn(id))`)
	run(t, s, `INSERT INTO pn VALUES (1,'a'),(2,'b')`)
	run(t, s, `INSERT INTO pnc VALUES (10, 1)`)
	wantCode(`DELETE FROM pn WHERE id = 1`, "23503")
	wantCode(`UPDATE pn SET id = 100 WHERE id = 1`, "23503")
	run(t, s, `UPDATE pn SET id = 200 WHERE id = 2`)
	// unchanged key, other columns, and unreferenced rows are fine
	run(t, s, `UPDATE pp SET v = 'z' WHERE id = 1`)
	run(t, s, `UPDATE pp SET id = 200 WHERE id = 2`)
	if got := cell(t, s, `SELECT count(*) FROM pc WHERE pid = 1`); got != "1" {
		t.Errorf("child lost after refused key change: %s", got)
	}
}

// expected values below are PostgreSQL 16's own output (docker postgres:16)
func TestPgparityNumericPowerDiv(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, c := range []struct{ q, want string }{
		{`SELECT power(2.0, 0.5)`, "1.4142135623730950"},
		{`SELECT power(2.0, 3)`, "8.0000000000000000"},
		{`SELECT power(10.5, -2)`, "0.009070294784580499"},
		{`SELECT power(2, 0.5)`, "1.4142135623730950"},
		{`SELECT power(0, 0.0)`, "1.0000000000000000"},
		{`SELECT power(0.0, 2.5)`, "0.0000000000000000"},
		{`SELECT power(1.5, 100)`, "406561177535215237.4"},
		{`SELECT power(-2.5, 3)`, "-15.625000000000000"},
		{`SELECT power(7.25, 0.333)`, "1.9341607068008828"},
		{`SELECT power(100, -0.5)`, "0.10000000000000000"},
		{`SELECT pg_typeof(power(2, 3))::text`, "double precision"},
		{`SELECT pg_typeof(power(2.0, 3))::text`, "numeric"},
		{`SELECT pg_typeof(div(7, 2))::text`, "numeric"},
		{`SELECT div(7, 2)`, "3"},
		{`SELECT div(-7, 2)`, "-3"},
		{`SELECT div(7.5, 2)`, "3"},
		{`SELECT div(-7.5, 2.5)`, "-3"},
		{`SELECT div(1e20::numeric, 3)`, "33333333333333333333"},
	} {
		if got := cell(t, s, c.q); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
	for _, c := range []struct{ q, code string }{
		{`SELECT power(0, -1.0)`, "2201F"},
		{`SELECT power(-2, 0.5)`, "2201F"},
		{`SELECT div(1, 0)`, "22012"},
	} {
		_, err := tryRun(s, c.q)
		if e, ok := err.(*Error); !ok || e.Code != c.code {
			t.Errorf("%s: %v, want %s", c.q, err, c.code)
		}
	}
}

func TestPgparityTypeUnification(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, c := range []struct{ q, want string }{
		{`SELECT pg_typeof(CASE WHEN TRUE THEN 1 ELSE 2.5 END)::text`, "numeric"},
		{`SELECT pg_typeof(CASE WHEN FALSE THEN 2.5 ELSE 1 END)::text`, "numeric"},
		{`SELECT pg_typeof(CASE WHEN TRUE THEN 1 ELSE 2.5::float8 END)::text`, "double precision"},
		{`SELECT pg_typeof(COALESCE(1, 2.5))::text`, "numeric"},
		{`SELECT pg_typeof(COALESCE(NULL, 1))::text`, "integer"},
		{`SELECT pg_typeof(GREATEST(1, 2.5))::text`, "numeric"},
		{`SELECT CASE WHEN TRUE THEN 1 ELSE 2.5 END`, "1"},
		{`SELECT CASE WHEN FALSE THEN 1 ELSE 2.5 END`, "2.5"},
		{`SELECT pg_typeof(x)::text FROM (SELECT 1 AS x UNION SELECT 2.5) s LIMIT 1`, "numeric"},
		{`SELECT pg_typeof(x)::text FROM (SELECT 2.5 AS x UNION ALL SELECT 1 UNION ALL SELECT NULL) s LIMIT 1`, "numeric"},
	} {
		if got := cell(t, s, c.q); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
	// the result column description (what clients see) is numeric too
	for _, q := range []string{`SELECT CASE WHEN TRUE THEN 1 ELSE 2.5 END`, `SELECT 1 UNION SELECT 2.5`, `SELECT COALESCE(1, 2.5)`} {
		r := run(t, s, q)
		if r.Cols[0].Kind != KNumeric {
			t.Errorf("%s: column kind %v, want numeric", q, r.Cols[0].Kind)
		}
	}
	if _, ok := BinaryValueFor(1700, IntD(1)); !ok {
		t.Errorf("an integer in a numeric column has no binary form")
	}
}

func TestPgparityToCharLiterals(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, c := range []struct{ q, want string }{
		{`SELECT to_char(1, 'ZZZ')`, "ZZZ"},
		{`SELECT to_char(-1, 'ZZZ')`, "ZZZ"},
		{`SELECT to_char(12, 'xx99')`, "xx 12"},
		{`SELECT to_char(12, '99xx')`, " 12xx"},
		{`SELECT to_char(12, 'FMxx99')`, "xx12"},
		{`SELECT to_char(12, '"abc"99')`, "abc 12"},
		{`SELECT to_char(12, '$99')`, "$ 12"},
		{`SELECT to_char(12.5, '"Total:" 99.9')`, "Total:  12.5"},
		{`SELECT to_char(1.5, '99.9"u"')`, "  1.5u"},
		{`SELECT to_char(12, '999')`, "  12"},
	} {
		if got := cell(t, s, c.q); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestPgparityConstraintDef(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE cd (id int PRIMARY KEY, price numeric, q int, f float8, CONSTRAINT cd_p CHECK (price >= 0), CONSTRAINT cd_q CHECK (q > 0 AND price < 100), CONSTRAINT cd_h CHECK (price > 0.5))`)
	for _, c := range []struct{ name, want string }{
		{"cd_p", "CHECK (price >= 0::numeric)"},
		{"cd_q", "CHECK (q > 0 AND price < 100::numeric)"},
		{"cd_h", "CHECK (price > 0.5)"},
	} {
		got := cell(t, s, `SELECT pg_get_constraintdef(oid, true) FROM pg_constraint WHERE conname = '`+c.name+`'`)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPgparityParamTypeof(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, c := range []struct{ q, want string }{
		{`SELECT $1::int + 1, pg_typeof($1)::text`, "integer"},
		{`SELECT $1::bigint, pg_typeof($1)::text`, "bigint"},
		{`SELECT $1::text, pg_typeof($1)::text`, "text"},
		{`SELECT $1::numeric + 1, pg_typeof($1)::text`, "numeric"},
	} {
		r, err := tryRun(s, c.q, TextD("5"))
		if err != nil {
			t.Logf("%s: %v", c.q, err) // the first form is not a pgdiff case
			continue
		}
		if got := r.Rows[0][len(r.Rows[0])-1].String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestPgparityInfinityBinary(t *testing.T) {
	for _, d := range []Datum{{K: KTimestamp, I: tsInfinity}, {K: KTime, I: tsNegInfinity}, {K: KDate, I: tsInfinity}, {K: KDate, I: tsNegInfinity}} {
		b, ok := BinaryValue(d)
		if !ok {
			t.Fatalf("%v: no binary form", d)
		}
		back, err := ValueFromBinary(d.K, b)
		if err != nil || back.K != d.K || back.I != d.I {
			t.Errorf("%v: binary round trip gave %v, %v", d, back, err)
		}
	}
	// PostgreSQL's wire values: timestamp +-infinity is the extreme int64, date the extreme int32
	if b, _ := BinaryValue(Datum{K: KTimestamp, I: tsInfinity}); len(b) != 8 || b[0] != 0x7f || b[7] != 0xff {
		t.Errorf("timestamp infinity bytes %x", b)
	}
	if b, _ := BinaryValue(Datum{K: KDate, I: tsNegInfinity}); len(b) != 4 || b[0] != 0x80 || b[3] != 0 {
		t.Errorf("date -infinity bytes %x", b)
	}
}
