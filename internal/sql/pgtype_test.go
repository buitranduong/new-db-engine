package sql

import (
	"strings"
	"testing"
)

// colOIDs runs q and returns "oid/mod" of each result column.
func colOIDs(t *testing.T, s *Session, q string) string {
	t.Helper()
	res := run(t, s, q)
	var out []string
	for _, c := range res.Cols {
		o := c.OID
		if o == 0 {
			o = map[Kind]uint32{KInt: 20, KFloat: 701, KText: 25, KBool: 16, KNumeric: 1700, KDate: 1082, KTime: 1184, KTimestamp: 1114}[c.Kind]
		}
		out = append(out, strings.TrimSpace(strings.Join([]string{Datum{K: KInt, I: int64(o)}.String(), Datum{K: KInt, I: int64(c.Mod)}.String()}, "/")))
	}
	return strings.Join(out, " ")
}

func TestPGTypesOIDs(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE ty (id INTEGER PRIMARY KEY, s SMALLINT, b BIGINT, v VARCHAR(10), c CHAR(3), r REAL, d DATE, t TIME, x TEXT, n NUMERIC(8,2))`)
	run(t, s, `INSERT INTO ty VALUES (1, 2, 3, 'abc', 'ab', 1.5, '2026-03-04', '10:30:45.5', 'x', 1.25)`)
	cases := []struct{ q, want string }{
		{`SELECT id, s, b, v, c, r, d, t, x, n FROM ty`, "23/-1 21/-1 20/0 1043/14 1042/7 700/-1 1082/-1 1083/-1 25/0 1700/0"},
		{`SELECT * FROM ty`, "23/-1 21/-1 20/0 1043/14 1042/7 700/-1 1082/-1 1083/-1 25/0 1700/0"},
		{`SELECT id + 1, s + s, b + 1, id * 2 FROM ty`, "23/-1 21/-1 20/0 23/-1"},
		{`SELECT 1, 2147483648, length(x), coalesce(id, 0) FROM ty`, "23/-1 20/0 23/-1 23/-1"},
		{`SELECT count(*), sum(id), min(id), max(s) FROM ty`, "20/0 20/0 23/-1 21/-1"},
		{`SELECT 1::int, 1::smallint, 1::bigint, 'a'::varchar(3), 1.5::real, 'x'::name, 5 & 3, 1 << 4`, "23/-1 21/-1 20/0 1043/7 700/-1 19/-1 23/-1 23/-1"},
		{`SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x)`, "23/-1"},
		{`SELECT id FROM (SELECT id FROM ty) q`, "23/-1"},
		{`SELECT id FROM ty UNION SELECT id FROM ty`, "23/-1"},
		{`SELECT row_number() OVER (ORDER BY id), lag(id) OVER (ORDER BY id) FROM ty`, "20/0 23/-1"},
		{`SELECT avg(id) FROM ty`, "1700/0"},
		{`SELECT pg_typeof(id), pg_typeof(1), pg_typeof(s + 1) FROM ty`, "25/0 25/0 25/0"},
		{`INSERT INTO ty (id) VALUES (9) RETURNING id, v`, "23/-1 1043/14"},
	}
	for _, c := range cases {
		if got := colOIDs(t, s, c.q); got != c.want {
			t.Errorf("%s\n got  %s\n want %s", c.q, got, c.want)
		}
	}
	if got := render(run(t, s, `SELECT pg_typeof(id), pg_typeof(s), pg_typeof(v), pg_typeof(c), pg_typeof(r), pg_typeof(d), pg_typeof(t), pg_typeof(1), pg_typeof(id + 1), pg_typeof(COALESCE(NULL, 1)) FROM ty WHERE id = 1`)); got !=
		"integer,smallint,character varying,character,real,date,time without time zone,integer,integer,integer" {
		t.Errorf("pg_typeof: %s", got)
	}
	// prepared statements describe the same types
	st, _ := ParseOne(`SELECT id, v FROM ty WHERE id = $1`)
	d, err := s.Describe(st)
	if err != nil || d.Cols[0].OID != 23 || d.Cols[1].OID != 1043 || d.Cols[1].Mod != 14 {
		t.Errorf("describe: %+v %v", d, err)
	}
	// values read back
	if got := render(run(t, s, `SELECT id, s, v, c, r, d, t FROM ty WHERE id = 1`)); got != "1,2,abc,ab,1.5,2026-03-04,10:30:45.5" {
		t.Errorf("values: %s", got)
	}
}

func TestPGTypeEnforcement(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE en (id INTEGER PRIMARY KEY, s SMALLINT, v VARCHAR(3), c CHAR(2), r REAL, t TIME, d DATE)`)
	run(t, s, `INSERT INTO en (id) VALUES (100)`)
	bad := []struct{ q, code string }{
		{`INSERT INTO en (id) VALUES (2147483648)`, "22003"},
		{`INSERT INTO en (id, s) VALUES (1, 32768)`, "22003"},
		{`INSERT INTO en (id, v) VALUES (1, 'abcd')`, "22001"},
		{`INSERT INTO en (id, c) VALUES (1, 'abc')`, "22001"},
		{`INSERT INTO en (id, t) VALUES (1, '25:00')`, "22007"},
		{`INSERT INTO en (id, d) VALUES (1, '2026-02-30')`, "22008"},
		{`INSERT INTO en (id, d) VALUES (1, 'x')`, "22007"},
		{`SELECT 2147483647 + 1`, "22003"},
		{`SELECT 2147483647 * 2`, "22003"},
		{`SELECT 32768::smallint`, "22003"},
		{`SELECT '99999999999'::int`, "22003"},
		{`SELECT 2147483648::int`, "22003"},
		{`SELECT sqrt(-1)`, "2201F"},
		{`SELECT ln(0)`, "2201E"},
		{`SELECT power(0, -1)`, "2201F"},
		{`SELECT chr(0)`, "54000"},
		{`SELECT substring('abc', 1, -1)`, "22011"},
		{`SELECT 1 FROM en LIMIT -1`, "2201W"},
		{`SELECT 1 AND TRUE`, "42804"},
		{`SELECT NOT 1`, "42804"},
		{`SELECT 1 FROM en WHERE id`, "42804"},
		{`SELECT 1::nosuchtype`, "42704"},
		{`CREATE TABLE dupc (a INT PRIMARY KEY, a INT)`, "42701"},
		{`SELECT t.x FROM (SELECT 1 AS a) s`, "42P01"},
		{`SELECT 1 ORDER BY 5`, "42P10"},
		{`SELECT 1e308::float8 * 10`, "22003"},
	}
	for _, c := range bad {
		if _, err := tryRun(s, c.q); code(err) != c.code {
			t.Errorf("%s: code %q (%v), want %s", c.q, code(err), err, c.code)
		}
	}
	run(t, s, `INSERT INTO en VALUES (1, 32767, 'abc', 'ab', 0.1, '10:30', '2026-01-15')`)
	run(t, s, `INSERT INTO en (id, v) VALUES (2, 'ab ')`) // trailing blanks beyond n are dropped
	if got := render(run(t, s, `SELECT v, r, t FROM en WHERE id < 100 ORDER BY id`)); got != "abc,0.1,10:30:00;ab ,NULL,NULL" {
		t.Errorf("values: %s", got)
	}
	if got := render(run(t, s, `SELECT 'abcdef'::varchar(3), 1.5::int, '10:30'::time, 0.1::real`)); got != "abc,2,10:30:00,0.1" {
		t.Errorf("casts: %s", got)
	}
}

