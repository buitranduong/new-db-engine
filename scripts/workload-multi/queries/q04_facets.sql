-- Facet counts for the shop filter panel (single table, columnar-view shape).
SELECT carrier_id, category, status, count(*) AS n, min(price) AS lo, max(price) AS hi, sum(price) AS total
FROM sims WHERE price BETWEEN 1000000 AND 3000000
GROUP BY carrier_id, category, status ORDER BY carrier_id, category, status;
