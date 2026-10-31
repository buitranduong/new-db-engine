-- Suite group 10: CTEs, subqueries, set operations. Syntax source: PostgreSQL 16 docs,
-- section 7.8 (WITH queries), 9.23 (subquery expressions), 7.4 (UNION, INTERSECT, EXCEPT).

-- ### 10_cte_subquery/setup
DROP SCHEMA IF EXISTS s10 CASCADE;
CREATE SCHEMA s10;
SET search_path = s10;
CREATE TABLE users (id int PRIMARY KEY, name text, country text, signup date, age int, vip boolean);
INSERT INTO users SELECT g AS id, 'user' || lpad(g::text, 4, '0') AS name, (ARRAY['VN', 'US', 'DE', 'JP', 'FR'])[1 + g % 5] AS country, DATE '2024-01-01' + (g % 90) AS signup, 18 + (g * 7) % 50 AS age, (g % 4 = 0) AS vip FROM generate_series(1, @N@) g;
CREATE TABLE events (id int PRIMARY KEY, user_id int, ts timestamp, kind text, amount numeric(10,2));
INSERT INTO events SELECT g AS id, 1 + ((g::bigint * 7) % @N@)::int AS user_id, TIMESTAMP '2024-03-01 00:00:00' + ((g::bigint * 1234) % 864000) * INTERVAL '1 second' AS ts, (ARRAY['view', 'click', 'cart', 'buy'])[1 + ((g::bigint * g) % 4)::int] AS kind, (((g::bigint * 37) % 1000)::numeric / 10)::numeric(10,2) AS amount FROM generate_series(1, @N@ * 10) g;
CREATE TABLE tree (id int PRIMARY KEY, parent int, name text, w int);
INSERT INTO tree VALUES (1, NULL, 'root', 1), (2, 1, 'a', 2), (3, 1, 'b', 3), (4, 2, 'a1', 4), (5, 2, 'a2', 5), (6, 4, 'a1x', 6), (7, 3, 'b1', 7);
CREATE TABLE edge (a int, b int);
INSERT INTO edge VALUES (1, 2), (2, 3), (3, 4), (4, 2), (5, 6);

