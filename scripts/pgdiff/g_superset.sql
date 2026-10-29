-- Group g: Apache Superset / SQLAlchemy (dialect postgresql) workload W9.
-- Reproduces the statement shapes Superset 3/4 + SQLAlchemy 1.4/2.0 + psycopg2 send:
-- (a) discovery and reflection on pg_catalog, (b) chart queries, (c) SQL Lab.
-- Data: three SIM-warehouse tables built with generate_series so results are deterministic.
-- psycopg2 interpolates %(param)s client side, so most statements are literal; the
-- "-- args:" statements model drivers that bind $n (pgx, psycopg3, asyncpg).

-- ### g/ss-setup
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS sims;
CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT NOT NULL, status TEXT NOT NULL, category TEXT, price NUMERIC(12,2) NOT NULL, note TEXT, created TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
CREATE TABLE orders (id BIGINT PRIMARY KEY, customer TEXT NOT NULL, channel TEXT NOT NULL, status TEXT NOT NULL, total NUMERIC(14,2) NOT NULL, created TIMESTAMPTZ NOT NULL);
CREATE TABLE order_items (id BIGINT PRIMARY KEY, order_id BIGINT NOT NULL, sim_number TEXT NOT NULL, qty INTEGER NOT NULL, unit_price NUMERIC(12,2) NOT NULL);
INSERT INTO sims (number, carrier, status, category, price, note, created, updated_at) SELECT '09' || lpad(g::text, 8, '0'), CASE g % 4 WHEN 0 THEN 'viettel' WHEN 1 THEN 'mobifone' WHEN 2 THEN 'vinaphone' ELSE 'vietnamobile' END, CASE (g / 4) % 5 WHEN 0 THEN 'available' WHEN 1 THEN 'sold' WHEN 2 THEN 'reserved' WHEN 3 THEN 'available' ELSE 'hold' END, CASE WHEN g % 7 = 0 THEN 'vip' WHEN g % 3 = 0 THEN 'dep' ELSE 'thuong' END, ((g * 37) % 5000) * 1000 + (g % 100) * 0.5, CASE WHEN g % 10 = 0 THEN NULL ELSE 'n' || (g % 13)::text END, TIMESTAMPTZ '2026-01-01 00:00:00+00' + (g % 200) * INTERVAL '1 day' + (g % 24) * INTERVAL '1 hour' + (g % 60) * INTERVAL '1 minute', TIMESTAMPTZ '2026-01-01 00:00:00+00' + (g % 200) * INTERVAL '1 day' + (g % 24 + g % 7) * INTERVAL '1 hour' + (g % 60) * INTERVAL '1 minute' FROM generate_series(1, 3000) AS g;
INSERT INTO orders (id, customer, channel, status, total, created) SELECT g, 'kh' || (g % 90)::text, CASE g % 3 WHEN 0 THEN 'web' WHEN 1 THEN 'crm' ELSE 'kho' END, CASE WHEN g % 11 = 0 THEN 'cancelled' WHEN g % 2 = 0 THEN 'paid' ELSE 'new' END, ((g * 53) % 900 + 100) * 10000 + (g % 10) * 0.25, TIMESTAMPTZ '2026-02-01 00:00:00+00' + (g % 150) * INTERVAL '1 day' + (g % 24) * INTERVAL '1 hour' FROM generate_series(1, 1000) AS g;
INSERT INTO order_items (id, order_id, sim_number, qty, unit_price) SELECT g, 1 + g % 1000, '09' || lpad((1 + (g * 7) % 3000)::text, 8, '0'), 1 + g % 3, ((g * 13) % 900 + 100) * 1000 + 0.5 FROM generate_series(1, 2500) AS g;
SELECT count(*) FROM sims;
SELECT count(*) FROM orders;
SELECT count(*) FROM order_items;
CREATE INDEX sims_carrier_status_idx ON sims (carrier, status);
CREATE INDEX orders_created_idx ON orders (created);
CREATE UNIQUE INDEX orders_customer_id_uq ON orders (customer, id);
ALTER TABLE order_items ADD CONSTRAINT order_items_order_fk FOREIGN KEY (order_id) REFERENCES orders (id);
ALTER TABLE sims ADD CONSTRAINT sims_price_ck CHECK (price >= 0);
COMMENT ON TABLE sims IS 'SIM warehouse';
ANALYZE sims;

-- ### g/ss-connect
-- SQLAlchemy first-connect checks (dialect.initialize) and driver handshakes.
select pg_catalog.version();
SELECT version();
show standard_conforming_strings;
select current_schema();
select pg_catalog.current_schema();
show transaction isolation level;
SHOW server_version;
SHOW server_version_num;
SELECT current_database();
SELECT current_user;
SELECT CAST('test plain returns' AS VARCHAR(60)) AS anon_1;
SELECT CAST('test unicode returns' AS VARCHAR(60)) AS anon_1;
SELECT 1;
SELECT 1 AS test;
SET search_path TO public;
SHOW search_path;
SET TIME ZONE 'UTC';
SHOW TimeZone;
SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED;
SET statement_timeout = 30000;
SHOW statement_timeout;
RESET statement_timeout;
SELECT current_setting('server_version_num')::int >= 90600;
SELECT pg_catalog.pg_is_in_recovery();
SELECT pg_catalog.pg_backend_pid() > 0;
SHOW client_encoding;
SELECT pg_catalog.pg_encoding_to_char(encoding) FROM pg_catalog.pg_database WHERE datname = current_database();

