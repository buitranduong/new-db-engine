-- Suite group 04: DML. Syntax source: PostgreSQL 16 docs, INSERT (ON CONFLICT), UPDATE, DELETE,
-- MERGE (new in 15; RETURNING for MERGE is 17 and is not used), COPY, TRUNCATE.

-- ### 04_dml/setup
DROP SCHEMA IF EXISTS s04 CASCADE;
CREATE SCHEMA s04;
SET search_path = s04;
CREATE TABLE t (id int PRIMARY KEY, name text NOT NULL, qty int DEFAULT 0, price numeric(8,2), tags text[], note text);
CREATE TABLE src (id int, name text, qty int);

-- ### 04_dml/insert
SET search_path = s04;
INSERT INTO t (id, name) VALUES (1, 'a');
INSERT INTO t VALUES (2, 'b', 5, 9.99, '{x,y}', 'n');
INSERT INTO t (id, name, qty) VALUES (3, 'c', 1), (4, 'd', 2), (5, 'e', 3);
INSERT INTO t SELECT g, 'g' || g, g * 2, g / 4.0, NULL, NULL FROM generate_series(10, 20) g;
INSERT INTO t (id, name) SELECT g + 100, 'h' || g FROM generate_series(1, 5) g RETURNING id, name;
INSERT INTO t (id, name) VALUES (1, 'dup');
INSERT INTO t (id) VALUES (50);
INSERT INTO t (id, name, nope) VALUES (50, 'x', 1);
INSERT INTO t (id, name) VALUES (50, 'x', 1);
INSERT INTO t (id, name) VALUES (50);
INSERT INTO t VALUES (60, 'toomany', 1, 1, NULL, NULL, 1);
INSERT INTO t (id, name, qty) VALUES (51, 'bad', 'notint');
INSERT INTO t (id, name, price) VALUES (52, 'bad', 123456789);
SELECT count(*) FROM t;
SELECT id, name, qty, price, tags, note FROM t WHERE id IN (1, 2, 3, 10, 11, 101) ORDER BY id;
INSERT INTO src VALUES (1, 'a', 1), (2, 'b', 2), (3, 'c', 3);
INSERT INTO t (id, name, qty) SELECT id + 500, name, qty FROM src RETURNING *;
WITH ins AS (INSERT INTO t (id, name) VALUES (700, 'cte') RETURNING id) SELECT id FROM ins;
INSERT INTO t (id, name) SELECT 800, 'x' WHERE false;
TABLE src;
DELETE FROM src;

-- ### 04_dml/update-delete
SET search_path = s04;
UPDATE t SET qty = qty + 1 WHERE id BETWEEN 1 AND 5;
SELECT id, qty FROM t WHERE id BETWEEN 1 AND 5 ORDER BY id;
UPDATE t SET name = upper(name), qty = qty * 2 WHERE id = 1 RETURNING id, name, qty;
UPDATE t SET (name, qty) = ('multi', 77) WHERE id = 2 RETURNING name, qty;
UPDATE t SET qty = s.qty FROM (VALUES (3, 300), (4, 400)) AS s(id, qty) WHERE t.id = s.id RETURNING t.id, t.qty;
UPDATE t SET qty = (SELECT max(qty) FROM t) WHERE id = 5;
SELECT qty FROM t WHERE id = 5;
UPDATE t SET name = NULL WHERE id = 1;
UPDATE t SET id = 2 WHERE id = 1;
UPDATE t SET nope = 1;
UPDATE t SET qty = DEFAULT WHERE id = 3 RETURNING qty;
UPDATE t SET tags = tags || 'z'::text WHERE id = 2 RETURNING tags;
UPDATE t SET qty = 1 WHERE false;
UPDATE t SET qty = qty;
DELETE FROM t WHERE id > 100 AND id < 200 RETURNING id;
DELETE FROM t USING (VALUES (10), (11)) AS d(id) WHERE t.id = d.id RETURNING t.id;
DELETE FROM t WHERE id IN (SELECT id FROM t WHERE id BETWEEN 12 AND 13);
DELETE FROM t WHERE nope = 1;
SELECT count(*) FROM t;
DELETE FROM t WHERE id = 999;
WITH d AS (DELETE FROM t WHERE id = 14 RETURNING *) SELECT id, name FROM d;
UPDATE ONLY t SET qty = 0 WHERE id = 15;