func TestPGCastColumnNames(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE cn (id INTEGER PRIMARY KEY, n TEXT)`)
	res := run(t, s, `SELECT 1::int, 1::bigint, 1::smallint, 'a'::text, 1.5::numeric, true::boolean, now()::date, id::text, substring('hello' FROM 2), position('l' IN 'hello'), (SELECT count(*) FROM cn), 'a' SIMILAR TO 'a', n IS DISTINCT FROM n FROM cn`)
	var names []string
	for _, c := range res.Cols {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "int4,int8,int2,text,numeric,bool,now,id,substring,position,count,?column?,?column?" {
		t.Errorf("names: %s", got)
	}
}

func TestWithDataModifyingCTE(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE w (id INTEGER PRIMARY KEY, v TEXT, n INTEGER)`)
	run(t, s, `INSERT INTO w VALUES (1,'a',10),(2,'b',20)`)
	steps := []struct{ q, want string }{
		{`WITH ins AS (INSERT INTO w VALUES (9, 'z', 1) RETURNING id) SELECT id FROM ins`, "9"},
		{`WITH upd AS (UPDATE w SET n = n + 1 WHERE id = 1 RETURNING n) SELECT n FROM upd`, "11"},
		{`WITH del AS (DELETE FROM w WHERE id = 9 RETURNING id) SELECT COUNT(*) FROM del`, "1"},
		{`WITH del AS (DELETE FROM w WHERE id = 9 RETURNING id) SELECT id FROM del`, ""},
		{`WITH t AS (SELECT id, n * 2 AS d FROM w) UPDATE w o SET n = t.d FROM t WHERE o.id = t.id`, ""},
		{`SELECT id, n FROM w ORDER BY id`, "1,22;2,40"},
		{`UPDATE w SET (v, n) = ('p', 7) WHERE id = 1`, ""},
		{`UPDATE w SET (v, n) = ROW('q', 8) WHERE id = 2`, ""},
		{`SELECT v, n FROM w ORDER BY id`, "p,7;q,8"},
		{`WITH moved AS (DELETE FROM w WHERE id = 2 RETURNING id, v, n) INSERT INTO w SELECT id + 10, v, n FROM moved`, ""},
		{`SELECT id FROM w ORDER BY id`, "1;12"},
	}
	for _, c := range steps {
		res, err := tryRun(s, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		if res.HasRows && render(res) != c.want {
			t.Errorf("%s = %q, want %q", c.q, render(res), c.want)
		}
	}
	// the data-modifying CTE and the main statement are one transaction
	run(t, s, `BEGIN`)
	run(t, s, `WITH ins AS (INSERT INTO w VALUES (50, 'r', 1) RETURNING id) SELECT id FROM ins`)
	run(t, s, `ROLLBACK`)
	if got := render(run(t, s, `SELECT count(*) FROM w WHERE id = 50`)); got != "0" {
		t.Errorf("rollback: %s", got)
	}
	if _, err := tryRun(s, `WITH ins AS (INSERT INTO w VALUES (60, 'r', 1) RETURNING id) SELECT 1/0 FROM ins`); code(err) != "22012" {
		t.Errorf("error in main: %v", err)
	}
	if got := render(run(t, s, `SELECT count(*) FROM w WHERE id = 60`)); got != "0" {
		t.Errorf("autocommit statement not atomic: %s", got)
	}
	// Describe types the CTE result from RETURNING
	st, _ := ParseOne(`WITH ins AS (INSERT INTO w VALUES ($1, 'z', 1) RETURNING id) SELECT id FROM ins`)
	d, err := s.Describe(st)
	if err != nil || len(d.Cols) != 1 || d.Cols[0].OID != 23 || len(d.Params) != 1 {
		t.Errorf("describe: %+v %v", d, err)
	}
}

