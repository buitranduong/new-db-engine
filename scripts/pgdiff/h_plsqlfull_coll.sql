-- Group h (plsqlfull): COLLATE on column and index definitions, CREATE COLLATION (staskd -collation en_US.utf8 against postgres:16 en_US.utf8).

-- ### h/co-column
-- drop: co_t
CREATE TABLE co_t (id int PRIMARY KEY, name text COLLATE "C", n2 varchar(20) COLLATE "en_US", d text);
INSERT INTO co_t VALUES (1,$$b$$,$$b$$,$$b$$),(2,$$A$$,$$A$$,$$A$$),(3,$$a$$,$$a$$,$$a$$),(4,$$B$$,$$B$$,$$B$$),(5,$$ x$$,$$x$$,$$x$$),(6,$$_y$$,$$y$$,$$y$$);
SELECT name FROM co_t ORDER BY name;
SELECT name FROM co_t ORDER BY name DESC;
SELECT n2 FROM co_t ORDER BY n2;
SELECT d FROM co_t ORDER BY d;
SELECT name FROM co_t WHERE name > $$a$$ ORDER BY name;
SELECT name FROM co_t WHERE name BETWEEN $$A$$ AND $$b$$ ORDER BY name;
SELECT n2 FROM co_t WHERE n2 > $$a$$ ORDER BY n2;
SELECT t.name FROM co_t t ORDER BY t.name LIMIT 3;
SELECT name FROM co_t ORDER BY name COLLATE "en_US";
SELECT name FROM co_t ORDER BY d;
SELECT a.attname, a.attcollation = 100 AS is_default FROM pg_attribute a WHERE a.attrelid = $$co_t$$::regclass AND a.attnum > 0 ORDER BY a.attnum;
SELECT column_name, collation_name FROM information_schema.columns WHERE table_name = $$co_t$$ ORDER BY ordinal_position;
SELECT a.attname, (SELECT c.collname FROM pg_catalog.pg_collation c, pg_catalog.pg_type t WHERE c.oid = a.attcollation AND t.oid = a.atttypid AND a.attcollation <> t.typcollation) AS attcollation FROM pg_catalog.pg_attribute a WHERE a.attrelid = $$co_t$$::regclass AND a.attnum > 0 ORDER BY a.attnum;
CREATE TABLE co_bad (x text COLLATE "no_such_collation");
ALTER TABLE co_t ADD COLUMN e text COLLATE "C";
ALTER TABLE co_t ALTER COLUMN d TYPE text COLLATE "C";
SELECT d FROM co_t ORDER BY d;
SELECT column_name, collation_name FROM information_schema.columns WHERE table_name = $$co_t$$ ORDER BY ordinal_position;
ALTER TABLE co_t ALTER COLUMN id TYPE bigint COLLATE "C";

-- ### h/co-index
-- drop: co_i
CREATE TABLE co_i (id int PRIMARY KEY, name text);
INSERT INTO co_i VALUES (1,$$b$$),(2,$$A$$),(3,$$a$$),(4,$$B$$);
CREATE INDEX co_i_c ON co_i (name COLLATE "C");
CREATE INDEX co_i_u ON co_i (lower(name) COLLATE "C", id);
SELECT indexname, indexdef FROM pg_indexes WHERE tablename = $$co_i$$ ORDER BY indexname;
SELECT pg_get_indexdef(i.indexrelid) FROM pg_index i WHERE i.indrelid = $$co_i$$::regclass ORDER BY 1;
SELECT name FROM co_i ORDER BY name COLLATE "C";
SELECT name FROM co_i ORDER BY name;
SELECT name FROM co_i WHERE name > $$a$$ ORDER BY name COLLATE "C";
CREATE INDEX co_i_bad ON co_i (name COLLATE "no_such");

-- ### h/co-create-collation
-- drop: co_u
CREATE COLLATION co_c (provider = libc, locale = $$C$$);
CREATE COLLATION co_en FROM "en_US";
CREATE COLLATION co_c (provider = libc, locale = $$C$$);
CREATE COLLATION IF NOT EXISTS co_c (provider = libc, locale = $$C$$);
CREATE COLLATION co_bad (provider = libc, locale = $$no_such_locale$$);
SELECT collname, collprovider, collisdeterministic FROM pg_collation WHERE collname LIKE $$co_%$$ ORDER BY collname;
CREATE TABLE co_u (id int PRIMARY KEY, name text COLLATE co_c, n2 text COLLATE co_en);
INSERT INTO co_u VALUES (1,$$b$$,$$b$$),(2,$$A$$,$$A$$),(3,$$a$$,$$a$$),(4,$$B$$,$$B$$);
SELECT name FROM co_u ORDER BY name;
SELECT n2 FROM co_u ORDER BY n2;
SELECT name FROM co_u ORDER BY name COLLATE co_en;
SELECT name FROM co_u ORDER BY name COLLATE "co_c";
DROP COLLATION co_c;
DROP TABLE co_u;
DROP COLLATION co_c;
DROP COLLATION co_en;
DROP COLLATION co_en;
DROP COLLATION IF EXISTS co_en;
SELECT count(*) FROM pg_collation WHERE collname LIKE $$co_%$$;
