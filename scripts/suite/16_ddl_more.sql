-- Suite group 16: more DDL and constraints (generated columns, referential actions, deferrable constraints,
-- NOT VALID, partition bounds, DEFAULT/HASH partitions, WITH CHECK OPTION, ALTER VIEW, identity ALTERs,
-- expression indexes, information_schema constraint views, search_path). Syntax source: PostgreSQL 16 docs
-- CREATE TABLE, ALTER TABLE, CREATE VIEW, SET CONSTRAINTS, section 5.3 (generated columns), 5.11 (partitioning).
-- Rows of HASH partitions are only counted in total: the hash function is not PostgreSQL's.

-- ### 16_ddl_more/setup
DROP SCHEMA IF EXISTS s16 CASCADE;
CREATE SCHEMA s16;

-- ### 16_ddl_more/generated
SET search_path = s16;
CREATE TABLE g (id int PRIMARY KEY, a int, b int GENERATED ALWAYS AS (a * 2) STORED, c text GENERATED ALWAYS AS (a::text || 'x') STORED);
INSERT INTO g (id, a) VALUES (1, 5), (2, NULL);
INSERT INTO g VALUES (3, 7, DEFAULT, DEFAULT);
SELECT id, b, c FROM g ORDER BY id;
UPDATE g SET a = 10 WHERE id = 1;
SELECT b FROM g WHERE id = 1;
UPDATE g SET b = DEFAULT WHERE id = 1;
CREATE INDEX g_b ON g (b);
SELECT id FROM g WHERE b = 20;
INSERT INTO g (id, a) VALUES (3, 100) ON CONFLICT (id) DO UPDATE SET a = excluded.a RETURNING id, b;
INSERT INTO g (id, a) SELECT 10 + x, x FROM generate_series(1, 3) x RETURNING id, b, c;
SELECT is_generated::text, column_default FROM information_schema.columns WHERE table_schema = 's16' AND table_name = 'g' AND column_name = 'b';
SELECT attname::text, attgenerated::text FROM pg_attribute WHERE attrelid = 'g'::regclass AND attnum > 0 ORDER BY attnum;
COPY g (id, a) FROM STDIN;
-- stdin: 20\t3\n21\t4\n\.\n
SELECT id, b FROM g WHERE id >= 20 ORDER BY id;
CREATE TABLE g_like (LIKE g INCLUDING GENERATED);
INSERT INTO g_like (id, a) VALUES (1, 4);
SELECT b FROM g_like;
ALTER TABLE g ADD COLUMN d int GENERATED ALWAYS AS (id * 3) STORED;
SELECT id, d FROM g WHERE id < 4 ORDER BY id;
ALTER TABLE g ALTER COLUMN d DROP EXPRESSION;
INSERT INTO g (id, d) VALUES (99, 99);
SELECT d FROM g WHERE id = 99;
ALTER TABLE g DROP COLUMN a CASCADE;
SELECT column_name::text FROM information_schema.columns WHERE table_schema = 's16' AND table_name = 'g' ORDER BY ordinal_position;

-- ### 16_ddl_more/generated-errors
SET search_path = s16;
CREATE TABLE ge (id int PRIMARY KEY, a int, b int GENERATED ALWAYS AS (a * 2) STORED);
INSERT INTO ge (id, a, b) VALUES (4, 1, 2);
INSERT INTO ge (id, a) VALUES (1, 1);
UPDATE ge SET b = 3;
CREATE TABLE ge2 (a int, b int GENERATED ALWAYS AS (a * 2) STORED DEFAULT 1);
CREATE TABLE ge3 (a int, b int GENERATED ALWAYS AS (b * 2) STORED);
CREATE TABLE ge4 (a int, b int GENERATED ALWAYS AS (a * 2) STORED, c int GENERATED ALWAYS AS (b + 1) STORED);
CREATE TABLE ge5 (a int, b timestamptz GENERATED ALWAYS AS (now()) STORED);
CREATE TABLE ge6 (a int, b int GENERATED ALWAYS AS ((SELECT 1)) STORED);
CREATE TABLE ge7 (a int, b int GENERATED ALWAYS AS (a * 2));
CREATE TABLE ge8 (a int, b int GENERATED ALWAYS AS (zz * 2) STORED);
ALTER TABLE ge ALTER COLUMN b SET DEFAULT 5;
ALTER TABLE ge DROP COLUMN a;
ALTER TABLE ge ALTER COLUMN a TYPE bigint;
ALTER TABLE ge ALTER COLUMN id DROP EXPRESSION;

