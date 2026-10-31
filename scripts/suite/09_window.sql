-- Suite group 09: window functions. Syntax source: PostgreSQL 16 docs, section 3.5 (tutorial),
-- 4.2.8 (window function calls: frames ROWS/RANGE/GROUPS, EXCLUDE, named windows), 9.22.

-- ### 09_window/setup
DROP SCHEMA IF EXISTS s09 CASCADE;
CREATE SCHEMA s09;
SET search_path = s09;
CREATE TABLE sc (id int PRIMARY KEY, grp text, v int, d date);
INSERT INTO sc SELECT g, (ARRAY['a', 'b', 'c'])[1 + g % 3], (g * 7) % 10, DATE '2024-01-01' + g FROM generate_series(1, 18) g;
INSERT INTO sc VALUES (19, 'a', NULL, '2024-02-01'), (20, 'b', NULL, '2024-02-02');
CREATE TABLE users (id int PRIMARY KEY, name text, country text, signup date, age int, vip boolean);
INSERT INTO users SELECT g AS id, 'user' || lpad(g::text, 4, '0') AS name, (ARRAY['VN', 'US', 'DE', 'JP', 'FR'])[1 + g % 5] AS country, DATE '2024-01-01' + (g % 90) AS signup, 18 + (g * 7) % 50 AS age, (g % 4 = 0) AS vip FROM generate_series(1, @N@) g;
CREATE TABLE events (id int PRIMARY KEY, user_id int, ts timestamp, kind text, amount numeric(10,2));
INSERT INTO events SELECT g AS id, 1 + ((g::bigint * 7) % @N@)::int AS user_id, TIMESTAMP '2024-03-01 00:00:00' + ((g::bigint * 1234) % 864000) * INTERVAL '1 second' AS ts, (ARRAY['view', 'click', 'cart', 'buy'])[1 + ((g::bigint * g) % 4)::int] AS kind, (((g::bigint * 37) % 1000)::numeric / 10)::numeric(10,2) AS amount FROM generate_series(1, @N@ * 10) g;

-- ### 09_window/ranking
SET search_path = s09;
SELECT id, grp, v, row_number() OVER (ORDER BY v, id) AS rn, rank() OVER (ORDER BY v) AS rk, dense_rank() OVER (ORDER BY v) AS drk FROM sc ORDER BY id;
SELECT id, grp, v, row_number() OVER (PARTITION BY grp ORDER BY v, id) AS rn, rank() OVER (PARTITION BY grp ORDER BY v) AS rk, dense_rank() OVER (PARTITION BY grp ORDER BY v DESC NULLS LAST) AS drk FROM sc ORDER BY id;
SELECT id, v, percent_rank() OVER (ORDER BY v), cume_dist() OVER (ORDER BY v), ntile(4) OVER (ORDER BY v, id) FROM sc ORDER BY id;
SELECT id, ntile(3) OVER (ORDER BY id), ntile(30) OVER (ORDER BY id) FROM sc WHERE id <= 5 ORDER BY id;
SELECT id, row_number() OVER (), count(*) OVER () FROM sc WHERE id <= 3 ORDER BY id;
SELECT id, row_number() OVER (ORDER BY grp) IS NOT NULL FROM sc WHERE id <= 3 ORDER BY id;
SELECT id, grp, rn FROM (SELECT id, grp, v, row_number() OVER (PARTITION BY grp ORDER BY v DESC NULLS LAST, id) AS rn FROM sc) t WHERE rn <= 2 ORDER BY grp, rn;
SELECT grp, id, v FROM (SELECT *, rank() OVER (PARTITION BY grp ORDER BY v DESC NULLS LAST) AS rk FROM sc) t WHERE rk = 1 ORDER BY grp, id;
SELECT id, rank() OVER (ORDER BY v) FROM sc WHERE rank() OVER (ORDER BY v) = 1;
SELECT id, row_number() OVER (ORDER BY id) FROM sc GROUP BY id HAVING row_number() OVER (ORDER BY id) > 1;
SELECT id, rank() FROM sc;
SELECT id, ntile(0) OVER (ORDER BY id) FROM sc;
SELECT id, ntile(NULL) OVER (ORDER BY id) FROM sc WHERE id < 3;
SELECT row_number() OVER (ORDER BY id DESC) AS rn, id FROM sc ORDER BY rn LIMIT 3;

