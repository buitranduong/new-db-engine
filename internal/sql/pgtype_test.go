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

func TestPGFormatFunctions(t *testing.T) {
	s := NewSession(newEngine(t))
	cases := []struct{ q, want string }{
		{`SELECT to_char(1234.5, 'FM9999.00'), to_char(0.5, '0.00'), to_char(1234567, 'FM9,999,999'), to_char(-5, 'FM000')`, "1234.50, 0.50,1,234,567,-005"},
		{`SELECT to_char(12345.678::numeric, 'FM999,999,999.00'), to_char(-12.5, '999.9'), to_char(7, '000')`, "12,345.68, -12.5, 007"},
		{`SELECT to_timestamp('2026-03-15 10:20:30', 'YYYY-MM-DD HH24:MI:SS')`, "2026-03-15 10:20:30+00"},
		{`SELECT to_date('15/03/2026', 'DD/MM/YYYY'), to_timestamp('15 Mar 2026 03:04 PM', 'DD Mon YYYY HH12:MI AM')`, "2026-03-15,2026-03-15 15:04:00+00"},
		{`SELECT '2026-01-15 10:00:00+00'::timestamptz AT TIME ZONE 'Asia/Ho_Chi_Minh', '2026-01-15 10:00:00'::timestamp AT TIME ZONE 'UTC'`, "2026-01-15 17:00:00,2026-01-15 10:00:00+00"},
		{`SELECT age('2026-03-15'::date, '2025-01-10'::date), age('2026-03-05'::date, '2026-01-10'::date), age('2025-01-10'::date, '2026-03-15'::date)`,
			"1 year 2 mons 5 days,1 mon 26 days,-1 years -2 mons -5 days"},
		{`SELECT quote_ident('a b'), quote_literal('a''b'), quote_nullable(NULL), translate('12345', '143', 'ax'), bit_length('a')`, `"a b",'a''b',NULL,a2x5,8`},
		{`SELECT (regexp_match('foo123bar', '([0-9]+)'))[1], regexp_split_to_array('a1b2c', '[0-9]')`, "123,{a,b,c}"},
	}
	for _, c := range cases {
		if got := render(run(t, s, c.q)); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
	for q, code := range map[string]string{
		`SELECT to_date('xx', 'YYYY')`:                               "22007",
		`SELECT to_date('2026-13-45', 'YYYY-MM-DD')`:                 "22008",
		`SELECT '2026-01-01'::timestamp AT TIME ZONE 'Nowhere/City'`: "22023",
	} {
		if _, err := tryRun(s, q); err == nil || AsError(err).Code != code {
			t.Errorf("%s: %v, want %s", q, err, code)
		}
	}
}

func TestUpdateReturningFrom(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE u1 (id INTEGER PRIMARY KEY, n INTEGER)`)
	run(t, s, `CREATE TABLE u2 (id INTEGER PRIMARY KEY, w TEXT)`)
	run(t, s, `INSERT INTO u1 VALUES (1, 10), (2, 20), (3, 30)`)
	run(t, s, `INSERT INTO u2 VALUES (1, 'x'), (3, 'z'), (9, 'q')`)
	if got := render(run(t, s, `UPDATE u1 t SET n = t.n + 1 FROM u2 s WHERE t.id = s.id RETURNING t.id, t.n, s.w`)); got != "1,11,x;3,31,z" {
		t.Errorf("update returning from: %s", got)
	}
	if got := render(run(t, s, `DELETE FROM u1 a USING u2 b WHERE a.id = b.id RETURNING a.id, b.w`)); got != "1,x;3,z" {
		t.Errorf("delete returning using: %s", got)
	}
	st, _ := ParseOne(`UPDATE u1 t SET n = 1 WHERE t.id = 1 RETURNING t.id, t.n`)
	d, err := s.Describe(st)
	if err != nil || len(d.Cols) != 2 || d.Cols[0].OID != 23 {
		t.Errorf("describe with alias: %+v %v", d, err)
	}
	// ON CONFLICT targets
	if _, err := tryRun(s, `INSERT INTO u1 VALUES (5, 5) ON CONFLICT (n) DO NOTHING`); code(err) != "42P10" {
		t.Errorf("on conflict non-unique: %v", err)
	}
	run(t, s, `INSERT INTO u1 VALUES (5, 5) ON CONFLICT (id) DO UPDATE SET n = excluded.n + 1`)
	run(t, s, `INSERT INTO u1 VALUES (5, 5) ON CONFLICT (id) DO UPDATE SET n = excluded.n + 1`)
	if got := render(run(t, s, `SELECT n FROM u1 WHERE id = 5`)); got != "6" {
		t.Errorf("upsert: %s", got)
	}
}

func TestSQLPrepareExecute(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE pe (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, s, `PREPARE ins (int, text) AS INSERT INTO pe VALUES ($1, $2)`)
	run(t, s, `EXECUTE ins(1, 'a')`)
	run(t, s, `EXECUTE ins(2, 'b')`)
	run(t, s, `PREPARE q AS SELECT v FROM pe WHERE id = $1`)
	if got := render(run(t, s, `EXECUTE q(2)`)); got != "b" {
		t.Errorf("execute: %s", got)
	}
	if _, err := tryRun(s, `PREPARE q AS SELECT 1`); code(err) != "42P05" {
		t.Errorf("duplicate prepare: %v", err)
	}
	if _, err := tryRun(s, `EXECUTE nosuch`); code(err) != "26000" {
		t.Errorf("missing prepared: %v", err)
	}
	run(t, s, `DEALLOCATE q`)
	if _, err := tryRun(s, `EXECUTE q(1)`); code(err) != "26000" {
		t.Errorf("after deallocate: %v", err)
	}
	run(t, s, `DEALLOCATE ALL`)
	if res := run(t, s, `VACUUM`); res.Tag != "VACUUM" {
		t.Errorf("vacuum tag %q", res.Tag)
	}
	if res := run(t, s, `ANALYZE pe`); res.Tag != "ANALYZE" {
		t.Errorf("analyze tag %q", res.Tag)
	}
}
