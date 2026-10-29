-- Group d: SIM inventory scenario.

-- ### d/sim-setup
-- drop: sims, orders, order_lines
CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'available', price NUMERIC(12,2) NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000001', 'viettel', 'available', 1500000), ('0901000002', 'viettel', 'available', 500000.50), ('0902000003', 'mobifone', 'available', 250000), ('0903000004', 'vinaphone', 'sold', 3000000), ('0912345678', 'viettel', 'available', 12000000), ('0987654321', 'viettel', 'reserved', 800000), ('0888123456', 'vinaphone', 'available', 99000), ('0777000111', 'mobifone', 'available', 0);
SELECT COUNT(*) FROM sims;
SELECT number, price FROM sims ORDER BY price DESC, number LIMIT 3;
SELECT number FROM sims WHERE number LIKE '090%';
SELECT number FROM sims WHERE number LIKE '090%' ORDER BY number;
SELECT number FROM sims WHERE number LIKE '0901%' AND status = 'available' ORDER BY number;
SELECT number FROM sims WHERE number LIKE '%6789';
SELECT number FROM sims WHERE number LIKE '09_2%';
SELECT number FROM sims WHERE left(number, 3) = '091';
SELECT number FROM sims WHERE number >= '0900' AND number < '0910' ORDER BY number;
SELECT number FROM sims WHERE starts_with(number, '098');
SELECT number FROM sims WHERE number ~ '^09[01]';
-- args: 090%
SELECT number FROM sims WHERE number LIKE $1 ORDER BY number;
-- args: viettel | available | 1000000 | 20000000
SELECT number, price FROM sims WHERE carrier = $1 AND status = $2 AND price BETWEEN $3::numeric AND $4::numeric ORDER BY price;
SELECT number FROM sims WHERE carrier IN ('viettel', 'mobifone') AND status = 'available' AND price >= 250000 AND price <= 1500000 ORDER BY number;
SELECT carrier, COUNT(*), SUM(price), MIN(price), MAX(price), AVG(price) FROM sims GROUP BY carrier ORDER BY carrier;
SELECT status, COUNT(*) FROM sims GROUP BY status ORDER BY status;
SELECT carrier, status, COUNT(*) FROM sims GROUP BY carrier, status ORDER BY carrier, status;
SELECT carrier, COUNT(*) FROM sims WHERE status = 'available' GROUP BY carrier HAVING COUNT(*) >= 2 ORDER BY carrier;
SELECT price FROM sims WHERE number = '0901000002';
SELECT price * 2, price + 0.5, price::bigint, price::text, round(price / 3, 2) FROM sims WHERE number = '0901000002';
SELECT SUM(price), AVG(price) FROM sims WHERE status = 'available';
SELECT pg_typeof(price)::text, pg_typeof(updated_at)::text, pg_typeof(number)::text FROM sims LIMIT 1;
SELECT number, updated_at IS NOT NULL FROM sims WHERE number = '0901000001';
SELECT COUNT(*) FROM sims WHERE updated_at > now() - INTERVAL '1 hour';
SELECT COUNT(*) FROM sims WHERE updated_at >= '2020-01-01'::timestamptz;
SELECT number FROM sims WHERE price = 0;
SELECT number FROM sims WHERE price > 1e6 ORDER BY number;
SELECT number FROM sims WHERE price > 1000000 ORDER BY price DESC, number OFFSET 1 LIMIT 2;
SELECT number, price, rank() OVER (PARTITION BY carrier ORDER BY price DESC) AS r FROM sims ORDER BY carrier, r, number;
SELECT number FROM sims WHERE number LIKE '09%' AND number NOT LIKE '%0' ORDER BY number;
SELECT number FROM sims WHERE number ILIKE '090%1' ORDER BY number;
SELECT carrier, array_agg(number ORDER BY number) FROM sims WHERE status = 'available' GROUP BY carrier ORDER BY carrier;
SELECT carrier, string_agg(number, ',' ORDER BY number) FROM sims GROUP BY carrier ORDER BY carrier;
SELECT COUNT(*) FILTER (WHERE status = 'available') AS avail, COUNT(*) FILTER (WHERE status <> 'available') AS other FROM sims;
SELECT number FROM sims WHERE price BETWEEN 100000 AND 600000 ORDER BY number;
SELECT substr(number, 1, 3) AS prefix, COUNT(*) FROM sims GROUP BY substr(number, 1, 3) ORDER BY prefix;
SELECT to_char(price, 'FM999,999,999.00') FROM sims WHERE number = '0912345678';
SELECT length(number) FROM sims GROUP BY length(number);
INSERT INTO sims (number, carrier) VALUES ('0905555555', 'viettel');
SELECT status, price FROM sims WHERE number = '0905555555';
INSERT INTO sims (number, carrier, status, price) VALUES ('0905555555', 'viettel', 'available', 1);
INSERT INTO sims (number, carrier, price) VALUES ('0906666666', NULL, 1);
INSERT INTO sims (number, carrier, price) VALUES ('0906666666', 'viettel', 'abc');
INSERT INTO sims (number, carrier, price) VALUES (NULL, 'viettel', 1);
INSERT INTO sims (number, carrier, price) VALUES (905777777, 'viettel', 1);
SELECT number FROM sims WHERE number = '905777777';

