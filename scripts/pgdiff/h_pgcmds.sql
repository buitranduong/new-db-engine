-- Group h: basic PostgreSQL commands an ORM / BI tool / app commonly sends (branch pgcmds).
-- Each statement is a small probe; see docs/14 appendix O.

-- ### h/maint-cmds
CHECKPOINT;
VACUUM;
ANALYZE;
VACUUM ANALYZE;
CREATE TABLE h_pc_m (id int PRIMARY KEY, v text);
INSERT INTO h_pc_m VALUES (1,'a'),(2,'b');
ANALYZE h_pc_m;
ANALYZE h_pc_m (v);
VACUUM h_pc_m;
VACUUM (ANALYZE) h_pc_m;
VACUUM (VERBOSE, ANALYZE) h_pc_m;
VACUUM FULL h_pc_m;
REINDEX TABLE h_pc_m;
CLUSTER h_pc_m USING h_pc_m_pkey;
SELECT count(*) FROM h_pc_m;
DISCARD ALL;
RESET ALL;
SELECT 1;

-- ### h/session-cmds
-- drop: h_pc_s
SET application_name = 'x1';
SHOW application_name;
SET client_encoding TO 'UTF8';
SET standard_conforming_strings = on;
SET extra_float_digits = 3;
SET statement_timeout = 5000;
SET lock_timeout = '2s';
SET idle_in_transaction_session_timeout = 0;
SET TIME ZONE 'UTC';
SET search_path TO public;
SET LOCAL statement_timeout = 1000;
SHOW server_version_num;
SHOW DateStyle;
SHOW TimeZone;
SHOW max_identifier_length;
SHOW transaction_read_only;
SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED;
SELECT set_config('myapp.tenant', '7', false);
SELECT current_setting('myapp.tenant');
SELECT current_setting('nope.nothing', true);
SELECT current_setting('nope.nothing');
SELECT pg_backend_pid() > 0;
SELECT current_database(), current_schema(), current_user, session_user;
SELECT pg_is_in_recovery();
SELECT txid_current() > 0;
SELECT pg_advisory_lock(42);
SELECT pg_try_advisory_lock(43);
SELECT pg_advisory_unlock(42);
SELECT pg_advisory_unlock_all();
SELECT pg_advisory_xact_lock(1);
LISTEN h_chan;
NOTIFY h_chan, 'hello';
UNLISTEN *;
SELECT pg_notify('h_chan', 'x');
SELECT pg_sleep(0);
SELECT pg_total_relation_size('pg_class') > 0;
SELECT pg_database_size(current_database()) > 0;
SELECT pg_size_pretty(1048576);
SELECT pg_get_serial_sequence('h_pc_s', 'id');
SELECT inet_client_addr() IS NOT NULL;

-- ### h/tx-cmds
-- drop: h_pc_t
CREATE TABLE h_pc_t (id int PRIMARY KEY, v int);
BEGIN ISOLATION LEVEL SERIALIZABLE;
INSERT INTO h_pc_t VALUES (1,1);
SAVEPOINT s1;
INSERT INTO h_pc_t VALUES (2,2);
ROLLBACK TO SAVEPOINT s1;
RELEASE SAVEPOINT s1;
COMMIT;
SELECT * FROM h_pc_t ORDER BY id;
BEGIN READ ONLY;
INSERT INTO h_pc_t VALUES (3,3);
ROLLBACK;
START TRANSACTION;
SELECT 1;
END;
BEGIN;
LOCK TABLE h_pc_t IN ACCESS EXCLUSIVE MODE;
LOCK TABLE h_pc_t IN SHARE ROW EXCLUSIVE MODE;
COMMIT;
BEGIN;
SELECT * FROM h_pc_t WHERE id = 1 FOR UPDATE;
SELECT * FROM h_pc_t WHERE id = 1 FOR UPDATE SKIP LOCKED;
SELECT * FROM h_pc_t WHERE id = 1 FOR UPDATE NOWAIT;
SELECT * FROM h_pc_t WHERE id = 1 FOR SHARE;
SELECT * FROM h_pc_t WHERE id = 1 FOR NO KEY UPDATE;
COMMIT;
BEGIN;
SET TRANSACTION ISOLATION LEVEL REPEATABLE READ;
SELECT 1;
COMMIT;
PREPARE h_pc_p (int) AS SELECT v FROM h_pc_t WHERE id = $1;
EXECUTE h_pc_p(1);
DEALLOCATE h_pc_p;
DEALLOCATE ALL;

