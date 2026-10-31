-- Group h (plsqlfull): triggers beyond trigplsql: whole-record NEW/OLD, INSTEAD OF, transition tables, TRUNCATE.

-- ### h/pt-record
-- drop: pt_main, pt_audit
CREATE TABLE pt_main (id int PRIMARY KEY, name text, amt numeric);
CREATE TABLE pt_audit (id serial PRIMARY KEY, op text, row_data jsonb, old_data jsonb);
CREATE FUNCTION pt_aud() RETURNS trigger AS $f$ BEGIN
  IF TG_OP = $$DELETE$$ THEN
    INSERT INTO pt_audit (op, old_data) VALUES (TG_OP, to_jsonb(OLD));
    RETURN OLD;
  ELSIF TG_OP = $$UPDATE$$ THEN
    INSERT INTO pt_audit (op, row_data, old_data) VALUES (TG_OP, to_jsonb(NEW), to_jsonb(OLD));
    RETURN NEW;
  END IF;
  INSERT INTO pt_audit (op, row_data) VALUES (TG_OP, to_jsonb(NEW));
  RETURN NEW;
END $f$ LANGUAGE plpgsql;
CREATE TRIGGER pt_aud_trg AFTER INSERT OR UPDATE OR DELETE ON pt_main FOR EACH ROW EXECUTE FUNCTION pt_aud();
INSERT INTO pt_main VALUES (1, $$a$$, 1.5), (2, $$b$$, 2);
UPDATE pt_main SET name = $$aa$$ WHERE id = 1;
DELETE FROM pt_main WHERE id = 2;
SELECT op, row_data, old_data FROM pt_audit ORDER BY id;
CREATE FUNCTION pt_cp() RETURNS trigger AS $f$ BEGIN
  INSERT INTO pt_audit (op, row_data) SELECT TG_OP, to_jsonb(t) FROM (SELECT NEW.*) t;
  RETURN NEW;
END $f$ LANGUAGE plpgsql;
DROP TRIGGER pt_aud_trg ON pt_main;
CREATE TRIGGER pt_cp_trg BEFORE INSERT ON pt_main FOR EACH ROW EXECUTE FUNCTION pt_cp();
INSERT INTO pt_main VALUES (3, $$c$$, 3);
SELECT op, row_data FROM pt_audit WHERE id > 3 ORDER BY id;

-- ### h/pt-instead
-- drop: pt_base
CREATE TABLE pt_base (id int PRIMARY KEY, name text, qty int);
INSERT INTO pt_base VALUES (1, $$a$$, 10), (2, $$b$$, 20);
CREATE VIEW pt_v AS SELECT id, upper(name) AS uname, qty * 2 AS dbl FROM pt_base;
CREATE FUNCTION pt_vtrg() RETURNS trigger AS $f$ BEGIN
  IF TG_OP = $$INSERT$$ THEN
    INSERT INTO pt_base VALUES (NEW.id, lower(NEW.uname), NEW.dbl / 2);
    RETURN NEW;
  ELSIF TG_OP = $$UPDATE$$ THEN
    UPDATE pt_base SET name = lower(NEW.uname), qty = NEW.dbl / 2 WHERE id = OLD.id;
    RETURN NEW;
  ELSE
    DELETE FROM pt_base WHERE id = OLD.id;
    RETURN OLD;
  END IF;
END $f$ LANGUAGE plpgsql;
CREATE TRIGGER pt_vtrg_i INSTEAD OF INSERT OR UPDATE OR DELETE ON pt_v FOR EACH ROW EXECUTE FUNCTION pt_vtrg();
INSERT INTO pt_v VALUES (3, $$CCC$$, 60);
SELECT * FROM pt_base ORDER BY id;
UPDATE pt_v SET dbl = 100 WHERE id = 1;
SELECT * FROM pt_base ORDER BY id;
DELETE FROM pt_v WHERE id >= 2;
SELECT * FROM pt_base ORDER BY id;
INSERT INTO pt_v (id, uname, dbl) SELECT 7, $$X$$, 8;
SELECT * FROM pt_base ORDER BY id;
CREATE TRIGGER pt_bad INSTEAD OF INSERT ON pt_base FOR EACH ROW EXECUTE FUNCTION pt_vtrg();
CREATE TRIGGER pt_bad2 AFTER INSERT ON pt_v FOR EACH ROW EXECUTE FUNCTION pt_vtrg();
SELECT tgname, tgtype FROM pg_trigger WHERE tgname = $$pt_vtrg_i$$;
SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname = $$pt_vtrg_i$$;
DROP VIEW pt_v;
SELECT count(*) FROM pg_trigger WHERE tgname = $$pt_vtrg_i$$;

