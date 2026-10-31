-- Suite group 15: types beyond group 01 (interval columns, ranges, macaddr/macaddr8, bit strings).
-- Source: PostgreSQL 16 documentation chapter 8 (data types), 9.9 (date/time functions), 9.20 (range functions),
-- 9.6 (bit string functions); the PostgreSQL source behind each type is named in docs/14 (row "types").
-- Cases for this task are added only in this and later files (15_*.sql); groups 01-14 belong to other tasks.
-- Each session sets search_path to s15; setup creates the schema and teardown drops it.

-- ### 15_types_more/setup
DROP SCHEMA IF EXISTS s15 CASCADE;
CREATE SCHEMA s15;

-- Suite group 15: types beyond group 01 (interval columns, ranges, macaddr8, bit strings, tsvector/tsquery).
-- Source: PostgreSQL 16 documentation chapter 8 (data types), 9.9 (date/time), 9.20 (range functions), 9.6 (bit strings).
-- Cases are added only in this and later files (15_*.sql ...); group files 01-14 belong to other tasks.

-- ### 15_types_more/interval-input
SET search_path = s15;
SELECT '1-2'::interval;
SELECT '-1-2'::interval;
SELECT '+1-2'::interval;
SELECT '1 2:03:04'::interval;
SELECT '1 -2:03:04'::interval;
SELECT '3:04'::interval;
SELECT '04:05.5'::interval;
SELECT '5'::interval;
SELECT '1 2'::interval;
SELECT '1.5 days'::interval;
SELECT '1.5 months'::interval;
SELECT '.5 years'::interval;
SELECT '1 year 2 months ago'::interval;
SELECT '@ 1 day 2 hours ago'::interval;
SELECT '-1 day +2 hours'::interval;
SELECT '1 day -2:00'::interval;
SELECT 'P1Y2M3DT4H5M6S'::interval;
SELECT 'P0001-02-03T04:05:06'::interval;
SELECT 'PT1.5S'::interval;
SELECT 'P1W'::interval;
SELECT 'P-1Y2M'::interval;
SELECT '1 y 2 mon 3 d'::interval;
SELECT '1h 2m 3s'::interval;
SELECT '1 hour 30 min'::interval;
SELECT '2 weeks'::interval;
SELECT '1 decade'::interval;
SELECT '1 century'::interval;
SELECT '1 millennium'::interval;
SELECT '1 microsecond'::interval;
SELECT '1.2345678 sec'::interval;
SELECT '3 days 04:05:06.789'::interval;
SELECT '-00:00:01'::interval;
SELECT '12:34:56.123456789'::interval;
SELECT '1:2'::interval;
SELECT '100:00:00'::interval;
SELECT '1 day 24:00:00'::interval;
SELECT 'infinity'::interval;
SELECT '-infinity'::interval;
SELECT '1 fortnight'::interval;
SELECT '1:2:3:4'::interval;
SELECT '1 hour 2'::interval;
SELECT '1.5 hours'::interval;
SELECT '1 mon 1.5 day'::interval;
SELECT '0.1 day'::interval;
SELECT '1e2 days'::interval;
SELECT '- 1 day'::interval;
SELECT '2147483647 months'::interval;
SELECT '2147483648 months'::interval;
SELECT '178000000 years'::interval;
SELECT '1 second 2 seconds'::interval;
SELECT '1 dow'::interval;
SELECT 'now'::interval;
SELECT '1 day ago ago'::interval;
SELECT '0.5 mon'::interval;
SELECT '1.5 week'::interval;
SELECT '1 month 2 days 3 hours ago'::interval;
SELECT '-1 month +2 days'::interval;
SELECT '1:30 ago'::interval;
SELECT '+5'::interval;
SELECT '-5'::interval;
SELECT '1.9999999 sec'::interval;
SELECT '1.3 years'::interval;
SELECT '0.0000005 sec'::interval;
SELECT '1.5 millisecond'::interval;
SELECT '0.5 microsecond'::interval;
SELECT '1.5 microsecond'::interval;
SELECT '0.3 mon'::interval;
SELECT '0.7 week'::interval;
SELECT '1.123456789 minute'::interval;
SELECT '0.00001 hour'::interval;
SELECT '1.5 decade'::interval;
SELECT '0.01 century'::interval;
SELECT '1.5 millennium'::interval;
SELECT '1 sec 2 ms'::interval;
SELECT '1 hour 02:00:00'::interval;
SELECT '5 mins 01:00:00'::interval;
SELECT '1 ms 1 ms'::interval;
SELECT '-1.5 days'::interval;
SELECT '1.5 day ago'::interval;
SELECT '2.7 months'::interval;
SELECT '0.33 year'::interval;

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
SELECT id FROM iv WHERE d = '720:00:00' ORDER BY id;
SELECT id FROM iv WHERE d > '29 days' AND d <= '30 days' ORDER BY id;
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
SELECT id FROM rg WHERE ir = '[3,6]' ORDER BY id;
SELECT id FROM rg WHERE nr = '[1.50,2.5]' ORDER BY id;
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

