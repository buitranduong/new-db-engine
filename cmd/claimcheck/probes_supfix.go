package main

// Probes of the supfix branch (docs/14 Appendix E follow-up): each expected value is what
// PostgreSQL 16 returns for the same statements. IDs start "sf_" ("ext_sf_" = extended protocol,
// where result column types matter). Registered through the extraChecks registry.

func init() {
	const tbl = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, q INTEGER, n NUMERIC(12,2), v TEXT, ts TIMESTAMPTZ)"
	const rows = "INSERT INTO cc_t VALUES (1, 2, 10.50, 'a', TIMESTAMPTZ '2026-01-05 10:00:00+00'), (2, 1, 1.00, 'a', TIMESTAMPTZ '2026-01-12 11:00:00+00'), (3, 4, 2.25, 'b', TIMESTAMPTZ '2026-02-01 12:00:00+00')"
	const other = "CREATE TABLE cc_p (id BIGINT PRIMARY KEY, k TEXT, w INTEGER)"
	const orows = "INSERT INTO cc_p VALUES (1, 'a', 5), (2, 'a', 1), (3, 'b', 9), (4, 'c', 3)"
	extraChecks = append(extraChecks,
		// GROUP BY: a repeated expression matches the select item (CASE, function calls, nested calls)
		check{"sf_group_by_repeated_function", "supfix", "1: group by", []string{tbl, rows, "SELECT count(*) FROM (SELECT to_char(ts, 'YYYY-MM') AS m, upper(substr(v, 1, 1)) AS u, count(*) FROM cc_t GROUP BY to_char(ts, 'YYYY-MM'), upper(substr(v, 1, 1))) g"}, "val:2", nil},
		check{"sf_group_by_case_between_in", "supfix", "1: group by", []string{tbl, rows, "SELECT k FROM (SELECT CASE WHEN q BETWEEN 1 AND 2 THEN 'x' WHEN v IN ('b') THEN 'y' ELSE 'z' END AS k, count(*) FROM cc_t GROUP BY CASE WHEN q BETWEEN 1 AND 2 THEN 'x' WHEN v IN ('b') THEN 'y' ELSE 'z' END ORDER BY k LIMIT 1) t"}, "val:x", nil},
		check{"sf_group_by_date_trunc_extract", "supfix", "1: group by", []string{tbl, rows, "SELECT count(*) FROM (SELECT date_trunc('week', ts), extract(dow FROM ts) FROM cc_t GROUP BY date_trunc('week', ts), extract(dow FROM ts)) g"}, "val:3", nil},
		// numeric typing: computed columns keep numeric (scale) in the extended protocol too
		check{"ext_sf_numeric_product_group", "supfix", "2: numeric", []string{tbl, other, rows, orows, "SELECT sum(t.q * t.n) FROM cc_t t JOIN cc_p p ON p.id = t.id GROUP BY p.k ORDER BY 1 LIMIT 1"}, "val:9.00", nil},
		check{"ext_sf_variance_numeric", "supfix", "2: numeric", []string{tbl, rows, "SELECT var_pop(n) FROM cc_t"}, "val:17.7638888888888889", nil},
		check{"ext_sf_stddev_int", "supfix", "2: numeric", []string{tbl, rows, "SELECT stddev(q) FROM cc_t"}, "val:1.5275252316519467", nil},
		check{"ext_sf_sum_bigint_window", "supfix", "2: numeric", []string{tbl, rows, "SELECT pg_typeof(sum(c) OVER ()) FROM (SELECT count(*) AS c FROM cc_t GROUP BY v) t LIMIT 1"}, "val:numeric", nil},
		check{"ext_sf_percent_of_total", "supfix", "2: numeric", []string{tbl, rows, "SELECT sum(n) * 100.0 / sum(sum(n)) OVER () FROM cc_t GROUP BY v ORDER BY v LIMIT 1"}, "val:83.6363636363636364", nil},
		// to_char: ISO week and the rest of the pattern table
		check{"sf_to_char_week_of_year", "supfix", "3: to_char", []string{"SELECT to_char(TIMESTAMPTZ '2026-12-31 23:59:59+00', 'IYYY-IW-ID WW W Q DDD J')"}, "val:2026-53-4 53 5 4 365 2461406", nil},
		check{"sf_to_char_ampm_ordinal", "supfix", "3: to_char", []string{"SELECT to_char(TIMESTAMPTZ '2026-03-01 13:05:00+00', 'FMDDth FMMonth YYYY, HH12:MI PM, FMDay FMRM')"}, "val:1st March 2026, 01:05 PM, Sunday III", nil},
		// regclass prints the relation name
		check{"sf_regclass_to_regclass_text", "supfix", "4: catalog", []string{tbl, "SELECT to_regclass('cc_t')::text || ',' || 'public.cc_t'::regclass::text"}, "val:cc_t,cc_t", nil},
		check{"sf_reltuples_and_size", "supfix", "4: catalog", []string{tbl, rows, "ANALYZE cc_t", "SELECT (SELECT reltuples FROM pg_class WHERE relname = 'cc_t') = 3 AND pg_relation_size('cc_t') > 0 AND pg_total_relation_size('cc_t') > pg_relation_size('cc_t')"}, "val:t", nil},
		check{"ext_sf_catalog_name_oid_types", "supfix", "4: catalog", []string{tbl, "SELECT pg_typeof(relname)::text || ',' || pg_typeof(oid)::text || ',' || pg_typeof(relkind)::text FROM pg_class WHERE relname = 'cc_t'"}, "val:name,oid,\"char\"", nil},
		check{"sf_viewdef_layout", "supfix", "4: catalog", []string{tbl, "CREATE VIEW cc_v AS SELECT id, v FROM cc_t WHERE v = 'a'", "SELECT pg_get_viewdef('cc_v'::regclass, true)"}, "val: SELECT id,\n    v\n   FROM cc_t\n  WHERE v = 'a'::text;", nil},
		// syntax
		check{"sf_cte_not_materialized", "supfix", "5: syntax", []string{"WITH c AS NOT MATERIALIZED (SELECT 7 AS x) SELECT x FROM c"}, "val:7", nil},
		check{"sf_quoted_label_with_quotes", "supfix", "5: syntax", []string{tbl, rows, "SELECT count(*) FROM (SELECT v AS \"v \"\"x\"\"\", count(*) AS \"my metric\" FROM cc_t GROUP BY 1) t"}, "val:2", nil},
		check{"sf_statistical_aggregates", "supfix", "5: aggregates", []string{tbl, rows, "SELECT round(corr(n, q)::numeric, 4) || ',' || round(covar_samp(n, q)::numeric, 4) || ',' || regr_count(n, q) || ',' || round(regr_slope(n, q)::numeric, 4) FROM cc_t"}, "val:-0.0687,-0.5417,3,-0.2321", nil},
		// EXISTS: residual predicates, HAVING, grouped subqueries
		check{"sf_exists_residual", "supfix", "6: exists", []string{tbl, other, rows, orows, "SELECT count(*) FROM cc_t t WHERE EXISTS (SELECT 1 FROM cc_p p WHERE p.k = t.v AND p.w < t.q)"}, "val:1", nil},
		check{"sf_not_exists_residual", "supfix", "6: exists", []string{tbl, other, rows, orows, "SELECT count(*) FROM cc_t t WHERE NOT EXISTS (SELECT 1 FROM cc_p p WHERE p.k = t.v AND p.w < t.q)"}, "val:2", nil},
		check{"sf_exists_having", "supfix", "6: exists", []string{tbl, other, rows, orows, "SELECT count(*) FROM cc_t t WHERE EXISTS (SELECT 1 FROM cc_p p WHERE p.k = t.v GROUP BY p.k HAVING count(*) > 1)"}, "val:2", nil},
		check{"sf_exists_having_empty_group", "supfix", "6: exists", []string{tbl, other, rows, orows, "SELECT count(*) FROM cc_t t WHERE EXISTS (SELECT 1 FROM cc_p p WHERE p.k = t.v || 'z' HAVING count(*) = 0)"}, "val:3", nil},
		check{"sf_in_grouped_subquery", "supfix", "6: exists", []string{tbl, other, rows, orows, "SELECT count(*) FROM cc_t t WHERE t.q IN (SELECT max(p.w) FROM cc_p p WHERE p.k = t.v GROUP BY p.k)"}, "val:0", nil},
		// EXPLAIN carries cost=a..b rows=n width=w on its first line (SQL Lab reads it)
		check{"sf_explain_has_cost", "supfix", "7: explain", []string{tbl, rows, "EXPLAIN SELECT v, count(*) FROM cc_t GROUP BY v"}, "has:cost=", nil},
		check{"sf_set_search_path_list", "supfix", "5: syntax", []string{"SET search_path TO public, pg_catalog", "SHOW search_path"}, "val:public, pg_catalog", nil},
		check{"sf_index_on_timestamp", "supfix", "5: syntax", []string{tbl, "CREATE INDEX cc_t_ts_idx ON cc_t (ts)", rows, "SELECT count(*) FROM cc_t WHERE ts >= TIMESTAMPTZ '2026-01-10 00:00:00+00'"}, "val:2", nil},
	)
}
