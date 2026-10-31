-- Group i (sqlgaps2): UPDATE SET (cols) = (SELECT ..), composite types and functions returning them,
-- event triggers, DDL rollback. Expected behaviour is PostgreSQL 16 docs: UPDATE (multiple-column
-- assignment from a sub-SELECT), "Composite Types", CREATE EVENT TRIGGER.

-- ### i/update-multi
-- drop: iu1, iu2
CREATE TABLE iu1 (id int PRIMARY KEY, a int, b text, c int);
CREATE TABLE iu2 (id int PRIMARY KEY, x int, y text);
INSERT INTO iu1 VALUES (1,1,$$a$$,10),(2,2,$$b$$,20),(3,3,$$c$$,30);
INSERT INTO iu2 VALUES (1,100,$$p$$),(2,200,$$q$$);
UPDATE iu1 SET (a,b) = (SELECT x,y FROM iu2 WHERE iu2.id=iu1.id);
SELECT id, a, b, c FROM iu1 ORDER BY id;
UPDATE iu1 SET (a,b) = (7,$$z$$) WHERE id=1;
UPDATE iu1 SET (a,b) = ROW(8,$$zz$$) WHERE id=2;
UPDATE iu1 SET (a,b,c) = (SELECT 1,$$k$$,5) WHERE id=3;
UPDATE iu1 SET (a) = (9) WHERE id=3;
UPDATE iu1 SET (a) = ROW(9) WHERE id=3;
UPDATE iu1 SET (a) = (SELECT 11) WHERE id=3;
UPDATE iu1 SET (a,b) = (c, b||$$!$$) WHERE id=1;
SELECT id, a, b, c FROM iu1 ORDER BY id;
UPDATE iu1 SET (a,b) = (SELECT 1) WHERE id=1;
UPDATE iu1 SET (a,b) = (1,2,3) WHERE id=1;
UPDATE iu1 SET (a,b) = (SELECT x,y FROM iu2) WHERE id=1;
UPDATE iu1 SET (a,b) = (SELECT x,y FROM iu2 WHERE false) WHERE id=1;
SELECT id, a, b, c FROM iu1 ORDER BY id;
UPDATE iu1 SET c = 1, (a,b) = (SELECT x,y FROM iu2 WHERE id=2) WHERE id=3 RETURNING id, a, b, c;
UPDATE iu1 SET (a,b) = (SELECT x,y FROM iu2 WHERE id=1), c = (SELECT max(x) FROM iu2) WHERE id=3;
UPDATE iu1 SET (a,b) = (SELECT x,y FROM iu2 WHERE id=1) FROM iu2 WHERE iu1.id=iu2.id AND iu2.id=2;
UPDATE iu1 SET (a,b) = (DEFAULT, DEFAULT) WHERE id=1;
UPDATE iu1 SET (a,b) = (SELECT max(x), min(y) FROM iu2) WHERE id=2;
UPDATE iu1 SET (a,b) = (SELECT i2.x, i2.y FROM iu2 i2 WHERE i2.id = iu1.id + 1) WHERE id=1 RETURNING a, b;
SELECT id, a, b, c FROM iu1 ORDER BY id;
DROP TABLE iu1;
DROP TABLE iu2;

-- ### i/composite-basic
-- drop: ic1
DROP TYPE IF EXISTS ic_pt CASCADE;
CREATE TYPE ic_pt AS (x int, y text);
CREATE TYPE ic_pt AS (x int);
CREATE TYPE ic_bad AS (x int, x text);
CREATE TABLE ic1 (id int PRIMARY KEY, p ic_pt);
INSERT INTO ic1 VALUES (1, ROW(1,$$a$$)), (2, ROW(2,$$b c$$)), (3, NULL);
INSERT INTO ic1 VALUES (4, $$(4,d)$$);
INSERT INTO ic1 VALUES (6, $$(x,d)$$);
SELECT id, p::text, (p).x, (p).y FROM ic1 ORDER BY id;
SELECT id FROM ic1 WHERE (p).x > 1 ORDER BY id;
SELECT ($$(5,x)$$::ic_pt).y;
SELECT ROW(1,$$z$$)::ic_pt::text;
SELECT ($$(5,x,y)$$::ic_pt)::text;
SELECT $$(5)$$::ic_pt;
SELECT $$5,x$$::ic_pt;
SELECT ($$(5,"a,b")$$::ic_pt).y;
SELECT (NULL::ic_pt).x;
SELECT ($$(,x)$$::ic_pt).x IS NULL;
UPDATE ic1 SET p = ROW(9,$$n$$) WHERE id=1;
UPDATE ic1 SET p = $$(9)$$ WHERE id=1;
SELECT id, p::text FROM ic1 ORDER BY id;
DROP TYPE ic_pt;
DROP TABLE ic1;
DROP TYPE ic_pt;
DROP TYPE ic_pt;

