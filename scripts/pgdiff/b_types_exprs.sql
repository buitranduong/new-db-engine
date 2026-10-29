-- Group b: types and expressions.

-- ### b/arithmetic
SELECT 1 + 2, 5 - 7, 3 * 4, 7 / 2, 7 % 3, -7 % 3, 2 ^ 10;
SELECT 7 / 2.0, 7.0 / 2, 1 / 3.0;
SELECT 0.1 + 0.2, 0.1::float8 + 0.2::float8;
SELECT 9223372036854775807::bigint, -9223372036854775808::bigint;
SELECT 2147483647 + 1;
SELECT 9223372036854775807 + 1;
SELECT pg_typeof(1)::text, pg_typeof(1.5)::text, pg_typeof(1::bigint)::text, pg_typeof(1::smallint + 1)::text;
SELECT abs(-5), abs(-5.5), sign(-3), ceil(1.2), floor(-1.2), round(2.5), round(3.5), round(2.567, 2), trunc(2.99), trunc(-2.99);
SELECT sqrt(16), sqrt(2), power(2, 8), power(2.0, 0.5), exp(0), ln(1), log(100), mod(10, 3), div(7, 2);
SELECT 5 & 3, 5 | 3, 5 # 3, ~5, 1 << 4, 256 >> 2;
SELECT greatest(1, 5, 3), least(4, 2, 9), greatest(1, NULL, 3), least('b', 'a');
SELECT 1.0::numeric, 1.50::numeric, 100::numeric(5,2), 1e3::numeric, 1.23e-4::numeric;
SELECT 12345678901234567890.123456789::numeric;
SELECT (0.1::numeric + 0.2::numeric), 1.1::numeric * 1.1::numeric, 10::numeric / 3;
SELECT 123456789012345678901234567890::numeric * 10;
SELECT 1e308::float8 * 10, -1e308::float8 * 10, 'NaN'::float8, 'Infinity'::float8;
SELECT 1.5::float4, 0.1::float4, 3.14159274::float4::float8;
SELECT 1.0::float8, 100000000000000000000::float8, 1e-7::float8, 123456789.123456789::float8;
SELECT sum(x), avg(x) FROM (VALUES (1), (2), (4)) AS t(x);
SELECT sum(x::numeric), avg(x::numeric) FROM (VALUES (1), (2), (4)) AS t(x);

-- ### b/strings
SELECT 'abc' || 'def', 'abc' || 1, 1 || 2;
SELECT length('héllo'), char_length('héllo'), octet_length('héllo'), bit_length('a');
SELECT upper('abc'), lower('ABC'), initcap('hello world');
SELECT substring('hello' FROM 2 FOR 3), substr('hello', 2), substr('hello', 0, 3), substring('hello', -1, 3);
SELECT position('lo' IN 'hello'), strpos('hello', 'l'), strpos('hello', 'z');
SELECT left('hello', 2), right('hello', 2), left('hello', -1), right('hello', -1);
SELECT trim('  x  '), ltrim('  x'), rtrim('x  '), btrim('xxhixx', 'x'), trim(both 'x' from 'xxhixx');
SELECT lpad('5', 3, '0'), rpad('5', 3, '*'), lpad('hello', 3), repeat('ab', 3), reverse('abc');
SELECT replace('banana', 'an', 'AN'), translate('12345', '143', 'ax'), overlay('Txxxxas' placing 'hom' from 2 for 4);
SELECT split_part('a,b,c', ',', 2), split_part('a,b,c', ',', 5), split_part('a,b,c', ',', -1);
SELECT concat('a', NULL, 'b'), concat_ws('-', 'a', NULL, 'b'), concat(1, 2);
SELECT 'a' || NULL, NULL || 'a';
SELECT ascii('A'), chr(66), chr(8364), md5('abc');
SELECT format('%s-%s', 'a', 1), format('%I', 'Hello World'), format('%L', 'it''s'), format('%5s|%-5s|', 'a', 'b');
SELECT quote_ident('a b'), quote_literal('a''b'), quote_nullable(NULL);
SELECT to_char(1234.5, 'FM9999.00'), to_char(0.5, '0.00'), to_char(1234567, 'FM9,999,999'), to_char(-5, 'FM000');
SELECT starts_with('hello', 'he'), 'hello' ^@ 'he';
SELECT regexp_replace('abc123', '[0-9]+', '#'), regexp_replace('aaa', 'a', 'b', 'g'), 'abc' ~ '^a', 'abc' ~* 'B', 'abc' !~ 'z';
SELECT (regexp_match('foo123bar', '([0-9]+)'))[1], regexp_split_to_array('a1b2c', '[0-9]');
SELECT 'abc' < 'abd', 'a' < 'B', 'Z' < 'a', 'abc' = 'ABC', 'é' > 'e';
SELECT 'a' COLLATE "C" < 'B' COLLATE "C";
SELECT 'abc'::char(5) || 'x', length('abc'::char(5)), 'abc'::varchar(2);
SELECT 'x'::text, 'x'::name, 'x'::"char", pg_typeof('x')::text, pg_typeof('x'::varchar)::text, pg_typeof('x'::char(3))::text;
SELECT E'a\tb', E'line1\nline2', 'a\tb', $$dollar 'quoted'$$, U&'\0041';
SELECT string_agg(x, ',' ORDER BY x) FROM (VALUES ('b'), ('a'), ('c')) AS t(x);

