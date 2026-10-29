package sql

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
)

func rowsStr(res *Result) string {
	var out []string
	for _, r := range res.Rows {
		var f []string
		for _, d := range r {
			f = append(f, d.String())
		}
		out = append(out, strings.Join(f, "|"))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// The hash semi/anti-join must return exactly what the nested loop returns,
// NULLs on either side included (NOT IN three-valued logic, NULL keys).
func TestSemiJoinMatchesNested(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE a (id BIGINT PRIMARY KEY, k BIGINT, k2 TEXT, v BIGINT, f DOUBLE PRECISION)`)
	run(t, s, `CREATE TABLE b (id BIGINT PRIMARY KEY, k BIGINT, k2 TEXT, v BIGINT, f DOUBLE PRECISION, flag BOOLEAN)`)
	rng := rand.New(rand.NewPCG(7, 9))
	nul := func(p int, v string) string {
		if rng.IntN(100) < p {
			return "NULL"
		}
		return v
	}
	for _, spec := range []struct {
		tbl string
		n   int
	}{{"a", 120}, {"b", 150}} {
		var vals []string
		for i := 0; i < spec.n; i++ {
			k := nul(15, fmt.Sprint(rng.IntN(40)))
			k2 := nul(15, fmt.Sprintf("'t%d'", rng.IntN(15)))
			v := nul(25, fmt.Sprint(rng.IntN(12)))
			f := nul(20, fmt.Sprint(float64(rng.IntN(40))))
			if spec.tbl == "b" {
				vals = append(vals, fmt.Sprintf("(%d,%s,%s,%s,%s,%s)", i, k, k2, v, f, nul(20, []string{"true", "false"}[rng.IntN(2)])))
			} else {
				vals = append(vals, fmt.Sprintf("(%d,%s,%s,%s,%s)", i, k, k2, v, f))
			}
		}
		run(t, s, "INSERT INTO "+spec.tbl+" VALUES "+strings.Join(vals, ","))
	}
	run(t, s, `CREATE TABLE emptyb (id BIGINT PRIMARY KEY, k BIGINT, v BIGINT)`)
	queries := []string{
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE a.k = b.k AND b.flag)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b x WHERE x.k = a.k AND x.k2 = a.k2 AND x.v > 5)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.k2 = a.k2)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k + 1 = a.k * 1)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.f = a.k)`,               // float vs int keys
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.f = a.f)`,               // float keys
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND b.v < a.v)`, // non-equi residual: nested
		`SELECT id FROM a WHERE a.v IN (SELECT b.v FROM b WHERE b.k = a.k)`,
		`SELECT id FROM a WHERE a.v NOT IN (SELECT b.v FROM b WHERE b.k = a.k)`,
		`SELECT id FROM a WHERE a.v NOT IN (SELECT b.v FROM b WHERE b.k = a.k AND b.k2 = a.k2)`,
		`SELECT id FROM a WHERE NOT (a.v IN (SELECT b.v FROM b WHERE b.k = a.k))`,
		`SELECT id FROM a WHERE a.v NOT IN (SELECT v FROM b)`,
		`SELECT id FROM a WHERE a.v IN (SELECT v FROM b)`,
		`SELECT id FROM a WHERE a.v NOT IN (SELECT b.v FROM b WHERE b.v IS NOT NULL AND b.k = a.k)`,
		`SELECT id, EXISTS (SELECT 1 FROM b WHERE b.k = a.k) AS e, a.v IN (SELECT b.v FROM b WHERE b.k = a.k) AS i, a.v NOT IN (SELECT b.v FROM b WHERE b.k = a.k) AS ni FROM a`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM emptyb WHERE emptyb.k = a.k)`,
		`SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM emptyb WHERE emptyb.k = a.k)`,
		`SELECT id FROM a WHERE a.v NOT IN (SELECT emptyb.v FROM emptyb WHERE emptyb.k = a.k)`,
		`SELECT id FROM a WHERE a.v IN (SELECT emptyb.v FROM emptyb WHERE emptyb.k = a.k)`,
		`SELECT id FROM a WHERE a.v NOT IN (SELECT v FROM emptyb)`,
		`SELECT k, count(*) FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.k = a.k) GROUP BY k`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k) AND NOT EXISTS (SELECT 1 FROM b WHERE b.k2 = a.k2 AND b.v = 3)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k LIMIT 1)`,                    // not rewritten
		`SELECT id FROM a WHERE EXISTS (SELECT count(*) FROM b WHERE b.k = a.k HAVING count(*) > 2)`, // not rewritten
		`SELECT id FROM a WHERE a.k2 = 'x' OR NOT EXISTS (SELECT 1 FROM b WHERE b.k2 = a.k2)`,
		`SELECT a.id FROM a JOIN b ON b.k = a.k WHERE NOT EXISTS (SELECT 1 FROM b b2 WHERE b2.k = a.k AND b2.id <> b.id)`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k AND EXISTS (SELECT 1 FROM b b3 WHERE b3.k = b.k AND b3.v = a.v))`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k = a.k2)`, // bigint vs text: nested semantics (error or empty), same on both paths
	}
	wantHash := 0
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
			wantHash++
		}
		n := 0
		if werr == nil {
			n = len(want.Rows)
		}
		t.Logf("rows=%-4d rewritten=%v  %s", n, decorrelated.Load() > before, q)
	}
	if wantHash < 20 {
		t.Fatalf("only %d of %d queries took the hash path", wantHash, len(queries))
	}
}
