package sql

import "testing"

func TestQueryMemBudget(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE m (id bigint PRIMARY KEY, v text)`)
	run(t, s, `INSERT INTO m SELECT g, repeat('x', 100) FROM generate_series(1, 2000) g`)
	defer func(q, n int64) { QueryMemLimit, SQLMemLimit = q, n }(QueryMemLimit, SQLMemLimit)

	QueryMemLimit, SQLMemLimit = 4<<20, 0
	// a 2000 x 2000 join materializes far more than 4 MB
	if _, err := tryRun(s, `SELECT count(*) FROM m a JOIN m b ON a.v = b.v`); code(err) != "53200" {
		t.Fatalf("join past the query budget: %v", err)
	}
	if r := run(t, s, `SELECT count(*) FROM m a JOIN m b ON a.id = b.id`); r.Rows[0][0].I != 2000 {
		t.Fatalf("small join: %s", render(r))
	}
	QueryMemLimit, SQLMemLimit = 0, 2<<20
	if _, err := tryRun(s, `SELECT count(*) FROM m a JOIN m b ON a.v = b.v`); code(err) != "53200" {
		t.Fatalf("join past the node budget: %v", err)
	}
	if n := SQLMemInUse(); n != 0 {
		t.Fatalf("%d bytes still reserved after the queries", n)
	}
}
