\set id random(0, 999999)
\set r random(1, 1000000000)
UPDATE kv SET n = :r, val = repeat(md5(:r::text), 32) WHERE id = 'k' || lpad(:id::text, 9, '0');
