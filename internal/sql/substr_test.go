package sql

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"stask/internal/planner"
	"stask/internal/proj"
)

func col0(res *Result) []string {
	var out []string
	for _, r := range res.Rows {
		out = append(out, fmt.Sprint(r[0].String()))
	}
	sort.Strings(out)
	return out
}

func lastPlan(s *Session) string { return s.vars[LastPlanVar] }

func randText(rng *rand.Rand) string {
	alpha := []rune("0123456789888abAB%_\\'é ")
	n := rng.Intn(12)
	if rng.Intn(3) == 0 {
		n = rng.Intn(3)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(alpha[rng.Intn(len(alpha))])
	}
	return b.String()
}

func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Every pattern predicate must return, through the view, what the scan returns.
func TestSubstrMatchesScan(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	scan, idx := NewSession(e), NewSession(e)
	idx.Views = p
	run(t, scan, `CREATE TABLE sim (id BIGINT PRIMARY KEY, num TEXT, carrier TEXT, price BIGINT)`)
	rng := rand.New(rand.NewSource(11))
	var all []string
	ins := func(from, to int) {
		var vals []string
		for i := from; i < to; i++ {
			num := "NULL"
			if rng.Intn(15) != 0 {
				s := randText(rng)
				all = append(all, s)
				num = sqlQuote(s)
			}
			vals = append(vals, fmt.Sprintf("(%d, %s, 'c%d', %d)", i, num, rng.Intn(4), rng.Intn(100)))
		}
		run(t, scan, "INSERT INTO sim VALUES "+strings.Join(vals, ","))
	}
	ins(0, 1500)
	// the index is created on existing rows, then rows change under it
	run(t, idx, `CREATE INDEX sim_num ON sim USING gin (num gin_trgm_ops)`)
	ins(1500, 3000)
	run(t, scan, "UPDATE sim SET num = num || 'x8' WHERE id % 7 = 0")
	run(t, scan, "DELETE FROM sim WHERE id % 11 = 0")
	run(t, scan, "UPDATE sim SET num = NULL WHERE id % 13 = 0")
	run(t, idx, "SET stask.consistency = strong")

	pats := []string{"%888%", "888%", "%888", "%8_8%", "%\\%%", "%\\_%", "a%", "%é%", "%", "%%", "", "x8", "%x8", "___", "%a_b%", "%AB%", "0%8%", "\\\\%", "%'%", "%1 %"}
	for i := 0; i < 150; i++ {
		s := all[rng.Intn(len(all))]
		r := []rune(s)
		if len(r) > 2 {
			a := rng.Intn(len(r))
			b := a + 1 + rng.Intn(len(r)-a)
			sub := strings.NewReplacer("%", "\\%", "_", "\\_", "\\", "\\\\").Replace(string(r[a:b]))
			pats = append(pats, "%"+sub+"%", sub+"%", "%"+sub, sub)
		}
	}
	checked, used := 0, 0
	for _, pat := range pats {
		for _, op := range []string{"LIKE", "ILIKE", "NOT LIKE"} {
			q := fmt.Sprintf("SELECT id FROM sim WHERE num %s %s", op, sqlQuote(pat))
			want, got := col0(run(t, scan, q)), col0(run(t, idx, q))
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Fatalf("%s: scan %d rows, view %d rows", q, len(want), len(got))
			}
			checked++
			if strings.HasPrefix(lastPlan(idx), "Trigram Scan") {
				used++
			}
			if op != "NOT LIKE" && !strings.HasPrefix(lastPlan(idx), "Trigram Scan") {
				t.Fatalf("%s: view not used: %s", q, lastPlan(idx))
			}
		}
	}
	for _, re := range []string{"^888", "888$", "888", "(?i)AB", "a.b", "^$", "8+8", "[0-9]88", "x8$", "^0.*8$", "(a|b)", "%", "\\_", "é{2}"} {
		for _, q := range []string{
			fmt.Sprintf("SELECT id FROM sim WHERE num ~ %s", sqlQuote(re)),
			fmt.Sprintf("SELECT id FROM sim WHERE num ~* %s", sqlQuote(re)),
			fmt.Sprintf("SELECT id FROM sim WHERE num !~ %s", sqlQuote(re)),
		} {
			want, got := col0(run(t, scan, q)), col0(run(t, idx, q))
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Fatalf("%s: scan %d rows, view %d rows", q, len(want), len(got))
			}
			checked++
		}
	}
	for _, k := range []string{"quad", "triple", "asc", "repeat", "palin"} {
		q := fmt.Sprintf("SELECT id FROM sim WHERE sim_pretty(num, '%s')", k)
		want, got := col0(run(t, scan, q)), col0(run(t, idx, q))
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Fatalf("%s: scan %d rows, view %d rows", q, len(want), len(got))
		}
		checked++
	}
	// with other filters, grouping and LIMIT
	for _, q := range []string{
		"SELECT carrier, count(*) FROM sim WHERE num LIKE '%88%' AND price < 50 GROUP BY carrier ORDER BY carrier",
		"SELECT count(*) FROM sim WHERE num ILIKE '%AB%' OR price = 3",
		"SELECT id FROM sim WHERE num LIKE '%8%' ORDER BY id LIMIT 20",
		"SELECT id FROM sim WHERE num LIKE '%8%' AND num LIKE '%9%' AND carrier = 'c1' ORDER BY id",
	} {
		w, g := run(t, scan, q), run(t, idx, q)
		if fmt.Sprint(w.Rows) != fmt.Sprint(g.Rows) {
			t.Fatalf("%s: differ\n%v\n%v", q, w.Rows, g.Rows)
		}
		checked++
	}
	// LIMIT without ORDER BY: the right number of matching rows
	got := run(t, idx, "SELECT id, num FROM sim WHERE num LIKE '%8%' LIMIT 25")
	if len(got.Rows) != 25 {
		t.Fatalf("LIMIT: %d rows", len(got.Rows))
	}
	for _, r := range got.Rows {
		if !strings.Contains(r[1].String(), "8") {
			t.Fatalf("LIMIT returned %q", r[1].String())
		}
	}
	t.Logf("%d statements agree with the scan; view used by %d LIKE statements", checked, used)
}

