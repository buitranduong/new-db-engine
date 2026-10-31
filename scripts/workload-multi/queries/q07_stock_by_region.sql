-- Dimension join over an aggregate: stock position per region and category.
SELECT st.region, k.category, sum(k.qty) AS qty, sum(k.reserved) AS reserved, sum(k.qty - k.reserved) AS free
FROM stock k JOIN stores st ON st.id = k.store_id
GROUP BY st.region, k.category ORDER BY st.region, k.category;
