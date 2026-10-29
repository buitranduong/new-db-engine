\set a random(0, :n - 1)
\set b random(0, :n - 1)
\set amt random(1, 100)
\set r random(1, 1000000000000000)
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT balance FROM accounts WHERE id = 'acc' || lpad(:a::text, 6, '0');
SELECT balance FROM accounts WHERE id = 'acc' || lpad(:b::text, 6, '0');
UPDATE accounts SET balance = balance - :amt WHERE id = 'acc' || lpad(:a::text, 6, '0');
UPDATE accounts SET balance = balance + :amt WHERE id = 'acc' || lpad(:b::text, 6, '0');
INSERT INTO transfers VALUES ('t' || :client_id || '-' || :r, 'acc' || lpad(:a::text, 6, '0'), 'acc' || lpad(:b::text, 6, '0'), :amt, now()) ON CONFLICT DO NOTHING;
COMMIT;
