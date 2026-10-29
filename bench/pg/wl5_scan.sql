\set a random(0, :n - 1)
SELECT * FROM accounts WHERE id >= 'acc' || lpad(:a::text, 6, '0') ORDER BY id LIMIT 10;
