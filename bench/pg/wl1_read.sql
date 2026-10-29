\set id random(0, 999999)
SELECT val FROM kv WHERE id = 'k' || lpad(:id::text, 9, '0');