-- ### 16_ddl_more/fk-actions
SET search_path = s16;
CREATE TABLE pa (id int PRIMARY KEY, v int UNIQUE);
CREATE TABLE ch (id int PRIMARY KEY, pid int DEFAULT 2 REFERENCES pa (id) ON UPDATE CASCADE ON DELETE SET NULL);
INSERT INTO pa VALUES (1, 1), (2, 2);
INSERT INTO ch VALUES (10, 1), (11, 2);
UPDATE pa SET id = 100 WHERE id = 1;
SELECT id, pid FROM ch ORDER BY id;
DELETE FROM pa WHERE id = 2;
SELECT id, pid FROM ch ORDER BY id;
DROP TABLE ch;
CREATE TABLE ch (id int PRIMARY KEY, pid int DEFAULT 2 REFERENCES pa (id) ON UPDATE SET DEFAULT ON DELETE SET DEFAULT);
INSERT INTO pa VALUES (2, 22), (3, 3), (4, 4);
INSERT INTO ch VALUES (10, 3), (11, 100), (12, 4);
DELETE FROM pa WHERE id = 3;
UPDATE pa SET id = 40 WHERE id = 4;
SELECT id, pid FROM ch ORDER BY id;
UPDATE pa SET id = 200 WHERE id = 100;
SELECT id, pid FROM ch ORDER BY id;
DROP TABLE ch;
CREATE TABLE ch (id int PRIMARY KEY, pid int REFERENCES pa (id) ON UPDATE RESTRICT ON DELETE RESTRICT);
INSERT INTO ch VALUES (10, 200);
UPDATE pa SET id = 201 WHERE id = 200;
DELETE FROM pa WHERE id = 200;
SELECT confupdtype::text, confdeltype::text FROM pg_constraint WHERE conname = 'ch_pid_fkey' AND connamespace = 's16'::regnamespace;
SELECT update_rule::text, delete_rule::text FROM information_schema.referential_constraints WHERE constraint_schema = 's16' AND constraint_name = 'ch_pid_fkey';

-- ### 16_ddl_more/deferrable
SET search_path = s16;
CREATE TABLE dp (id int PRIMARY KEY);
CREATE TABLE d1 (id int PRIMARY KEY, pid int REFERENCES dp (id) DEFERRABLE INITIALLY DEFERRED);
CREATE TABLE d2 (id int PRIMARY KEY, pid int REFERENCES dp (id) DEFERRABLE INITIALLY IMMEDIATE);
BEGIN;
INSERT INTO d1 VALUES (1, 999);
INSERT INTO dp VALUES (999);
COMMIT;
SELECT id, pid FROM d1;
BEGIN;
INSERT INTO d1 VALUES (2, 888);
COMMIT;
SELECT count(*) FROM d1;
INSERT INTO d2 VALUES (1, 777);
BEGIN;
SET CONSTRAINTS ALL DEFERRED;
INSERT INTO d2 VALUES (1, 777);
INSERT INTO dp VALUES (777);
COMMIT;
SELECT id, pid FROM d2;
BEGIN;
SET CONSTRAINTS d2_pid_fkey DEFERRED;
INSERT INTO d2 VALUES (2, 555);
SET CONSTRAINTS d2_pid_fkey IMMEDIATE;
ROLLBACK;
BEGIN;
SET CONSTRAINTS dp_pkey DEFERRED;
ROLLBACK;
SELECT condeferrable, condeferred FROM pg_constraint WHERE conname = 'd1_pid_fkey' AND connamespace = 's16'::regnamespace;
SELECT is_deferrable::text, initially_deferred::text FROM information_schema.table_constraints WHERE constraint_schema = 's16' AND constraint_name = 'd2_pid_fkey';

