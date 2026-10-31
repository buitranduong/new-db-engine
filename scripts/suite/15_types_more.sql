-- Suite group 15: types beyond group 01 (interval columns, ranges, macaddr/macaddr8, bit strings).
-- Source: PostgreSQL 16 documentation chapter 8 (data types), 9.9 (date/time functions), 9.20 (range functions),
-- 9.6 (bit string functions); the PostgreSQL source behind each type is named in docs/14 (row "types").
-- Cases for this task are added only in this and later files (15_*.sql); groups 01-14 belong to other tasks.
-- Each session sets search_path to s15; setup creates the schema and teardown drops it.

-- ### 15_types_more/setup
DROP SCHEMA IF EXISTS s15 CASCADE;
CREATE SCHEMA s15;

-- ### 15_types_more/interval-column
SET search_path = s15;
CREATE TABLE iv (id int PRIMARY KEY, d interval, p interval(2), ym interval year to month, ds interval day to second(3));
INSERT INTO iv VALUES (1, '1 year 2 months 3 days 04:05:06.789', '1.23456 sec', '1 year 14 months', '1 day 2:03:04.56789');
INSERT INTO iv VALUES (2, '-3 days -2 hours', '00:00:00.005', '6 months', '-1 hour');
INSERT INTO iv VALUES (3, '30 days', NULL, NULL, '25 hours');
INSERT INTO iv VALUES (4, '1 month', NULL, NULL, NULL);
INSERT INTO iv VALUES (5, '0', '1 min', '-1 year', '0.5 sec');
SELECT * FROM iv ORDER BY id;
SELECT id, d FROM iv ORDER BY d, id;
SELECT id, d FROM iv ORDER BY d DESC, id;
SELECT id FROM iv WHERE d > '1 day' ORDER BY id;
SELECT id FROM iv WHERE d = '30 days' ORDER BY id;
SELECT id FROM iv WHERE d BETWEEN '-1 day' AND '1 month' ORDER BY id;
SELECT min(d), max(d) FROM iv;
SELECT sum(d), avg(d) FROM iv;
SELECT sum(ds), avg(ds) FROM iv;
SELECT count(DISTINCT d) FROM iv;
SELECT d, count(*) FROM iv GROUP BY d ORDER BY d;
SELECT pg_typeof(d)::text, pg_typeof(p)::text, pg_typeof(d + d)::text, pg_typeof(d * 2)::text FROM iv LIMIT 1;
SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'iv'::regclass AND attnum > 0 ORDER BY attnum;
SELECT d::text, d::varchar FROM iv ORDER BY id;
UPDATE iv SET d = d + '1 day' WHERE id = 1 RETURNING d;
UPDATE iv SET d = d * 2 RETURNING id, d;
UPDATE iv SET d = d / 3 WHERE id <= 2 RETURNING id, d;
DELETE FROM iv WHERE d < '0' RETURNING id;
CREATE INDEX iv_d ON iv (d);
SELECT id FROM iv WHERE d >= '1 month' ORDER BY d, id;
SELECT id FROM iv WHERE d = '1 mon' ORDER BY id;
CREATE TABLE iv2 (d interval PRIMARY KEY);
INSERT INTO iv2 VALUES ('1 day'), ('2 days'), ('1 hour');
INSERT INTO iv2 VALUES ('1 day');
SELECT d FROM iv2 ORDER BY d;
SELECT d FROM iv2 WHERE d > '2 hours' ORDER BY d;
ALTER TABLE iv2 ADD COLUMN e interval DEFAULT '1 hour';
SELECT * FROM iv2 ORDER BY d;
CREATE TABLE iv3 AS SELECT id, d FROM iv;
SELECT * FROM iv3 ORDER BY id;
CREATE TABLE iv4 (d interval NOT NULL DEFAULT interval '1 day 2 hours', c interval CHECK (c > interval '0'));
INSERT INTO iv4 (c) VALUES ('1 hour');
INSERT INTO iv4 (c) VALUES ('-1 hour');
SELECT * FROM iv4;

