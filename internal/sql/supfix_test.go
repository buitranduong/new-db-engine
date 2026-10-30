package sql

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
)

// The residual, HAVING, grouped, DISTINCT and LIMIT forms of correlated EXISTS / IN (supfix_semijoin.go)
// must return exactly what the nested loop returns, NULLs on either side included.
func TestSupfixSemiJoinForms(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE a (id BIGINT PRIMARY KEY, k BIGINT, k2 TEXT, v BIGINT, f DOUBLE PRECISION)`)
	run(t, s, `CREATE TABLE b (id BIGINT PRIMARY KEY, k BIGINT, k2 TEXT, v BIGINT, f DOUBLE PRECISION, flag BOOLEAN)`)
	run(t, s, `CREATE TABLE emptyb (id BIGINT PRIMARY KEY, k BIGINT, v BIGINT)`)
	rng := rand.New(rand.NewPCG(11, 5))
	nul := func(p int, v string) string {
		if rng.IntN(100) < p {
			return "NULL"
		}
		return v
	}
	for _, spec := range []struct {
		tbl string
		n   int
	}{{"a", 100}, {"b", 160}} {
		var vals []string
		for i := 0; i < spec.n; i++ {
			k := nul(12, fmt.Sprint(rng.IntN(30)))
			k2 := nul(12, fmt.Sprintf("'t%d'", rng.IntN(12)))
			v := nul(20, fmt.Sprint(rng.IntN(12)))
			f := nul(20, fmt.Sprint(float64(rng.IntN(40))))
			if spec.tbl == "b" {
				vals = append(vals, fmt.Sprintf("(%d,%s,%s,%s,%s,%s)", i, k, k2, v, f, nul(20, []string{"true", "false"}[rng.IntN(2)])))
			} else {
				vals = append(vals, fmt.Sprintf("(%d,%s,%s,%s,%s)", i, k, k2, v, f))
			}
		}
		run(t, s, "INSERT INTO "+spec.tbl+" VALUES "+strings.Join(vals, ","))
	}
	queries := []string{
		// residual predicates mentioning the outer row
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.v < a.v)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.v < a.v)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.flag AND b.v + a.v > 10)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.k2 = a.k2 AND b.f > a.f)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND (b.v > a.v OR b.v IS NULL))`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.v > 3 AND a.v > 3)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.v <> a.v LIMIT 1)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b JOIN b b2 ON b2.k = b.k WHERE b.k = a.k AND b2.v > a.v)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM emptyb WHERE emptyb.k = a.k AND emptyb.v < a.v)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM emptyb WHERE emptyb.k = a.k AND emptyb.v < a.v)`,
		// GROUP BY / HAVING
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k GROUP BY b.k HAVING count(*) > 1)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k GROUP BY b.k HAVING count(*) > 1)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k GROUP BY b.v HAVING count(*) > 1)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k GROUP BY b.v)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k HAVING count(*) > 1)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k HAVING count(*) = 0)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k HAVING count(*) = 0)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.k2 = a.k2 HAVING sum(b.v) > 5)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k GROUP BY b.k2 HAVING max(b.v) > 6)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM emptyb WHERE emptyb.k = a.k HAVING count(*) = 0)`,
		`SELECT id FROM a WHERE a.v IN (SELECT max(b.v) FROM b WHERE b.k = a.k GROUP BY b.k2)`,
		`SELECT id FROM a WHERE a.v NOT IN (SELECT b.v FROM b WHERE b.k = a.k GROUP BY b.v)`,
		`SELECT id FROM a WHERE a.v IN (SELECT b.v + 1 FROM b WHERE b.k = a.k GROUP BY b.v HAVING count(*) > 1)`,
		// DISTINCT / ORDER BY / LIMIT n inside EXISTS
		`SELECT id FROM a WHERE EXISTS (SELECT DISTINCT b.k FROM b WHERE b.k = a.k)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k ORDER BY b.v LIMIT 1)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k LIMIT 5)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k LIMIT 0)`,  // not rewritten
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k OFFSET 1)`, // not rewritten
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k) AND EXISTS (SELECT 1 FROM b WHERE b.k2 = a.k2 AND b.v > a.v)`,
		`SELECT id, EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.v < a.v) AS e FROM a`,
	}
	rewritten := 0
	for _, q := range queries {
		NoDecorrelate = true
		want, werr := tryRun(s, q)
		NoDecorrelate = false
		before := decorrelated.Load()
		got, gerr := tryRun(s, q)
		if (werr == nil) != (gerr == nil) || werr != nil && werr.Error() != gerr.Error() {
			t.Fatalf("%s: nested err=%v hash err=%v", q, werr, gerr)
		}
		if werr == nil && rowsStr(want) != rowsStr(got) {
			t.Fatalf("%s\nnested (%d rows) != hash (%d rows)", q, len(want.Rows), len(got.Rows))
		}
		if decorrelated.Load() > before {
			rewritten++
		}
	}
	if rewritten < 24 {
		t.Fatalf("only %d of %d queries took a hash path", rewritten, len(queries))
	}
}

