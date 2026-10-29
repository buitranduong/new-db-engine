-- Query set for `stashadow compare -queries`. Each statement ends with ";" at the end of a line.
-- name: sims_count_by_status
SELECT status, count(*) FROM sims GROUP BY status;
-- name: sims_count_by_carrier_status
SELECT carrier, status, count(*) FROM sims GROUP BY carrier, status;
-- name: sims_price_stats
SELECT carrier, min(price), max(price), sum(price), avg(price) FROM sims GROUP BY carrier;
-- name: sims_prefix
SELECT number, price FROM sims WHERE number LIKE '0912%' ORDER BY number LIMIT 50;
-- name: sims_suffix
SELECT number FROM sims WHERE number LIKE '%8888' ORDER BY number;
-- name: sims_filter_carrier_price
SELECT number, price FROM sims WHERE carrier = 'viettel' AND status = 'available' AND price BETWEEN 100000 AND 300000 ORDER BY number LIMIT 100;
-- name: sims_pretty_tail
SELECT count(*) FROM sims WHERE number LIKE '%888' OR number LIKE '%999';
-- name: orders_by_status
SELECT status, count(*), sum(total) FROM orders GROUP BY status;
-- name: orders_total_vs_items
SELECT o.status, count(*), sum(i.price) FROM orders o JOIN order_items i ON i.order_id = o.id WHERE i.active GROUP BY o.status;
-- name: items_live_per_number
SELECT count(*) FROM (SELECT number FROM order_items WHERE active GROUP BY number HAVING count(*) > 1) x;
-- name: reserved_sold_have_items
SELECT s.status, count(*) FROM sims s WHERE s.status IN ('reserved','sold') AND EXISTS (SELECT 1 FROM order_items i WHERE i.number = s.number AND i.active) GROUP BY s.status;
-- name: history_per_status
SELECT status, count(*) FROM sim_history GROUP BY status;
-- name: history_last_price
SELECT number, count(*), max(price) FROM sim_history GROUP BY number ORDER BY count(*) DESC, number LIMIT 20;
-- name: sims_token_not_null
SELECT count(*), count(token), count(blob), count(meta) FROM sims;
-- name: sims_point
SELECT number, carrier, status, price, grade FROM sims WHERE number = '0900000042';
