-- Group h: SQL cursors, row locks, LOCK TABLE, advisory locks, SET ROLE (task `cursors`).
-- Single-connection sessions only (pgdiff runs one connection per session): the
-- concurrent behaviour (waiting, NOWAIT, SKIP LOCKED between sessions) is covered by
-- internal/sql/cursors_test.go and the pgx/psycopg2/JDBC tests, not here.
-- ORDER BY in every cursor query: without it the row order is physical on PG and key order on STASK.

-- ### h/cur-setup
-- drop: hc_nums
CREATE TABLE hc_nums (id INT PRIMARY KEY, v TEXT, status TEXT);
INSERT INTO hc_nums SELECT g, 'v' || g, CASE WHEN g % 3 = 0 THEN 'taken' ELSE 'free' END FROM generate_series(1, 20) AS g;
SELECT count(*) FROM hc_nums;

-- ### h/cur-forward
-- drop: hc_nums
CREATE TABLE hc_nums (id INT PRIMARY KEY, v TEXT, status TEXT);
INSERT INTO hc_nums SELECT g, 'v' || g, 'free' FROM generate_series(1, 20) AS g;
BEGIN;
DECLARE c1 CURSOR FOR SELECT id, v FROM hc_nums ORDER BY id;
FETCH 3 FROM c1;
FETCH NEXT FROM c1;
FETCH c1;
FETCH FORWARD 2 FROM c1;
FETCH FORWARD FROM c1;
MOVE FORWARD 4 IN c1;
FETCH 2 IN c1;
FETCH ALL FROM c1;
FETCH NEXT FROM c1;
FETCH 0 FROM c1;
CLOSE c1;
COMMIT;

-- ### h/cur-scroll
-- drop: hc_nums
CREATE TABLE hc_nums (id INT PRIMARY KEY, v TEXT);
INSERT INTO hc_nums SELECT g, 'v' || g FROM generate_series(1, 10) AS g;
BEGIN;
DECLARE c2 SCROLL CURSOR FOR SELECT id, v FROM hc_nums ORDER BY id;
FETCH LAST FROM c2;
FETCH PRIOR FROM c2;
FETCH FIRST FROM c2;
FETCH PRIOR FROM c2;
FETCH ABSOLUTE 4 FROM c2;
FETCH ABSOLUTE -2 FROM c2;
FETCH ABSOLUTE 0 FROM c2;
FETCH ABSOLUTE 99 FROM c2;
FETCH PRIOR FROM c2;
FETCH RELATIVE -3 FROM c2;
FETCH RELATIVE 2 FROM c2;
FETCH RELATIVE 0 FROM c2;
FETCH BACKWARD 2 FROM c2;
FETCH BACKWARD ALL FROM c2;
FETCH FORWARD ALL FROM c2;
MOVE BACKWARD 3 FROM c2;
FETCH -2 FROM c2;
MOVE ABSOLUTE 5 IN c2;
FETCH NEXT FROM c2;
MOVE LAST IN c2;
MOVE FIRST IN c2;
FETCH FORWARD 0 FROM c2;
CLOSE c2;
ROLLBACK;

-- ### h/cur-errors
-- drop: hc_nums
CREATE TABLE hc_nums (id INT PRIMARY KEY, v TEXT);
INSERT INTO hc_nums SELECT g, 'v' || g FROM generate_series(1, 5) AS g;
DECLARE bad CURSOR FOR SELECT id FROM hc_nums;
FETCH 1 FROM nosuch;
CLOSE nosuch;
BEGIN;
DECLARE dup CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
DECLARE dup CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
ROLLBACK;
BEGIN;
DECLARE ns NO SCROLL CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
FETCH 2 FROM ns;
FETCH PRIOR FROM ns;
ROLLBACK;
BEGIN;
DECLARE ns2 NO SCROLL CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
FETCH FORWARD 2 FROM ns2;
FETCH FIRST FROM ns2;
ROLLBACK;
BEGIN;
DECLARE ns3 NO SCROLL CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
FETCH LAST FROM ns3;
ROLLBACK;
BEGIN;
FETCH 1 FROM gone;
ROLLBACK;

-- ### h/cur-lifecycle
-- drop: hc_nums
CREATE TABLE hc_nums (id INT PRIMARY KEY, v TEXT);
INSERT INTO hc_nums SELECT g, 'v' || g FROM generate_series(1, 5) AS g;
BEGIN;
DECLARE a1 CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
DECLARE a2 SCROLL CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
SELECT name, is_holdable, is_binary, is_scrollable FROM pg_cursors WHERE name <> '' ORDER BY name;
FETCH 1 FROM a1;
CLOSE a1;
SELECT name FROM pg_cursors WHERE name <> '' ORDER BY name;
CLOSE ALL;
SELECT count(*) FROM pg_cursors WHERE name <> '';
DECLARE a3 CURSOR WITH HOLD FOR SELECT id FROM hc_nums ORDER BY id;
COMMIT;
SELECT name, is_holdable FROM pg_cursors WHERE name <> '' ORDER BY name;
FETCH 2 FROM a3;
CLOSE a3;
SELECT count(*) FROM pg_cursors WHERE name <> '';
BEGIN;
DECLARE a4 CURSOR FOR SELECT id FROM hc_nums ORDER BY id;
ROLLBACK;
SELECT count(*) FROM pg_cursors WHERE name <> '';
FETCH 1 FROM a4;

