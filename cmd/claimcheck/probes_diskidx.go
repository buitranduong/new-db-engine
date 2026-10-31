package main

// Probes for disk-resident index storage (docs/18). The server under test
// runs without -secindex-disk, so these pin the default-off contract; the
// behaviour with the option on is covered by the -race tests of
// internal/core, internal/proj (via internal/sql) and internal/repl.

func init() {
	const base = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, a BIGINT, v TEXT)"
	extraChecks = append(extraChecks,
		check{"diskidx_off_secondary_0A000", "diskidx", "18 §10", []string{base, "CREATE INDEX cc_t_a ON cc_t (a)", "ALTER INDEX cc_t_a SET (index_storage = 'disk')"}, "err:0A000", nil},
		check{"diskidx_off_trigram_0A000", "diskidx", "18 §13", []string{base, "CREATE INDEX cc_t_v ON cc_t USING gin (v gin_trgm_ops)", "ALTER INDEX cc_t_v SET (index_storage = 'disk')"}, "err:0A000", nil},
		check{"diskidx_table_off_0A000", "diskidx", "18 §10", []string{base, "CREATE INDEX cc_t_a ON cc_t (a)", "ALTER TABLE cc_t SET (index_storage = 'disk')"}, "err:0A000", nil},
		check{"diskidx_memory_on_memory_ok", "diskidx", "18 §10", []string{base, "CREATE INDEX cc_t_a ON cc_t (a)", "ALTER INDEX cc_t_a SET (index_storage = 'memory')"}, "ok", nil},
		check{"diskidx_bad_value_22023", "diskidx", "18 §10", []string{base, "CREATE INDEX cc_t_a ON cc_t (a)", "ALTER INDEX cc_t_a SET (index_storage = 'tape')"}, "err:22023", nil},
		check{"diskidx_query_unaffected", "diskidx", "18 §10", []string{base, "INSERT INTO cc_t VALUES (1,5,'x'),(2,5,'y'),(3,6,'z')", "CREATE INDEX cc_t_a ON cc_t (a)", "SELECT count(*) FROM cc_t WHERE a = 5"}, "val:2", nil},
	)
}
