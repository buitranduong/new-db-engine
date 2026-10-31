package pgwire

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RAISE NOTICE / WARNING / INFO inside functions reach the client as NoticeResponse with their SQLSTATE,
// DETAIL and HINT, also when the statement then fails; an error carries DETAIL and HINT (docs/14 appendix Z).
func TestPlpgsqlNoticesAndErrorFieldsOnTheWire(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	var notices []pgconn.Notice
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, *n) }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE FUNCTION wn() RETURNS int AS $f$ BEGIN RAISE NOTICE 'n % ', 1; RAISE WARNING 'w1' USING DETAIL = 'dd', HINT = 'hh'; RAISE INFO 'i1'; RAISE DEBUG 'dbg'; RETURN 1; END $f$ LANGUAGE plpgsql`,
		`CREATE FUNCTION wf() RETURNS int AS $f$ BEGIN RAISE NOTICE 'before the error'; RAISE EXCEPTION 'bad' USING ERRCODE = '22012', DETAIL = 'dtl', HINT = 'hnt'; END $f$ LANGUAGE plpgsql`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT wn()`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("SELECT wn(): %d %v", n, err)
	}
	if len(notices) != 3 {
		t.Fatalf("got %d notices, want 3 (NOTICE, WARNING, INFO): %+v", len(notices), notices)
	}
	if notices[0].Severity != "NOTICE" || notices[0].Message != "n 1 " || notices[0].Code != "00000" {
		t.Errorf("notice 0: %+v", notices[0])
	}
	if w := notices[1]; w.Severity != "WARNING" || w.Code != "01000" || w.Detail != "dd" || w.Hint != "hh" {
		t.Errorf("warning: %+v", w)
	}
	if notices[2].Severity != "INFO" {
		t.Errorf("info: %+v", notices[2])
	}
	notices = nil
	err = conn.QueryRow(ctx, `SELECT wf()`).Scan(&n)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "22012" || pe.Detail != "dtl" || pe.Hint != "hnt" || pe.Message != "bad" {
		t.Fatalf("error: %#v", err)
	}
	if len(notices) != 1 || notices[0].Message != "before the error" {
		t.Errorf("notice before the error: %+v", notices)
	}
	// the simple protocol too
	notices = nil
	if _, err := conn.Exec(ctx, `DO $f$ BEGIN RAISE NOTICE 'from do'; END $f$`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Message != "from do" {
		t.Errorf("DO notice: %+v", notices)
	}
}