-- ### g/ss-reflect-tables
-- Inspector: schema, table, view names and has_table.
SELECT pg_catalog.pg_namespace.nspname FROM pg_catalog.pg_namespace WHERE pg_catalog.pg_namespace.nspname != 'information_schema' AND pg_catalog.pg_namespace.nspname NOT LIKE 'pg_%' ORDER BY pg_catalog.pg_namespace.nspname;
SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' ORDER BY nspname;
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' ORDER BY pg_catalog.pg_class.relname;
SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY c.relname;
SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('v', 'm') ORDER BY c.relname;
SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind = 'f' ORDER BY c.relname;
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relname = 'sims' AND pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p', 'f', 'v', 'm']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog';
select relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where pg_catalog.pg_table_is_visible(c.oid) and relname='sims';
select relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where pg_catalog.pg_table_is_visible(c.oid) and relname='no_such_table';
-- args: sims
select relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where pg_catalog.pg_table_is_visible(c.oid) and relname=$1;
-- note: raw oid values differ between servers by design; only the shape matters
SELECT c.oid FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE (pg_catalog.pg_table_is_visible(c.oid)) AND c.relname = 'sims' AND c.relkind in ('r', 'v', 'm', 'f', 'p');
SELECT c.oid IS NOT NULL AS has_oid, c.relkind, n.nspname FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE (pg_catalog.pg_table_is_visible(c.oid)) AND c.relname = 'sims' AND c.relkind in ('r', 'v', 'm', 'f', 'p');
SELECT c.relname, c.relkind, c.relispartition FROM pg_catalog.pg_class c WHERE c.relname = 'sims';
SELECT c.relname, c.relkind FROM pg_catalog.pg_class c WHERE c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = 'public') AND c.relkind IN ('r', 'v', 'm', 'f', 'p') AND c.relname IN ('sims', 'orders', 'order_items') ORDER BY 1;
SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name;
SELECT table_schema, table_name, table_type FROM information_schema.tables WHERE table_name = 'sims';
SELECT schema_name FROM information_schema.schemata ORDER BY schema_name;
SELECT has_table_privilege('sims', 'SELECT');
SELECT has_schema_privilege('public', 'USAGE');
SELECT pg_catalog.pg_get_userbyid(c.relowner) FROM pg_catalog.pg_class c WHERE c.relname = 'sims';
SELECT pg_catalog.pg_table_is_visible('sims'::regclass);
SELECT 'sims'::regclass::oid IS NOT NULL;
SELECT 'public.sims'::regclass::text;
SELECT to_regclass('sims')::text;
SELECT to_regclass('nope')::text;

-- ### g/ss-reflect-columns
-- get_columns, SQLAlchemy 1.4 and 2.0 shapes; format_type, pg_get_expr, comments, identity.
SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid) FROM pg_catalog.pg_attrdef d WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef) AS DEFAULT, a.attnotnull, a.attrelid AS table_oid, pgd.description AS comment FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_description pgd ON (pgd.objoid = a.attrelid AND pgd.objsubid = a.attnum) WHERE a.attrelid = (SELECT c.oid FROM pg_catalog.pg_class c WHERE c.relname = 'sims' AND pg_catalog.pg_table_is_visible(c.oid)) AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT pg_catalog.pg_class.relname, pg_catalog.pg_attribute.attname, pg_catalog.format_type(pg_catalog.pg_attribute.atttypid, pg_catalog.pg_attribute.atttypmod) AS format_type, (SELECT pg_catalog.pg_get_expr(pg_catalog.pg_attrdef.adbin, pg_catalog.pg_attrdef.adrelid) AS pg_get_expr_1 FROM pg_catalog.pg_attrdef WHERE pg_catalog.pg_attrdef.adrelid = pg_catalog.pg_attribute.attrelid AND pg_catalog.pg_attrdef.adnum = pg_catalog.pg_attribute.attnum AND pg_catalog.pg_attribute.atthasdef) AS "default", pg_catalog.pg_attribute.attnotnull AS not_null, pg_catalog.pg_class.relname AS table_name, pg_catalog.pg_description.description AS comment, pg_catalog.pg_attribute.attgenerated AS generated FROM pg_catalog.pg_class LEFT OUTER JOIN pg_catalog.pg_attribute ON pg_catalog.pg_class.oid = pg_catalog.pg_attribute.attrelid AND pg_catalog.pg_attribute.attnum > 0 AND NOT pg_catalog.pg_attribute.attisdropped LEFT OUTER JOIN pg_catalog.pg_description ON pg_catalog.pg_description.objoid = pg_catalog.pg_attribute.attrelid AND pg_catalog.pg_description.objsubid = pg_catalog.pg_attribute.attnum JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p', 'f', 'v', 'm']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' AND pg_catalog.pg_class.relname IN ('sims', 'orders', 'order_items') ORDER BY pg_catalog.pg_class.relname, pg_catalog.pg_attribute.attnum;
SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod) AS ftype, a.attnotnull, a.atthasdef, a.attidentity, a.attgenerated FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'order_items'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT a.attname, a.atttypid, a.atttypmod, a.attlen, a.attnum, a.attndims FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'sims'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT t.typname, t.typcategory, t.typlen, t.typtype FROM pg_catalog.pg_type t WHERE t.oid IN (SELECT a.atttypid FROM pg_attribute a WHERE a.attrelid = 'sims'::regclass AND a.attnum > 0) ORDER BY t.typname;
SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid) FROM pg_catalog.pg_attrdef d WHERE d.adrelid = 'sims'::regclass ORDER BY d.adnum;
SELECT t.typname AS "name", pg_catalog.format_type(t.typbasetype, t.typtypmod) AS "attype", NOT t.typnotnull AS "nullable", t.typdefault AS "default", pg_catalog.pg_type_is_visible(t.oid) AS "visible", n.nspname AS "schema" FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace WHERE t.typtype = 'd' ORDER BY n.nspname, t.typname;
SELECT t.typname AS "name", pg_catalog.pg_type_is_visible(t.oid) AS "visible", n.nspname AS "schema", e.enumlabel AS "label" FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace LEFT JOIN pg_catalog.pg_enum e ON t.oid = e.enumtypid WHERE t.typtype = 'e' ORDER BY "schema", "name", e.oid;
SELECT column_name, data_type, is_nullable, column_default, character_maximum_length, numeric_precision, numeric_scale FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'sims' ORDER BY ordinal_position;
SELECT column_name, data_type, udt_name FROM information_schema.columns WHERE table_name = 'orders' ORDER BY ordinal_position;
SELECT pg_catalog.pg_description.description FROM pg_catalog.pg_description WHERE pg_catalog.pg_description.objoid = 'sims'::regclass AND pg_catalog.pg_description.objsubid = 0;
SELECT n.nspname, c.relname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod) FROM pg_attribute a JOIN pg_class c ON a.attrelid = c.oid JOIN pg_namespace n ON c.relnamespace = n.oid WHERE c.relname = 'sims' AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT pg_catalog.format_type(1700, 1245188);
SELECT pg_catalog.format_type(1184, NULL), pg_catalog.format_type(25, NULL), pg_catalog.format_type(23, NULL), pg_catalog.format_type(1043, 36);

