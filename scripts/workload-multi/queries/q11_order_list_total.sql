-- Paginated list with the total row count (count(*) OVER ()) and two joins.
SELECT o.id, o.created_at, o.status, o.total, cu.segment, st.name, count(*) OVER () AS total_rows
FROM orders o JOIN customers cu ON cu.id = o.customer_id JOIN stores st ON st.id = o.store_id
WHERE o.status = 'paid' AND o.created_at >= timestamptz '2026-09-15 00:00:00+00' AND st.region = 'south'
ORDER BY o.created_at DESC, o.id DESC LIMIT 50 OFFSET 100;