-- ### 16_ddl_more/not-valid
SET search_path = s16;
CREATE TABLE np (id int PRIMARY KEY);
INSERT INTO np VALUES (1), (2);
CREATE TABLE nv (id int PRIMARY KEY, pid int);
INSERT INTO nv VALUES (1, 1), (2, 1000);
ALTER TABLE nv ADD CONSTRAINT nv_fk FOREIGN KEY (pid) REFERENCES np (id) NOT VALID;
INSERT INTO nv VALUES (3, 1001);
INSERT INTO nv VALUES (4, 2);
ALTER TABLE nv VALIDATE CONSTRAINT nv_fk;
DELETE FROM nv WHERE pid > 100;
ALTER TABLE nv VALIDATE CONSTRAINT nv_fk;
SELECT convalidated FROM pg_constraint WHERE conname = 'nv_fk' AND connamespace = 's16'::regnamespace;
INSERT INTO nv VALUES (30, 2000);
INSERT INTO nv VALUES (50, 1);
ALTER TABLE nv ADD CONSTRAINT nv_ck CHECK (id < 40) NOT VALID;
INSERT INTO nv VALUES (5, 1);
INSERT INTO nv VALUES (41, 1);
ALTER TABLE nv VALIDATE CONSTRAINT nv_ck;
CREATE TABLE u2 (a int, b int, UNIQUE NULLS NOT DISTINCT (a, b));
INSERT INTO u2 VALUES (1, NULL);
INSERT INTO u2 VALUES (1, NULL);
CREATE TABLE u1 (a int, b int, UNIQUE (a, b));
INSERT INTO u1 VALUES (1, NULL), (1, NULL);
CREATE TABLE u3 (a int UNIQUE NULLS NOT DISTINCT);
INSERT INTO u3 VALUES (NULL);
INSERT INTO u3 VALUES (NULL);

-- ### 16_ddl_more/partitions
SET search_path = s16;
CREATE TABLE pl (country text, n int) PARTITION BY LIST (country);
INSERT INTO pl VALUES ('FR', 2);
CREATE TABLE pl_us PARTITION OF pl FOR VALUES IN ('US');
INSERT INTO pl VALUES ('FR', 2);
INSERT INTO pl_us VALUES ('DE', 3);
INSERT INTO pl VALUES ('US', 1);
UPDATE pl_us SET country = 'DE';
UPDATE pl SET country = 'DE';
CREATE TABLE pl_def PARTITION OF pl DEFAULT;
CREATE TABLE pl_def2 PARTITION OF pl DEFAULT;
INSERT INTO pl VALUES ('FR', 2), ('US', 5);
SELECT n FROM pl_def ORDER BY n;
SELECT n FROM pl_us ORDER BY n;
CREATE TABLE pl_fr PARTITION OF pl FOR VALUES IN ('FR');
CREATE TABLE pl_us2 PARTITION OF pl FOR VALUES IN ('US', 'CA');
ALTER TABLE pl DETACH PARTITION pl_us;
SELECT n FROM pl_us ORDER BY n;
SELECT count(*) FROM pl;
INSERT INTO pl_us VALUES ('XX', 1);
ALTER TABLE pl ATTACH PARTITION pl_us FOR VALUES IN ('US');
DELETE FROM pl_us WHERE country = 'XX';
ALTER TABLE pl ATTACH PARTITION pl_us FOR VALUES IN ('US');
SELECT count(*) FROM pl_us;
DROP TABLE pl_us;
SELECT count(*) FROM pl;
CREATE TABLE pr (d date, v int) PARTITION BY RANGE (d);
CREATE TABLE pr1 PARTITION OF pr FOR VALUES FROM ('2024-01-01') TO ('2024-02-01');
CREATE TABLE pr2 PARTITION OF pr FOR VALUES FROM ('2024-02-01') TO ('2024-03-01');
CREATE TABLE pr3 PARTITION OF pr FOR VALUES FROM ('2024-01-15') TO ('2024-02-15');
INSERT INTO pr VALUES ('2024-01-05', 1), ('2024-02-05', 2);
INSERT INTO pr VALUES ('2025-01-05', 1);
INSERT INTO pr VALUES (NULL, 1);
UPDATE pr SET d = '2024-02-10' WHERE v = 1;
SELECT count(*) FROM pr1;
SELECT count(*) FROM pr2;
SELECT d, v FROM pr ORDER BY v;
CREATE TABLE ph (id int, v int) PARTITION BY HASH (id);
CREATE TABLE ph0 PARTITION OF ph FOR VALUES WITH (MODULUS 2, REMAINDER 0);
CREATE TABLE ph1 PARTITION OF ph FOR VALUES WITH (MODULUS 2, REMAINDER 1);
CREATE TABLE ph1b PARTITION OF ph FOR VALUES WITH (MODULUS 2, REMAINDER 1);
INSERT INTO ph SELECT x, x FROM generate_series(1, 100) x;
SELECT (SELECT count(*) FROM ph0) + (SELECT count(*) FROM ph1) = (SELECT count(*) FROM ph);