-- ### h/copy-explain-misc
-- drop: h_pc_c
CREATE TABLE h_pc_c (id int PRIMARY KEY, v text);
INSERT INTO h_pc_c VALUES (1,'a');
EXPLAIN SELECT * FROM h_pc_c;
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM h_pc_c;
EXPLAIN (COSTS OFF) SELECT * FROM h_pc_c WHERE id = 1;
TABLE h_pc_c;
VALUES (1,'a'),(2,'b');
SELECT * FROM h_pc_c TABLESAMPLE SYSTEM (100);
SELECT ctid IS NOT NULL FROM h_pc_c;
SELECT tableoid::regclass FROM h_pc_c;
DO $$ BEGIN NULL; END $$;
DO $$ BEGIN INSERT INTO h_pc_c VALUES (9,'z'); END $$;
SELECT count(*) FROM h_pc_c;

-- ### h/functions-ddl
-- drop: h_pc_f
CREATE OR REPLACE FUNCTION h_pc_add(a int, b int) RETURNS int AS $$ SELECT a + b $$ LANGUAGE sql IMMUTABLE;
SELECT h_pc_add(1, 2);
CREATE OR REPLACE FUNCTION h_pc_const() RETURNS text LANGUAGE plpgsql AS $$ BEGIN RETURN 'hi'; END $$;
SELECT h_pc_const();
DROP FUNCTION h_pc_add(int, int);
DROP FUNCTION IF EXISTS h_pc_const();
CREATE FUNCTION h_pc_trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.v := 1; RETURN NEW; END $$;
CREATE SEQUENCE h_pc_seq START 5 INCREMENT 5;
SELECT nextval('h_pc_seq'), nextval('h_pc_seq');
SELECT currval('h_pc_seq');
SELECT setval('h_pc_seq', 100);
ALTER SEQUENCE h_pc_seq RESTART WITH 1;
SELECT nextval('h_pc_seq');
DROP SEQUENCE h_pc_seq;
CREATE TABLE h_pc_f (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v text);
INSERT INTO h_pc_f (v) VALUES ('a'),('b');
INSERT INTO h_pc_f (id, v) VALUES (10,'c');
INSERT INTO h_pc_f (id, v) OVERRIDING SYSTEM VALUE VALUES (10,'c');
SELECT id, v FROM h_pc_f ORDER BY id;
CREATE TABLE h_pc_f2 (id serial PRIMARY KEY, v text);
INSERT INTO h_pc_f2 (v) VALUES ('a') RETURNING id;
ALTER TABLE h_pc_f2 ALTER COLUMN id RESTART WITH 50;
SELECT pg_get_serial_sequence('h_pc_f2', 'id');

-- ### h/types-minimal
-- drop: h_pc_ty
SELECT '192.168.1.5'::inet;
SELECT '192.168.1.0/24'::cidr;
SELECT '192.168.1.5'::inet << '192.168.1.0/24'::cidr;
SELECT host('192.168.1.5/24'::inet);
SELECT '12.50'::money;
SELECT 12.5::numeric::money;
SELECT B'101'::bit(3);
SELECT '101'::varbit;
SELECT '1 day 2 hours'::interval;
SELECT ARRAY['1 day'::interval, '2 hours'::interval];
SELECT ARRAY['10:00'::time, '11:00'::time];
SELECT 'a'::'char';
SELECT '{1,2,3}'::int[];
SELECT '00:11:22:33:44:55'::macaddr;
SELECT int4range(1,5);
SELECT '$1,234.50'::money;
SELECT 1::oid;
SELECT 'x'::name;
SELECT 'x'::regclass;
SELECT 'abc' COLLATE 'C' < 'abd';
SELECT 'abc' COLLATE 'default' < 'abd';
SELECT 'a' COLLATE 'POSIX' = 'a';
SELECT 'a' COLLATE 'en_US' = 'a';
CREATE TABLE h_pc_ty (id int PRIMARY KEY, ip inet, m money, flags bit(4), name varchar(10) COLLATE 'C');
INSERT INTO h_pc_ty VALUES (1, '10.0.0.1', '5.00', B'1010', 'x');
SELECT ip::text, m::text, flags::text, name FROM h_pc_ty;
SELECT * FROM h_pc_ty WHERE ip = '10.0.0.1';