-- ### h/cur-snapshot
-- drop: hc_nums
CREATE TABLE hc_nums (id INT PRIMARY KEY, v TEXT);
INSERT INTO hc_nums SELECT g, 'v' || g FROM generate_series(1, 6) AS g;
BEGIN;
DECLARE s1 CURSOR FOR SELECT id, v FROM hc_nums ORDER BY id;
INSERT INTO hc_nums VALUES (100, 'late');
UPDATE hc_nums SET v = 'changed' WHERE id = 2;
FETCH ALL FROM s1;
SELECT count(*) FROM hc_nums;
COMMIT;
DECLARE h1 CURSOR WITH HOLD FOR SELECT id, v FROM hc_nums ORDER BY id;
DELETE FROM hc_nums WHERE id > 3;
FETCH ALL FROM h1;
CLOSE h1;
BEGIN;
DECLARE w1 CURSOR FOR SELECT id, v FROM hc_nums WHERE id <= 3 ORDER BY id;
FETCH 1 FROM w1;
DECLARE w2 CURSOR FOR VALUES (1, 'a'), (2, 'b');
FETCH ALL FROM w2;
ROLLBACK;

-- ### h/cur-args
-- drop: hc_nums
CREATE TABLE hc_nums (id INT PRIMARY KEY, v TEXT);
INSERT INTO hc_nums SELECT g, 'v' || g FROM generate_series(1, 8) AS g;
BEGIN;
-- args: 3
DECLARE p1 CURSOR FOR SELECT id, v FROM hc_nums WHERE id > $1::int ORDER BY id;
FETCH ALL FROM p1;
DECLARE agg CURSOR FOR SELECT count(*) AS n, max(id) AS m FROM hc_nums;
FETCH 5 FROM agg;
DECLARE j CURSOR FOR SELECT a.id, b.id AS bid FROM hc_nums a JOIN hc_nums b ON b.id = a.id + 1 ORDER BY a.id;
FETCH 2 FROM j;
ROLLBACK;

-- ### h/lock-table
-- drop: hl_t
CREATE TABLE hl_t (id INT PRIMARY KEY, v TEXT);
INSERT INTO hl_t VALUES (1, 'a');
LOCK TABLE hl_t;
LOCK TABLE hl_t IN ACCESS SHARE MODE;
BEGIN;
LOCK TABLE hl_t;
COMMIT;
BEGIN;
LOCK TABLE hl_t IN ACCESS SHARE MODE;
LOCK TABLE hl_t IN ROW SHARE MODE;
LOCK TABLE hl_t IN ROW EXCLUSIVE MODE;
LOCK TABLE hl_t IN SHARE UPDATE EXCLUSIVE MODE;
LOCK TABLE hl_t IN SHARE MODE;
LOCK TABLE hl_t IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE hl_t IN EXCLUSIVE MODE;
LOCK TABLE hl_t IN ACCESS EXCLUSIVE MODE NOWAIT;
LOCK TABLE ONLY hl_t IN ACCESS EXCLUSIVE MODE;
LOCK TABLE hl_t, hl_t IN SHARE MODE;
SELECT v FROM hl_t WHERE id = 1;
UPDATE hl_t SET v = 'b' WHERE id = 1;
COMMIT;
BEGIN;
LOCK TABLE hl_missing;
ROLLBACK;
BEGIN;
LOCK TABLE hl_t IN BOGUS MODE;
ROLLBACK;

