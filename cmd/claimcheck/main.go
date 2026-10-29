// claimcheck runs one SQL probe per claim the docs make (docs/14) against a
// running staskd and prints a markdown table: claim, probe result, verdict.
// It is the "actual" column of the plan-target-actual report: rerun it after
// every merge and paste the output into docs/14.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// check: Steps run in order; the last one is compared with Want.
// Want: "ok" (no error), "err:SQLSTATE", "val:text" (exact), "has:text" (substring).
type check struct {
	ID, Task, Doc string
	Steps         []string
	Want          string
	Args          []any
}

func main() {
	url := flag.String("url", "postgres://app@127.0.0.1:5432/stask?sslmode=disable", "staskd pg url")
	only := flag.String("only", "", "run only checks whose id contains this")
	flag.Parse()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *url+"&default_query_exec_mode=simple_protocol")
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(2)
	}
	defer conn.Close(ctx)
	var xconn *pgx.Conn // extended protocol (Parse/Bind/Execute), for the ext_ probes
	defer func() {
		if xconn != nil {
			xconn.Close(ctx)
		}
	}()

	cs := checks()
	pass, total := 0, 0
	fmt.Println("| id | task | doc | want | actual | verdict |")
	fmt.Println("|---|---|---|---|---|---|")
	for _, c := range cs {
		if *only != "" && !strings.Contains(c.ID, *only) {
			continue
		}
		total++
		if conn.IsClosed() {
			if conn, err = pgx.Connect(ctx, *url+"&default_query_exec_mode=simple_protocol"); err != nil {
				fmt.Fprintln(os.Stderr, "reconnect:", err)
				os.Exit(2)
			}
		}
		use := conn
		if strings.HasPrefix(c.ID, "ext_") || c.ID == "extended_protocol_param" {
			if xconn == nil || xconn.IsClosed() {
				if xconn, err = pgx.Connect(ctx, *url+"&default_query_exec_mode=cache_statement"); err != nil {
					fmt.Fprintln(os.Stderr, "connect (extended):", err)
					os.Exit(2)
				}
			}
			use = xconn
		}
		actual, ok := run(ctx, *url, use, c)
		v := "FAIL"
		if ok {
			v, pass = "PASS", pass+1
		}
		fmt.Printf("| %s | %s | %s | `%s` | %s | **%s** |\n", c.ID, c.Task, c.Doc, c.Want, cell(actual), v)
	}
	fmt.Printf("\n%d of %d probes pass\n", pass, total)
}

func cell(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "|", "/"), "\n", " ")
	if len(s) > 110 {
		s = s[:110] + "…"
	}
	return "`" + s + "`"
}

func run(ctx context.Context, url string, conn *pgx.Conn, c check) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if c.Steps[0] == "@copy" { // COPY FROM STDIN text, then count
		return runCopy(cctx, conn, c)
	}
	// cleanup is best effort so probes do not depend on each other
	defer func() {
		conn.Exec(ctx, "DROP SEQUENCE IF EXISTS cc_seq")
		for _, t := range []string{"cc_t", "cc_p", "cc_c", "cc_v"} {
			conn.Exec(ctx, "DROP VIEW IF EXISTS "+t)
			conn.Exec(ctx, "DROP TABLE IF EXISTS "+t+" CASCADE")
		}
		conn.Exec(ctx, "ROLLBACK")
		conn.Exec(ctx, "RESET statement_timeout")
	}()
	conn.Exec(ctx, "ROLLBACK")
	conn.Exec(ctx, "RESET ALL")
	for _, t := range []string{"cc_v"} {
		conn.Exec(cctx, "DROP VIEW IF EXISTS "+t)
	}
	for _, t := range []string{"cc_c", "cc_p", "cc_t"} {
		conn.Exec(cctx, "DROP TABLE IF EXISTS "+t+" CASCADE")
	}
	for i, s := range c.Steps {
		last := i == len(c.Steps)-1
		if strings.HasPrefix(s, "~") { // setup step that may fail
			conn.Exec(cctx, s[1:])
			continue
		}
		if !last || !strings.HasPrefix(c.Want, "val:") && !strings.HasPrefix(c.Want, "has:") {
			_, err := conn.Exec(cctx, s)
			if last {
				return verdict(c.Want, "", err)
			}
			if err != nil {
				return "setup step failed: " + errText(err), false
			}
			continue
		}
		var out string
		q := "SELECT (" + strings.TrimSuffix(strings.TrimSpace(s), ";") + ")::text"
		if fullStatement(s) {
			q = s
		}
		err := conn.QueryRow(cctx, q, c.Args...).Scan(&out)
		return verdict(c.Want, out, err)
	}
	return "no steps", false
}