-- ### 15_types_more/interval-arith
SET search_path = s15;
SELECT interval '1 day' + interval '2 hours', interval '1 day' - interval '2 hours', -interval '1 day 2 hours';
SELECT interval '1 month' + interval '-1 day', interval '1 year' * 1.5, 2 * interval '1 day 3 hours', interval '1 day' / 2;
SELECT interval '1 mon' * 0.5, interval '1 mon' / 4, interval '10 days' * 0.25, interval '1 day 12:00' / 7;
SELECT interval '1 day' * 0, interval '1 day' / 0;
SELECT interval '1 year 2 mons 3 days' * 2.5, interval '3 days 04:05:06' * 1.1, interval '5 days' / 1.5;
SELECT timestamp '2024-01-31 10:00' + interval '1 month', timestamp '2024-01-31' + interval '1 month 1 day', timestamp '2024-03-31' - interval '1 month';
SELECT date '2024-01-31' + interval '1 month', date '2024-01-31' - interval '1 day 2 hours', interval '1 day' + date '2024-02-28';
SELECT timestamptz '2024-01-31 10:00+00' + interval '1 day 2 hours', timestamptz '2024-01-31 10:00+00' - interval '1 month';
SELECT timestamp '2024-03-01 00:00' - timestamp '2024-01-01 00:00', timestamp '2024-01-02 12:00' - timestamp '2024-01-01', timestamp '2024-01-01' - timestamp '2024-01-03 05:00';
SELECT timestamptz '2024-03-01 00:00+00' - timestamptz '2024-01-01 00:00+00';
SELECT date '2024-03-01' - date '2024-01-01', age(timestamp '2024-03-15', timestamp '2023-01-10');
SELECT justify_hours(interval '50 hours'), justify_days(interval '65 days'), justify_interval(interval '1 month -1 hour'), justify_interval(interval '-1 month 1 hour');
SELECT justify_hours(interval '-50 hours 1 day'), justify_days(interval '35 days -1 month'), justify_interval(interval '40 days 50 hours');
SELECT make_interval(1, 2, 3, 4, 5, 6, 7.5), make_interval(days => 5), make_interval(hours => 25, mins => 61), make_interval(), make_interval(years => -1, secs => 0.000001);
SELECT interval '1 day' < interval '25 hours', interval '1 month' = interval '30 days', interval '1 month' > interval '29 days', interval '1 day' = interval '24 hours';
SELECT greatest(interval '1 day', interval '2 hours'), least(interval '1 day', interval '2 hours');
SELECT interval '1 day' IN (interval '24 hours', interval '2 days');
SELECT sum(i), avg(i), min(i), max(i) FROM (VALUES (interval '1 day'), (interval '2 days 03:00'), (interval '-4 hours')) v(i);
SELECT avg(i) FROM (VALUES (interval '1 month'), (interval '15 days'), (interval '1 day 1 hour')) v(i);
SELECT generate_series(timestamp '2024-01-31', timestamp '2024-05-31', interval '1 month');
SELECT generate_series(timestamp '2024-01-01', timestamp '2024-01-03', interval '12 hours');
SELECT generate_series(timestamp '2024-01-03', timestamp '2024-01-01', interval '-1 day');
SELECT generate_series(date '2024-01-01', date '2024-01-10', interval '3 days');

-- ### 15_types_more/interval-extract
SET search_path = s15;
SELECT extract(year FROM interval '2 years 14 months 40 days 25:30:45.678'), extract(month FROM interval '2 years 14 months'), extract(day FROM interval '2 years 14 months 40 days 25:30:45.678');
SELECT extract(hour FROM interval '2 years 40 days 25:30:45.678'), extract(minute FROM interval '25:30:45.678'), extract(second FROM interval '25:30:45.678'), extract(milliseconds FROM interval '25:30:45.678'), extract(microseconds FROM interval '25:30:45.678');
SELECT extract(epoch FROM interval '1 year 2 months 3 days 04:05:06.789'), extract(epoch FROM interval '-1 day'), extract(decade FROM interval '25 years'), extract(century FROM interval '250 years'), extract(millennium FROM interval '2500 years'), extract(quarter FROM interval '4 months');
SELECT date_part('year', interval '2 years 3 months'), date_part('second', interval '1 min 5.5 sec'), date_part('epoch', interval '1 day'), date_part('day', interval '-3 days -2 hours');
SELECT extract(dow FROM interval '1 day');
SELECT extract(foo FROM interval '1 day');
SELECT pg_typeof(extract(second FROM interval '1 day'))::text, pg_typeof(date_part('second', interval '1 day'))::text;