-- ### g/ss-reflect-keys
-- get_pk_constraint, get_foreign_keys, get_indexes, get_unique_constraints, get_check_constraints.
SELECT a.attname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_index i ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey) WHERE i.indrelid = 'sims'::regclass AND i.indisprimary ORDER BY a.attnum;
SELECT a.attname, a.attnum, c.conname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_constraint c ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey) WHERE c.contype = 'p' AND a.attrelid = 'sims'::regclass ORDER BY a.attnum;
SELECT pg_catalog.pg_attribute.attname AS cols, pg_catalog.pg_constraint.conname AS name, pg_catalog.pg_constraint.conkey AS colnums FROM pg_catalog.pg_attribute JOIN pg_catalog.pg_constraint ON pg_catalog.pg_attribute.attrelid = pg_catalog.pg_constraint.conrelid WHERE pg_catalog.pg_attribute.attrelid = 'orders'::regclass AND pg_catalog.pg_constraint.contype = 'p' AND pg_catalog.pg_attribute.attnum = ANY (pg_catalog.pg_constraint.conkey) ORDER BY pg_catalog.pg_attribute.attnum;
SELECT conname, pg_catalog.pg_get_constraintdef(oid, true) FROM pg_catalog.pg_constraint WHERE conrelid = 'sims'::regclass AND contype = 'p';
SELECT r.conname, pg_catalog.pg_get_constraintdef(r.oid, true) AS condef, n.nspname AS conschema FROM pg_catalog.pg_constraint r, pg_namespace n, pg_class c WHERE r.conrelid = 'order_items'::regclass AND r.contype = 'f' AND c.oid = confrelid AND n.oid = c.relnamespace ORDER BY 1;
SELECT con.conname, con.confupdtype, con.confdeltype, con.confmatchtype, con.condeferrable, con.condeferred FROM pg_catalog.pg_constraint con WHERE con.conrelid = 'order_items'::regclass AND con.contype = 'f';
SELECT c.conname, c.contype, c.conrelid::regclass::text, c.confrelid::regclass::text FROM pg_catalog.pg_constraint c WHERE c.conrelid IN ('sims'::regclass, 'orders'::regclass, 'order_items'::regclass) ORDER BY c.conname;
SELECT i.relname AS relname, ix.indisunique, ix.indexprs, a.attname, a.attnum, c.conrelid IS NOT NULL AS has_con, ix.indkey::varchar, ix.indoption::varchar, i.reloptions, am.amname, pg_get_expr(ix.indpred, ix.indrelid), ix.indnkeyatts AS indnkeyatts FROM pg_class t JOIN pg_index ix ON t.oid = ix.indrelid JOIN pg_class i ON i.oid = ix.indexrelid LEFT OUTER JOIN pg_attribute a ON t.oid = a.attrelid AND a.attnum = ANY (ix.indkey) LEFT OUTER JOIN pg_constraint c ON (ix.indrelid = c.conrelid AND ix.indexrelid = c.conindid AND c.contype IN ('p', 'u', 'x')) LEFT OUTER JOIN pg_am am ON i.relam = am.oid WHERE t.relkind IN ('r', 'v', 'f', 'm', 'p') AND t.oid = 'sims'::regclass AND ix.indisprimary = 'f' ORDER BY t.relname, i.relname, a.attnum;
SELECT i.relname, ix.indisunique, ix.indisprimary, pg_catalog.pg_get_indexdef(ix.indexrelid) FROM pg_catalog.pg_index ix JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid WHERE ix.indrelid = 'sims'::regclass ORDER BY i.relname;
SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'sims' ORDER BY indexname;
SELECT schemaname, tablename, indexname FROM pg_catalog.pg_indexes WHERE schemaname = 'public' ORDER BY indexname;
SELECT conname, ARRAY(SELECT attname FROM pg_attribute WHERE attnum = ANY (conkey) AND attrelid = conrelid ORDER BY attnum) AS cols FROM pg_catalog.pg_constraint WHERE conrelid = 'orders'::regclass AND contype = 'u';
SELECT pg_catalog.pg_constraint.conname AS name, pg_catalog.pg_get_constraintdef(pg_catalog.pg_constraint.oid, true) AS src FROM pg_catalog.pg_constraint WHERE pg_catalog.pg_constraint.conrelid = 'sims'::regclass AND pg_catalog.pg_constraint.contype = 'c' ORDER BY pg_catalog.pg_constraint.conname;
SELECT * FROM pg_catalog.pg_stats WHERE tablename = 'sims' AND attname = 'carrier';
SELECT relname, reltuples::bigint, relpages FROM pg_class WHERE relname = 'sims';
SELECT n_live_tup FROM pg_stat_user_tables WHERE relname = 'sims';
SELECT pg_total_relation_size('sims') > 0;
SELECT pg_relation_size('sims') > 0;