-- ### 15_types_more/macaddr-input
SET search_path = s15;
SELECT '08:00:2b:01:02:03'::macaddr, '08-00-2b-01-02-03'::macaddr, '08002b:010203'::macaddr, '08002b-010203'::macaddr, '0800.2b01.0203'::macaddr, '0800-2b01-0203'::macaddr, '08002b010203'::macaddr, 'AA:BB:CC:DD:EE:FF'::macaddr;
SELECT '08:00:2b:01:02:03'::macaddr8, '08-00-2b-01-02-03'::macaddr8, '08002b:010203'::macaddr8, '0800.2b01.0203'::macaddr8, '08002b010203'::macaddr8, 'AA:BB:CC:DD:EE:FF'::macaddr8;
SELECT '08:00:2b:01:02:03:04:05'::macaddr8, '08-00-2b-01-02-03-04-05'::macaddr8, '08002b:0102030405'::macaddr8, '08002b-0102030405'::macaddr8, '0800.2b01.0203.0405'::macaddr8, '0800-2b01-0203-0405'::macaddr8, '08002b01:02030405'::macaddr8, '08002b0102030405'::macaddr8;
SELECT '08:00:2b:01:02'::macaddr;
SELECT '08:00:2b:01:02:03:04'::macaddr8;
SELECT 'xx:00:2b:01:02:03'::macaddr8;
SELECT '08:00:2b:01:02:03:04:05'::macaddr;
SELECT pg_typeof('08:00:2b:01:02:03'::macaddr8)::text, pg_typeof(~'08:00:2b:01:02:03'::macaddr8)::text, pg_typeof(trunc('08:00:2b:01:02:03'::macaddr))::text;

-- ### 15_types_more/macaddr-ops
SET search_path = s15;
SELECT ~'08:00:2b:01:02:03'::macaddr, ~'08:00:2b:01:02:03:04:05'::macaddr8;
SELECT '08:00:2b:01:02:03'::macaddr & 'ff:ff:ff:00:00:00'::macaddr, '08:00:2b:01:02:03'::macaddr | '00:00:00:ff:ff:ff'::macaddr;
SELECT '08:00:2b:01:02:03:04:05'::macaddr8 & 'ff:ff:ff:00:00:00:00:00'::macaddr8, '08:00:2b:01:02:03:04:05'::macaddr8 | '00:00:00:ff:ff:ff:ff:ff'::macaddr8;
SELECT trunc('08:00:2b:01:02:03'::macaddr), trunc('08:00:2b:01:02:03:04:05'::macaddr8), macaddr8_set7bit('00:00:2b:01:02:03:04:05'::macaddr8), macaddr8_set7bit('08:00:2b:01:02:03'::macaddr8);
SELECT '08:00:2b:01:02:03'::macaddr = '08:00:2b:01:02:03', '08:00:2b:01:02:03'::macaddr = '08-00-2B-01-02-03', '08:00:2b:01:02:03'::macaddr < '08:00:2b:01:02:04'::macaddr, '08:00:2b:01:02:03'::macaddr8 = '08:00:2b:ff:fe:01:02:03'::macaddr8, '09:00:2b:01:02:03'::macaddr > '08:00:2b:ff:ff:ff'::macaddr;
SELECT '08:00:2b:01:02:03'::macaddr::macaddr8, '08:00:2b:ff:fe:01:02:03'::macaddr8::macaddr;
SELECT '08:00:2b:01:02:03:04:05'::macaddr8::macaddr;
SELECT '08:00:2b:01:02:03'::macaddr::text, '08:00:2b:01:02:03:04:05'::macaddr8::text;

