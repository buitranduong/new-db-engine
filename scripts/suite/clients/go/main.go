// Command main is the pgx v5 smoke program: connect, SELECT 1, a prepared statement, COPY.
// Uses PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE (or DATABASE_URL).
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL pgx:", err)
		os.Exit(1)
	}
	fmt.Println("OK pgx")
}

func run() error {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
			env("PGUSER", "postgres"), env("PGPASSWORD", ""), env("PGHOST", "127.0.0.1"),
			env("PGPORT", "5432"), env("PGDATABASE", "postgres"), env("PGSSLMODE", "disable"))
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		return fmt.Errorf("select 1: %v %d", err, one)
	}
	if _, err := conn.Exec(ctx, "DROP TABLE IF EXISTS suite_smoke"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE suite_smoke (id int PRIMARY KEY, v text)"); err != nil {
		return err
	}
	defer conn.Exec(ctx, "DROP TABLE IF EXISTS suite_smoke")

	// prepared statement (named, extended protocol)
	if _, err := conn.Prepare(ctx, "smoke_ins", "INSERT INTO suite_smoke VALUES ($1, $2)"); err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	if _, err := conn.Exec(ctx, "smoke_ins", 1, "prepared"); err != nil {
		return fmt.Errorf("exec prepared: %w", err)
	}

	// COPY FROM STDIN (text) via the wire-level CopyFrom, then COPY TO STDOUT
	in := strings.NewReader("2\tcopied\n3\tcopied too\n")
	tag, err := conn.PgConn().CopyFrom(ctx, in, "COPY suite_smoke FROM STDIN")
	if err != nil || tag.RowsAffected() != 2 {
		return fmt.Errorf("copy from: %v %v", err, tag)
	}
	var out bytes.Buffer
	if _, err := conn.PgConn().CopyTo(ctx, &out, "COPY suite_smoke TO STDOUT"); err != nil {
		return fmt.Errorf("copy to: %w", err)
	}
	if got := strings.Count(out.String(), "\n"); got != 3 {
		return fmt.Errorf("copy to returned %d lines, want 3: %q", got, out.String())
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM suite_smoke WHERE v LIKE $1", "copied%").Scan(&n); err != nil || n != 2 {
		return fmt.Errorf("param query: %v %d", err, n)
	}
	return nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
