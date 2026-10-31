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