-- ### 04_dml/upsert
SET search_path = s04;
CREATE TABLE u (id int PRIMARY KEY, name text UNIQUE, hits int DEFAULT 1, upd text);
INSERT INTO u VALUES (1, 'a', 1, 'ins'), (2, 'b', 1, 'ins');
INSERT INTO u VALUES (1, 'a2', 1, 'dup') ON CONFLICT DO NOTHING;
INSERT INTO u VALUES (1, 'a3', 1, 'dup') ON CONFLICT (id) DO NOTHING RETURNING id;
INSERT INTO u VALUES (3, 'c', 1, 'new') ON CONFLICT (id) DO NOTHING RETURNING id;
INSERT INTO u VALUES (1, 'x', 1, 'ups') ON CONFLICT (id) DO UPDATE SET hits = u.hits + 1, upd = EXCLUDED.upd RETURNING *;
INSERT INTO u VALUES (9, 'b', 1, 'by name') ON CONFLICT (name) DO UPDATE SET hits = u.hits + 10 RETURNING id, name, hits;
INSERT INTO u VALUES (9, 'b', 1, 'no target') ON CONFLICT DO UPDATE SET hits = 0;
INSERT INTO u VALUES (9, 'b', 1, 'wrong target') ON CONFLICT (id) DO NOTHING;
INSERT INTO u VALUES (1, 'z', 1, 'where') ON CONFLICT (id) DO UPDATE SET hits = 100 WHERE u.hits > 1000 RETURNING id;
INSERT INTO u VALUES (4, 'd', 1, 'a'), (4, 'dd', 1, 'b') ON CONFLICT (id) DO UPDATE SET upd = EXCLUDED.upd;
INSERT INTO u VALUES (5, 'e', 1, 'a'), (5, 'ee', 1, 'b') ON CONFLICT (id) DO NOTHING;
INSERT INTO u SELECT g, 'n' || g, 1, 'bulk' FROM generate_series(1, 6) g ON CONFLICT (id) DO UPDATE SET hits = u.hits + 1;
SELECT id, name, hits, upd FROM u ORDER BY id;
INSERT INTO u VALUES (1, 'a', 1, 'c') ON CONFLICT ON CONSTRAINT u_pkey DO UPDATE SET hits = 0 RETURNING hits;

-- ### 04_dml/merge
SET search_path = s04;
CREATE TABLE tgt (id int PRIMARY KEY, v text, n int);
CREATE TABLE mrg (id int, v text, n int);
INSERT INTO tgt VALUES (1, 'one', 1), (2, 'two', 2), (3, 'three', 3);
INSERT INTO mrg VALUES (2, 'TWO', 20), (3, 'THREE', 30), (4, 'FOUR', 40);
MERGE INTO tgt USING mrg ON tgt.id = mrg.id WHEN MATCHED THEN UPDATE SET v = mrg.v, n = tgt.n + mrg.n WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (mrg.id, mrg.v, mrg.n);
SELECT * FROM tgt ORDER BY id;
MERGE INTO tgt USING mrg ON tgt.id = mrg.id WHEN MATCHED AND mrg.n > 25 THEN DELETE WHEN MATCHED THEN DO NOTHING;
SELECT * FROM tgt ORDER BY id;
MERGE INTO tgt t USING (SELECT 1 AS id, 'uno' AS v) s ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v WHEN NOT MATCHED BY SOURCE THEN DELETE;
MERGE INTO tgt t USING (SELECT 1 AS id, 'uno' AS v) s ON t.id = s.id WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.v, 0) WHEN MATCHED THEN UPDATE SET v = s.v;
SELECT * FROM tgt ORDER BY id;
MERGE INTO tgt USING (VALUES (1, 'a'), (1, 'b')) AS s(id, v) ON tgt.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v;
MERGE INTO tgt USING nosuch ON true WHEN MATCHED THEN DELETE;
MERGE INTO tgt USING mrg ON tgt.id = mrg.id WHEN NOT MATCHED THEN INSERT VALUES (mrg.id, 'x', 1) WHEN NOT MATCHED THEN DO NOTHING;
SELECT count(*) FROM tgt;

