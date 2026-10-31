-- Suite group 13: catalog and introspection, the queries drivers (pgjdbc, pgx, psycopg, Npgsql) and
-- ORMs (Hibernate, SQLAlchemy, EF Core) and tools (psql \d, pg_dump, DBeaver) run. Object OIDs differ
-- between servers, so only names, types and flags are selected. EXPLAIN checks the output shape only
-- (a single column named "QUERY PLAN" with text rows); plan text is not compared.

-- ### 13_catalog/setup
DROP SCHEMA IF EXISTS s13 CASCADE;
CREATE SCHEMA s13;
SET search_path = s13;
CREATE TYPE color AS ENUM ('red', 'green');
CREATE TABLE parent (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, code varchar(20) NOT NULL UNIQUE, price numeric(10,2) DEFAULT 1.50, tags text[], c color, created timestamptz DEFAULT now(), j jsonb);
CREATE TABLE child (id serial PRIMARY KEY, pid bigint NOT NULL REFERENCES parent(id) ON DELETE CASCADE, note text, qty int CHECK (qty > 0));
CREATE INDEX child_pid_idx ON child (pid);
CREATE INDEX child_note_part ON child (note) WHERE qty > 5;
CREATE VIEW pv AS SELECT id, code FROM parent;
CREATE SEQUENCE extra_seq;
COMMENT ON TABLE parent IS 'parent table';
COMMENT ON COLUMN parent.code IS 'the code';

-- ### 13_catalog/information-schema
SET search_path = s13;
SELECT table_name::text, table_type::text FROM information_schema.tables WHERE table_schema = 's13' ORDER BY table_name;
SELECT column_name::text, ordinal_position, column_default, is_nullable::text, data_type::text, character_maximum_length, numeric_precision, numeric_scale, udt_name::text, is_identity::text FROM information_schema.columns WHERE table_schema = 's13' AND table_name = 'parent' ORDER BY ordinal_position;
SELECT column_name::text, data_type::text, is_nullable::text, column_default FROM information_schema.columns WHERE table_schema = 's13' AND table_name = 'child' ORDER BY ordinal_position;
SELECT table_name::text, column_name::text FROM information_schema.columns WHERE table_schema = 's13' AND table_name = 'pv' ORDER BY ordinal_position;
SELECT constraint_name::text, constraint_type::text, table_name::text FROM information_schema.table_constraints WHERE table_schema = 's13' AND constraint_name NOT LIKE '%not_null' ORDER BY table_name, constraint_type, constraint_name;
SELECT tc.constraint_name::text, kcu.column_name::text, kcu.ordinal_position FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema WHERE tc.table_schema = 's13' AND tc.constraint_type = 'PRIMARY KEY' ORDER BY 1, 3;
SELECT tc.table_name::text, kcu.column_name::text, ccu.table_name::text AS ref_table, ccu.column_name::text AS ref_col, rc.delete_rule::text FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON kcu.constraint_name = tc.constraint_name AND kcu.constraint_schema = tc.constraint_schema JOIN information_schema.constraint_column_usage ccu ON ccu.constraint_name = tc.constraint_name AND ccu.constraint_schema = tc.constraint_schema JOIN information_schema.referential_constraints rc ON rc.constraint_name = tc.constraint_name AND rc.constraint_schema = tc.constraint_schema WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = 's13';
SELECT constraint_name::text, check_clause FROM information_schema.check_constraints WHERE constraint_schema = 's13' AND check_clause LIKE '%qty%' ORDER BY 1;
SELECT schema_name::text FROM information_schema.schemata WHERE schema_name IN ('s13', 'public', 'pg_catalog', 'information_schema') ORDER BY 1;
SELECT sequence_name::text, data_type::text, start_value, increment FROM information_schema.sequences WHERE sequence_schema = 's13' ORDER BY 1;
SELECT table_name::text FROM information_schema.views WHERE table_schema = 's13';
SELECT table_name::text, privilege_type::text FROM information_schema.table_privileges WHERE table_schema = 's13' AND grantee = current_user AND privilege_type = 'SELECT' ORDER BY 1;
SELECT count(*) > 0 FROM information_schema.columns WHERE table_schema = 'pg_catalog';
SELECT routine_name::text FROM information_schema.routines WHERE routine_schema = 's13';
SELECT domain_name::text FROM information_schema.domains WHERE domain_schema = 's13';
SELECT data_type::text FROM information_schema.columns WHERE table_schema = 's13' AND column_name = 'c';
SELECT current_schema(), current_schemas(false), current_catalog IS NOT NULL;

