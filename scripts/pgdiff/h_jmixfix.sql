-- Group h: what a Jmix (Hibernate + Liquibase + pgjdbc) application needs, branch jmixfix
-- (docs/14 appendix F). Indexes, UNIQUE and FOREIGN KEY on uuid / numeric / timestamp(tz) /
-- date / time / bytea, user-named constraints, three-part column names, the pgjdbc
-- metadata functions, '2026-03-04 +00' dates, regclass::text, the oid column type.

-- ### h/uuid-keys
DROP TABLE IF EXISTS jm_line;
DROP TABLE IF EXISTS jm_order;
DROP TABLE IF EXISTS jm_sim;
CREATE TABLE jm_sim (ID uuid NOT NULL, VERSION integer NOT NULL, MSISDN varchar(20), PRICE numeric(19,2), PHOTO oid, CREATED_DATE timestamp, DUE date, CONSTRAINT pk_jm_sim PRIMARY KEY (ID));
CREATE TABLE jm_order (ID uuid NOT NULL, SIM_ID uuid, NOTE varchar(50) DEFAULT 'NEW', CONSTRAINT pk_jm_order PRIMARY KEY (ID));
ALTER TABLE jm_order ADD CONSTRAINT FK_JM_ORDER_ON_SIM FOREIGN KEY (SIM_ID) REFERENCES jm_sim (ID);
CREATE INDEX IDX_JM_ORDER_SIM ON jm_order(SIM_ID);
CREATE UNIQUE INDEX UNQ_JM_SIM_MSISDN ON jm_sim(MSISDN);
CREATE INDEX IDX_JM_SIM_PRICE ON jm_sim(PRICE);
CREATE INDEX IDX_JM_SIM_CREATED ON jm_sim(CREATED_DATE);
CREATE INDEX IDX_JM_SIM_DUE ON jm_sim(DUE);
INSERT INTO jm_sim VALUES ('00000000-0000-0000-0000-000000000001', 1, '0901', 10.50, NULL, '2026-01-01 10:00:00.123456', '2026-03-04');
INSERT INTO jm_sim VALUES ('00000000-0000-0000-0000-000000000002', 1, '0902', -2.25, NULL, '2026-01-02 11:00:00', '2026-03-05');
INSERT INTO jm_sim VALUES ('00000000-0000-0000-0000-000000000003', 1, '0903', 10.5, NULL, NULL, NULL);
INSERT INTO jm_sim VALUES ('00000000-0000-0000-0000-000000000004', 1, '0901', 1, NULL, NULL, NULL);
INSERT INTO jm_sim VALUES ('00000000-0000-0000-0000-000000000001', 1, '0999', 1, NULL, NULL, NULL);
INSERT INTO jm_order (ID, SIM_ID) VALUES ('10000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001');
INSERT INTO jm_order (ID, SIM_ID) VALUES ('10000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-0000000000ff');
DELETE FROM jm_sim WHERE ID = '00000000-0000-0000-0000-000000000001';
SELECT id, price FROM jm_sim WHERE price = 10.5 ORDER BY id;
SELECT id FROM jm_sim WHERE price > -3 AND price < 10.50 ORDER BY price;
SELECT id FROM jm_sim ORDER BY price DESC, id;
SELECT id FROM jm_sim WHERE created_date >= '2026-01-02' ORDER BY id;
SELECT id FROM jm_sim WHERE due = '2026-03-05';
SELECT id FROM jm_sim WHERE id = '{00000000-0000-0000-0000-000000000002}';
SELECT id FROM jm_sim WHERE id IN ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000003') ORDER BY id;
SELECT note FROM jm_order;
SELECT conname, contype FROM pg_constraint WHERE conrelid = 'jm_sim'::regclass ORDER BY conname;
SELECT conname, conrelid::regclass::text, confrelid::regclass::text FROM pg_constraint WHERE contype = 'f' AND conrelid = 'jm_order'::regclass;
SELECT column_name, column_default FROM information_schema.columns WHERE table_name = 'jm_order' ORDER BY ordinal_position;
SELECT data_type FROM information_schema.columns WHERE table_name = 'jm_sim' AND column_name = 'photo';
SELECT indexname FROM pg_indexes WHERE tablename = 'jm_sim' ORDER BY indexname;
UPDATE jm_sim SET price = 3.333 WHERE id = '00000000-0000-0000-0000-000000000004';
SELECT price FROM jm_sim WHERE id = '00000000-0000-0000-0000-000000000004';

