-- Suite group 03: views. Syntax source: PostgreSQL 16 docs, CREATE VIEW, CREATE MATERIALIZED VIEW,
-- REFRESH MATERIALIZED VIEW, WITH RECURSIVE (section 7.8).

-- ### 03_views/setup
DROP SCHEMA IF EXISTS s03 CASCADE;
CREATE SCHEMA s03;
SET search_path = s03;
CREATE TABLE users (id int PRIMARY KEY, name text, country text, signup date, age int, vip boolean);
INSERT INTO users SELECT g AS id, 'user' || lpad(g::text, 4, '0') AS name, (ARRAY['VN', 'US', 'DE', 'JP', 'FR'])[1 + g % 5] AS country, DATE '2024-01-01' + (g % 90) AS signup, 18 + (g * 7) % 50 AS age, (g % 4 = 0) AS vip FROM generate_series(1, @N@) g;
CREATE TABLE events (id int PRIMARY KEY, user_id int, ts timestamp, kind text, amount numeric(10,2));
INSERT INTO events SELECT g AS id, 1 + ((g::bigint * 7) % @N@)::int AS user_id, TIMESTAMP '2024-03-01 00:00:00' + ((g::bigint * 1234) % 864000) * INTERVAL '1 second' AS ts, (ARRAY['view', 'click', 'cart', 'buy'])[1 + ((g::bigint * g) % 4)::int] AS kind, (((g::bigint * 37) % 1000)::numeric / 10)::numeric(10,2) AS amount FROM generate_series(1, @N@ * 10) g;

-- ### 03_views/plain
SET search_path = s03;
CREATE VIEW v_vip AS SELECT id, name, country FROM users WHERE vip;
CREATE VIEW v_cols (uid, uname) AS SELECT id, name FROM users WHERE id <= 3;
CREATE VIEW v_agg AS SELECT country, count(*) AS n, avg(age)::numeric(5,2) AS avg_age FROM users GROUP BY country;
CREATE VIEW v_join AS SELECT u.name, count(e.id) AS ev FROM users u LEFT JOIN events e ON e.user_id = u.id GROUP BY u.name;
SELECT count(*) FROM v_vip;
SELECT * FROM v_cols ORDER BY uid;
SELECT * FROM v_agg ORDER BY country;
SELECT * FROM v_join ORDER BY name LIMIT 3;
SELECT * FROM v_vip WHERE id < 20 ORDER BY id;
SELECT column_name::text, data_type::text FROM information_schema.columns WHERE table_schema = 's03' AND table_name = 'v_agg' ORDER BY ordinal_position;
SELECT table_name::text, view_definition IS NOT NULL FROM information_schema.views WHERE table_schema = 's03' ORDER BY table_name;
CREATE VIEW v_vip AS SELECT 1;
CREATE OR REPLACE VIEW v_vip AS SELECT id, name, country FROM users WHERE vip AND id < 50;
SELECT count(*) FROM v_vip;
CREATE OR REPLACE VIEW v_vip AS SELECT id FROM users;
CREATE OR REPLACE VIEW v_vip AS SELECT id, name, country, age FROM users WHERE vip;
SELECT count(*) FROM v_vip;
ALTER VIEW v_vip RENAME TO v_vip2;
ALTER VIEW v_vip2 RENAME COLUMN age TO years;
SELECT years FROM v_vip2 WHERE id = 4;
CREATE VIEW v_nested AS SELECT country, n FROM v_agg WHERE n > 0;
SELECT * FROM v_nested ORDER BY country;
DROP VIEW v_agg;
DROP VIEW v_agg CASCADE;
SELECT count(*) FROM v_nested;
CREATE VIEW v_bad AS SELECT nope FROM users;
CREATE VIEW v_secbar WITH (security_barrier) AS SELECT id FROM users WHERE id < 5;
SELECT count(*) FROM v_secbar;
DROP VIEW IF EXISTS v_nothere;
DROP VIEW v_nothere;