-- ### 16_ddl_more/views
SET search_path = s16;
CREATE TABLE vt (id int PRIMARY KEY, a int);
CREATE VIEW vc AS SELECT id, a FROM vt WHERE a > 0 WITH CHECK OPTION;
INSERT INTO vc VALUES (1, 5);
INSERT INTO vc VALUES (2, -5);
UPDATE vc SET a = -1;
UPDATE vc SET a = 7;
SELECT a FROM vt;
CREATE VIEW vn AS SELECT id, a FROM vt WHERE a > 0;
INSERT INTO vn VALUES (3, -3);
SELECT count(*) FROM vn;
CREATE VIEW vl AS SELECT id, a FROM vt WHERE a < 100 WITH LOCAL CHECK OPTION;
INSERT INTO vl VALUES (4, 200);
ALTER VIEW vn RENAME TO vn2;
SELECT count(*) FROM vn2;
ALTER TABLE vt RENAME TO vt2;
SELECT count(*) FROM vc;
CREATE RECURSIVE VIEW rv (n) AS VALUES (1) UNION ALL SELECT n + 1 FROM rv WHERE n < 5;
SELECT n FROM rv ORDER BY n;
CREATE MATERIALIZED VIEW mv AS SELECT id, a FROM vt2 WITH NO DATA;
SELECT * FROM mv;
REFRESH MATERIALIZED VIEW mv;
CREATE UNIQUE INDEX mv_id ON mv (id);
REFRESH MATERIALIZED VIEW CONCURRENTLY mv;
ALTER MATERIALIZED VIEW mv RENAME TO mv2;
SELECT count(*) FROM mv2;
SELECT matviewname::text, ispopulated FROM pg_matviews WHERE schemaname = 's16';

-- ### 16_ddl_more/indexes-alter
SET search_path = s16;
CREATE TABLE ti (id int PRIMARY KEY, ts timestamp, name text, code varchar(20));
INSERT INTO ti SELECT x, timestamp '2024-01-01' + (x || ' hours')::interval, 'name' || x, 'AB' || (x % 7) || 'zz' FROM generate_series(1, 200) x;
CREATE INDEX ti_d ON ti (date_trunc('day', ts));
CREATE INDEX ti_s ON ti (substr(code, 1, 3));
CREATE INDEX ti_i ON ti (id) INCLUDE (name);
CREATE INDEX CONCURRENTLY ti_c ON ti (name);
SELECT count(*) FROM ti WHERE date_trunc('day', ts) = '2024-01-03';
SELECT count(*) FROM ti WHERE substr(code, 1, 3) = 'AB3';
SELECT date_trunc('day', ts)::text, count(*) FROM ti GROUP BY 1 ORDER BY 1 LIMIT 3;
CREATE INDEX ti_bad ON ti (date_trunc('foo', ts));
REINDEX INDEX ti_d;
REINDEX TABLE ti;
ALTER INDEX ti_c RENAME TO ti_c2;
ALTER INDEX ti_i SET (fillfactor = 70);
COMMENT ON TABLE ti IS 'hello';
COMMENT ON COLUMN ti.name IS 'col';
SELECT obj_description('ti'::regclass), col_description('ti'::regclass, 3);
ALTER TABLE ti SET (autovacuum_enabled = false);
ALTER TABLE ti ALTER COLUMN name SET STATISTICS 100;
ALTER TABLE ti ALTER COLUMN name SET STORAGE external;
ALTER TABLE ti DISABLE TRIGGER ALL;
ALTER TABLE ti ENABLE TRIGGER ALL;
CREATE TABLE ti_like (LIKE ti INCLUDING ALL);
CREATE TABLE ti_nodata AS SELECT * FROM ti WITH NO DATA;
SELECT count(*) FROM ti_nodata;

