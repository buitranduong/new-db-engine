-- Window ranking per partition, filtered (the R2 operator shape on another table): 3 cheapest available TAM_HOA sims per store.
SELECT store_id, number, price, rn FROM (
  SELECT store_id, number, price, row_number() OVER (PARTITION BY store_id ORDER BY price, number) AS rn
  FROM sims WHERE category = 'TAM_HOA' AND status = 'available'
) t WHERE rn <= 3 ORDER BY store_id, rn;
