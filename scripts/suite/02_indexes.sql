-- Suite group 02: indexes. Syntax source: PostgreSQL 16 docs, CREATE INDEX and chapter 11.
-- Results are compared on query output; EXPLAIN shape is only checked in group 13.

-- ### 02_indexes/setup
DROP SCHEMA IF EXISTS s02 CASCADE;
CREATE SCHEMA s02;
SET search_path = s02;
CREATE TABLE users (id int PRIMARY KEY, name text, country text, signup date, age int, vip boolean);
INSERT INTO users SELECT g AS id, 'user' || lpad(g::text, 4, '0') AS name, (ARRAY['VN', 'US', 'DE', 'JP', 'FR'])[1 + g % 5] AS country, DATE '2024-01-01' + (g % 90) AS signup, 18 + (g * 7) % 50 AS age, (g % 4 = 0) AS vip FROM generate_series(1, @N@) g;
CREATE TABLE events (id int PRIMARY KEY, user_id int, ts timestamp, kind text, amount numeric(10,2));
INSERT INTO events SELECT g AS id, 1 + ((g::bigint * 7) % @N@)::int AS user_id, TIMESTAMP '2024-03-01 00:00:00' + ((g::bigint * 1234) % 864000) * INTERVAL '1 second' AS ts, (ARRAY['view', 'click', 'cart', 'buy'])[1 + ((g::bigint * g) % 4)::int] AS kind, (((g::bigint * 37) % 1000)::numeric / 10)::numeric(10,2) AS amount FROM generate_series(1, @N@ * 10) g;