func fullStatement(s string) bool {
	u := strings.ToUpper(strings.TrimSpace(s))
	for _, p := range []string{"SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "SHOW", "VALUES", "EXECUTE"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

func verdict(want, got string, err error) (string, bool) {
	var pe *pgconn.PgError
	code := ""
	if errors.As(err, &pe) {
		code = pe.Code
	}
	switch {
	case strings.HasPrefix(want, "err:"):
		if err == nil {
			return "no error", false
		}
		return code + " " + errText(err), code == strings.TrimPrefix(want, "err:")
	case err != nil:
		return errText(err), false
	case want == "ok":
		return "ok", true
	case strings.HasPrefix(want, "val:"):
		return got, got == strings.TrimPrefix(want, "val:")
	case strings.HasPrefix(want, "has:"):
		return got, strings.Contains(got, strings.TrimPrefix(want, "has:"))
	}
	return "bad want", false
}

func errText(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code + ": " + pe.Message
	}
	return err.Error()
}

func runCopy(ctx context.Context, conn *pgx.Conn, c check) (string, bool) {
	conn.Exec(ctx, "DROP TABLE IF EXISTS cc_t")
	if _, err := conn.Exec(ctx, "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v TEXT)"); err != nil {
		return errText(err), false
	}
	defer conn.Exec(ctx, "DROP TABLE IF EXISTS cc_t")
	_, err := conn.PgConn().CopyFrom(ctx, bytes.NewBufferString("1\ta\n2\tb\n3\t\\N\n"), "COPY cc_t FROM STDIN")
	if err != nil {
		return errText(err), false
	}
	var n string
	if err := conn.QueryRow(ctx, "SELECT count(*)::text FROM cc_t").Scan(&n); err != nil {
		return errText(err), false
	}
	return n + " rows", n == "3"
}

const T = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v TEXT)"

func checks() []check { return append(baseChecks(), compatChecks()...) }

