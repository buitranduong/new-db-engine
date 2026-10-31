-- Consistency probe: stock.reserved must equal the number of reserved sims per (store, category) at ONE snapshot.
-- Must return zero rows on a quiescent database and under concurrent load when run in one snapshot (strong contract).
SELECT k.store_id, k.category, k.reserved, coalesce(x.n, 0) AS actual
FROM stock k LEFT JOIN (SELECT store_id, category, count(*) AS n FROM sims WHERE status = 'reserved' GROUP BY 1, 2) x
  ON x.store_id = k.store_id AND x.category = k.category
WHERE k.reserved <> coalesce(x.n, 0) ORDER BY 1, 2;