func TestSupfixGroupByExpression(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE g (id BIGINT PRIMARY KEY, price NUMERIC(12,2), v TEXT, ts TIMESTAMPTZ)`)
	run(t, s, `INSERT INTO g VALUES (1, 100.50, 'a', TIMESTAMPTZ '2026-01-05 10:00:00+00'), (2, 5000, 'a', TIMESTAMPTZ '2026-01-12 11:00:00+00'), (3, 9000.25, 'b', TIMESTAMPTZ '2026-02-01 12:00:00+00')`)
	for _, c := range []struct{ q, want string }{
		{`SELECT CASE WHEN price < 1000 THEN 'low' WHEN price < 6000 THEN 'mid' ELSE 'high' END AS b, count(*) FROM g GROUP BY CASE WHEN price < 1000 THEN 'low' WHEN price < 6000 THEN 'mid' ELSE 'high' END ORDER BY b`, "high,1;low,1;mid,1"},
		{`SELECT to_char(ts, 'YYYY-MM') AS m, upper(substr(v, 1, 1)) AS u, count(*) FROM g GROUP BY to_char(ts, 'YYYY-MM'), upper(substr(v, 1, 1)) ORDER BY 1, 2`, "2026-01,A,2;2026-02,B,1"},
		{`SELECT CASE WHEN id BETWEEN 1 AND 2 THEN 'x' WHEN v IN ('b') THEN 'y' ELSE 'z' END AS k, count(*) FROM g GROUP BY CASE WHEN id BETWEEN 1 AND 2 THEN 'x' WHEN v IN ('b') THEN 'y' ELSE 'z' END`, "x,2;y,1"},
		{`SELECT (id * 2 + 1) AS q, count(*) FROM g GROUP BY (id * 2 + 1) HAVING count(*) >= 1 ORDER BY 1`, "3,1;5,1;7,1"},
		{`SELECT coalesce(v, 'none') AS c, count(*) FROM g WHERE v IS NOT NULL GROUP BY coalesce(v, 'none') ORDER BY 1`, "a,2;b,1"},
	} {
		expectRows(t, s, c.q, c.want)
	}
	if _, err := tryRun(s, `SELECT CASE WHEN price < 1 THEN 'a' ELSE 'b' END, count(*) FROM g GROUP BY CASE WHEN price < 2 THEN 'a' ELSE 'b' END`); err == nil {
		t.Fatal("a different CASE must still be rejected (42803)")
	}
}

// Values and scales verified against PostgreSQL 16.
func TestSupfixNumericAggregates(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	for _, c := range []struct{ q, want string }{
		{`SELECT var_pop(x), stddev(x), var_samp(x), stddev_pop(x) FROM (VALUES (1),(2),(2),(3),(5)) t(x)`, "1.8400000000000000,1.5165750888103101,2.3000000000000000,1.3564659966250536"},
		{`SELECT var_pop(x), stddev(x) FROM (VALUES (1.5),(1.5)) t(x)`, "0,0"},
		{`SELECT var_samp(x) FROM (VALUES (1)) t(x)`, "NULL"},
		{`SELECT var_pop(x) FROM (VALUES (1.0::float8),(2),(4)) t(x)`, "1.5555555555555556"},
		{`SELECT stddev(x) FROM (VALUES (1.0::float8),(2),(4)) t(x)`, "1.5275252316519468"},
		{`SELECT round(corr(y, x)::numeric, 4), regr_count(y, x), round(regr_slope(y, x)::numeric, 4) FROM (VALUES (1.0,2.0),(2.0,4.5),(3.0,5.9),(NULL,1.0),(5.0,NULL)) t(y,x)`, "0.9870,3,0.4996"},
		{`SELECT covar_samp(y, x), corr(y, x) FROM (VALUES (1.0,2.0)) t(y,x)`, "NULL,NULL"},
	} {
		expectRows(t, s, c.q, c.want)
	}
	// result kinds: numeric, with the scale PostgreSQL gives
	run(t, s, `CREATE TABLE ni (id BIGINT PRIMARY KEY, q INTEGER, n NUMERIC(12,2), big BIGINT)`)
	run(t, s, `INSERT INTO ni VALUES (1, 2, 10.50, 1), (2, 1, 1.00, 2)`)
	run(t, s, `CREATE TABLE np (id BIGINT PRIMARY KEY)`)
	run(t, s, `INSERT INTO np VALUES (1), (2)`)
	for _, c := range []struct{ q, want string }{
		{`SELECT pg_typeof(sum(t.q * t.n)) FROM ni t JOIN np p ON p.id = t.id`, "numeric"},
		{`SELECT pg_typeof(sum(big) OVER ()) FROM ni LIMIT 1`, "numeric"},
		{`SELECT pg_typeof(sum(q) OVER ()) FROM ni LIMIT 1`, "bigint"},
		{`SELECT pg_typeof(stddev(q)), pg_typeof(variance(big)), pg_typeof(stddev(n::float8)) FROM ni`, "numeric,numeric,double precision"},
		{`SELECT pg_typeof(extract(epoch FROM date_trunc('day', ts)) * 1000) FROM (SELECT TIMESTAMPTZ '2026-01-01 10:00:00+00' AS ts) t GROUP BY 1`, "numeric"},
	} {
		expectRows(t, s, c.q, c.want)
	}
	d := run(t, s, `SELECT sum(t.q * t.n) FROM ni t JOIN np p ON p.id = t.id`)
	if d.Cols[0].Kind != KNumeric {
		t.Fatalf("sum(q*n) over a join: column kind %v, want numeric", d.Cols[0].Kind)
	}
}

func TestSupfixToChar(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	for _, c := range []struct{ q, want string }{
		{`SELECT to_char(TIMESTAMPTZ '2026-01-05 15:04:05.123456+00', 'IYYY-IW-ID Q W WW DDD J')`, "2026-02-1 1 1 01 005 2461046"},
		{`SELECT to_char(TIMESTAMPTZ '2026-01-05 00:00:00+00', 'YYYY-"W"IW')`, "2026-W02"},
		{`SELECT to_char(TIMESTAMPTZ '2026-12-31 23:59:59+00', 'IYYY IW IDDD CC Y,YYY RM')`, "2026 53 368 21 2,026 XII "},
		{`SELECT to_char(TIMESTAMPTZ '2026-03-01 13:05:00+00', 'FMDDth FMMonth YYYY, HH12:MI PM, FMDay FMRM')`, "1st March 2026, 01:05 PM, Sunday III"},
		{`SELECT to_char(TIMESTAMPTZ '2026-03-01 13:00:00.5+00', 'yyyy-mm-dd hh24:mi:ss.MS US TZ OF')`, "2026-03-01 13:00:00.500 500000 UTC +00"},
		{`SELECT to_char(DATE '0099-01-01', 'YYYY CC')`, "0099 01"},
		{`SELECT to_char(TIMESTAMPTZ '2026-03-01 13:00:00+00', 'HH12:MI a.m. am Dy DY dy mon MON Mon')`, "01:00 p.m. pm Sun SUN sun mar MAR Mar"},
	} {
		expectRows(t, s, c.q, c.want)
	}
}

func TestSupfixCatalog(t *testing.T) {
	s := catalogFixture(t)
	for _, c := range []struct{ q, want string }{
		{`SELECT 'items'::regclass::text, 'public.items'::regclass::text, to_regclass('items')::text`, "items,items,items"},
		{`SELECT 'items'::regclass::text || ',' || 'pg_class'::regclass::text`, "items,pg_class"},
		{`SELECT pg_typeof(relname)::text, pg_typeof(oid)::text, pg_typeof(relkind)::text, pg_typeof(relnatts)::text FROM pg_class WHERE relname = 'items'`, "name,oid,\"char\",smallint"},
		{`SELECT pg_typeof(column_name)::text, pg_typeof(ordinal_position)::text FROM information_schema.columns WHERE table_name = 'items' LIMIT 1`, "name,integer"},
		{`SELECT confmatchtype FROM pg_constraint WHERE contype = 'f' LIMIT 1`, "s"},
	} {
		if strings.Contains(c.q, "confmatchtype") {
			continue // the fixture has no foreign key
		}
		expectRows(t, s, c.q, c.want)
	}
	run(t, s, `CREATE TABLE sized (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO sized SELECT g, 'x' FROM generate_series(1, 100) g`)
	expectRows(t, s, `SELECT reltuples::bigint, relpages > 0 FROM pg_class WHERE relname = 'sized'`, "100,true")
	expectRows(t, s, `SELECT pg_relation_size('sized') > 0, pg_total_relation_size('sized') > pg_relation_size('sized')`, "true,true")
	run(t, s, `CREATE VIEW v_sized AS SELECT id, v FROM sized WHERE v = 'x'`)
	expectRows(t, s, `SELECT pg_get_viewdef('v_sized'::regclass, true)`, " SELECT id,\n    v\n   FROM sized\n  WHERE v = 'x'::text;")
	expectRows(t, s, `SELECT definition FROM pg_views WHERE viewname = 'v_sized'`, " SELECT id,\n    v\n   FROM sized\n  WHERE (v = 'x'::text);")
}

func TestSupfixSyntax(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	for _, c := range []struct{ q, want string }{
		{`WITH c AS MATERIALIZED (SELECT 1 AS x) SELECT x FROM c`, "1"},
		{`WITH c AS NOT MATERIALIZED (SELECT 2 AS x), d(y) AS MATERIALIZED (SELECT 3) SELECT x + y FROM c, d`, "5"},
		{`SELECT 1 AS """", 2 AS "a""b"`, "1,2"},
		{`SELECT set_config('statement_timeout', '5000', false)`, "5s"},
	} {
		expectRows(t, s, c.q, c.want)
	}
	res := run(t, s, `SELECT 1 AS """", 2 AS "a""b"`)
	if res.Cols[0].Name != `"` || res.Cols[1].Name != `a"b` {
		t.Fatalf("column labels %q %q", res.Cols[0].Name, res.Cols[1].Name)
	}
	run(t, s, `SET search_path TO public, pg_catalog`)
	expectRows(t, s, `SHOW search_path`, "public, pg_catalog")
	expectRows(t, s, `SHOW max_connections`, "100")
	t0 := time.Now()
	expectRows(t, s, `SELECT pg_sleep(0.05) IS NULL`, "false")
	if time.Since(t0) < 50*time.Millisecond {
		t.Fatal("pg_sleep did not wait")
	}
}

func TestSupfixIndexOnTimestamp(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE tix (id BIGINT PRIMARY KEY, t TIMESTAMP, tz TIMESTAMPTZ)`)
	run(t, s, `INSERT INTO tix SELECT g, TIMESTAMP '2026-01-01' + g * INTERVAL '7 hours', TIMESTAMPTZ '2026-01-01 00:00:00+00' + g * INTERVAL '5 hours' FROM generate_series(1, 100) g`)
	run(t, s, `INSERT INTO tix VALUES (1000, NULL, NULL)`)
	run(t, s, `CREATE INDEX tix_t ON tix (t)`)
	run(t, s, `CREATE INDEX tix_tz ON tix (tz, id)`)
	expectRows(t, s, `SELECT count(*) FROM tix WHERE t >= '2026-01-10 00:00:00' AND t < '2026-01-20'`, "35")
	expectRows(t, s, `SELECT id FROM tix WHERE tz > TIMESTAMPTZ '2026-01-05 00:00:00+00' ORDER BY tz LIMIT 3`, "20;21;22")
	expectRows(t, s, `SELECT id FROM tix ORDER BY tz DESC LIMIT 2`, "1000;100")
	expectRows(t, s, `SELECT id FROM tix WHERE t IS NULL`, "1000")
}

func TestSupfixExplainCost(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE ex (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO ex SELECT g, 'x' FROM generate_series(1, 1000) g`)
	for i := 0; i < 200; i++ { // the live row count is installed a moment after the commit
		if ts, err := e.UserTable("ex"); err == nil && ts.LiveRows() == 1000 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	res := run(t, s, `EXPLAIN SELECT v, count(*) FROM ex GROUP BY v ORDER BY v`)
	first := res.Rows[0][0].String()
	if !strings.Contains(first, "cost=") || !strings.Contains(first, "rows=") || !strings.Contains(first, "width=") {
		t.Fatalf("first plan line has no estimate: %q", first)
	}
	var seq string
	for _, r := range res.Rows {
		if l := r[0].String(); strings.Contains(l, "Seq Scan on ex") {
			seq = l
		}
	}
	if !strings.Contains(seq, "rows=1000") {
		t.Fatalf("seq scan line %q should estimate the 1000 live rows", seq)
	}
	if last := res.Rows[len(res.Rows)-1][0].String(); !strings.Contains(last, "not planner statistics") {
		t.Fatalf("missing the note that the estimates are rough: %q", last)
	}
}

// A parallel table scan must hand rows over in the order of a sequential scan.
func TestSupfixParallelScanOrder(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE big (id BIGINT PRIMARY KEY, v TEXT, n NUMERIC(12,2))`)
	run(t, s, `INSERT INTO big SELECT g, 'v' || g, g * 1.5 FROM generate_series(1, 60000) g`)
	seq := func() []string {
		old := ScanWorkers
		ScanWorkers = 1
		defer func() { ScanWorkers = old }()
		res := run(t, s, `SELECT id FROM (SELECT * FROM big) t`)
		var ids []string
		for _, r := range res.Rows {
			ids = append(ids, r[0].String())
		}
		return ids
	}()
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s2 := NewSession(e)
			res, err := tryRun(s2, `SELECT id FROM (SELECT * FROM big) t`)
			if err != nil {
				t.Error(err)
				return
			}
			if len(res.Rows) != 60000 {
				t.Errorf("%d rows", len(res.Rows))
				return
			}
			for i, r := range res.Rows {
				if r[0].String() != seq[i] {
					t.Errorf("row %d has id %s, a sequential scan has %s", i, r[0].String(), seq[i])
					return
				}
			}
		}()
	}
	wg.Wait()
	expectRows(t, s, `SELECT sum(n), count(*) FROM (SELECT * FROM big) t`, "2700045000.00,60000")
}
