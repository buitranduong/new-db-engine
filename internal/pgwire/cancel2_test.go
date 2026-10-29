package pgwire

import (
	"context"
	"testing"
	"time"

	"stask/internal/core"
)

// Every operator that can run long must honour statement_timeout (57014):
// function scans, sorts, hash aggregation, DISTINCT, windows, set operations,
// recursive CTEs and joins over function scans.
func TestStatementTimeoutOperators(t *testing.T) {
	url, _, _ := startServerWith(t, core.Options{}, nil)
	c := connect(t, url)
	ctx := context.Background()
	if _, err := c.Exec(ctx, "SET statement_timeout = '150ms'"); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"function scan":  "SELECT count(*) FROM generate_series(1, 100000000000)",
		"scan in select": "SELECT generate_series(1, 100000000000)",
		"sort":           "SELECT g FROM generate_series(1, 20000000) g ORDER BY md5(g::text)",
		"group by":       "SELECT g % 1000003, count(*) FROM generate_series(1, 90000000) g GROUP BY 1",
		"distinct":       "SELECT DISTINCT g FROM generate_series(1, 90000000) g",
		"window":         "SELECT sum(g) OVER (ORDER BY g) FROM generate_series(1, 90000000) g",
		"union":          "SELECT g FROM generate_series(1, 50000000) g UNION SELECT g FROM generate_series(1, 50000000) g",
		"cross join":     "SELECT count(*) FROM generate_series(1, 20000) a, generate_series(1, 20000) b WHERE a + b < 0",
		"recursive":      "WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r) SELECT count(*) FROM r",
		"numbers":        "SELECT count(*) FROM numbers(100000000000)",
	} {
		start := time.Now()
		_, err := c.Exec(ctx, q)
		if sqlState(err) != "57014" {
			t.Errorf("%s: got %v after %v", name, err, time.Since(start))
			continue
		}
		if d := time.Since(start); d > 150*time.Millisecond+slack*400*time.Millisecond {
			t.Errorf("%s: stopped after %v", name, d)
		}
	}
	var n int64
	if err := c.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("session unusable after timeouts: %v", err)
	}
}

// A CancelRequest stops a function scan too.
func TestCancelFunctionScan(t *testing.T) {
	url, _, _ := startServerWith(t, core.Options{}, nil)
	c := connect(t, url)
	done := make(chan error, 1)
	go func() {
		_, err := c.Exec(context.Background(), "SELECT count(*) FROM generate_series(1, 100000000000)")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := c.PgConn().CancelRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if sqlState(err) != "57014" {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("function scan not cancelled")
	}
}
