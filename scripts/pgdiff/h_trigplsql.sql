-- Group h (trigplsql): PL/pgSQL trigger functions and CREATE TRIGGER ... EXECUTE FUNCTION (branch trigplsql, docs/14 appendix W).

-- ### h/tp-crm-guard
-- drop: h_tp_task
CREATE TABLE h_tp_task (id uuid PRIMARY KEY, maintenance_id uuid, description text);
CREATE OR REPLACE FUNCTION h_tp_guard()
RETURNS trigger AS $$
BEGIN
  IF OLD.maintenance_id IS NOT NULL
     AND NEW.maintenance_id IS NOT NULL
     AND NEW.maintenance_id <> OLD.maintenance_id THEN
    RAISE EXCEPTION 'Việc cần làm % đã gắn phiếu bảo trì % — không được ghi đè sang phiếu %',
      OLD.id, OLD.maintenance_id, NEW.maintenance_id
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS h_tp_guard_trg ON h_tp_task;
CREATE TRIGGER h_tp_guard_trg
BEFORE UPDATE OF maintenance_id ON h_tp_task
FOR EACH ROW EXECUTE FUNCTION h_tp_guard();
DROP TRIGGER IF EXISTS h_tp_guard_trg ON h_tp_task;
CREATE TRIGGER h_tp_guard_trg
BEFORE UPDATE OF maintenance_id ON h_tp_task
FOR EACH ROW EXECUTE FUNCTION h_tp_guard();
INSERT INTO h_tp_task VALUES ('00000000-0000-0000-0000-0000000000a1', NULL, 'x');
UPDATE h_tp_task SET maintenance_id = '11111111-1111-1111-1111-111111111111';
SELECT maintenance_id FROM h_tp_task;
UPDATE h_tp_task SET maintenance_id = '11111111-1111-1111-1111-111111111111';
UPDATE h_tp_task SET maintenance_id = NULL;
UPDATE h_tp_task SET maintenance_id = '22222222-2222-2222-2222-222222222222';
UPDATE h_tp_task SET maintenance_id = '11111111-1111-1111-1111-111111111111';
SELECT maintenance_id FROM h_tp_task;
UPDATE h_tp_task SET description = 'y';
SELECT description FROM h_tp_task;
SELECT count(*) FROM pg_trigger WHERE tgname = 'h_tp_guard_trg';
BEGIN;
UPDATE h_tp_task SET maintenance_id = '11111111-1111-1111-1111-111111111111';
SELECT 1;
ROLLBACK;
SELECT maintenance_id FROM h_tp_task;
DROP FUNCTION h_tp_guard() CASCADE;

-- ### h/tp-row-features
-- drop: h_tp_item, h_tp_log
CREATE TABLE h_tp_item (id bigint PRIMARY KEY, name text, qty bigint, note text, upd bigint);
CREATE TABLE h_tp_log (n bigserial PRIMARY KEY, msg text);
CREATE FUNCTION h_tp_bi() RETURNS trigger AS $$
DECLARE total bigint := 0; i integer;
BEGIN
  INSERT INTO h_tp_log(msg) VALUES (TG_OP || '/' || TG_NAME || '/' || TG_TABLE_NAME || '/' || TG_WHEN || '/' || TG_LEVEL || '/' || TG_NARGS || '/' || TG_ARGV[0] || '/' || TG_ARGV[1]);
  IF NEW.qty IS NULL THEN NEW.qty := 0; END IF;
  IF NEW.name = 'skip' THEN RETURN NULL; END IF;
  FOR i IN 1..4 LOOP
    IF i = 3 THEN CONTINUE; END IF;
    total := total + i;
  END LOOP;
  i := 0;
  WHILE i < 3 LOOP i := i + 1; END LOOP;
  LOOP
    i := i + 1;
    EXIT WHEN i >= 10;
  END LOOP;
  CASE
    WHEN NEW.qty > 100 THEN NEW.note := 'big';
    WHEN NEW.qty > 10 THEN NEW.note := 'medium';
    ELSE NEW.note := 'small';
  END CASE;
  CASE NEW.name WHEN 'a', 'b' THEN NEW.name := upper(NEW.name); ELSE NULL; END CASE;
  NEW.upd := total * 100 + i;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_bi BEFORE INSERT ON h_tp_item FOR EACH ROW EXECUTE PROCEDURE h_tp_bi('x', 'y z');
INSERT INTO h_tp_item VALUES (1, 'a', NULL, NULL, NULL), (2, 'skip', 5, NULL, NULL), (3, 'c', 50, NULL, NULL), (4, 'd', 500, NULL, NULL);
SELECT id, name, qty, note, upd FROM h_tp_item ORDER BY id;
SELECT msg FROM h_tp_log ORDER BY n LIMIT 1;
CREATE FUNCTION h_tp_au() RETURNS trigger AS $$
DECLARE c bigint; rec record; sum bigint := 0;
BEGIN
  SELECT count(*) INTO c FROM h_tp_item WHERE qty >= OLD.qty;
  IF EXISTS (SELECT 1 FROM h_tp_item WHERE id = NEW.id AND qty > 1000) THEN
    RAISE EXCEPTION USING MESSAGE = 'qty too large for ' || NEW.id, ERRCODE = 'check_violation';
  END IF;
  FOR rec IN SELECT id, qty FROM h_tp_item ORDER BY id LOOP
    sum := sum + rec.qty;
  END LOOP;
  PERFORM 1;
  INSERT INTO h_tp_log(msg) VALUES ('upd ' || OLD.qty || '->' || NEW.qty || ' ge=' || c || ' sum=' || sum);
  RETURN NULL;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_au AFTER UPDATE ON h_tp_item FOR EACH ROW WHEN (OLD.qty IS DISTINCT FROM NEW.qty) EXECUTE FUNCTION h_tp_au();
