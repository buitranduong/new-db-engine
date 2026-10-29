DROP TABLE IF EXISTS kv;
CREATE TABLE kv (id text PRIMARY KEY, n bigint, val text);
-- 1 KB incompressible-enough value (32 different md5s), same size as the STASK run
INSERT INTO kv SELECT 'k' || lpad(i::text, 9, '0'), i,
  (SELECT string_agg(md5((i * 31 + j)::text), '') FROM generate_series(1, 32) j)
FROM generate_series(0, :n - 1) i;
VACUUM ANALYZE kv;
CHECKPOINT;
