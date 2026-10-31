-- Suite group 08: aggregates. Syntax source: PostgreSQL 16 docs, section 9.21 (aggregate functions),
-- 7.2.4 (GROUPING SETS, CUBE, ROLLUP), 4.2.7 (aggregate expressions, FILTER, ORDER BY inside).

-- ### 08_aggregates/setup
DROP SCHEMA IF EXISTS s08 CASCADE;
CREATE SCHEMA s08;
SET search_path = s08;
CREATE TABLE users (id int PRIMARY KEY, name text, country text, signup date, age int, vip boolean);
INSERT INTO users SELECT g AS id, 'user' || lpad(g::text, 4, '0') AS name, (ARRAY['VN', 'US', 'DE', 'JP', 'FR'])[1 + g % 5] AS country, DATE '2024-01-01' + (g % 90) AS signup, 18 + (g * 7) % 50 AS age, (g % 4 = 0) AS vip FROM generate_series(1, @N@) g;
CREATE TABLE events (id int PRIMARY KEY, user_id int, ts timestamp, kind text, amount numeric(10,2));
INSERT INTO events SELECT g AS id, 1 + ((g::bigint * 7) % @N@)::int AS user_id, TIMESTAMP '2024-03-01 00:00:00' + ((g::bigint * 1234) % 864000) * INTERVAL '1 second' AS ts, (ARRAY['view', 'click', 'cart', 'buy'])[1 + ((g::bigint * g) % 4)::int] AS kind, (((g::bigint * 37) % 1000)::numeric / 10)::numeric(10,2) AS amount FROM generate_series(1, @N@ * 10) g;
CREATE TABLE sales (id int PRIMARY KEY, region text, prod text, qty int, price numeric(8,2), flag boolean);
INSERT INTO sales SELECT g, (ARRAY['north', 'south', 'east', 'west'])[1 + g % 4], (ARRAY['a', 'b', 'c'])[1 + g % 3], 1 + (g * 7) % 9, (10 + (g * 13) % 50)::numeric, g % 5 <> 0 FROM generate_series(1, 36) g;
INSERT INTO sales VALUES (100, NULL, 'a', NULL, NULL, NULL);

-- ### 08_aggregates/basic-group-by
SET search_path = s08;
SELECT count(*), count(qty), count(DISTINCT region), sum(qty), avg(qty), min(qty), max(qty), sum(price), avg(price), min(prod), max(prod) FROM sales;
SELECT region, count(*), sum(qty), round(avg(price), 2) FROM sales GROUP BY region ORDER BY region NULLS LAST;
SELECT region, prod, sum(qty) FROM sales GROUP BY region, prod ORDER BY region NULLS LAST, prod;
SELECT region, count(*) FROM sales GROUP BY region HAVING count(*) > 9 ORDER BY region;
SELECT region, sum(qty) AS s FROM sales GROUP BY region HAVING sum(qty) > 50 AND region IS NOT NULL ORDER BY s DESC, region;
SELECT region, sum(qty) AS s FROM sales GROUP BY 1 ORDER BY 2 DESC NULLS LAST, 1;
SELECT region AS r, count(*) FROM sales GROUP BY r ORDER BY r NULLS LAST;
SELECT lower(region), count(*) FROM sales GROUP BY lower(region) ORDER BY 1 NULLS LAST;
SELECT qty % 3 AS m, count(*) FROM sales GROUP BY qty % 3 ORDER BY m NULLS LAST;
SELECT count(*) FROM sales WHERE false;
SELECT sum(qty), avg(qty), min(qty), max(qty), count(qty) FROM sales WHERE false;
SELECT region, count(*) FROM sales WHERE false GROUP BY region;
SELECT sum(qty) FROM sales GROUP BY ();
SELECT region, qty FROM sales GROUP BY region;
SELECT region, count(*) FROM sales;
SELECT count(*) FROM sales GROUP BY nope;
SELECT sum(prod) FROM sales;
SELECT count(*) FILTER (WHERE flag), count(*) FILTER (WHERE NOT flag), count(*) FILTER (WHERE flag IS NULL), sum(qty) FILTER (WHERE region = 'north'), avg(price) FILTER (WHERE prod = 'a') FROM sales;
SELECT region, count(*) FILTER (WHERE prod = 'a') AS a, count(*) FILTER (WHERE prod = 'b') AS b, count(*) FILTER (WHERE prod = 'c') AS c FROM sales GROUP BY region ORDER BY region NULLS LAST;
SELECT count(DISTINCT prod), count(DISTINCT (region, prod)), sum(DISTINCT qty), avg(DISTINCT qty), count(DISTINCT qty) FILTER (WHERE flag) FROM sales;
SELECT string_agg(prod, ',' ORDER BY id) FROM sales WHERE id <= 6;
SELECT string_agg(DISTINCT prod, ',' ORDER BY prod) FROM sales;
SELECT region, string_agg(prod, '' ORDER BY id) FROM sales WHERE id <= 12 GROUP BY region ORDER BY region;
SELECT string_agg(region, ','), string_agg(region, ',' ORDER BY id) FROM sales WHERE id IN (1, 2, 100);
SELECT array_agg(id ORDER BY id DESC) FROM sales WHERE id <= 5;
SELECT array_agg(region ORDER BY id) FROM sales WHERE id IN (1, 100, 2);
SELECT array_agg(DISTINCT qty ORDER BY qty) FROM sales;
SELECT bool_and(flag), bool_or(flag), every(qty > 0), bool_and(qty > 5), bool_or(qty > 8) FROM sales;
SELECT region, bool_and(flag), bool_or(NOT flag) FROM sales GROUP BY region ORDER BY region NULLS LAST;
SELECT bit_and(qty), bit_or(qty), bit_xor(qty) FROM sales;
SELECT jsonb_agg(DISTINCT prod ORDER BY prod), jsonb_object_agg(id, prod ORDER BY id) FROM sales WHERE id <= 3;
SELECT min(region), max(region), min(price), max(price), min(id), max(id) FROM sales;
SELECT sum(qty)::float8 / count(qty), sum(price) / sum(qty) FROM sales;
SELECT count(*) AS n FROM (SELECT DISTINCT region, prod FROM sales) z;
SELECT max(region) FILTER (WHERE false), count(*) FILTER (WHERE true) FROM sales;
SELECT sum(qty) OVER (), count(*) FROM sales;
SELECT sum(sum(qty)) FROM sales;
SELECT region, sum(qty) FROM sales WHERE sum(qty) > 1 GROUP BY region;