-- ### b/null-logic
SELECT NULL = NULL, NULL IS NULL, NULL IS NOT NULL, 1 = NULL, NULL <> 1;
SELECT NULL AND FALSE, NULL AND TRUE, NULL OR TRUE, NULL OR FALSE, NOT NULL::boolean;
SELECT NULL IS DISTINCT FROM NULL, 1 IS DISTINCT FROM NULL, 1 IS NOT DISTINCT FROM 1;
SELECT TRUE IS TRUE, NULL::boolean IS TRUE, NULL::boolean IS NOT TRUE, NULL::boolean IS UNKNOWN, FALSE IS FALSE;
SELECT COALESCE(NULL, NULL, 3), COALESCE(NULL, 'a'), NULLIF(1, 1), NULLIF(1, 2), NULLIF('a', 'A');
SELECT 1 IN (1, NULL), 2 IN (1, NULL), 2 NOT IN (1, NULL), NULL IN (1);
SELECT COUNT(*), COUNT(x), SUM(x), AVG(x), MIN(x), MAX(x) FROM (VALUES (1), (NULL), (3)) AS t(x);
SELECT COUNT(*), COUNT(x), SUM(x), MAX(x) FROM (SELECT 1 AS x WHERE FALSE) AS t;
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x;
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x DESC;
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x NULLS FIRST;
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x DESC NULLS LAST;
SELECT DISTINCT x FROM (VALUES (1), (NULL), (NULL), (1)) AS t(x);
SELECT x, COUNT(*) FROM (VALUES (1), (NULL), (NULL), (1)) AS t(x) GROUP BY x;

-- ### b/case-coalesce
SELECT CASE WHEN 1 = 1 THEN 'y' ELSE 'n' END, CASE 2 WHEN 1 THEN 'one' WHEN 2 THEN 'two' END, CASE WHEN FALSE THEN 1 END;
SELECT CASE WHEN x > 2 THEN 'big' WHEN x > 1 THEN 'mid' ELSE 'small' END FROM (VALUES (1), (2), (3)) AS t(x);
SELECT CASE WHEN TRUE THEN 1 ELSE 'a' END;
SELECT CASE WHEN TRUE THEN 1 ELSE 2.5 END, pg_typeof(CASE WHEN TRUE THEN 1 ELSE 2.5 END)::text;
SELECT CASE WHEN 1/0 = 1 THEN 1 ELSE 0 END;
SELECT CASE WHEN TRUE THEN 1 ELSE 1/0 END;
SELECT COALESCE(1, 'a');
SELECT pg_typeof(COALESCE(NULL, 1))::text, pg_typeof(COALESCE(NULL, 1.5))::text, pg_typeof(NULL)::text;