-- ### d/sim-reserve
-- drop: sims
CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'available', price NUMERIC(12,2) NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000001', 'viettel', 'available', 1500000), ('0901000002', 'viettel', 'available', 500000), ('0902000003', 'mobifone', 'sold', 250000);
UPDATE sims SET status = 'reserved', updated_at = now() WHERE number = '0901000001' AND status = 'available' RETURNING number, status, price;
UPDATE sims SET status = 'reserved', updated_at = now() WHERE number = '0901000001' AND status = 'available' RETURNING number;
UPDATE sims SET status = 'reserved' WHERE number = '0902000003' AND status = 'available';
UPDATE sims SET status = 'reserved' WHERE number = '0000000000' AND status = 'available' RETURNING number;
-- args: 0901000002
UPDATE sims SET status = 'sold', updated_at = now() WHERE number = $1 AND status = 'available' RETURNING number, carrier, status, price;
-- args: 0901000002
UPDATE sims SET status = 'sold', updated_at = now() WHERE number = $1 AND status = 'available' RETURNING number;
-- args: reserved
UPDATE sims SET status = 'available', updated_at = now() WHERE status = $1 RETURNING number, status;
UPDATE sims SET status = 'reserved', updated_at = now() WHERE number IN (SELECT number FROM sims WHERE status = 'available' ORDER BY number LIMIT 1) RETURNING number;
UPDATE sims SET price = price * 1.1 WHERE carrier = 'viettel' RETURNING number, price;
UPDATE sims SET price = round(price * 0.9, 0) WHERE carrier = 'viettel';
SELECT number, price, status FROM sims ORDER BY number;
UPDATE sims SET status = 'sold' WHERE status = 'available' RETURNING number, status;
SELECT status, COUNT(*) FROM sims GROUP BY status ORDER BY status;
SELECT number FROM sims WHERE status = 'available' ORDER BY number LIMIT 1 FOR UPDATE SKIP LOCKED;
BEGIN;
SELECT number FROM sims WHERE number = '0901000001' FOR UPDATE;
UPDATE sims SET status = 'available' WHERE number = '0901000001' RETURNING number, status;
COMMIT;
UPDATE sims SET updated_at = '2026-01-01 00:00:00+00' WHERE number = '0901000001' RETURNING updated_at;
UPDATE sims SET updated_at = '2026-01-01 07:00:00+07' WHERE number = '0901000001' RETURNING updated_at;
DELETE FROM sims WHERE status = 'sold' RETURNING number;
SELECT COUNT(*) FROM sims;

-- ### d/sim-bulk-import
-- drop: sims
CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'available', price NUMERIC(12,2) NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000001', 'viettel', 'available', 100), ('0901000002', 'viettel', 'available', 200), ('0901000003', 'mobifone', 'available', 300) ON CONFLICT (number) DO UPDATE SET carrier = EXCLUDED.carrier, price = EXCLUDED.price, updated_at = now();
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000002', 'vinaphone', 'available', 250), ('0901000004', 'viettel', 'available', 400), ('0901000005', 'viettel', 'available', 500) ON CONFLICT (number) DO UPDATE SET carrier = EXCLUDED.carrier, price = EXCLUDED.price, updated_at = now();
SELECT number, carrier, price FROM sims ORDER BY number;
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000001', 'x', 'sold', 1) ON CONFLICT (number) DO UPDATE SET price = EXCLUDED.price WHERE sims.status = 'available' RETURNING number, price, status;
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000003', 'x', 'sold', 1) ON CONFLICT (number) DO NOTHING RETURNING number;
INSERT INTO sims (number, carrier, price) VALUES ('0901000006', 'viettel', 600), ('0901000006', 'viettel', 601) ON CONFLICT (number) DO UPDATE SET price = EXCLUDED.price;
INSERT INTO sims (number, carrier, price) SELECT '09020' || lpad(g::text, 5, '0'), CASE WHEN g % 2 = 0 THEN 'viettel' ELSE 'mobifone' END, g * 1000 FROM generate_series(1, 50) g ON CONFLICT (number) DO NOTHING;
SELECT COUNT(*), SUM(price) FROM sims;
SELECT carrier, COUNT(*) FROM sims WHERE number LIKE '09020%' GROUP BY carrier ORDER BY carrier;
INSERT INTO sims (number, carrier, price) SELECT '09020' || lpad(g::text, 5, '0'), 'vinaphone', 1 FROM generate_series(40, 60) g ON CONFLICT (number) DO UPDATE SET carrier = EXCLUDED.carrier RETURNING number;
SELECT COUNT(*) FROM sims WHERE carrier = 'vinaphone';
INSERT INTO sims (number, carrier, price) SELECT '0903' || lpad(g::text, 6, '0'), 'viettel', 1 FROM generate_series(1, 20) g;
SELECT COUNT(*) FROM sims;
INSERT INTO sims (number, carrier, price) SELECT number, carrier, price FROM sims WHERE number LIKE '0903%';
-- args: 0909111111 | viettel | 1234567.89
INSERT INTO sims (number, carrier, price) VALUES ($1, $2, $3) ON CONFLICT (number) DO UPDATE SET price = EXCLUDED.price RETURNING number, price;
-- args: 0909111111 | viettel | 7.5
INSERT INTO sims (number, carrier, price) VALUES ($1, $2, $3) ON CONFLICT (number) DO UPDATE SET price = EXCLUDED.price RETURNING number, price;
INSERT INTO sims (number, carrier, price) VALUES ('1', 'a', 1), ('2', 'a', 2), ('3', NULL, 3) ON CONFLICT (number) DO NOTHING;
SELECT COUNT(*) FROM sims;
DELETE FROM sims WHERE number LIKE '0903%';
SELECT COUNT(*) FROM sims;
DELETE FROM sims WHERE number LIKE '09020%' AND price < 10000 RETURNING number;
SELECT COUNT(*) FROM sims;