-- ### 08_aggregates/grouping-sets
SET search_path = s08;
SELECT region, prod, sum(qty) FROM sales WHERE region IS NOT NULL GROUP BY ROLLUP (region, prod) ORDER BY region NULLS LAST, prod NULLS LAST;
SELECT region, prod, sum(qty) FROM sales WHERE region IS NOT NULL GROUP BY CUBE (region, prod) ORDER BY region NULLS LAST, prod NULLS LAST;
SELECT region, prod, sum(qty) FROM sales WHERE region IS NOT NULL GROUP BY GROUPING SETS ((region), (prod), ()) ORDER BY region NULLS LAST, prod NULLS LAST;
SELECT region, prod, sum(qty) FROM sales WHERE region IS NOT NULL GROUP BY GROUPING SETS ((region, prod), (region), ()) ORDER BY region NULLS LAST, prod NULLS LAST;
SELECT region, prod, grouping(region), grouping(prod), grouping(region, prod), count(*) FROM sales WHERE region IS NOT NULL GROUP BY ROLLUP (region, prod) ORDER BY 3, 4, region NULLS LAST, prod NULLS LAST;
SELECT CASE WHEN grouping(region) = 1 THEN 'ALL' ELSE region END AS r, sum(qty) FROM sales GROUP BY ROLLUP (region) ORDER BY 1;
SELECT region, prod, sum(qty) FROM sales GROUP BY region, ROLLUP (prod) ORDER BY region NULLS LAST, prod NULLS LAST;
SELECT region, sum(qty) FROM sales GROUP BY GROUPING SETS (region, ()) HAVING sum(qty) > 10 ORDER BY region NULLS LAST;
SELECT region, prod, sum(qty) FROM sales GROUP BY ROLLUP ((region, prod)) ORDER BY region NULLS LAST, prod NULLS LAST;
SELECT region, grouping(prod) FROM sales GROUP BY region;
SELECT a, b, count(*) FROM (VALUES (1, 1), (1, 2), (2, 1)) AS t(a, b) GROUP BY CUBE (a, b) ORDER BY a NULLS LAST, b NULLS LAST;
SELECT a, count(*) FROM (VALUES (1), (1), (NULL)) AS t(a) GROUP BY ROLLUP (a) ORDER BY a NULLS LAST, 2;
SELECT a, grouping(a), count(*) FROM (VALUES (1), (1), (NULL)) AS t(a) GROUP BY ROLLUP (a) ORDER BY 2, a NULLS LAST;