-- ### b/timestamps
SELECT '2026-01-15 10:30:45'::timestamp, '2026-01-15 10:30:45.123456'::timestamp, '2026-01-15'::date;
SELECT '2026-01-15 10:30:45+07'::timestamptz, '2026-01-15 10:30:45'::timestamptz;
SELECT '2026-01-15 10:30:45.5+00'::timestamptz;
SELECT pg_typeof('2026-01-15'::date)::text, pg_typeof('2026-01-15 10:00'::timestamp)::text, pg_typeof('2026-01-15 10:00+00'::timestamptz)::text, pg_typeof('10:00'::time)::text;
SELECT '10:30:45'::time, '10:30:45.5'::time;
SELECT '2026-01-31'::date + 1, '2026-01-31'::date - 31, '2026-03-01'::date - '2026-02-01'::date;
SELECT '2026-01-31'::date + INTERVAL '1 month', '2026-01-31 10:00'::timestamp + INTERVAL '1 day 2 hours';
SELECT '2026-01-15 10:00'::timestamp - '2026-01-14 08:30'::timestamp;
SELECT '1 day'::interval, '1 year 2 mons 3 days 04:05:06'::interval, '90 minutes'::interval, '-1 day'::interval;
SELECT INTERVAL '1 day' * 3, INTERVAL '2 hours' / 2, INTERVAL '1 month' + INTERVAL '1 day';
SELECT EXTRACT(year FROM '2026-03-15 10:20:30'::timestamp), EXTRACT(month FROM '2026-03-15'::date), EXTRACT(dow FROM '2026-03-15'::date), EXTRACT(doy FROM '2026-03-15'::date);
SELECT EXTRACT(epoch FROM '2026-01-01 00:00:00+00'::timestamptz), EXTRACT(hour FROM '2026-03-15 10:20:30'::timestamp), EXTRACT(second FROM '2026-03-15 10:20:30.5'::timestamp);
SELECT date_part('year', '2026-03-15'::date), date_part('minute', '2026-03-15 10:20:30'::timestamp);
SELECT date_trunc('month', '2026-03-15 10:20:30'::timestamp), date_trunc('day', '2026-03-15 10:20:30'::timestamp), date_trunc('hour', '2026-03-15 10:20:30'::timestamptz);
SELECT to_char('2026-03-15 10:20:30'::timestamp, 'YYYY-MM-DD HH24:MI:SS'), to_char('2026-03-05'::date, 'DD/MM/YY'), to_char('2026-03-15'::date, 'Day Mon');
SELECT to_timestamp('2026-03-15 10:20:30', 'YYYY-MM-DD HH24:MI:SS'), to_date('15/03/2026', 'DD/MM/YYYY');
SELECT to_timestamp(0), to_timestamp(1700000000.5);
SELECT '2026-01-15 10:00:00+00'::timestamptz AT TIME ZONE 'Asia/Ho_Chi_Minh';
SELECT '2026-01-15 10:00:00'::timestamp AT TIME ZONE 'UTC';
SELECT age('2026-03-15'::date, '2025-01-10'::date);
SELECT now() > '2020-01-01'::timestamptz, current_date > '2020-01-01'::date, pg_typeof(now())::text, pg_typeof(current_date)::text, pg_typeof(current_timestamp)::text;
SELECT '2026-02-30'::date;
SELECT '2026-13-01'::timestamp;
SELECT 'notadate'::timestamptz;
SELECT make_date(2026, 3, 15), make_timestamp(2026, 3, 15, 10, 20, 30);
SELECT '2026-01-01'::date < '2026-01-02'::date, '2026-01-01 00:00'::timestamp = '2026-01-01'::date;
SELECT generate_series('2026-01-01'::date, '2026-01-05'::date, '1 day'::interval);
SELECT 'infinity'::timestamp, '-infinity'::date;
SELECT '2026-03-15 10:20:30.123456'::timestamptz;
SHOW timezone;
SELECT '2026-01-15 10:00:00'::timestamptz;

