-- Run by run-pg.sh load every few seconds from ONE session (the "daily Excel import", scaled): re-price a 1% slice of
-- sims in one multi-row UPDATE that also writes history, in one transaction. :slice in [0,99], :nmax = sim count - 1.
BEGIN;
WITH ch AS (UPDATE sims SET price = 500000 + ((price::bigint * 7 + :slice) % 4000000)::int, updated_at = now()
            WHERE (('x' || substr(md5(number), 1, 6))::bit(24)::int % 100) = :slice RETURNING number, price)
INSERT INTO history SELECT 3000000000 + (random() * 1e9)::bigint, number, now(), 'price', null, price::text FROM ch;
COMMIT;
