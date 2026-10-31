-- Deterministic data generator. Parameters (psql -v): n (sims, default 300000), seed (text salt, default 'stask1').
-- Same n and seed give identical data on the same PostgreSQL major version (hashtext is stable within a major version;
-- STASK loads the CSV dumped by run-pg.sh dump, so engines compare on identical bytes).
\if :{?n} \else \set n 300000 \endif
\if :{?seed} \else \set seed 'stask1' \endif
\set ON_ERROR_STOP 1
-- helper: stable pseudo-random int in [0, 2^31) from a key and a salt
CREATE OR REPLACE FUNCTION h(k bigint, salt text) RETURNS bigint LANGUAGE sql IMMUTABLE AS
$$ SELECT abs(hashtext(k::text || ':' || salt))::bigint $$;

INSERT INTO carriers VALUES (1,'A','Alpha'),(2,'B','Beta'),(3,'C','Gamma');
INSERT INTO stores SELECT g, 'store-' || g, (ARRAY['north','central','south','online'])[1 + g % 4], 5 + (g * 7) % 6
  FROM generate_series(1, 40) g;
INSERT INTO customers SELECT g, (ARRAY['retail','agent','vip','corp'])[1 + h(g,:'seed' || 'seg') % 4],
  'city-' || (h(g,:'seed' || 'city') % 60) FROM generate_series(1, :n / 20) g;

INSERT INTO sims
SELECT '09' || lpad(g::text, 9, '0'),
  CASE WHEN h(g,:'seed' || 'c') % 10 < 3 THEN 1 WHEN h(g,:'seed' || 'c') % 10 < 8 THEN 2 ELSE 3 END,
  1 + h(g,:'seed' || 's') % 40,
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + h(g,:'seed' || 'k') % 5],
  500000 + (h(g,:'seed' || 'p') % 4000000)::int,
  5 + (h(g,:'seed' || 'r') % 6)::int,
  CASE WHEN h(g,:'seed' || 't') % 100 < 70 THEN 'available' WHEN h(g,:'seed' || 't') % 100 < 80 THEN 'reserved' ELSE 'sold' END,
  timestamptz '2026-08-01 00:00:00+00' + (h(g,:'seed' || 'u') % (90*86400)) * interval '1 second'
FROM generate_series(0, :n - 1) g;

-- stock: one row per (store, category) summarising the sims at generation time (invariant: see q13)
INSERT INTO stock
SELECT store_id, category, count(*), count(*) FILTER (WHERE status = 'reserved') FROM sims GROUP BY 1, 2;

-- orders: one per 10 sims, spread over the 90 days; items point to sold or reserved sims
INSERT INTO orders
SELECT g, 1 + h(g,:'seed' || 'oc') % (:n / 20), 1 + h(g,:'seed' || 'os') % 40,
  (ARRAY['open','paid','paid','shipped','cancelled'])[1 + h(g,:'seed' || 'ost') % 5],
  timestamptz '2026-08-01 00:00:00+00' + (h(g,:'seed' || 'ot') % (90*86400)) * interval '1 second', 0
FROM generate_series(1, :n / 10) g;
INSERT INTO order_items
SELECT g, 1 + (g - 1) % (:n / 10),
  '09' || lpad((h(g,:'seed' || 'oi') % :n)::text, 9, '0'), 0
FROM generate_series(1, :n * 15 / 100) g;
UPDATE order_items i SET price = s.price FROM sims s WHERE s.number = i.sim_number;
UPDATE orders o SET total = x.t FROM (SELECT order_id, sum(price) t FROM order_items GROUP BY 1) x WHERE x.order_id = o.id;

-- history: 3 changes per sim on average over the last 60 days
INSERT INTO history
SELECT g, '09' || lpad((h(g,:'seed' || 'hs') % :n)::text, 9, '0'),
  timestamptz '2026-09-01 00:00:00+00' + (h(g,:'seed' || 'ht') % (60*86400)) * interval '1 second',
  (ARRAY['price','price','status','store'])[1 + h(g,:'seed' || 'hf') % 4],
  (h(g,:'seed' || 'ho') % 4000000)::text, (h(g,:'seed' || 'hn') % 4000000)::text
FROM generate_series(1, :n * 3) g;

CREATE INDEX ON sims (category, price);
CREATE INDEX ON sims (carrier_id, status);
CREATE INDEX ON order_items (order_id);
CREATE INDEX ON order_items (sim_number);
CREATE INDEX ON orders (created_at);
CREATE INDEX ON history (sim_number, changed_at);
ANALYZE;