-- ### 10_cte_subquery/with
SET search_path = s10;
WITH x AS (SELECT 1 AS a) SELECT * FROM x;
WITH x AS (SELECT 1 AS a), y AS (SELECT a + 1 AS b FROM x) SELECT * FROM x, y;
WITH x(p, q) AS (SELECT 1, 2) SELECT p + q FROM x;
WITH x AS (SELECT id, age FROM users WHERE id <= 10), y AS (SELECT avg(age) AS m FROM x) SELECT x.id FROM x, y WHERE x.age > y.m ORDER BY x.id;
WITH x AS MATERIALIZED (SELECT id FROM users WHERE id < 4), y AS NOT MATERIALIZED (SELECT id FROM users WHERE id < 3) SELECT count(*) FROM x JOIN y USING (id);
WITH x AS (SELECT 1 AS a) SELECT (SELECT a FROM x) + (SELECT a FROM x);
WITH x AS (SELECT 1 AS a), x AS (SELECT 2) SELECT * FROM x;
WITH x AS (SELECT * FROM y), y AS (SELECT 1) SELECT * FROM x;
WITH x AS (SELECT 1 AS a) SELECT a FROM x UNION ALL SELECT 2 ORDER BY a DESC;
WITH x AS (SELECT 1 AS a) INSERT INTO tree (id, name) SELECT a + 100, 'cte' FROM x RETURNING id;
WITH u AS (UPDATE tree SET w = w + 1 WHERE id = 101 RETURNING id, w), d AS (DELETE FROM tree WHERE id = 101 RETURNING id) SELECT u.w, d.id FROM u JOIN d USING (id);
WITH d AS (DELETE FROM tree WHERE id = 101 RETURNING id) SELECT count(*) FROM d;
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10) SELECT sum(i), count(*), max(i) FROM n;
WITH RECURSIVE n(i) AS (SELECT 1 UNION SELECT i % 3 + 1 FROM n) SELECT i FROM n ORDER BY i;
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n) SELECT i FROM n LIMIT 5;
WITH RECURSIVE t AS (SELECT id, name::text AS path, 1 AS depth FROM tree WHERE parent IS NULL UNION ALL SELECT c.id, t.path || '/' || c.name, t.depth + 1 FROM tree c JOIN t ON c.parent = t.id) SELECT id, path, depth FROM t ORDER BY path;
WITH RECURSIVE t AS (SELECT id, w, ARRAY[id] AS ids FROM tree WHERE id = 1 UNION ALL SELECT c.id, t.w + c.w, t.ids || c.id FROM tree c JOIN t ON c.parent = t.id) SELECT id, w, ids FROM t ORDER BY ids;
WITH RECURSIVE up AS (SELECT id, parent FROM tree WHERE id = 6 UNION ALL SELECT p.id, p.parent FROM tree p JOIN up ON p.id = up.parent) SELECT id FROM up ORDER BY id;
WITH RECURSIVE sub AS (SELECT id FROM tree WHERE id = 2 UNION ALL SELECT c.id FROM tree c JOIN sub ON c.parent = sub.id) SELECT count(*) FROM sub;
WITH RECURSIVE r(a, b, path, cyc) AS (SELECT a, b, ARRAY[a], false FROM edge WHERE a = 1 UNION ALL SELECT e.a, e.b, r.path || e.a, e.a = ANY (r.path) FROM edge e JOIN r ON e.a = r.b WHERE NOT r.cyc) SELECT a, b, path, cyc FROM r ORDER BY path;
WITH RECURSIVE r(a, b) AS (SELECT a, b FROM edge WHERE a = 1 UNION SELECT e.a, e.b FROM edge e JOIN r ON e.a = r.b) SELECT a, b FROM r ORDER BY a, b;
WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 5) CYCLE n SET is_cycle USING path SELECT n, is_cycle FROM t ORDER BY n;
WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 5) SEARCH DEPTH FIRST BY n SET ord SELECT n FROM t ORDER BY ord;
WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n * 2 FROM t WHERE n < 100) SELECT n FROM t ORDER BY n DESC;
WITH RECURSIVE t AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM t, t t2 WHERE n < 3) SELECT * FROM t;
WITH RECURSIVE t AS (SELECT 1 AS n UNION ALL SELECT count(*) FROM t) SELECT * FROM t;
WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT 'a' FROM t) SELECT * FROM t;
WITH RECURSIVE days AS (SELECT DATE '2024-03-01' AS d UNION ALL SELECT d + 1 FROM days WHERE d < DATE '2024-03-05') SELECT d, to_char(d, 'Dy') FROM days ORDER BY d;
WITH RECURSIVE fib(n, a, b) AS (SELECT 1, 0::numeric, 1::numeric UNION ALL SELECT n + 1, b, a + b FROM fib WHERE n < 30) SELECT n, a FROM fib WHERE n IN (1, 10, 30) ORDER BY n;