-- ### b/casts
SELECT '12'::int, '12'::bigint, '12.5'::numeric, '12.5'::float8, ' 12 '::int, '1e2'::float8;
SELECT 12::text, 12.50::text, true::text, false::text, NULL::text;
SELECT CAST(12 AS varchar), CAST('12' AS integer), 12::smallint, 12::int2, 12::int4, 12::int8;
SELECT 1.5::int, 2.5::int, 3.5::int, -1.5::int, 1.4999::int;
SELECT 'abc'::int;
SELECT '12abc'::int;
SELECT ''::int;
SELECT '99999999999'::int;
SELECT 32768::smallint;
SELECT 1.5::numeric::text, '1.50'::numeric::text, 0::numeric::text, (-0.0)::numeric::text;
SELECT 't'::boolean, 'true'::boolean, 'yes'::boolean, 'on'::boolean, '1'::boolean, 'f'::boolean, 'no'::boolean, '0'::boolean;
SELECT 'maybe'::boolean;
SELECT 1::boolean, 0::boolean, 5::boolean;
SELECT true::int, false::int;
SELECT true AND false, true OR false, NOT true;
SELECT 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid, pg_typeof('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid)::text;
SELECT 'zzz'::uuid;
SELECT '\xdeadbeef'::bytea, pg_typeof('\xdeadbeef'::bytea)::text, length('\xdeadbeef'::bytea), encode('abc'::bytea, 'hex'), decode('616263', 'hex');
SELECT '{"a":1}'::json, '{"a": 1}'::jsonb, pg_typeof('{"a":1}'::jsonb)::text;
SELECT '{bad'::jsonb;
SELECT '{"a":{"b":[1,2,3]}}'::jsonb -> 'a' -> 'b' ->> 1, '{"a":1}'::jsonb ->> 'a', '{"a":1}'::jsonb @> '{"a":1}'::jsonb, '{"a":1,"b":2}'::jsonb ? 'b';
SELECT jsonb_build_object('a', 1, 'b', 'x'), json_build_array(1, 'a', NULL), to_jsonb(ARRAY[1,2]), jsonb_typeof('[1]'::jsonb);
SELECT ARRAY[1,2,3], ARRAY['a','b'], ARRAY[1,2,3][2], pg_typeof(ARRAY[1,2,3])::text, array_length(ARRAY[1,2,3], 1), cardinality(ARRAY[1,2,3]);
SELECT '{1,2,3}'::int[], ARRAY[1,2] || ARRAY[3], 2 = ANY(ARRAY[1,2,3]), array_append(ARRAY[1], 2), array_to_string(ARRAY[1,2,3], ','), string_to_array('a,b,c', ',');
SELECT unnest(ARRAY[3,1,2]);
SELECT ARRAY[1, NULL, 3], '{}'::int[], ARRAY[[1,2],[3,4]];
SELECT ROW(1, 'a'), (1, 'a') = (1, 'a'), (1, 2) < (1, 3);
SELECT '1 2 3'::int2vector;
SELECT 'pd_nosuch'::regclass;
SELECT '$1,234.50'::money;
SELECT '192.168.0.1'::inet, '10.0.0.0/8'::cidr;
SELECT '1010'::bit(4), B'101';
SELECT int4range(1, 5), daterange('2026-01-01', '2026-02-01');
SELECT 'a'::text::bytea;
SELECT length(gen_random_uuid()::text), length(md5(random()::text));
SELECT 0.1 = 0.1::float8, 0.1::float4 = 0.1::float8;

-- ### b/like-in-exists
-- drop: pd_p, pd_q
CREATE TABLE pd_p (id INTEGER PRIMARY KEY, name TEXT, grp INTEGER);
CREATE TABLE pd_q (id INTEGER PRIMARY KEY, pid INTEGER, amt NUMERIC(10,2));
INSERT INTO pd_p VALUES (1, 'Alice', 1), (2, 'bob', 1), (3, 'Carol', 2), (4, 'alan', 3), (5, 'A_B', 2), (6, '100%', 2);
INSERT INTO pd_q VALUES (1, 1, 10.5), (2, 1, 20), (3, 3, 5.25), (4, 9, 1);
SELECT name FROM pd_p WHERE name LIKE 'A%';
SELECT name FROM pd_p WHERE name ILIKE 'a%';
SELECT name FROM pd_p WHERE name NOT LIKE '%o%';
SELECT name FROM pd_p WHERE name LIKE '_ob';
SELECT name FROM pd_p WHERE name LIKE 'A\_B';
SELECT name FROM pd_p WHERE name LIKE '100\%';
SELECT name FROM pd_p WHERE name LIKE 'A!_B' ESCAPE '!';
SELECT name FROM pd_p WHERE name SIMILAR TO '(A|a)l%';
SELECT name FROM pd_p WHERE name ~ '^[A-Z]';
SELECT name FROM pd_p WHERE lower(name) LIKE 'al%' ORDER BY name;
SELECT name FROM pd_p WHERE id IN (1, 3, 99);
SELECT name FROM pd_p WHERE id NOT IN (1, 3);
SELECT name FROM pd_p WHERE id IN (SELECT pid FROM pd_q);
SELECT name FROM pd_p p WHERE EXISTS (SELECT 1 FROM pd_q q WHERE q.pid = p.id);
SELECT name FROM pd_p p WHERE NOT EXISTS (SELECT 1 FROM pd_q q WHERE q.pid = p.id);
SELECT name FROM pd_p WHERE id = ANY(ARRAY[2, 4]);
SELECT name FROM pd_p WHERE id BETWEEN 2 AND 4;
SELECT name FROM pd_p WHERE id NOT BETWEEN 2 AND 4;
SELECT name FROM pd_p WHERE id > (SELECT AVG(id) FROM pd_p);
SELECT name, (SELECT SUM(amt) FROM pd_q WHERE pid = p.id) AS total FROM pd_p p;
SELECT id FROM pd_p WHERE id = (SELECT pid FROM pd_q);
-- args: A%
SELECT name FROM pd_p WHERE name LIKE $1;
-- args: 1 | 3
SELECT name FROM pd_p WHERE id IN ($1::int, $2::int);

-- ### b/joins-setops
-- drop: pd_p, pd_q
CREATE TABLE pd_p (id INTEGER PRIMARY KEY, name TEXT);
CREATE TABLE pd_q (id INTEGER PRIMARY KEY, pid INTEGER, amt NUMERIC(10,2));
INSERT INTO pd_p VALUES (1, 'a'), (2, 'b'), (3, 'c');
INSERT INTO pd_q VALUES (1, 1, 10.5), (2, 1, 20), (3, 3, 5.25), (4, 9, 1);
SELECT p.name, q.amt FROM pd_p p JOIN pd_q q ON q.pid = p.id;
SELECT p.name, q.amt FROM pd_p p LEFT JOIN pd_q q ON q.pid = p.id;
SELECT p.name, q.amt FROM pd_p p RIGHT JOIN pd_q q ON q.pid = p.id;
SELECT p.name, q.amt FROM pd_p p FULL JOIN pd_q q ON q.pid = p.id;
SELECT COUNT(*) FROM pd_p CROSS JOIN pd_q;
SELECT p.name FROM pd_p p JOIN pd_q q USING (id);
SELECT * FROM pd_p NATURAL JOIN pd_q;
SELECT id FROM pd_p UNION SELECT pid FROM pd_q;
SELECT id FROM pd_p UNION ALL SELECT pid FROM pd_q;
SELECT id FROM pd_p INTERSECT SELECT pid FROM pd_q;
SELECT id FROM pd_p EXCEPT SELECT pid FROM pd_q;
SELECT id FROM pd_p UNION SELECT pid FROM pd_q ORDER BY 1 DESC;
SELECT id, name FROM pd_p UNION SELECT pid FROM pd_q;
SELECT id FROM pd_p UNION SELECT name FROM pd_p;
SELECT p.name, (SELECT COUNT(*) FROM pd_q q WHERE q.pid = p.id) FROM pd_p p;
SELECT p.id FROM pd_p p, LATERAL (SELECT 1 FROM pd_q q WHERE q.pid = p.id LIMIT 1) l;
SELECT a.id, b.id FROM pd_p a JOIN pd_p b ON a.id < b.id;
SELECT id FROM pd_p JOIN pd_q ON pd_q.pid = pd_p.id;
SELECT name FROM pd_p, pd_q WHERE pd_q.pid = pd_p.id AND pd_q.amt > 10;

-- ### b/aggregate-group
-- drop: pd_s
CREATE TABLE pd_s (id INTEGER PRIMARY KEY, carrier TEXT, region TEXT, amt NUMERIC(10,2), qty INTEGER);
INSERT INTO pd_s VALUES (1, 'viettel', 'n', 10.5, 1), (2, 'viettel', 's', 20.25, 2), (3, 'mobi', 'n', 5, 3), (4, 'mobi', 'n', 7.75, NULL), (5, 'vina', 's', 100, 5), (6, NULL, 'n', 1, 1);
SELECT carrier, COUNT(*), SUM(amt), AVG(amt), MIN(amt), MAX(amt) FROM pd_s GROUP BY carrier;
SELECT carrier, SUM(qty), AVG(qty) FROM pd_s GROUP BY carrier;
SELECT carrier, COUNT(*) FROM pd_s GROUP BY carrier HAVING COUNT(*) > 1;
SELECT carrier, region, COUNT(*) FROM pd_s GROUP BY carrier, region;
SELECT carrier, SUM(amt) AS total FROM pd_s GROUP BY carrier ORDER BY total DESC NULLS LAST;
SELECT region, COUNT(DISTINCT carrier) FROM pd_s GROUP BY region;
SELECT COUNT(*), SUM(amt), AVG(amt), pg_typeof(SUM(amt))::text, pg_typeof(AVG(amt))::text, pg_typeof(COUNT(*))::text, pg_typeof(SUM(qty))::text FROM pd_s;
SELECT carrier, SUM(amt) FROM pd_s GROUP BY 1;
SELECT region, SUM(amt) FROM pd_s GROUP BY region ORDER BY 1;
SELECT id, SUM(amt) FROM pd_s GROUP BY region;
SELECT bool_and(qty > 0), bool_or(qty > 4), array_agg(id ORDER BY id), string_agg(carrier, ',' ORDER BY id) FROM pd_s;
SELECT stddev(amt), variance(amt) FROM pd_s;
SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY amt) FROM pd_s;
SELECT COUNT(*) FILTER (WHERE region = 'n'), SUM(amt) FILTER (WHERE carrier = 'mobi') FROM pd_s;
SELECT carrier, region, SUM(amt) FROM pd_s GROUP BY ROLLUP (carrier, region);
SELECT carrier, SUM(amt) FROM pd_s GROUP BY GROUPING SETS ((carrier), ());
SELECT DISTINCT carrier FROM pd_s;
SELECT DISTINCT carrier, region FROM pd_s;
SELECT DISTINCT ON (carrier) carrier, id FROM pd_s ORDER BY carrier, id DESC;
SELECT SUM(amt) FROM pd_s WHERE id > 100;
SELECT carrier, COUNT(*) FROM pd_s WHERE carrier IS NOT NULL GROUP BY carrier ORDER BY COUNT(*) DESC, carrier;
SELECT AVG(qty)::numeric(10,2), ROUND(AVG(amt), 1) FROM pd_s;
SELECT max(carrier), min(carrier) FROM pd_s;

