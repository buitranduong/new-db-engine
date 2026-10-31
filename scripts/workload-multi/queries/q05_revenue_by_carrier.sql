-- 3-table join + aggregate: revenue of paid/shipped orders in the last 30 days by carrier and region.
SELECT c.code, st.region, count(DISTINCT o.id) AS orders, count(*) AS items, sum(i.price) AS revenue
FROM orders o
JOIN order_items i ON i.order_id = o.id
JOIN sims s ON s.number = i.sim_number
JOIN carriers c ON c.id = s.carrier_id
JOIN stores st ON st.id = o.store_id
WHERE o.created_at >= timestamptz '2026-10-01 00:00:00+00' AND o.created_at < timestamptz '2026-10-31 00:00:00+00'
  AND o.status IN ('paid', 'shipped')
GROUP BY c.code, st.region ORDER BY c.code, st.region;