-- ### 16_ddl_more/identity
SET search_path = s16;
CREATE TABLE idt (id int GENERATED BY DEFAULT AS IDENTITY, x int);
INSERT INTO idt (x) VALUES (1), (2);
INSERT INTO idt (id, x) VALUES (10, 3);
ALTER TABLE idt ALTER COLUMN id SET GENERATED ALWAYS;
INSERT INTO idt (id, x) VALUES (11, 3);
ALTER TABLE idt ALTER COLUMN id RESTART WITH 50;
INSERT INTO idt (x) VALUES (9);
SELECT id, x FROM idt ORDER BY id;
CREATE TABLE nid (a int NOT NULL, b int);
ALTER TABLE nid ALTER COLUMN b ADD GENERATED ALWAYS AS IDENTITY;
ALTER TABLE nid ALTER COLUMN a ADD GENERATED ALWAYS AS IDENTITY;
INSERT INTO nid (b) VALUES (5);
SELECT a, b FROM nid;
ALTER TABLE nid ALTER COLUMN a SET GENERATED BY DEFAULT;
INSERT INTO nid VALUES (7, 7);
ALTER TABLE nid ALTER COLUMN a DROP IDENTITY;
ALTER TABLE nid ALTER COLUMN a DROP IDENTITY;
CREATE SEQUENCE sq1 CYCLE MAXVALUE 3 START 1;
SELECT nextval('sq1'), nextval('sq1'), nextval('sq1'), nextval('sq1');
ALTER SEQUENCE sq1 RESTART WITH 2 INCREMENT BY 2 NO CYCLE;
SELECT nextval('sq1');

-- ### 16_ddl_more/info-schema
SET search_path = s16;
CREATE TABLE ip (id int PRIMARY KEY, v int UNIQUE, w int, CONSTRAINT ip_w CHECK (w > 0));
CREATE TABLE ic (id serial PRIMARY KEY, pid int NOT NULL REFERENCES ip (id) ON DELETE CASCADE ON UPDATE SET NULL, v int, UNIQUE (pid, v), CHECK (v < 100));
SELECT constraint_name::text, table_name::text, constraint_type::text FROM information_schema.table_constraints WHERE table_schema = 's16' AND table_name IN ('ip', 'ic') AND constraint_name NOT LIKE '%not_null' ORDER BY 1, 2;
SELECT constraint_name::text, table_name::text, column_name::text, ordinal_position FROM information_schema.key_column_usage WHERE table_schema = 's16' AND table_name IN ('ip', 'ic') ORDER BY 1, 2, 4;
SELECT constraint_name::text, unique_constraint_name::text, match_option::text, update_rule::text, delete_rule::text FROM information_schema.referential_constraints WHERE constraint_schema = 's16' ORDER BY 1;
SELECT constraint_name::text, check_clause::text FROM information_schema.check_constraints WHERE constraint_schema = 's16' AND constraint_name NOT LIKE '%not_null' ORDER BY 1;
SELECT constraint_name::text, table_name::text, column_name::text FROM information_schema.constraint_column_usage WHERE table_schema = 's16' AND table_name IN ('ip', 'ic') ORDER BY 1, 2, 3;
SELECT count(*) FROM information_schema.table_constraints WHERE table_schema = 's16' AND table_name = 'ic' AND constraint_name LIKE '%not_null';
SELECT schemaname::text, sequencename::text, increment_by FROM pg_sequences WHERE schemaname = 's16' ORDER BY 2;

-- ### 16_ddl_more/search-path
SET search_path = s16;
CREATE SCHEMA s16b;
SET search_path = s16b;
CREATE TABLE t (id int PRIMARY KEY, n int);
INSERT INTO t VALUES (1, 1), (2, 2);
CREATE VIEW v AS SELECT * FROM t;
CREATE MATERIALIZED VIEW mv AS SELECT id FROM t;
SELECT count(*) FROM v;
SELECT count(*) FROM mv;
INSERT INTO t VALUES (3, 3);
REFRESH MATERIALIZED VIEW mv;
SELECT count(*) FROM mv;
CREATE TEMP TABLE tt (a int);
INSERT INTO tt VALUES (1);
SELECT count(*) FROM tt;
SELECT count(*) FROM pg_temp.tt;
COPY t FROM STDIN;
-- stdin: 10\t10\n\.\n
SELECT count(*) FROM t;
SELECT 't'::regclass::text, 's16b'::regnamespace::text;
SET search_path = public, s16b;
SELECT count(*) FROM v;
CREATE TABLE t (id int PRIMARY KEY, n int);
SELECT count(*) FROM s16b.v;
SELECT count(*) FROM t;
SELECT schemaname::text, matviewname::text FROM pg_matviews WHERE schemaname = 's16b';
DROP TABLE public.t;

-- ### 16_ddl_more/teardown
DROP SCHEMA s16 CASCADE;
DROP SCHEMA IF EXISTS s16b CASCADE;
