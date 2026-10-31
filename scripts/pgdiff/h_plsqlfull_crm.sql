-- Group h (plsqlfull): the changesets of the CRM's functions.xml (docs/14 appendix V item 2) and their catalog rows.

-- ### h/crm-functions
-- drop: crm_task, crm_task2
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE OR REPLACE FUNCTION unaccent_lower(text)
            RETURNS text AS $$
              SELECT regexp_replace(lower(unaccent($1)), '[^a-z0-9]', '', 'g')
            $$ LANGUAGE SQL IMMUTABLE;
CREATE OR REPLACE FUNCTION unaccent_lower(character varying)
            RETURNS text AS $$
              SELECT unaccent_lower($1::text)
            $$ LANGUAGE SQL IMMUTABLE;
CREATE OR REPLACE FUNCTION unaccent_lower(text)
            RETURNS text AS $$
              SELECT regexp_replace(lower(unaccent($1)), '[^a-z0-9]', '', 'g')
            $$ LANGUAGE SQL IMMUTABLE;
SELECT unaccent_lower($$Đỗ Thị Hạnh$$), unaccent_lower($$Nguyễn Văn An$$::varchar), unaccent_lower(NULL);
SELECT pronargs, proargtypes::text, prorettype::regtype::text, prokind, provolatile, proisstrict, proretset, prolang FROM pg_proc WHERE proname = $$unaccent_lower$$ ORDER BY proargtypes::text;
SELECT proname, pg_get_function_arguments(oid), pg_get_function_identity_arguments(oid), pg_get_function_result(oid) FROM pg_proc WHERE proname = $$unaccent_lower$$ ORDER BY 2;
SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = $$unaccent_lower$$ ORDER BY proargtypes::text;
SELECT proname, pronargs FROM pg_proc WHERE proname LIKE $$unaccent%$$ ORDER BY proname, pronargs;
SELECT oid::regprocedure::text FROM pg_proc WHERE proname = $$unaccent_lower$$ ORDER BY 1;
SELECT p.oid::regproc::text, p.proargtypes::oidvector::text FROM pg_proc p WHERE proname = $$unaccent_lower$$ ORDER BY 2;
CREATE TABLE crm_task (id uuid PRIMARY KEY, maintenance_id uuid, description text);
CREATE OR REPLACE FUNCTION task_maintenance_link_guard()
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
DROP TRIGGER IF EXISTS trg_task_maintenance_link_guard ON crm_task;
CREATE TRIGGER trg_task_maintenance_link_guard
            BEFORE UPDATE OF maintenance_id ON crm_task
            FOR EACH ROW EXECUTE FUNCTION task_maintenance_link_guard();
INSERT INTO crm_task VALUES ($$00000000-0000-0000-0000-0000000000a1$$, NULL, $$x$$);
UPDATE crm_task SET maintenance_id = $$11111111-1111-1111-1111-111111111111$$;
UPDATE crm_task SET maintenance_id = $$22222222-2222-2222-2222-222222222222$$;
SELECT maintenance_id FROM crm_task;
SELECT proname, prokind, prorettype::regtype::text, prolang, pg_get_function_result(oid) FROM pg_proc WHERE proname = $$task_maintenance_link_guard$$;
SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname = $$trg_task_maintenance_link_guard$$;
SELECT l.lanname, p.proname FROM pg_proc p JOIN pg_language l ON l.oid = p.prolang WHERE p.proname IN ($$unaccent_lower$$, $$task_maintenance_link_guard$$) ORDER BY 2, 1;
SELECT t.tgname, p.proname FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid WHERE t.tgname = $$trg_task_maintenance_link_guard$$;
SELECT n.nspname, p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE p.proname = $$unaccent_lower$$ ORDER BY 1;
SELECT e.extname, count(*) FROM pg_extension e JOIN pg_depend d ON d.refobjid = e.oid AND d.deptype = $$e$$ JOIN pg_proc p ON d.objid = p.oid WHERE e.extname = $$unaccent$$ GROUP BY 1;
