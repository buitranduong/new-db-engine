-- Suite group 11: ClickHouse-style analytics translated to PostgreSQL 16.
-- Each case names the ClickHouse function or pattern it stands for (ClickHouse docs: windowFunnel,
-- retention, argMax/argMin, quantile*/quantileExact, topK, uniqExact, histogram, arrayJoin, WITH FILL,
-- toStartOfInterval, sumIf/countIf, groupArray) and shows the PostgreSQL equivalent.
-- Data: users(id,country,signup,...) and events(id,user_id,ts,kind,amount), ts spread over 10 days.

-- ### 11_analytics/setup
DROP SCHEMA IF EXISTS s11 CASCADE;
CREATE SCHEMA s11;
SET search_path = s11;
CREATE TABLE users (id int PRIMARY KEY, name text, country text, signup date, age int, vip boolean);
INSERT INTO users SELECT g AS id, 'user' || lpad(g::text, 4, '0') AS name, (ARRAY['VN', 'US', 'DE', 'JP', 'FR'])[1 + g % 5] AS country, DATE '2024-01-01' + (g % 90) AS signup, 18 + (g * 7) % 50 AS age, (g % 4 = 0) AS vip FROM generate_series(1, @N@) g;
CREATE TABLE events (id int PRIMARY KEY, user_id int, ts timestamp, kind text, amount numeric(10,2));
INSERT INTO events SELECT g AS id, 1 + ((g::bigint * 7) % @N@)::int AS user_id, TIMESTAMP '2024-03-01 00:00:00' + ((g::bigint * 1234) % 864000) * INTERVAL '1 second' AS ts, (ARRAY['view', 'click', 'cart', 'buy'])[1 + ((g::bigint * g) % 4)::int] AS kind, (((g::bigint * 37) % 1000)::numeric / 10)::numeric(10,2) AS amount FROM generate_series(1, @N@ * 10) g;
CREATE TABLE hits (id int PRIMARY KEY, uid int, ts timestamp, url text, ms int);
INSERT INTO hits SELECT g, 1 + g % 6, TIMESTAMP '2024-03-01 08:00:00' + (g * 7 + (g % 4) * 40) * INTERVAL '1 minute', '/p' || (g % 5), 50 + (g * 37) % 400 FROM generate_series(1, 120) g;

-- ### 11_analytics/sessionization
SET search_path = s11;
-- ClickHouse: sessions by gap (runningDifference / neighbor / sum(is_new) OVER)
SELECT uid, ts, ts - lag(ts) OVER (PARTITION BY uid ORDER BY ts) AS gap FROM hits WHERE uid = 1 ORDER BY ts LIMIT 5;
SELECT uid, count(*) AS events, count(DISTINCT sess) AS sessions FROM (SELECT uid, sum(new_sess) OVER (PARTITION BY uid ORDER BY ts, id) AS sess FROM (SELECT uid, ts, id, CASE WHEN ts - lag(ts) OVER (PARTITION BY uid ORDER BY ts, id) > INTERVAL '30 minutes' OR lag(ts) OVER (PARTITION BY uid ORDER BY ts, id) IS NULL THEN 1 ELSE 0 END AS new_sess FROM hits) a) b GROUP BY uid ORDER BY uid;
WITH f AS (SELECT uid, ts, id, CASE WHEN ts - lag(ts) OVER (PARTITION BY uid ORDER BY ts, id) > INTERVAL '30 minutes' THEN 1 ELSE 0 END AS brk FROM hits), s AS (SELECT *, sum(brk) OVER (PARTITION BY uid ORDER BY ts, id) AS sess FROM f) SELECT uid, sess, min(ts) AS start_ts, max(ts) AS end_ts, count(*) AS n, max(ts) - min(ts) AS dur FROM s GROUP BY uid, sess ORDER BY uid, sess;
WITH f AS (SELECT uid, ts, id, CASE WHEN ts - lag(ts) OVER (PARTITION BY uid ORDER BY ts, id) > INTERVAL '1 hour' THEN 1 ELSE 0 END AS brk FROM hits), s AS (SELECT *, sum(brk) OVER (PARTITION BY uid ORDER BY ts, id) AS sess FROM f) SELECT uid, sess, count(*) FROM s GROUP BY uid, sess ORDER BY uid, sess;
WITH f AS (SELECT uid, ts, id, url, CASE WHEN ts - lag(ts) OVER (PARTITION BY uid ORDER BY ts, id) > INTERVAL '1 hour' THEN 1 ELSE 0 END AS brk FROM hits), s AS (SELECT *, sum(brk) OVER (PARTITION BY uid ORDER BY ts, id) AS sess FROM f) SELECT uid, sess, (array_agg(url ORDER BY ts, id))[1] AS landing, (array_agg(url ORDER BY ts DESC, id DESC))[1] AS exit_page FROM s GROUP BY uid, sess ORDER BY uid, sess LIMIT 8;
SELECT uid, id, ts, row_number() OVER (PARTITION BY uid, date_trunc('day', ts) ORDER BY ts, id) AS nth_of_day FROM hits WHERE uid = 2 ORDER BY ts, id LIMIT 6;
SELECT uid, ts, id, lead(url) OVER (PARTITION BY uid ORDER BY ts, id) AS next_url FROM hits WHERE uid = 3 ORDER BY ts, id LIMIT 4;
SELECT url, next_url, count(*) FROM (SELECT url, lead(url) OVER (PARTITION BY uid ORDER BY ts, id) AS next_url FROM hits) t WHERE next_url IS NOT NULL GROUP BY url, next_url ORDER BY 3 DESC, 1, 2 LIMIT 5;