-- ### i/composite-functions
-- drop: icf
DROP TYPE IF EXISTS icf_t CASCADE;
CREATE TYPE icf_t AS (n int, s text);
CREATE TABLE icf (id int, nm text);
INSERT INTO icf VALUES (1,$$a$$),(2,$$b$$);
CREATE FUNCTION icf_mk(a int, b text) RETURNS icf_t AS $f$ BEGIN RETURN ROW(a, b); END $f$ LANGUAGE plpgsql;
SELECT icf_mk(3,$$c$$)::text;
SELECT (icf_mk(3,$$c$$)).n, (icf_mk(3,$$c$$)).s;
SELECT * FROM icf_mk(4,$$d$$);
SELECT s FROM icf_mk(5,$$e$$);
CREATE FUNCTION icf_set(n int) RETURNS SETOF icf_t AS $f$ BEGIN FOR i IN 1..n LOOP RETURN NEXT ROW(i, $$r$$||i); END LOOP; END $f$ LANGUAGE plpgsql;
SELECT * FROM icf_set(3);
SELECT n, s FROM icf_set(3) WHERE n > 1 ORDER BY n;
SELECT m.s FROM icf_set(2) m ORDER BY m.n;
SELECT count(*) FROM icf_set(4);
CREATE FUNCTION icf_tbl(n int) RETURNS TABLE (i int, s text) AS $f$ BEGIN RETURN QUERY SELECT g, $$s$$||g FROM generate_series(1,n) g; END $f$ LANGUAGE plpgsql;
SELECT * FROM icf_tbl(2);
SELECT s FROM icf_tbl(3) WHERE i = 2;
CREATE FUNCTION icf_sql(n int) RETURNS icf_t AS $f$ SELECT n, $$q$$::text $f$ LANGUAGE sql;
SELECT * FROM icf_sql(5);
SELECT (icf_sql(6)).s;
CREATE FUNCTION icf_sqls(n int) RETURNS SETOF icf_t AS $f$ SELECT g, $$q$$::text FROM generate_series(1,n) g $f$ LANGUAGE sql;
SELECT * FROM icf_sqls(2);
CREATE FUNCTION icf_row() RETURNS SETOF icf AS $f$ SELECT * FROM icf $f$ LANGUAGE sql;
SELECT id, nm FROM icf_row() ORDER BY id;
CREATE FUNCTION icf_out(OUT a int, OUT b text) AS $f$ SELECT 1, $$o$$::text $f$ LANGUAGE sql;
SELECT * FROM icf_out();
SELECT icf_out()::text;
CREATE FUNCTION icf_arg(r icf_t) RETURNS text AS $f$ SELECT r::text $f$ LANGUAGE sql;
SELECT icf_arg(ROW(1,$$w$$)::icf_t);
CREATE FUNCTION icf_null() RETURNS icf_t AS $f$ BEGIN RETURN NULL; END $f$ LANGUAGE plpgsql;
SELECT icf_null() IS NULL;
DROP FUNCTION icf_mk(int, text);
DROP FUNCTION icf_set(int);
DROP FUNCTION icf_tbl(int);
DROP FUNCTION icf_sql(int);
DROP FUNCTION icf_sqls(int);
DROP FUNCTION icf_row();
DROP FUNCTION icf_out();
DROP FUNCTION icf_arg(icf_t);
DROP FUNCTION icf_null();
DROP TYPE icf_t;
DROP TABLE icf;