func TestOperators(t *testing.T) {
	s := NewSession(newEngine(t))
	cases := []struct{ q, want string }{
		{`SELECT 5 & 3, 5 | 3, 5 # 3, ~5, 1 << 4, 256 >> 2`, "1,7,6,-6,16,64"},
		{`SELECT 2 ^ 10, 2 ^ 3 ^ 2, -2 ^ 2, 1 + 2 * 3 ^ 2`, "1024,64,4,19"},
		{`SELECT 1 | 2 + 4, 6 & 3 = 2`, "7,t"},
		{`SELECT NULL::boolean IS UNKNOWN, true IS NOT UNKNOWN`, "t,t"},
		{`SELECT 'a_b' LIKE 'a!_b' ESCAPE '!', 'axb' LIKE 'a!_b' ESCAPE '!', 'a%' LIKE 'a|%' ESCAPE '|'`, "t,f,t"},
		{`SELECT 'abc' SIMILAR TO '(a|b)%', 'abc' NOT SIMILAR TO 'x%', 'a.c' SIMILAR TO 'a.c', 'abc' SIMILAR TO 'a.c'`, "t,t,t,f"},
		{`SELECT 'hello' ^@ 'he', 'hello' ^@ 'lo'`, "t,f"},
		{`SELECT '-1 day'::interval`, "-1 days"},
	}
	for _, c := range cases {
		if got := render(run(t, s, c.q)); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
}
