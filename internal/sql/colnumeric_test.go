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

// numeric columns of a columnar table: the view must answer exactly as the
// rows do (value, scale, NULLs, NaN-free groups), and use the view exactly
// when the column is a numeric(p<=18,s).
func TestViewNumericMatchesRowPath(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	rows, view := NewSession(e), NewSession(e)
	view.Views = p
	run(t, rows, `CREATE TABLE m (id BIGINT PRIMARY KEY, g TEXT, p numeric(12,2), q numeric(18,4), r numeric(5,0),
		big numeric(18,0), u numeric, w numeric(30,5), c numeric(6,6)) WITH (columnar = true)`)
	rng := rand.New(rand.NewSource(11))
	null := func(s string) string {
		if rng.Intn(20) == 0 {
			return "NULL"
		}
		return s
	}
	dec := func(digits, scale int) string { // a random decimal with that many integer+fraction digits
		s := make([]byte, digits)
		for i := range s {
			s[i] = byte('0' + rng.Intn(10))
		}
		if rng.Intn(3) == 0 { // magnitudes vary
			for i := 0; i < digits/2; i++ {
				s[i] = '0'
			}
		}
		x := string(s)
		if scale > 0 {
			x = x[:digits-scale] + "." + x[digits-scale:]
		}
		if rng.Intn(3) == 0 {
			x = "-" + x
		}
		return x
	}
	const n = 60000
	for b := 0; b < n/500; b++ {
		var vals []string
		for i := 0; i < 500; i++ {
			id := b*500 + i
			g := fmt.Sprintf("'g%d'", rng.Intn(6))
			pv := dec(12, 2)
			if rng.Intn(4) == 0 {
				pv = fmt.Sprint(rng.Intn(40)) + "." + fmt.Sprintf("%02d", rng.Intn(100)) // few distinct, to group by
			}
			vals = append(vals, fmt.Sprintf("(%d, %s, %s, %s, %s, %s, %s, %s, %s)", id, g, null(pv), null(dec(18, 4)),
				null(dec(5, 0)), null(dec(18, 0)), null(dec(20, 3)), null(dec(30, 5)), null(dec(6, 6))))
		}
		run(t, rows, "INSERT INTO m VALUES "+strings.Join(vals, ","))
	}
	run(t, rows, "UPDATE m SET p = p + 1 WHERE id % 7 = 0")
	run(t, rows, "DELETE FROM m WHERE id % 11 = 0")
	for !p.ColF.Ready() {
		time.Sleep(time.Millisecond)
	}

	for _, c := range []struct {
		q       string
		useView bool
	}{
		{"SELECT g, count(*), count(p), sum(p), avg(p), min(p), max(p) FROM m GROUP BY g ORDER BY g", true},
		{"SELECT g, sum(q), avg(q), min(q), max(q) FROM m GROUP BY g ORDER BY g", true},
		{"SELECT g, sum(r), avg(r), min(r), max(r) FROM m GROUP BY g", true},
		{"SELECT g, sum(big), avg(big), min(big), max(big) FROM m GROUP BY g", true}, // 128-bit sums
		{"SELECT sum(big), avg(big) FROM m", true},
		{"SELECT sum(c), avg(c), min(c), max(c), count(c) FROM m", true},
		{"SELECT sum(p), sum(q), sum(r) FROM m WHERE p > 10.5", true},
		{"SELECT count(*), sum(p) FROM m WHERE p = 10.555", true}, // more digits than the scale: no value equals it
		{"SELECT count(*), sum(p) FROM m WHERE p < 10.555", true},
		{"SELECT count(*), sum(p) FROM m WHERE p <= 10.555 AND p >= -3.001", true},
		{"SELECT count(*), sum(p) FROM m WHERE p > 10.555", true},
		{"SELECT count(*), sum(p) FROM m WHERE p >= 10.555", true},
		{"SELECT count(*), sum(p) FROM m WHERE p <> 10.555", true},
		{"SELECT count(*), sum(p) FROM m WHERE p > 99999999999999999999", true},
		{"SELECT count(*), sum(p) FROM m WHERE p < -99999999999999999999", true},
		{"SELECT count(*), sum(p) FROM m WHERE p <> -99999999999999999999", true},
		{"SELECT count(*), sum(p) FROM m WHERE p = 7", true},
		{"SELECT count(*), sum(p) FROM m WHERE 7.5 < p", true},
		{"SELECT count(*), sum(p) FROM m WHERE p BETWEEN 5 AND 20.25", true},
		{"SELECT count(*), sum(p) FROM m WHERE p IN (1.50, 2, 3.456, 12.07, NULL)", true},
		{"SELECT count(*), sum(p) FROM m WHERE p NOT IN (1.50, 2, 3.456, 12.07)", true},
		{"SELECT count(*), sum(p) FROM m WHERE p IS NULL", true},
		{"SELECT count(*), sum(p), avg(p) FROM m WHERE p IS NOT NULL AND g = 'g1'", true},
		{"SELECT count(*), sum(p) FROM m WHERE p < $1", true},
		{"SELECT p, count(*), sum(q) FROM m WHERE p BETWEEN 0 AND 40 GROUP BY p ORDER BY p", true}, // group by numeric
		{"SELECT p, count(*) FROM m GROUP BY p ORDER BY 2 DESC, 1 LIMIT 10", true},
		{"SELECT g, p, count(*), min(q) FROM m WHERE p BETWEEN 0 AND 3 GROUP BY g, p ORDER BY g, p", true},
		{"SELECT r, count(*), sum(p) FROM m GROUP BY r ORDER BY r", true},
		{"SELECT g, sum(p) AS s FROM m GROUP BY g ORDER BY s DESC LIMIT 2", true},
		{"SELECT g, avg(p) AS s FROM m GROUP BY g ORDER BY s LIMIT 3", true},
		{"SELECT g, sum(big) AS s FROM m GROUP BY g ORDER BY s DESC LIMIT 3", true},
		{"SELECT g, min(p) AS s FROM m GROUP BY g ORDER BY s, g LIMIT 3", true},
		{"SELECT g, max(q) AS s FROM m GROUP BY g HAVING max(q) > 0 ORDER BY s", true},
		{"SELECT g, sum(p) FROM m GROUP BY g HAVING sum(p) > 1000000", true},
		{"SELECT g, count(DISTINCT p), count(DISTINCT q) FROM m GROUP BY g", false}, // two distinct columns
		{"SELECT g, count(DISTINCT p), sum(q) FROM m GROUP BY g", true},
		{"SELECT p, count(DISTINCT g), sum(q) FROM m GROUP BY p", true},
		{"SELECT sum(p) FROM m WHERE g = 'none'", true}, // NULL
		{"SELECT g, sum(p), sum(p) * 2, sum(p) + sum(q) FROM m GROUP BY g", true},
		// not held by the view
		{"SELECT g, sum(u), avg(u), min(u), max(u) FROM m GROUP BY g", false},
		{"SELECT g, sum(w) FROM m GROUP BY g", false},
		{"SELECT g, count(u), count(w) FROM m GROUP BY g", false},
		{"SELECT g, sum(p), sum(u) FROM m GROUP BY g", false},
		{"SELECT u, count(*) FROM m GROUP BY u ORDER BY 2 DESC, 1 LIMIT 3", false},
		{"SELECT count(*) FROM m WHERE w > 5", false},
		{"SELECT count(*) FROM m WHERE p > 5.5::float8", false},
	} {
		before := ViewAggregates.Load()
		want := run(t, rows, c.q, IntD(1500))
		got := run(t, view, c.q, IntD(1500))
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
		if len(want.Rows) == 0 {
			t.Fatalf("%s: no rows", c.q)
		}
	}
	// the sample covers what it should: negative, large and wide values
	if r := run(t, rows, "SELECT count(*) FROM m WHERE p < 0 AND big > 100000000000000000"); r.Rows[0][0].I == 0 {
		t.Fatal("no negative / large values in the sample")
	}

	// a NaN takes only its column out of the view
	run(t, rows, "INSERT INTO m (id, g, p, q) VALUES (-1, 'g0', 'NaN', 1.5)")
	for i := 0; i < 2000; i++ { // the view catches up
		time.Sleep(5 * time.Millisecond)
		before := ViewAggregates.Load()
		run(t, view, "SELECT count(*), sum(q) FROM m")
		if ViewAggregates.Load() > before {
			break
		}
	}
	for _, c := range []struct {
		q       string
		useView bool
	}{
		{"SELECT g, sum(p), max(p) FROM m GROUP BY g ORDER BY g", false},
		{"SELECT g, sum(q), max(q) FROM m GROUP BY g ORDER BY g", true},
		{"SELECT p, count(*) FROM m GROUP BY p ORDER BY 2 DESC, 1 LIMIT 3", false},
	} {
		before := ViewAggregates.Load()
		want := run(t, rows, c.q)
		got := run(t, view, c.q)
		if used := ViewAggregates.Load() > before; used != c.useView {
			t.Fatalf("%s: answered from the view %v, want %v", c.q, used, c.useView)
		}
		if g, w := fmt.Sprint(got.Rows), fmt.Sprint(want.Rows); g != w {
			t.Fatalf("%s:\n view %s\n rows %s", c.q, g, w)
		}
	}
}
