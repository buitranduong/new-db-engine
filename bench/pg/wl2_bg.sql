\set r random(1, 1000000000)
UPDATE sales SET qty = 1 + (random()*19)::int, price = round((random()*5000)::numeric, 2)
WHERE id IN (SELECT (random() * :n * 1.0)::bigint FROM generate_series(1, 100));
