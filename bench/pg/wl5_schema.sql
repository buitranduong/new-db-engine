DROP TABLE IF EXISTS accounts; DROP TABLE IF EXISTS transfers;
CREATE TABLE accounts (id text PRIMARY KEY, balance bigint NOT NULL, owner text);
CREATE TABLE transfers (id text PRIMARY KEY, src text, dst text, amount bigint, ts timestamptz);
INSERT INTO accounts SELECT 'acc' || lpad(i::text, 6, '0'), 1000000, md5(i::text) FROM generate_series(0, :n - 1) i;
VACUUM ANALYZE accounts;