-- ### 08_aggregates/statistical
SET search_path = s08;
SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY qty), percentile_disc(0.5) WITHIN GROUP (ORDER BY qty), percentile_cont(0.9) WITHIN GROUP (ORDER BY price), percentile_disc(0.9) WITHIN GROUP (ORDER BY price) FROM sales;
SELECT percentile_cont(ARRAY[0.25, 0.5, 0.75]) WITHIN GROUP (ORDER BY qty), percentile_disc(ARRAY[0.1, 0.9]) WITHIN GROUP (ORDER BY price) FROM sales;
SELECT region, percentile_cont(0.5) WITHIN GROUP (ORDER BY price) FROM sales GROUP BY region ORDER BY region NULLS LAST;
SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY ts) FROM (VALUES (TIMESTAMP '2024-01-01'), (TIMESTAMP '2024-01-03')) t(ts);
SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY i) FROM (VALUES (INTERVAL '1 hour'), (INTERVAL '3 hours')) t(i);
SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY prod) FROM sales;
SELECT percentile_cont(1.5) WITHIN GROUP (ORDER BY qty) FROM sales;
SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY prod) FROM sales;
SELECT mode() WITHIN GROUP (ORDER BY qty), mode() WITHIN GROUP (ORDER BY prod), mode() WITHIN GROUP (ORDER BY region) FROM sales;
SELECT region, mode() WITHIN GROUP (ORDER BY prod) FROM sales GROUP BY region ORDER BY region NULLS LAST;
SELECT rank(5) WITHIN GROUP (ORDER BY qty), dense_rank(5) WITHIN GROUP (ORDER BY qty), percent_rank(5) WITHIN GROUP (ORDER BY qty), cume_dist(5) WITHIN GROUP (ORDER BY qty) FROM sales;
SELECT stddev(qty), stddev_pop(qty), stddev_samp(qty), variance(qty), var_pop(qty), var_samp(qty) FROM sales;
SELECT round(stddev(price), 6), round(variance(price), 6), round(avg(price), 6) FROM sales;
SELECT stddev(qty) FROM sales WHERE id = 1;
SELECT stddev_pop(qty), var_pop(qty) FROM sales WHERE id = 1;
SELECT round(corr(qty, price)::numeric, 8), round(covar_pop(qty, price)::numeric, 8), round(covar_samp(qty, price)::numeric, 8), round(regr_slope(price, qty)::numeric, 8), round(regr_intercept(price, qty)::numeric, 8), round(regr_r2(price, qty)::numeric, 8), regr_count(price, qty), round(regr_avgx(price, qty)::numeric, 8), round(regr_avgy(price, qty)::numeric, 8) FROM sales;
SELECT corr(x, y), regr_slope(y, x) FROM (VALUES (1, 2), (2, 4), (3, 6)) t(x, y);
SELECT corr(x, y) FROM (VALUES (1, 2), (1, 4)) t(x, y);
SELECT avg(x), sum(x), avg(x::numeric), sum(x::bigint), avg(x::float8), sum(x::float4), pg_typeof(avg(x))::text, pg_typeof(sum(x))::text, pg_typeof(sum(x::bigint))::text, pg_typeof(sum(x::float4))::text, pg_typeof(count(*))::text FROM (VALUES (1), (2), (4)) t(x);
SELECT avg(i) FROM (VALUES (INTERVAL '1 hour'), (INTERVAL '2 hours')) t(i);
SELECT sum(i) FROM (VALUES (INTERVAL '1 hour'), (INTERVAL '2 hours')) t(i);
SELECT min(d), max(d) FROM (VALUES (DATE '2024-01-01'), (DATE '2024-03-01')) t(d);
SELECT min(t), max(t) FROM (VALUES ('b'::text), ('a'), ('C')) t(t);
SELECT min(a), max(a) FROM (VALUES (ARRAY[1, 2]), (ARRAY[1, 3])) t(a);
SELECT count(*) FROM (SELECT 1 UNION ALL SELECT 1) t;
SELECT sum(x), avg(x) FROM (VALUES (0.1), (0.2), (0.3)) t(x);
SELECT sum(x), avg(x) FROM (VALUES (0.1::float8), (0.2), (0.3)) t(x);
SELECT sum(x) FROM (VALUES (9223372036854775807::bigint), (1)) t(x);
SELECT sum(x::int) FROM (VALUES (2147483647), (1)) t(x);
SELECT any_value(x) IS NOT NULL FROM (VALUES (1), (2)) t(x);
SELECT min(x), max(x) FROM (VALUES ('NaN'::float8), (1), (2)) t(x);
SELECT json_agg(x ORDER BY x), json_agg(DISTINCT x ORDER BY x) FROM (VALUES (2), (1), (2)) t(x);
SELECT xmlagg(x::xml) FROM (VALUES ('<a/>'), ('<b/>')) t(x);
SELECT sum(x ORDER BY x) FROM (VALUES (1), (2)) t(x);
SELECT count(ALL x), count(DISTINCT x) FROM (VALUES (1), (1), (NULL)) t(x);
SELECT first_val FROM (SELECT (array_agg(x ORDER BY x))[1] AS first_val FROM (VALUES (3), (1), (2)) t(x)) z;

-- ### 08_aggregates/teardown
DROP SCHEMA s08 CASCADE;