-- ### 15_types_more/macaddr-column
SET search_path = s15;
CREATE TABLE mc (id int PRIMARY KEY, m macaddr, m8 macaddr8);
INSERT INTO mc VALUES (1, '08:00:2b:01:02:03', '08:00:2b:01:02:03:04:05');
INSERT INTO mc VALUES (2, 'AA-BB-CC-DD-EE-FF', 'aabbccddeeff0011');
INSERT INTO mc VALUES (3, '00:00:00:00:00:01', '0800.2b01.0203');
INSERT INTO mc VALUES (4, NULL, NULL);
SELECT * FROM mc ORDER BY id;
SELECT id FROM mc WHERE m = 'AA:BB:CC:DD:EE:FF' ORDER BY id;
SELECT id FROM mc WHERE m8 > '08:00:2b:01:02:03:04:00' ORDER BY m8, id;
SELECT id FROM mc ORDER BY m8, id;
SELECT id, trunc(m), ~m8 FROM mc ORDER BY id;
SELECT id, m8::macaddr FROM mc WHERE id = 3;
CREATE UNIQUE INDEX mc_m ON mc (m);
INSERT INTO mc VALUES (5, '08-00-2b-01-02-03', NULL);
SELECT id FROM mc WHERE m = '08002b010203';
SELECT m, count(*) FROM mc GROUP BY m ORDER BY m;
SELECT pg_typeof(m)::text, pg_typeof(m8)::text FROM mc LIMIT 1;
SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'mc'::regclass AND attnum > 1 ORDER BY attnum;
CREATE TABLE mcb (id int, m macaddr, m8 macaddr8);
INSERT INTO mcb VALUES (1, '08:00:2b:01:02:03', '08:00:2b:01:02:03:04:05'), (2, NULL, NULL);
COPY s15.mcb TO STDOUT;
COPY s15.mcb TO STDOUT WITH (FORMAT binary);
-- stdin: 3\t0a:0b:0c:0d:0e:0f\t0a-0b-0c-0d-0e-0f-10-11\n
COPY s15.mcb FROM STDIN;
SELECT * FROM mcb ORDER BY id;
-- args: aa:bb:cc:dd:ee:ff
SELECT $1::macaddr8;

-- ### 15_types_more/bit-literals
SET search_path = s15;
SELECT B'101', B'', X'1F', x'ff', B'1'::bit(4), B'101'::bit(5), B'101'::bit(2), '101'::bit(3), '101'::varbit, '101'::varbit(2), 'x1F'::bit(8);
SELECT pg_typeof(B'101')::text, pg_typeof(B'101'::varbit)::text, pg_typeof(X'1F')::text, length(B'10101'), bit_length(B'10101'), octet_length(B'10101'), octet_length(B'');
SELECT B'1' = B'1', B'101' = B'101', B'101' < B'110', B'1' < B'10', B'10' > B'1', B'01' < B'1', B'0' < B'00', B'101' <> B'100';
SELECT B'1100' & B'1010', B'1100' | B'1010', B'1100' # B'1010', ~B'1100', B'1100' << 1, B'1100' << 2, B'1100' >> 1, B'1100' >> 5, B'1100' << 9, B'1100' << -1;
SELECT B'101' || B'11', B'' || B'1', B'1' || B'0' || B'1';
SELECT B'1100' & B'101';
SELECT B'1100' | B'101';
SELECT B'1100' # B'101';
SELECT get_bit(B'10110', 0), get_bit(B'10110', 1), get_bit(B'10110', 4), set_bit(B'10110', 0, 0), set_bit(B'10110', 1, 1), bit_count(B'10110'), bit_count(B'');
SELECT get_bit(B'101', 3);
SELECT set_bit(B'101', 1, 2);
SELECT substring(B'101101' from 2 for 3), substring(B'101101' from 4), substring(B'101101' from 0 for 3), substring(B'101101' from 5 for 10), substring(B'101101' from 7);
SELECT position(B'11' in B'0110'), position(B'111' in B'0110'), position(B'' in B'0110');
SELECT overlay(B'101101' placing B'00' from 2), overlay(B'101101' placing B'00' from 2 for 3), overlay(B'101101' placing B'' from 3 for 2);
SELECT 5::bit(8), 5::bit(3), 5::bit(2), (-1)::bit(4), 5::bit, 255::bit(16), (-1)::bit(40), 5::bigint::bit(70);
SELECT B'101'::int, B'11111111'::int, B'11111111'::bigint, B'1'::int;
SELECT ('1' || repeat('0', 33))::varbit::int;
SELECT B'102';
SELECT '1010'::bit(3)::text, B'1010'::text, B'101'::varbit(5)::text;
SELECT B'111' & '101', '101' | B'010';
SELECT x'zz';

