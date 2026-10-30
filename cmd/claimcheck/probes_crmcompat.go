package main

// Probes for task crmcompat: what a Jmix application's Liquibase changelogs and queries need (docs/14 Appendix P).
// The expected values are PostgreSQL 16's.

func init() {
	const base = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, code TEXT, del TIMESTAMPTZ, st TEXT)"
	extraChecks = append(extraChecks,
		check{"crm_partial_unique_soft_delete", "crmcompat", "05 §18", []string{base, "CREATE UNIQUE INDEX cc_t_u ON cc_t (code) WHERE del IS NULL", "INSERT INTO cc_t VALUES (1,'a',NULL,'x')", "INSERT INTO cc_t VALUES (2,'a',now(),'x')", "INSERT INTO cc_t VALUES (3,'a',now(),'x')", "SELECT count(*) FROM cc_t WHERE code = 'a'"}, "val:3", nil},
		check{"crm_partial_unique_violation", "crmcompat", "05 §18", []string{base, "CREATE UNIQUE INDEX cc_t_u ON cc_t (code) WHERE del IS NULL", "INSERT INTO cc_t VALUES (1,'a',NULL,'x')", "INSERT INTO cc_t VALUES (2,'a',NULL,'x')"}, "err:23505", nil},
		check{"crm_partial_unique_revive_conflict", "crmcompat", "05 §18", []string{base, "CREATE UNIQUE INDEX cc_t_u ON cc_t (code) WHERE del IS NULL", "INSERT INTO cc_t VALUES (1,'a',NULL,'x')", "INSERT INTO cc_t VALUES (2,'a',now(),'x')", "UPDATE cc_t SET del = NULL WHERE id = 2"}, "err:23505", nil},
		check{"crm_partial_unique_in_list", "crmcompat", "05 §18", []string{base, "CREATE UNIQUE INDEX cc_t_u ON cc_t (code) WHERE st IN ('P','Q') AND code IS NOT NULL", "INSERT INTO cc_t VALUES (1,'a',NULL,'P')", "INSERT INTO cc_t VALUES (2,'a',NULL,'Z')", "INSERT INTO cc_t VALUES (3,'a',NULL,'Z')", "SELECT count(*) FROM cc_t"}, "val:3", nil},
		check{"crm_partial_build_over_duplicates", "crmcompat", "05 §18", []string{base, "INSERT INTO cc_t VALUES (1,'a',NULL,'x'),(2,'a',NULL,'x')", "CREATE UNIQUE INDEX cc_t_u ON cc_t (code) WHERE del IS NULL"}, "err:23505", nil},
		check{"crm_drop_create_index_same_tx", "crmcompat", "05 §18", []string{base, "CREATE UNIQUE INDEX cc_t_u ON cc_t (code)", "BEGIN", "DROP INDEX IF EXISTS cc_t_u", "CREATE UNIQUE INDEX cc_t_u ON cc_t (code) WHERE del IS NULL", "COMMIT", "INSERT INTO cc_t VALUES (1,'a',NULL,'x')", "INSERT INTO cc_t VALUES (2,'a',now(),'x')", "SELECT count(*) FROM cc_t"}, "val:2", nil},
		check{"crm_alter_index_rename", "crmcompat", "05 §18", []string{base, "CREATE INDEX cc_t_i ON cc_t (code)", "ALTER INDEX cc_t_i RENAME TO cc_t_j", "SELECT string_agg(indexname, ',' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'cc_t'"}, "val:cc_t_j,cc_t_pkey", nil},
		check{"crm_rename_constraint_fk", "crmcompat", "05 §18", []string{base, "CREATE TABLE cc_c (id BIGINT PRIMARY KEY, r BIGINT, CONSTRAINT cc_fk1 FOREIGN KEY (r) REFERENCES cc_t (id))", "ALTER TABLE cc_c RENAME CONSTRAINT cc_fk1 TO cc_fk2", "SELECT conname FROM pg_constraint WHERE conrelid = 'cc_c'::regclass AND contype = 'f'"}, "val:cc_fk2", nil},
		check{"crm_rename_constraint_pk", "crmcompat", "05 §18", []string{base, "ALTER TABLE cc_t RENAME CONSTRAINT cc_t_pkey TO pk_cc_t", "SELECT conname FROM pg_constraint WHERE conrelid = 'cc_t'::regclass AND contype = 'p'"}, "val:pk_cc_t", nil},
		check{"crm_alter_timestamp_to_timestamptz", "crmcompat", "05 §18", []string{"CREATE TABLE cc_p (id BIGINT PRIMARY KEY, t TIMESTAMP)", "INSERT INTO cc_p VALUES (1, '2026-01-02 03:04:05')", "ALTER TABLE cc_p ALTER COLUMN t TYPE TIMESTAMP WITH TIME ZONE USING (t::TIMESTAMP WITH TIME ZONE)", "SELECT t::text FROM cc_p"}, "val:2026-01-02 03:04:05+00", nil},
		check{"crm_get_keywords", "crmcompat", "05 §18", []string{"SELECT (count(*) > 10)::text FROM pg_catalog.pg_get_keywords() WHERE word <> ALL ('{a,abs}')"}, "val:true", nil},
		check{"crm_unaccent_extension", "crmcompat", "05 §18", []string{"CREATE EXTENSION IF NOT EXISTS unaccent", "SELECT unaccent('Nguyễn Thị Đào')"}, "val:Nguyen Thi Dao", nil},
		check{"crm_sql_function", "crmcompat", "05 §18", []string{"CREATE EXTENSION IF NOT EXISTS unaccent", "CREATE OR REPLACE FUNCTION cc_ul(text) RETURNS text AS $$ SELECT regexp_replace(lower(unaccent($1)), '[^a-z0-9]', '', 'g') $$ LANGUAGE SQL IMMUTABLE", "SELECT cc_ul('Trần Văn Ánh!')"}, "val:tranvananh", nil},
	)
}