-- ### 09_window/value-functions
SET search_path = s09;
SELECT id, v, lag(v) OVER (ORDER BY id), lead(v) OVER (ORDER BY id), lag(v, 2) OVER (ORDER BY id), lead(v, 2, -1) OVER (ORDER BY id), lag(v, 1, 99) OVER (ORDER BY id) FROM sc ORDER BY id;
SELECT id, grp, v, lag(v) OVER (PARTITION BY grp ORDER BY id), v - lag(v) OVER (PARTITION BY grp ORDER BY id) AS delta FROM sc ORDER BY id;
SELECT id, first_value(v) OVER (ORDER BY id), last_value(v) OVER (ORDER BY id), last_value(v) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING), nth_value(v, 3) OVER (ORDER BY id), nth_value(v, 3) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM sc ORDER BY id;
SELECT id, grp, first_value(id) OVER (PARTITION BY grp ORDER BY v DESC NULLS LAST, id), last_value(id) OVER (PARTITION BY grp ORDER BY v DESC NULLS LAST, id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM sc ORDER BY id;
SELECT id, v, first_value(v) OVER (ORDER BY id ROWS 2 PRECEDING), last_value(v) OVER (ORDER BY id ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING) FROM sc WHERE id <= 6 ORDER BY id;
SELECT id, lag(v) IGNORE NULLS OVER (ORDER BY id) FROM sc WHERE id > 17 ORDER BY id;
SELECT id, nth_value(v, 0) OVER (ORDER BY id) FROM sc;
SELECT id, lag(v, -1) OVER (ORDER BY id), lead(v, -1) OVER (ORDER BY id) FROM sc WHERE id <= 3 ORDER BY id;
SELECT id, lag(d) OVER (ORDER BY id), d - lag(d) OVER (ORDER BY id) FROM sc WHERE id <= 3 ORDER BY id;
SELECT id, lag(grp, 1, 'none') OVER (ORDER BY id) FROM sc WHERE id <= 3 ORDER BY id;
SELECT id, lag(v, 1, 'x') OVER (ORDER BY id) FROM sc WHERE id <= 3;

-- ### 09_window/frames
SET search_path = s09;
SELECT id, v, sum(v) OVER (ORDER BY id) AS run, sum(v) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS run_rows, sum(v) OVER (ORDER BY v) AS run_peers, sum(v) OVER () AS total FROM sc ORDER BY id;
SELECT id, v, avg(v) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) AS ma3, avg(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) AS centered, count(v) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND 2 FOLLOWING) AS c5 FROM sc ORDER BY id;
SELECT id, v, sum(v) OVER (ORDER BY v RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) AS rng, sum(v) OVER (ORDER BY v RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS cum FROM sc WHERE v IS NOT NULL ORDER BY id;
SELECT id, d, count(*) OVER (ORDER BY d RANGE BETWEEN INTERVAL '3 days' PRECEDING AND CURRENT ROW) AS last4days FROM sc WHERE id <= 8 ORDER BY id;
SELECT id, v, sum(v) OVER (ORDER BY v GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) AS g1, count(*) OVER (ORDER BY v GROUPS BETWEEN CURRENT ROW AND 1 FOLLOWING) AS g2 FROM sc WHERE v IS NOT NULL ORDER BY id;
SELECT id, v, sum(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW) AS ex_cur, sum(v) OVER (ORDER BY v RANGE BETWEEN CURRENT ROW AND CURRENT ROW EXCLUDE TIES) AS ex_ties, sum(v) OVER (ORDER BY v RANGE BETWEEN CURRENT ROW AND CURRENT ROW EXCLUDE GROUP) AS ex_grp FROM sc WHERE id <= 8 ORDER BY id;
SELECT id, sum(v) OVER (ORDER BY id ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) AS rev_run, min(v) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) AS mn, max(v) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) AS mx FROM sc WHERE id <= 8 ORDER BY id;
SELECT id, grp, sum(v) OVER (PARTITION BY grp ORDER BY id) AS run, round(avg(v) OVER (PARTITION BY grp ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW), 3) AS ma, count(*) OVER (PARTITION BY grp) AS n FROM sc ORDER BY id;
SELECT id, array_agg(v) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW), string_agg(grp, '') OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) FROM sc WHERE id <= 5 ORDER BY id;
SELECT id, sum(v) OVER w, avg(v) OVER w, count(*) OVER (w ROWS UNBOUNDED PRECEDING) FROM sc WHERE id <= 6 WINDOW w AS (PARTITION BY grp ORDER BY id) ORDER BY id;
SELECT id, sum(v) OVER w1, rank() OVER w2 FROM sc WHERE id <= 6 WINDOW w1 AS (ORDER BY id), w2 AS (w1 ROWS BETWEEN CURRENT ROW AND CURRENT ROW) ORDER BY id;
SELECT id, sum(v) OVER nosuch FROM sc;
SELECT id, sum(v) OVER (ORDER BY id RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM sc WHERE id <= 3;
SELECT id, sum(v) OVER (ORDER BY id ROWS BETWEEN 1 FOLLOWING AND 1 PRECEDING) FROM sc;
SELECT id, sum(v) OVER (ORDER BY d, id ROWS BETWEEN 1 PRECEDING AND 1 PRECEDING) FROM sc WHERE id <= 4 ORDER BY id;
SELECT id, sum(v) FILTER (WHERE grp = 'a') OVER (ORDER BY id) FROM sc WHERE id <= 6 ORDER BY id;
SELECT id, count(DISTINCT v) OVER (ORDER BY id) FROM sc;
SELECT id, sum(v) OVER (ORDER BY id) - sum(v) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) FROM sc WHERE id <= 5 ORDER BY id;
SELECT grp, sum(v), sum(sum(v)) OVER (ORDER BY grp), rank() OVER (ORDER BY sum(v) DESC, grp) FROM sc GROUP BY grp ORDER BY grp;
SELECT DISTINCT grp, sum(v) OVER (PARTITION BY grp) FROM sc ORDER BY grp;
SELECT id, v, round(100.0 * v / sum(v) OVER (), 2) AS pct, round(100.0 * sum(v) OVER (ORDER BY id) / sum(v) OVER (), 2) AS cum_pct FROM sc WHERE id <= 5 ORDER BY id;
SELECT u.country, u.id, e.cnt, sum(e.cnt) OVER (PARTITION BY u.country ORDER BY u.id) FROM users u JOIN (SELECT user_id, count(*) AS cnt FROM events GROUP BY user_id) e ON e.user_id = u.id WHERE u.id <= 10 ORDER BY u.country, u.id;

-- ### 09_window/teardown
DROP SCHEMA s09 CASCADE;
