package main

// Probes of task jmixfix (docs/14 appendix F): what a Jmix application (Hibernate, Liquibase,
// pgjdbc, SQLAlchemy) needs. The expected value is what PostgreSQL 16 returns.

func init() {
	const u1 = "'00000000-0000-0000-0000-000000000001'"
	const u2 = "'00000000-0000-0000-0000-000000000002'"
	extraChecks = append(extraChecks,
		check{"jmix_index_uuid", "jmixfix", "F.5 #1", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, u UUID)", "CREATE INDEX cc_t_u ON cc_t (u)"}, "ok", nil},
		check{"jmix_unique_uuid_dup", "jmixfix", "F.5 #1", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, u UUID UNIQUE)", "INSERT INTO cc_t VALUES (1, " + u1 + ")", "INSERT INTO cc_t VALUES (2, " + u1 + ")"}, "err:23505", nil},
		check{"jmix_fk_uuid_ok", "jmixfix", "F.5 #1", []string{"CREATE TABLE cc_p (id UUID PRIMARY KEY)", "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, p UUID, CONSTRAINT fk_t_p FOREIGN KEY (p) REFERENCES cc_p (id))", "INSERT INTO cc_p VALUES (" + u1 + ")", "INSERT INTO cc_t VALUES (1, " + u1 + ")"}, "ok", nil},
		check{"jmix_fk_uuid_violation", "jmixfix", "F.5 #1", []string{"CREATE TABLE cc_p (id UUID PRIMARY KEY)", "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, p UUID REFERENCES cc_p (id))", "INSERT INTO cc_t VALUES (1, " + u2 + ")"}, "err:23503", nil},
		check{"jmix_index_numeric_order", "jmixfix", "F.5 #4", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, n NUMERIC(19,2))", "CREATE INDEX cc_t_n ON cc_t (n)", "INSERT INTO cc_t VALUES (1, 10.5), (2, -2.25), (3, 0), (4, 100)", "SELECT string_agg(id::text, ',' ORDER BY n) FROM cc_t WHERE n > -3"}, "val:2,3,1,4", nil},
		check{"jmix_unique_numeric_scale", "jmixfix", "F.5 #4", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, n NUMERIC)", "CREATE UNIQUE INDEX cc_t_n ON cc_t (n)", "INSERT INTO cc_t VALUES (1, 1.5)", "INSERT INTO cc_t VALUES (2, 1.50)"}, "err:23505", nil},
		check{"jmix_index_timestamptz_micro", "jmixfix", "F.5 #1", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, ts TIMESTAMPTZ)", "CREATE INDEX cc_t_ts ON cc_t (ts)", "INSERT INTO cc_t VALUES (1, '2026-01-01 00:00:00.000002+00'), (2, '2026-01-01 00:00:00.000001+00')", "SELECT id FROM cc_t WHERE ts > '2026-01-01 00:00:00.000001+00'"}, "val:1", nil},
		check{"jmix_index_bytea_date", "jmixfix", "F.5 #1", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, b BYTEA, d DATE)", "CREATE INDEX cc_t_b ON cc_t (b)", "CREATE INDEX cc_t_d ON cc_t (d)", "INSERT INTO cc_t VALUES (1, '\\x00', '2026-03-04'), (2, '\\x0000', '2026-03-05')", "SELECT id FROM cc_t WHERE b = '\\x0000' AND d > '2026-03-04'"}, "val:2", nil},
		check{"jmix_three_part_column", "jmixfix", "F.5 #2", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY)", "INSERT INTO cc_t VALUES (7)", "SELECT public.cc_t.id FROM public.cc_t"}, "val:7", nil},
		check{"jmix_pg_class_relname_threepart", "jmixfix", "F.5 #2", []string{T, "SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class WHERE pg_catalog.pg_class.relname = 'cc_t'"}, "val:cc_t", nil},
		check{"jmix_expandarray_composite_pk", "jmixfix", "F.5 #3", []string{"CREATE TABLE cc_t (a INT, b INT, PRIMARY KEY (a, b))", "SELECT count(*) FROM (SELECT (information_schema._pg_expandarray(i.indkey)).n FROM pg_index i WHERE i.indrelid = 'cc_t'::regclass AND i.indisprimary) s"}, "val:2", nil},
		check{"jmix_getTypeInfo_array_in", "jmixfix", "F.5 #3", []string{"SELECT typinput = 'pg_catalog.array_in'::regproc FROM pg_type WHERE typname = '_int4'"}, "val:t", nil},
		check{"jmix_date_tz_suffix", "jmixfix", "F.5 #6", []string{"SELECT '2026-03-04 +00'::date::text"}, "val:2026-03-04", nil},
		check{"jmix_pk_constraint_name", "jmixfix", "F.5 #9", []string{"CREATE TABLE cc_t (id UUID NOT NULL, CONSTRAINT pk_cc_t PRIMARY KEY (id))", "SELECT conname FROM pg_constraint WHERE conrelid = 'cc_t'::regclass"}, "val:pk_cc_t", nil},
		check{"jmix_pk_name_in_error", "jmixfix", "F.5 #9", []string{"CREATE TABLE cc_t (id UUID NOT NULL, CONSTRAINT pk_cc_t PRIMARY KEY (id))", "INSERT INTO cc_t VALUES (" + u1 + ")", "INSERT INTO cc_t VALUES (" + u1 + ")"}, "err:23505", nil},
		check{"jmix_regclass_text_conrelid", "jmixfix", "F.5 #9", []string{T, "SELECT conrelid::regclass::text FROM pg_constraint WHERE conrelid = 'cc_t'::regclass"}, "val:cc_t", nil},
		check{"jmix_column_default_cast", "jmixfix", "F.5 #9", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, s VARCHAR(10) DEFAULT 'NEW')", "SELECT column_default FROM information_schema.columns WHERE table_name = 'cc_t' AND column_name = 's'"}, "val:'NEW'::character varying", nil},
		check{"jmix_oid_column", "jmixfix", "F.5 #8", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, photo OID)", "INSERT INTO cc_t VALUES (1, 4000000000)", "SELECT photo FROM cc_t"}, "val:4000000000", nil},
	)
}