-- ### 11_analytics/funnel-retention-cohort
SET search_path = s11;
-- ClickHouse: windowFunnel(window)(ts, kind='view', kind='cart', kind='buy')
WITH steps AS (SELECT user_id, min(ts) FILTER (WHERE kind = 'view') AS t1, min(ts) FILTER (WHERE kind = 'cart') AS t2, min(ts) FILTER (WHERE kind = 'buy') AS t3 FROM events GROUP BY user_id) SELECT count(*) FILTER (WHERE t1 IS NOT NULL) AS s1, count(*) FILTER (WHERE t2 > t1) AS s2, count(*) FILTER (WHERE t3 > t2 AND t2 > t1) AS s3 FROM steps;
WITH ordered AS (SELECT user_id, array_agg(kind ORDER BY ts, id) AS path FROM events GROUP BY user_id) SELECT count(*) FILTER (WHERE path @> ARRAY['view']) AS viewed, count(*) FILTER (WHERE path @> ARRAY['view', 'cart']) AS both_vc, count(*) FILTER (WHERE 'buy' = ANY (path)) AS bought FROM ordered;
WITH ordered AS (SELECT user_id, array_agg(kind ORDER BY ts, id) AS path FROM events GROUP BY user_id) SELECT user_id, path[1:4] AS first4, array_length(path, 1) AS n, array_position(path, 'buy') AS first_buy_pos FROM ordered WHERE user_id <= 5 ORDER BY user_id;
SELECT kind, count(*) AS n, count(DISTINCT user_id) AS users, round(100.0 * count(DISTINCT user_id) / (SELECT count(DISTINCT user_id) FROM events), 2) AS pct_users FROM events GROUP BY kind ORDER BY n DESC, kind;
SELECT step, count(DISTINCT user_id) FROM (SELECT user_id, CASE kind WHEN 'view' THEN 1 WHEN 'click' THEN 2 WHEN 'cart' THEN 3 ELSE 4 END AS step FROM events) s GROUP BY step ORDER BY step;
-- ClickHouse: retention(cond1, cond2, ...) per user, here day-N retention by cohort day
WITH first_day AS (SELECT user_id, min(date_trunc('day', ts))::date AS d0 FROM events GROUP BY user_id), act AS (SELECT DISTINCT user_id, date_trunc('day', ts)::date AS d FROM events) SELECT f.d0, count(DISTINCT f.user_id) AS cohort, count(DISTINCT a.user_id) FILTER (WHERE a.d = f.d0 + 1) AS d1, count(DISTINCT a.user_id) FILTER (WHERE a.d = f.d0 + 2) AS d2, count(DISTINCT a.user_id) FILTER (WHERE a.d >= f.d0 + 1) AS later FROM first_day f JOIN act a USING (user_id) GROUP BY f.d0 ORDER BY f.d0;
WITH act AS (SELECT DISTINCT user_id, date_trunc('day', ts)::date AS d FROM events) SELECT user_id, array_agg(d ORDER BY d) AS days, count(*) AS n FROM act WHERE user_id <= 4 GROUP BY user_id ORDER BY user_id;
WITH c AS (SELECT u.id, u.country, u.signup, date_trunc('month', u.signup)::date AS cohort FROM users u), a AS (SELECT user_id, count(*) AS n, sum(amount) AS spent FROM events GROUP BY user_id) SELECT c.cohort, count(*) AS users, count(a.user_id) AS active, round(avg(a.n), 3) AS avg_events, sum(a.spent) AS spent FROM c LEFT JOIN a ON a.user_id = c.id GROUP BY c.cohort ORDER BY c.cohort;
WITH f AS (SELECT user_id, min(ts) AS first_ts FROM events GROUP BY user_id) SELECT date_trunc('day', f.first_ts)::date AS cohort_day, date_trunc('day', e.ts)::date - date_trunc('day', f.first_ts)::date AS age_days, count(DISTINCT e.user_id) AS users FROM f JOIN events e USING (user_id) GROUP BY 1, 2 ORDER BY 1, 2 LIMIT 12;
SELECT country, count(DISTINCT user_id) AS buyers, sum(amount) AS revenue, round(sum(amount) / count(DISTINCT user_id), 2) AS arpu FROM events JOIN users ON users.id = user_id WHERE kind = 'buy' GROUP BY country ORDER BY country;

