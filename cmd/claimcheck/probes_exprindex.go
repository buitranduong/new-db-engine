package main

// Probes for expression indexes (CREATE INDEX ON t (lower(col)), docs/05 §18
// and docs/14 task exprindex). The expected values are PostgreSQL 16's, except
// where noted.

func init() {
	const base = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, v TEXT)"
	extraChecks = append(extraChecks,
		check{"exprindex_create", "exprindex", "05 §18", []string{base, "CREATE INDEX cc_t_lower ON cc_t (lower(v))"}, "ok", nil},
		check{"exprindex_used_by_explain", "exprindex", "05 §18", []string{base, "INSERT INTO cc_t VALUES (1,'A'),(2,'b')", "CREATE INDEX cc_t_lower ON cc_t (lower(v))", "EXPLAIN SELECT id FROM cc_t WHERE lower(v) = 'a'"}, "has:Index Scan using cc_t_lower", nil},
		check{"exprindex_lookup_result", "exprindex", "05 §18", []string{base, "INSERT INTO cc_t VALUES (1,'Alice'),(2,'bob')", "CREATE INDEX cc_t_lower ON cc_t (lower(v))", "INSERT INTO cc_t VALUES (3,'ALICE')", "UPDATE cc_t SET v='BOB' WHERE id=1", "SELECT string_agg(id::text, ',' ORDER BY id) FROM cc_t WHERE lower(v) = 'bob'"}, "val:1,2", nil},
		check{"exprindex_unique_violation", "exprindex", "05 §18", []string{base, "CREATE UNIQUE INDEX cc_t_lu ON cc_t (lower(v))", "INSERT INTO cc_t VALUES (1,'A')", "INSERT INTO cc_t VALUES (2,'a')"}, "err:23505", nil},
		check{"exprindex_unique_build_duplicate", "exprindex", "05 §18", []string{base, "INSERT INTO cc_t VALUES (1,'A'),(2,'a')", "CREATE UNIQUE INDEX cc_t_lu ON cc_t (lower(v))"}, "err:23505", nil},
		check{"exprindex_pg_indexes", "exprindex", "05 §18", []string{base, "CREATE INDEX cc_t_lower ON cc_t (lower(v))", "SELECT indexdef FROM pg_indexes WHERE indexname = 'cc_t_lower'"}, "val:CREATE INDEX cc_t_lower ON public.cc_t USING btree (lower(v))", nil},
		check{"exprindex_unsupported_expr_0A000", "exprindex", "05 §18", []string{base, "CREATE INDEX cc_t_x ON cc_t ((v || 'x'))"}, "err:0A000", nil},
	)
}
