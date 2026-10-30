-- Group h: the supfix branch (docs/14 Appendix E follow-up). Cases for GROUP BY expressions, numeric
-- typing of computed columns, to_char patterns, regclass text, catalog column types, CTE and
-- identifier syntax, statistical aggregates and correlated EXISTS / IN forms.
-- All data is built with generate_series, row order is always fixed by ORDER BY.

-- ### h/hs-setup
DROP TABLE IF EXISTS hs_items;
DROP TABLE IF EXISTS hs_sims;
DROP VIEW IF EXISTS hs_v_sold;
CREATE TABLE hs_sims (number text PRIMARY KEY, carrier text NOT NULL, status text NOT NULL, price numeric(12,2) NOT NULL, qty integer NOT NULL, big bigint NOT NULL, created timestamptz NOT NULL, note text);
CREATE TABLE hs_items (id bigint PRIMARY KEY, number text NOT NULL, qty integer NOT NULL, unit_price numeric(12,2) NOT NULL, active boolean NOT NULL);
INSERT INTO hs_sims SELECT '09' || lpad(g::text, 6, '0'), CASE g % 3 WHEN 0 THEN 'viettel' WHEN 1 THEN 'mobi' ELSE 'vina' END, CASE g % 4 WHEN 0 THEN 'sold' WHEN 1 THEN 'reserved' ELSE 'available' END, (g % 50) * 1000 + 0.5, g % 7, g * 1000000000, timestamptz '2026-01-01 00:00:00+00' + g * interval '19 hours', CASE WHEN g % 5 = 0 THEN NULL ELSE 'n' || g END FROM generate_series(1, 400) g;
INSERT INTO hs_items SELECT g, '09' || lpad((1 + (g * 7) % 400)::text, 6, '0'), 1 + g % 3, ((g * 13) % 90 + 10) * 100 + 0.25, g % 3 <> 0 FROM generate_series(1, 700) g;
CREATE INDEX hs_sims_created_idx ON hs_sims (created);
CREATE INDEX hs_items_number_idx ON hs_items (number);
CREATE VIEW hs_v_sold AS SELECT number, carrier, price FROM hs_sims WHERE status = 'sold';
ANALYZE hs_sims;
SELECT count(*) FROM hs_sims;

-- ### h/hs-groupby
SELECT CASE WHEN price < 10000 THEN 'low' WHEN price < 30000 THEN 'mid' ELSE 'high' END AS b, count(*) FROM hs_sims GROUP BY CASE WHEN price < 10000 THEN 'low' WHEN price < 30000 THEN 'mid' ELSE 'high' END ORDER BY b;
SELECT CASE WHEN price < 10000 THEN 'low' ELSE 'high' END AS b, count(*), sum(price) FROM hs_sims GROUP BY 1 ORDER BY 1;
SELECT date_trunc('month', created) AS m, count(*) FROM hs_sims GROUP BY date_trunc('month', created) ORDER BY 1;
SELECT to_char(created, 'YYYY-MM') AS ym, extract(dow from created) AS d, count(*) FROM hs_sims GROUP BY to_char(created, 'YYYY-MM'), extract(dow from created) ORDER BY 1, 2;
SELECT upper(substr(carrier, 1, 2)) AS c, count(*) FROM hs_sims GROUP BY upper(substr(carrier, 1, 2)) ORDER BY 1;
SELECT coalesce(note, 'none') AS n2, count(*) FROM hs_sims WHERE qty IN (1, 2) GROUP BY coalesce(note, 'none') ORDER BY 2 DESC, 1 LIMIT 3;
SELECT CASE WHEN qty BETWEEN 1 AND 3 THEN 'a' WHEN note IS NULL THEN 'b' ELSE 'c' END AS k, count(*) FROM hs_sims GROUP BY CASE WHEN qty BETWEEN 1 AND 3 THEN 'a' WHEN note IS NULL THEN 'b' ELSE 'c' END ORDER BY 1;
SELECT floor(price / 10000) AS fl, count(*) FROM hs_sims GROUP BY floor(price / 10000) ORDER BY 1;
SELECT (qty * 2 + 1) AS q2, count(*) FROM hs_sims GROUP BY (qty * 2 + 1) HAVING count(*) > 10 ORDER BY 1;