-- ### 11_analytics/time-buckets-gaps
SET search_path = s11;
-- ClickHouse: toStartOfHour / toStartOfInterval / toStartOfDay
SELECT date_trunc('day', ts) AS day, count(*) AS n, sum(amount) AS s FROM events GROUP BY 1 ORDER BY 1;
SELECT date_trunc('hour', ts) AS h, count(*) FROM events WHERE ts < '2024-03-01 06:00' GROUP BY 1 ORDER BY 1;
SELECT TIMESTAMP '2024-03-01 00:00' + floor(extract(epoch FROM ts - TIMESTAMP '2024-03-01 00:00') / 900) * INTERVAL '15 minutes' AS bucket15, count(*) FROM events WHERE ts < '2024-03-01 03:00' GROUP BY 1 ORDER BY 1;
SELECT to_timestamp(floor(extract(epoch FROM ts) / 3600) * 3600) AT TIME ZONE 'UTC' AS h, count(*) FROM events WHERE ts < '2024-03-01 03:00' GROUP BY 1 ORDER BY 1;
SELECT date_bin('6 hours', ts, TIMESTAMP '2024-03-01') AS b6, count(*) FROM events GROUP BY 1 ORDER BY 1 LIMIT 6;
SELECT date_trunc('week', ts)::date AS wk, extract(isodow FROM ts) AS dow, count(*) FROM events GROUP BY 1, 2 ORDER BY 1, 2 LIMIT 7;
-- ClickHouse: ORDER BY ... WITH FILL (dense time series)
SELECT d::date AS day, coalesce(x.n, 0) AS n FROM generate_series(DATE '2024-02-28', DATE '2024-03-12', INTERVAL '1 day') d LEFT JOIN (SELECT date_trunc('day', ts)::date AS day, count(*) AS n FROM events GROUP BY 1) x ON x.day = d::date ORDER BY d;
SELECT g.h, coalesce(c.n, 0) AS n, sum(coalesce(c.n, 0)) OVER (ORDER BY g.h) AS cum FROM generate_series(TIMESTAMP '2024-03-01 00:00', TIMESTAMP '2024-03-01 11:00', INTERVAL '1 hour') g(h) LEFT JOIN (SELECT date_trunc('hour', ts) AS h, count(*) AS n FROM events GROUP BY 1) c ON c.h = g.h ORDER BY g.h;
-- sparse series: only days with data, plus the previous value carried forward (ClickHouse: WITH FILL + INTERPOLATE)
WITH days AS (SELECT d::date AS day FROM generate_series(DATE '2024-03-01', DATE '2024-03-10', INTERVAL '1 day') d), v AS (SELECT date_trunc('day', ts)::date AS day, sum(amount) AS s FROM events WHERE user_id = 3 AND kind = 'buy' GROUP BY 1) SELECT days.day, v.s, (SELECT s FROM v v2 WHERE v2.day <= days.day ORDER BY v2.day DESC LIMIT 1) AS carried FROM days LEFT JOIN v USING (day) ORDER BY days.day;
-- gaps and islands: consecutive-day runs per user
WITH d AS (SELECT DISTINCT user_id, date_trunc('day', ts)::date AS day FROM events WHERE user_id <= 3), g AS (SELECT user_id, day, day - (row_number() OVER (PARTITION BY user_id ORDER BY day))::int AS grp FROM d) SELECT user_id, min(day) AS start_day, max(day) AS end_day, count(*) AS days FROM g GROUP BY user_id, grp ORDER BY user_id, start_day;
SELECT day, n, n - lag(n) OVER (ORDER BY day) AS delta, round(100.0 * (n - lag(n) OVER (ORDER BY day)) / nullif(lag(n) OVER (ORDER BY day), 0), 2) AS pct FROM (SELECT date_trunc('day', ts)::date AS day, count(*) AS n FROM events GROUP BY 1) t ORDER BY day;
SELECT day, n, round(avg(n) OVER (ORDER BY day ROWS BETWEEN 2 PRECEDING AND CURRENT ROW), 3) AS ma3, sum(n) OVER (ORDER BY day) AS cum FROM (SELECT date_trunc('day', ts)::date AS day, count(*) AS n FROM events GROUP BY 1) t ORDER BY day;
SELECT extract(hour FROM ts)::int AS hr, count(*) FROM events GROUP BY 1 ORDER BY 1 LIMIT 5;
SELECT date_trunc('month', signup)::date AS m, count(*) FROM users GROUP BY 1 ORDER BY 1;
SELECT to_char(ts, 'YYYY-MM-DD') AS day, to_char(ts, 'HH24') AS hr, count(*) FROM events WHERE ts < '2024-03-01 02:00' GROUP BY 1, 2 ORDER BY 1, 2;

