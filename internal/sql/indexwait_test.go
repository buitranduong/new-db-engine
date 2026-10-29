package sql

import (
	"strings"
	"testing"
	"time"

	"stask/internal/planner"
)

// CREATE INDEX .. gin_trgm_ops returns an index the very next query uses;
// CONCURRENTLY returns at once and EXPLAIN reports the index is building.
func TestCreateTrigramIndexWaits(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	s := NewSession(e)
	s.Views = p
	run(t, s, `CREATE TABLE sim (id BIGINT PRIMARY KEY, num TEXT)`)
	run(t, s, `INSERT INTO sim SELECT i, '09' || lpad(i::text, 8, '0') FROM generate_series(1, 20000) i`)
	t0 := time.Now()
	run(t, s, `CREATE INDEX sim_num ON sim USING gin (num gin_trgm_ops)`)
	t.Logf("create index took %v", time.Since(t0))
	out := explainText(t, s, `EXPLAIN SELECT id FROM sim WHERE num LIKE '%888%'`)
	if !strings.Contains(out, "Trigram Scan on sim using sim_num") {
		t.Fatalf("index not usable right after CREATE INDEX:\n%s", out)
	}
	run(t, s, `DROP INDEX sim_num`)

	release := p.Sub.Hold() // the view cannot build while held
	run(t, s, `CREATE INDEX CONCURRENTLY sim_num2 ON sim USING gin (num gin_trgm_ops)`)
	out = explainText(t, s, `EXPLAIN SELECT id FROM sim WHERE num LIKE '%888%'`)
	if !strings.Contains(out, "Seq Scan on sim") || !strings.Contains(out, "still being built") {
		t.Fatalf("EXPLAIN does not report the building index:\n%s", out)
	}
	// the query still answers (from the table)
	if r := run(t, s, `SELECT count(*) FROM sim WHERE num LIKE '%888%'`); r.Rows[0][0].I == 0 {
		t.Fatal("no rows from the scan")
	}
	release()
	deadline := time.Now().Add(20 * time.Second)
	for {
		out = explainText(t, s, `EXPLAIN SELECT id FROM sim WHERE num LIKE '%888%'`)
		if strings.Contains(out, "Trigram Scan") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("index never became usable:\n%s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