-- ### g/ss-chart-basic
-- Chart queries: bar / pie / big number / table, with Superset aliasing and LIMIT 50000.
SELECT carrier AS carrier, count(*) AS count FROM sims GROUP BY carrier ORDER BY count DESC, carrier LIMIT 50000;
SELECT carrier AS carrier, COUNT(*) AS "COUNT(*)" FROM sims GROUP BY carrier ORDER BY "COUNT(*)" DESC, carrier LIMIT 50000;
SELECT status AS status, COUNT(*) AS "COUNT(*)" FROM sims WHERE carrier = 'viettel' GROUP BY status ORDER BY "COUNT(*)" DESC, status LIMIT 50000;
SELECT COUNT(*) AS "COUNT(*)" FROM sims LIMIT 50000;
SELECT COUNT(DISTINCT carrier) AS "COUNT_DISTINCT(carrier)" FROM sims LIMIT 50000;
SELECT COUNT(DISTINCT number) AS "COUNT_DISTINCT(number)" FROM sims LIMIT 50000;
SELECT SUM(price) AS "SUM(price)" FROM sims LIMIT 50000;
SELECT AVG(price) AS "AVG(price)" FROM sims LIMIT 50000;
SELECT MIN(price) AS "MIN(price)", MAX(price) AS "MAX(price)" FROM sims LIMIT 50000;
SELECT carrier AS carrier, SUM(price) AS "SUM(price)", AVG(price) AS "AVG(price)", MIN(price) AS "MIN(price)", MAX(price) AS "MAX(price)" FROM sims GROUP BY carrier ORDER BY "SUM(price)" DESC, carrier LIMIT 50000;
SELECT carrier, status, COUNT(*) AS cnt FROM sims GROUP BY carrier, status ORDER BY carrier, status LIMIT 50000;
SELECT category AS category, SUM(price) AS "SUM(price)" FROM sims GROUP BY category ORDER BY "SUM(price)" DESC NULLS LAST, category LIMIT 50000;
SELECT * FROM sims ORDER BY number LIMIT 100;
SELECT number AS number, carrier AS carrier, status AS status, price AS price FROM sims ORDER BY price DESC, number LIMIT 1000;
SELECT number, price FROM sims ORDER BY price DESC, number LIMIT 25 OFFSET 50;
SELECT number, price FROM sims ORDER BY number LIMIT 10 OFFSET 2990;
SELECT carrier, MIN(created) AS first_created, MAX(updated_at) AS last_update FROM sims GROUP BY carrier ORDER BY carrier;
SELECT ROUND(AVG(price), 2) AS avg_price, ROUND(SUM(price) / COUNT(*), 2) AS avg2, ROUND(price, 0) AS rp FROM sims WHERE number = '0900000007' GROUP BY price;
SELECT carrier, ROUND(AVG(price), 2) AS "ROUND(AVG(price))", ROUND(STDDEV_SAMP(price), 2) AS sd FROM sims GROUP BY carrier ORDER BY carrier;
SELECT SUM(price) / NULLIF(COUNT(*), 0) AS ratio FROM sims WHERE carrier = 'nope';
SELECT carrier, SUM(price) / NULLIF(SUM(CASE WHEN status = 'sold' THEN price END), 0) AS ratio FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, SUM(CASE WHEN status = 'sold' THEN 1 ELSE 0 END) AS sold, SUM(CASE WHEN status = 'available' THEN 1 ELSE 0 END) AS available FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, COUNT(CASE WHEN status = 'sold' THEN 1 END) AS sold, COUNT(*) FILTER (WHERE status = 'available') AS available FROM sims GROUP BY carrier ORDER BY carrier;
SELECT COALESCE(note, 'unknown') AS note, COUNT(*) AS cnt FROM sims GROUP BY COALESCE(note, 'unknown') ORDER BY cnt DESC, note LIMIT 50000;
SELECT CASE WHEN price < 1000000 THEN 'low' WHEN price < 3000000 THEN 'mid' ELSE 'high' END AS bucket, COUNT(*) AS cnt FROM sims GROUP BY CASE WHEN price < 1000000 THEN 'low' WHEN price < 3000000 THEN 'mid' ELSE 'high' END ORDER BY bucket;
SELECT CAST(price AS BIGINT) AS p, COUNT(*) FROM sims GROUP BY CAST(price AS BIGINT) ORDER BY 2 DESC, 1 LIMIT 5;
SELECT price::int AS p, price::text AS ptext, price::float8 AS pf FROM sims WHERE number = '0900000003';
SELECT price * 1.1 AS up, price + 0.5 AS plus, price / 3 AS third, price % 7 AS md FROM sims WHERE number = '0900000003';
SELECT SUM(price * 1.1) AS s FROM sims WHERE carrier = 'viettel';
SELECT * FROM sims LIMIT 0;
SELECT * FROM sims WHERE 1 = 0;
SELECT * FROM sims WHERE (1 = 1) AND true ORDER BY number LIMIT 2;
SELECT COUNT(*) OVER () AS total, number FROM sims ORDER BY number LIMIT 2;
SELECT SUM(qty) AS q, SUM(qty * unit_price) AS rev, AVG(qty) AS aq, MAX(unit_price) AS mx FROM order_items;
SELECT order_id, SUM(qty) AS q FROM order_items GROUP BY order_id ORDER BY q DESC, order_id LIMIT 3;
SELECT COUNT(*), COUNT(note), COUNT(DISTINCT note), SUM(CASE WHEN note IS NULL THEN 1 ELSE 0 END) FROM sims;
SELECT carrier AS "carrier ""x""", COUNT(*) AS "my metric" FROM sims GROUP BY 1 ORDER BY 1 LIMIT 2;