-- ### h/expr-index
-- drop: h_pc_ei
CREATE TABLE h_pc_ei (id int PRIMARY KEY, a text, b text, ts timestamp, n int);
INSERT INTO h_pc_ei VALUES (1,'B','x','2026-01-01 10:00',3),(2,'a','y','2026-01-02 10:00',1),(3,'C','z','2026-01-03 10:00',2);
CREATE INDEX h_pc_ei_l ON h_pc_ei (lower(a));
SELECT id FROM h_pc_ei ORDER BY lower(a) LIMIT 2;
SELECT id FROM h_pc_ei WHERE lower(a) = 'b';
CREATE INDEX h_pc_ei_m ON h_pc_ei (lower(a), lower(b));
CREATE INDEX h_pc_ei_c ON h_pc_ei (a, lower(b));
CREATE INDEX h_pc_ei_d ON h_pc_ei (date_trunc('day', ts));
CREATE INDEX h_pc_ei_cat ON h_pc_ei ((a || b));
CREATE INDEX h_pc_ei_cast ON h_pc_ei ((n::text));
CREATE INDEX h_pc_ei_sub ON h_pc_ei (substr(a, 1, 1));
CREATE INDEX h_pc_ei_coa ON h_pc_ei (coalesce(a, ''));
CREATE UNIQUE INDEX h_pc_ei_u ON h_pc_ei (lower(a));
INSERT INTO h_pc_ei VALUES (4,'b','q','2026-01-04',9);
CREATE TABLE h_pc_ei2 (id int PRIMARY KEY, x text, UNIQUE (lower(x)));
CREATE TABLE h_pc_ei3 (id int PRIMARY KEY, x text, CONSTRAINT u1 UNIQUE (x));
ALTER TABLE h_pc_ei3 ADD CONSTRAINT u2 UNIQUE (id, x);
CREATE INDEX CONCURRENTLY h_pc_ei_conc ON h_pc_ei3 (x);
CREATE INDEX IF NOT EXISTS h_pc_ei_conc ON h_pc_ei3 (x);
CREATE INDEX h_pc_ei_part ON h_pc_ei3 (x) WHERE x IS NOT NULL;
CREATE INDEX h_pc_ei_inc ON h_pc_ei3 (x) INCLUDE (id);
CREATE INDEX h_pc_ei_gin ON h_pc_ei3 USING gin (x);
CREATE INDEX h_pc_ei_hash ON h_pc_ei3 USING hash (x);
CREATE INDEX h_pc_ei_bt ON h_pc_ei3 USING btree (x DESC NULLS LAST);
CREATE INDEX h_pc_ei_ops ON h_pc_ei3 (x text_pattern_ops);
DROP INDEX CONCURRENTLY h_pc_ei_conc;

-- ### h/partition
-- drop: h_pc_pt, h_pc_pt1, h_pc_pt2, h_pc_pl, h_pc_pl1
CREATE TABLE h_pc_pt (id int, ts date, v text) PARTITION BY RANGE (ts);
CREATE TABLE h_pc_pt1 PARTITION OF h_pc_pt FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE h_pc_pt2 PARTITION OF h_pc_pt FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
INSERT INTO h_pc_pt VALUES (1,'2026-01-05','a'),(2,'2026-02-05','b');
INSERT INTO h_pc_pt VALUES (3,'2027-01-05','c');
SELECT id FROM h_pc_pt ORDER BY id;
SELECT id FROM h_pc_pt WHERE ts = '2026-02-05';
SELECT count(*) FROM h_pc_pt1;
CREATE TABLE h_pc_pl (id int, region text) PARTITION BY LIST (region);
CREATE TABLE h_pc_pl1 PARTITION OF h_pc_pl FOR VALUES IN ('eu','us');
INSERT INTO h_pc_pl VALUES (1,'eu');
SELECT count(*) FROM h_pc_pl;
UPDATE h_pc_pt SET v = 'q' WHERE id = 1;
DELETE FROM h_pc_pt WHERE id = 2;
SELECT id, v FROM h_pc_pt ORDER BY id;