-- ### 11_analytics/topk-quantiles-histograms
SET search_path = s11;
-- ClickHouse: topK(3)(kind), uniqExact(user_id), quantileExact(0.5)(amount), histogram(5)(amount)
SELECT kind, count(*) AS c FROM events GROUP BY kind ORDER BY c DESC, kind LIMIT 3;
SELECT user_id, count(*) AS c FROM events GROUP BY user_id ORDER BY c DESC, user_id LIMIT 5;
SELECT country, user_id, c FROM (SELECT u.country, e.user_id, count(*) AS c, row_number() OVER (PARTITION BY u.country ORDER BY count(*) DESC, e.user_id) AS rn FROM events e JOIN users u ON u.id = e.user_id GROUP BY u.country, e.user_id) t WHERE rn <= 2 ORDER BY country, rn;
SELECT count(DISTINCT user_id) AS uniq_exact, count(*) AS total, count(DISTINCT (user_id, kind)) AS uniq_pairs, count(DISTINCT date_trunc('day', ts)) AS days FROM events;
SELECT date_trunc('day', ts)::date AS day, count(DISTINCT user_id) AS dau FROM events GROUP BY 1 ORDER BY 1;
SELECT kind, count(DISTINCT user_id) FILTER (WHERE amount >= 50) AS big_spenders, count(*) FILTER (WHERE amount >= 50) AS big FROM events GROUP BY kind ORDER BY kind;
SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY amount) AS q50_exact, percentile_cont(0.5) WITHIN GROUP (ORDER BY amount) AS median, percentile_disc(ARRAY[0.1, 0.5, 0.9, 0.99]) WITHIN GROUP (ORDER BY amount) AS qs FROM events;
SELECT kind, percentile_cont(ARRAY[0.5, 0.9]) WITHIN GROUP (ORDER BY amount) AS p50_p90, min(amount), max(amount), round(avg(amount), 4) FROM events GROUP BY kind ORDER BY kind;
SELECT user_id, percentile_disc(0.5) WITHIN GROUP (ORDER BY amount) AS med FROM events WHERE user_id <= 5 GROUP BY user_id ORDER BY user_id;
SELECT country, round(percentile_cont(0.95) WITHIN GROUP (ORDER BY ms)::numeric, 3) AS p95 FROM hits JOIN users ON users.id = hits.uid GROUP BY country ORDER BY country;
SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY ms), percentile_disc(0.99) WITHIN GROUP (ORDER BY ms), max(ms) FROM hits;
SELECT width_bucket(amount, 0, 100, 5) AS b, count(*), min(amount), max(amount) FROM events GROUP BY 1 ORDER BY 1;
SELECT floor(amount / 20) * 20 AS lo, count(*) FROM events GROUP BY 1 ORDER BY 1;
SELECT b, lo, hi, n, repeat('*', (n * 40 / max(n) OVER ())::int) AS bar FROM (SELECT width_bucket(ms, 50, 450, 8) AS b, 50 + (width_bucket(ms, 50, 450, 8) - 1) * 50 AS lo, 50 + width_bucket(ms, 50, 450, 8) * 50 AS hi, count(*) AS n FROM hits GROUP BY 1) t ORDER BY b;
SELECT bucket, count(*) FROM (SELECT ntile(4) OVER (ORDER BY amount, id) AS bucket FROM events) t GROUP BY bucket ORDER BY bucket;
SELECT round(avg(amount), 4), round(stddev_samp(amount), 4), round(var_pop(amount), 4), round(corr(amount, user_id)::numeric, 6) FROM events;
SELECT kind, round(sum(amount), 2) AS s, round(100.0 * sum(amount) / sum(sum(amount)) OVER (), 3) AS share FROM events GROUP BY kind ORDER BY kind;
SELECT mode() WITHIN GROUP (ORDER BY kind) AS most_common, mode() WITHIN GROUP (ORDER BY user_id) AS busiest FROM events;