-- ### g/ss-chart-time
-- Time-series charts: time grains of the Superset postgres engine spec.
SELECT DATE_TRUNC('day', created) AS created, COUNT(*) AS "COUNT(*)" FROM sims GROUP BY DATE_TRUNC('day', created) ORDER BY DATE_TRUNC('day', created) LIMIT 5;
SELECT DATE_TRUNC('day', created) AS __timestamp, COUNT(*) AS "COUNT(*)" FROM sims WHERE created >= TO_TIMESTAMP('2026-02-01 00:00:00.000000', 'YYYY-MM-DD HH24:MI:SS.US') AND created < TO_TIMESTAMP('2026-02-08 00:00:00.000000', 'YYYY-MM-DD HH24:MI:SS.US') GROUP BY DATE_TRUNC('day', created) ORDER BY "COUNT(*)" DESC, __timestamp LIMIT 50000;
SELECT DATE_TRUNC('week', created) AS __timestamp, COUNT(*) AS "COUNT(*)" FROM sims GROUP BY DATE_TRUNC('week', created) ORDER BY __timestamp LIMIT 50000;
SELECT DATE_TRUNC('week', created + INTERVAL '1 day') - INTERVAL '1 day' AS __timestamp, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 5;
SELECT DATE_TRUNC('week', created) + INTERVAL '5 days' AS __timestamp, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 5;
SELECT DATE_TRUNC('month', created) AS __timestamp, COUNT(*) AS "COUNT(*)", SUM(price) AS "SUM(price)" FROM sims GROUP BY DATE_TRUNC('month', created) ORDER BY __timestamp;
SELECT DATE_TRUNC('quarter', created) AS __timestamp, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT DATE_TRUNC('year', created) AS __timestamp, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT DATE_TRUNC('hour', created) AS __timestamp, COUNT(*) FROM sims WHERE created < TIMESTAMPTZ '2026-01-03 00:00:00+00' GROUP BY 1 ORDER BY 1;
SELECT DATE_TRUNC('minute', created) AS __timestamp, COUNT(*) FROM sims WHERE created < TIMESTAMPTZ '2026-01-01 03:00:00+00' GROUP BY 1 ORDER BY 1 LIMIT 10;
SELECT DATE_TRUNC('second', created) AS __timestamp, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 3;
SELECT DATE_TRUNC('month', created)::date AS m, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT DATE_TRUNC('month', created)::text AS m FROM sims WHERE number = '0900000005';
SELECT TO_CHAR(created, 'YYYY-MM-DD') AS d, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 5;
SELECT TO_CHAR(created, 'YYYY-MM') AS ym, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT TO_CHAR(created, 'YYYY-"W"IW') AS yw, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 4;
SELECT TO_CHAR(created, 'HH24') AS hr, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT TO_CHAR(created, 'Dy') AS dow, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT TO_CHAR(created, 'Mon YYYY') AS label FROM sims WHERE number = '0900000005';
SELECT TO_CHAR(price, 'FM999,999,999.00') AS pretty FROM sims WHERE number = '0900000005';
SELECT EXTRACT(epoch FROM created) * 1000 AS epoch_ms FROM sims WHERE number = '0900000005';
SELECT extract(epoch from DATE_TRUNC('day', created)) * 1000 AS __timestamp, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 3;
SELECT EXTRACT(year FROM created) AS y, EXTRACT(month FROM created) AS m, EXTRACT(dow FROM created) AS dow, EXTRACT(hour FROM created) AS h FROM sims WHERE number = '0900000005';
SELECT EXTRACT(month FROM created) AS m, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT date_part('year', created) AS y, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT CAST(created AS DATE) AS d, COUNT(*) FROM sims GROUP BY CAST(created AS DATE) ORDER BY d LIMIT 5;
SELECT created::date AS d, SUM(price) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 3;
SELECT CAST(created AS TIMESTAMP) AS ts FROM sims WHERE number = '0900000005';
SELECT created::timestamp AS ts, pg_typeof(created::timestamp)::text FROM sims WHERE number = '0900000005';
SELECT TO_TIMESTAMP('2026-03-01 10:11:12.000000', 'YYYY-MM-DD HH24:MI:SS.US');
SELECT TO_TIMESTAMP(1767225600);
SELECT TO_DATE('2026-03-01', 'YYYY-MM-DD');
SELECT TO_CHAR(TO_TIMESTAMP('2026-03-01 10:11:12', 'YYYY-MM-DD HH24:MI:SS'), 'YYYY-MM-DD HH24:MI:SS');
SELECT COUNT(*) FROM sims WHERE created BETWEEN TO_TIMESTAMP('2026-03-01 00:00:00.000000', 'YYYY-MM-DD HH24:MI:SS.US') AND TO_TIMESTAMP('2026-03-31 23:59:59.999999', 'YYYY-MM-DD HH24:MI:SS.US');
SELECT COUNT(*) FROM sims WHERE created >= '2026-03-01 00:00:00.000000' AND created < '2026-04-01 00:00:00.000000';
SELECT COUNT(*) FROM sims WHERE created >= DATE '2026-03-01' AND created < DATE '2026-03-01' + INTERVAL '1 month';
SELECT COUNT(*) FROM sims WHERE created >= TIMESTAMPTZ '2026-05-01 00:00:00+00' - INTERVAL '7 days' AND created < TIMESTAMPTZ '2026-05-01 00:00:00+00';
SELECT COUNT(*) FROM sims WHERE created > now() - INTERVAL '10 years';
SELECT COUNT(*) FROM sims WHERE updated_at > created;
SELECT DATE_TRUNC('day', created AT TIME ZONE 'Asia/Ho_Chi_Minh') AS d, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 3;
SELECT DATE_TRUNC('month', created AT TIME ZONE 'UTC') AS m, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 3;
SELECT TO_CHAR(created AT TIME ZONE 'Asia/Ho_Chi_Minh', 'YYYY-MM-DD HH24:MI') AS t FROM sims WHERE number = '0900000005';
SELECT COUNT(*) FROM sims WHERE created >= CURRENT_DATE - 100000;
SELECT COUNT(*) FROM sims WHERE created >= now() - INTERVAL '1 year' AND created < now();
SELECT (DATE '2026-03-01' + INTERVAL '1 month')::date AS d, DATE '2026-03-31' - DATE '2026-03-01' AS diff;
SELECT AVG(EXTRACT(epoch FROM (updated_at - created)) / 3600) AS avg_hours FROM sims;
SELECT carrier, MAX(updated_at - created) AS max_lag FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, DATE_TRUNC('month', created) AS m, COUNT(*) AS cnt FROM sims GROUP BY carrier, DATE_TRUNC('month', created) ORDER BY m, carrier LIMIT 12;
-- args: 2026-03-01 | 2026-04-01
SELECT DATE_TRUNC('day', created) AS d, COUNT(*) FROM sims WHERE created >= $1::timestamptz AND created < $2::timestamptz GROUP BY 1 ORDER BY 1 LIMIT 3;
-- args: 2026-03-01 00:00:00+00
SELECT COUNT(*) FROM sims WHERE created >= $1::timestamptz;
-- args: 2026-03-01 00:00:00
SELECT COUNT(*) FROM sims WHERE created >= $1;