-- ### h/numeric-unique
DROP TABLE IF EXISTS jm_num;
CREATE TABLE jm_num (id integer PRIMARY KEY, n numeric, c numeric(12,3));
CREATE UNIQUE INDEX jm_num_n ON jm_num (n);
INSERT INTO jm_num VALUES (1, 1.5, 0.001), (2, -1.5, 1000000.123), (3, 0, -0.5), (4, 100, 99999.999), (5, 0.001, 0), (6, NULL, NULL), (7, NULL, NULL);
INSERT INTO jm_num VALUES (8, 1.50, 1);
INSERT INTO jm_num VALUES (9, 0.0, 1);
INSERT INTO jm_num VALUES (10, 15E-1, 1);
CREATE INDEX jm_num_c ON jm_num (c);
SELECT id FROM jm_num WHERE n = 1.500 ORDER BY id;
SELECT id FROM jm_num WHERE n > -2 ORDER BY n, id;
SELECT id FROM jm_num WHERE n BETWEEN 0 AND 100 ORDER BY n;
SELECT id FROM jm_num ORDER BY n NULLS LAST, id;
SELECT id FROM jm_num ORDER BY c DESC NULLS LAST, id;
SELECT id FROM jm_num WHERE c >= 0.001 AND c < 1000000 ORDER BY c;
SELECT id FROM jm_num WHERE n IS NULL ORDER BY id;
SELECT count(*) FROM jm_num WHERE n = 0;
SELECT id FROM jm_num WHERE n IN (0.001, 100, 7) ORDER BY id;

-- ### h/timestamps-bytea
DROP TABLE IF EXISTS jm_ts;
CREATE TABLE jm_ts (id integer PRIMARY KEY, ts timestamptz, tn timestamp, d date, b bytea);
CREATE INDEX jm_ts_ts ON jm_ts (ts);
CREATE INDEX jm_ts_tn ON jm_ts (tn);
CREATE INDEX jm_ts_d ON jm_ts (d);
CREATE UNIQUE INDEX jm_ts_b ON jm_ts (b);
INSERT INTO jm_ts VALUES (1, '2026-01-01 00:00:00.000001+00', '2026-01-01 00:00:00.000001', '2026-01-01', '\x00');
INSERT INTO jm_ts VALUES (2, '2026-01-01 00:00:00+00', '2026-01-01 00:00:00', '2025-12-31', '\x0000');
INSERT INTO jm_ts VALUES (3, '1969-12-31 23:59:59.999999+00', '1969-12-31 23:59:59.999999', '1969-12-31', '\xff');
INSERT INTO jm_ts VALUES (4, '2026-01-01 07:00:00+07', NULL, NULL, '\x');
INSERT INTO jm_ts VALUES (5, NULL, NULL, NULL, '\x00');
SELECT id FROM jm_ts ORDER BY ts, id;
SELECT id FROM jm_ts WHERE ts = '2026-01-01 00:00:00+00' ORDER BY id;
SELECT id FROM jm_ts WHERE ts > '2026-01-01 00:00:00+00' ORDER BY id;
SELECT id FROM jm_ts WHERE tn < '2026-01-01' ORDER BY tn;
SELECT id FROM jm_ts WHERE d BETWEEN '2025-12-31' AND '2026-01-01' ORDER BY d, id;
SELECT id FROM jm_ts ORDER BY b, id;
SELECT id FROM jm_ts WHERE b = '\x0000';
SELECT id FROM jm_ts WHERE b > '\x00' ORDER BY b;