UPDATE h_tp_item SET name = 'zz' WHERE id = 3;
UPDATE h_tp_item SET qty = 60 WHERE id = 3;
SELECT msg FROM h_tp_log WHERE msg LIKE 'upd%';
UPDATE h_tp_item SET qty = 2000 WHERE id = 4;
SELECT qty FROM h_tp_item WHERE id = 4;
CREATE FUNCTION h_tp_bd() RETURNS trigger AS $$ BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.qty >= 100 THEN RETURN NULL; END IF;
    RETURN OLD;
  END IF;
  IF NEW.name = 'keep' THEN RETURN OLD; END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_bd BEFORE DELETE OR UPDATE OF name ON h_tp_item FOR EACH ROW EXECUTE FUNCTION h_tp_bd();
DELETE FROM h_tp_item;
SELECT id FROM h_tp_item ORDER BY id;
UPDATE h_tp_item SET name = 'keep';
SELECT name FROM h_tp_item;
CREATE FUNCTION h_tp_noret() RETURNS trigger AS $$ BEGIN NULL; END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_noret AFTER INSERT ON h_tp_log FOR EACH ROW EXECUTE FUNCTION h_tp_noret();
INSERT INTO h_tp_log(msg) VALUES ('q');
DROP TRIGGER h_tp_noret ON h_tp_log;
CREATE TRIGGER h_tp_x AFTER INSERT ON h_tp_log FOR EACH ROW EXECUTE FUNCTION h_tp_nonesuch();
CREATE FUNCTION h_tp_notrig() RETURNS int AS $$ BEGIN RETURN 1; END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_x AFTER INSERT ON h_tp_log FOR EACH ROW EXECUTE FUNCTION h_tp_notrig();
CREATE TRIGGER h_tp_x AFTER UPDATE OF nope ON h_tp_item FOR EACH ROW EXECUTE FUNCTION h_tp_au();
DROP TABLE h_tp_item;
DROP FUNCTION h_tp_bi();
DROP FUNCTION h_tp_au();
DROP FUNCTION h_tp_bd();
DROP FUNCTION h_tp_noret();
DROP FUNCTION h_tp_notrig();

-- ### h/tp-statement
-- drop: h_tp_m, h_tp_stm
CREATE TABLE h_tp_m (id bigint PRIMARY KEY, v bigint);
CREATE TABLE h_tp_stm (n bigserial PRIMARY KEY, msg text);
CREATE FUNCTION h_tp_stm() RETURNS trigger AS $$ BEGIN
  INSERT INTO h_tp_stm(msg) VALUES (TG_WHEN || ' ' || TG_OP || ' ' || TG_LEVEL);
  RETURN NULL;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_s_b BEFORE INSERT OR UPDATE OR DELETE ON h_tp_m FOR EACH STATEMENT EXECUTE FUNCTION h_tp_stm();
CREATE TRIGGER h_tp_s_a AFTER INSERT OR UPDATE OR DELETE ON h_tp_m EXECUTE FUNCTION h_tp_stm();
INSERT INTO h_tp_m VALUES (1,1),(2,2),(3,3);
UPDATE h_tp_m SET v = v + 1;
UPDATE h_tp_m SET v = 0 WHERE id = 99;
DELETE FROM h_tp_m WHERE id = 3;
SELECT msg FROM h_tp_stm ORDER BY n;
DROP FUNCTION h_tp_stm() CASCADE;

-- ### h/tp-transactions
-- drop: h_tp_a, h_tp_b, h_tp_r
CREATE TABLE h_tp_a (id bigint PRIMARY KEY, v bigint);
CREATE TABLE h_tp_b (id bigint PRIMARY KEY, v bigint);
CREATE FUNCTION h_tp_a_ai() RETURNS trigger AS $$ BEGIN
  INSERT INTO h_tp_b VALUES (NEW.id, NEW.v * 10);
  IF NEW.v < 0 THEN RAISE EXCEPTION 'negative %', NEW.v; END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_a_ai AFTER INSERT ON h_tp_a FOR EACH ROW EXECUTE FUNCTION h_tp_a_ai();
BEGIN;
INSERT INTO h_tp_a VALUES (1, 1);
SELECT count(*) FROM h_tp_b;
ROLLBACK;
SELECT (SELECT count(*) FROM h_tp_a) AS a, (SELECT count(*) FROM h_tp_b) AS b;
INSERT INTO h_tp_a VALUES (2, 2), (3, -3);
SELECT (SELECT count(*) FROM h_tp_a) AS a, (SELECT count(*) FROM h_tp_b) AS b;
BEGIN;
INSERT INTO h_tp_a VALUES (4, 4);
COMMIT;
SELECT v FROM h_tp_b;
BEGIN;
SAVEPOINT sp;
INSERT INTO h_tp_a VALUES (5, 5);
ROLLBACK TO sp;
COMMIT;
SELECT count(*) FROM h_tp_b;
CREATE TABLE h_tp_r (id bigint PRIMARY KEY, n bigint);
INSERT INTO h_tp_r VALUES (1, 0);
CREATE FUNCTION h_tp_r_au() RETURNS trigger AS $$ BEGIN
  UPDATE h_tp_r SET n = n + 1 WHERE id = NEW.id;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER h_tp_r_au AFTER UPDATE ON h_tp_r FOR EACH ROW WHEN (NEW.n < 5) EXECUTE FUNCTION h_tp_r_au();
UPDATE h_tp_r SET n = 1 WHERE id = 1;
SELECT n FROM h_tp_r;
DROP FUNCTION h_tp_a_ai() CASCADE;
DROP FUNCTION h_tp_r_au() CASCADE;