-- ### 11_analytics/argmax-pivot-arrayjoin
SET search_path = s11;
-- ClickHouse: argMax(url, ts), argMin(url, ts) per group
SELECT DISTINCT ON (uid) uid, url AS last_url, ts FROM hits ORDER BY uid, ts DESC, id DESC;
SELECT DISTINCT ON (uid) uid, url AS first_url, ts FROM hits ORDER BY uid, ts, id;
SELECT uid, (array_agg(url ORDER BY ts DESC, id DESC))[1] AS arg_max_url, (array_agg(url ORDER BY ts, id))[1] AS arg_min_url, (array_agg(ms ORDER BY ms DESC, id))[1] AS max_ms FROM hits GROUP BY uid ORDER BY uid;
SELECT uid, (SELECT url FROM hits h2 WHERE h2.uid = h.uid ORDER BY ms DESC, id LIMIT 1) AS slowest_url FROM (SELECT DISTINCT uid FROM hits) h ORDER BY uid;
SELECT user_id, kind, amount FROM (SELECT user_id, kind, amount, row_number() OVER (PARTITION BY user_id ORDER BY amount DESC, id) AS rn FROM events WHERE user_id <= 4) t WHERE rn = 1 ORDER BY user_id;
-- ClickHouse: sumIf / countIf pivot
SELECT user_id, count(*) FILTER (WHERE kind = 'view') AS views, count(*) FILTER (WHERE kind = 'click') AS clicks, count(*) FILTER (WHERE kind = 'cart') AS carts, count(*) FILTER (WHERE kind = 'buy') AS buys, sum(amount) FILTER (WHERE kind = 'buy') AS revenue FROM events WHERE user_id <= 5 GROUP BY user_id ORDER BY user_id;
SELECT date_trunc('day', ts)::date AS day, sum(amount) FILTER (WHERE kind = 'view') AS v, sum(amount) FILTER (WHERE kind = 'buy') AS b, round(100.0 * count(*) FILTER (WHERE kind = 'buy') / count(*), 2) AS conv FROM events GROUP BY 1 ORDER BY 1;
SELECT country, sum(CASE WHEN kind = 'buy' THEN amount ELSE 0 END) AS buy_amt, sum((kind = 'buy')::int) AS buys, avg(amount) FILTER (WHERE kind = 'cart') AS avg_cart FROM events JOIN users ON users.id = user_id GROUP BY country ORDER BY country;
-- ClickHouse: groupArray, arrayJoin, arrayMap equivalents
SELECT user_id, array_agg(id ORDER BY id) AS ids, array_agg(DISTINCT kind ORDER BY kind) AS kinds FROM events WHERE user_id <= 3 GROUP BY user_id ORDER BY user_id;
SELECT user_id, (array_agg(amount ORDER BY ts, id))[1:3] AS first3 FROM events WHERE user_id <= 3 GROUP BY user_id ORDER BY user_id;
SELECT user_id, x, ord FROM (SELECT user_id, array_agg(kind ORDER BY ts, id) AS path FROM events WHERE user_id <= 2 GROUP BY user_id) t, unnest(path) WITH ORDINALITY AS u(x, ord) WHERE ord <= 3 ORDER BY user_id, ord;
SELECT tag, count(*) FROM (SELECT unnest(ARRAY[kind, country]) AS tag FROM events JOIN users ON users.id = user_id WHERE events.id <= 20) t GROUP BY tag ORDER BY 2 DESC, 1;
SELECT t.id, k FROM (VALUES (1, ARRAY['a', 'b']), (2, ARRAY['c']), (3, ARRAY[]::text[])) AS t(id, tags) CROSS JOIN LATERAL unnest(t.tags) AS k ORDER BY 1, 2;
SELECT t.id, k FROM (VALUES (1, ARRAY['a', 'b']), (2, ARRAY['c']), (3, ARRAY[]::text[])) AS t(id, tags) LEFT JOIN LATERAL unnest(t.tags) AS k ON true ORDER BY 1, 2;
SELECT id, jsonb_array_elements_text(tags) FROM (VALUES (1, '["x", "y"]'::jsonb), (2, '["z"]')) AS t(id, tags) ORDER BY 1, 2;
SELECT id, e.key, e.value FROM (VALUES (1, '{"p": 1, "q": 2}'::jsonb)) AS t(id, doc), jsonb_each_text(doc) AS e ORDER BY 2;
SELECT array(SELECT x * x FROM unnest(ARRAY[1, 2, 3]) AS x), ARRAY(SELECT x FROM unnest(ARRAY[3, 1, 2]) AS x ORDER BY x DESC), (SELECT sum(x) FROM unnest(ARRAY[1, 2, 3]) AS x), (SELECT count(*) FROM unnest(ARRAY[1, 2, 3]) AS x WHERE x > 1);
SELECT array_length(array_agg(DISTINCT user_id), 1), cardinality(array_agg(user_id)) FROM events;
SELECT g, x FROM generate_series(1, 3) g, LATERAL generate_series(1, g) x ORDER BY g, x;
SELECT s FROM generate_subscripts(ARRAY[10, 20, 30], 1) s;

