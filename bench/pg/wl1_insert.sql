\set r random(1, 1000000000000000)
INSERT INTO kv VALUES ('i' || :client_id || '-' || :r, 0, repeat(md5(:r::text), 32)) ON CONFLICT DO NOTHING;
