package pgwire

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// interval and time of day, scalar and array, as binary parameters (appendix P of docs/14).
func TestBinaryIntervalTimeParams(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var s string
	iv := pgtype.Interval{Microseconds: 3_600_000_000, Days: 2, Months: 1, Valid: true}
	if err := conn.QueryRow(ctx, `SELECT $1::interval::text`, iv).Scan(&s); err != nil {
		t.Fatalf("interval param: %v", err)
	}
	if s != "1 mon 2 days 01:00:00" {
		t.Errorf("interval = %q", s)
	}
	var n int
	var txt string
	if err := conn.QueryRow(ctx, `SELECT $1::interval[]::text`, []pgtype.Interval{iv, iv}).Scan(&txt); err != nil || txt != `{"1 mon 2 days 01:00:00","1 mon 2 days 01:00:00"}` {
		t.Errorf("interval[] round trip: %q %v", txt, err)
	}
	var first string
	if err := conn.QueryRow(ctx, `SELECT (SELECT x::text FROM unnest($1::interval[]) x ORDER BY x LIMIT 1)`, []pgtype.Interval{{Microseconds: 60_000_000, Valid: true}, {Microseconds: 30_000_000, Valid: true}}).Scan(&first); err != nil || first != "00:00:30" {
		t.Errorf("interval[] element: %q %v", first, err)
	}
	tm := pgtype.Time{Microseconds: int64(10*time.Hour/time.Microsecond) + 30*60*1_000_000, Valid: true}
	if err := conn.QueryRow(ctx, `SELECT cardinality($1::time[])`, []pgtype.Time{tm, tm}).Scan(&n); err != nil || n != 2 {
		t.Errorf("time[] length: %d %v", n, err)
	}
}
