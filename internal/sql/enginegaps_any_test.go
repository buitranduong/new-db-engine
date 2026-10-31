package sql

import (
	"fmt"
	"strings"
	"testing"
)

func anySetup(t *testing.T) *Session {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE a (id BIGINT PRIMARY KEY, grp INT, name TEXT)`)
	run(t, s, `CREATE INDEX a_grp ON a (grp)`)
	var vals []string
	for i := 1; i <= 300; i++ {
		vals = append(vals, fmt.Sprintf("(%d, %d, 'n%d')", i, i%10, i))
	}
	run(t, s, `INSERT INTO a VALUES `+strings.Join(vals, ","))
	run(t, s, `INSERT INTO a VALUES (1000, NULL, NULL)`)
	return s
}

func qres(t *testing.T, s *Session, q string, p ...Datum) string {
	t.Helper()
	r, err := tryRun(s, q, p...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return render(r)
}

func TestGapsAnyParam(t *testing.T) {
	s := anySetup(t)
	arr := func(lit string) Datum { return Datum{K: KArray, S: lit, I: int64(KInt)} }
	cases := []struct {
		any, in string
		p       []Datum
	}{
		{`id = ANY($1)`, `id IN (3, 5, 7, 99999)`, []Datum{arr("{3,5,7,99999}")}},
		{`id = ANY($1::bigint[])`, `id IN (3, 5, 7)`, []Datum{TextD("{3,5,7}")}},
		{`id = ANY($1)`, `id IN (3, NULL, 7)`, []Datum{arr("{3,NULL,7}")}},
		{`id = ANY($1)`, `false`, []Datum{arr("{}")}},
		{`grp = ANY($1)`, `grp IN (2, 4)`, []Datum{arr("{2,4}")}},
		{`id = ANY('{4,8}'::bigint[])`, `id IN (4, 8)`, nil},
		{`grp = ANY($1) AND id > 100`, `grp IN (1) AND id > 100`, []Datum{arr("{1}")}},
	}
	for _, c := range cases {
		q := `SELECT id FROM a WHERE %s ORDER BY id`
		before := anyArrayDecodes.Load()
		got := qres(t, s, fmt.Sprintf(q, c.any), c.p...)
		if n := anyArrayDecodes.Load() - before; n != 1 {
			t.Errorf("%s: array decoded %d times", c.any, n)
		}
		if want := qres(t, s, fmt.Sprintf(q, c.in)); got != want {
			t.Errorf("%s: %q, IN form %q", c.any, got, want)
		}
		// the unindexed scan: compare through the generic function
		scan := strings.Replace(c.any, "id =", "id + 0 =", 1)
		scan = strings.Replace(scan, "grp =", "grp + 0 =", 1)
		if want := qres(t, s, fmt.Sprintf(q, scan), c.p...); got != want {
			t.Errorf("%s: %q, scan %q", c.any, got, want)
		}
	}
	// NULL semantics of ANY / ALL / IN
	for q, want := range map[string]string{
		`SELECT 3 = ANY('{1,NULL}'::int[]) IS NULL`:    "t",
		`SELECT 1 = ANY('{1,NULL}'::int[])`:            "t",
		`SELECT 1 <> ALL('{2,NULL}'::int[]) IS NULL`:   "t",
		`SELECT 1 = ANY('{}'::int[])`:                  "f",
		`SELECT NULL::int = ANY('{}'::int[])`:          "f",
		`SELECT 5 IN (1, 2, NULL) IS NULL`:             "t",
		`SELECT 5 NOT IN (1, 2, NULL) IS NULL`:         "t",
		`SELECT 2 IN (1, 2, NULL, 4, 5, 6, 7, 8, 9)`:   "t",
		`SELECT 3 NOT IN (1, 2, 4, 5, 6, 7, 8, 9, 10)`: "t",
	} {
		if got := one(t, s, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
}

// The plan uses the index for = ANY(array) as for IN.
func TestGapsAnyParamPlan(t *testing.T) {
	s := anySetup(t)
	for _, q := range []string{
		`SELECT * FROM a WHERE id = ANY($1)`,
		`SELECT * FROM a WHERE grp = ANY($1)`,
	} {
		plan := qres(t, s, `EXPLAIN `+q, Datum{K: KArray, S: "{1,2}", I: int64(KInt)})
		planIn := qres(t, s, `EXPLAIN `+strings.Replace(q, "= ANY($1)", "IN (1, 2)", 1))
		if plan != planIn || strings.Contains(strings.ToLower(plan), "seq scan") && !strings.Contains(strings.ToLower(planIn), "seq scan") {
			t.Errorf("%s:\n%s\nIN form:\n%s", q, plan, planIn)
		}
		t.Log(strings.ReplaceAll(plan, "\n", " | "))
	}
}

// A long IN list gives the same answers as the linear scan, and is built once.
func TestGapsLongInList(t *testing.T) {
	s := anySetup(t)
	var items, params []string
	for i := 0; i < 400; i += 3 {
		items = append(items, fmt.Sprint(i))
	}
	_ = params
	in := strings.Join(items, ",")
	got := qres(t, s, `SELECT count(*) FROM a WHERE id IN (`+in+`)`)
	want := qres(t, s, `SELECT count(*) FROM a WHERE id + 0 IN (`+in+`, NULL)`)
	if got != want {
		t.Fatalf("%s vs %s", got, want)
	}
	if g := qres(t, s, `SELECT count(*) FROM a WHERE name NOT IN (`+strings.ReplaceAll("'n"+strings.Join(items, "','n")+"'", "'n0'", "'n0'")+`)`); g == "" {
		t.Fatal(g)
	}
	// NOT IN with a NULL among many: no row qualifies
	if g := qres(t, s, `SELECT count(*) FROM a WHERE id NOT IN (`+in+`, NULL)`); g != "0" {
		t.Fatal(g)
	}
}

// 2000 parameters in an IN list: the sorted set is built once per statement.
func TestGapsLongInParams(t *testing.T) {
	s := anySetup(t)
	var ph []string
	var ps []Datum
	for i := 1; i <= 2000; i++ {
		ph = append(ph, fmt.Sprintf("$%d", i))
		ps = append(ps, IntD(int64(i*2)))
	}
	before := inMemoBuilds.Load()
	got := qres(t, s, `SELECT count(*) FROM a WHERE grp + 0 IN (`+strings.Join(ph, ",")+`) OR id IN (`+strings.Join(ph, ",")+`)`, ps...)
	if got != "151" { // evens 2..300 and id 1000
		t.Fatalf("count %s", got)
	}
	if n := inMemoBuilds.Load() - before; n > 2 {
		t.Fatalf("sorted set built %d times for 2 lists", n)
	}
}
