package pgwire

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The TimeZone startup parameter sets the session zone (pgjdbc sends the JVM zone), SET / RESET TIME ZONE change it,
// and the server announces every change with ParameterStatus because TimeZone is GUC_REPORT (PostgreSQL protocol
// docs, "ParameterStatus"; guc_tables.c GUC_REPORT).
func TestTimeZoneStartupAndParameterStatus(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["TimeZone"] = "Asia/Ho_Chi_Minh"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	pc := conn.PgConn()
	if got := pc.ParameterStatus("TimeZone"); got != "Asia/Ho_Chi_Minh" {
		t.Fatalf("startup ParameterStatus TimeZone = %q", got)
	}
	var s string
	if err := conn.QueryRow(ctx, "SELECT (timestamptz '2024-03-10 07:00:00+00')::text").Scan(&s); err != nil || s != "2024-03-10 14:00:00+07" {
		t.Fatalf("got %q, %v", s, err)
	}
	if _, err := conn.Exec(ctx, "SET TIME ZONE 'Asia/Tokyo'"); err != nil {
		t.Fatal(err)
	}
	if got := pc.ParameterStatus("TimeZone"); got != "Asia/Tokyo" {
		t.Fatalf("after SET: ParameterStatus TimeZone = %q", got)
	}
	if _, err := conn.Exec(ctx, "RESET TIME ZONE"); err != nil {
		t.Fatal(err)
	}
	if got := pc.ParameterStatus("TimeZone"); got != "UTC" {
		t.Fatalf("after RESET: ParameterStatus TimeZone = %q", got)
	}
	cfg.RuntimeParams["TimeZone"] = "Nowhere/Land"
	if c2, err := pgx.ConnectConfig(ctx, cfg); err == nil {
		c2.Close(ctx)
		t.Fatal("unknown startup TimeZone accepted")
	}
}