-- ### 15_types_more/interval-cast-text
SET search_path = s15;
SELECT CAST('1 day' AS interval), '1 day'::interval(0), '1.5 sec'::interval(0), '1.4 sec'::interval(0), '1.55 sec'::interval(1), '1 day 2:30:15'::interval hour to minute;
SELECT interval '1 day 2:03:04' day to hour, interval '1 year 2 mons' year, interval '1 year 7 mons' year to month, interval '4 days 05:06:07' minute to second;
SELECT interval '3' day, interval '2' hour, interval '1 year', INTERVAL '5 minutes';
SELECT ('1 day'::interval)::text || 'x', concat('a', interval '1 day'), interval '1 day'::text;
SELECT array[interval '1 day', interval '2 hours'];
SELECT (array[interval '1 day', interval '2 hours'])[2];
SELECT interval '1 day' IS NULL, interval '1 day' IS NOT NULL, NULL::interval;
SELECT coalesce(NULL::interval, interval '1 hour'), nullif(interval '1 day', interval '24 hours'), CASE WHEN true THEN interval '1 day' ELSE interval '2 days' END;
SELECT isfinite(interval '1 day');
SELECT interval '1 day' + 5;
SELECT interval '2147483647 months' + interval '1 month';
SELECT interval '2147483647 days' * 2;
SELECT 'abc'::interval;
SELECT interval '1 day' * 'x';

-- ### 15_types_more/interval-style
SET search_path = s15;
SET IntervalStyle = 'iso_8601';
SELECT interval '1 year 2 mons 3 days 04:05:06.7', interval '-1 day -2 hours', interval '0', interval '1.5 sec';
SET IntervalStyle = 'sql_standard';
SELECT interval '1 year 2 mons', interval '3 days 04:05:06', interval '-1 year -2 mons', interval '1 year -3 days 04:05:06', interval '0', interval '04:05:06', interval '-3 days -04:05:06';
SET IntervalStyle = 'postgres_verbose';
SELECT interval '1 year 2 mons 3 days 04:05:06.7', interval '-1 day -2 hours', interval '0', interval '1 day -2 hours', interval '-5 min';
SET IntervalStyle = 'postgres';
SELECT interval '1 year 2 mons 3 days 04:05:06.7', interval '-1 day 2 hours', interval '1 day -02:00', interval '-1 year 2 mons -3 days 4 hours';
SHOW IntervalStyle;
SELECT interval '1 day' ::text;

-- ### 15_types_more/interval-binary
SET search_path = s15;
CREATE TABLE ivb (id int, d interval);
INSERT INTO ivb VALUES (1, '1 year 2 mons 3 days 04:05:06.789'), (2, '-1 day'), (3, NULL);
-- stdin: 4\t5 days 06:00\n5\t-2 mons
COPY s15.ivb FROM STDIN;
COPY s15.ivb TO STDOUT;
COPY s15.ivb TO STDOUT WITH (FORMAT binary);
SELECT d FROM ivb ORDER BY id;
-- args: 1 day 2 hours
SELECT $1::interval + interval '1 hour';
-- args: 1 day 2 hours
SELECT $1::interval;