-- ### h/for-update
-- drop: hf_t
CREATE TABLE hf_t (id INT PRIMARY KEY, v TEXT, status TEXT);
INSERT INTO hf_t SELECT g, 'v' || g, CASE WHEN g % 2 = 0 THEN 'free' ELSE 'used' END FROM generate_series(1, 10) AS g;
BEGIN;
SELECT id FROM hf_t WHERE id = 3 FOR UPDATE;
SELECT id FROM hf_t WHERE status = 'free' ORDER BY id LIMIT 2 FOR UPDATE;
SELECT id FROM hf_t WHERE status = 'free' ORDER BY id LIMIT 2 FOR UPDATE SKIP LOCKED;
SELECT id FROM hf_t WHERE status = 'free' ORDER BY id LIMIT 2 FOR UPDATE NOWAIT;
SELECT id FROM hf_t ORDER BY id LIMIT 3 OFFSET 1 FOR NO KEY UPDATE;
SELECT id FROM hf_t WHERE id < 4 ORDER BY id FOR SHARE;
SELECT id FROM hf_t WHERE id < 4 ORDER BY id FOR KEY SHARE;
SELECT id FROM hf_t WHERE id < 4 ORDER BY id FOR SHARE NOWAIT;
SELECT id FROM hf_t WHERE id < 3 ORDER BY id FOR UPDATE OF hf_t;
SELECT t.id FROM hf_t t WHERE t.id < 3 ORDER BY t.id FOR UPDATE OF t SKIP LOCKED;
SELECT t.id FROM hf_t t WHERE t.id < 3 ORDER BY t.id FOR UPDATE OF nosuch;
UPDATE hf_t SET status = 'taken' WHERE id IN (2, 4);
SELECT id, status FROM hf_t WHERE id <= 4 ORDER BY id;
COMMIT;
SELECT id FROM hf_t WHERE id = 2 FOR UPDATE;
SELECT id FROM hf_t WHERE id = 99 FOR UPDATE;
SELECT count(*) FROM hf_t FOR UPDATE;
SELECT DISTINCT status FROM hf_t FOR UPDATE;
SELECT status, count(*) FROM hf_t GROUP BY status FOR UPDATE;
SELECT a.id FROM hf_t a JOIN hf_t b ON a.id = b.id WHERE a.id < 3 ORDER BY a.id FOR UPDATE;
SELECT a.id FROM hf_t a JOIN hf_t b ON a.id = b.id WHERE a.id < 3 ORDER BY a.id FOR UPDATE OF a SKIP LOCKED;

-- ### h/advisory
SELECT pg_try_advisory_lock(101);
SELECT pg_try_advisory_lock(101);
SELECT pg_advisory_unlock(101);
SELECT pg_advisory_unlock(101);
SELECT pg_advisory_unlock(101);
SELECT pg_advisory_lock(7);
SELECT pg_advisory_lock_shared(7);
SELECT pg_advisory_unlock_shared(7);
SELECT pg_advisory_unlock(7);
SELECT pg_try_advisory_lock(1, 2);
SELECT pg_try_advisory_lock_shared(1, 2);
SELECT pg_advisory_unlock(1, 2);
SELECT pg_advisory_unlock_shared(1, 2);
SELECT pg_advisory_unlock(1, 2);
SELECT pg_advisory_unlock_all();
SELECT pg_try_advisory_xact_lock(5);
BEGIN;
SELECT pg_advisory_xact_lock(5);
SELECT pg_try_advisory_xact_lock(5);
SELECT pg_try_advisory_xact_lock_shared(5);
SELECT pg_advisory_xact_lock(5, 6);
COMMIT;
SELECT pg_advisory_unlock(5);
SELECT pg_advisory_lock(9223372036854775807);
SELECT pg_advisory_unlock(9223372036854775807);
SELECT pg_advisory_lock(-1);
SELECT pg_advisory_unlock(-1);
SELECT pg_advisory_lock(NULL::bigint);
SELECT pg_advisory_lock(1, 2147483648);
SELECT pg_advisory_unlock_all();

-- ### h/role
-- drop: hr_t
CREATE TABLE hr_t (id INT PRIMARY KEY, v TEXT);
INSERT INTO hr_t VALUES (1, 'a');
DROP ROLE IF EXISTS hr_ro;
CREATE ROLE hr_ro;
GRANT SELECT ON hr_t TO hr_ro;
SHOW role;
SET ROLE hr_ro;
SHOW role;
SELECT current_user;
SELECT id FROM hr_t;
INSERT INTO hr_t VALUES (2, 'b');
SET ROLE hr_nosuch;
RESET ROLE;
SHOW role;
SET ROLE hr_ro;
SET ROLE NONE;
SHOW role;
BEGIN;
SET LOCAL ROLE hr_ro;
SELECT current_user;
COMMIT;
SHOW role;
SET SESSION AUTHORIZATION hr_ro;
SELECT session_user, current_user;
RESET SESSION AUTHORIZATION;
SELECT 1;
SET ROLE hr_ro;
DISCARD ALL;
SHOW role;
SET ROLE hr_ro;
RESET ALL;
SELECT current_user = session_user;
RESET ROLE;
DROP ROLE hr_ro;

-- ### h/session
SHOW lock_timeout;
SET lock_timeout = '2s';
SHOW lock_timeout;
RESET lock_timeout;
SHOW lock_timeout;
PREPARE hs_p AS SELECT 1;
DEALLOCATE ALL;
EXECUTE hs_p;
PREPARE hs_p2 AS SELECT 2;
DISCARD ALL;
EXECUTE hs_p2;
DISCARD PLANS;
DISCARD SEQUENCES;
DISCARD TEMP;
BEGIN;
DISCARD ALL;
ROLLBACK;
