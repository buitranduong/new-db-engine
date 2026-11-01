package sql

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestR1PointCost measures what the harness' r1_point statement (docs/32: a primary-key lookup with two joins, the P
// workload of docs/34 H7) costs inside the engine: plan, parse versus execute time, allocations per statement. It
// asserts only that the plan is an index lookup (no table scan of sims), which the harness measurements need; the numbers
// are logged (go test -v -run TestR1PointCost) for docs/14 row OLTP1.
func TestR1PointCost(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, "CREATE TABLE carriers (id int PRIMARY KEY, code text NOT NULL, name text NOT NULL)")
	run(t, s, "CREATE TABLE stores (id int PRIMARY KEY, name text NOT NULL, region text NOT NULL, rate int NOT NULL)")
	run(t, s, "CREATE TABLE sims (number text PRIMARY KEY, carrier_id int NOT NULL, store_id int NOT NULL, category text NOT NULL, price int NOT NULL, store_rate int NOT NULL, status text NOT NULL, updated_at timestamptz NOT NULL)")
	run(t, s, "INSERT INTO carriers VALUES (1,'VT','Viettel'),(2,'VN','Vina'),(3,'MB','Mobi')")
	var b strings.Builder
	b.WriteString("INSERT INTO stores VALUES ")
	for i := 1; i <= 40; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(%d,'s%d','r%d',%d)", i, i, i%5, 5+i%6)
	}
	run(t, s, b.String())
	const n = 50000
	for lo := 0; lo < n; lo += 5000 {
		b.Reset()
		b.WriteString("INSERT INTO sims VALUES ")
		for i := lo; i < lo+5000; i++ {
			if i > lo {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "('09%09d',%d,%d,'c%d',%d,%d,'available',now())", i, 1+i%3, 1+i%40, i%5, 500000+i, 5+i%6)
		}
		run(t, s, b.String())
	}
	q := func(i int) string {
		return fmt.Sprintf(`SELECT s.number, s.price, s.status, c.code, st.name, st.region
FROM sims s JOIN carriers c ON c.id = s.carrier_id JOIN stores st ON st.id = s.store_id WHERE s.number = '09%09d'`, i)
	}
	plan := render(run(t, s, "EXPLAIN "+q(12345)))
	t.Logf("plan:\n%s", plan)
	if strings.Contains(plan, "Seq Scan on sims") {
		t.Fatalf("r1_point scans sims instead of using the primary key:\n%s", plan)
	}
	// parse versus whole statement
	const iters = 3000
	t0 := time.Now()
	for i := 0; i < iters; i++ {
		if _, err := Parse(q(i)); err != nil {
			t.Fatal(err)
		}
	}
	parse := time.Since(t0) / iters
	t0 = time.Now()
	for i := 0; i < iters; i++ {
		run(t, s, q(i))
	}
	whole := time.Since(t0) / iters
	allocs := testing.AllocsPerRun(500, func() { run(t, s, q(777)) })
	t.Logf("N=%d sims: parse %v, parse+plan+execute %v per statement, %.0f allocations per statement", n, parse, whole, allocs)
}
