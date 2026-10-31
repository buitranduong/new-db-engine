-- Suite group 07: joins. Syntax source: PostgreSQL 16 docs, section 7.2.1 (joined tables), 7.2.1.5 (LATERAL).

-- ### 07_joins/setup
DROP SCHEMA IF EXISTS s07 CASCADE;
CREATE SCHEMA s07;
SET search_path = s07;
CREATE TABLE dept (id int PRIMARY KEY, name text);
CREATE TABLE emp (id int PRIMARY KEY, name text, dept_id int, boss int, sal int);
CREATE TABLE proj (id int PRIMARY KEY, name text, lead int);
INSERT INTO dept VALUES (1, 'eng'), (2, 'ops'), (3, 'empty');
INSERT INTO emp VALUES (1, 'ann', 1, NULL, 100), (2, 'bob', 1, 1, 80), (3, 'cat', 2, 1, 70), (4, 'dan', NULL, 3, 60), (5, 'eve', 2, 3, 90), (6, 'fay', 1, 2, 80);
INSERT INTO proj VALUES (1, 'alpha', 1), (2, 'beta', 3), (3, 'gamma', NULL), (4, 'delta', 99);
CREATE TABLE users (id int PRIMARY KEY, name text, country text, signup date, age int, vip boolean);
INSERT INTO users SELECT g AS id, 'user' || lpad(g::text, 4, '0') AS name, (ARRAY['VN', 'US', 'DE', 'JP', 'FR'])[1 + g % 5] AS country, DATE '2024-01-01' + (g % 90) AS signup, 18 + (g * 7) % 50 AS age, (g % 4 = 0) AS vip FROM generate_series(1, @N@) g;
CREATE TABLE events (id int PRIMARY KEY, user_id int, ts timestamp, kind text, amount numeric(10,2));
INSERT INTO events SELECT g AS id, 1 + ((g::bigint * 7) % @N@)::int AS user_id, TIMESTAMP '2024-03-01 00:00:00' + ((g::bigint * 1234) % 864000) * INTERVAL '1 second' AS ts, (ARRAY['view', 'click', 'cart', 'buy'])[1 + ((g::bigint * g) % 4)::int] AS kind, (((g::bigint * 37) % 1000)::numeric / 10)::numeric(10,2) AS amount FROM generate_series(1, @N@ * 10) g;

-- ### 07_joins/inner-outer
SET search_path = s07;
SELECT e.name, d.name FROM emp e JOIN dept d ON d.id = e.dept_id ORDER BY e.id;
SELECT e.name, d.name FROM emp e INNER JOIN dept d ON d.id = e.dept_id AND d.name <> 'ops' ORDER BY e.id;
SELECT e.name, d.name FROM emp e LEFT JOIN dept d ON d.id = e.dept_id ORDER BY e.id;
SELECT e.name, d.name FROM emp e LEFT OUTER JOIN dept d ON d.id = e.dept_id AND d.name = 'eng' ORDER BY e.id;
SELECT e.name, d.name FROM emp e LEFT JOIN dept d ON d.id = e.dept_id WHERE d.name = 'eng' ORDER BY e.id;
SELECT e.name, d.name FROM emp e LEFT JOIN dept d ON d.id = e.dept_id WHERE d.id IS NULL ORDER BY e.id;
SELECT e.name, d.name FROM emp e RIGHT JOIN dept d ON d.id = e.dept_id ORDER BY d.id, e.id;
SELECT e.name, d.name FROM emp e FULL JOIN dept d ON d.id = e.dept_id ORDER BY d.id NULLS LAST, e.id;
SELECT e.name, d.name FROM emp e FULL JOIN dept d ON d.id = e.dept_id WHERE e.id IS NULL OR d.id IS NULL ORDER BY e.id NULLS LAST;
SELECT p.name, e.name FROM proj p FULL OUTER JOIN emp e ON e.id = p.lead ORDER BY p.id NULLS LAST, e.id;
SELECT count(*) FROM emp e, dept d;
SELECT count(*) FROM emp CROSS JOIN dept;
SELECT e.name, d.name FROM emp e CROSS JOIN dept d WHERE e.dept_id = d.id ORDER BY e.id;
SELECT e.name, d.name FROM emp e, dept d WHERE e.dept_id = d.id ORDER BY e.id;
SELECT e.name, d.name FROM emp e JOIN dept d ON e.dept_id = d.id JOIN proj p ON p.lead = e.id ORDER BY e.id;
SELECT e.name, d.name, p.name FROM emp e LEFT JOIN dept d ON d.id = e.dept_id LEFT JOIN proj p ON p.lead = e.id ORDER BY e.id;
SELECT e.name FROM emp e JOIN dept d ON e.sal > 85 ORDER BY e.id, d.id LIMIT 3;
SELECT e.name, d.name FROM emp e JOIN dept d ON e.dept_id <> d.id ORDER BY e.id, d.id LIMIT 4;
SELECT e.name FROM emp e JOIN dept d ON nope = 1;
SELECT name FROM emp e JOIN dept d ON d.id = e.dept_id;
SELECT e.name FROM emp e JOIN dept ON dept.id = e.dept_id AND dept.name = 'ops' ORDER BY e.id;
SELECT id FROM emp JOIN dept ON dept.id = emp.dept_id;
SELECT * FROM emp e JOIN dept d ON d.id = e.dept_id ORDER BY e.id LIMIT 1;
SELECT e.*, d.name AS dname FROM emp e JOIN dept d ON d.id = e.dept_id ORDER BY e.id LIMIT 1;
SELECT count(*) FROM users u JOIN events e ON e.user_id = u.id;
SELECT u.country, count(*) FROM users u JOIN events e ON e.user_id = u.id GROUP BY u.country ORDER BY u.country;
SELECT u.id, count(e.id) FROM users u LEFT JOIN events e ON e.user_id = u.id AND e.kind = 'buy' WHERE u.id <= 5 GROUP BY u.id ORDER BY u.id;
SELECT count(*) FROM users u JOIN events e ON e.user_id = u.id AND e.ts > '2024-03-05' JOIN users u2 ON u2.id = e.user_id AND u2.vip;