-- ### d/sim-order-transaction
-- drop: order_lines, orders, sims
CREATE TABLE sims (number TEXT PRIMARY KEY, carrier TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'available', price NUMERIC(12,2) NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE orders (id BIGSERIAL PRIMARY KEY, customer TEXT NOT NULL, total NUMERIC(14,2) NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE order_lines (id BIGSERIAL PRIMARY KEY, order_id BIGINT NOT NULL, number TEXT NOT NULL, price NUMERIC(12,2) NOT NULL);
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000001', 'viettel', 'available', 1500000), ('0901000002', 'viettel', 'available', 500000), ('0902000003', 'mobifone', 'available', 250000), ('0903000004', 'vinaphone', 'sold', 3000000);
BEGIN;
INSERT INTO orders (customer) VALUES ('Nguyen Van A') RETURNING id;
UPDATE sims SET status = 'sold', updated_at = now() WHERE number = '0901000001' AND status = 'available' RETURNING number, price;
INSERT INTO order_lines (order_id, number, price) VALUES (1, '0901000001', 1500000) RETURNING id, order_id, number, price;
UPDATE sims SET status = 'sold', updated_at = now() WHERE number = '0902000003' AND status = 'available' RETURNING number, price;
INSERT INTO order_lines (order_id, number, price) VALUES (1, '0902000003', 250000);
UPDATE orders SET total = (SELECT SUM(price) FROM order_lines WHERE order_id = 1) WHERE id = 1 RETURNING id, total;
SELECT COUNT(*) FROM order_lines WHERE order_id = 1;
COMMIT;
SELECT id, customer, total FROM orders;
SELECT status, COUNT(*) FROM sims GROUP BY status ORDER BY status;
BEGIN;
INSERT INTO orders (customer) VALUES ('Tran Thi B') RETURNING id;
UPDATE sims SET status = 'sold' WHERE number = '0903000004' AND status = 'available' RETURNING number;
ROLLBACK;
SELECT COUNT(*) FROM orders;
SELECT COUNT(*) FROM sims WHERE status = 'sold';
BEGIN;
INSERT INTO orders (customer) VALUES ('Le C');
UPDATE sims SET status = 'sold' WHERE number = '0901000002' AND status = 'available';
INSERT INTO order_lines (order_id, number, price) VALUES (2, '0901000002', NULL);
COMMIT;
SELECT COUNT(*) FROM orders;
SELECT status FROM sims WHERE number = '0901000002';
BEGIN;
INSERT INTO orders (customer) VALUES ('Le C');
SAVEPOINT l1;
INSERT INTO order_lines (order_id, number, price) VALUES (3, '0901000002', NULL);
ROLLBACK TO SAVEPOINT l1;
INSERT INTO order_lines (order_id, number, price) VALUES (3, '0901000002', 500000);
UPDATE orders SET total = 500000 WHERE id = 3;
COMMIT;
SELECT o.id, o.customer, o.total, COUNT(l.id) AS lines, SUM(l.price) AS line_sum FROM orders o LEFT JOIN order_lines l ON l.order_id = o.id GROUP BY o.id, o.customer, o.total ORDER BY o.id;
SELECT o.customer, l.number, s.carrier, l.price FROM orders o JOIN order_lines l ON l.order_id = o.id JOIN sims s ON s.number = l.number ORDER BY o.id, l.number;
SELECT s.carrier, SUM(l.price) FROM order_lines l JOIN sims s ON s.number = l.number GROUP BY s.carrier ORDER BY s.carrier;
SELECT s.number FROM sims s WHERE NOT EXISTS (SELECT 1 FROM order_lines l WHERE l.number = s.number) ORDER BY s.number;
SELECT s.number, l.id FROM sims s LEFT JOIN order_lines l ON l.number = s.number ORDER BY s.number;
WITH t AS (SELECT order_id, SUM(price) AS s FROM order_lines GROUP BY order_id) UPDATE orders o SET total = t.s FROM t WHERE t.order_id = o.id RETURNING o.id, o.total;
DELETE FROM order_lines WHERE order_id = 3 RETURNING id, number;
SELECT currval('orders_id_seq');
SELECT last_value FROM orders_id_seq;