-- ### g/ss-chart-filters
-- Dashboard filters, dropdown values, Superset WHERE/HAVING forms.
SELECT DISTINCT carrier FROM sims ORDER BY carrier LIMIT 1000;
SELECT DISTINCT carrier AS carrier FROM sims ORDER BY carrier ASC LIMIT 1000;
SELECT DISTINCT status AS status FROM sims WHERE carrier IN ('viettel') ORDER BY status LIMIT 1000;
SELECT DISTINCT category FROM sims ORDER BY category NULLS FIRST;
SELECT DISTINCT note FROM sims ORDER BY note NULLS LAST LIMIT 20;
SELECT DISTINCT ON (carrier) carrier, number FROM sims ORDER BY carrier, number;
SELECT MIN(created) AS min_dttm, MAX(created) AS max_dttm FROM sims;
SELECT MIN(price) AS min, MAX(price) AS max FROM sims;
SELECT carrier, COUNT(*) FROM sims WHERE (status = 'sold') AND (category IN ('vip', 'dep')) GROUP BY carrier ORDER BY carrier;
SELECT carrier, COUNT(*) FROM sims WHERE carrier IN ('viettel', 'mobifone') AND status NOT IN ('hold') GROUP BY carrier ORDER BY carrier;
SELECT COUNT(*) FROM sims WHERE carrier IN ('viettel', 'mobifone', 'vinaphone') AND status IN ('sold', 'reserved') AND category IN ('vip');
SELECT COUNT(*) FROM sims WHERE number ILIKE '%0123%';
SELECT COUNT(*) FROM sims WHERE carrier ILIKE '%VIET%';
SELECT COUNT(*) FROM sims WHERE number LIKE '0900001%';
SELECT COUNT(*) FROM sims WHERE number NOT ILIKE '%1%';
SELECT COUNT(*) FROM sims WHERE LOWER(carrier) LIKE '%tel%';
SELECT COUNT(*) FROM sims WHERE note IS NULL;
SELECT COUNT(*) FROM sims WHERE note IS NOT NULL AND note <> 'n3';
SELECT COUNT(*) FROM sims WHERE price BETWEEN 1000000 AND 2000000;
SELECT COUNT(*) FROM sims WHERE price >= 1000000 AND price < 2000000.5;
SELECT COUNT(*) FROM sims WHERE price > 4999999.5;
SELECT COUNT(*) FROM sims WHERE price = 12345;
SELECT COUNT(*) FROM sims WHERE price IN (1000.00, 2000.50, 5000);
SELECT carrier, SUM(price) AS s FROM sims GROUP BY carrier HAVING (SUM(price) > 1000000000) ORDER BY s DESC, carrier;
SELECT carrier, COUNT(*) AS c FROM sims GROUP BY carrier HAVING COUNT(*) > 750 ORDER BY carrier;
SELECT carrier, status, COUNT(*) AS c FROM sims GROUP BY carrier, status HAVING COUNT(*) BETWEEN 100 AND 200 ORDER BY carrier, status;
SELECT status, COUNT(*) FROM sims WHERE number = ANY (ARRAY['0900000001', '0900000002', '0900000003']) GROUP BY status ORDER BY status;
SELECT status, COUNT(*) FROM sims WHERE carrier = ANY ('{viettel,mobifone}') GROUP BY status ORDER BY status;
SELECT COUNT(*) FROM sims WHERE (carrier = 'viettel' OR (status = 'sold' AND price > 100000)) AND NOT (category = 'dep');
SELECT COUNT(*) FROM sims WHERE EXISTS (SELECT 1 FROM order_items oi WHERE oi.sim_number = sims.number);
SELECT COUNT(*) FROM sims WHERE number IN (SELECT sim_number FROM order_items WHERE qty >= 3);
SELECT COUNT(*) FROM sims WHERE price > (SELECT AVG(price) FROM sims);
-- args: viettel | 1000000
SELECT COUNT(*) FROM sims WHERE carrier = $1 AND price > $2::numeric;
-- args: viettel | mobifone | vinaphone
SELECT carrier, COUNT(*) FROM sims WHERE carrier IN ($1, $2, $3) GROUP BY carrier ORDER BY carrier;
-- args: %123%
SELECT COUNT(*) FROM sims WHERE number ILIKE $1;
-- args: sold | 50
SELECT number FROM sims WHERE status = $1 ORDER BY number LIMIT $2::int;
-- args: null
SELECT COUNT(*) FROM sims WHERE note IS NOT DISTINCT FROM $1;
/* {"dashboard_id": 1, "slice_id": 42, "channel": "ab12", "username": "admin"} */ SELECT carrier AS carrier, COUNT(*) AS "COUNT(*)" FROM sims GROUP BY carrier ORDER BY carrier LIMIT 50000;
-- {"dashboard_id": 1}
SELECT COUNT(*) FROM sims;
/* superset comment */ SELECT COUNT(*) FROM sims -- trailing comment
;

