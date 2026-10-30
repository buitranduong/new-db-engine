package pgwire

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// scalar runs q and returns the single text value.
func scalar(t *testing.T, conn *pgx.Conn, q string) string {
	t.Helper()
	var s *string
	if err := conn.QueryRow(context.Background(), q).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if s == nil {
		return "<NULL>"
	}
	return *s
}

// pgres: gaps pgdiff found in the catalog relations Superset and friends read
// (docs/14, task pgres, group g).
func TestPgresCatalog(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE sims (number text PRIMARY KEY, price numeric CHECK (price >= 0))`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO sims VALUES ('1', 1), ('2', 2), ('3', 3)`); err != nil {
		t.Fatal(err)
	}
	// the login user finds itself in pg_roles / pg_user
	for _, q := range []string{
		`SELECT rolname::text FROM pg_roles WHERE rolname = current_user`,
		`SELECT usename::text FROM pg_user WHERE usename = current_user`,
	} {
		if got := scalar(t, conn, q); got != "app" {
			t.Errorf("%s = %q, want app", q, got)
		}
	}
	// pg_stat_user_tables.n_live_tup, pg_stats exists
	if got := scalar(t, conn, `SELECT n_live_tup::text FROM pg_stat_user_tables WHERE relname = 'sims'`); got != "3" {
		t.Errorf("n_live_tup = %q", got)
	}
	if got := scalar(t, conn, `SELECT count(*)::text FROM pg_catalog.pg_stats WHERE tablename = 'sims'`); got != "0" {
		t.Errorf("pg_stats count = %q", got)
	}
	// pg_constraint.conkey is int2[] (1005), as PostgreSQL announces
	rows, err := conn.Query(ctx, `SELECT conkey FROM pg_constraint WHERE conrelid = 'sims'::regclass AND contype = 'p'`)
	if err != nil {
		t.Fatal(err)
	}
	if fd := rows.FieldDescriptions(); fd[0].DataTypeOID != 1005 {
		t.Errorf("conkey oid %d, want 1005", fd[0].DataTypeOID)
	}
	rows.Close()
}