// A change is found at once under strong, and the plan says what answered.
func TestSubstrFreshness(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	w, r := NewSession(e), NewSession(e)
	r.Views = p
	run(t, w, `CREATE TABLE sim (id BIGINT PRIMARY KEY, num TEXT, carrier TEXT, price BIGINT) WITH (trigram = 'num')`)
	for !p.SubF.Ready() {
		time.Sleep(time.Millisecond)
	}
	run(t, r, "SET stask.consistency = strong")
	for i := 0; i < 300; i++ {
		num := fmt.Sprintf("090%07d", i)
		run(t, w, fmt.Sprintf("INSERT INTO sim VALUES (%d, '%s', 'viettel', 100)", i, num))
		if i%3 == 0 {
			run(t, w, fmt.Sprintf("UPDATE sim SET carrier = 'mobi', price = 250 WHERE id = %d", i/2))
		}
		res := run(t, r, fmt.Sprintf("SELECT id, carrier, price FROM sim WHERE num LIKE '%%%s'", num[5:]))
		found := false
		for _, row := range res.Rows {
			if row[0].I == int64(i) {
				found = true
			}
		}
		if !found {
			t.Fatalf("row %d not found right after its commit: %s", i, lastPlan(r))
		}
		if !strings.Contains(lastPlan(r), "engine=substr") && !strings.Contains(lastPlan(r), "Seq Scan") {
			t.Fatalf("plan: %s", lastPlan(r))
		}
		// no stale data: the updated row is seen with its new values
		if i%3 == 0 {
			res = run(t, r, fmt.Sprintf("SELECT carrier, price FROM sim WHERE num LIKE '%%%07d' AND carrier = 'mobi'", i/2))
			if len(res.Rows) != 1 || res.Rows[0][1].I != 250 {
				t.Fatalf("stale row for id %d: %v", i/2, res.Rows)
			}
		}
	}
	pl := lastPlan(r)
	for _, want := range []string{"applied_lsn=", "view_lag_ms=", "Trigram Scan"} {
		if !strings.Contains(pl, want) {
			t.Fatalf("plan %q lacks %q", pl, want)
		}
	}
	ex := run(t, r, "EXPLAIN SELECT carrier, count(*) FROM sim WHERE num LIKE '%888%' GROUP BY carrier")
	var lines []string
	for _, row := range ex.Rows {
		lines = append(lines, row[0].String())
	}
	t.Log("\n" + strings.Join(lines, "\n"))
	if !strings.Contains(strings.Join(lines, "\n"), "Trigram Scan on sim using sim_num_trgm_idx") {
		t.Fatal("EXPLAIN does not show the trigram scan")
	}
	// the other consistency settings are accepted, bad ones refused
	for _, m := range []string{"bounded", "eventual", "session"} {
		run(t, r, "SET stask.consistency = "+m)
		run(t, r, "SELECT id FROM sim WHERE num LIKE '%123%'")
	}
	if _, err := tryRun(r, "SET stask.consistency = sometimes"); err == nil {
		t.Fatal("bad consistency accepted")
	}
	run(t, r, "SET stask.max_lag_ms = 50")
	// DROP INDEX returns to the scan
	run(t, w, "DROP INDEX sim_num_trgm_idx")
	time.Sleep(50 * time.Millisecond)
	ex = run(t, r, "EXPLAIN SELECT id FROM sim WHERE num LIKE '%123%'")
	if !strings.HasPrefix(ex.Rows[0][0].String(), "Seq Scan") {
		t.Fatal("dropped index still used")
	}
}

