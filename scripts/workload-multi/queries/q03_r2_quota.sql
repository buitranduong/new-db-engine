-- R2: quota mix per carrier (30% A / 50% B / 20% C of 100), priority store_rate, stable seeded pseudo-random spread.
-- psql -v page=1|2|3. Total order: store_rate DESC, md5(number||seed), number (number is the key).
SELECT number, carrier_id, price, store_rate FROM (
  SELECT number, carrier_id, price, store_rate,
         row_number() OVER (PARTITION BY carrier_id ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims
  WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3)
) t
WHERE (carrier_id = 1 AND rn BETWEEN (:page - 1) * 30 + 1 AND :page * 30)
   OR (carrier_id = 2 AND rn BETWEEN (:page - 1) * 50 + 1 AND :page * 50)
   OR (carrier_id = 3 AND rn BETWEEN (:page - 1) * 20 + 1 AND :page * 20)
ORDER BY store_rate DESC, md5(number || '42'), number;