-- ### 15_types_more/range-literals
SET search_path = s15;
SELECT '[1,5)'::int4range, '(1,5]'::int4range, '[1,5]'::int4range, '(1,5)'::int4range, '[1,1)'::int4range, '(1,2)'::int4range, 'empty'::int4range, ' EMPTY '::int4range;
SELECT '[,5)'::int4range, '[1,)'::int4range, '(,)'::int4range, '[,]'::int4range, '[ 1 , 5 )'::int4range;
SELECT '[1,5)'::int8range, '[9223372036854775806,9223372036854775807)'::int8range, '(1,9)'::int8range;
SELECT '[1.5,2.5)'::numrange, '(0,10.00]'::numrange, '[1,1]'::numrange, '(1,1]'::numrange, '[,3.14)'::numrange, '[NaN,NaN]'::numrange;
SELECT '[2024-01-01,2024-02-01)'::daterange, '(2024-01-01,2024-02-01]'::daterange, '[2024-01-01,)'::daterange, '[2024-01-01,2024-01-01]'::daterange;
SELECT '[2024-01-01 10:00,2024-01-01 12:30:15)'::tsrange, '(2024-01-01 10:00,)'::tsrange;
SELECT '[2024-01-01 10:00+00,2024-01-01 12:30:15+00)'::tstzrange, '[2024-01-01 10:00+02,2024-01-01 12:30:15+02]'::tstzrange;
SELECT int4range(1, 5), int4range(1, 5, '[]'), int4range(1, 5, '(]'), int4range(1, NULL), int4range(NULL, NULL), int4range(5, 5), int4range(5, 5, '[]');
SELECT int8range(1, 10), numrange(1.5, 2.5, '[]'), daterange('2024-01-01', '2024-02-01'), daterange(date '2024-01-01', date '2024-01-31', '[]');
SELECT tsrange(timestamp '2024-01-01 00:00', timestamp '2024-01-02 00:00'), tstzrange(timestamptz '2024-01-01 00:00+00', timestamptz '2024-01-02 00:00+00', '[]');
SELECT '[a,b)'::int4range;
SELECT '[5,1)'::int4range;
SELECT '[1,2'::int4range;
SELECT '1,2)'::int4range;
SELECT '[1;2)'::int4range;
SELECT '[1,2) x'::int4range;
SELECT '[1,2147483647]'::int4range;
SELECT '[1,99999999999)'::int4range;
SELECT int4range(1, 2, 'xx');
SELECT int4range(5, 1);
SELECT pg_typeof('[1,2)'::int4range)::text, pg_typeof(daterange('2024-01-01', '2024-02-01'))::text, pg_typeof(lower('[1,2)'::int4range))::text, pg_typeof('[1,2)'::numrange * '[1,3)'::numrange)::text;

-- ### 15_types_more/range-ops
SET search_path = s15;
SELECT lower('[1,5)'::int4range), upper('[1,5)'::int4range), lower('(1,5]'::int4range), upper('(1,5]'::int4range), lower('empty'::int4range), upper('[1,)'::int4range);
SELECT lower_inc('[1,5)'::int4range), upper_inc('[1,5)'::int4range), lower_inc('(1,5]'::int4range), upper_inc('(1,5]'::int4range), lower_inf('(,5)'::int4range), upper_inf('[1,)'::int4range), isempty('empty'::int4range), isempty('[1,2)'::int4range);
SELECT lower('[1.5,2.5)'::numrange), upper('[1.5,2.5)'::numrange), lower('[2024-01-01,2024-02-01)'::daterange), upper('[2024-01-01,2024-02-01]'::daterange), lower('[2024-01-01 10:00,2024-01-02)'::tsrange);
SELECT '[1,5)'::int4range @> 3, '[1,5)'::int4range @> 5, '[1,5)'::int4range @> 1, '[1,5]'::int4range @> 5, '[1,)'::int4range @> 100000, 3 <@ '[1,5)'::int4range, 9 <@ '[1,5)'::int4range;
SELECT '[1,10)'::int4range @> '[2,5)'::int4range, '[1,10)'::int4range @> '[2,12)'::int4range, '[2,5)'::int4range <@ '[1,10)'::int4range, '[1,10)'::int4range @> 'empty'::int4range, 'empty'::int4range @> '[1,2)'::int4range;
SELECT '[1,5)'::int4range && '[4,8)'::int4range, '[1,5)'::int4range && '[5,8)'::int4range, '[1,5)'::int4range && 'empty'::int4range, '[1,5]'::int4range && '[5,8)'::int4range;
SELECT '[1,5)'::int4range << '[5,8)'::int4range, '[1,5)'::int4range << '[4,8)'::int4range, '[5,8)'::int4range >> '[1,5)'::int4range, '[1,5)'::int4range &< '[3,8)'::int4range, '[3,8)'::int4range &> '[1,5)'::int4range;
SELECT '[1,5)'::int4range -|- '[5,8)'::int4range, '[1,5)'::int4range -|- '[6,8)'::int4range, '[1,5]'::int4range -|- '[6,8)'::int4range, '[1,5)'::int4range -|- '(5,8)'::int4range;
SELECT '[1,5)'::int4range + '[4,8)'::int4range, '[1,5)'::int4range + '[5,8)'::int4range, '[1,5)'::int4range + 'empty'::int4range;
SELECT '[1,5)'::int4range * '[3,8)'::int4range, '[1,5)'::int4range * '[6,8)'::int4range, '[1,10)'::int4range * '[3,5)'::int4range;
SELECT '[1,10)'::int4range - '[3,5)'::int4range;
SELECT '[1,10)'::int4range - '[5,15)'::int4range, '[1,10)'::int4range - '[-5,3)'::int4range, '[1,10)'::int4range - '[20,30)'::int4range, '[3,5)'::int4range - '[1,10)'::int4range;
SELECT '[1,5)'::int4range + '[7,8)'::int4range;
SELECT range_merge('[1,3)'::int4range, '[7,9)'::int4range), range_merge('[1,3)'::int4range, 'empty'::int4range);
SELECT '[1,5)'::int4range = '[1,5)'::int4range, '[1,5)'::int4range = '[1,4]'::int4range, '[1,5)'::int4range <> '[1,6)'::int4range, '[1,5)'::int4range < '[1,6)'::int4range, '[2,5)'::int4range > '[1,6)'::int4range, 'empty'::int4range < '[1,2)'::int4range, '(,5)'::int4range < '[1,5)'::int4range;
SELECT '[1.0,2.0)'::numrange = '[1,2)'::numrange, '[2024-01-01,2024-01-31]'::daterange = '[2024-01-01,2024-02-01)'::daterange;
SELECT '[2024-01-01,2024-02-01)'::daterange @> date '2024-01-15', '[2024-01-01,2024-02-01)'::daterange @> date '2024-02-01', '[2024-01-01 10:00,2024-01-01 12:00)'::tsrange @> timestamp '2024-01-01 11:00', '[1.5,2.5)'::numrange @> 2.0, '[1.5,2.5)'::numrange @> 2;
SELECT '[2024-01-01,2024-02-01)'::daterange && '[2024-01-31,2024-03-01)'::daterange, '[2024-01-01,2024-02-01)'::daterange -|- '[2024-02-01,2024-03-01)'::daterange;
SELECT '[1,5)'::int4range @> NULL::int4, NULL::int4range @> 3, lower(NULL::int4range), '[1,5)'::int4range && NULL::int4range;
SELECT '[1,5)'::int4range IS NULL, NULL::int4range IS NULL, coalesce(NULL::int4range, '[1,2)'::int4range);
SELECT greatest('[1,5)'::int4range, '[2,3)'::int4range), least('[1,5)'::int4range, '[2,3)'::int4range);

