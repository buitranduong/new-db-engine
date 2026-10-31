package main

// Probes of the engine gaps closed on branch enginegaps (docs/14 Appendix X): a transaction that
// writes then ALTERs, VALUES / RETURNING subqueries, UPDATE/DELETE aliases, serializable scans and
// the statements made transactional under the DDL journal. IDs start "eg_".

func init() {
	pre := func(extra ...string) []string {
		return append([]string{"~ROLLBACK", "~DROP TABLE IF EXISTS eg_t", "~DROP TABLE IF EXISTS eg_u", "~DROP TABLE IF EXISTS eg_k",
			"~DROP SCHEMA IF EXISTS eg_s CASCADE", "~DROP TYPE IF EXISTS eg_mood CASCADE", "~DROP DOMAIN IF EXISTS eg_pos CASCADE",
			"~DROP ROLE IF EXISTS eg_r", "~DROP PUBLICATION IF EXISTS eg_pub", "~DROP MATERIALIZED VIEW IF EXISTS eg_mv",
			"~DROP TABLE IF EXISTS eg_tmp",
			"CREATE TABLE eg_t (id INT PRIMARY KEY, v TEXT)", "INSERT INTO eg_t VALUES (1, 'a')"}, extra...)
	}
	extraChecks = append(extraChecks,
		check{"eg_write_then_alter_commit", "enginegaps", "ddl", pre("BEGIN", "INSERT INTO eg_t VALUES (2, 'b')", "ALTER TABLE eg_t ADD COLUMN c INT DEFAULT 7", "COMMIT", "SELECT sum(c)::text FROM eg_t"), "val:14", nil},
		check{"eg_write_then_rename_commit", "enginegaps", "ddl", pre("BEGIN", "INSERT INTO eg_t VALUES (2, 'b')", "ALTER TABLE eg_t RENAME COLUMN v TO w", "COMMIT", "SELECT w FROM eg_t WHERE id = 2"), "val:b", nil},
		check{"eg_write_then_index_commit", "enginegaps", "ddl", pre("BEGIN", "INSERT INTO eg_t VALUES (2, 'b')", "CREATE INDEX eg_t_v ON eg_t (v)", "COMMIT", "SELECT id::text FROM eg_t WHERE v = 'b'"), "val:2", nil},
		check{"eg_rollback_to_after_alter", "enginegaps", "ddl", pre("BEGIN", "INSERT INTO eg_t VALUES (2, 'b')", "SAVEPOINT sp", "ALTER TABLE eg_t ADD COLUMN c INT", "ROLLBACK TO sp", "COMMIT", "SELECT count(*)::text FROM eg_t"), "val:2", nil},
		check{"eg_values_subquery", "enginegaps", "dml", pre("INSERT INTO eg_t VALUES (2, (SELECT v FROM eg_t WHERE id = 1))", "SELECT v FROM eg_t WHERE id = 2"), "val:a", nil},
		check{"eg_values_subquery_expr", "enginegaps", "dml", pre("INSERT INTO eg_t VALUES ((SELECT max(id) + 1 FROM eg_t), 'n')", "SELECT id::text FROM eg_t WHERE v = 'n'"), "val:2", nil},
		check{"eg_returning_subquery", "enginegaps", "dml", pre("INSERT INTO eg_t VALUES (2, 'b') RETURNING (SELECT count(*) FROM eg_t)::text"), "val:1", nil},
		check{"eg_update_alias_as", "enginegaps", "dml", pre("UPDATE eg_t AS x SET v = x.v || '!' WHERE x.id = 1", "SELECT v FROM eg_t"), "val:a!", nil},
		check{"eg_delete_alias", "enginegaps", "dml", pre("DELETE FROM eg_t x WHERE x.id = 1", "SELECT count(*)::text FROM eg_t"), "val:0", nil},
		check{"eg_serializable_scan_commit", "enginegaps", "tx", pre("BEGIN ISOLATION LEVEL SERIALIZABLE", "SELECT count(*) FROM eg_t", "INSERT INTO eg_t VALUES (2, 'b')", "COMMIT", "SELECT count(*)::text FROM eg_t"), "val:2", nil},
		check{"eg_show_isolation_in_tx", "enginegaps", "tx", pre("BEGIN ISOLATION LEVEL REPEATABLE READ", "SHOW transaction_isolation"), "val:repeatable read", nil},
		check{"eg_rollback_create_role", "enginegaps", "ddl", pre("BEGIN", "CREATE ROLE eg_r LOGIN", "GRANT SELECT ON eg_t TO eg_r", "ROLLBACK", "DROP ROLE eg_r"), "err:42704", nil},
		check{"eg_rollback_drop_role", "enginegaps", "ddl", pre("CREATE ROLE eg_r", "BEGIN", "DROP ROLE eg_r", "ROLLBACK", "DROP ROLE eg_r"), "ok", nil},
		check{"eg_rollback_add_pk", "enginegaps", "ddl", pre("CREATE TABLE eg_k (a INT)", "BEGIN", "ALTER TABLE eg_k ADD PRIMARY KEY (a)", "ROLLBACK", "INSERT INTO eg_k VALUES (1), (1)", "SELECT count(*)::text FROM eg_k"), "val:2", nil},
		check{"eg_commit_add_pk", "enginegaps", "ddl", pre("CREATE TABLE eg_k (a INT)", "BEGIN", "ALTER TABLE eg_k ADD PRIMARY KEY (a)", "COMMIT", "INSERT INTO eg_k VALUES (1), (1)"), "err:23505", nil},
		check{"eg_rollback_enum_add_value", "enginegaps", "ddl", pre("CREATE TYPE eg_mood AS ENUM ('a','b')", "BEGIN", "ALTER TYPE eg_mood ADD VALUE 'c'", "ROLLBACK", "SELECT 'c'::eg_mood::text"), "err:22P02", nil},
		check{"eg_rollback_drop_type_cascade", "enginegaps", "ddl", pre("CREATE TYPE eg_mood AS ENUM ('a','b')", "CREATE TABLE eg_u (id INT PRIMARY KEY, m eg_mood)", "INSERT INTO eg_u VALUES (1, 'a')", "BEGIN", "DROP TYPE eg_mood CASCADE", "ROLLBACK", "SELECT m::text FROM eg_u"), "val:a", nil},
		check{"eg_rollback_drop_schema_cascade", "enginegaps", "ddl", pre("CREATE SCHEMA eg_s", "CREATE TABLE eg_s.x (id INT PRIMARY KEY)", "INSERT INTO eg_s.x VALUES (1)", "BEGIN", "DROP SCHEMA eg_s CASCADE", "ROLLBACK", "SELECT count(*)::text FROM eg_s.x"), "val:1", nil},
		check{"eg_commit_drop_schema_cascade", "enginegaps", "ddl", pre("CREATE SCHEMA eg_s", "CREATE TABLE eg_s.x (id INT PRIMARY KEY)", "BEGIN", "DROP SCHEMA eg_s CASCADE", "COMMIT", "SELECT count(*)::text FROM eg_s.x"), "err:42P01", nil},
		check{"eg_rollback_trigram_index", "enginegaps", "ddl", pre("BEGIN", "CREATE INDEX eg_t_trgm ON eg_t USING gin (v gin_trgm_ops)", "ROLLBACK", "CREATE INDEX eg_t_trgm ON eg_t USING gin (v gin_trgm_ops)"), "ok", nil},
		check{"eg_rollback_publication", "enginegaps", "ddl", pre("BEGIN", "CREATE PUBLICATION eg_pub FOR TABLE eg_t", "ROLLBACK", "DROP PUBLICATION eg_pub"), "err:42704", nil},
		check{"eg_rollback_drop_temp", "enginegaps", "ddl", pre("CREATE TEMP TABLE eg_tmp (id INT)", "INSERT INTO eg_tmp VALUES (5)", "BEGIN", "DROP TABLE eg_tmp", "ROLLBACK", "SELECT id::text FROM eg_tmp"), "val:5", nil},
		check{"eg_rollback_to_create_temp", "enginegaps", "ddl", pre("BEGIN", "SAVEPOINT sp", "CREATE TEMP TABLE eg_tmp (id INT)", "ROLLBACK TO sp", "CREATE TEMP TABLE eg_tmp (id INT, z INT)", "COMMIT", "SELECT count(z)::text FROM eg_tmp"), "val:0", nil},
		check{"eg_rollback_refresh_matview", "enginegaps", "ddl", pre("CREATE MATERIALIZED VIEW eg_mv AS SELECT count(*) AS n FROM eg_t", "INSERT INTO eg_t VALUES (2, 'b')", "BEGIN", "REFRESH MATERIALIZED VIEW eg_mv", "ROLLBACK", "SELECT n::text FROM eg_mv"), "val:1", nil},
		check{"eg_rollback_drop_matview", "enginegaps", "ddl", pre("CREATE MATERIALIZED VIEW eg_mv AS SELECT count(*) AS n FROM eg_t", "BEGIN", "DROP MATERIALIZED VIEW eg_mv", "ROLLBACK", "SELECT n::text FROM eg_mv"), "val:1", nil},
		check{"ext_eg_any_pk_array", "enginegaps", "dml", pre("INSERT INTO eg_t VALUES (2, 'b'), (3, 'c')", "SELECT string_agg(v, ',' ORDER BY id) FROM eg_t WHERE id = ANY($1)"), "val:a,c", []any{[]int64{1, 3, 99}}},
		check{"ext_eg_any_empty_array", "enginegaps", "dml", pre("SELECT count(*)::text FROM eg_t WHERE id = ANY($1)"), "val:0", []any{[]int64{}}},
		check{"eg_after_row_after_statement", "enginegaps", "trigger", pre("~DROP TABLE IF EXISTS eg_lg", "CREATE TABLE eg_lg (n INT)", "CREATE TRIGGER eg_ar AFTER INSERT ON eg_t FOR EACH ROW BEGIN INSERT INTO eg_lg VALUES ((SELECT count(*) FROM eg_t)); END", "INSERT INTO eg_t VALUES (2, 'a'), (3, 'b')", "SELECT string_agg(n::text, ',') FROM eg_lg"), "val:3,3", nil},
		check{"eg_truncate_trigger", "enginegaps", "trigger", pre("~DROP TABLE IF EXISTS eg_lg", "CREATE TABLE eg_lg (n INT)", "CREATE TRIGGER eg_tt AFTER TRUNCATE ON eg_t FOR EACH STATEMENT BEGIN INSERT INTO eg_lg VALUES (1); END", "TRUNCATE eg_t", "SELECT count(*)::text FROM eg_lg"), "val:1", nil},
		check{"eg_insert_statement_trigger", "enginegaps", "trigger", pre("~DROP TABLE IF EXISTS eg_lg", "CREATE TABLE eg_lg (n INT)", "CREATE TRIGGER eg_ci AFTER INSERT ON eg_t FOR EACH STATEMENT BEGIN INSERT INTO eg_lg VALUES (1); END", "INSERT INTO eg_t VALUES (5, 'z')", "SELECT count(*)::text FROM eg_lg"), "val:1", nil},
	)
}