-- ### i/evtrig-basic
-- drop: iev_log, iev_t1, iev_t2, iev_t3
DROP EVENT TRIGGER IF EXISTS iev_start;
DROP EVENT TRIGGER IF EXISTS iev_end;
CREATE TABLE iev_log (id serial PRIMARY KEY, ev text, tag text);
CREATE FUNCTION iev_fn() RETURNS event_trigger AS $f$ BEGIN INSERT INTO iev_log (ev, tag) VALUES (TG_EVENT, TG_TAG); END $f$ LANGUAGE plpgsql;
CREATE EVENT TRIGGER iev_start ON ddl_command_start EXECUTE FUNCTION iev_fn();
CREATE EVENT TRIGGER iev_end ON ddl_command_end EXECUTE FUNCTION iev_fn();
CREATE EVENT TRIGGER iev_end ON ddl_command_end EXECUTE FUNCTION iev_fn();
CREATE TABLE iev_t1 (id int PRIMARY KEY, v text);
CREATE INDEX iev_t1_v ON iev_t1 (v);
ALTER TABLE iev_t1 ADD COLUMN w int;
CREATE VIEW iev_v AS SELECT * FROM iev_t1;
CREATE SEQUENCE iev_s;
INSERT INTO iev_t1 VALUES (1, $$a$$);
UPDATE iev_t1 SET v = $$b$$;
TRUNCATE iev_t1;
COMMENT ON TABLE iev_t1 IS $$x$$;
CREATE TABLE iev_t2 AS SELECT 1 AS a;
SELECT 1 AS a INTO iev_t3;
CREATE TYPE iev_ty AS ENUM ($$a$$);
ALTER TYPE iev_ty ADD VALUE $$b$$;
DROP TYPE iev_ty;
CREATE TYPE iev_ct AS (x int, y text);
DROP TYPE iev_ct;
CREATE FUNCTION iev_f() RETURNS int AS $f$ SELECT 1 $f$ LANGUAGE sql;
DROP FUNCTION iev_f();
CREATE SCHEMA iev_sch;
DROP SCHEMA iev_sch;
DROP VIEW iev_v;
DROP SEQUENCE iev_s;
DROP INDEX iev_t1_v;
DROP TABLE iev_t1;
DROP TABLE IF EXISTS iev_nonexistent;
CREATE TABLE IF NOT EXISTS iev_t2 (a int);
SELECT count(*) FROM iev_log;
ALTER TABLE iev_nonexistent ADD COLUMN a int;
SELECT count(*) FROM iev_log;
CREATE TRIGGER iev_tr BEFORE INSERT ON iev_t2 FOR EACH ROW EXECUTE FUNCTION iev_fn();
SELECT ev, tag FROM iev_log ORDER BY id;
DELETE FROM iev_log;
ALTER EVENT TRIGGER iev_start DISABLE;
CREATE TABLE iev_t4 (a int);
SELECT ev, tag FROM iev_log ORDER BY id;
SELECT evtname, evtevent, evtenabled FROM pg_event_trigger ORDER BY evtname;
ALTER EVENT TRIGGER iev_start ENABLE;
ALTER EVENT TRIGGER iev_start RENAME TO iev_start2;
SELECT evtname, evtevent, evtenabled FROM pg_event_trigger ORDER BY evtname;
DROP EVENT TRIGGER iev_start;
DROP EVENT TRIGGER IF EXISTS iev_start;
ALTER EVENT TRIGGER iev_nosuch DISABLE;
DROP EVENT TRIGGER iev_start2;
DROP EVENT TRIGGER iev_end;
DROP TABLE iev_t4;
DROP TABLE iev_t3;
DROP TABLE iev_t2;
DROP FUNCTION iev_fn();
DROP TABLE iev_log;

-- ### i/evtrig-filter-errors
-- drop: iev2_log, iev2_t
DROP EVENT TRIGGER IF EXISTS iev2_f;
CREATE TABLE iev2_log (id serial PRIMARY KEY, ev text, tag text);
CREATE FUNCTION iev2_fn() RETURNS event_trigger AS $f$ BEGIN INSERT INTO iev2_log (ev, tag) VALUES (TG_EVENT, TG_TAG); END $f$ LANGUAGE plpgsql;
CREATE EVENT TRIGGER iev2_f ON ddl_command_end WHEN TAG IN ($$CREATE TABLE$$, $$DROP TABLE$$) EXECUTE FUNCTION iev2_fn();
CREATE TABLE iev2_t (a int);
CREATE VIEW iev2_v AS SELECT 1;
DROP VIEW iev2_v;
DROP TABLE iev2_t;
SELECT ev, tag FROM iev2_log ORDER BY id;
DROP EVENT TRIGGER iev2_f;
CREATE EVENT TRIGGER iev2_bad ON ddl_command_end WHEN TAG IN ($$NOPE$$) EXECUTE FUNCTION iev2_fn();
CREATE EVENT TRIGGER iev2_bad2 ON nosuchevent EXECUTE FUNCTION iev2_fn();
CREATE EVENT TRIGGER iev2_bad3 ON ddl_command_end EXECUTE FUNCTION iev2_nosuchfn();
CREATE FUNCTION iev2_notrig() RETURNS int AS $f$ SELECT 1 $f$ LANGUAGE sql;
CREATE EVENT TRIGGER iev2_bad4 ON ddl_command_end EXECUTE FUNCTION iev2_notrig();
SELECT iev2_fn();
DROP FUNCTION iev2_notrig();
DROP FUNCTION iev2_fn();
DROP TABLE iev2_log;