-- ### 13_catalog/pg-catalog
SET search_path = s13;
SELECT c.relname::text, c.relkind::text, c.relhasindex, c.relispartition, c.relpersistence::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 's13' ORDER BY c.relname;
SELECT a.attname::text, a.attnum, format_type(a.atttypid, a.atttypmod), a.attnotnull, a.atthasdef, a.attidentity::text, a.attgenerated::text FROM pg_attribute a WHERE a.attrelid = 's13.parent'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT pg_get_expr(d.adbin, d.adrelid) FROM pg_attrdef d WHERE d.adrelid = 's13.parent'::regclass ORDER BY d.adnum;
SELECT conname::text, contype::text, pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace = 's13'::regnamespace ORDER BY conname;
SELECT indexrelid::regclass::text, indisunique, indisprimary, pg_get_indexdef(indexrelid) FROM pg_index WHERE indrelid = 's13.child'::regclass ORDER BY 1;
SELECT i.relname::text, a.attname::text, ix.indisprimary FROM pg_index ix JOIN pg_class t ON t.oid = ix.indrelid JOIN pg_class i ON i.oid = ix.indexrelid JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY (ix.indkey) WHERE t.relname = 'parent' AND t.relnamespace = 's13'::regnamespace ORDER BY i.relname, a.attname;
SELECT typname::text, typtype::text, typcategory::text FROM pg_type WHERE typnamespace = 's13'::regnamespace AND typtype <> 'c' ORDER BY typname;
SELECT enumlabel::text, enumsortorder FROM pg_enum WHERE enumtypid = 's13.color'::regtype ORDER BY enumsortorder;
SELECT typname::text, typlen, typbyval, typcategory::text FROM pg_type WHERE typname IN ('int2', 'int4', 'int8', 'text', 'bool', 'numeric', 'float8', 'uuid', 'jsonb', 'timestamptz', 'bytea', 'varchar') ORDER BY typname;
SELECT oid::int, typname::text FROM pg_type WHERE typname IN ('bool', 'int8', 'int2', 'int4', 'text', 'float4', 'float8', 'varchar', 'date', 'timestamp', 'timestamptz', 'numeric', 'uuid', 'jsonb', 'json', 'bytea', 'interval', 'time') ORDER BY oid;
SELECT nspname::text FROM pg_namespace WHERE nspname IN ('public', 'pg_catalog', 'information_schema', 's13') ORDER BY 1;
SELECT relname::text, obj_description(oid, 'pg_class') FROM pg_class WHERE relnamespace = 's13'::regnamespace AND obj_description(oid, 'pg_class') IS NOT NULL;
SELECT attname::text, col_description('s13.parent'::regclass, attnum) FROM pg_attribute WHERE attrelid = 's13.parent'::regclass AND attnum > 0 AND col_description('s13.parent'::regclass, attnum) IS NOT NULL;
SELECT pg_get_viewdef('s13.pv'::regclass), pg_get_viewdef('s13.pv'::regclass, true);
SELECT pg_get_serial_sequence('s13.child', 'id'), pg_get_serial_sequence('s13.parent', 'id');
SELECT 's13.parent'::regclass::text, 's13.parent'::regclass::oid IS NOT NULL, 'int4'::regtype::text, 'integer'::regtype::oid::int, 'pg_catalog.text'::regtype::text, to_regclass('s13.nosuch') IS NULL, to_regclass('s13.child')::text, to_regtype('int8')::text, 'pg_class'::regclass::text;
SELECT 's13.nosuch'::regclass;
SELECT format_type(23, NULL), format_type(1043, 24), format_type(1700, 655366), format_type(25, NULL), format_type(1184, NULL), format_type(1007, NULL);
SELECT amname::text, amtype::text FROM pg_am WHERE amname IN ('btree', 'hash', 'gin', 'gist', 'brin') ORDER BY 1;
SELECT rolname::text, rolsuper, rolcanlogin FROM pg_roles WHERE rolname = current_user;
SELECT usename::text FROM pg_user WHERE usename = current_user;
SELECT pg_encoding_to_char(encoding)::text FROM pg_database WHERE datname = current_database();
SELECT schemaname::text, tablename::text, tableowner IS NOT NULL, hasindexes FROM pg_tables WHERE schemaname = 's13' ORDER BY tablename;
SELECT schemaname::text, viewname::text FROM pg_views WHERE schemaname = 's13';
SELECT schemaname::text, sequencename::text, data_type::text FROM pg_sequences WHERE schemaname = 's13' ORDER BY 2;
SELECT schemaname::text, tablename::text, indexname::text FROM pg_indexes WHERE schemaname = 's13' ORDER BY 3;
SELECT name::text FROM pg_settings WHERE name IN ('server_version_num', 'max_connections', 'standard_conforming_strings', 'client_encoding', 'integer_datetimes', 'TimeZone') ORDER BY 1;
SELECT setting FROM pg_settings WHERE name IN ('standard_conforming_strings', 'client_encoding', 'integer_datetimes') ORDER BY name;
SELECT count(*) > 0 FROM pg_proc WHERE proname IN ('now', 'count', 'lower', 'date_trunc');
SELECT proname::text, prokind::text FROM pg_proc WHERE proname IN ('count', 'lower', 'rank') AND pronamespace = 'pg_catalog'::regnamespace ORDER BY proname, prokind;
SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE pid = pg_backend_pid();
SELECT pg_backend_pid() > 0, pg_postmaster_start_time() IS NOT NULL, pg_is_in_recovery(), current_setting('transaction_isolation');
SELECT has_table_privilege('s13.parent', 'SELECT'), has_schema_privilege('s13', 'USAGE'), has_database_privilege(current_database(), 'CONNECT'), has_column_privilege('s13.parent', 'code', 'SELECT');
SELECT pg_table_is_visible('s13.parent'::regclass), pg_type_is_visible('s13.color'::regtype);
SELECT pg_relation_size('s13.parent') >= 0, pg_total_relation_size('s13.parent') >= 0, pg_size_pretty(1048576::bigint), pg_size_pretty(1536::bigint), pg_database_size(current_database()) > 0;
SELECT pg_typeof(1)::text, pg_typeof(now())::text, pg_typeof(ARRAY[1])::text, pg_typeof(1::oid)::text;
SELECT version() ~ '^PostgreSQL 16', current_setting('server_version') ~ '^16', current_setting('server_version_num')::int / 10000;
SELECT set_config('app.x', 'v', false), current_setting('app.x'), current_setting('app.nosuch', true);
SELECT current_setting('nosuch');
SELECT pg_get_userbyid(10) IS NOT NULL, pg_encoding_to_char(6), pg_catalog.pg_get_keywords() IS NOT NULL LIMIT 1;
SHOW server_version_num;
SHOW client_encoding;
SHOW standard_conforming_strings;
SHOW search_path;
SHOW all_nonexistent;
SET search_path TO s13, public;
SHOW search_path;
SELECT count(*) FROM parent;
RESET search_path;

