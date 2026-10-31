-- Join + top-N: 20 best customers by spend in the last 60 days (segment and city from the dimension).
SELECT cu.id, cu.segment, cu.city, count(*) AS orders, sum(o.total) AS spend
FROM orders o JOIN customers cu ON cu.id = o.customer_id
WHERE o.created_at >= timestamptz '2026-09-01 00:00:00+00' AND o.status <> 'cancelled'
GROUP BY cu.id, cu.segment, cu.city ORDER BY spend DESC, cu.id LIMIT 20;
