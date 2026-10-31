-- pgbench script (weight 2): new order with one item for a random existing sim (insert-only on orders/order_items/history).
\set n random(0, :nmax)
\set cust random(1, :ncust)
\set store random(1, 40)
\set oid random(100000000, 900000000)
\set id :oid * 100 + :client_id
BEGIN;
INSERT INTO orders VALUES (:id, :cust, :store, 'open', now(), 0) ON CONFLICT DO NOTHING;
INSERT INTO order_items SELECT :id, :id, number, price FROM sims
  WHERE number = '09' || lpad(:n::text, 9, '0') ON CONFLICT DO NOTHING;
INSERT INTO history SELECT :id, number, now(), 'status', status, 'ordered' FROM sims
  WHERE number = '09' || lpad(:n::text, 9, '0') ON CONFLICT DO NOTHING;
COMMIT;