-- ### 13_catalog/driver-queries
SET search_path = s13;
-- pgjdbc getTables / getColumns / getPrimaryKeys / getIndexInfo style
SELECT NULL::text AS table_cat, n.nspname::text AS table_schem, c.relname::text AS table_name, CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'v' THEN 'VIEW' WHEN 'S' THEN 'SEQUENCE' WHEN 'i' THEN 'INDEX' WHEN 'p' THEN 'PARTITIONED TABLE' ELSE 'OTHER' END AS table_type, d.description FROM pg_catalog.pg_namespace n, pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_description d ON (c.oid = d.objoid AND d.objsubid = 0 AND d.classoid = 'pg_class'::regclass) WHERE c.relnamespace = n.oid AND n.nspname LIKE 's13' AND c.relkind IN ('r', 'p', 'v') ORDER BY table_type, table_schem, table_name;
SELECT a.attname::text, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnum, a.attnotnull, a.atthasdef FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON a.attrelid = c.oid JOIN pg_catalog.pg_namespace n ON c.relnamespace = n.oid WHERE n.nspname = 's13' AND c.relname = 'child' AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT a.attname::text, a.attnum, ct.relname::text AS pk_name FROM pg_catalog.pg_class ct JOIN pg_catalog.pg_attribute a ON (ct.oid = a.attrelid) JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid) JOIN (SELECT i.indexrelid, i.indrelid, i.indisprimary, information_schema._pg_expandarray(i.indkey) AS keys FROM pg_catalog.pg_index i) i ON (a.attnum = (i.keys).x AND a.attrelid = i.indrelid) WHERE ct.relname = 'child' AND n.nspname = 's13' AND i.indisprimary ORDER BY a.attnum;
SELECT t.typname::text, t.oid::int, t.typtype::text, t.typelem::int, t.typarray::int FROM pg_catalog.pg_type t WHERE t.typname IN ('int4', '_int4', 'text', '_text') ORDER BY 1;
SELECT pg_catalog.pg_get_userbyid(c.relowner) IS NOT NULL FROM pg_class c WHERE c.oid = 's13.parent'::regclass;
-- psql \dt, \d table, \dn style
SELECT n.nspname::text AS "Schema", c.relname::text AS "Name", CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' WHEN 'p' THEN 'partitioned table' END AS "Type" FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', '') AND n.nspname <> 'pg_catalog' AND n.nspname !~ '^pg_toast' AND n.nspname <> 'information_schema' AND pg_catalog.pg_table_is_visible(c.oid) AND n.nspname = 's13' ORDER BY 1, 2;
SELECT c.oid::int > 0, c.relchecks, c.relkind::text, c.relhasindex, c.relhasrules, c.relhastriggers, c.relrowsecurity, c.relforcerowsecurity, c.relispartition FROM pg_catalog.pg_class c WHERE c.oid = 's13.parent'::regclass;
SELECT a.attname::text, pg_catalog.format_type(a.atttypid, a.atttypmod), (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid, true) FROM pg_catalog.pg_attrdef d WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef), a.attnotnull, a.attcollation <> 0, a.attidentity::text, a.attgenerated::text FROM pg_catalog.pg_attribute a WHERE a.attrelid = 's13.parent'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT c2.relname::text, i.indisprimary, i.indisunique, pg_catalog.pg_get_indexdef(i.indexrelid, 0, true), pg_catalog.pg_get_constraintdef(con.oid, true), contype::text FROM pg_catalog.pg_class c, pg_catalog.pg_class c2, pg_catalog.pg_index i LEFT JOIN pg_catalog.pg_constraint con ON (conrelid = i.indrelid AND conindid = i.indexrelid AND contype IN ('p', 'u', 'x')) WHERE c.oid = 's13.parent'::regclass AND c.oid = i.indrelid AND i.indexrelid = c2.oid ORDER BY i.indisprimary DESC, c2.relname;
SELECT conname::text, pg_catalog.pg_get_constraintdef(r.oid, true) FROM pg_catalog.pg_constraint r WHERE r.conrelid = 's13.child'::regclass AND r.contype = 'f' ORDER BY 1;
SELECT pg_catalog.pg_get_constraintdef(r.oid, true) FROM pg_catalog.pg_constraint r WHERE r.conrelid = 's13.child'::regclass AND r.contype = 'c';
-- SQLAlchemy dialect reflection
SELECT pg_catalog.pg_class.relname::text FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p']) AND pg_catalog.pg_class.relpersistence != 't' AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' AND pg_catalog.pg_namespace.nspname = 's13' ORDER BY pg_catalog.pg_class.relname;
SELECT pg_catalog.pg_class.relname::text FROM pg_catalog.pg_class WHERE pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_class.relname = 'parent' AND pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p', 'f', 'v', 'm']);
SELECT pg_catalog.pg_get_serial_sequence('s13.child', 'id');
SHOW transaction isolation level;
SELECT current_schema();
SELECT pg_catalog.version();
SELECT 'x'::name, 'x'::name::text, pg_typeof('x'::name)::text;
SELECT oid::int > 0, oid::regclass::text FROM pg_class WHERE oid = 's13.parent'::regclass;
-- Hibernate / Liquibase / Flyway / Jmix
SELECT current_database() IS NOT NULL, current_user IS NOT NULL, session_user IS NOT NULL;
SELECT table_name::text FROM information_schema.tables WHERE table_schema = 's13' AND table_type = 'BASE TABLE' AND table_name = 'parent';
SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'plpgsql';
SELECT 1 FROM pg_catalog.pg_class WHERE relname = 'parent' AND relnamespace = (SELECT oid FROM pg_catalog.pg_namespace WHERE nspname = current_schema()) OR true LIMIT 1;
SELECT nextval('s13.extra_seq'), currval('s13.extra_seq');
SELECT last_value, is_called FROM s13.extra_seq;