-- ### g/ss-virtual
-- Virtual datasets: subquery in FROM with the alias Superset generates.
SELECT carrier AS carrier, SUM(price) AS "SUM(price)" FROM (SELECT number, carrier, status, price FROM sims WHERE status <> 'hold') AS virtual_table GROUP BY carrier ORDER BY "SUM(price)" DESC, carrier LIMIT 50000;
SELECT * FROM (SELECT carrier, COUNT(*) AS cnt FROM sims GROUP BY carrier) AS virtual_table ORDER BY cnt DESC, carrier LIMIT 1000;
SELECT COUNT(*) AS "COUNT(*)" FROM (SELECT * FROM sims WHERE category = 'vip') AS virtual_table LIMIT 50000;
SELECT status, COUNT(*) FROM (SELECT s.number, s.carrier, s.status, o.channel FROM sims s JOIN order_items oi ON oi.sim_number = s.number JOIN orders o ON o.id = oi.order_id) AS virtual_table WHERE channel = 'web' GROUP BY status ORDER BY status;
SELECT channel, COUNT(DISTINCT customer) AS buyers, SUM(total) AS revenue FROM orders GROUP BY channel ORDER BY channel;
SELECT o.channel, s.carrier, SUM(oi.qty * oi.unit_price) AS revenue FROM orders o JOIN order_items oi ON oi.order_id = o.id JOIN sims s ON s.number = oi.sim_number GROUP BY o.channel, s.carrier ORDER BY o.channel, s.carrier;
SELECT o.channel, COUNT(*) AS lines, SUM(oi.qty) AS qty FROM orders o LEFT JOIN order_items oi ON oi.order_id = o.id GROUP BY o.channel ORDER BY o.channel;
SELECT s.carrier, COUNT(oi.id) AS lines FROM sims s LEFT JOIN order_items oi ON oi.sim_number = s.number GROUP BY s.carrier ORDER BY s.carrier;
SELECT DATE_TRUNC('month', o.created) AS m, SUM(o.total) AS revenue FROM orders o WHERE o.status <> 'cancelled' GROUP BY 1 ORDER BY 1;
SELECT status, SUM(total) AS t, AVG(total) AS a, COUNT(*) AS c FROM orders GROUP BY status ORDER BY status;
SELECT customer, SUM(total) AS t FROM orders GROUP BY customer ORDER BY t DESC, customer LIMIT 10;
SELECT carrier, 'sims' AS src, COUNT(*) AS n FROM sims GROUP BY carrier UNION ALL SELECT channel, 'orders', COUNT(*) FROM orders GROUP BY channel ORDER BY 2, 1;
SELECT x.k, SUM(x.v) FROM (SELECT carrier AS k, price AS v FROM sims UNION ALL SELECT channel AS k, total AS v FROM orders) AS x GROUP BY x.k ORDER BY x.k;
SELECT carrier FROM sims UNION SELECT channel FROM orders ORDER BY 1;
SELECT carrier FROM sims INTERSECT SELECT 'viettel' ORDER BY 1;
SELECT t.n, t.cnt FROM (SELECT COALESCE(note, 'none') AS n, COUNT(*) AS cnt FROM sims GROUP BY 1) t WHERE t.cnt > 200 ORDER BY t.cnt DESC, t.n;

