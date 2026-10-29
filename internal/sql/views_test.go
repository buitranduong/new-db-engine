package sql

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"stask/internal/planner"
)

// A session answering aggregates from the columnar view must return what a
// session scanning rows returns, and use the view exactly when it may.
func TestViewAggregatesMatchRowPath(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	rows, view := NewSession(e), NewSession(e)
	view.Views = p
	run(t, rows, `CREATE TABLE o (id BIGINT PRIMARY KEY, cust BIGINT, amount DOUBLE PRECISION, qty BIGINT,
		status TEXT, label VARCHAR, created TIMESTAMPTZ) WITH (columnar = true)`)
	rng := rand.New(rand.NewSource(5))
	null := func(s string) string {
		if rng.Intn(25) == 0 {
			return "NULL"
		}
		return s
	}
	for b := 0; b < 40; b++ {
		var vals []string
		for i := 0; i < 250; i++ {
			id := b*250 + i
			cust := fmt.Sprint(rng.Intn(400))
			amount := fmt.Sprint(float64(rng.Intn(4_000_000)) / 4)
			if id%400 == 7 {
				cust, amount = "9999", "NULL" // a customer whose amounts are all NULL
			}
			vals = append(vals, fmt.Sprintf("(%d, %s, %s, %s, %s, %s, %s)", id, null(cust), null(amount),
				null(fmt.Sprint(rng.Intn(20))), null(fmt.Sprintf("'s%d'", rng.Intn(5))), null(fmt.Sprintf("'l%d'", rng.Intn(3))),
				null(fmt.Sprintf("'2026-10-%02d 10:00:00+00'", 1+rng.Intn(28)))))
		}
		run(t, rows, "INSERT INTO o VALUES "+strings.Join(vals, ","))
	}
	run(t, rows, "UPDATE o SET status = 'paid' WHERE qty = 3")
	run(t, rows, "DELETE FROM o WHERE qty = 4")
	for !p.ColF.Ready() {
		time.Sleep(time.Millisecond)
	}

	for _, c := range []struct {
		q       string
		useView bool
	}{
		{"SELECT status, count(*), sum(amount) FROM o GROUP BY status", true},
		{"SELECT count(*), sum(amount), avg(amount), min(qty), max(qty), count(status) FROM o WHERE amount > 500000", true},
		{"SELECT cust, sum(amount) AS s FROM o GROUP BY cust ORDER BY s DESC LIMIT 5", true},
		{"SELECT cust, sum(amount) FROM o GROUP BY cust ORDER BY 2 LIMIT 3 OFFSET 2", true},
		{"SELECT cust, sum(amount) FROM o GROUP BY cust ORDER BY 2 DESC LIMIT 3", true}, // NULL sum first
		{"SELECT status, count(qty) AS n FROM o WHERE status <> 'paid' AND qty >= 3 GROUP BY status HAVING count(qty) > 10 ORDER BY n", true},
		{"SELECT cust, max(qty), o.label FROM o WHERE 100000 < amount AND label = 'l1' GROUP BY cust, o.label ORDER BY cust DESC LIMIT 7", true},
		{"SELECT sum(qty), avg(amount), min(amount) FROM o WHERE amount > 1e12", true},
		{"SELECT label, count(*), upper(label) FROM o GROUP BY label ORDER BY label", true},
		{"SELECT count(*) FROM o WHERE amount < $1", true},
		{"SELECT status, count(*) FROM o WHERE status IN ('s1', 's3', NULL, 'nope') GROUP BY status", true},
		{"SELECT count(*), sum(amount) FROM o WHERE qty BETWEEN 3 AND 7 AND amount IS NOT NULL", true},
		{"SELECT label, count(*), max(qty) FROM o WHERE status IS NULL GROUP BY label", true},
		{"SELECT count(*) FROM o WHERE qty = '5' AND cust IN (1, 2, 3, 9999)", true}, // text constant coerced as compare() does
		{"SELECT count(*), sum(amount) FROM o WHERE created >= '2026-10-15 00:00:00+00' AND created < '2026-10-20 00:00:00+00'", true},
		{"SELECT status, count(*) FROM o WHERE amount IN (1000.25, 250000, 7.5) GROUP BY status", true},
		{"SELECT count(*) FROM o WHERE cust IN (1, 2) OR qty = 1", true},
		{"SELECT status, count(*), sum(amount) FROM o WHERE (status = 's1' AND qty > 10) OR (status IS NULL AND amount < 100000) OR label = 'l2' GROUP BY status", true},
		{"SELECT count(*) FROM o WHERE qty NOT IN (1, 2) AND status NOT IN ('s0')", true},
		{"SELECT count(*) FROM o WHERE qty NOT IN (1, NULL)", false},
		{"SELECT status, count(DISTINCT cust), count(*), sum(amount), min(qty) FROM o GROUP BY status", true},
		{"SELECT count(DISTINCT status), count(DISTINCT status), avg(amount) FROM o WHERE qty > 3", true},
		{"SELECT label, count(DISTINCT label) FROM o GROUP BY label", true},
		{"SELECT cust, count(DISTINCT qty) AS d FROM o GROUP BY cust ORDER BY d DESC, cust LIMIT 5", true}, // all groups from the view, sorted by SQL
		{"SELECT cust, count(DISTINCT qty) AS d FROM o GROUP BY cust ORDER BY d DESC LIMIT 1", true},
		{"SELECT count(DISTINCT cust), count(DISTINCT qty) FROM o", false}, // two distinct columns
		{"SELECT count(DISTINCT amount) FROM o", false},                    // float column                         // never true: left to the row path
		{"SELECT count(*) FROM o WHERE NOT (qty = 1)", false},              // NOT
		{"SELECT count(*) FROM o WHERE qty = 1 OR amount + 1 > 5", false},  // one branch not expressible
		{"SELECT count(*) FROM o WHERE qty BETWEEN 3.5 AND 7", false},      // int column, float constant
		{"SELECT count(*), sum(amount) FROM o WHERE id = 17", false},       // point lookup
		{"SELECT count(DISTINCT cust) FROM o", true},
		{"SELECT date_trunc('day', created) AS d, count(*), sum(amount) FROM o GROUP BY date_trunc('day', created) ORDER BY d", true},
		{"SELECT date_trunc('week', created), status, count(DISTINCT cust), max(created) FROM o WHERE qty > 2 GROUP BY date_trunc('week', created), status", true},
		{"SELECT date_trunc('Month', created), count(*) FROM o GROUP BY date_trunc('Month', created)", true}, // units ignore case
		{"SELECT date_trunc('day', created), min(created), count(*) FROM o GROUP BY date_trunc('day', created) HAVING count(*) > 1", true},
		{"SELECT date_trunc('day', created) AS d, sum(amount) AS s FROM o GROUP BY date_trunc('day', created) ORDER BY s DESC LIMIT 1", true},
		{"SELECT date_trunc('decade', created), count(*) FROM o GROUP BY date_trunc('decade', created)", true},
		{"SELECT status, count(*) FROM o WHERE amount + 1 > 5 GROUP BY status", false},       // expression filter
		{"SELECT amount, count(*) FROM o GROUP BY amount ORDER BY 2 DESC, 1 LIMIT 3", false}, // float key
	} {
		before := ViewAggregates.Load()
		want := run(t, rows, c.q, FloatD(300000))
		got := run(t, view, c.q, FloatD(300000))
		if used := ViewAggregates.Load() > before; used != c.useView {
			t.Fatalf("%s: answered from the view %v, want %v", c.q, used, c.useView)
		}
		text := func(r *Result) string {
			var out []string
			for _, row := range r.Rows {
				out = append(out, fmt.Sprint(row))
			}
			if !strings.Contains(c.q, "ORDER BY") { // order unspecified
				sort.Strings(out)
			}
			return fmt.Sprint(r.Cols, out)
		}
		if g, w := text(got), text(want); g != w {
			t.Fatalf("%s:\n view %s\n rows %s", c.q, g, w)
		}
		t.Logf("%s: %d rows", c.q, len(got.Rows))
	}

	// a session accepting stale reads still gets the same answer here
	run(t, view, "SET stask.read_staleness = '2s'")
	got, want := run(t, view, "SELECT status, count(*) FROM o GROUP BY status ORDER BY 1"), run(t, rows, "SELECT status, count(*) FROM o GROUP BY status ORDER BY 1")
	if fmt.Sprint(got.Rows) != fmt.Sprint(want.Rows) {
		t.Fatalf("with read_staleness: %v, want %v", got.Rows, want.Rows)
	}
	run(t, view, "SET stask.read_staleness = 0")
	if _, err := tryRun(view, "SET stask.read_staleness = 'soon'"); err == nil {
		t.Fatal("invalid read_staleness accepted")
	}

	// inside a transaction the view could show later commits: never used
	run(t, view, "BEGIN")
	before := ViewAggregates.Load()
	run(t, view, "SELECT status, count(*) FROM o GROUP BY status")
	run(t, view, "COMMIT")
	if ViewAggregates.Load() != before {
		t.Fatal("explicit transaction answered from the view")
	}
}
