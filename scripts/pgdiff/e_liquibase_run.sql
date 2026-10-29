-- Group e: the statements Liquibase 4.x sends at start-up and for the usual
-- changeSets (no Java on the test VM: written after its JDBC traces).
-- Tables without PRIMARY KEY (DATABASECHANGELOG) and with composite ones.

-- ### e/liquibase-startup
-- drop: lb_orders, lb_person, lb_link, databasechangelog, databasechangeloglock
-- note: Liquibase first probes the lock table; it does not exist yet
SELECT COUNT(*) FROM public.databasechangeloglock;
CREATE TABLE public.databasechangeloglock (ID INTEGER NOT NULL, LOCKED BOOLEAN NOT NULL, LOCKGRANTED TIMESTAMP WITHOUT TIME ZONE, LOCKEDBY VARCHAR(255), CONSTRAINT DATABASECHANGELOGLOCK_PKEY PRIMARY KEY (ID));
DELETE FROM public.databasechangeloglock;
INSERT INTO public.databasechangeloglock (ID, LOCKED) VALUES (1, FALSE);
SELECT COUNT(*) FROM public.databasechangeloglock;
SELECT LOCKED FROM public.databasechangeloglock WHERE ID=1;
UPDATE public.databasechangeloglock SET LOCKED = TRUE, LOCKEDBY = 'host (10.0.0.1)', LOCKGRANTED = '2026-01-01 10:00:00' WHERE ID = 1 AND LOCKED = FALSE;
SELECT MD5SUM FROM public.databasechangelog LIMIT 1;
CREATE TABLE public.databasechangelog (ID VARCHAR(255) NOT NULL, AUTHOR VARCHAR(255) NOT NULL, FILENAME VARCHAR(255) NOT NULL, DATEEXECUTED TIMESTAMP WITHOUT TIME ZONE NOT NULL, ORDEREXECUTED INTEGER NOT NULL, EXECTYPE VARCHAR(10) NOT NULL, MD5SUM VARCHAR(35), DESCRIPTION VARCHAR(255), COMMENTS VARCHAR(255), TAG VARCHAR(255), LIQUIBASE VARCHAR(20), CONTEXTS VARCHAR(255), LABELS VARCHAR(255), DEPLOYMENT_ID VARCHAR(10));
SELECT COUNT(*) FROM public.databasechangelog;
SELECT MAX(ORDEREXECUTED) FROM public.databasechangelog;
SELECT * FROM public.databasechangelog ORDER BY DATEEXECUTED ASC, ORDEREXECUTED ASC;
-- changeSet 1: createTable with a composite primary key, a default and a foreign key target
CREATE TABLE public.lb_person (id BIGINT NOT NULL, name VARCHAR(100) NOT NULL, created TIMESTAMP WITHOUT TIME ZONE DEFAULT now(), CONSTRAINT lb_person_pkey PRIMARY KEY (id));
INSERT INTO public.databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('cs-1', 'dev', 'db/changelog.xml', NOW(), 1, '9:aaa', 'createTable tableName=lb_person', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890');
-- changeSet 2: a link table whose key is composite
CREATE TABLE public.lb_link (person_id BIGINT NOT NULL, tag VARCHAR(50) NOT NULL, weight INTEGER, CONSTRAINT lb_link_pkey PRIMARY KEY (person_id, tag));
INSERT INTO public.databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('cs-2', 'dev', 'db/changelog.xml', NOW(), 2, '9:bbb', 'createTable tableName=lb_link', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890');
-- changeSet 3: a table created without a key, the key added afterwards
CREATE TABLE public.lb_orders (id BIGINT NOT NULL, person_id BIGINT, total NUMERIC(12,2));
ALTER TABLE public.lb_orders ADD CONSTRAINT lb_orders_pkey PRIMARY KEY (id);
ALTER TABLE public.lb_orders ADD CONSTRAINT fk_orders_person FOREIGN KEY (person_id) REFERENCES public.lb_person (id) ON UPDATE NO ACTION ON DELETE NO ACTION;
CREATE INDEX idx_orders_person ON public.lb_orders(person_id);
ALTER TABLE public.lb_orders ADD note VARCHAR(200);
INSERT INTO public.databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('cs-3', 'dev', 'db/changelog.xml', NOW(), 3, '9:ccc', 'addPrimaryKey, addForeignKeyConstraint, createIndex', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890');
-- changeSet 4: data
INSERT INTO public.lb_person (id, name) VALUES (1, 'ann');
INSERT INTO public.lb_person (id, name) VALUES (2, 'bob');
INSERT INTO public.lb_link (person_id, tag, weight) VALUES (1, 'x', 5);
INSERT INTO public.lb_link (person_id, tag, weight) VALUES (1, 'y', 6);
INSERT INTO public.lb_link (person_id, tag, weight) VALUES (1, 'x', 7);
INSERT INTO public.lb_orders (id, person_id, total) VALUES (10, 1, 9.50);
INSERT INTO public.lb_orders (id, person_id, total) VALUES (11, 3, 1.00);
INSERT INTO public.databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('cs-4', 'dev', 'db/changelog.xml', NOW(), 4, '9:ddd', 'insert tableName=lb_person', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890');
SELECT person_id, tag, weight FROM public.lb_link ORDER BY person_id, tag;
SELECT ID, AUTHOR, EXECTYPE, ORDEREXECUTED FROM public.databasechangelog ORDER BY ORDEREXECUTED;
-- a failed changeSet is not recorded; ranRollback
DELETE FROM public.databasechangelog WHERE ID = 'cs-4' AND AUTHOR = 'dev' AND FILENAME = 'db/changelog.xml';
SELECT COUNT(*) FROM public.databasechangelog;
-- release the lock
UPDATE public.databasechangeloglock SET LOCKED = FALSE, LOCKGRANTED = NULL, LOCKEDBY = NULL WHERE ID = 1;
SELECT LOCKED FROM public.databasechangeloglock;

-- ### e/liquibase-metadata
-- drop: lb_meta, databasechangelog
CREATE TABLE lb_meta (a INTEGER NOT NULL, b VARCHAR(20) NOT NULL, c TEXT, CONSTRAINT lb_meta_pk PRIMARY KEY (a, b));
CREATE TABLE databasechangelog (ID VARCHAR(255) NOT NULL, AUTHOR VARCHAR(255) NOT NULL);
SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('lb_meta', 'databasechangelog') ORDER BY table_name;
SELECT column_name, ordinal_position, is_nullable FROM information_schema.columns WHERE table_name = 'lb_meta' ORDER BY ordinal_position;
SELECT column_name, ordinal_position, is_nullable FROM information_schema.columns WHERE table_name = 'databasechangelog' ORDER BY ordinal_position;
SELECT kcu.column_name, kcu.ordinal_position FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON tc.constraint_name = kcu.constraint_name AND tc.table_name = kcu.table_name WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_name = 'lb_meta' ORDER BY kcu.ordinal_position;
SELECT count(*) FROM information_schema.table_constraints WHERE constraint_type = 'PRIMARY KEY' AND table_name = 'databasechangelog';
SELECT a.attname, a.attnum FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = 'lb_meta' AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT a.attname, a.attnum FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = 'databasechangelog' AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;
SELECT i.indisprimary, i.indnatts, i.indkey::text FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid WHERE c.relname = 'lb_meta';
SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid WHERE c.relname = 'databasechangelog';
SELECT con.conname, con.contype, con.conkey::text FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid WHERE c.relname = 'lb_meta';
SELECT pg_get_constraintdef(con.oid) FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid WHERE c.relname = 'lb_meta' AND con.contype = 'p';
SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'lb_meta';
SELECT count(*) FROM pg_indexes WHERE tablename = 'databasechangelog';
SELECT * FROM lb_meta;

-- ### e/composite-and-nopk-dml
-- drop: e_ck, e_np
CREATE TABLE e_ck (a INTEGER, b TEXT, v INTEGER, PRIMARY KEY (a, b));
INSERT INTO e_ck VALUES (1, 'x', 1), (1, 'y', 2), (2, 'x', 3);
INSERT INTO e_ck VALUES (1, 'x', 9);
INSERT INTO e_ck (a, v) VALUES (4, 1);
INSERT INTO e_ck VALUES (1, 'x', 5) ON CONFLICT (a, b) DO UPDATE SET v = e_ck.v + excluded.v;
INSERT INTO e_ck VALUES (1, 'y', 5) ON CONFLICT (a, b) DO NOTHING;
SELECT a, b, v FROM e_ck ORDER BY a, b;
UPDATE e_ck SET v = v * 10 WHERE a = 1;
UPDATE e_ck SET b = 'y' WHERE a = 1 AND b = 'x';
DELETE FROM e_ck WHERE a = 2 AND b = 'x' RETURNING a, b, v;
SELECT * FROM e_ck ORDER BY a, b;
CREATE TABLE e_np (g INTEGER, v TEXT);
INSERT INTO e_np VALUES (1, 'a'), (1, 'a'), (2, 'b');
SELECT * FROM e_np ORDER BY g, v;
SELECT count(*), count(DISTINCT g) FROM e_np;
UPDATE e_np SET v = 'z' WHERE g = 1 RETURNING g, v;
DELETE FROM e_np WHERE v = 'z';
SELECT * FROM e_np;
INSERT INTO e_np (g) VALUES (7) RETURNING *;
UPDATE e_np SET g = g + 1 RETURNING *;
SELECT e_np.* FROM e_np;
SELECT t.* FROM e_np t, e_ck c WHERE c.a = t.g - 5;
