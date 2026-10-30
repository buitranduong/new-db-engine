package sql

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMergePerf times MERGE of a 200k-row staging table into a 1M-row table
// against INSERT ... ON CONFLICT DO UPDATE with the same data. It only runs with
// CTAS_PERF=1 (it needs about 1 GB; set TMPDIR to a RAM disk).
//
//	CTAS_PERF=1 TMPDIR=/dev/shm/x go test -run TestMergePerf -v -count=1 ./internal/sql
func TestMergePerf(t *testing.T) {
	if os.Getenv("CTAS_PERF") == "" {
		t.Skip("set CTAS_PERF=1")
	}
	const target, staging = 1_000_000, 200_000
	load := func(s *Session) {
		run(t, s, `CREATE TABLE tgt (number TEXT PRIMARY KEY, status TEXT, price NUMERIC(12,2))`)
		run(t, s, `CREATE TABLE stg (number TEXT, status TEXT, price NUMERIC(12,2))`)
		for lo := 1; lo <= target; lo += 100_000 {
			run(t, s, fmt.Sprintf(`INSERT INTO tgt SELECT lpad(g::text, 10, '0'), 'old', 1.5 FROM generate_series(%d, %d) g`, lo, lo+99_999))
		}
		// half of the staging rows exist in the target, half are new
		run(t, s, fmt.Sprintf(`INSERT INTO stg SELECT lpad((g + %d)::text, 10, '0'), 'new', 2.5 FROM generate_series(1, %d) g`, target-staging/2, staging))
	}
	time1 := func(label string, s *Session, q string) time.Duration {
		t0 := time.Now()
		r := run(t, s, q)
		d := time.Since(t0)
		t.Logf("%-28s %-14s %v  (%.0f rows/s)", label, r.Tag, d.Round(time.Millisecond), float64(staging)/d.Seconds())
		return d
	}
	e1 := newEngine(t)
	s1 := NewSession(e1)
	load(s1)
	time1("join only (count)", s1, `SELECT count(*) FROM stg LEFT JOIN tgt ON tgt.number = stg.number`)
	time1("join only (all cols)", s1, `SELECT tgt.number, tgt.status, tgt.price, tgt.number IS NOT NULL, stg.status, stg.price FROM stg LEFT JOIN tgt ON tgt.number = stg.number`)
	dm := time1("MERGE", s1, `MERGE INTO tgt USING stg ON tgt.number = stg.number
		WHEN MATCHED THEN UPDATE SET status = stg.status, price = stg.price
		WHEN NOT MATCHED THEN INSERT (number, status, price) VALUES (stg.number, stg.status, stg.price)`)
	r := run(t, s1, `SELECT count(*), count(*) FILTER (WHERE status = 'new') FROM tgt`)
	if r.Rows[0][0].I != target+staging/2 || r.Rows[0][1].I != staging {
		t.Fatalf("after MERGE: %s", render(r))
	}
	e2 := newEngine(t)
	s2 := NewSession(e2)
	load(s2)
	di := time1("INSERT ... ON CONFLICT", s2, `INSERT INTO tgt SELECT number, status, price FROM stg
		ON CONFLICT (number) DO UPDATE SET status = EXCLUDED.status, price = EXCLUDED.price`)
	r = run(t, s2, `SELECT count(*), count(*) FILTER (WHERE status = 'new') FROM tgt`)
	if r.Rows[0][0].I != target+staging/2 || r.Rows[0][1].I != staging {
		t.Fatalf("after ON CONFLICT: %s", render(r))
	}
	t.Logf("MERGE / ON CONFLICT = %.2f", dm.Seconds()/di.Seconds())
}