-- ### 13_catalog/explain-shape
SET search_path = s13;
INSERT INTO parent (code, price) VALUES ('A', 1), ('B', 2), ('C', 3);
INSERT INTO child (pid, note, qty) SELECT id, 'n' || id, id FROM parent;
-- shape
EXPLAIN SELECT * FROM parent WHERE id = 1;
-- shape
EXPLAIN SELECT * FROM parent p JOIN child c ON c.pid = p.id WHERE p.code = 'A' ORDER BY c.id LIMIT 5;
-- shape
EXPLAIN (COSTS OFF) SELECT count(*) FROM child GROUP BY pid;
-- shape
EXPLAIN (FORMAT JSON) SELECT 1;
-- shape
EXPLAIN (FORMAT TEXT, COSTS OFF, SUMMARY OFF) SELECT 1;
-- shape
EXPLAIN (ANALYZE, TIMING OFF, COSTS OFF, SUMMARY OFF) SELECT * FROM parent WHERE id < 0;
-- shape
EXPLAIN INSERT INTO child (pid, qty) VALUES (1, 1);
-- shape
EXPLAIN DELETE FROM child WHERE id = 99999;
-- shape
EXPLAIN SELECT * FROM nosuch;
-- shape
EXPLAIN (VERBOSE) SELECT id FROM parent;
-- shape
EXPLAIN (BOGUS) SELECT 1;
SELECT count(*) FROM parent;
SELECT count(*) FROM child;

-- ### 13_catalog/teardown
DROP SCHEMA s13 CASCADE;