-- ### 15_types_more/range-column
SET search_path = s15;
CREATE TABLE rg (id int PRIMARY KEY, ir int4range, nr numrange, dr daterange, tr tsrange, tz tstzrange, br int8range);
INSERT INTO rg VALUES (1, '[1,10)', '[1.5,2.5]', '[2024-01-01,2024-02-01)', '[2024-01-01 10:00,2024-01-01 12:00)', '[2024-01-01 10:00+00,2024-01-01 12:00+00)', '[1,100000000000)');
INSERT INTO rg VALUES (2, '[5,)', '(,0)', '[2024-01-15,)', '(,2024-01-01 00:00)', '(,)', 'empty');
INSERT INTO rg VALUES (3, 'empty', NULL, '[2023-12-01,2024-01-01]', NULL, NULL, NULL);
INSERT INTO rg VALUES (4, int4range(3, 7), numrange(10, 20, '(]'), daterange('2024-03-01', '2024-04-01'), NULL, NULL, int8range(1, 2));
INSERT INTO rg VALUES (5, '[1,10)', '[1.5,2.5]', NULL, NULL, NULL, NULL);
SELECT * FROM rg ORDER BY id;
SELECT id, ir FROM rg ORDER BY ir, id;
SELECT id, ir FROM rg ORDER BY ir DESC, id;
SELECT id, nr FROM rg ORDER BY nr, id;
SELECT id, dr FROM rg ORDER BY dr, id;
SELECT id FROM rg WHERE ir @> 5 ORDER BY id;
SELECT id FROM rg WHERE ir && '[8,20)' ORDER BY id;
SELECT id FROM rg WHERE ir = '[1,10)' ORDER BY id;
SELECT id FROM rg WHERE ir < '[4,5)' ORDER BY id;
SELECT id FROM rg WHERE dr @> date '2024-01-20' ORDER BY id;
SELECT id FROM rg WHERE dr << '[2024-02-01,)' ORDER BY id;
SELECT id FROM rg WHERE isempty(ir) ORDER BY id;
SELECT id, lower(ir), upper(ir), lower(dr), upper(nr) FROM rg ORDER BY id;
SELECT ir, count(*) FROM rg GROUP BY ir ORDER BY ir;
SELECT DISTINCT ir FROM rg ORDER BY ir;
SELECT a.id, b.id FROM rg a JOIN rg b ON a.ir && b.ir AND a.id < b.id ORDER BY 1, 2;
SELECT id, ir * '[5,20)'::int4range AS x FROM rg WHERE id IN (1, 2, 4) ORDER BY id;
SELECT pg_typeof(ir)::text, pg_typeof(nr)::text, pg_typeof(dr)::text, pg_typeof(tr)::text, pg_typeof(tz)::text, pg_typeof(br)::text FROM rg LIMIT 1;
SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'rg'::regclass AND attnum > 0 ORDER BY attnum;
SELECT typname, typtype::text, typcategory::text FROM pg_type WHERE typname IN ('int4range', 'numrange', 'tsrange', 'tstzrange', 'daterange', 'int8range') ORDER BY typname;
SELECT ir::text, nr::text FROM rg WHERE id = 1;
UPDATE rg SET ir = ir + '[10,12)'::int4range WHERE id = 1 RETURNING ir;
UPDATE rg SET dr = daterange(lower(dr), upper(dr), '[]') WHERE id = 4 RETURNING dr;
CREATE INDEX rg_ir ON rg (ir);
CREATE INDEX rg_dr ON rg (dr);
SELECT id FROM rg WHERE ir = '[3,7)' ORDER BY id;
SELECT id FROM rg WHERE ir > '[3,7)' ORDER BY ir, id;
SELECT id FROM rg WHERE dr >= '[2024-01-15,)' ORDER BY dr, id;
CREATE TABLE rg2 (r int4range PRIMARY KEY, n int);
INSERT INTO rg2 VALUES ('[1,5)', 1), ('[5,10)', 2), ('[2,3)', 3);
INSERT INTO rg2 VALUES ('[1,5)', 4);
INSERT INTO rg2 VALUES ('[1,4]', 5);
SELECT * FROM rg2 ORDER BY r;
CREATE TABLE rg3 AS SELECT id, ir, dr FROM rg;
SELECT * FROM rg3 ORDER BY id;
CREATE TABLE rg4 (r daterange NOT NULL DEFAULT '[2024-01-01,2024-02-01)', CHECK (NOT isempty(r)));
INSERT INTO rg4 DEFAULT VALUES;
INSERT INTO rg4 VALUES ('empty');
SELECT * FROM rg4;
SELECT r @> date '2024-01-05', upper(r) - lower(r) FROM rg4;
ALTER TABLE rg4 ADD COLUMN s numrange;
UPDATE rg4 SET s = numrange(1, 2);
SELECT * FROM rg4;
SELECT ir FROM rg WHERE ir = ANY (ARRAY['[1,10)'::int4range]) ORDER BY id;
SELECT ir FROM rg WHERE ir IN ('[1,10)', '[3,7)') ORDER BY id;
SELECT CASE WHEN ir @> 5 THEN 'in' ELSE 'out' END FROM rg ORDER BY id;
SELECT ir, row_number() OVER (ORDER BY ir) FROM rg ORDER BY id;