-- ### 15_types_more/bit-column
SET search_path = s15;
CREATE TABLE bt (id int PRIMARY KEY, b bit(4), v varbit(6), w bit varying, f bit);
INSERT INTO bt VALUES (1, B'1010', B'101', B'1', B'1');
INSERT INTO bt VALUES (2, '0110', '', '10101010101', '0');
INSERT INTO bt VALUES (3, B'0000', B'111111', NULL, NULL);
INSERT INTO bt VALUES (4, X'F', B'01', B'', B'1');
INSERT INTO bt VALUES (5, B'101', NULL, NULL, NULL);
INSERT INTO bt VALUES (6, B'1010', B'1111111', NULL, NULL);
INSERT INTO bt VALUES (7, B'1010', B'1', NULL, B'11');
SELECT * FROM bt ORDER BY id;
SELECT id, b FROM bt ORDER BY b, id;
SELECT id, v FROM bt ORDER BY v, id;
SELECT id, w FROM bt ORDER BY w DESC, id;
SELECT id FROM bt WHERE b = B'1010' ORDER BY id;
SELECT id FROM bt WHERE b = '1010' ORDER BY id;
SELECT id FROM bt WHERE v > B'1' ORDER BY id;
SELECT id FROM bt WHERE v BETWEEN B'0' AND B'11' ORDER BY id;
SELECT id, b & B'0110', b | B'0001', b # B'1111', ~b, b << 1, b >> 1, b || v FROM bt WHERE id IN (1, 2, 4) ORDER BY id;
SELECT id, length(b), length(v), bit_length(w), get_bit(b, 1), bit_count(b), substring(v from 1 for 2), b::int FROM bt WHERE id IN (1, 2, 4) ORDER BY id;
UPDATE bt SET b = b # B'1111', v = set_bit(v, 0, 1) WHERE id = 1 RETURNING b, v;
SELECT b, count(*) FROM bt GROUP BY b ORDER BY b;
SELECT DISTINCT v FROM bt ORDER BY v;
CREATE INDEX bt_v ON bt (v);
SELECT id FROM bt WHERE v >= B'11' ORDER BY v, id;
SELECT pg_typeof(b)::text, pg_typeof(v)::text, pg_typeof(w)::text, pg_typeof(f)::text, pg_typeof(b & b)::text, pg_typeof(b || v)::text, pg_typeof(~v)::text FROM bt LIMIT 1;
SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'bt'::regclass AND attnum > 1 ORDER BY attnum;
CREATE TABLE bt2 AS SELECT id, b, v FROM bt WHERE id < 4;
SELECT * FROM bt2 ORDER BY id;
SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'bt2'::regclass AND attnum > 1 ORDER BY attnum;
CREATE TABLE bt3 (b bit(3) DEFAULT B'101', c varbit(4) CHECK (length(c) > 1));
INSERT INTO bt3 (c) VALUES (B'11');
INSERT INTO bt3 (c) VALUES (B'1');
SELECT * FROM bt3;
SELECT typname, typcategory::text FROM pg_type WHERE typname IN ('bit', 'varbit', 'macaddr8') ORDER BY typname;

