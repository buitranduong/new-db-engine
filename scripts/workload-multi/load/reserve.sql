-- pgbench script (weight 5): reserve one available sim and bump stock.reserved in ONE transaction. Keeps invariant q13.
\set n random(0, :nmax)
BEGIN;
WITH r AS (UPDATE sims SET status = 'reserved', updated_at = now()
           WHERE number = '09' || lpad(:n::text, 9, '0') AND status = 'available' RETURNING store_id, category)
UPDATE stock k SET reserved = k.reserved + 1 FROM r WHERE k.store_id = r.store_id AND k.category = r.category;
COMMIT;