-- ### 02_indexes/btree
SET search_path = s02;
CREATE INDEX ix_users_name ON users (name);
CREATE INDEX ix_users_country_age ON users (country, age DESC);
CREATE INDEX ix_events_user_ts ON events (user_id, ts);
CREATE INDEX ix_events_kind_nulls ON events (kind DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS ix_users_name ON users (name);
CREATE INDEX ix_users_name ON users (name);
CREATE INDEX ix_bad ON users (nope);
CREATE INDEX ix_bad ON nosuch (id);
SELECT id FROM users WHERE name = 'user0007';
SELECT id FROM users WHERE country = 'VN' AND age > 60 ORDER BY age DESC, id LIMIT 5;
SELECT count(*) FROM events WHERE user_id = 7;
SELECT id FROM events WHERE user_id = 7 AND ts >= '2024-03-03' ORDER BY ts, id LIMIT 5;
SELECT id FROM users WHERE name LIKE 'user000%' ORDER BY id;
SELECT id FROM users WHERE name >= 'user0010' AND name < 'user0013' ORDER BY id;
SELECT indexname::text, indexdef FROM pg_indexes WHERE schemaname = 's02' AND tablename = 'users' ORDER BY indexname;
SELECT i.relname::text, am.amname::text, ix.indisunique, ix.indisprimary FROM pg_index ix JOIN pg_class i ON i.oid = ix.indexrelid JOIN pg_am am ON am.oid = i.relam WHERE ix.indrelid = 's02.users'::regclass ORDER BY i.relname;

-- ### 02_indexes/unique-partial-expression
SET search_path = s02;
CREATE UNIQUE INDEX ux_users_name ON users (name);
INSERT INTO users VALUES (9001, 'user0001', 'VN', '2024-01-01', 20, false);
INSERT INTO users VALUES (9001, 'unique9001', 'VN', '2024-01-01', 20, false);
CREATE INDEX ix_vip ON users (id) WHERE vip;
CREATE UNIQUE INDEX ux_partial ON users (country) WHERE id = 1;
CREATE INDEX ix_lower_name ON users (lower(name));
CREATE INDEX ix_expr2 ON users ((age * 2), (country || '-' || id::text));
CREATE UNIQUE INDEX ux_expr ON users (upper(name)) WHERE id < 5000;
INSERT INTO users VALUES (9002, 'UNIQUE9001', 'VN', '2024-01-01', 20, false);
SELECT count(*) FROM users WHERE vip;
SELECT id FROM users WHERE lower(name) = 'user0042';
SELECT id FROM users WHERE age * 2 = 100 ORDER BY id LIMIT 3;
CREATE INDEX ix_cover ON users (country) INCLUDE (name, age);
CREATE UNIQUE INDEX ux_cover ON users (id) INCLUDE (name);
SELECT name, age FROM users WHERE country = 'JP' ORDER BY id LIMIT 3;
CREATE INDEX ix_hash ON users USING hash (name);
SELECT id FROM users WHERE name = 'user0033';
CREATE INDEX ix_brin ON events USING brin (ts);
SELECT count(*) FROM events WHERE ts >= '2024-03-05';
CREATE INDEX ix_gin_arr ON events USING gin ((ARRAY[user_id, id % 3]));
CREATE TABLE docs (id int PRIMARY KEY, tags text[], body jsonb, rng int4range, t tsvector);
INSERT INTO docs SELECT g, ARRAY['t' || g % 3, 't' || g % 5], jsonb_build_object('k', g % 4, 'name', 'n' || g), int4range(g, g + 5), to_tsvector('simple', 'word' || g % 7 || ' common') FROM generate_series(1, @N@) g;
CREATE INDEX ix_gin_tags ON docs USING gin (tags);
CREATE INDEX ix_gin_body ON docs USING gin (body);
CREATE INDEX ix_gin_body_path ON docs USING gin (body jsonb_path_ops);
CREATE INDEX ix_gist_rng ON docs USING gist (rng);
CREATE INDEX ix_gin_ts ON docs USING gin (t);
CREATE INDEX ix_spgist ON docs USING spgist (rng);
SELECT count(*) FROM docs WHERE tags @> ARRAY['t1'];
SELECT count(*) FROM docs WHERE tags && ARRAY['t1', 't4'];
SELECT count(*) FROM docs WHERE body @> '{"k": 2}';
SELECT count(*) FROM docs WHERE body ? 'name';
SELECT count(*) FROM docs WHERE rng && int4range(10, 20);
SELECT count(*) FROM docs WHERE rng @> 15;
SELECT count(*) FROM docs WHERE t @@ to_tsquery('simple', 'word3');
CREATE INDEX ix_bad_am ON docs USING nosuchmethod (id);
CREATE INDEX ix_bad_op ON docs USING gin (id);

-- ### 02_indexes/trigram
SET search_path = s02;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX ix_trgm_name ON users USING gin (name gin_trgm_ops);
CREATE INDEX ix_trgm_gist ON users USING gist (name gist_trgm_ops);
SELECT count(*) FROM users WHERE name LIKE '%007%';
SELECT id FROM users WHERE name ILIKE '%USER001%' ORDER BY id;
SELECT id FROM users WHERE name LIKE '%99' ORDER BY id;
SELECT id FROM users WHERE name ~ 'user00[1-2]5' ORDER BY id;
SELECT similarity('hello', 'hallo'), show_trgm('cat'), word_similarity('word', 'two words'), 'abc' % 'abd';
SELECT id FROM users WHERE name % 'user0045' ORDER BY similarity(name, 'user0045') DESC, id LIMIT 3;
SELECT 'cat' <-> 'cut';

-- ### 02_indexes/maintenance
SET search_path = s02;
CREATE INDEX ix_m1 ON users (age);
ALTER INDEX ix_m1 RENAME TO ix_m2;
ALTER INDEX ix_m1 RENAME TO ix_m3;
REINDEX INDEX ix_m2;
REINDEX TABLE users;
REINDEX SCHEMA s02;
DROP INDEX ix_m2;
DROP INDEX ix_m2;
DROP INDEX IF EXISTS ix_m2;
CREATE INDEX CONCURRENTLY ix_conc ON users (signup);
DROP INDEX CONCURRENTLY ix_conc;
CREATE INDEX ix_ts ON users (age) WITH (fillfactor = 70);
CREATE INDEX ix_tbs ON users (age);
DROP INDEX ix_ts, ix_tbs;
ALTER TABLE users ADD CONSTRAINT users_name_uk UNIQUE (name);
ALTER TABLE users DROP CONSTRAINT users_name_uk;
SELECT count(*) FROM pg_indexes WHERE schemaname = 's02' AND indexname = 'ix_m2';
ANALYZE users;
VACUUM users;
CLUSTER users USING users_pkey;
SELECT count(*) FROM users;
DROP INDEX users_pkey;
ALTER TABLE users DROP CONSTRAINT users_pkey;
SELECT count(*) FROM pg_indexes WHERE schemaname = 's02' AND tablename = 'users' AND indexname = 'users_pkey';

-- ### 02_indexes/teardown
DROP SCHEMA s02 CASCADE;