func TestPrettyFlags(t *testing.T) {
	for s, want := range map[string]uint8{
		"0908888888": proj.PrettyQuad | proj.PrettyTriple | proj.PrettyRepeat | proj.PrettyPalin, "0901238888": proj.PrettyQuad | proj.PrettyTriple,
		"0900001234": proj.PrettyAsc, "0909121212": proj.PrettyRepeat, "0909123123": proj.PrettyRepeat,
		"0901237887": proj.PrettyPalin, "0901123321": proj.PrettyPalin, "090 123 456": proj.PrettyAsc, "12": 0, "": 0,
		"0901234777": proj.PrettyTriple,
	} {
		if got := proj.PrettyFlags(s); got != want {
			t.Errorf("%q: %b want %b", s, got, want)
		}
	}
}

// A lagging view is never read unmarked beyond the bound: bounded waits for
// it and then answers from the core, eventual reads it as it is and says so.
func TestSubstrLagContract(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	w, r := NewSession(e), NewSession(e)
	r.Views = p
	run(t, w, `CREATE TABLE sim (id BIGINT PRIMARY KEY, num TEXT, carrier TEXT, price BIGINT) WITH (trigram = 'num')`)
	run(t, w, `INSERT INTO sim VALUES (1, '0901112222', 'viettel', 100), (2, '0903334444', 'mobi', 100)`)
	run(t, r, "SET stask.consistency = strong")
	run(t, r, "SELECT id FROM sim WHERE num LIKE '%2222'") // view caught up
	release := p.Sub.Hold()
	run(t, w, `UPDATE sim SET carrier = 'vina', price = 999 WHERE id = 1`)
	run(t, w, `INSERT INTO sim VALUES (3, '0905552222', 'vina', 50)`)
	time.Sleep(120 * time.Millisecond) // the view is now >100ms behind

	q := "SELECT id, carrier, price FROM sim WHERE num LIKE '%2222' ORDER BY id"
	run(t, r, "SET stask.consistency = bounded")
	run(t, r, "SET stask.max_lag_ms = 50")
	res := run(t, r, q)
	if len(res.Rows) != 2 || res.Rows[0][1].String() != "vina" || res.Rows[0][2].I != 999 {
		t.Fatalf("bounded beyond its bound returned stale rows: %v (%s)", res.Rows, lastPlan(r))
	}
	if !strings.HasPrefix(lastPlan(r), "Seq Scan") || !strings.Contains(lastPlan(r), "engine=core") || !strings.Contains(lastPlan(r), "lag") {
		t.Fatalf("plan does not say the core answered: %s", lastPlan(r))
	}
	run(t, r, "SET stask.max_lag_ms = 5000") // within the bound the lagging view may answer, and says how far behind it is
	res = run(t, r, q)
	if !strings.Contains(lastPlan(r), "view_lag_ms=") {
		t.Fatalf("plan lacks the lag: %s", lastPlan(r))
	}
	t.Logf("bounded 5s: %d rows; %s", len(res.Rows), lastPlan(r))
	run(t, r, "SET stask.consistency = eventual")
	res = run(t, r, q)
	if !strings.Contains(lastPlan(r), "engine=substr") {
		t.Fatalf("eventual should read the view: %s", lastPlan(r))
	}
	if len(res.Rows) != 1 { // id 1 now has another carrier but its number still matches; id 3 is not in the view yet
		t.Logf("eventual rows: %v", res.Rows)
	}
	release()
	run(t, r, "SET stask.consistency = strong")
	res = run(t, r, q)
	if len(res.Rows) != 2 || res.Rows[0][2].I != 999 || !strings.Contains(lastPlan(r), "engine=substr") {
		t.Fatalf("strong after catch-up: %v %s", res.Rows, lastPlan(r))
	}
}