-- ### b/order-limit
-- drop: pd_o
CREATE TABLE pd_o (id INTEGER PRIMARY KEY, v TEXT);
INSERT INTO pd_o SELECT g, chr(96 + (g % 5) + 1) FROM generate_series(1, 20) g;
SELECT id FROM pd_o ORDER BY id LIMIT 5;
SELECT id FROM pd_o ORDER BY id LIMIT 5 OFFSET 5;
SELECT id FROM pd_o ORDER BY id OFFSET 18;
SELECT id FROM pd_o ORDER BY id LIMIT 0;
SELECT id FROM pd_o ORDER BY id LIMIT ALL OFFSET 19;
SELECT id FROM pd_o ORDER BY id FETCH FIRST 2 ROWS ONLY;
SELECT v, id FROM pd_o ORDER BY v, id DESC LIMIT 6;
SELECT v, COUNT(*) FROM pd_o GROUP BY v ORDER BY 2 DESC, 1;
SELECT id FROM pd_o ORDER BY id LIMIT -1;
SELECT id FROM pd_o ORDER BY v DESC, id LIMIT 3;
SELECT id, v FROM pd_o ORDER BY lower(v), id LIMIT 3;
SELECT id FROM pd_o ORDER BY 5;
SELECT id AS x FROM pd_o ORDER BY x DESC LIMIT 2;
-- args: 3 | 2
SELECT id FROM pd_o ORDER BY id LIMIT $1 OFFSET $2;
SELECT (SELECT MAX(id) FROM pd_o) - (SELECT MIN(id) FROM pd_o);
SELECT id FROM pd_o WHERE id > 15 FOR UPDATE;
SELECT id FROM pd_o ORDER BY id LIMIT 2 FOR UPDATE SKIP LOCKED;