-- ### 15_types_more/bit-binary
SET search_path = s15;
CREATE TABLE btb (id int, b bit(5), v varbit);
INSERT INTO btb VALUES (1, B'10110', B'101'), (2, B'00000', B''), (3, NULL, B'1111111111'), (4, B'11111', NULL);
COPY s15.btb TO STDOUT;
COPY s15.btb TO STDOUT WITH (FORMAT binary);
-- stdin: 5\t01010\t0101010101\n
COPY s15.btb FROM STDIN;
SELECT * FROM btb ORDER BY id;
-- args: 10101
SELECT $1::varbit;
-- args: 10101
SELECT $1::bit(5) & B'11000';

-- ### 15_types_more/tsvector-input
SET search_path = s15;
SELECT 'a b'::tsvector, 'b a  a'::tsvector, 'a:1 b:2'::tsvector, $$'a b':1$$::tsvector, 'a:1A'::tsvector, 'a:3,1,2B,1C'::tsvector, 'a:1D'::tsvector, 'Foo bar'::tsvector, 'a:16384'::tsvector, ''::tsvector;
SELECT $$'it''s':2 'x\\y' 'q:r'$$::tsvector, $$a\ b$$::tsvector, '''a'':1B,1A,1D'::tsvector, 'foo:5 foo:3'::tsvector, 'a:1,2 b:3'::tsvector, 'a:1 b:2 a:1'::tsvector, 'a:1 a:3 a:2'::tsvector;
SELECT 'a:0'::tsvector;
SELECT 'a:'::tsvector;
SELECT 'a:1x'::tsvector;
SELECT $$'a$$::tsvector;
SELECT 'a:1 b:2'::tsvector || 'c:1 a:3'::tsvector, 'a b'::tsvector || 'c'::tsvector, 'a:1'::tsvector || ''::tsvector;
SELECT length('a:1 b'::tsvector), 'a b'::tsvector = 'b a'::tsvector, 'a'::tsvector < 'b'::tsvector, 'a:1'::tsvector <> 'a:2'::tsvector, 'a'::tsvector::text, pg_typeof('a'::tsvector)::text;

-- ### 15_types_more/tsquery-input
SET search_path = s15;
SELECT 'a & b'::tsquery, 'a | b & c'::tsquery, '(a | b) & c'::tsquery, '!a'::tsquery, '!(a & b)'::tsquery, 'a <-> b'::tsquery, 'a <3> b'::tsquery, 'a:*'::tsquery, 'a:AB'::tsquery, 'a:*B'::tsquery, $$'a b'$$::tsquery;
SELECT 'a & !b | c'::tsquery, '!!a'::tsquery, 'a & (b & c)'::tsquery, '(a & b) & c'::tsquery, 'a | (b | c)'::tsquery, 'a <-> (b <-> c)'::tsquery, '(a <-> b) <-> c'::tsquery, 'a & b <-> c'::tsquery, '(a | b) <-> c'::tsquery, '!(a)'::tsquery, 'Fat & Rats'::tsquery, ''::tsquery, 'a<1>b'::tsquery, 'a<0>b'::tsquery;
SELECT 'a'::tsquery || 'b'::tsquery, 'a'::tsquery && 'b'::tsquery, !! 'a'::tsquery, 'a'::tsquery <-> 'b'::tsquery, 'a & b'::tsquery && 'c | d'::tsquery, ('a | b'::tsquery) <-> ('c & d'::tsquery);
SELECT ('a & b'::tsquery) @> 'a'::tsquery, 'a'::tsquery <@ 'a & b'::tsquery, 'a & b'::tsquery @> 'c'::tsquery, numnode('a & b | c'::tsquery), querytree('a & !b'::tsquery), querytree('a | !b'::tsquery), querytree('!a'::tsquery), numnode(''::tsquery);
SELECT pg_typeof('a'::tsquery)::text, pg_typeof('a'::tsquery && 'b'::tsquery)::text, pg_typeof(!! 'a'::tsquery)::text, pg_typeof('a'::tsquery <-> 'b'::tsquery)::text, 'a'::tsquery = 'a'::tsquery, 'a'::tsquery < 'b'::tsquery;
SELECT 'a & '::tsquery;
SELECT '(a'::tsquery;
SELECT 'a b'::tsquery;
SELECT 'a)'::tsquery;
SELECT '& a'::tsquery;
SELECT 'a <-> '::tsquery;
SELECT 'a:'::tsquery;

-- ### 15_types_more/tsearch-match
SET search_path = s15;
SELECT 'a:1 b:2'::tsvector @@ 'a & b'::tsquery, 'a:1 b:3'::tsvector @@ 'a <-> b'::tsquery, 'a:1 b:2'::tsvector @@ 'a <-> b'::tsquery, 'a:1A b:2'::tsvector @@ 'a:B'::tsquery, 'a:1A b:2'::tsvector @@ 'a:A'::tsquery, 'a:1A b:2'::tsvector @@ 'a:AB'::tsquery, 'abc'::tsvector @@ 'ab:*'::tsquery, 'a'::tsvector @@ '!a'::tsquery, 'a'::tsvector @@ 'b | a'::tsquery, 'a:1 b:3'::tsvector @@ 'a <2> b'::tsquery;
SELECT 'a b'::tsquery @@ 'a b'::tsvector;
SELECT 'a'::tsquery @@ 'a:1 b'::tsvector, 'b'::tsquery @@ 'a:1 b'::tsvector, 'a b'::tsvector @@ 'a <-> b'::tsquery, 'a b'::tsvector @@ ''::tsquery;
SELECT 'a:1 b:2 c:3'::tsvector @@ '(a & b) <-> c'::tsquery, 'a:1 b:2 c:3'::tsvector @@ 'a <-> b <-> c'::tsquery, 'a:1 b:2 c:4'::tsvector @@ 'a <-> b <-> c'::tsquery, 'a:1 b:2 c:3'::tsvector @@ '!b'::tsquery, 'a:1 b:2 c:3'::tsvector @@ 'a & !d'::tsquery;
SELECT 'a:1 b:2 c:3'::tsvector @@ '(a | b) <-> c'::tsquery, 'a:1 b:3 c:4'::tsvector @@ '(a | b) <-> c'::tsquery, 'a:1 b:2 c:3'::tsvector @@ 'c <-> (a | b)'::tsquery, 'the:1 quick:2 fox:3'::tsvector @@ 'quick <-> fox'::tsquery, 'the:1 quick:2 fox:3'::tsvector @@ 'the <2> fox'::tsquery, 'the:1 quick:2 fox:3'::tsvector @@ 'the <-> fox'::tsquery, 'foobar:1 foo:2'::tsvector @@ 'foo:*'::tsquery;

-- ### 15_types_more/tsearch-column
SET search_path = s15;
CREATE TABLE ts (id int PRIMARY KEY, v tsvector, q tsquery);
INSERT INTO ts VALUES (1, 'quick:1 brown:2 fox:3', 'quick & fox'), (2, 'lazy:1 dog:2', 'dog | cat'), (3, '', ''), (4, NULL, NULL), (5, 'fox:1B jumps:2', '!fox');
SELECT * FROM ts ORDER BY id;
SELECT id FROM ts WHERE v @@ 'fox'::tsquery ORDER BY id;
SELECT id FROM ts WHERE v @@ 'fox:B'::tsquery ORDER BY id;
SELECT id FROM ts WHERE v @@ q ORDER BY id;
SELECT id FROM ts WHERE 'dog lazy'::tsvector @@ q ORDER BY id;
SELECT id, length(v), numnode(q) FROM ts ORDER BY id;
SELECT id, v || 'extra:1'::tsvector FROM ts WHERE id IN (1, 2) ORDER BY id;
SELECT id FROM ts WHERE v = 'jumps:2 fox:1B' ORDER BY id;
SELECT id FROM ts ORDER BY v, id;
SELECT v, count(*) FROM ts GROUP BY v ORDER BY v;
CREATE INDEX ts_gin ON ts USING gin (v);
CREATE INDEX ts_btree ON ts (q);
SELECT id FROM ts WHERE v @@ 'dog'::tsquery ORDER BY id;
SELECT id FROM ts WHERE v = 'dog:2 lazy:1' ORDER BY id;
UPDATE ts SET v = v || 'new:1'::tsvector WHERE id = 2 RETURNING v;
SELECT pg_typeof(v)::text, pg_typeof(q)::text FROM ts LIMIT 1;
SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'ts'::regclass AND attnum > 1 ORDER BY attnum;
SELECT typname, typcategory::text FROM pg_type WHERE typname IN ('tsvector', 'tsquery') ORDER BY typname;
CREATE TABLE ts2 AS SELECT id, v, q FROM ts WHERE id < 3;
SELECT * FROM ts2 ORDER BY id;
SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'ts2'::regclass AND attnum > 1 ORDER BY attnum;

-- ### 15_types_more/tsearch-binary
SET search_path = s15;
CREATE TABLE tsb (id int, v tsvector, q tsquery);
INSERT INTO tsb VALUES (1, 'a:1A,3 bb:2 ccc', 'a & b'), (2, '', 'a:*B | !c'), (3, NULL, 'a <3> (b & c)'), (4, 'x:5', '!(a | b) & c');
COPY s15.tsb TO STDOUT;
COPY s15.tsb TO STDOUT WITH (FORMAT binary);
-- stdin: 5\t'q r':1,2\t'x' <-> 'y'\n
COPY s15.tsb FROM STDIN;
SELECT * FROM tsb ORDER BY id;
-- args: a:1 b:2
SELECT $1::tsvector;
-- args: a & b
SELECT $1::tsquery;
-- args: a & b
SELECT 'a:1 b:2'::tsvector @@ $1::tsquery;

-- ### 15_types_more/interval-aggregates
SET search_path = s15;
CREATE TABLE ivbig AS SELECT n, ((n % 97) || ' hours')::interval + ((n % 5) || ' days')::interval AS d FROM generate_series(1, 20000) n;
SELECT sum(d), avg(d), min(d), max(d), count(DISTINCT d) FROM ivbig;
SELECT n % 3 AS g, sum(d), max(d) FROM ivbig GROUP BY n % 3 ORDER BY 1;
SELECT d, count(*) FROM ivbig GROUP BY d ORDER BY d LIMIT 5;
SELECT n FROM ivbig ORDER BY d DESC, n LIMIT 5;
SELECT a.n, b.n FROM ivbig a JOIN ivbig b ON a.d = b.d AND a.n < b.n WHERE a.n < 4 ORDER BY 1, 2 LIMIT 5;
SELECT n, d - lag(d) OVER (ORDER BY n) FROM ivbig WHERE n <= 4 ORDER BY n;
SELECT n, sum(d) OVER (ORDER BY n ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) FROM ivbig WHERE n <= 4 ORDER BY n;
SELECT extract(epoch FROM sum(d)) FROM ivbig;
SELECT n FROM ivbig WHERE d = interval '1 day 1 hour' AND n < 400 ORDER BY n;

-- ### 15_types_more/types-catalog
SET search_path = s15;
CREATE TABLE allx (a interval, b interval(3), c interval year to month, d int4range, e daterange, f macaddr, g macaddr8, h bit(5), i varbit(7), j varbit, k tsvector, l tsquery);
SELECT column_name, data_type, udt_name, character_maximum_length, interval_type, interval_precision, datetime_precision FROM information_schema.columns WHERE table_schema = 's15' AND table_name = 'allx' ORDER BY ordinal_position;
SELECT attname, atttypid::regtype::text, atttypmod FROM pg_attribute WHERE attrelid = 'allx'::regclass AND attnum > 0 ORDER BY attnum;
SELECT 'int4range'::regtype::text, 'tsvector'::regtype::text, 'macaddr8'::regtype::text, 'bit'::regtype::text, 'varbit'::regtype::text, 'interval'::regtype::text, 'bit varying'::regtype::text;
SELECT typname, typlen, typtype::text, typcategory::text, typelem, typarray FROM pg_type WHERE typname IN ('interval', 'int4range', 'int8range', 'numrange', 'tsrange', 'tstzrange', 'daterange', 'macaddr', 'macaddr8', 'bit', 'varbit', 'tsvector', 'tsquery') ORDER BY typname;

-- ### 15_types_more/teardown
DROP SCHEMA s15 CASCADE;
