package sql

import (
	"strings"
	"testing"

	"stask/internal/planner"
)

func explainText(t *testing.T, s *Session, q string) string {
	t.Helper()
	res := run(t, s, q)
	var ls []string
	for _, r := range res.Rows {
		ls = append(ls, r[0].String())
	}
	return strings.Join(ls, "\n")
}

func TestExplainAllStatements(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	s := NewSession(e)
	s.Views = p
	run(t, s, `CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT, status TEXT, price BIGINT)`)
	run(t, s, `CREATE INDEX sims_cs ON sims (carrier, status)`)
	run(t, s, `CREATE INDEX sims_trgm ON sims USING gin (number gin_trgm_ops)`)
	run(t, s, `CREATE TABLE staging (number TEXT PRIMARY KEY, carrier TEXT)`)
	run(t, s, `INSERT INTO sims SELECT 'n' || i, 'c' || (i % 3), 'available', i FROM generate_series(1, 300) i`)
	run(t, s, `INSERT INTO staging SELECT 'n' || i, 'x' FROM generate_series(1, 50) i`)

	cases := []struct{ q, want string }{
		{`EXPLAIN SELECT * FROM sims WHERE number = 'n5'`, "Index Scan using sims_pkey on sims"},
		{`EXPLAIN SELECT * FROM sims WHERE carrier = 'c1' AND status = 'available'`, "Index Scan using sims_cs on sims"},
		{`EXPLAIN SELECT * FROM sims WHERE price > 5`, "Seq Scan on sims"},
		{`EXPLAIN SELECT * FROM sims WHERE number LIKE '%88%'`, "Trigram Scan on sims using sims_trgm"},
		{`EXPLAIN SELECT carrier, count(*) FROM sims GROUP BY carrier`, "HashAggregate"},
		{`EXPLAIN SELECT * FROM sims ORDER BY price LIMIT 3`, "Limit"},
		{`EXPLAIN INSERT INTO sims VALUES ('z', 'c', 's', 1)`, "Insert on sims"},
		{`EXPLAIN INSERT INTO sims SELECT number, carrier, 's', 1 FROM staging WHERE number = 'n1'`, "Index Scan using staging_pkey on staging"},
		{`EXPLAIN UPDATE sims SET price = 1 WHERE number = 'n5'`, "Index Scan using sims_pkey on sims"},
		{`EXPLAIN UPDATE sims SET price = 1 WHERE carrier = 'c1'`, "Index Scan using sims_cs on sims"},
		{`EXPLAIN UPDATE sims SET price = t.price FROM (SELECT number, 5 AS price FROM staging) t WHERE sims.number = t.number`, "Hash Inner Join"},
		{`EXPLAIN UPDATE sims SET status = 'x' WHERE NOT EXISTS (SELECT 1 FROM staging t WHERE t.number = sims.number)`, "Hash Anti Join"},
		{`EXPLAIN DELETE FROM sims WHERE number = 'n7'`, "Delete on sims"},
		{`EXPLAIN DELETE FROM sims WHERE price < 0`, "Seq Scan on sims"},
		{`EXPLAIN WITH x AS (SELECT number FROM staging) SELECT * FROM sims WHERE number IN (SELECT number FROM x)`, "CTE Scan on x"},
		{`EXPLAIN WITH d AS (DELETE FROM staging WHERE number = 'n3' RETURNING number) SELECT * FROM d`, "Delete on staging"},
		{`EXPLAIN SELECT s.number FROM sims s JOIN staging t ON t.number = s.number`, "Hash Inner Join"},
		{`EXPLAIN SELECT number FROM sims UNION SELECT number FROM staging`, "SetOp UNION"},
		{`EXPLAIN (ANALYZE false) SELECT 1`, "Result"},
	}
	for _, c := range cases {
		out := explainText(t, s, c.q)
		t.Logf("%s\n%s", c.q, out)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: want %q in\n%s", c.q, c.want, out)
		}
	}
	// ordered index scan removes the sort
	if out := explainText(t, s, `EXPLAIN SELECT * FROM sims WHERE carrier = 'c1' ORDER BY status LIMIT 5`); strings.Contains(out, "Sort") || !strings.Contains(out, "sims_cs") {
		t.Errorf("ordered index scan: %s", out)
	}
	// EXPLAIN does not run the statement
	run(t, s, `EXPLAIN DELETE FROM staging`)
	if r := run(t, s, `SELECT count(*) FROM staging`); r.Rows[0][0].I != 50 {
		t.Fatalf("EXPLAIN DELETE deleted rows: %v", r.Rows)
	}
	// ANALYZE runs it and reports rows and time
	out := explainText(t, s, `EXPLAIN ANALYZE UPDATE sims SET price = price + 1 WHERE carrier = 'c1'`)
	t.Log(out)
	if !strings.Contains(out, "rows=100") || !strings.Contains(out, "Execution Time:") || !strings.Contains(out, "Spilled to disk: false") {
		t.Errorf("analyze output: %s", out)
	}
	if r := run(t, s, `SELECT count(*) FROM sims WHERE price > 1000`); r.Rows[0][0].I != 0 {
		t.Fatal("unexpected")
	}
	out = explainText(t, s, `EXPLAIN ANALYZE DELETE FROM staging WHERE number = 'n1'`)
	if !strings.Contains(out, "rows=1") {
		t.Errorf("analyze delete: %s", out)
	}
	if r := run(t, s, `SELECT count(*) FROM staging`); r.Rows[0][0].I != 49 {
		t.Fatalf("EXPLAIN ANALYZE DELETE did not delete: %v", r.Rows)
	}
	out = explainText(t, s, `EXPLAIN ANALYZE SELECT * FROM sims WHERE number LIKE '%88%'`)
	if !strings.Contains(out, "Trigram Scan") || !strings.Contains(out, "applied_lsn=") || !strings.Contains(out, "view_lag_ms=") {
		t.Errorf("trigram analyze: %s", out)
	}
	// inside a transaction, ANALYZE of a write can be rolled back
	run(t, s, `BEGIN`)
	run(t, s, `EXPLAIN ANALYZE DELETE FROM staging`)
	run(t, s, `ROLLBACK`)
	if r := run(t, s, `SELECT count(*) FROM staging`); r.Rows[0][0].I != 49 {
		t.Fatalf("rolled back analyze: %v", r.Rows)
	}
	// parameters
	sts, err := Parse(`EXPLAIN SELECT * FROM sims WHERE number = $1`)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Describe(sts[0])
	if err != nil || len(d.Params) != 1 || d.Params[0] != KText {
		t.Fatalf("describe: %+v %v", d, err)
	}
	if out, err := s.exec(sts[0], []Datum{TextD("n5")}); err != nil || !strings.Contains(out.Rows[0][0].String(), "sims_pkey") {
		t.Fatalf("explain with a parameter: %v %v", out, err)
	}
}