-- ### b/window-cte
-- drop: pd_w
CREATE TABLE pd_w (id INTEGER PRIMARY KEY, grp TEXT, v INTEGER);
INSERT INTO pd_w VALUES (1, 'a', 10), (2, 'a', 20), (3, 'a', 20), (4, 'b', 5), (5, 'b', 15);
SELECT id, row_number() OVER (ORDER BY id) FROM pd_w ORDER BY id;
SELECT id, row_number() OVER (PARTITION BY grp ORDER BY v DESC, id) FROM pd_w ORDER BY id;
SELECT id, rank() OVER (PARTITION BY grp ORDER BY v), dense_rank() OVER (PARTITION BY grp ORDER BY v) FROM pd_w ORDER BY id;
SELECT id, SUM(v) OVER (PARTITION BY grp), SUM(v) OVER (ORDER BY id) FROM pd_w ORDER BY id;
SELECT id, lag(v) OVER (ORDER BY id), lead(v, 2, -1) OVER (ORDER BY id) FROM pd_w ORDER BY id;
SELECT id, first_value(v) OVER (PARTITION BY grp ORDER BY id), last_value(v) OVER (PARTITION BY grp ORDER BY id) FROM pd_w ORDER BY id;
SELECT id, AVG(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM pd_w ORDER BY id;
SELECT id, ntile(2) OVER (ORDER BY id), percent_rank() OVER (ORDER BY id), cume_dist() OVER (ORDER BY id) FROM pd_w ORDER BY id;
SELECT id, COUNT(*) OVER () FROM pd_w ORDER BY id;
SELECT id, SUM(v) OVER w FROM pd_w WINDOW w AS (PARTITION BY grp) ORDER BY id;
WITH x AS (SELECT grp, SUM(v) AS s FROM pd_w GROUP BY grp) SELECT grp, s FROM x WHERE s > 20;
WITH x AS (SELECT 1 AS a), y AS (SELECT a + 1 AS b FROM x) SELECT a, b FROM x, y;
WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r WHERE n < 5) SELECT n FROM r;
WITH ins AS (INSERT INTO pd_w VALUES (9, 'z', 1) RETURNING id) SELECT id FROM ins;
WITH del AS (DELETE FROM pd_w WHERE id = 9 RETURNING id) SELECT COUNT(*) FROM del;
WITH upd AS (UPDATE pd_w SET v = v + 1 WHERE id = 1 RETURNING v) SELECT v FROM upd;
SELECT * FROM (SELECT grp, MAX(v) AS m FROM pd_w GROUP BY grp) s WHERE m > 10 ORDER BY grp;
SELECT * FROM generate_series(1, 5) AS g(n) WHERE n % 2 = 1;
SELECT g, g * g FROM generate_series(1, 4) g;
SELECT * FROM (VALUES (1, 'a'), (2, 'b')) AS t(id, name);
SELECT g FROM generate_series(10, 1, -3) g;
SELECT generate_series(1, 3) AS g, 'x';

