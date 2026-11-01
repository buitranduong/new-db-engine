package sql

import "testing"

// A point read through the general executor (a join by primary key, as the R1 read of the harness) must
// not fail with 53200 because other queries hold the node's shared budget; a query that needs more than its
// floor still does (loudly, never a partial result).
func TestPointReadSurvivesFullNodeBudget(t *testing.T) {
	s := joinPushSession(t)
	run(t, s, `CREATE TABLE bigp (id BIGINT PRIMARY KEY, cid INT, v TEXT)`)
	run(t, s, `INSERT INTO bigp SELECT g, g % 5 + 1, repeat('x', 200) FROM generate_series(1, 60000) g`)
	defer func(q, n int64, d string) { QueryMemLimit, SQLMemLimit, SpillDir = q, n, d }(QueryMemLimit, SQLMemLimit, SpillDir)
	QueryMemLimit, SQLMemLimit, SpillDir = 0, 64<<20, ""
	base := sqlMemUsed.Load()
	sqlMemUsed.Add(SQLMemLimit) // other queries hold the whole node budget
	defer sqlMemUsed.Add(-SQLMemLimit)

	point := `SELECT b.id, b.v, c.name FROM bigp b JOIN carriers c ON c.id = b.cid WHERE b.id = 4242`
	for i := 0; i < 20; i++ {
		res, err := tryRun(s, point)
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("point read with the node budget used up: %v", err)
		}
	}
	big := `SELECT b.id, b.v, c.name FROM bigp b JOIN carriers c ON c.id = b.cid WHERE b.id > 10`
	if res, err := tryRun(s, big); code(err) != "53200" {
		t.Fatalf("a 60000-row result with the node budget used up: want 53200, got %v (%v)", err, res != nil)
	}
	if got := sqlMemUsed.Load(); got != base+SQLMemLimit {
		t.Errorf("the shared counter leaks: %d, want %d", got, base+SQLMemLimit)
	}
}
