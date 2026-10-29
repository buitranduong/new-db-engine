-- Group f: SQL gaps that simbench exposed (docs/15 section 6.8, branch sqlgaps).
-- Parameters in the select list of INSERT..SELECT, correlated EXISTS / NOT EXISTS
-- / IN / NOT IN with NULLs (hash semi/anti-join), EXPLAIN of every statement kind.

-- ### f/params-select-list
-- drop: fa, ft
CREATE TABLE fa (id BIGINT PRIMARY KEY, k BIGINT, k2 TEXT, v BIGINT);
INSERT INTO fa SELECT i, CASE WHEN i % 7 = 0 THEN NULL ELSE i % 23 END, CASE WHEN i % 11 = 0 THEN NULL ELSE 't' || (i % 9) END, CASE WHEN i % 5 = 0 THEN NULL ELSE i % 8 END FROM generate_series(1, 60) i;
CREATE TABLE ft (a BIGINT, b TEXT, c TEXT, d BIGINT);
-- args: 1001 | web | 5
INSERT INTO ft SELECT $1, k2, $2, v FROM fa WHERE id = $3;
-- args: 1002 | web | 7
INSERT INTO ft (a, b, c, d) SELECT $1, k2, $2, v FROM fa WHERE id >= $3 AND id < 10;
-- args: 2000 | k | 20
INSERT INTO ft SELECT $1, upper($2), CASE WHEN v > 3 THEN 'big' ELSE $2 END, coalesce(v, $3) FROM fa WHERE id <= 12;
SELECT a, b, c, d FROM ft ORDER BY a, b, c, d;
-- args: 5 | z | 8
SELECT $1::bigint + id, $2::text || k2, CASE WHEN id > $3::bigint THEN 1 ELSE 0 END FROM fa WHERE id < 12 ORDER BY id;
-- args: 100 | 3
UPDATE ft SET d = s.p FROM (SELECT id, v + $1::bigint AS p FROM fa WHERE id < $2::bigint) s WHERE ft.a = 2000 AND ft.d = s.id;
-- args: 2000
DELETE FROM ft USING (SELECT $1::bigint AS x) q WHERE ft.a = q.x;
SELECT count(*) FROM ft;

