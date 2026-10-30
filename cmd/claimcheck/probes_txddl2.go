package main

// Probes of transactional DDL, second round (branch txddl2, docs/05 section "DDL trong giao dich"):
// savepoints, DROP+CREATE of one name, views, sequences, constraints, RENAME, ALTER COLUMN, TRUNCATE,
// types. IDs start "tx2_".

func init() {
	pre := func(extra ...string) []string {
		return append([]string{"~ROLLBACK", "~DROP VIEW IF EXISTS tx2_v", "~DROP SEQUENCE IF EXISTS tx2_q", "~DROP TYPE IF EXISTS tx2_mood",
			"~DROP TABLE IF EXISTS tx2_t", "~DROP TABLE IF EXISTS tx2_t2", "~DROP TABLE IF EXISTS tx2_u",
			"CREATE TABLE tx2_u (id INT PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO tx2_u VALUES (1, 'x', 'y')"}, extra...)
	}
	extraChecks = append(extraChecks,
		check{"tx2_savepoint_rollback_create", "tx", "ddl", pre("BEGIN", "CREATE TABLE tx2_t (id INT PRIMARY KEY)", "SAVEPOINT sp", "CREATE TABLE tx2_t2 (id INT PRIMARY KEY)", "ROLLBACK TO sp", "COMMIT", "SELECT count(*)::text FROM tx2_t2"), "err:42P01", nil},
		check{"tx2_savepoint_keeps_before", "tx", "ddl", pre("BEGIN", "CREATE TABLE tx2_t (id INT PRIMARY KEY)", "SAVEPOINT sp", "CREATE TABLE tx2_t2 (id INT PRIMARY KEY)", "ROLLBACK TO sp", "COMMIT", "SELECT count(*)::text FROM tx2_t"), "val:0", nil},
		check{"tx2_savepoint_rollback_alter", "tx", "ddl", pre("BEGIN", "SAVEPOINT sp", "ALTER TABLE tx2_u ADD COLUMN c INT", "CREATE INDEX tx2_u_a ON tx2_u (a)", "ROLLBACK TO sp", "COMMIT", "SELECT c FROM tx2_u"), "err:42703", nil},
		check{"tx2_savepoint_release_keeps", "tx", "ddl", pre("BEGIN", "SAVEPOINT sp", "CREATE TABLE tx2_t (id INT PRIMARY KEY)", "RELEASE sp", "COMMIT", "SELECT count(*)::text FROM tx2_t"), "val:0", nil},
		check{"tx2_savepoint_rollback_drop", "tx", "ddl", pre("BEGIN", "SAVEPOINT sp", "DROP TABLE tx2_u", "ROLLBACK TO sp", "COMMIT", "SELECT a FROM tx2_u"), "val:x", nil},
		check{"tx2_drop_create_same_name", "tx", "ddl", pre("BEGIN", "DROP TABLE tx2_u", "CREATE TABLE tx2_u (id INT PRIMARY KEY, z TEXT)", "INSERT INTO tx2_u VALUES (1, 'new')", "COMMIT", "SELECT z FROM tx2_u"), "val:new", nil},
		check{"tx2_drop_create_rollback", "tx", "ddl", pre("BEGIN", "DROP TABLE tx2_u", "CREATE TABLE tx2_u (id INT PRIMARY KEY, z TEXT)", "ROLLBACK", "SELECT a FROM tx2_u"), "val:x", nil},
		check{"tx2_dropped_is_invisible", "tx", "ddl", pre("BEGIN", "DROP TABLE tx2_u", "SELECT a FROM tx2_u"), "err:42P01", nil},
		check{"tx2_rollback_create_view", "tx", "ddl", pre("BEGIN", "CREATE VIEW tx2_v AS SELECT id FROM tx2_u", "ROLLBACK", "SELECT * FROM tx2_v"), "err:42P01", nil},
		check{"tx2_view_visible_in_tx", "tx", "ddl", pre("BEGIN", "CREATE VIEW tx2_v AS SELECT a FROM tx2_u", "SELECT a FROM tx2_v"), "val:x", nil},
		check{"tx2_rollback_drop_view", "tx", "ddl", pre("CREATE VIEW tx2_v AS SELECT a FROM tx2_u", "BEGIN", "DROP VIEW tx2_v", "ROLLBACK", "SELECT a FROM tx2_v"), "val:x", nil},
		check{"tx2_rollback_create_sequence", "tx", "ddl", pre("BEGIN", "CREATE SEQUENCE tx2_q", "ROLLBACK", "SELECT nextval('tx2_q')::text"), "err:42P01", nil},
		check{"tx2_rollback_check_constraint", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx2_u ADD CONSTRAINT tx2_chk CHECK (id < 100)", "ROLLBACK", "INSERT INTO tx2_u VALUES (200, 'p', 'q')", "SELECT count(*)::text FROM tx2_u"), "val:2", nil},
		check{"tx2_rollback_rename_column", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx2_u RENAME COLUMN a TO aa", "ROLLBACK", "SELECT a FROM tx2_u"), "val:x", nil},
		check{"tx2_rollback_rename_table", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx2_u RENAME TO tx2_t", "ROLLBACK", "SELECT a FROM tx2_u"), "val:x", nil},
		check{"tx2_rollback_set_not_null", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx2_u ALTER COLUMN a SET NOT NULL", "ROLLBACK", "INSERT INTO tx2_u VALUES (2, NULL, 'q')", "SELECT count(*)::text FROM tx2_u"), "val:2", nil},
		check{"tx2_rollback_set_default", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx2_u ALTER COLUMN a SET DEFAULT 'd'", "ROLLBACK", "INSERT INTO tx2_u (id) VALUES (2)", "SELECT a IS NULL FROM tx2_u WHERE id = 2"), "val:t", nil},
		check{"tx2_rollback_truncate", "tx", "ddl", pre("BEGIN", "TRUNCATE tx2_u", "ROLLBACK", "SELECT count(*)::text FROM tx2_u"), "val:1", nil},
		check{"tx2_commit_truncate", "tx", "ddl", pre("BEGIN", "TRUNCATE tx2_u", "COMMIT", "SELECT count(*)::text FROM tx2_u"), "val:0", nil},
		check{"tx2_rollback_create_type", "tx", "ddl", pre("BEGIN", "CREATE TYPE tx2_mood AS ENUM ('a', 'b')", "ROLLBACK", "SELECT 'a'::tx2_mood::text"), "err:42704", nil},
	)
}
