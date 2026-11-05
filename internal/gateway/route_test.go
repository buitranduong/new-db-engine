package gateway

import "testing"

func TestPriorityOf(t *testing.T) {
	for sql, want := range map[string]int{
		"SELECT * FROM sims ORDER BY price LIMIT 100 OFFSET 200": prioPage,
		"select * from sims order by price limit 100;":           prioPage,
		"SELECT count(*) FROM sims WHERE status = 'x'":           prioReport,
		"SELECT * FROM (SELECT * FROM sims LIMIT 10) s":          prioReport,
		"SELECT region, count(*) FROM orders GROUP BY region":    prioReport,
	} {
		if got := priorityOf(sql); got != want {
			t.Errorf("%s: priority %d, want %d", sql, got, want)
		}
	}
}
