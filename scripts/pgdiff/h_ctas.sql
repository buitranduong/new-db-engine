-- Group h: the basic table/data commands (branch ctas): CREATE TABLE AS, SELECT INTO, LIKE, TEMP,
-- materialized views, MERGE, TRUNCATE, INSERT forms. Each ### block is one session; sessions that
-- follow check that what is private to a session is gone.

-- ### h/ctas-types
-- drop: h_src, h_c1, h_c2, h_c3, h_c4
CREATE TABLE h_src (id INTEGER PRIMARY KEY, name VARCHAR(20), price NUMERIC(12,2), d DATE, small SMALLINT, big BIGINT, r REAL, ts TIMESTAMPTZ, flag BOOLEAN, note TEXT);
INSERT INTO h_src VALUES (1, 'alpha', 10.50, '2026-01-02', 3, 9000000000, 1.5, '2026-01-02 03:04:05+00', true, 'n1'), (2, 'beta', 20.00, '2026-02-03', 4, 8, 2.5, '2026-02-03 00:00:00+00', false, NULL), (3, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
CREATE TABLE h_c1 AS SELECT * FROM h_src;
SELECT * FROM h_c1 ORDER BY id;
SELECT attname::text, atttypid::int, atttypmod, attnotnull FROM pg_attribute WHERE attrelid = 'h_c1'::regclass AND attnum > 0 ORDER BY attnum;
CREATE TABLE h_c2 AS SELECT id, id * 2 AS dbl, id::bigint AS b, id::smallint AS s, price * 2 AS p2, 'x' AS lit, upper(name) AS up, now() IS NOT NULL AS ok, d + 1 AS d1 FROM h_src;
SELECT * FROM h_c2 ORDER BY id;
SELECT attname::text, atttypid::int, atttypmod FROM pg_attribute WHERE attrelid = 'h_c2'::regclass AND attnum > 0 ORDER BY attnum;
CREATE TABLE h_c3 (a, b) AS SELECT id, name FROM h_src WHERE id < 3;
SELECT a, b FROM h_c3 ORDER BY a;
CREATE TABLE h_c4 AS SELECT id FROM h_src WITH NO DATA;
SELECT count(*) FROM h_c4;
INSERT INTO h_c4 VALUES (7);
SELECT * FROM h_c4;
CREATE TABLE h_c4 AS SELECT 1;
CREATE TABLE IF NOT EXISTS h_c4 AS SELECT 1;
CREATE TABLE h_c5 AS SELECT 1 AS x, 2 AS x;
-- the new table has no primary key and nullable columns
INSERT INTO h_c1 (id) VALUES (1);
SELECT count(*) FROM h_c1 WHERE id = 1;
CREATE TABLE h_c6 AS VALUES (1, 'a'), (2, 'b');
SELECT * FROM h_c6 ORDER BY 1;
CREATE TABLE h_c7 AS SELECT carrier, count(*) AS n FROM (VALUES ('v'), ('v'), ('m')) t(carrier) GROUP BY carrier;
SELECT * FROM h_c7 ORDER BY carrier;

-- ### h/select-into
-- drop: h_si_src, h_si1, h_si2
CREATE TABLE h_si_src (id INTEGER PRIMARY KEY, v TEXT);
INSERT INTO h_si_src VALUES (1, 'a'), (2, 'b'), (3, 'c');
SELECT id, v INTO h_si1 FROM h_si_src WHERE id < 3;
SELECT * FROM h_si1 ORDER BY id;
SELECT count(*) AS n, max(id) AS m INTO h_si2 FROM h_si_src;
SELECT * FROM h_si2;
SELECT * INTO h_si1 FROM h_si_src;

-- ### h/like
-- drop: h_like_src, h_l1, h_l2, h_l3, h_l4
CREATE TABLE h_like_src (id SERIAL PRIMARY KEY, code TEXT NOT NULL DEFAULT 'zz', n INTEGER CHECK (n > 0), u TEXT UNIQUE, price NUMERIC(10,3) DEFAULT 1.5);
CREATE INDEX h_like_src_n ON h_like_src (n);
CREATE TABLE h_l1 (LIKE h_like_src);
INSERT INTO h_l1 (id, code, n, u) VALUES (1, 'c', -5, 'a'), (1, 'c', -5, 'a');
SELECT id, code, n, u, price FROM h_l1 ORDER BY id;
INSERT INTO h_l1 (id) VALUES (2);
CREATE TABLE h_l2 (LIKE h_like_src INCLUDING ALL, extra INTEGER);
INSERT INTO h_l2 (n, u) VALUES (1, 'a');
SELECT id, code, n, u, price, extra FROM h_l2;
INSERT INTO h_l2 (n, u) VALUES (-1, 'b');
INSERT INTO h_l2 (n, u) VALUES (2, 'a');
INSERT INTO h_l2 (id, n, u) VALUES (1, 2, 'c');
CREATE TABLE h_l3 (LIKE h_like_src INCLUDING DEFAULTS);
INSERT INTO h_l3 (n) VALUES (5);
SELECT code, price FROM h_l3;
SELECT attname::text, atttypid::int, atttypmod, attnotnull FROM pg_attribute WHERE attrelid = 'h_l2'::regclass AND attnum > 0 ORDER BY attnum;
CREATE TABLE h_l4 (extra TEXT, LIKE h_like_src INCLUDING CONSTRAINTS);
INSERT INTO h_l4 (extra, code, n) VALUES ('e', 'c', 0);
SELECT attname::text FROM pg_attribute WHERE attrelid = 'h_l4'::regclass AND attnum > 0 ORDER BY attnum;
CREATE TABLE h_l5 (LIKE h_nosuch);

-- ### h/temp-create
CREATE TABLE h_t_pub (id INTEGER PRIMARY KEY, v TEXT);
INSERT INTO h_t_pub VALUES (1, 'public');
CREATE TEMP TABLE h_t_pub (id INTEGER PRIMARY KEY, v TEXT);
INSERT INTO h_t_pub VALUES (1, 'temp'), (2, 'temp2');
SELECT id, v FROM h_t_pub ORDER BY id;
CREATE TEMP TABLE h_t_only AS SELECT 7 AS x;
SELECT x FROM h_t_only;
SELECT relname::text, relpersistence::text, relkind::text FROM pg_class WHERE relname IN ('h_t_only', 'h_t_pub') ORDER BY relname, relpersistence;
SELECT count(*) FROM h_t_pub JOIN h_t_only ON h_t_only.x = h_t_pub.id + 5;
CREATE TEMPORARY TABLE h_t_only (a INTEGER);
CREATE TEMP TABLE IF NOT EXISTS h_t_only (a INTEGER);
DROP TABLE h_t_pub;
SELECT id, v FROM h_t_pub;
CREATE UNLOGGED TABLE h_t_unl (a INTEGER PRIMARY KEY);
INSERT INTO h_t_unl VALUES (1);
SELECT a FROM h_t_unl;
SELECT relpersistence::text FROM pg_class WHERE relname = 'h_t_unl';
DROP TABLE h_t_unl;
DROP TABLE h_t_pub;

-- ### h/temp-gone
-- another connection: nothing of the previous session is left
SELECT x FROM h_t_only;
SELECT count(*) FROM pg_class WHERE relname = 'h_t_only';
SELECT id, v FROM h_t_pub;
DROP TABLE h_t_pub;

-- ### h/temp-oncommit
BEGIN;
CREATE TEMP TABLE h_oc1 (a INTEGER) ON COMMIT DROP;
CREATE TEMP TABLE h_oc2 (a INTEGER) ON COMMIT DELETE ROWS;
CREATE TEMP TABLE h_oc3 (a INTEGER) ON COMMIT PRESERVE ROWS;
INSERT INTO h_oc1 VALUES (1);
INSERT INTO h_oc2 VALUES (2);
INSERT INTO h_oc3 VALUES (3);
SELECT a FROM h_oc2;
COMMIT;
SELECT a FROM h_oc1;
SELECT count(*) FROM h_oc2;
SELECT a FROM h_oc3;
BEGIN;
CREATE TEMP TABLE h_oc4 (a INTEGER);
ROLLBACK;
SELECT * FROM h_oc4;
CREATE TEMP TABLE h_oc5 (a INTEGER) ON COMMIT DROP;
SELECT * FROM h_oc5;

-- ### h/matview
-- drop: h_sales
CREATE TABLE h_sales (id INTEGER PRIMARY KEY, carrier TEXT, amt NUMERIC(12,2));
INSERT INTO h_sales VALUES (1, 'v', 10.00), (2, 'v', 5.50), (3, 'm', 1.00);
CREATE MATERIALIZED VIEW h_mv AS SELECT carrier, sum(amt) AS total, count(*) AS n FROM h_sales GROUP BY carrier;
SELECT carrier, total, n FROM h_mv ORDER BY carrier;
INSERT INTO h_sales VALUES (4, 'm', 9.00);
SELECT carrier, total FROM h_mv ORDER BY carrier;
REFRESH MATERIALIZED VIEW h_mv;
SELECT carrier, total FROM h_mv ORDER BY carrier;
SELECT relname::text, relkind::text FROM pg_class WHERE relname = 'h_mv';
SELECT matviewname::text, ispopulated, hasindexes FROM pg_matviews WHERE matviewname = 'h_mv';
SELECT count(*) FROM pg_tables WHERE tablename = 'h_mv';
SELECT attname::text, atttypid::int, atttypmod FROM pg_attribute WHERE attrelid = 'h_mv'::regclass AND attnum > 0 ORDER BY attnum;
INSERT INTO h_mv VALUES ('x', 1, 1);
UPDATE h_mv SET n = 0;
DELETE FROM h_mv;
REFRESH MATERIALIZED VIEW CONCURRENTLY h_mv;
CREATE UNIQUE INDEX h_mv_carrier ON h_mv (carrier);
DELETE FROM h_sales WHERE id = 3;
REFRESH MATERIALIZED VIEW CONCURRENTLY h_mv;
SELECT carrier, total FROM h_mv ORDER BY carrier;
CREATE MATERIALIZED VIEW h_mv2 AS SELECT id FROM h_sales WITH NO DATA;
SELECT * FROM h_mv2;
SELECT ispopulated FROM pg_matviews WHERE matviewname = 'h_mv2';
REFRESH MATERIALIZED VIEW h_mv2;
SELECT count(*) FROM h_mv2;
REFRESH MATERIALIZED VIEW h_mv2 WITH NO DATA;
SELECT * FROM h_mv2;
CREATE MATERIALIZED VIEW h_mv2 AS SELECT 1;
CREATE MATERIALIZED VIEW IF NOT EXISTS h_mv2 AS SELECT 1;
REFRESH MATERIALIZED VIEW h_sales;
DROP MATERIALIZED VIEW h_sales;
DROP TABLE h_mv2;
DROP MATERIALIZED VIEW h_mv2;
DROP MATERIALIZED VIEW h_mv2;
DROP MATERIALIZED VIEW IF EXISTS h_mv2;
DROP MATERIALIZED VIEW h_mv;
SELECT * FROM h_mv;

-- ### h/merge
-- drop: h_tgt, h_stg, h_np
CREATE TABLE h_tgt (number TEXT PRIMARY KEY, status TEXT, price NUMERIC(12,2) DEFAULT 1);
CREATE TABLE h_stg (number TEXT, status TEXT, price NUMERIC(12,2), del BOOLEAN);
INSERT INTO h_tgt VALUES ('1', 'old', 10), ('2', 'old', 20), ('3', 'old', 30), ('4', 'keep', 40);
INSERT INTO h_stg VALUES ('1', 'new', 11, false), ('2', NULL, 0, true), ('5', 'fresh', 55, false), ('3', 'same', 33, false);
MERGE INTO h_tgt t USING h_stg s ON t.number = s.number WHEN MATCHED AND s.del THEN DELETE WHEN MATCHED AND s.status = 'same' THEN DO NOTHING WHEN MATCHED THEN UPDATE SET status = s.status, price = s.price + t.price WHEN NOT MATCHED THEN INSERT (number, status, price) VALUES (s.number, s.status, s.price);
SELECT number, status, price FROM h_tgt ORDER BY number;
INSERT INTO h_stg VALUES ('1', 'dup', 1, false);
MERGE INTO h_tgt USING h_stg ON h_tgt.number = h_stg.number WHEN MATCHED THEN UPDATE SET status = h_stg.status;
SELECT status FROM h_tgt WHERE number = '1';
MERGE INTO h_tgt USING (SELECT '9' AS n, 'x' AS st) q ON h_tgt.number = q.n WHEN NOT MATCHED THEN INSERT (number, status, price) VALUES (q.n, q.st, DEFAULT);
SELECT price FROM h_tgt WHERE number = '9';
-- args: 9
MERGE INTO h_tgt USING (SELECT $1::text AS n) q ON h_tgt.number = q.n WHEN MATCHED THEN DELETE;
MERGE INTO h_tgt t USING h_stg s ON t.number = s.number WHEN MATCHED AND t.status = 'zzz' THEN DELETE;
MERGE INTO h_tgt t USING h_stg s ON t.number = s.number WHEN NOT MATCHED THEN INSERT VALUES (s.number, s.status, s.price) WHEN MATCHED THEN DO NOTHING;
SELECT count(*) FROM h_tgt;
BEGIN;
MERGE INTO h_tgt t USING h_stg s ON t.number = s.number AND s.status = 'fresh' WHEN MATCHED THEN DELETE;
SELECT count(*) FROM h_tgt WHERE number = '5';
ROLLBACK;
SELECT count(*) FROM h_tgt WHERE number = '5';
CREATE TABLE h_np (a INTEGER, b TEXT);
INSERT INTO h_np VALUES (1, 'a'), (2, 'b');
MERGE INTO h_np USING (VALUES (2, 'B'), (3, 'c')) v(a, b) ON h_np.a = v.a WHEN MATCHED THEN UPDATE SET b = v.b WHEN NOT MATCHED THEN INSERT VALUES (v.a, v.b);
SELECT a, b FROM h_np ORDER BY a;
MERGE INTO h_np USING h_nosuch ON true WHEN MATCHED THEN DELETE;
MERGE INTO h_np USING h_stg ON true WHEN MATCHED THEN UPDATE SET nosuchcol = 1;
MERGE INTO h_np USING h_stg ON true;

-- ### h/truncate
-- drop: h_tp, h_tc, h_tg
CREATE TABLE h_tp (id INTEGER PRIMARY KEY);
CREATE TABLE h_tc (id SERIAL PRIMARY KEY, pid INTEGER REFERENCES h_tp(id));
CREATE TABLE h_tg (id SERIAL PRIMARY KEY, cid INTEGER REFERENCES h_tc(id));
INSERT INTO h_tp VALUES (1);
INSERT INTO h_tc (pid) VALUES (1), (1);
INSERT INTO h_tg (cid) VALUES (1);
TRUNCATE h_tp;
TRUNCATE h_tp RESTRICT;
TRUNCATE h_tp, h_tc, h_tg;
SELECT (SELECT count(*) FROM h_tp) AS p, (SELECT count(*) FROM h_tc) AS c, (SELECT count(*) FROM h_tg) AS g;
INSERT INTO h_tp VALUES (1);
INSERT INTO h_tc (pid) VALUES (1);
SELECT id FROM h_tc;
INSERT INTO h_tg (cid) VALUES (3);
TRUNCATE h_tp CASCADE;
SELECT (SELECT count(*) FROM h_tp) AS p, (SELECT count(*) FROM h_tc) AS c, (SELECT count(*) FROM h_tg) AS g;
INSERT INTO h_tp VALUES (1);
INSERT INTO h_tc (pid) VALUES (1);
SELECT id FROM h_tc;
TRUNCATE h_tc, h_tg CONTINUE IDENTITY;
INSERT INTO h_tc (pid) VALUES (1);
SELECT id FROM h_tc;
TRUNCATE TABLE h_tc, h_tg RESTART IDENTITY;
INSERT INTO h_tc (pid) VALUES (1);
SELECT id FROM h_tc;
TRUNCATE h_nosuch;
TRUNCATE ONLY h_tg;
TRUNCATE h_tg, h_tg;

-- ### h/insert-forms
-- drop: h_ia, h_ib, h_dv
CREATE TABLE h_ia (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v TEXT DEFAULT 'd');
CREATE TABLE h_ib (id INTEGER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, v TEXT DEFAULT 'd');
INSERT INTO h_ia (id, v) VALUES (5, 'x');
INSERT INTO h_ia (id, v) OVERRIDING SYSTEM VALUE VALUES (5, 'x');
INSERT INTO h_ia (id, v) OVERRIDING USER VALUE VALUES (50, 'y');
INSERT INTO h_ib (id, v) OVERRIDING USER VALUE VALUES (50, 'y');
INSERT INTO h_ia DEFAULT VALUES;
INSERT INTO h_ib DEFAULT VALUES;
SELECT id, v FROM h_ia ORDER BY id;
SELECT id, v FROM h_ib ORDER BY id;
ALTER TABLE h_ib ALTER COLUMN v SET DEFAULT 'zz';
INSERT INTO h_ib DEFAULT VALUES;
ALTER TABLE h_ib ALTER COLUMN v SET NOT NULL;
INSERT INTO h_ib (v) VALUES (NULL);
ALTER TABLE h_ib ALTER COLUMN v DROP NOT NULL;
INSERT INTO h_ib (v) VALUES (NULL);
ALTER TABLE h_ib ALTER COLUMN v DROP DEFAULT;
INSERT INTO h_ib (id) VALUES (99);
SELECT id, v FROM h_ib WHERE id IN (3, 99) ORDER BY id;
CREATE TABLE h_dv (a INTEGER DEFAULT 7, b TEXT, c TIMESTAMPTZ DEFAULT now() );
INSERT INTO h_dv DEFAULT VALUES;
SELECT a, b, c IS NOT NULL AS c FROM h_dv;
INSERT INTO h_dv DEFAULT VALUES RETURNING a, b;