-- ### 07_joins/using-natural-self
SET search_path = s07;
CREATE TABLE ja (id int, k int, va text);
CREATE TABLE jb (id int, k int, vb text);
INSERT INTO ja VALUES (1, 10, 'a1'), (2, 20, 'a2'), (3, NULL, 'a3');
INSERT INTO jb VALUES (1, 10, 'b1'), (2, 21, 'b2'), (4, NULL, 'b4');
SELECT * FROM ja JOIN jb USING (id) ORDER BY id;
SELECT * FROM ja JOIN jb USING (id, k) ORDER BY id;
SELECT id, ja.k, va, vb FROM ja LEFT JOIN jb USING (id) ORDER BY id;
SELECT id, va, vb FROM ja FULL JOIN jb USING (id) ORDER BY id;
SELECT ja.id, jb.id FROM ja FULL JOIN jb USING (id) ORDER BY 1 NULLS LAST, 2;
SELECT * FROM ja NATURAL JOIN jb ORDER BY id;
SELECT * FROM ja NATURAL LEFT JOIN jb ORDER BY id;
SELECT * FROM ja NATURAL FULL JOIN jb ORDER BY id NULLS LAST;
SELECT * FROM ja NATURAL JOIN dept;
SELECT * FROM ja JOIN jb USING (nope);
SELECT ja.id FROM ja JOIN jb USING (id) WHERE ja.k = 10;
SELECT id FROM ja JOIN jb USING (id) JOIN dept USING (id) ORDER BY id;
SELECT a.name AS emp, b.name AS boss FROM emp a JOIN emp b ON b.id = a.boss ORDER BY a.id;
SELECT a.name AS emp, b.name AS boss FROM emp a LEFT JOIN emp b ON b.id = a.boss ORDER BY a.id;
SELECT a.name, b.name FROM emp a JOIN emp b ON a.dept_id = b.dept_id AND a.id < b.id ORDER BY a.id, b.id;
SELECT a.name, count(b.id) AS reports FROM emp a LEFT JOIN emp b ON b.boss = a.id GROUP BY a.name ORDER BY a.name;
SELECT a.name, b.name FROM emp a JOIN emp b ON a.sal = b.sal AND a.id <> b.id ORDER BY a.id;
SELECT a.id FROM emp a JOIN emp a ON a.id = a.id;
SELECT x.n, y.n FROM generate_series(1, 3) x(n) JOIN generate_series(2, 4) y(n) ON y.n = x.n + 1 ORDER BY 1;
SELECT * FROM (SELECT 1 AS a) x JOIN (SELECT 1 AS a) y USING (a);
SELECT * FROM (SELECT 1 AS a) x, (SELECT 2 AS a) y;
SELECT x.* FROM (SELECT 1 AS a, 2 AS b) x;
SELECT * FROM (SELECT 1) x;
SELECT * FROM (VALUES (1)) x;
SELECT t.* FROM (VALUES (1, 2)) AS t(a, b);