-- ### h/pt-null-instead
-- drop: pt_base2
CREATE TABLE pt_base2 (id int PRIMARY KEY, v int);
CREATE VIEW pt_v2 AS SELECT id, v FROM pt_base2;
CREATE FUNCTION pt_skip() RETURNS trigger AS $f$ BEGIN RETURN NULL; END $f$ LANGUAGE plpgsql;
CREATE TRIGGER pt_skip_trg INSTEAD OF INSERT ON pt_v2 FOR EACH ROW EXECUTE FUNCTION pt_skip();
INSERT INTO pt_v2 VALUES (1, 1), (2, 2);
SELECT count(*) FROM pt_base2;
DROP VIEW pt_v2;

-- ### h/pt-transition
-- drop: pt_tt, pt_tlog
CREATE TABLE pt_tt (id int PRIMARY KEY, v int);
CREATE TABLE pt_tlog (op text, n int, s int);
CREATE FUNCTION pt_trans_ins() RETURNS trigger AS $f$ BEGIN
  INSERT INTO pt_tlog SELECT TG_OP, count(*), sum(v) FROM newrows;
  RETURN NULL;
END $f$ LANGUAGE plpgsql;
CREATE FUNCTION pt_trans_upd() RETURNS trigger AS $f$ BEGIN
  INSERT INTO pt_tlog SELECT TG_OP, count(*), sum(n.v - o.v) FROM newrows n JOIN oldrows o ON o.id = n.id;
  RETURN NULL;
END $f$ LANGUAGE plpgsql;
CREATE FUNCTION pt_trans_del() RETURNS trigger AS $f$ BEGIN
  INSERT INTO pt_tlog SELECT TG_OP, count(*), sum(v) FROM oldrows;
  RETURN NULL;
END $f$ LANGUAGE plpgsql;
CREATE TRIGGER pt_ti AFTER INSERT ON pt_tt REFERENCING NEW TABLE AS newrows FOR EACH STATEMENT EXECUTE FUNCTION pt_trans_ins();
CREATE TRIGGER pt_tu AFTER UPDATE ON pt_tt REFERENCING OLD TABLE AS oldrows NEW TABLE AS newrows FOR EACH STATEMENT EXECUTE FUNCTION pt_trans_upd();
CREATE TRIGGER pt_td AFTER DELETE ON pt_tt REFERENCING OLD TABLE AS oldrows FOR EACH STATEMENT EXECUTE FUNCTION pt_trans_del();
INSERT INTO pt_tt VALUES (1, 10), (2, 20), (3, 30);
UPDATE pt_tt SET v = v + 1 WHERE id < 3;
DELETE FROM pt_tt WHERE id > 1;
UPDATE pt_tt SET v = v WHERE false;
SELECT * FROM pt_tlog ORDER BY op;
SELECT tgname, pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = $$pt_tt$$::regclass ORDER BY tgname;
CREATE TRIGGER pt_bad_ref BEFORE INSERT ON pt_tt REFERENCING NEW TABLE AS x FOR EACH STATEMENT EXECUTE FUNCTION pt_trans_ins();
CREATE TRIGGER pt_bad_ref2 AFTER INSERT OR UPDATE ON pt_tt REFERENCING NEW TABLE AS x FOR EACH STATEMENT EXECUTE FUNCTION pt_trans_ins();

-- ### h/pt-truncate
-- drop: pt_tr, pt_trlog
CREATE TABLE pt_tr (id int);
CREATE TABLE pt_trlog (msg text);
INSERT INTO pt_tr VALUES (1), (2);
CREATE FUNCTION pt_trunc() RETURNS trigger AS $f$ BEGIN INSERT INTO pt_trlog VALUES (TG_WHEN || $$ $$ || TG_OP || $$ $$ || TG_LEVEL); RETURN NULL; END $f$ LANGUAGE plpgsql;
CREATE TRIGGER pt_trb BEFORE TRUNCATE ON pt_tr FOR EACH STATEMENT EXECUTE FUNCTION pt_trunc();
CREATE TRIGGER pt_tra AFTER TRUNCATE ON pt_tr FOR EACH STATEMENT EXECUTE FUNCTION pt_trunc();
TRUNCATE pt_tr;
SELECT count(*) FROM pt_tr;
SELECT msg FROM pt_trlog ORDER BY msg;
SELECT tgname, tgtype FROM pg_trigger WHERE tgrelid = $$pt_tr$$::regclass ORDER BY tgname;
CREATE TRIGGER pt_trrow BEFORE TRUNCATE ON pt_tr FOR EACH ROW EXECUTE FUNCTION pt_trunc();
BEGIN;
INSERT INTO pt_tr VALUES (5);
TRUNCATE pt_tr;
ROLLBACK;
SELECT count(*) FROM pt_trlog;
