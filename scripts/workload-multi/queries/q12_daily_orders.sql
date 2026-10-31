-- Time series: orders and items per day (join + date_trunc group).
SELECT date_trunc('day', o.created_at) AS day, count(DISTINCT o.id) AS orders, count(i.id) AS items, sum(i.price) AS revenue
FROM orders o LEFT JOIN order_items i ON i.order_id = o.id
WHERE o.created_at >= timestamptz '2026-10-01 00:00:00+00'
GROUP BY 1 ORDER BY 1;
