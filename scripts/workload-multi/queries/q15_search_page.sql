-- Prefix + price band + carrier filter, ORDER BY, offset page, with the total count.
SELECT number, carrier_id, price, store_rate, count(*) OVER () AS total_rows FROM sims
WHERE number LIKE '09000%' AND price BETWEEN 1500000 AND 2500000 AND carrier_id IN (1, 2) AND status = 'available'
ORDER BY store_rate DESC, price, number LIMIT 20 OFFSET 40;
