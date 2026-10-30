package main

// Probes of transactional DDL (branch txddl, docs/05 section "DDL trong giao dich"). IDs start "tx_".

func init() {
	pre := func(extra ...string) []string {
		return append([]string{"~ROLLBACK", "~DROP TABLE IF EXISTS tx_t", "~DROP TABLE IF EXISTS tx_u", "CREATE TABLE tx_u (id INT PRIMARY KEY, a TEXT, b TEXT)", "INSERT INTO tx_u VALUES (1, 'x', 'y')"}, extra...)
	}
	extraChecks = append(extraChecks,
		check{"tx_rollback_create_table", "tx", "ddl", pre("BEGIN", "CREATE TABLE tx_t (id INT PRIMARY KEY)", "INSERT INTO tx_t VALUES (1)", "ROLLBACK", "SELECT count(*)::text FROM tx_t"), "err:42P01", nil},
		check{"tx_commit_create_table", "tx", "ddl", pre("BEGIN", "CREATE TABLE tx_t (id INT PRIMARY KEY)", "INSERT INTO tx_t VALUES (1)", "COMMIT", "SELECT count(*)::text FROM tx_t"), "val:1", nil},
		check{"tx_rollback_add_column", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx_u ADD COLUMN c INT", "ROLLBACK", "SELECT c FROM tx_u"), "err:42703", nil},
		check{"tx_rollback_drop_column", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx_u DROP COLUMN b", "ROLLBACK", "SELECT b FROM tx_u"), "val:y", nil},
		check{"tx_commit_drop_column", "tx", "ddl", pre("BEGIN", "ALTER TABLE tx_u DROP COLUMN b", "COMMIT", "SELECT b FROM tx_u"), "err:42703", nil},
		check{"tx_rollback_drop_table", "tx", "ddl", pre("BEGIN", "DROP TABLE tx_u", "ROLLBACK", "SELECT a FROM tx_u"), "val:x", nil},
		check{"tx_commit_drop_table", "tx", "ddl", pre("BEGIN", "DROP TABLE tx_u", "COMMIT", "SELECT a FROM tx_u"), "err:42P01", nil},
		check{"tx_rollback_create_index", "tx", "ddl", pre("BEGIN", "CREATE INDEX tx_u_a ON tx_u (a)", "ROLLBACK", "SELECT count(*)::text FROM pg_indexes WHERE indexname = 'tx_u_a'"), "val:0", nil},
		check{"tx_rollback_drop_index", "tx", "ddl", pre("CREATE INDEX tx_u_a ON tx_u (a)", "BEGIN", "DROP INDEX tx_u_a", "ROLLBACK", "SELECT count(*)::text FROM pg_indexes WHERE indexname = 'tx_u_a'"), "val:1", nil},
	)
}