-- ### h/hs-numeric
SELECT sum(i.qty * i.unit_price) FROM hs_items i JOIN hs_sims s ON s.number = i.number;
SELECT s.carrier, sum(i.qty * i.unit_price) AS revenue FROM hs_items i JOIN hs_sims s ON s.number = i.number GROUP BY s.carrier ORDER BY s.carrier;
SELECT extract(epoch FROM date_trunc('day', created)) * 1000 AS ts, count(*) FROM hs_sims GROUP BY 1 ORDER BY 1 LIMIT 3;
SELECT carrier, stddev(price), variance(price), stddev_pop(price), var_pop(price) FROM hs_sims GROUP BY carrier ORDER BY carrier;
SELECT stddev(qty), variance(qty), stddev(big), var_samp(big) FROM hs_sims;
SELECT stddev(price::float8), variance(price::float8) FROM hs_sims;
SELECT carrier, sum(price) * 100.0 / sum(sum(price)) OVER () AS pct FROM hs_sims GROUP BY carrier ORDER BY carrier;
SELECT carrier, count(*) AS c, sum(count(*)) OVER () AS total, sum(count(*)) OVER (ORDER BY carrier) AS running FROM hs_sims GROUP BY carrier ORDER BY carrier;
SELECT avg(qty), avg(big), sum(qty), sum(big) FROM hs_sims;
SELECT sum(big) OVER (), sum(qty) OVER () FROM hs_sims ORDER BY number LIMIT 1;
SELECT round(avg(price), 3), max(price) * 2, min(price) / 3 FROM hs_sims;

-- ### h/hs-tochar
SELECT to_char(created, 'YYYY-"W"IW') AS yw, count(*) FROM hs_sims GROUP BY 1 ORDER BY 1 LIMIT 5;
SELECT to_char(timestamptz '2026-01-05 15:04:05.123456+00', 'IYYY-IW-ID Q W WW DDD J');
SELECT to_char(timestamptz '2026-12-31 23:59:59+00', 'IYYY IW IDDD CC Y,YYY RM FMMonth FMDD');
SELECT to_char(timestamptz '2026-03-01 13:00:00+00', 'HH12:MI AM, DDth "of" Month YYYY, Dy DY');
SELECT to_char(timestamptz '2026-03-01 13:00:00.5+00', 'yyyy-mm-dd hh24:mi:ss.MS US TZ OF');
SELECT to_char(date '2027-01-03', 'IYYY-IW-ID'), to_char(date '0099-01-01', 'YYYY CC');

-- ### h/hs-regclass
SELECT 'hs_sims'::regclass::text, 'public.hs_sims'::regclass::text, to_regclass('hs_items')::text;
SELECT c.conname, c.conrelid::regclass::text, c.confrelid::regclass::text FROM pg_catalog.pg_constraint c WHERE c.conrelid = 'hs_sims'::regclass ORDER BY c.conname;
SELECT pg_total_relation_size('hs_sims') > 0, pg_relation_size('hs_sims') > 0, pg_total_relation_size('hs_sims') > pg_relation_size('hs_sims');
SELECT relname, reltuples::bigint, relpages > 0 FROM pg_class WHERE relname = 'hs_sims';
SELECT pg_get_viewdef('hs_v_sold'::regclass, true);
SELECT definition FROM pg_catalog.pg_views WHERE viewname = 'hs_v_sold';
SELECT a.attname, a.atttypid, a.attlen, a.attnum, a.attndims FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'hs_items'::regclass AND a.attnum > 0 ORDER BY a.attnum;
SELECT c.relname, c.relkind, c.relnatts FROM pg_class c WHERE c.relname IN ('hs_sims', 'hs_items') ORDER BY 1;
SELECT con.conname, con.contype, con.confmatchtype FROM pg_catalog.pg_constraint con WHERE con.conrelid = 'hs_items'::regclass ORDER BY 1;
SELECT current_schema(), current_setting('server_version_num')::int >= 160000;
SELECT column_name, ordinal_position, data_type, is_nullable FROM information_schema.columns WHERE table_name = 'hs_items' ORDER BY ordinal_position;