-- ### h/three-part-columns
DROP TABLE IF EXISTS jm_tp;
CREATE TABLE jm_tp (a integer PRIMARY KEY, b text);
INSERT INTO jm_tp VALUES (1, 'x'), (2, 'y');
SELECT public.jm_tp.a FROM public.jm_tp ORDER BY 1;
SELECT public.jm_tp.a, public.jm_tp.b FROM public.jm_tp WHERE public.jm_tp.a = 2;
SELECT jm_tp.b FROM public.jm_tp ORDER BY jm_tp.a DESC;
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class WHERE pg_catalog.pg_class.relname = 'jm_tp';
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p']) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' AND pg_catalog.pg_class.relname = 'jm_tp';
SELECT pg_catalog.pg_attribute.attname FROM pg_catalog.pg_attribute WHERE pg_catalog.pg_attribute.attrelid = 'jm_tp'::regclass AND pg_catalog.pg_attribute.attnum > 0 ORDER BY pg_catalog.pg_attribute.attnum;
SELECT pg_catalog.pg_namespace.nspname FROM pg_catalog.pg_namespace WHERE (pg_catalog.pg_namespace.nspname NOT LIKE 'pg_%' ESCAPE '/') AND pg_catalog.pg_namespace.nspname != 'information_schema' ORDER BY 1;

-- ### h/metadata-functions
DROP TABLE IF EXISTS jm_cpk;
CREATE TABLE jm_cpk (a integer, b integer, c text, d integer, PRIMARY KEY (a, b));
CREATE INDEX jm_cpk_cd ON jm_cpk (c, d);
SELECT (information_schema._pg_expandarray(i.indkey)).n AS key_seq, (information_schema._pg_expandarray(i.indkey)).x AS x FROM pg_catalog.pg_index i WHERE i.indrelid = 'jm_cpk'::regclass AND i.indisprimary ORDER BY 1;
SELECT a.attname, (information_schema._pg_expandarray(i.indkey)).n FROM pg_catalog.pg_class ct JOIN pg_catalog.pg_attribute a ON (ct.oid = a.attrelid) JOIN pg_catalog.pg_index i ON (a.attrelid = i.indrelid) WHERE ct.relname = 'jm_cpk' AND i.indisprimary AND a.attnum > 0 ORDER BY 1, 2;
SELECT unnest(i.indkey) AS attnum, generate_subscripts(i.indkey, 1) AS ord FROM pg_catalog.pg_index i WHERE i.indrelid = 'jm_cpk'::regclass AND i.indisprimary;
SELECT count(*) FROM pg_catalog.pg_opclass WHERE false;
SELECT typinput = 'pg_catalog.array_in'::regproc FROM pg_catalog.pg_type WHERE typname = '_int4';
SELECT typinput = 'pg_catalog.array_in'::regproc FROM pg_catalog.pg_type WHERE typname = 'int4';
SELECT pg_get_indexdef(i.indexrelid, 2, false) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'jm_cpk_cd';
SELECT attname FROM pg_attribute a JOIN pg_index i ON a.attrelid = i.indrelid WHERE i.indisprimary AND i.indrelid = 'jm_cpk'::regclass AND a.attnum = ANY (i.indkey) ORDER BY attnum;

-- ### h/date-timezone-suffix
DROP TABLE IF EXISTS jm_dz;
CREATE TABLE jm_dz (id integer PRIMARY KEY, d date, t timestamp, tz timestamptz);
INSERT INTO jm_dz VALUES (1, '2026-03-04 +00', '2026-03-04 +00', '2026-03-04 +00');
INSERT INTO jm_dz VALUES (2, '2026-03-04 +07', '2026-03-04 +07', '2026-03-04 +07');
INSERT INTO jm_dz VALUES (3, '2026-03-04 00:00:00+07', '2026-03-04 00:00:00+07', '2026-03-04 00:00:00+07');
SELECT id, d, t, tz FROM jm_dz ORDER BY id;
SELECT '2026-03-04 +00'::date;
SELECT '2026-03-04 -05'::date;
SELECT '2026-03-04 +05:30'::timestamptz;

-- ### h/current-user
SELECT current_user = session_user;
SELECT current_user IS NOT NULL;

-- ### h/oid-column
DROP TABLE IF EXISTS jm_blob;
CREATE TABLE jm_blob (id integer PRIMARY KEY, photo oid);
INSERT INTO jm_blob VALUES (1, 16385), (2, NULL), (3, 4000000000);
SELECT id, photo FROM jm_blob ORDER BY id;
SELECT photo FROM jm_blob WHERE id = 3;
SELECT column_name, data_type, udt_name FROM information_schema.columns WHERE table_name = 'jm_blob' ORDER BY ordinal_position;
