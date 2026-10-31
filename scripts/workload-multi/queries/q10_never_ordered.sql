-- Anti join: available sims never put in an order, count by carrier and category.
SELECT s.carrier_id, s.category, count(*) AS n
FROM sims s
WHERE s.status = 'available'
  AND NOT EXISTS (SELECT 1 FROM order_items i WHERE i.sim_number = s.number)
GROUP BY s.carrier_id, s.category ORDER BY s.carrier_id, s.category;