-- ### 03_views/updatable
SET search_path = s03;
CREATE TABLE base (id int PRIMARY KEY, v text, grp int);
INSERT INTO base SELECT g, 'v' || g, g % 3 FROM generate_series(1, 9) g;
CREATE VIEW uv AS SELECT id, v FROM base WHERE grp = 1;
CREATE VIEW uv_chk AS SELECT id, v, grp FROM base WHERE grp = 1 WITH CHECK OPTION;
INSERT INTO uv VALUES (100, 'via view');
SELECT id, v, grp FROM base WHERE id = 100;
UPDATE uv SET v = 'changed' WHERE id = 1;
UPDATE uv SET v = 'nope' WHERE id = 2;
SELECT id, v FROM base WHERE id IN (1, 2) ORDER BY id;
DELETE FROM uv WHERE id = 4;
SELECT count(*) FROM base WHERE grp = 1;
INSERT INTO uv_chk VALUES (101, 'ok', 1);
INSERT INTO uv_chk VALUES (102, 'bad', 2);
UPDATE uv_chk SET grp = 2 WHERE id = 7;
CREATE VIEW nv AS SELECT grp, count(*) AS n FROM base GROUP BY grp;
INSERT INTO nv VALUES (1, 1);
UPDATE nv SET n = 0;
SELECT table_name::text, is_updatable::text, is_insertable_into::text FROM information_schema.views WHERE table_schema = 's03' AND table_name IN ('uv', 'nv') ORDER BY table_name;
CREATE VIEW uv_ret AS SELECT * FROM base;
INSERT INTO uv_ret VALUES (200, 'r', 0) RETURNING id, v;
UPDATE uv_ret SET v = 'rr' WHERE id = 200 RETURNING id, v;
DELETE FROM uv_ret WHERE id = 200 RETURNING id;

-- ### 03_views/materialized
SET search_path = s03;
CREATE MATERIALIZED VIEW mv_country AS SELECT country, count(*) AS n, sum(age) AS total_age FROM users GROUP BY country;
SELECT * FROM mv_country ORDER BY country;
CREATE UNIQUE INDEX ux_mv_country ON mv_country (country);
CREATE MATERIALIZED VIEW mv_empty AS SELECT * FROM users WHERE id < 3 WITH NO DATA;
SELECT * FROM mv_empty;
REFRESH MATERIALIZED VIEW mv_empty;
SELECT count(*) FROM mv_empty;
INSERT INTO users VALUES (5000, 'extra', 'VN', '2024-01-01', 30, false);
SELECT n FROM mv_country WHERE country = 'VN';
REFRESH MATERIALIZED VIEW mv_country;
SELECT n FROM mv_country WHERE country = 'VN';
REFRESH MATERIALIZED VIEW CONCURRENTLY mv_country;
REFRESH MATERIALIZED VIEW CONCURRENTLY mv_empty;
SELECT matviewname::text, ispopulated FROM pg_matviews WHERE schemaname = 's03' ORDER BY matviewname;
INSERT INTO mv_country VALUES ('XX', 1, 1);
UPDATE mv_country SET n = 0;
ALTER MATERIALIZED VIEW mv_country RENAME TO mv_c2;
SELECT count(*) FROM mv_c2;
CREATE VIEW over_mv AS SELECT country FROM mv_c2 WHERE n > 1;
SELECT count(*) FROM over_mv;
DROP MATERIALIZED VIEW mv_c2;
DROP MATERIALIZED VIEW mv_c2 CASCADE;
DROP MATERIALIZED VIEW IF EXISTS mv_nothere;
SELECT count(*) FROM pg_matviews WHERE schemaname = 's03';

-- ### 03_views/recursive
SET search_path = s03;
CREATE RECURSIVE VIEW nums (n) AS SELECT 1 UNION ALL SELECT n + 1 FROM nums WHERE n < 10;
SELECT sum(n), count(*) FROM nums;
CREATE TABLE tree (id int PRIMARY KEY, parent int, name text);
INSERT INTO tree VALUES (1, NULL, 'root'), (2, 1, 'a'), (3, 1, 'b'), (4, 2, 'a1'), (5, 2, 'a2'), (6, 4, 'a1x');
CREATE VIEW tree_paths AS WITH RECURSIVE t AS (SELECT id, parent, name::text AS path, 0 AS depth FROM tree WHERE parent IS NULL UNION ALL SELECT c.id, c.parent, t.path || '/' || c.name, t.depth + 1 FROM tree c JOIN t ON c.parent = t.id) SELECT * FROM t;
SELECT id, path, depth FROM tree_paths ORDER BY path;
SELECT max(depth) FROM tree_paths;

-- ### 03_views/teardown
DROP SCHEMA s03 CASCADE;