-- ### 10_cte_subquery/subqueries
SET search_path = s10;
SELECT id FROM users WHERE age = (SELECT max(age) FROM users) ORDER BY id;
SELECT id FROM users WHERE age > (SELECT avg(age) FROM users) AND id <= 10 ORDER BY id;
SELECT id FROM users WHERE id IN (SELECT user_id FROM events WHERE kind = 'buy' AND amount > 95) ORDER BY id;
SELECT id FROM users WHERE id NOT IN (SELECT user_id FROM events) ORDER BY id;
SELECT id FROM users u WHERE EXISTS (SELECT 1 FROM events e WHERE e.user_id = u.id AND e.amount = 99.90) ORDER BY id;
SELECT id FROM users u WHERE NOT EXISTS (SELECT 1 FROM events e WHERE e.user_id = u.id AND e.kind = 'buy') ORDER BY id;
SELECT id, (SELECT count(*) FROM events e WHERE e.user_id = u.id) AS n FROM users u WHERE id <= 5 ORDER BY id;
SELECT id, (SELECT max(amount) FROM events e WHERE e.user_id = u.id AND e.kind = 'view') FROM users u WHERE id <= 5 ORDER BY id;
SELECT id FROM users WHERE age > ANY (SELECT age FROM users WHERE id <= 3) AND id <= 6 ORDER BY id;
SELECT id FROM users WHERE age >= ALL (SELECT age FROM users WHERE id <= 5) AND id <= 20 ORDER BY id;
SELECT id FROM users WHERE age = SOME (SELECT age FROM users WHERE id = 2) ORDER BY id;
SELECT id FROM users WHERE age > ALL (SELECT age FROM users WHERE false) AND id <= 3 ORDER BY id;
SELECT id FROM users WHERE age > ANY (SELECT age FROM users WHERE false);
SELECT id FROM users WHERE (country, vip) IN (SELECT 'VN', true) AND id < 30 ORDER BY id;
SELECT id FROM users WHERE (id, age) = (SELECT id, age FROM users WHERE id = 3);
SELECT id FROM users WHERE id IN (1, (SELECT 2), (SELECT max(id) FROM users WHERE id < 4)) ORDER BY id;
SELECT x.m FROM (SELECT country, max(age) AS m FROM users GROUP BY country) x ORDER BY x.country;
SELECT country, m FROM (SELECT country, max(age) AS m FROM users GROUP BY country) x WHERE m > (SELECT avg(age) FROM users) ORDER BY country;
SELECT * FROM (SELECT id, name FROM users WHERE id <= 2) AS a, (SELECT id, age FROM users WHERE id <= 2) AS b WHERE a.id = b.id ORDER BY a.id;
SELECT * FROM (SELECT 1) AS x;
SELECT (SELECT 1 UNION ALL SELECT 2);
SELECT (SELECT 1 WHERE false) IS NULL;
SELECT EXISTS (SELECT 1 FROM users WHERE id = 1), EXISTS (SELECT 1 FROM users WHERE id = -1), NOT EXISTS (SELECT 1 WHERE false);
SELECT 1 IN (SELECT 1 UNION SELECT 2), 3 NOT IN (SELECT 1 UNION SELECT 2), NULL IN (SELECT 1), 1 IN (SELECT NULL::int), 1 NOT IN (SELECT NULL::int);
SELECT id FROM users u WHERE age > (SELECT avg(age) FROM users x WHERE x.country = u.country) AND id <= 12 ORDER BY id;
SELECT id FROM users u WHERE (SELECT count(*) FROM events e WHERE e.user_id = u.id) > 10 ORDER BY id LIMIT 5;
SELECT u.id FROM users u WHERE u.id = (SELECT e.user_id FROM events e WHERE e.id = u.id * 2);
SELECT id FROM users WHERE id = (SELECT user_id FROM events);
SELECT id FROM users WHERE id IN (SELECT id, age FROM users);
SELECT id FROM users WHERE id = (SELECT id, age FROM users WHERE id = 1);
SELECT id FROM users u WHERE EXISTS (SELECT u.nope);
SELECT id FROM users WHERE id IN (SELECT user_id FROM events WHERE kind = (SELECT 'buy')) AND id <= 5 ORDER BY id;
SELECT (SELECT count(*) FROM (SELECT DISTINCT user_id FROM events) z), (SELECT count(DISTINCT user_id) FROM events);
SELECT id FROM users ORDER BY (SELECT count(*) FROM events e WHERE e.user_id = users.id) DESC, id LIMIT 3;