-- ### 07_joins/lateral-semi-anti
SET search_path = s07;
SELECT d.name, t.name, t.sal FROM dept d LEFT JOIN LATERAL (SELECT e.name, e.sal FROM emp e WHERE e.dept_id = d.id ORDER BY e.sal DESC, e.id LIMIT 2) t ON true ORDER BY d.id, t.sal DESC, t.name;
SELECT d.name, t.name FROM dept d, LATERAL (SELECT e.name FROM emp e WHERE e.dept_id = d.id ORDER BY e.sal DESC, e.id LIMIT 1) t ORDER BY d.id;
SELECT d.name, t.name FROM dept d CROSS JOIN LATERAL (SELECT e.name FROM emp e WHERE e.dept_id = d.id ORDER BY e.id LIMIT 1) t ORDER BY d.id;
SELECT e.name, g FROM emp e, LATERAL generate_series(1, e.id % 3) g ORDER BY e.id, g;
SELECT e.name, x.v FROM emp e CROSS JOIN LATERAL (VALUES (e.sal), (e.sal * 2)) x(v) WHERE e.id <= 2 ORDER BY e.id, x.v;
SELECT e.id, t.c FROM emp e LEFT JOIN LATERAL (SELECT count(*) AS c FROM emp r WHERE r.boss = e.id) t ON true ORDER BY e.id;
SELECT d.name, t.name FROM dept d, (SELECT e.name FROM emp e WHERE e.dept_id = d.id) t;
SELECT u.id, e.id FROM users u CROSS JOIN LATERAL (SELECT id FROM events ev WHERE ev.user_id = u.id ORDER BY ts, id LIMIT 1) e WHERE u.id <= 3 ORDER BY u.id;
SELECT u.id, a.arr FROM users u CROSS JOIN LATERAL (SELECT array_agg(kind ORDER BY id) AS arr FROM (SELECT kind, id FROM events ev WHERE ev.user_id = u.id ORDER BY id LIMIT 3) z) a WHERE u.id <= 2 ORDER BY u.id;
SELECT name FROM dept d WHERE EXISTS (SELECT 1 FROM emp e WHERE e.dept_id = d.id) ORDER BY id;
SELECT name FROM dept d WHERE NOT EXISTS (SELECT 1 FROM emp e WHERE e.dept_id = d.id) ORDER BY id;
SELECT name FROM dept WHERE id IN (SELECT dept_id FROM emp) ORDER BY id;
SELECT name FROM dept WHERE id NOT IN (SELECT dept_id FROM emp) ORDER BY id;
SELECT name FROM dept WHERE id NOT IN (SELECT dept_id FROM emp WHERE dept_id IS NOT NULL) ORDER BY id;
SELECT d.name FROM dept d LEFT JOIN emp e ON e.dept_id = d.id WHERE e.id IS NULL ORDER BY d.id;
SELECT DISTINCT d.name FROM dept d JOIN emp e ON e.dept_id = d.id ORDER BY d.name;
SELECT name FROM emp e WHERE EXISTS (SELECT 1 FROM proj p WHERE p.lead = e.id) ORDER BY id;
SELECT name FROM emp e WHERE NOT EXISTS (SELECT 1 FROM proj p WHERE p.lead = e.id) ORDER BY id;
SELECT p.name FROM proj p WHERE p.lead NOT IN (SELECT id FROM emp) ORDER BY p.id;
SELECT p.name FROM proj p WHERE NOT EXISTS (SELECT 1 FROM emp e WHERE e.id = p.lead) ORDER BY p.id;
SELECT count(*) FROM users u WHERE EXISTS (SELECT 1 FROM events e WHERE e.user_id = u.id AND e.kind = 'buy');
SELECT count(*) FROM users u WHERE NOT EXISTS (SELECT 1 FROM events e WHERE e.user_id = u.id AND e.amount > 99);
SELECT (SELECT name FROM dept d WHERE d.id = e.dept_id), name FROM emp e ORDER BY e.id;
SELECT e.name, (SELECT count(*) FROM emp r WHERE r.boss = e.id) FROM emp e ORDER BY e.id;
SELECT e.name FROM emp e WHERE e.sal > (SELECT avg(sal) FROM emp) ORDER BY e.id;
SELECT e.name FROM emp e WHERE e.sal = (SELECT max(sal) FROM emp x WHERE x.dept_id = e.dept_id) ORDER BY e.id;
SELECT (SELECT id FROM emp WHERE sal = 80);
SELECT (SELECT id, name FROM emp WHERE id = 1);
SELECT id FROM emp WHERE sal = (SELECT sal FROM emp WHERE sal = 80);

-- ### 07_joins/teardown
DROP SCHEMA s07 CASCADE;