-- ### b/functions-misc
SELECT version() LIKE 'PostgreSQL%';
SELECT current_database() IS NOT NULL, current_schema(), current_user IS NOT NULL, session_user IS NOT NULL;
SELECT pg_typeof(1)::text, pg_typeof('a'::text)::text, pg_typeof(now())::text, pg_typeof(1 = 1)::text, pg_typeof(1.5::float4)::text;
SELECT random() >= 0 AND random() < 1, setseed(0.5) IS NULL;
SELECT pg_backend_pid() > 0;
SELECT current_setting('server_version_num')::int >= 140000;
SELECT current_setting('standard_conforming_strings'), current_setting('client_encoding'), current_setting('DateStyle'), current_setting('TimeZone');
SELECT set_config('application.name', 'x', FALSE), current_setting('application.name');
SHOW server_version_num;
SHOW client_encoding;
SHOW standard_conforming_strings;
SHOW DateStyle;
SHOW search_path;
SHOW application_name;
SET application_name = 'pgdiff';
SHOW application_name;
SET search_path TO public;
SET extra_float_digits = 3;
SET client_min_messages = warning;
SET lock_timeout = '5s';
SET LOCAL statement_timeout = 1000;
RESET ALL;
SELECT 1 AS "Quoted Name", 2 AS lower, 3 AS "UPPER";
SELECT 1 AS UPPERCASE;
SELECT;
SELECT 1, 1;
SELECT 'a' AS x, 'b' AS x;
SELECT 1 /* inline comment */ + 1; -- trailing comment
SELECT * FROM (SELECT 1) s;
SELECT ?column?;
SELECT $1;
-- args: 5
SELECT $1 + 1;
-- args: abc
SELECT $1 || 'x';
-- args: 5
SELECT $1::int + 1, pg_typeof($1)::text;
-- args: null
SELECT $1::int IS NULL;