-- ### f/semijoin
-- drop: fa, fb, fe
CREATE TABLE fa (id BIGINT PRIMARY KEY, k BIGINT, k2 TEXT, v BIGINT, f DOUBLE PRECISION);
CREATE TABLE fb (id BIGINT PRIMARY KEY, k BIGINT, k2 TEXT, v BIGINT, f DOUBLE PRECISION, flag BOOLEAN);
CREATE TABLE fe (id BIGINT PRIMARY KEY, k BIGINT, v BIGINT);
INSERT INTO fa SELECT i, CASE WHEN i % 7 = 0 THEN NULL ELSE i % 23 END, CASE WHEN i % 11 = 0 THEN NULL ELSE 't' || (i % 9) END, CASE WHEN i % 5 = 0 THEN NULL ELSE i % 8 END, CASE WHEN i % 6 = 0 THEN NULL ELSE (i % 17)::float8 END FROM generate_series(1, 120) i;
INSERT INTO fb SELECT i, CASE WHEN i % 9 = 0 THEN NULL ELSE i % 19 END, CASE WHEN i % 13 = 0 THEN NULL ELSE 't' || (i % 7) END, CASE WHEN i % 4 = 0 THEN NULL ELSE i % 6 END, CASE WHEN i % 8 = 0 THEN NULL ELSE (i % 13)::float8 END, CASE WHEN i % 10 = 0 THEN NULL ELSE i % 2 = 0 END FROM generate_series(1, 150) i;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE NOT EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE NOT EXISTS (SELECT 1 FROM fb WHERE fa.k = fb.k AND fb.flag) ORDER BY id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb x WHERE x.k = fa.k AND x.k2 = fa.k2 AND x.v > 3) ORDER BY id;
SELECT id FROM fa WHERE NOT EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k AND fb.k2 = fa.k2) ORDER BY id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.k + 1 = fa.k * 1) ORDER BY id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.f = fa.k) ORDER BY id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.f = fa.f) ORDER BY id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k AND fb.v < fa.v) ORDER BY id;
SELECT id FROM fa WHERE fa.v IN (SELECT fb.v FROM fb WHERE fb.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE fa.v NOT IN (SELECT fb.v FROM fb WHERE fb.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE fa.v NOT IN (SELECT fb.v FROM fb WHERE fb.k = fa.k AND fb.k2 = fa.k2) ORDER BY id;
SELECT id FROM fa WHERE NOT (fa.v IN (SELECT fb.v FROM fb WHERE fb.k = fa.k)) ORDER BY id;
SELECT id FROM fa WHERE fa.v NOT IN (SELECT v FROM fb) ORDER BY id;
SELECT id FROM fa WHERE fa.v IN (SELECT v FROM fb) ORDER BY id;
SELECT id FROM fa WHERE fa.v NOT IN (SELECT fb.v FROM fb WHERE fb.v IS NOT NULL AND fb.k = fa.k) ORDER BY id;
SELECT id, EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k) AS e, fa.v IN (SELECT fb.v FROM fb WHERE fb.k = fa.k) AS i, fa.v NOT IN (SELECT fb.v FROM fb WHERE fb.k = fa.k) AS ni FROM fa ORDER BY id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fe WHERE fe.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE NOT EXISTS (SELECT 1 FROM fe WHERE fe.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE fa.v NOT IN (SELECT fe.v FROM fe WHERE fe.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE fa.v IN (SELECT fe.v FROM fe WHERE fe.k = fa.k) ORDER BY id;
SELECT id FROM fa WHERE fa.v NOT IN (SELECT v FROM fe) ORDER BY id;
SELECT k, count(*) FROM fa WHERE NOT EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k) GROUP BY k ORDER BY k;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k) AND NOT EXISTS (SELECT 1 FROM fb WHERE fb.k2 = fa.k2 AND fb.v = 3) ORDER BY id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k LIMIT 1) ORDER BY id;
SELECT id FROM fa WHERE fa.k2 = 'x' OR NOT EXISTS (SELECT 1 FROM fb WHERE fb.k2 = fa.k2) ORDER BY id;
SELECT fa.id FROM fa JOIN fb ON fb.k = fa.k WHERE NOT EXISTS (SELECT 1 FROM fb b2 WHERE b2.k = fa.k AND b2.id <> fb.id) ORDER BY fa.id;
SELECT id FROM fa WHERE EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k AND EXISTS (SELECT 1 FROM fb b3 WHERE b3.k = fb.k AND b3.v = fa.v)) ORDER BY id;
INSERT INTO fe SELECT id, k, v FROM fa WHERE NOT EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k);
SELECT count(*) FROM fe;
UPDATE fa SET v = -1 FROM fb WHERE fa.k = fb.k AND NOT EXISTS (SELECT 1 FROM fe WHERE fe.k = fa.k);
SELECT id, v FROM fa WHERE v = -1 ORDER BY id;
UPDATE fa SET v = -2 WHERE NOT EXISTS (SELECT 1 FROM fb WHERE fb.k = fa.k AND fb.flag);
SELECT count(*) FROM fa WHERE v = -2;
DELETE FROM fb WHERE NOT EXISTS (SELECT 1 FROM fa WHERE fa.k = fb.k);
SELECT count(*) FROM fb;

-- ### f/explain
-- drop: fx, fy
CREATE TABLE fx (id BIGINT PRIMARY KEY, k TEXT, v BIGINT);
CREATE INDEX fx_k ON fx (k);
CREATE TABLE fy (id BIGINT PRIMARY KEY, k TEXT);
INSERT INTO fx SELECT i, 'k' || (i % 10), i FROM generate_series(1, 100) i;
INSERT INTO fy SELECT i, 'k' || (i % 10) FROM generate_series(1, 20) i;
-- note: plan text differs by design (no cost model); the statement must be accepted and return one text column
EXPLAIN SELECT * FROM fx WHERE id = 5;
-- note: plan text differs by design
EXPLAIN SELECT * FROM fx WHERE k = 'k3';
-- note: plan text differs by design
EXPLAIN INSERT INTO fx VALUES (1000, 'z', 1);
-- note: plan text differs by design
EXPLAIN UPDATE fx SET v = 1 WHERE k = 'k3';
-- note: plan text differs by design
EXPLAIN DELETE FROM fx WHERE id = 5;
-- note: plan text differs by design
EXPLAIN WITH c AS (SELECT k FROM fy) SELECT * FROM fx WHERE k IN (SELECT k FROM c);
-- note: plan text differs by design
EXPLAIN UPDATE fx SET v = 0 WHERE NOT EXISTS (SELECT 1 FROM fy WHERE fy.k = fx.k);
SELECT count(*) FROM fx;
-- note: EXPLAIN ANALYZE of a write runs it; row counts then agree
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) DELETE FROM fy WHERE id > 15;
SELECT count(*) FROM fy;
