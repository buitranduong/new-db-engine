package main

// Probes for the Apache Superset / SQLAlchemy (dialect postgresql) workload W9, one per
// important difference found by scripts/pgdiff/g_superset.sql (docs/14 Appendix C).
// IDs start "sq_" ("ext_sq_" = extended protocol, where the numeric-as-float8 difference shows). The expected value is what PostgreSQL 16 returns.
//
// Registration needs one line in checks() of main.go (not edited by this branch):
//
//	func checks() []check { return append(append(baseChecks(), compatChecks()...), supersetChecks()...) }

func supersetChecks() []check {
	const tsTbl = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, ts TIMESTAMPTZ)"
	return []check{
		// SQLAlchemy 2.0 reflection (get_table_names, get_columns, get_pk_constraint, comments, check
		// constraints) writes every column as schema.table.column.
		{"sq_catalog_threepart_colref", "superset", "a: reflection", []string{T, "SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class WHERE pg_catalog.pg_class.relname = 'cc_t' AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid)"}, "val:cc_t", nil},
		// Calculated columns / buckets: the same CASE in SELECT and GROUP BY.
		{"sq_group_by_case_expr", "superset", "b: chart", []string{T, TS3, "SELECT CASE WHEN id < 2 THEN 'lo' ELSE 'hi' END AS b FROM cc_t GROUP BY CASE WHEN id < 2 THEN 'lo' ELSE 'hi' END ORDER BY b LIMIT 1"}, "val:hi", nil},
		// Revenue charts: SUM(qty * unit_price) over a JOIN must stay numeric with its scale (22.00, not 22).
		{"ext_sq_numeric_sum_join_product", "superset", "b: chart", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, q INTEGER, n NUMERIC(12,2))", "CREATE TABLE cc_p (id BIGINT PRIMARY KEY)", "INSERT INTO cc_t VALUES (1, 2, 10.50), (2, 1, 1.00)", "INSERT INTO cc_p VALUES (1), (2)", "SELECT sum(t.q * t.n) FROM cc_t t JOIN cc_p p ON p.id = t.id"}, "val:22.00", nil},
		// "Epoch ms" time column of the time-series chart: EXTRACT returns numeric since PG 14, shown as 1767261600000.000000.
		{"ext_sq_extract_epoch_group_by", "superset", "b: chart", []string{tsTbl, "INSERT INTO cc_t VALUES (1, TIMESTAMPTZ '2026-01-01 10:00:00+00')", "SELECT extract(epoch from ts) * 1000 AS e FROM cc_t GROUP BY 1 ORDER BY 1"}, "val:1767261600000.000000", nil},
		// Week labels: ISO week and a quoted literal inside the to_char pattern.
		{"sq_to_char_iso_week_literal", "superset", "b: chart", []string{"SELECT to_char(TIMESTAMPTZ '2026-01-05 00:00:00+00', 'IYYY-\"W\"IW')"}, "val:2026-W02", nil},
		// The regclass text form is the table name, not the numeric oid.
		{"sq_regclass_text_name", "superset", "a: reflection", []string{T, "SELECT 'cc_t'::regclass::text"}, "val:cc_t", nil},
		// SQL Lab users paste CTEs written for PostgreSQL 12+.
		{"sq_cte_materialized", "superset", "c: SQL Lab", []string{"WITH c AS MATERIALIZED (SELECT 1 AS x) SELECT x FROM c"}, "val:1", nil},
		// Column labels with a quote are emitted as "a ""b""".
		{"sq_quoted_ident_doubled_quote", "superset", "b: chart", []string{"SELECT 1::text AS \"a \"\"b\"\"\""}, "val:1", nil},
		// Index on the time column Superset filters and groups by.
		{"sq_index_on_timestamptz", "superset", "b: chart", []string{tsTbl, "CREATE INDEX cc_t_ts_idx ON cc_t (ts)"}, "ok", nil},
		// Aggregates beyond the basic set (corr, ...) must be recognised as aggregates, not scalars.
		{"sq_corr_aggregate_group_by", "superset", "b: chart", []string{T, TS3, "SELECT count(*) FROM (SELECT v, corr(id, id) FROM cc_t GROUP BY v) g"}, "val:3", nil},
		// Control (no difference expected): SQL Lab sends several statements in one simple-protocol message.
		{"sq_multi_statement_simple_protocol", "superset", "c: SQL Lab", []string{"SELECT 1; SELECT 2"}, "ok", nil},
	}
}
