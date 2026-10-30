package main

// Probes for task trigplsql: PL/pgSQL trigger functions (CREATE TRIGGER .. EXECUTE FUNCTION), function and
// index catalogs (docs/14 Appendix W). The expected values are PostgreSQL 16's.

func init() {
	const tbl = "CREATE TABLE cc_t (id BIGINT PRIMARY KEY, a TEXT, n BIGINT)"
	const guardFn = "CREATE OR REPLACE FUNCTION cc_guard() RETURNS trigger AS $$ BEGIN " +
		"IF OLD.n IS NOT NULL AND NEW.n IS NOT NULL AND NEW.n <> OLD.n THEN RAISE EXCEPTION 'link % -> %', OLD.n, NEW.n USING ERRCODE = '23514'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql"
	const guardTrg = "CREATE TRIGGER cc_guard_trg BEFORE UPDATE OF n ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_guard()"
	const setFn = "CREATE OR REPLACE FUNCTION cc_set() RETURNS trigger AS $$ BEGIN " +
		"IF TG_OP = 'INSERT' THEN NEW.a := upper(NEW.a) || ':' || TG_WHEN || ':' || TG_LEVEL || ':' || TG_TABLE_NAME || ':' || coalesce(TG_ARGV[0], '-'); END IF; " +
		"IF NEW.a LIKE 'SKIP:%' THEN RETURN NULL; END IF; RETURN NEW; END $$ LANGUAGE plpgsql"
	extraChecks = append(extraChecks,
		check{"trig_fn_guard_blocks_overwrite", "trigplsql", "05 §18", []string{tbl, guardFn, guardTrg, "INSERT INTO cc_t VALUES (1,'x',10)", "UPDATE cc_t SET n = 11"}, "err:23514", nil},
		check{"trig_fn_guard_keeps_value", "trigplsql", "05 §18", []string{tbl, guardFn, guardTrg, "INSERT INTO cc_t VALUES (1,'x',10)", "~UPDATE cc_t SET n = 11", "SELECT n FROM cc_t"}, "val:10", nil},
		check{"trig_fn_guard_allows_null_transitions", "trigplsql", "05 §18", []string{tbl, guardFn, guardTrg, "INSERT INTO cc_t VALUES (1,'x',NULL)", "UPDATE cc_t SET n = 5", "UPDATE cc_t SET n = 5", "UPDATE cc_t SET n = NULL", "UPDATE cc_t SET n = 6", "SELECT n FROM cc_t"}, "val:6", nil},
		check{"trig_fn_update_of_other_column", "trigplsql", "05 §18", []string{tbl, "CREATE OR REPLACE FUNCTION cc_boom() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$ LANGUAGE plpgsql",
			"CREATE TRIGGER cc_boom_trg BEFORE UPDATE OF n ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_boom()", "INSERT INTO cc_t VALUES (1,'x',1)", "UPDATE cc_t SET a = 'y'", "SELECT a FROM cc_t"}, "val:y", nil},
		check{"trig_fn_before_modify_new_and_vars", "trigplsql", "05 §18", []string{tbl, setFn, "CREATE TRIGGER cc_set_trg BEFORE INSERT ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_set('arg')", "INSERT INTO cc_t VALUES (1,'abc',1)", "SELECT a FROM cc_t"}, "val:ABC:BEFORE:ROW:cc_t:arg", nil},
		check{"trig_fn_before_return_null_skips_row", "trigplsql", "05 §18", []string{tbl, setFn, "CREATE TRIGGER cc_set_trg BEFORE INSERT ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_set()", "INSERT INTO cc_t VALUES (1,'skip',1),(2,'ok',2)", "SELECT count(*) FROM cc_t"}, "val:1", nil},
		check{"trig_fn_rolled_back_with_transaction", "trigplsql", "05 §18", []string{tbl, "CREATE TABLE cc_c (id BIGINT PRIMARY KEY, n BIGINT)",
			"CREATE OR REPLACE FUNCTION cc_copy() RETURNS trigger AS $$ BEGIN INSERT INTO cc_c VALUES (NEW.id, NEW.n); RETURN NEW; END $$ LANGUAGE plpgsql",
			"CREATE TRIGGER cc_copy_trg AFTER INSERT ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_copy()", "BEGIN", "INSERT INTO cc_t VALUES (1,'x',1)", "ROLLBACK", "SELECT count(*) FROM cc_c"}, "val:0", nil},
		check{"trig_fn_failed_statement_undoes_trigger_writes", "trigplsql", "05 §18", []string{tbl, "CREATE TABLE cc_c (id BIGINT PRIMARY KEY, n BIGINT)",
			"CREATE OR REPLACE FUNCTION cc_copy() RETURNS trigger AS $$ BEGIN INSERT INTO cc_c VALUES (NEW.id, NEW.n); IF NEW.n < 0 THEN RAISE EXCEPTION 'neg'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql",
			"CREATE TRIGGER cc_copy_trg AFTER INSERT ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_copy()", "~INSERT INTO cc_t VALUES (1,'x',1),(2,'y',-1)", "SELECT (SELECT count(*) FROM cc_c) + (SELECT count(*) FROM cc_t)"}, "val:0", nil},
		check{"trig_fn_recursion_limit", "trigplsql", "05 §18", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, a TEXT, n BIGINT)", "INSERT INTO cc_t VALUES (1,'x',0)",
			"CREATE OR REPLACE FUNCTION cc_rec() RETURNS trigger AS $$ BEGIN UPDATE cc_t SET n = n + 1; RETURN NEW; END $$ LANGUAGE plpgsql",
			"CREATE TRIGGER cc_rec_trg AFTER UPDATE ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_rec()", "UPDATE cc_t SET n = 100"}, "err:54001", nil},
		check{"trig_fn_statement_level", "trigplsql", "05 §18", []string{tbl, "CREATE TABLE cc_c (id BIGINT PRIMARY KEY, n BIGINT)",
			"CREATE OR REPLACE FUNCTION cc_stm() RETURNS trigger AS $$ BEGIN INSERT INTO cc_c SELECT count(*) + 1, 0 FROM cc_c; RETURN NULL; END $$ LANGUAGE plpgsql",
			"CREATE TRIGGER cc_stm_trg AFTER INSERT ON cc_t FOR EACH STATEMENT EXECUTE FUNCTION cc_stm()", "INSERT INTO cc_t VALUES (1,'x',1),(2,'y',2),(3,'z',3)", "SELECT count(*) FROM cc_c"}, "val:1", nil},
		check{"trig_fn_create_checks_function", "trigplsql", "05 §18", []string{tbl, "CREATE TRIGGER cc_x AFTER INSERT ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_nonexistent_fn()"}, "err:42883", nil},
		check{"trig_fn_loops_and_case", "trigplsql", "05 §18", []string{tbl, "CREATE OR REPLACE FUNCTION cc_lp() RETURNS trigger AS $$ DECLARE s bigint := 0; i int; BEGIN FOR i IN 1..5 LOOP IF i = 3 THEN CONTINUE; END IF; s := s + i; END LOOP; " +
			"CASE WHEN s > 10 THEN NEW.n := s; ELSE NEW.n := -1; END CASE; RETURN NEW; END $$ LANGUAGE plpgsql",
			"CREATE TRIGGER cc_lp_trg BEFORE INSERT ON cc_t FOR EACH ROW EXECUTE FUNCTION cc_lp()", "INSERT INTO cc_t VALUES (1,'x',0)", "SELECT n FROM cc_t"}, "val:12", nil},
		check{"trig_fn_pg_proc_lists_user_functions", "trigplsql", "05 §18", []string{"CREATE OR REPLACE FUNCTION cc_pp(a int) RETURNS text AS $$ SELECT 'x' $$ LANGUAGE sql IMMUTABLE",
			"SELECT (count(*) = 1)::text FROM pg_proc WHERE proname = 'cc_pp' AND pronamespace = 2200 AND prokind = 'f' AND prorettype = 25 AND proargtypes::text = '23' AND prosrc LIKE '%SELECT ''x''%'"}, "val:true", nil},
		check{"trig_fn_pg_get_functiondef", "trigplsql", "05 §18", []string{"CREATE OR REPLACE FUNCTION cc_pp(a int) RETURNS text AS $$ SELECT 'x' $$ LANGUAGE sql IMMUTABLE",
			"SELECT (pg_get_functiondef(oid) = E'CREATE OR REPLACE FUNCTION public.cc_pp(a integer)\\n RETURNS text\\n LANGUAGE sql\\n IMMUTABLE\\nAS $function$ SELECT ''x'' $function$\\n')::text FROM pg_proc WHERE proname = 'cc_pp'"}, "val:true", nil},
		check{"trig_fn_pg_trigger_row", "trigplsql", "05 §18", []string{tbl, guardFn, guardTrg, "SELECT (tgtype = 19 AND tgfoid = (SELECT oid FROM pg_proc WHERE proname = 'cc_guard'))::text FROM pg_trigger WHERE tgname = 'cc_guard_trg'"}, "val:true", nil},
		check{"trig_fn_partial_index_any_form", "trigplsql", "05 §18", []string{"CREATE TABLE cc_t (id BIGINT PRIMARY KEY, st VARCHAR(20), del TIMESTAMPTZ)", "CREATE UNIQUE INDEX cc_t_u ON cc_t (st) WHERE del IS NULL AND st IN ('PENDING', 'IN_PROGRESS')",
			"SELECT indexdef FROM pg_indexes WHERE indexname = 'cc_t_u'"}, "val:CREATE UNIQUE INDEX cc_t_u ON public.cc_t USING btree (st) WHERE ((del IS NULL) AND ((st)::text = ANY ((ARRAY['PENDING'::character varying, 'IN_PROGRESS'::character varying])::text[])))", nil},
	)
}
