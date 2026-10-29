-- Group a: DDL/DML as Liquibase and Hibernate emit it.

-- ### a/liquibase-bootstrap
-- drop: databasechangelog, databasechangeloglock
CREATE TABLE databasechangeloglock (ID INTEGER NOT NULL, LOCKED BOOLEAN NOT NULL, LOCKGRANTED TIMESTAMP WITHOUT TIME ZONE, LOCKEDBY VARCHAR(255), CONSTRAINT PK_DATABASECHANGELOGLOCK PRIMARY KEY (ID));
DELETE FROM databasechangeloglock;
INSERT INTO databasechangeloglock (ID, LOCKED) VALUES (1, FALSE);
SELECT COUNT(*) FROM databasechangeloglock;
UPDATE databasechangeloglock SET LOCKED = TRUE, LOCKEDBY = 'host (10.0.0.1)', LOCKGRANTED = '2026-01-01 10:00:00' WHERE ID = 1 AND LOCKED = FALSE;
UPDATE databasechangeloglock SET LOCKED = TRUE, LOCKEDBY = 'other', LOCKGRANTED = '2026-01-01 10:00:01' WHERE ID = 1 AND LOCKED = FALSE;
SELECT LOCKED, LOCKEDBY FROM databasechangeloglock WHERE ID = 1;
CREATE TABLE databasechangelog (ID VARCHAR(255) NOT NULL, AUTHOR VARCHAR(255) NOT NULL, FILENAME VARCHAR(255) NOT NULL, DATEEXECUTED TIMESTAMP WITHOUT TIME ZONE NOT NULL, ORDEREXECUTED INTEGER NOT NULL, EXECTYPE VARCHAR(10) NOT NULL, MD5SUM VARCHAR(35), DESCRIPTION VARCHAR(255), COMMENTS VARCHAR(255), TAG VARCHAR(255), LIQUIBASE VARCHAR(20), CONTEXTS VARCHAR(255), LABELS VARCHAR(255), DEPLOYMENT_ID VARCHAR(10));
SELECT COUNT(*) FROM databasechangelog;
SELECT MAX(ORDEREXECUTED) FROM databasechangelog;
INSERT INTO databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('1', 'admin', 'db/changelog.xml', NOW(), 1, '9:abc', 'createTable tableName=sims', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890');
INSERT INTO databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('2', 'admin', 'db/changelog.xml', NOW(), 2, '9:def', 'addColumn', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890');
SELECT ID, AUTHOR, FILENAME, EXECTYPE, ORDEREXECUTED FROM databasechangelog ORDER BY ORDEREXECUTED ASC;
SELECT MD5SUM, ID, AUTHOR, FILENAME FROM databasechangelog ORDER BY DATEEXECUTED ASC, ORDEREXECUTED ASC;
UPDATE databasechangelog SET MD5SUM = '9:new' WHERE ID = '1' AND AUTHOR = 'admin' AND FILENAME = 'db/changelog.xml';
DELETE FROM databasechangelog WHERE ID = '2' AND AUTHOR = 'admin' AND FILENAME = 'db/changelog.xml';
UPDATE databasechangeloglock SET LOCKED = FALSE, LOCKGRANTED = NULL, LOCKEDBY = NULL WHERE ID = 1;
SELECT LOCKED FROM databasechangeloglock;

-- ### a/create-table-constraints
-- drop: pd_item
CREATE TABLE pd_item (id BIGINT NOT NULL, name VARCHAR(100) NOT NULL, qty INTEGER DEFAULT 0 NOT NULL, price NUMERIC(12,2) DEFAULT 0.00, active BOOLEAN DEFAULT TRUE, created TIMESTAMP WITHOUT TIME ZONE DEFAULT now(), CONSTRAINT pk_pd_item PRIMARY KEY (id), CONSTRAINT ck_pd_qty CHECK (qty >= 0));
INSERT INTO pd_item (id, name) VALUES (1, 'a');
SELECT id, name, qty, price, active, created IS NOT NULL AS has_created FROM pd_item;
INSERT INTO pd_item (id, name, qty, price, active) VALUES (2, 'b', 5, 9.5, FALSE);
SELECT id, qty, price, active FROM pd_item ORDER BY id;
SELECT price FROM pd_item WHERE id = 2;
SELECT pg_typeof(price)::text, pg_typeof(created)::text, pg_typeof(active)::text, pg_typeof(id)::text FROM pd_item WHERE id = 1;
INSERT INTO pd_item (id, name, qty) VALUES (3, 'c', -1);
INSERT INTO pd_item (id, name) VALUES (1, 'dup');
INSERT INTO pd_item (id) VALUES (4);
INSERT INTO pd_item (id, name, qty) VALUES (5, 'e', NULL);
INSERT INTO pd_item (id, name, price) VALUES (6, 'f', 12345678901.99);
SELECT price FROM pd_item WHERE id = 6;
INSERT INTO pd_item (id, name, price) VALUES (7, 'g', 1.005);
SELECT price FROM pd_item WHERE id = 7;
INSERT INTO pd_item (id, name, price) VALUES (8, 'h', 99999999999.99);
INSERT INTO pd_item (id, name) VALUES (9, repeat('x', 101));
INSERT INTO pd_item (id, name) VALUES (10, repeat('x', 100));
SELECT length(name) FROM pd_item WHERE id = 10;

-- ### a/alter-table
-- drop: pd_alt
CREATE TABLE pd_alt (id BIGINT PRIMARY KEY, v TEXT);
INSERT INTO pd_alt VALUES (1, 'a'), (2, 'b');
ALTER TABLE pd_alt ADD COLUMN w INTEGER DEFAULT 7;
SELECT id, w FROM pd_alt ORDER BY id;
ALTER TABLE pd_alt ADD COLUMN n TIMESTAMP;
ALTER TABLE pd_alt ALTER COLUMN v SET NOT NULL;
INSERT INTO pd_alt (id, v) VALUES (3, NULL);
ALTER TABLE pd_alt ALTER COLUMN v DROP NOT NULL;
INSERT INTO pd_alt (id, v) VALUES (3, NULL);
ALTER TABLE pd_alt ALTER COLUMN w SET DEFAULT 9;
INSERT INTO pd_alt (id, v) VALUES (4, 'd');
SELECT id, w FROM pd_alt WHERE id = 4;
ALTER TABLE pd_alt ALTER COLUMN w DROP DEFAULT;
ALTER TABLE pd_alt RENAME COLUMN v TO val;
SELECT val FROM pd_alt WHERE id = 1;
ALTER TABLE pd_alt ADD CONSTRAINT ck_pd_alt CHECK (id > 0);
INSERT INTO pd_alt (id) VALUES (-5);
ALTER TABLE pd_alt DROP CONSTRAINT ck_pd_alt;
INSERT INTO pd_alt (id) VALUES (-5);
ALTER TABLE pd_alt ALTER COLUMN w TYPE BIGINT;
ALTER TABLE pd_alt ALTER COLUMN w TYPE TEXT USING w::text;
ALTER TABLE pd_alt DROP COLUMN n;
ALTER TABLE pd_alt ADD COLUMN IF NOT EXISTS w INTEGER;
ALTER TABLE pd_alt RENAME TO pd_alt2;
SELECT COUNT(*) FROM pd_alt2;
ALTER TABLE pd_alt2 RENAME TO pd_alt;
ALTER TABLE pd_nonexistent ADD COLUMN x INT;
ALTER TABLE pd_alt ADD COLUMN id INT;
ALTER TABLE pd_alt DROP COLUMN nosuchcol;

-- ### a/insert-returning
-- drop: pd_ret
CREATE TABLE pd_ret (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL, score NUMERIC(8,2), created TIMESTAMPTZ DEFAULT now());
INSERT INTO pd_ret (name, score) VALUES ('a', 1.5) RETURNING id, name, score;
INSERT INTO pd_ret (name, score) VALUES ('b', 2) RETURNING id;
INSERT INTO pd_ret (name) VALUES ('c'), ('d') RETURNING id, name;
INSERT INTO pd_ret (name) VALUES ('e') RETURNING *;
INSERT INTO pd_ret (name) VALUES ('f') RETURNING id AS new_id, upper(name) AS up;
INSERT INTO pd_ret (id, name) VALUES (100, 'g') RETURNING id;
INSERT INTO pd_ret (name) VALUES ('h') RETURNING id;
INSERT INTO pd_ret (name) VALUES ('i') RETURNING created IS NOT NULL;
-- args: z | 3.25
INSERT INTO pd_ret (name, score) VALUES ($1, $2) RETURNING id, name, score;
-- args: y | null
INSERT INTO pd_ret (name, score) VALUES ($1, $2::numeric) RETURNING name, score;
SELECT currval(pg_get_serial_sequence('pd_ret', 'id')) > 0;
SELECT COUNT(*), MAX(id) FROM pd_ret;

-- ### a/on-conflict
-- drop: pd_oc
CREATE TABLE pd_oc (id INTEGER PRIMARY KEY, v TEXT, n INTEGER DEFAULT 0);
INSERT INTO pd_oc VALUES (1, 'a', 1);
INSERT INTO pd_oc VALUES (1, 'b', 2) ON CONFLICT (id) DO NOTHING;
SELECT * FROM pd_oc;
INSERT INTO pd_oc VALUES (1, 'b', 2) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v, n = pd_oc.n + EXCLUDED.n;
SELECT * FROM pd_oc;
INSERT INTO pd_oc VALUES (1, 'c', 3) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v RETURNING id, v, n;
INSERT INTO pd_oc VALUES (2, 'x', 1) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v RETURNING id, v, n;
INSERT INTO pd_oc VALUES (1, 'd', 4) ON CONFLICT (id) DO NOTHING RETURNING id;
INSERT INTO pd_oc VALUES (1, 'e', 5) ON CONFLICT DO NOTHING;
INSERT INTO pd_oc VALUES (1, 'w', 1) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v WHERE pd_oc.n > 100;
SELECT v FROM pd_oc WHERE id = 1;
INSERT INTO pd_oc VALUES (3, 'm', 1), (3, 'n', 2) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v;
INSERT INTO pd_oc VALUES (4, 'm', 1), (5, 'n', 2) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v;
SELECT id, v FROM pd_oc ORDER BY id;
INSERT INTO pd_oc VALUES (1, 'q', 1) ON CONFLICT (v) DO NOTHING;
INSERT INTO pd_oc VALUES (1, 'q', 1);

-- ### a/update-from
-- drop: pd_u1, pd_u2
CREATE TABLE pd_u1 (id INTEGER PRIMARY KEY, v TEXT, n INTEGER);
CREATE TABLE pd_u2 (id INTEGER PRIMARY KEY, w TEXT);
INSERT INTO pd_u1 VALUES (1, 'a', 10), (2, 'b', 20), (3, 'c', 30);
INSERT INTO pd_u2 VALUES (1, 'x'), (3, 'z'), (9, 'q');
UPDATE pd_u1 SET v = pd_u2.w FROM pd_u2 WHERE pd_u1.id = pd_u2.id;
SELECT * FROM pd_u1 ORDER BY id;
UPDATE pd_u1 t SET n = t.n + 1 FROM pd_u2 s WHERE t.id = s.id RETURNING t.id, t.n, s.w;
UPDATE pd_u1 SET n = n * 2 WHERE id IN (SELECT id FROM pd_u2) RETURNING id, n;
UPDATE pd_u1 SET n = (SELECT COUNT(*) FROM pd_u2) WHERE id = 2;
UPDATE pd_u1 SET v = 'k', n = 0 WHERE id > 100;
UPDATE pd_u1 SET (v, n) = ('p', 7) WHERE id = 1;
SELECT * FROM pd_u1 WHERE id = 1;
UPDATE pd_u1 SET nosuch = 1;
UPDATE pd_u1 SET n = 'abc';
UPDATE pd_u1 SET id = 2 WHERE id = 1;

-- ### a/delete-using
-- drop: pd_d1, pd_d2
CREATE TABLE pd_d1 (id INTEGER PRIMARY KEY, grp INTEGER);
CREATE TABLE pd_d2 (grp INTEGER PRIMARY KEY, bad BOOLEAN);
INSERT INTO pd_d1 VALUES (1, 1), (2, 1), (3, 2), (4, 3);
INSERT INTO pd_d2 VALUES (1, TRUE), (2, FALSE), (3, TRUE);
DELETE FROM pd_d1 USING pd_d2 WHERE pd_d1.grp = pd_d2.grp AND pd_d2.bad RETURNING pd_d1.id;
SELECT id FROM pd_d1 ORDER BY id;
DELETE FROM pd_d1 WHERE id = 3 RETURNING *;
DELETE FROM pd_d1 WHERE id = 999;
DELETE FROM pd_d1 WHERE grp IN (SELECT grp FROM pd_d2 WHERE NOT bad);
DELETE FROM pd_d1;
TRUNCATE pd_d2;
SELECT COUNT(*) FROM pd_d2;
DELETE FROM pd_nosuchtable;

-- ### a/serial-and-identity
-- drop: pd_ser, pd_idn
CREATE TABLE pd_ser (id SERIAL PRIMARY KEY, v TEXT);
INSERT INTO pd_ser (v) VALUES ('a'), ('b');
SELECT id, v FROM pd_ser ORDER BY id;
INSERT INTO pd_ser (id, v) VALUES (10, 'c');
INSERT INTO pd_ser (v) VALUES ('d') RETURNING id;
CREATE TABLE pd_idn (id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, v TEXT);
INSERT INTO pd_idn (v) VALUES ('a') RETURNING id;
INSERT INTO pd_idn (v) VALUES ('b') RETURNING id;
SELECT pg_typeof(id)::text FROM pd_idn LIMIT 1;

-- ### a/transaction-basics
-- drop: pd_tx
CREATE TABLE pd_tx (id INTEGER PRIMARY KEY, v INTEGER);
BEGIN;
INSERT INTO pd_tx VALUES (1, 1);
SELECT COUNT(*) FROM pd_tx;
ROLLBACK;
SELECT COUNT(*) FROM pd_tx;
BEGIN;
INSERT INTO pd_tx VALUES (1, 1);
INSERT INTO pd_tx VALUES (1, 2);
SELECT COUNT(*) FROM pd_tx;
ROLLBACK;
BEGIN;
INSERT INTO pd_tx VALUES (2, 1);
COMMIT;
SELECT COUNT(*) FROM pd_tx;
COMMIT;
ROLLBACK;
BEGIN;
BEGIN;
ROLLBACK;
SAVEPOINT s1;
BEGIN;
INSERT INTO pd_tx VALUES (3, 1);
SAVEPOINT s1;
INSERT INTO pd_tx VALUES (4, 1);
ROLLBACK TO SAVEPOINT s1;
COMMIT;
SELECT id FROM pd_tx ORDER BY id;
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT 1;
COMMIT;
SHOW transaction_isolation;