-- ### h/hs-syntax
WITH c AS MATERIALIZED (SELECT * FROM hs_sims WHERE status = 'sold') SELECT count(*) FROM c;
WITH c AS NOT MATERIALIZED (SELECT * FROM hs_sims WHERE status = 'sold') SELECT count(*) FROM c;
SELECT carrier AS "carrier ""x""", count(*) AS "my metric" FROM hs_sims GROUP BY 1 ORDER BY 1 LIMIT 2;
SELECT 1 AS """", 2 AS "a""";
SELECT status, CORR(price, extract(epoch FROM created)) IS NOT NULL AS has_corr FROM hs_sims GROUP BY status ORDER BY status;
SELECT status, round(corr(price, qty)::numeric, 6), round(covar_pop(price, qty)::numeric, 3), round(regr_slope(price, qty)::numeric, 3) FROM hs_sims GROUP BY status ORDER BY status;
SELECT regr_count(price, qty), round(regr_r2(price, qty)::numeric, 6), round(regr_intercept(price, qty)::numeric, 3) FROM hs_sims;
SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY price), percentile_disc(0.25) WITHIN GROUP (ORDER BY qty) FROM hs_sims;
SELECT set_config('statement_timeout', '5000', false);
SHOW max_connections;
SET search_path TO public, pg_catalog;
SHOW search_path;
SELECT pg_sleep(0.01) IS NULL;
CREATE INDEX hs_sims_note_created_idx ON hs_sims (created, qty);
SELECT count(*) FROM hs_sims WHERE created >= timestamptz '2026-02-01 00:00:00+00' AND created < timestamptz '2026-03-01 00:00:00+00';

-- ### h/hs-exists
SELECT s.status, count(*) FROM hs_sims s WHERE s.status IN ('reserved', 'sold') AND EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number AND i.active) GROUP BY s.status ORDER BY 1;
SELECT count(*) FROM hs_sims s WHERE NOT EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number AND i.active);
SELECT count(*) FROM hs_sims s WHERE EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number AND i.unit_price > s.price);
SELECT count(*) FROM hs_sims s WHERE NOT EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number AND i.qty * 1000 > s.price AND i.active);
SELECT count(*) FROM hs_sims s WHERE EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number GROUP BY i.number HAVING count(*) > 1);
SELECT count(*) FROM hs_sims s WHERE EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number HAVING count(*) = 0);
SELECT count(*) FROM hs_sims s WHERE NOT EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number HAVING sum(i.qty) > 2);
SELECT count(*) FROM hs_sims s WHERE EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number AND i.active GROUP BY i.qty HAVING max(i.unit_price) > s.price);
SELECT count(*) FROM hs_sims s WHERE s.number IN (SELECT i.number FROM hs_items i WHERE i.active GROUP BY i.number);
SELECT count(*) FROM hs_sims s WHERE s.qty NOT IN (SELECT i.qty FROM hs_items i WHERE i.number = s.number GROUP BY i.qty);
SELECT count(*) FROM hs_sims s WHERE EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number LIMIT 1);
SELECT count(*) FROM hs_sims s WHERE EXISTS (SELECT DISTINCT i.number FROM hs_items i WHERE i.number = s.number ORDER BY 1);
SELECT count(*) FROM hs_sims s WHERE EXISTS (SELECT 1 FROM hs_items i WHERE i.number = s.number AND i.id = (SELECT max(id) FROM hs_items WHERE number = s.number));

-- ### h/hs-cleanup
DROP VIEW hs_v_sold;
DROP TABLE hs_items;
DROP TABLE hs_sims;