-- ### h/crypto
SELECT length(gen_random_uuid()::text);
SELECT md5('abc');
SELECT encode(sha256('abc'::bytea), 'hex');
SELECT encode(digest('abc', 'sha256'), 'hex');
SELECT crypt('pw', gen_salt('bf')) IS NOT NULL;
SELECT crypt('pw', crypt('pw', gen_salt('bf'))) = crypt('pw', gen_salt('bf'));
SELECT encode(gen_random_bytes(4), 'hex') IS NOT NULL;
SELECT encode(hmac('abc', 'k', 'sha256'), 'hex');
SELECT uuid_generate_v4() IS NOT NULL;

-- ### h/misc-select
-- drop: h_pc_x
CREATE TABLE h_pc_x (id int PRIMARY KEY, a int, b text);
INSERT INTO h_pc_x VALUES (1,1,'a'),(2,1,'b'),(3,2,'c');
SELECT DISTINCT ON (a) a, id FROM h_pc_x ORDER BY a, id DESC;
SELECT a, string_agg(b, ',' ORDER BY b) FROM h_pc_x GROUP BY a ORDER BY a;
SELECT a, array_agg(id ORDER BY id) FROM h_pc_x GROUP BY a ORDER BY a;
SELECT id, row_number() OVER (PARTITION BY a ORDER BY id) FROM h_pc_x ORDER BY id;
SELECT a, count(*) FILTER (WHERE id > 1) FROM h_pc_x GROUP BY a ORDER BY a;
SELECT a, count(*) FROM h_pc_x GROUP BY ROLLUP (a) ORDER BY a;
SELECT a, count(*) FROM h_pc_x GROUP BY GROUPING SETS ((a), ()) ORDER BY a;
SELECT * FROM generate_series(1,3) AS g(n);
SELECT * FROM h_pc_x x, LATERAL (SELECT count(*) c FROM h_pc_x y WHERE y.a = x.a) l ORDER BY x.id;
WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3) SELECT * FROM r;
INSERT INTO h_pc_x VALUES (1,9,'z') ON CONFLICT (id) DO UPDATE SET a = EXCLUDED.a RETURNING id, a;
INSERT INTO h_pc_x VALUES (1,9,'z') ON CONFLICT DO NOTHING;
UPDATE h_pc_x SET a = x2.a FROM h_pc_x x2 WHERE h_pc_x.id = 3 AND x2.id = 1;
DELETE FROM h_pc_x USING h_pc_x y WHERE h_pc_x.id = y.id AND y.id = 3;
SELECT * FROM h_pc_x ORDER BY id OFFSET 1 LIMIT 1;
SELECT * FROM h_pc_x ORDER BY id FETCH FIRST 1 ROWS ONLY;
SELECT a IS NOT DISTINCT FROM 1 FROM h_pc_x ORDER BY id;
SELECT to_char(now(), 'YYYY') = to_char(now(), 'YYYY');
SELECT extract(epoch FROM '2026-01-01'::timestamptz);
SELECT '2026-01-01'::date + 30;
SELECT age('2026-03-01'::date, '2026-01-01'::date);
SELECT jsonb_build_object('a', 1) || '{"b":2}'::jsonb;
SELECT regexp_replace('abc', 'b', 'X');
SELECT 'abc' ~* 'B';
SELECT 'abc' SIMILAR TO 'a%';
SELECT unnest(ARRAY[1,2]) ;
SELECT current_timestamp(3) IS NOT NULL;
SELECT quote_ident('a b'), quote_literal('it''s'), quote_nullable(NULL);