func baseChecks() []check {
	return []check{
		// ---- P1.2 ALTER TABLE
		{"alter_add_default", "P1.2", "05 §11", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "ALTER TABLE cc_t ADD COLUMN w BIGINT DEFAULT 7", "SELECT w FROM cc_t WHERE id=1"}, "val:7", nil},
		{"alter_drop_column", "P1.2", "05 §11", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "ALTER TABLE cc_t DROP COLUMN v", "SELECT count(*) FROM cc_t"}, "val:1", nil},
		{"alter_rename_column", "P1.2", "05 §11", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "ALTER TABLE cc_t RENAME COLUMN v TO z", "SELECT z FROM cc_t"}, "val:a", nil},
		{"alter_set_not_null", "P1.2", "05 §11", []string{T, "ALTER TABLE cc_t ALTER COLUMN v SET NOT NULL"}, "ok", nil},
		{"alter_set_default", "P1.2", "05 §11", []string{T, "ALTER TABLE cc_t ALTER COLUMN v SET DEFAULT 'x'"}, "ok", nil},
		{"alter_column_type", "P1.2", "05 §11 (listed as not done)", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v BIGINT)", "ALTER TABLE cc_t ALTER COLUMN v TYPE TEXT"}, "ok", nil},
		{"alter_rename_table", "P1.2", "05 §11", []string{T, "ALTER TABLE cc_t RENAME TO cc_p"}, "ok", nil},
		// ---- P1.3 / P1.4
		{"default_now", "P1.3", "05 §2", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, c TIMESTAMPTZ DEFAULT now())", "INSERT INTO cc_t (id) VALUES (1)", "SELECT count(*) FROM cc_t WHERE c IS NOT NULL"}, "val:1", nil},
		{"check_23514", "P1.3", "05 §2", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, a BIGINT CHECK (a > 0))", "INSERT INTO cc_t VALUES (1,-1)"}, "err:23514", nil},
		{"not_null_23502", "P1.3", "05 §2", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, a BIGINT NOT NULL)", "INSERT INTO cc_t (id) VALUES (1)"}, "err:23502", nil},
		{"serial", "P1.4", "05 §2", []string{"CREATE TABLE cc_t (id SERIAL PRIMARY KEY, v TEXT)", "INSERT INTO cc_t (v) VALUES ('a')", "INSERT INTO cc_t (v) VALUES ('b')", "SELECT max(id) FROM cc_t"}, "val:2", nil},
		// ---- P1.5
		{"truncate", "P1.5", "05 §2", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "TRUNCATE cc_t", "SELECT count(*) FROM cc_t"}, "val:0", nil},
		{"drop_table", "P1.5", "05 §2", []string{T, "DROP TABLE cc_t", "SELECT count(*) FROM cc_t"}, "err:42P01", nil},
		{"copy_from_stdin", "P1.5", "05 §15", []string{"@copy"}, "ok", nil},
		{"create_view", "P1.5", "05 §16", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "CREATE VIEW cc_v AS SELECT id FROM cc_t", "SELECT count(*) FROM cc_v"}, "val:1", nil},
		// ---- P1.1 data types
		{"ts_microseconds", "P1.1", "12 N2", []string{"SELECT '2026-01-01 00:00:00.123456+00'::timestamptz::text"}, "has:.123456", nil},
		{"numeric_exact", "P1.1", "12 N2", []string{"SELECT (0.1::numeric + 0.2::numeric)::text"}, "val:0.3", nil},
		{"numeric_wide", "P1.1", "12 N2", []string{"SELECT 12345678901234567890.123456789::numeric::text"}, "val:12345678901234567890.123456789", nil},
		{"uuid_type", "P1.1", "12 N2", []string{"SELECT pg_typeof('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid)::text"}, "val:uuid", nil},
		{"bytea_type", "P1.1", "12 N2", []string{"SELECT pg_typeof('\\xdeadbeef'::bytea)::text"}, "val:bytea", nil},
		{"jsonb_validates", "P1.1", "12 N2", []string{"SELECT '{bad'::jsonb"}, "err:22P02", nil},
		{"array_literal", "P1.1", "12 N2", []string{"SELECT (ARRAY[1,2,3])[2]::text"}, "val:2", nil},
		// ---- P2
		{"create_index", "P2.1", "12 P2.1", []string{T, "CREATE INDEX cc_i ON cc_t (v)"}, "ok", nil},
		{"unique_constraint_23505", "P2.2", "12 P2.2", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, e TEXT UNIQUE)", "INSERT INTO cc_t VALUES (1,'x')", "INSERT INTO cc_t VALUES (2,'x')"}, "err:23505", nil},
		{"foreign_key_23503", "P2.2", "12 P2.2", []string{"CREATE TABLE cc_p (id BIGINT PRIMARY KEY)", "CREATE TABLE cc_c (id BIGINT PRIMARY KEY, pid BIGINT REFERENCES cc_p(id))", "INSERT INTO cc_c VALUES (1, 99)"}, "err:23503", nil},
		{"serializable_scan", "P2.3", "12 P2.3 (scan rejected)", []string{T, "BEGIN ISOLATION LEVEL SERIALIZABLE", "SELECT count(*) FROM cc_t", "COMMIT"}, "ok", nil},
		// ---- P4 catalog
		{"pg_class", "P4.1", "05 §17", []string{T, "SELECT count(*) FROM pg_catalog.pg_class WHERE relname = 'cc_t'"}, "val:1", nil},
		{"information_schema", "P4.1", "05 §17", []string{T, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'cc_t'"}, "val:2", nil},
		{"regclass_cast", "P4.1", "05 §17", []string{T, "SELECT count(*) FROM pg_catalog.pg_class WHERE oid = 'cc_t'::regclass"}, "val:1", nil},
		// ---- P5
		{"statement_timeout", "P5.3", "05 §13", []string{"SET statement_timeout = '100ms'", "SELECT count(*) FROM generate_series(1, 100000000000)"}, "err:57014", nil},
		{"create_role", "P5.2", "12 P5.2", []string{"CREATE ROLE cc_r LOGIN PASSWORD 'x'"}, "ok", nil},
		{"grant_select", "P5.2", "12 P5.2", []string{T, "GRANT SELECT ON cc_t TO PUBLIC"}, "ok", nil},
		{"pg_stat_activity", "P5.5", "12 P5.5", []string{"SELECT count(*) FROM pg_stat_activity WHERE pid = pg_backend_pid()"}, "val:1", nil},
		// ---- P7
		{"create_trigger", "P7.1", "12 P7.1", []string{T, "CREATE TRIGGER cc_tr AFTER INSERT ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_fn()"}, "ok", nil},
		{"trigger_sql_body_fires", "P7.1", "05 §18 (SQL-body syntax)", []string{T, "CREATE TABLE cc_p (id BIGINT PRIMARY KEY, w TEXT)", "CREATE TRIGGER cc_tr AFTER INSERT ON cc_t FOR EACH ROW BEGIN INSERT INTO cc_p VALUES (NEW.id, NEW.v); END", "INSERT INTO cc_t VALUES (1, 'z')", "SELECT w FROM cc_p WHERE id = 1"}, "val:z", nil},
		{"trigger_raise_abort", "P7.1", "05 §18", []string{T, "CREATE TRIGGER cc_tr BEFORE INSERT ON cc_t FOR EACH ROW BEGIN SELECT RAISE(ABORT, 'no'); END", "INSERT INTO cc_t VALUES (1, 'z')"}, "err:P0001", nil},
		// ---- baseline SQL the README claims
		{"on_conflict_update", "base", "05 §1", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "INSERT INTO cc_t VALUES (1,'b') ON CONFLICT (id) DO UPDATE SET v = excluded.v", "SELECT v FROM cc_t WHERE id=1"}, "val:b", nil},
		{"join", "base", "05 §1", []string{T, "CREATE TABLE cc_p (id BIGINT PRIMARY KEY, w TEXT)", "INSERT INTO cc_t VALUES (1,'a')", "INSERT INTO cc_p VALUES (1,'j')", "SELECT p.w FROM cc_t t JOIN cc_p p ON p.id = t.id"}, "val:j", nil},
		{"columnar_aggregate", "base", "08", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, a DOUBLE PRECISION) WITH (columnar = true)", "INSERT INTO cc_t VALUES (1, 1.5)", "INSERT INTO cc_t VALUES (2, 2.5)", "SELECT sum(a) FROM cc_t"}, "val:4", nil},
		{"extended_protocol_param", "base", "05", []string{"SELECT $1::bigint + 1"}, "val:2", []any{int64(1)}},
		{"listen", "base", "10", []string{"LISTEN cc_ch"}, "ok", nil},
		{"bool_cast_text", "base", "PG compat", []string{"SELECT true::text"}, "val:true", nil},
		// ---- found by cmd/pgdiff (docs/14 Phụ lục B): each is a difference from PostgreSQL 16
		{"pd_table_without_pk", "pgdiff", "a: Liquibase databasechangelog", []string{"CREATE TABLE cc_t (id VARCHAR(255) NOT NULL, author VARCHAR(255))"}, "ok", nil},
		{"pd_composite_pk", "pgdiff", "c: keys", []string{"CREATE TABLE cc_t (a INT, b INT, PRIMARY KEY (a, b))"}, "ok", nil},
		{"pd_savepoint", "pgdiff", "a: transactions", []string{"BEGIN", "SAVEPOINT cc_sp", "ROLLBACK TO SAVEPOINT cc_sp"}, "ok", nil},
		{"pd_varchar_len_22001", "pgdiff", "a: constraints", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v VARCHAR(2))", "INSERT INTO cc_t VALUES (1, 'abc')"}, "err:22001", nil},
		{"pd_numeric_scale_rounds", "pgdiff", "d: SIM price", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, n NUMERIC(12,2))", "INSERT INTO cc_t VALUES (1, 1.005)", "SELECT n::text FROM cc_t"}, "val:1.01", nil},
		{"pd_numeric_overflow_22003", "pgdiff", "a: constraints", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, n NUMERIC(4,2))", "INSERT INTO cc_t VALUES (1, 999.99)"}, "err:22003", nil},
		{"pd_numeric_mul_exact", "pgdiff", "d: SIM price", []string{"SELECT (1500000.00::numeric * 1.1)::text"}, "val:1650000.000", nil},
		{"pd_int_overflow_22003", "pgdiff", "b: arithmetic", []string{"SELECT 2147483647 + 1"}, "err:22003", nil},
		{"pd_bigint_overflow_22003", "pgdiff", "b: arithmetic", []string{"SELECT 9223372036854775807 + 1"}, "err:22003", nil},
		{"pd_int_literal_type", "pgdiff", "b: types", []string{"SELECT pg_typeof(1)::text"}, "val:integer", nil},
		{"pd_timestamp_type", "pgdiff", "b: timestamps", []string{"SELECT pg_typeof('2026-01-15 10:00:00'::timestamp)::text"}, "val:timestamp without time zone", nil},
		{"pd_timestamp_no_seconds", "pgdiff", "b: timestamps", []string{"SELECT '2026-01-15 10:00'::timestamp::text"}, "val:2026-01-15 10:00:00", nil},
		{"pd_time_type", "pgdiff", "b: timestamps", []string{"SELECT '10:30:45'::time::text"}, "val:10:30:45", nil},
		{"pd_at_time_zone", "pgdiff", "b: timestamps", []string{"SELECT ('2026-01-15 10:00:00+00'::timestamptz AT TIME ZONE 'UTC')::text"}, "val:2026-01-15 10:00:00", nil},
		{"pd_to_timestamp", "pgdiff", "b: timestamps", []string{"SELECT to_timestamp('2026-03-15', 'YYYY-MM-DD')::date::text"}, "val:2026-03-15", nil},
		{"pd_bad_date_22008", "pgdiff", "c: sqlstate", []string{"SELECT '2026-02-30'::date"}, "err:22008", nil},
		{"pd_bad_timestamptz_22007", "pgdiff", "c: sqlstate", []string{"SELECT 'notadate'::timestamptz"}, "err:22007", nil},
		{"pd_data_modifying_cte", "pgdiff", "b: CTE", []string{T, "WITH i AS (INSERT INTO cc_t VALUES (1, 'a') RETURNING id) SELECT id FROM i", "SELECT count(*) FROM cc_t"}, "val:1", nil},
		{"pd_update_alias", "pgdiff", "a: Hibernate", []string{T, "INSERT INTO cc_t VALUES (1, 'a')", "UPDATE cc_t x SET v = 'b' WHERE x.id = 1"}, "ok", nil},
		{"pd_update_row_value", "pgdiff", "a: DML", []string{T, "INSERT INTO cc_t VALUES (1, 'a')", "UPDATE cc_t SET (id, v) = (1, 'q') WHERE id = 1"}, "ok", nil},
		{"pd_bitwise_or", "pgdiff", "b: operators", []string{"SELECT 5 | 3"}, "val:7", nil},
		{"pd_power_operator", "pgdiff", "b: operators", []string{"SELECT 2 ^ 10"}, "val:1024", nil},
		{"pd_dollar_quote", "pgdiff", "b: strings", []string{"SELECT $$a'b$$"}, "val:a'b", nil},
		{"pd_is_unknown", "pgdiff", "b: NULL logic", []string{"SELECT CASE WHEN NULL::boolean IS UNKNOWN THEN 'y' ELSE 'n' END"}, "val:y", nil},
		{"pd_like_escape", "pgdiff", "b: LIKE", []string{"SELECT CASE WHEN 'a_b' LIKE 'a!_b' ESCAPE '!' THEN 'y' ELSE 'n' END"}, "val:y", nil},
		{"pd_similar_to", "pgdiff", "b: LIKE", []string{"SELECT CASE WHEN 'Alice' SIMILAR TO '(A|a)l%' THEN 'y' ELSE 'n' END"}, "val:y", nil},
		{"pd_collation_order", "pgdiff", "b: ORDER BY (PG locale en_US.utf8)", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v TEXT)", "INSERT INTO cc_t VALUES (1, 'B'), (2, 'a')", "SELECT v FROM cc_t ORDER BY v LIMIT 1"}, "val:a", nil},
		{"pd_read_only_txn_25006", "pgdiff", "c: transactions", []string{T, "BEGIN READ ONLY", "INSERT INTO cc_t VALUES (1, 'a')"}, "err:25006", nil},
		{"pd_default_isolation", "pgdiff", "c: transactions", []string{"SHOW transaction_isolation"}, "val:read committed", nil},
		{"pd_jsonb_arrow", "pgdiff", "b: casts", []string{"SELECT '{\"a\":1}'::jsonb ->> 'a'"}, "val:1", nil},
		{"pd_sqrt_negative_2201F", "pgdiff", "c: arithmetic", []string{"SELECT sqrt(-1)"}, "err:2201F", nil},
		{"pd_unknown_stmt_42601", "pgdiff", "c: syntax", []string{"SELEC 1"}, "err:42601", nil},
		{"pd_unknown_type_42704", "pgdiff", "c: missing objects", []string{"SELECT 1::nosuchtype"}, "err:42704", nil},
		{"pd_on_conflict_no_index_42P10", "pgdiff", "a: ON CONFLICT", []string{T, "INSERT INTO cc_t VALUES (1, 'q') ON CONFLICT (v) DO NOTHING"}, "err:42P10", nil},
		{"pd_limit_negative_2201W", "pgdiff", "b: LIMIT", []string{T, "SELECT id FROM cc_t LIMIT -1"}, "err:2201W", nil},
		{"pd_where_not_boolean_42804", "pgdiff", "c: types", []string{"SELECT 1 FROM (VALUES (1)) t(a) WHERE a"}, "err:42804", nil},
		{"pd_ctas", "pgdiff", "c: DDL", []string{"CREATE TABLE cc_c AS SELECT 1 AS a"}, "ok", nil},
		{"pd_create_temp_table", "pgdiff", "c: DDL", []string{"CREATE TEMP TABLE cc_t (a INT)"}, "ok", nil},
		{"pd_explain", "pgdiff", "c: DDL", []string{"EXPLAIN SELECT 1"}, "ok", nil},
		{"pd_uuid_type_oid", "pgdiff", "b: casts", []string{"SELECT pg_typeof(gen_random_uuid())::text"}, "val:uuid", nil},
		// ---- not claimed anywhere: discover gaps
		{"unclaimed_cte", "-", "-", []string{"WITH x AS (SELECT 1 AS a) SELECT a FROM x"}, "val:1", nil},
		{"unclaimed_window", "-", "-", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "SELECT row_number() OVER (ORDER BY id) FROM cc_t"}, "val:1", nil},
		{"unclaimed_string_agg", "-", "-", []string{T, "INSERT INTO cc_t VALUES (1,'a')", "SELECT string_agg(v, ',') FROM cc_t"}, "val:a", nil},
		{"unclaimed_generate_series", "-", "-", []string{"SELECT count(*) FROM generate_series(1,10)"}, "val:10", nil},
	}
}