-- ### 15_types_more/range-binary
SET search_path = s15;
CREATE TABLE rgb (id int, ir int4range, dr daterange, nr numrange, tz tstzrange);
INSERT INTO rgb VALUES (1, '[1,10)', '[2024-01-01,2024-02-01)', '[1.5,2.5]', '[2024-01-01 10:00+00,2024-01-02 10:00+00)'), (2, 'empty', NULL, '(,)', NULL), (3, '(,5)', '[2024-01-01,)', NULL, NULL);
-- stdin: 4\t[3,4)\t[2024-05-01,2024-05-02)\t(1,2)\t[2024-05-01 00:00+00,2024-05-02 00:00+00)\n5\tempty\tempty\tempty\tempty
COPY s15.rgb FROM STDIN;
COPY s15.rgb TO STDOUT;
COPY s15.rgb TO STDOUT WITH (FORMAT binary);
SELECT * FROM rgb ORDER BY id;
-- args: [3,9)
SELECT $1::int4range @> 5;
-- args: [3,9)
SELECT $1::int4range;
-- args: 4
SELECT id FROM rgb WHERE ir @> $1::int4 ORDER BY id;
-- args: [2024-01-01,2024-01-15]
SELECT $1::daterange;

-- ### 15_types_more/teardown
DROP SCHEMA s15 CASCADE;