-- ### 10_cte_subquery/set-operations
SET search_path = s10;
SELECT g FROM generate_series(1, 5) g UNION SELECT g FROM generate_series(4, 8) g ORDER BY 1;
SELECT g FROM generate_series(1, 5) g UNION ALL SELECT g FROM generate_series(4, 8) g ORDER BY 1;
SELECT g FROM generate_series(1, 5) g INTERSECT SELECT g FROM generate_series(4, 8) g ORDER BY 1;
SELECT g FROM generate_series(1, 5) g INTERSECT ALL SELECT g FROM generate_series(4, 8) g ORDER BY 1;
SELECT g FROM generate_series(1, 5) g EXCEPT SELECT g FROM generate_series(4, 8) g ORDER BY 1;
SELECT g FROM generate_series(1, 5) g EXCEPT ALL SELECT g FROM generate_series(4, 8) g ORDER BY 1;
SELECT x FROM (VALUES (1), (1), (2), (2), (2)) a(x) EXCEPT ALL SELECT x FROM (VALUES (1), (2)) b(x) ORDER BY 1;
SELECT x FROM (VALUES (1), (1), (2), (2), (2)) a(x) INTERSECT ALL SELECT x FROM (VALUES (1), (1), (1), (2)) b(x) ORDER BY 1;
SELECT x FROM (VALUES (1), (1), (NULL), (NULL)) a(x) UNION SELECT NULL ORDER BY 1 NULLS LAST;
SELECT x FROM (VALUES (1), (NULL)) a(x) INTERSECT SELECT x FROM (VALUES (NULL), (2)) b(x);
SELECT x FROM (VALUES (1), (NULL)) a(x) EXCEPT SELECT x FROM (VALUES (NULL), (2)) b(x);
SELECT 1 UNION SELECT 2 UNION SELECT 3 ORDER BY 1 DESC;
SELECT 1 UNION SELECT 2 INTERSECT SELECT 2 ORDER BY 1;
(SELECT 1 UNION SELECT 2) INTERSECT SELECT 2;
SELECT 1 EXCEPT SELECT 2 UNION SELECT 3 ORDER BY 1;
(SELECT id FROM users ORDER BY id LIMIT 2) UNION ALL (SELECT id FROM users ORDER BY id DESC LIMIT 2) ORDER BY 1;
SELECT id FROM users WHERE id < 3 UNION ALL SELECT id FROM users WHERE id > @N@ - 2 ORDER BY id DESC LIMIT 3;
SELECT 1 AS a, 'x' UNION ALL SELECT 2, 'y' ORDER BY a DESC;
SELECT 1 AS a UNION ALL SELECT 2.5 ORDER BY a;
SELECT 1 UNION ALL SELECT 'a';
SELECT 1, 2 UNION ALL SELECT 3;
SELECT '1' UNION ALL SELECT 2;
SELECT 'a' UNION ALL SELECT 'b'::varchar;
SELECT NULL UNION ALL SELECT 1;
SELECT DATE '2024-01-01' UNION ALL SELECT TIMESTAMP '2024-01-02 00:00';
SELECT 1 UNION ALL SELECT 2 ORDER BY nope;
SELECT 1 AS a UNION SELECT 2 ORDER BY a + 1;
SELECT country FROM users UNION SELECT 'XX' ORDER BY 1;
SELECT country, count(*) FROM users GROUP BY country UNION ALL SELECT 'total', count(*) FROM users ORDER BY 2, 1;
SELECT kind FROM events EXCEPT SELECT 'view' ORDER BY 1;
SELECT user_id FROM events WHERE kind = 'buy' INTERSECT SELECT user_id FROM events WHERE kind = 'view' ORDER BY 1 LIMIT 5;
SELECT user_id FROM events WHERE kind = 'cart' EXCEPT SELECT user_id FROM events WHERE kind = 'buy' ORDER BY 1 LIMIT 5;
SELECT count(*) FROM (SELECT user_id FROM events UNION SELECT id FROM users) z;
SELECT count(*) FROM (SELECT user_id FROM events UNION ALL SELECT id FROM users) z;
TABLE tree ORDER BY id LIMIT 2;
SELECT * FROM tree WHERE id = 1 UNION ALL TABLE tree ORDER BY id LIMIT 3;
VALUES (1), (2) UNION ALL VALUES (3) ORDER BY 1;

-- ### 10_cte_subquery/teardown
DROP SCHEMA s10 CASCADE;
