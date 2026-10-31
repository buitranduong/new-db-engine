-- Group h (trigplsql): catalog fidelity for functions, triggers and partial-index predicates (docs/14 appendix W).

-- ### h/tc-proc
-- drop: h_tc_t
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE TABLE h_tc_t (id bigint PRIMARY KEY, v text);
CREATE FUNCTION h_tc_uu(varchar) RETURNS text AS $$ select 1::text $$ LANGUAGE sql IMMUTABLE;
CREATE FUNCTION h_tc_pp(a int, b text) RETURNS void AS $$ begin null; end $$ LANGUAGE plpgsql;
CREATE PROCEDURE h_tc_pr(x bigint) LANGUAGE sql AS $$ select 1 $$;
CREATE FUNCTION h_tc_tr() RETURNS trigger AS $$
BEGIN
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER h_tc_trg BEFORE UPDATE OF v ON h_tc_t FOR EACH ROW EXECUTE FUNCTION h_tc_tr();
CREATE TRIGGER h_tc_trg2 AFTER INSERT OR DELETE ON h_tc_t FOR EACH ROW WHEN (NEW.id > 0 OR OLD.id > 0) EXECUTE PROCEDURE h_tc_tr();
CREATE TRIGGER h_tc_trg3 AFTER INSERT ON h_tc_t FOR EACH STATEMENT EXECUTE FUNCTION h_tc_tr();
-- ordered
SELECT proname, pronamespace, prokind, prosecdef, proisstrict, proretset, provolatile, proparallel, pronargs, prorettype, proargtypes, prosrc
FROM pg_proc WHERE proname LIKE 'h\_tc%' OR proname LIKE 'unaccent%' ORDER BY proname, proargtypes;
-- ordered
SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname LIKE 'h\_tc%' ORDER BY proname;
-- ordered
SELECT proname, pg_get_function_arguments(oid), pg_get_function_result(oid) FROM pg_proc WHERE proname LIKE 'h\_tc%' ORDER BY proname;
-- ordered
SELECT count(*) FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND proname LIKE 'h\_tc%';
-- ordered
SELECT tgname, tgtype, tgnargs, tgfoid = (SELECT oid FROM pg_proc WHERE proname = 'h_tc_tr') AS fn, tgattr::text FROM pg_trigger WHERE tgname LIKE 'h\_tc%' ORDER BY tgname;
-- ordered
SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname LIKE 'h\_tc%' ORDER BY tgname;
DROP TRIGGER h_tc_trg ON h_tc_t;
DROP FUNCTION h_tc_tr();
DROP FUNCTION h_tc_tr() CASCADE;
DROP FUNCTION h_tc_uu(varchar);
DROP FUNCTION h_tc_pp(int, text);
DROP PROCEDURE h_tc_pr(bigint);
SELECT count(*) FROM pg_proc WHERE proname LIKE 'h\_tc%';

-- ### h/tc-partial-index
-- drop: h_tc_p
CREATE TABLE h_tc_p (id bigint PRIMARY KEY, status varchar(20), kind text, n int, big bigint, active boolean, deleted_at timestamptz);
CREATE UNIQUE INDEX h_tc_p1 ON h_tc_p (kind) WHERE deleted_at IS NULL AND status IN ('PENDING', 'IN_PROGRESS');
CREATE INDEX h_tc_p2 ON h_tc_p (kind) WHERE kind IN ('a', 'b', 'c');
CREATE INDEX h_tc_p3 ON h_tc_p (n) WHERE n IN (1, 2, 3);
CREATE INDEX h_tc_p4 ON h_tc_p (big) WHERE big IN (1, 2);
CREATE INDEX h_tc_p5 ON h_tc_p (kind) WHERE status = 'X';
CREATE INDEX h_tc_p6 ON h_tc_p (kind) WHERE kind <> 'X';
CREATE INDEX h_tc_p7 ON h_tc_p (id) WHERE status NOT IN ('A', 'B');
CREATE INDEX h_tc_p8 ON h_tc_p (kind) WHERE status IN ('ONLY');
CREATE INDEX h_tc_p9 ON h_tc_p (kind) WHERE active AND n > 0;
CREATE INDEX h_tc_p10 ON h_tc_p (kind) WHERE kind = 'it''s';
CREATE INDEX h_tc_p11 ON h_tc_p (kind) WHERE deleted_at IS NULL;
CREATE INDEX h_tc_p12 ON h_tc_p (kind) WHERE NOT active;
CREATE INDEX h_tc_p13 ON h_tc_p (kind) WHERE n NOT IN (4, 5);
-- ordered
SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'h_tc_p' ORDER BY indexname;
-- ordered
SELECT c.relname, pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = 'h_tc_p'::regclass ORDER BY c.relname;
-- ordered
SELECT c.relname, pg_get_expr(i.indpred, i.indrelid) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = 'h_tc_p'::regclass AND i.indpred IS NOT NULL ORDER BY c.relname;