-- ### 04_dml/copy-text-csv
SET search_path = s04;
CREATE TABLE cp (id int, name text, price numeric(8,2), flag boolean, d date, note text);
-- stdin: 1\talpha\t9.50\tt\t2024-01-02\thello\n2\tbeta\t\\N\tf\t2024-01-03\tcomma,in text\n3\t\\N\t0.01\t\\N\t\\N\ttab\\there\\nnewline\n
COPY cp FROM STDIN;
SELECT * FROM cp ORDER BY id;
COPY cp TO STDOUT;
COPY cp (id, name) TO STDOUT;
COPY (SELECT id, name FROM cp WHERE id < 3 ORDER BY id) TO STDOUT;
COPY cp TO STDOUT WITH (FORMAT csv);
COPY cp TO STDOUT WITH (FORMAT csv, HEADER);
COPY cp TO STDOUT WITH (FORMAT csv, DELIMITER ';', NULL 'NULL', QUOTE '"');
COPY cp TO STDOUT WITH (FORMAT csv, FORCE_QUOTE (name, note));
COPY cp TO STDOUT WITH (DELIMITER '|', NULL '');
-- stdin: 10,csv one,1.25,true,2024-02-01,"quoted, with comma"\n11,"csv ""two""",,false,2024-02-02,\n12,,3,,,"multi\nline"\n
COPY cp FROM STDIN WITH (FORMAT csv);
-- stdin: id,name\n20,header row\n
COPY cp (id, name) FROM STDIN WITH (FORMAT csv, HEADER);
SELECT id, name, price, flag, d, note FROM cp ORDER BY id;
-- stdin: 30\tbad\tnotanumber\tt\t2024-01-01\tx\n
COPY cp FROM STDIN;
-- stdin: 31\tshort\n
COPY cp FROM STDIN;
-- stdin: 32\ttoo\t1\tt\t2024-01-01\tx\textra\n
COPY cp FROM STDIN;
SELECT count(*) FROM cp;
COPY nosuch FROM STDIN;
COPY cp (nope) FROM STDIN;
-- stdin: 40\twith default\n
COPY cp (id, name) FROM STDIN;
SELECT price IS NULL, flag IS NULL FROM cp WHERE id = 40;
COPY cp FROM STDIN WITH (FORMAT nosuch);
COPY (SELECT 1 AS a, 'x' AS b UNION ALL SELECT 2, NULL ORDER BY 1) TO STDOUT WITH (FORMAT csv, HEADER);
COPY (SELECT ARRAY[1,2], '{"a":1}'::jsonb, '2024-01-01 10:00:00'::timestamp, 1.50::numeric, true) TO STDOUT;

-- ### 04_dml/copy-binary
SET search_path = s04;
CREATE TABLE cb (id int, name text, price numeric(8,2), d date);
INSERT INTO cb VALUES (1, 'x', 1.25, '2024-01-01'), (2, NULL, NULL, NULL);
COPY cb TO STDOUT WITH (FORMAT binary);
COPY (SELECT 1::int2, 2::int4, 3::int8, 1.5::float8, true, 'txt'::text) TO STDOUT WITH (FORMAT binary);
COPY (SELECT NULL::int) TO STDOUT WITH (FORMAT binary);

-- ### 04_dml/truncate-misc
SET search_path = s04;
CREATE TABLE tr1 (id serial PRIMARY KEY, v int);
CREATE TABLE tr2 (id int PRIMARY KEY, tid int REFERENCES tr1(id));
INSERT INTO tr1 (v) VALUES (1), (2), (3);
INSERT INTO tr2 VALUES (1, 1);
TRUNCATE tr1;
TRUNCATE tr1 CASCADE;
SELECT count(*) FROM tr1;
SELECT count(*) FROM tr2;
INSERT INTO tr1 (v) VALUES (9);
SELECT id FROM tr1;
TRUNCATE TABLE tr1 RESTART IDENTITY CASCADE;
INSERT INTO tr1 (v) VALUES (9);
SELECT id FROM tr1;
TRUNCATE tr1, tr2;
TRUNCATE nosuch;
TRUNCATE ONLY tr1, tr2;
SELECT 1 FROM tr1;
BEGIN;
TRUNCATE tr1, tr2;
INSERT INTO tr1 (v) VALUES (5);
ROLLBACK;
SELECT v FROM tr1;
INSERT INTO tr1 (v) VALUES (1);
INSERT INTO tr1 (v) VALUES (2) RETURNING id, v * 10 AS ten;
SELECT * FROM (VALUES (1, 'a'), (2, 'b')) AS v(n, s) ORDER BY n;
VALUES (1, 'x'), (2, 'y') ORDER BY 1 DESC;
VALUES (1), (2.5), (3);
VALUES (1, 'a'), (2);
TABLE tr1;

-- ### 04_dml/teardown
DROP SCHEMA s04 CASCADE;
