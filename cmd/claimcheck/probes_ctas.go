package main

// Probes of the basic table/data commands (branch ctas, docs/05 last section, docs/14 Appendix B group h):
// CREATE TABLE AS, SELECT INTO, LIKE, TEMP, materialized views, MERGE, TRUNCATE, INSERT forms.
// IDs start "ct_". The expected value is what PostgreSQL 16 returns. A connection is reused between
// probes, so each probe that makes an object outside cc_t/cc_p/cc_c/cc_v drops it first.

func init() {
	const dm = "~DROP MATERIALIZED VIEW IF EXISTS cc_mv"
	const dmv = "~DROP MATERIALIZED VIEW IF EXISTS cc_mv2"
	const dt = "~DROP TABLE IF EXISTS cc_tt"
	const dp = "~DROP TABLE IF EXISTS cc_pub"
	extraChecks = append(extraChecks, []check{
		{"ct_ctas_rows", "ctas", "05 §ctas", []string{T, TS3, "CREATE TABLE cc_c AS SELECT id, v FROM cc_t WHERE id < 3", "SELECT count(*) FROM cc_c"}, "val:2", nil},
		{"ct_ctas_int4_type", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INTEGER PRIMARY KEY, n NUMERIC(12,2), s VARCHAR(7))", "INSERT INTO cc_p VALUES (1, 1.00, 'a')", "CREATE TABLE cc_c AS SELECT id, n, s FROM cc_p", "SELECT pg_typeof(id)::text FROM cc_c LIMIT 1"}, "val:integer", nil},
		{"ct_ctas_numeric_typmod", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INTEGER PRIMARY KEY, n NUMERIC(12,2), s VARCHAR(7))", "CREATE TABLE cc_c AS SELECT id, n, s FROM cc_p", "SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'cc_c'::regclass AND attname = 'n'"}, "val:numeric(12,2)", nil},
		{"ct_ctas_varchar_typmod", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INTEGER PRIMARY KEY, n NUMERIC(12,2), s VARCHAR(7))", "CREATE TABLE cc_c AS SELECT id, n, s FROM cc_p", "SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'cc_c'::regclass AND attname = 's'"}, "val:character varying(7)", nil},
		{"ct_ctas_no_data", "ctas", "05 §ctas", []string{T, TS3, "CREATE TABLE cc_c AS SELECT * FROM cc_t WITH NO DATA", "SELECT count(*) FROM cc_c"}, "val:0", nil},
		{"ct_ctas_column_names", "ctas", "05 §ctas", []string{"CREATE TABLE cc_c (a, b) AS SELECT 1, 'x'", "SELECT b FROM cc_c WHERE a = 1"}, "val:x", nil},
		{"ct_ctas_exists_42P07", "ctas", "05 §ctas", []string{T, "CREATE TABLE cc_c AS SELECT 1", "CREATE TABLE cc_c AS SELECT 2"}, "err:42P07", nil},
		{"ct_select_into", "ctas", "05 §ctas", []string{T, TS3, "SELECT id, v INTO cc_c FROM cc_t WHERE id > 1", "SELECT count(*) FROM cc_c"}, "val:2", nil},
		{"ct_like_basic", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INT PRIMARY KEY, code TEXT NOT NULL DEFAULT 'z')", "CREATE TABLE cc_c (LIKE cc_p)", "INSERT INTO cc_c (id, code) VALUES (1, 'a'), (1, 'a')", "SELECT count(*) FROM cc_c"}, "val:2", nil},
		{"ct_like_not_null", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INT PRIMARY KEY, code TEXT NOT NULL DEFAULT 'z')", "CREATE TABLE cc_c (LIKE cc_p)", "INSERT INTO cc_c (id) VALUES (1)"}, "err:23502", nil},
		{"ct_like_including_defaults", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INT PRIMARY KEY, code TEXT NOT NULL DEFAULT 'z')", "CREATE TABLE cc_c (LIKE cc_p INCLUDING DEFAULTS)", "INSERT INTO cc_c (id) VALUES (1)", "SELECT code FROM cc_c"}, "val:z", nil},
		{"ct_like_including_all_pk", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INT PRIMARY KEY, n INT CHECK (n > 0), u TEXT UNIQUE)", "CREATE TABLE cc_c (LIKE cc_p INCLUDING ALL, extra INT)", "INSERT INTO cc_c (id, n, u) VALUES (1, 1, 'a')", "INSERT INTO cc_c (id, n, u) VALUES (1, 2, 'b')"}, "err:23505", nil},
		{"ct_like_including_all_check", "ctas", "05 §ctas", []string{"CREATE TABLE cc_p (id INT PRIMARY KEY, n INT CHECK (n > 0), u TEXT UNIQUE)", "CREATE TABLE cc_c (LIKE cc_p INCLUDING ALL)", "INSERT INTO cc_c (id, n, u) VALUES (1, -1, 'a')"}, "err:23514", nil},
		{"ct_temp_shadows_public", "ctas", "05 §temp", []string{dp, dp, "CREATE TABLE cc_pub (v TEXT)", "INSERT INTO cc_pub VALUES ('public')", "CREATE TEMP TABLE cc_pub (v TEXT)", "INSERT INTO cc_pub VALUES ('temp')", "SELECT v FROM cc_pub"}, "val:temp", nil},
		{"ct_temp_in_pg_temp_schema", "ctas", "05 §temp", []string{dt, "CREATE TEMPORARY TABLE cc_tt (a INT)", "SELECT relpersistence::text FROM pg_class WHERE relname = 'cc_tt'"}, "val:t", nil},
		{"ct_temp_on_commit_drop", "ctas", "05 §temp", []string{dt, "BEGIN", "CREATE TEMP TABLE cc_tt (a INT) ON COMMIT DROP", "COMMIT", "SELECT * FROM cc_tt"}, "err:42P01", nil},
		{"ct_temp_on_commit_delete_rows", "ctas", "05 §temp", []string{dt, "BEGIN", "CREATE TEMP TABLE cc_tt (a INT) ON COMMIT DELETE ROWS", "INSERT INTO cc_tt VALUES (1)", "COMMIT", "SELECT count(*) FROM cc_tt"}, "val:0", nil},
		{"ct_temp_ctas", "ctas", "05 §temp", []string{dt, "CREATE TEMP TABLE cc_tt AS SELECT 5 AS x", "SELECT x FROM cc_tt"}, "val:5", nil},
		{"ct_unlogged_table", "ctas", "05 §temp", []string{"CREATE UNLOGGED TABLE cc_c (a INT PRIMARY KEY)", "INSERT INTO cc_c VALUES (1)", "SELECT a FROM cc_c"}, "val:1", nil},
		{"ct_matview_select", "ctas", "05 §matview", []string{dm, T, TS3, "CREATE MATERIALIZED VIEW cc_mv AS SELECT count(*) AS n FROM cc_t", "SELECT n FROM cc_mv"}, "val:3", nil},
		{"ct_matview_stale_until_refresh", "ctas", "05 §matview", []string{dm, T, TS3, "CREATE MATERIALIZED VIEW cc_mv AS SELECT count(*) AS n FROM cc_t", "INSERT INTO cc_t VALUES (9, 'z')", "SELECT n FROM cc_mv"}, "val:3", nil},
		{"ct_matview_refresh", "ctas", "05 §matview", []string{dm, T, TS3, "CREATE MATERIALIZED VIEW cc_mv AS SELECT count(*) AS n FROM cc_t", "INSERT INTO cc_t VALUES (9, 'z')", "REFRESH MATERIALIZED VIEW cc_mv", "SELECT n FROM cc_mv"}, "val:4", nil},
		{"ct_matview_relkind", "ctas", "05 §matview", []string{dm, "CREATE MATERIALIZED VIEW cc_mv AS SELECT 1 AS n", "SELECT relkind::text FROM pg_class WHERE relname = 'cc_mv'"}, "val:m", nil},
		{"ct_matview_pg_matviews", "ctas", "05 §matview", []string{dm, "CREATE MATERIALIZED VIEW cc_mv AS SELECT 1 AS n", "SELECT matviewname::text FROM pg_matviews WHERE matviewname = 'cc_mv'"}, "val:cc_mv", nil},
		{"ct_matview_no_data_55000", "ctas", "05 §matview", []string{dm, "CREATE MATERIALIZED VIEW cc_mv AS SELECT 1 AS n WITH NO DATA", "SELECT * FROM cc_mv"}, "err:55000", nil},
		{"ct_matview_write_42809", "ctas", "05 §matview", []string{dm, "CREATE MATERIALIZED VIEW cc_mv AS SELECT 1 AS n", "INSERT INTO cc_mv VALUES (2)"}, "err:42809", nil},
		{"ct_matview_concurrently_needs_index", "ctas", "05 §matview", []string{dm, "CREATE MATERIALIZED VIEW cc_mv AS SELECT 1 AS n", "REFRESH MATERIALIZED VIEW CONCURRENTLY cc_mv"}, "err:55000", nil},
		{"ct_matview_concurrently", "ctas", "05 §matview", []string{dm, T, TS3, "CREATE MATERIALIZED VIEW cc_mv AS SELECT id FROM cc_t", "CREATE UNIQUE INDEX cc_mv_id ON cc_mv (id)", "DELETE FROM cc_t WHERE id = 1", "REFRESH MATERIALIZED VIEW CONCURRENTLY cc_mv", "SELECT count(*) FROM cc_mv"}, "val:2", nil},
		{"ct_matview_drop", "ctas", "05 §matview", []string{dm, "CREATE MATERIALIZED VIEW cc_mv AS SELECT 1 AS n", "DROP MATERIALIZED VIEW cc_mv", "SELECT * FROM cc_mv"}, "err:42P01", nil},
		{"ct_matview_drop_table_42809", "ctas", "05 §matview", []string{dm, "CREATE MATERIALIZED VIEW cc_mv AS SELECT 1 AS n", "DROP TABLE cc_mv"}, "err:42809", nil},
		{"ct_matview_drop_if_exists", "ctas", "05 §matview", []string{dmv, "DROP MATERIALIZED VIEW IF EXISTS cc_mv2"}, "ok", nil},
		{"ct_merge_update_insert", "ctas", "05 §merge", []string{T, "CREATE TABLE cc_p (id BIGINT, v TEXT)", "INSERT INTO cc_t VALUES (1, 'old'), (2, 'old')", "INSERT INTO cc_p VALUES (2, 'new'), (3, 'ins')", "MERGE INTO cc_t t USING cc_p s ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v WHEN NOT MATCHED THEN INSERT (id, v) VALUES (s.id, s.v)", "SELECT string_agg(id || v, ',' ORDER BY id) FROM cc_t"}, "val:1old,2new,3ins", nil},
		{"ct_merge_delete", "ctas", "05 §merge", []string{T, "CREATE TABLE cc_p (id BIGINT, v TEXT)", "INSERT INTO cc_t VALUES (1, 'a'), (2, 'b')", "INSERT INTO cc_p VALUES (2, 'x')", "MERGE INTO cc_t t USING cc_p s ON t.id = s.id WHEN MATCHED THEN DELETE", "SELECT count(*) FROM cc_t"}, "val:1", nil},
		{"ct_merge_condition_do_nothing", "ctas", "05 §merge", []string{T, "CREATE TABLE cc_p (id BIGINT, v TEXT)", "INSERT INTO cc_t VALUES (1, 'a')", "INSERT INTO cc_p VALUES (1, 'skip')", "MERGE INTO cc_t t USING cc_p s ON t.id = s.id WHEN MATCHED AND s.v = 'skip' THEN DO NOTHING WHEN MATCHED THEN UPDATE SET v = s.v", "SELECT v FROM cc_t"}, "val:a", nil},
		{"ct_merge_twice_21000", "ctas", "05 §merge", []string{T, "CREATE TABLE cc_p (id BIGINT, v TEXT)", "INSERT INTO cc_t VALUES (1, 'a')", "INSERT INTO cc_p VALUES (1, 'x'), (1, 'y')", "MERGE INTO cc_t t USING cc_p s ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v"}, "err:21000", nil},
		{"ct_merge_values_source", "ctas", "05 §merge", []string{T, "MERGE INTO cc_t t USING (VALUES (1, 'a'), (2, 'b')) s(id, v) ON t.id = s.id WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.v)", "SELECT count(*) FROM cc_t"}, "val:2", nil},
		{"ct_merge_tag", "ctas", "05 §merge", []string{T, "CREATE TABLE cc_p (id BIGINT, v TEXT)", "INSERT INTO cc_p VALUES (1, 'a'), (2, 'b')", "MERGE INTO cc_t t USING cc_p s ON t.id = s.id WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.v)"}, "ok", nil},
		{"ct_truncate_fk_0A000", "ctas", "05 §truncate", []string{"CREATE TABLE cc_p (id BIGINT PRIMARY KEY)", "CREATE TABLE cc_c (id BIGINT PRIMARY KEY, pid BIGINT REFERENCES cc_p(id))", "TRUNCATE cc_p"}, "err:0A000", nil},
		{"ct_truncate_cascade", "ctas", "05 §truncate", []string{"CREATE TABLE cc_p (id BIGINT PRIMARY KEY)", "CREATE TABLE cc_c (id BIGINT PRIMARY KEY, pid BIGINT REFERENCES cc_p(id))", "INSERT INTO cc_p VALUES (1)", "INSERT INTO cc_c VALUES (1, 1)", "TRUNCATE cc_p CASCADE", "SELECT count(*) FROM cc_c"}, "val:0", nil},
		{"ct_truncate_several", "ctas", "05 §truncate", []string{T, "CREATE TABLE cc_p (id BIGINT PRIMARY KEY)", "INSERT INTO cc_t VALUES (1, 'a')", "INSERT INTO cc_p VALUES (1)", "TRUNCATE cc_t, cc_p", "SELECT (SELECT count(*) FROM cc_t) + (SELECT count(*) FROM cc_p)"}, "val:0", nil},
		{"ct_truncate_restart_identity", "ctas", "05 §truncate", []string{"CREATE TABLE cc_t (id SERIAL PRIMARY KEY, v TEXT)", "INSERT INTO cc_t (v) VALUES ('a'), ('b')", "TRUNCATE cc_t RESTART IDENTITY", "INSERT INTO cc_t (v) VALUES ('c')", "SELECT id FROM cc_t"}, "val:1", nil},
		{"ct_truncate_continue_identity", "ctas", "05 §truncate", []string{"CREATE TABLE cc_t (id SERIAL PRIMARY KEY, v TEXT)", "INSERT INTO cc_t (v) VALUES ('a'), ('b')", "TRUNCATE cc_t CONTINUE IDENTITY", "INSERT INTO cc_t (v) VALUES ('c')", "SELECT id FROM cc_t"}, "val:3", nil},
		{"ct_insert_overriding_system", "ctas", "05 §insert", []string{"CREATE TABLE cc_t (id INT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v TEXT)", "INSERT INTO cc_t (id, v) OVERRIDING SYSTEM VALUE VALUES (7, 'a')", "SELECT id FROM cc_t"}, "val:7", nil},
		{"ct_insert_overriding_user", "ctas", "05 §insert", []string{"CREATE TABLE cc_t (id INT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v TEXT)", "INSERT INTO cc_t (id, v) OVERRIDING USER VALUE VALUES (7, 'a')", "SELECT id FROM cc_t"}, "val:1", nil},
		{"ct_insert_default_values", "ctas", "05 §insert", []string{"CREATE TABLE cc_t (id SERIAL PRIMARY KEY, v TEXT DEFAULT 'd')", "INSERT INTO cc_t DEFAULT VALUES", "SELECT v FROM cc_t"}, "val:d", nil},
	}...)
}