-- ### i/evtrig-sqldrop
-- drop: iev3_log, iev3_dl, iev3_a, iev3_b
DROP EVENT TRIGGER IF EXISTS iev3_d;
CREATE TABLE iev3_log (id serial PRIMARY KEY, ev text, tag text);
CREATE TABLE iev3_dl (ev text, typ text, nm text);
CREATE FUNCTION iev3_drop() RETURNS event_trigger AS $f$ DECLARE r record; BEGIN FOR r IN SELECT * FROM pg_event_trigger_dropped_objects() LOOP INSERT INTO iev3_dl VALUES (TG_TAG, r.object_type, r.object_name); END LOOP; INSERT INTO iev3_log (ev, tag) VALUES (TG_EVENT, TG_TAG); END $f$ LANGUAGE plpgsql;
CREATE EVENT TRIGGER iev3_d ON sql_drop EXECUTE FUNCTION iev3_drop();
CREATE TABLE iev3_a (a int);
CREATE TABLE iev3_b (a int);
CREATE VIEW iev3_v AS SELECT 1;
CREATE SEQUENCE iev3_s;
CREATE TYPE iev3_ty AS ENUM ($$a$$);
CREATE SCHEMA iev3_sch;
DROP TABLE iev3_a;
DROP TABLE IF EXISTS iev3_nonexistent;
DROP VIEW iev3_v;
DROP SEQUENCE iev3_s;
DROP SCHEMA iev3_sch;
DROP TABLE iev3_b;
SELECT ev, typ, nm FROM iev3_dl WHERE typ IN ($$table$$, $$view$$, $$sequence$$, $$schema$$) ORDER BY nm;
SELECT ev, tag FROM iev3_log ORDER BY id;
SELECT * FROM pg_event_trigger_dropped_objects();
DROP EVENT TRIGGER iev3_d;
DROP TYPE iev3_ty;
DROP FUNCTION iev3_drop();
DROP TABLE iev3_dl;
DROP TABLE iev3_log;

-- ### i/evtrig-abort
-- drop: iev4_log, iev4_t
DROP EVENT TRIGGER IF EXISTS iev4_s;
DROP EVENT TRIGGER IF EXISTS iev4_e;
CREATE TABLE iev4_log (id serial PRIMARY KEY, ev text, tag text);
CREATE FUNCTION iev4_fn() RETURNS event_trigger AS $f$ BEGIN INSERT INTO iev4_log (ev, tag) VALUES (TG_EVENT, TG_TAG); END $f$ LANGUAGE plpgsql;
CREATE FUNCTION iev4_err() RETURNS event_trigger AS $f$ BEGIN RAISE EXCEPTION $$no ddl for you: %$$, TG_TAG; END $f$ LANGUAGE plpgsql;
CREATE EVENT TRIGGER iev4_s ON ddl_command_start EXECUTE FUNCTION iev4_fn();
CREATE EVENT TRIGGER iev4_e ON ddl_command_start WHEN TAG IN ($$CREATE TABLE$$) EXECUTE FUNCTION iev4_err();
CREATE TABLE iev4_t (a int);
SELECT count(*) FROM pg_class WHERE relname = $$iev4_t$$;
SELECT ev, tag FROM iev4_log ORDER BY id;
BEGIN;
CREATE VIEW iev4_v AS SELECT 1;
CREATE TABLE iev4_t2 (a int);
ROLLBACK;
SELECT count(*) FROM pg_class WHERE relname = $$iev4_v$$;
SELECT ev, tag FROM iev4_log ORDER BY id;
BEGIN;
CREATE VIEW iev4_v AS SELECT 1;
COMMIT;
SELECT ev, tag FROM iev4_log ORDER BY id;
DROP VIEW iev4_v;
DROP EVENT TRIGGER iev4_e;
DROP EVENT TRIGGER iev4_s;
DROP FUNCTION iev4_err();
DROP FUNCTION iev4_fn();
DROP TABLE iev4_log;