-- ### 11_analytics/running-metrics
SET search_path = s11;
SELECT day, revenue, sum(revenue) OVER (ORDER BY day) AS cum_revenue, round(avg(revenue) OVER (ORDER BY day ROWS BETWEEN 2 PRECEDING AND CURRENT ROW), 3) AS ma3, max(revenue) OVER (ORDER BY day ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS peak, rank() OVER (ORDER BY revenue DESC) AS rk FROM (SELECT date_trunc('day', ts)::date AS day, sum(amount) AS revenue FROM events WHERE kind = 'buy' GROUP BY 1) t ORDER BY day;
SELECT user_id, ts, amount, sum(amount) OVER (PARTITION BY user_id ORDER BY ts, id) AS running, count(*) OVER (PARTITION BY user_id ORDER BY ts, id) AS nth FROM events WHERE user_id = 1 ORDER BY ts, id LIMIT 8;
SELECT id, ts, ts - lag(ts) OVER (PARTITION BY user_id ORDER BY ts, id) AS since_prev, first_value(kind) OVER (PARTITION BY user_id ORDER BY ts, id) AS first_kind FROM events WHERE user_id = 2 ORDER BY ts, id LIMIT 6;
SELECT user_id, sum(amount) AS total, rank() OVER (ORDER BY sum(amount) DESC, user_id) AS rk, round(100.0 * sum(sum(amount)) OVER (ORDER BY sum(amount) DESC, user_id) / sum(sum(amount)) OVER (), 3) AS cum_share FROM events GROUP BY user_id ORDER BY rk LIMIT 5;
SELECT kind, day, n, sum(n) OVER (PARTITION BY kind ORDER BY day) AS cum FROM (SELECT kind, date_trunc('day', ts)::date AS day, count(*) AS n FROM events GROUP BY 1, 2) t ORDER BY kind, day LIMIT 8;
SELECT date_trunc('day', ts)::date AS day, count(*) AS n, count(*) FILTER (WHERE kind = 'buy') AS buys, round(avg(amount) FILTER (WHERE kind = 'buy'), 3) AS avg_buy FROM events GROUP BY ROLLUP (1) ORDER BY 1 NULLS LAST;
SELECT country, kind, count(*), sum(amount) FROM events JOIN users ON users.id = user_id GROUP BY CUBE (country, kind) ORDER BY country NULLS LAST, kind NULLS LAST LIMIT 12;
SELECT u.id, u.country, count(e.id) AS ev, coalesce(sum(e.amount), 0) AS amt FROM users u LEFT JOIN events e ON e.user_id = u.id GROUP BY u.id, u.country HAVING count(e.id) > 0 ORDER BY amt DESC, u.id LIMIT 5;
SELECT * FROM (SELECT user_id, ts, kind, lead(kind) OVER (PARTITION BY user_id ORDER BY ts, id) AS next_kind FROM events) t WHERE kind = 'cart' AND next_kind = 'buy' AND user_id <= 10 ORDER BY user_id, ts;

-- ### 11_analytics/teardown
DROP SCHEMA s11 CASCADE;