-- ### g/ss-analytic
-- Window functions, percentile, top-N per group, pivot-style multi GROUP BY.
SELECT carrier, status, COUNT(*) AS cnt, RANK() OVER (PARTITION BY carrier ORDER BY COUNT(*) DESC, status) AS rk FROM sims GROUP BY carrier, status ORDER BY carrier, rk;
SELECT carrier, SUM(price) AS s, RANK() OVER (ORDER BY SUM(price) DESC) AS rk, DENSE_RANK() OVER (ORDER BY SUM(price) DESC) AS drk FROM sims GROUP BY carrier ORDER BY rk, carrier;
SELECT m, c, SUM(c) OVER (ORDER BY m) AS running FROM (SELECT DATE_TRUNC('month', created) AS m, COUNT(*) AS c FROM sims GROUP BY 1) t ORDER BY m;
SELECT carrier, m, SUM(c) OVER (PARTITION BY carrier ORDER BY m) AS running FROM (SELECT carrier, DATE_TRUNC('month', created) AS m, COUNT(*) AS c FROM sims GROUP BY 1, 2) t ORDER BY carrier, m;
SELECT m, c, LAG(c) OVER (ORDER BY m) AS prev, c - LAG(c) OVER (ORDER BY m) AS diff FROM (SELECT DATE_TRUNC('month', created) AS m, COUNT(*) AS c FROM sims GROUP BY 1) t ORDER BY m;
SELECT m, AVG(c) OVER (ORDER BY m ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) AS ma3 FROM (SELECT DATE_TRUNC('month', created) AS m, COUNT(*) AS c FROM sims GROUP BY 1) t ORDER BY m;
SELECT carrier, SUM(price) * 100.0 / SUM(SUM(price)) OVER () AS pct FROM sims GROUP BY carrier ORDER BY carrier;
SELECT number, price, ROW_NUMBER() OVER (PARTITION BY carrier ORDER BY price DESC, number) AS rn FROM sims ORDER BY carrier, rn LIMIT 8;
SELECT carrier, number, price FROM (SELECT carrier, number, price, ROW_NUMBER() OVER (PARTITION BY carrier ORDER BY price DESC, number) AS rn FROM sims) t WHERE rn <= 3 ORDER BY carrier, rn;
SELECT c.carrier, t.number, t.price FROM (SELECT DISTINCT carrier FROM sims) c CROSS JOIN LATERAL (SELECT number, price FROM sims s WHERE s.carrier = c.carrier ORDER BY price DESC, number LIMIT 3) t ORDER BY c.carrier, t.price DESC, t.number;
SELECT c.carrier, t.number FROM (SELECT DISTINCT carrier FROM sims) c LEFT JOIN LATERAL (SELECT number FROM sims s WHERE s.carrier = c.carrier AND s.status = 'sold' ORDER BY number LIMIT 2) t ON TRUE ORDER BY 1, 2;
SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY price) AS median FROM sims;
SELECT carrier, percentile_cont(0.5) WITHIN GROUP (ORDER BY price) AS median, percentile_cont(0.9) WITHIN GROUP (ORDER BY price) AS p90 FROM sims GROUP BY carrier ORDER BY carrier;
SELECT percentile_disc(0.25) WITHIN GROUP (ORDER BY price) AS q1 FROM sims;
SELECT mode() WITHIN GROUP (ORDER BY carrier) AS m FROM sims;
SELECT carrier, STDDEV(price) AS sd, VARIANCE(price) AS var, STDDEV_POP(price) AS sdp FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, CORR(price, EXTRACT(epoch FROM created)) IS NOT NULL AS has_corr FROM sims GROUP BY carrier ORDER BY carrier;
SELECT NTILE(4) OVER (ORDER BY price, number) AS q, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1;
SELECT carrier, status, category, COUNT(*) AS c, SUM(price) AS s FROM sims GROUP BY carrier, status, category ORDER BY carrier, status, category LIMIT 20;
SELECT carrier, category, SUM(CASE WHEN status = 'sold' THEN price ELSE 0 END) AS sold_amt FROM sims GROUP BY ROLLUP (carrier, category) ORDER BY carrier NULLS LAST, category NULLS LAST;
SELECT carrier, status, COUNT(*) FROM sims GROUP BY GROUPING SETS ((carrier), (status), ()) ORDER BY carrier NULLS LAST, status NULLS LAST;
SELECT carrier, array_agg(DISTINCT status ORDER BY status) AS sts FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, string_agg(DISTINCT category, ',' ORDER BY category) AS cats FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, COUNT(*) FILTER (WHERE price > 2500000) AS pricey FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, bool_or(price > 4999000) AS any_big, bool_and(price >= 0) AS all_nonneg FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, json_agg(status ORDER BY status) IS NOT NULL AS j FROM sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, FIRST_VALUE(number) OVER (PARTITION BY carrier ORDER BY created, number) AS first_num FROM sims ORDER BY carrier, first_num LIMIT 3;
SELECT generate_series AS d FROM generate_series(1, 5) ORDER BY 1;
SELECT d::date AS day FROM generate_series(TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-05 00:00:00+00', INTERVAL '1 day') AS d ORDER BY 1;
SELECT dd.day, COALESCE(c.n, 0) AS n FROM (SELECT generate_series(DATE '2026-01-01', DATE '2026-01-05', INTERVAL '1 day')::date AS day) dd LEFT JOIN (SELECT created::date AS day, COUNT(*) AS n FROM sims GROUP BY 1) c ON c.day = dd.day ORDER BY dd.day;

-- ### g/ss-sqllab
-- SQL Lab: EXPLAIN, SHOW, SET, CTE, comments, DML/DDL guards, transactions.
EXPLAIN SELECT carrier, COUNT(*) FROM sims GROUP BY carrier;
EXPLAIN SELECT * FROM sims WHERE number = '0900000001';
EXPLAIN (FORMAT JSON) SELECT COUNT(*) FROM sims;
EXPLAIN ANALYZE SELECT COUNT(*) FROM sims;
SET statement_timeout = '60s';
SHOW statement_timeout;
SET LOCAL statement_timeout = 1000;
SET work_mem = '64MB';
SHOW work_mem;
SET search_path TO public, pg_catalog;
SHOW search_path;
SHOW DateStyle;
SHOW max_connections;
WITH by_carrier AS (SELECT carrier, SUM(price) AS s FROM sims GROUP BY carrier) SELECT carrier, s FROM by_carrier ORDER BY s DESC, carrier;
WITH a AS (SELECT carrier, COUNT(*) AS n FROM sims GROUP BY carrier), b AS (SELECT carrier, AVG(price) AS p FROM sims GROUP BY carrier) SELECT a.carrier, a.n, ROUND(b.p, 2) AS avg_price FROM a JOIN b ON b.carrier = a.carrier ORDER BY a.carrier;
WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 10) SELECT SUM(n) FROM t;
WITH c AS MATERIALIZED (SELECT * FROM sims WHERE status = 'sold') SELECT COUNT(*) FROM c;
SELECT * FROM sims WHERE number = '0900000001';
SELECT 'a' AS x, 1 AS y, NULL AS z, 1.50 AS w, TRUE AS b, now() IS NOT NULL AS c;
SELECT 1 AS a, 2 AS a;
SELECT 1/0;
SELECT nosuchcol FROM sims;
SELECT * FROM nosuchtable;
SELECT carrier, COUNT(*) FROM sims;
SELECT price::int FROM sims WHERE number = 'abc' || price::text;
SELECT count(*) FROM (SELECT * FROM sims LIMIT 10) AS inner_qry;
SELECT * FROM (SELECT * FROM sims ORDER BY number) AS inner_qry LIMIT 3;
SELECT number FROM sims ORDER BY number LIMIT 1001;
BEGIN;
SELECT COUNT(*) FROM sims;
ROLLBACK;
BEGIN READ ONLY;
SELECT COUNT(*) FROM orders;
COMMIT;
CREATE TEMP TABLE tmp_report AS SELECT carrier, COUNT(*) AS n FROM sims GROUP BY carrier;
SELECT * FROM tmp_report ORDER BY carrier;
CREATE TABLE report_snapshot AS SELECT carrier, COUNT(*) AS n FROM sims GROUP BY carrier;
SELECT * FROM report_snapshot ORDER BY carrier;
CREATE VIEW v_sold AS SELECT number, carrier, price FROM sims WHERE status = 'sold';
SELECT carrier, COUNT(*) FROM v_sold GROUP BY carrier ORDER BY carrier;
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind IN ('v', 'm') AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' ORDER BY 1;
SELECT definition FROM pg_catalog.pg_views WHERE viewname = 'v_sold';
SELECT pg_get_viewdef('v_sold'::regclass, true);
DROP VIEW v_sold;
DROP TABLE report_snapshot;
DROP TABLE IF EXISTS tmp_report;
SELECT pg_sleep(0.05) IS NULL;
SELECT set_config('statement_timeout', '5000', false);
SELECT current_setting('statement_timeout');
SELECT pid, state, query FROM pg_stat_activity WHERE pid = pg_backend_pid();
SELECT datname FROM pg_database ORDER BY datname;
SELECT rolname FROM pg_roles WHERE rolname = current_user;
SELECT usename FROM pg_user WHERE usename = current_user;
SELECT has_database_privilege(current_database(), 'CONNECT');

-- ### g/ss-concurrent-a
-- Session B of the pair reads while this one holds an open transaction (report vs order flow).
BEGIN;
INSERT INTO orders (id, customer, channel, status, total, created) VALUES (9001, 'khx', 'web', 'new', 1000000.50, TIMESTAMPTZ '2026-06-01 10:00:00+00');
SELECT COUNT(*) FROM orders;
SELECT SUM(total) FROM orders WHERE id = 9001;
ROLLBACK;
SELECT COUNT(*) FROM orders;

-- ### g/ss-concurrent-b
SELECT COUNT(*) FROM orders;
SELECT channel, COUNT(*) FROM orders GROUP BY channel ORDER BY channel;
SELECT COUNT(*) FROM orders WHERE id = 9001;

-- ### g/ss-cleanup
-- drop: order_items, orders, sims
SELECT 1;
