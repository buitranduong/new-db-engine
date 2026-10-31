-- Group h (plsqlfull): PL/pgSQL completeness (branch plsqlfull, docs/14 appendix Z).

-- ### h/pf-out-params
CREATE FUNCTION pf_minmax(a int, b int, OUT lo int, OUT hi int) AS $f$ BEGIN lo := least(a,b); hi := greatest(a,b); END $f$ LANGUAGE plpgsql;
SELECT * FROM pf_minmax(5, 3);
SELECT lo, hi FROM pf_minmax(1, 9);
SELECT pf_minmax(7, 2);
CREATE FUNCTION pf_one_out(x int, OUT y int) AS $f$ BEGIN y := x * 2; END $f$ LANGUAGE plpgsql;
SELECT pf_one_out(4);
SELECT pg_typeof(pf_one_out(4))::text;
CREATE FUNCTION pf_inout(INOUT a int, INOUT b text) AS $f$ BEGIN a := a + 1; b := b || 1; END $f$ LANGUAGE plpgsql;
SELECT * FROM pf_inout(1, $$x$$);
DROP FUNCTION pf_minmax(int, int);
DROP FUNCTION pf_one_out(int);
DROP FUNCTION pf_inout(int, text);

-- ### h/pf-defaults
CREATE FUNCTION pf_def(a int, b int DEFAULT 10, c text DEFAULT $$z$$) RETURNS text AS $f$ BEGIN RETURN a || $$-$$ || b || $$-$$ || c; END $f$ LANGUAGE plpgsql;
SELECT pf_def(1);
SELECT pf_def(1, 2);
SELECT pf_def(1, 2, $$q$$);
SELECT pf_def(a => 5, c => $$k$$);
SELECT pf_def();
DROP FUNCTION pf_def(int, int, text);

-- ### h/pf-overload
CREATE FUNCTION pf_ov(a int) RETURNS text AS $f$ SELECT $$int$$ $f$ LANGUAGE sql;
CREATE FUNCTION pf_ov(a text) RETURNS text AS $f$ SELECT $$text$$ $f$ LANGUAGE sql;
CREATE FUNCTION pf_ov(a int, b int) RETURNS text AS $f$ SELECT $$int2$$ $f$ LANGUAGE sql;
SELECT pf_ov(1);
SELECT pf_ov($$a$$::text);
SELECT pf_ov(1, 2);
SELECT pf_ov(1.5);
CREATE OR REPLACE FUNCTION pf_ov(a int) RETURNS text AS $f$ SELECT $$int-new$$ $f$ LANGUAGE sql;
SELECT pf_ov(1);
SELECT pf_ov($$a$$::text);
SELECT count(*) FROM pg_proc WHERE proname = $$pf_ov$$;
DROP FUNCTION pf_ov(int);
SELECT pf_ov(1, 2);
SELECT pf_ov($$a$$::text);
DROP FUNCTION pf_ov(text);
DROP FUNCTION pf_ov(int, int);

-- ### h/pf-return-type
CREATE FUNCTION pf_i() RETURNS integer AS $f$ BEGIN RETURN 5; END $f$ LANGUAGE plpgsql;
SELECT pf_i();
SELECT pg_typeof(pf_i())::text;
CREATE FUNCTION pf_si() RETURNS smallint AS $f$ SELECT 5::smallint $f$ LANGUAGE sql;
SELECT pg_typeof(pf_si())::text;
CREATE FUNCTION pf_n() RETURNS numeric AS $f$ SELECT 1.5 $f$ LANGUAGE sql;
SELECT pg_typeof(pf_n())::text;
CREATE FUNCTION pf_d() RETURNS date AS $f$ SELECT current_date $f$ LANGUAGE sql;
SELECT pg_typeof(pf_d())::text;
DROP FUNCTION pf_i();
DROP FUNCTION pf_si();
DROP FUNCTION pf_n();
DROP FUNCTION pf_d();

-- ### h/pf-setof
CREATE TABLE pf_t (id int PRIMARY KEY, name text);
INSERT INTO pf_t VALUES (1,$$a$$),(2,$$b$$),(3,$$c$$);
CREATE FUNCTION pf_rq() RETURNS TABLE(id int, name text) AS $f$ BEGIN RETURN QUERY SELECT t.id, t.name FROM pf_t t ORDER BY t.id; END $f$ LANGUAGE plpgsql;
SELECT * FROM pf_rq();
SELECT name FROM pf_rq() WHERE id > 1;
CREATE FUNCTION pf_rn(n int) RETURNS SETOF int AS $f$ BEGIN FOR i IN 1..n LOOP RETURN NEXT i * i; END LOOP; RETURN; END $f$ LANGUAGE plpgsql;
SELECT * FROM pf_rn(4);
SELECT sum(x) FROM pf_rn(4) x;
SELECT pf_rn(3);
CREATE FUNCTION pf_rt() RETURNS SETOF pf_t AS $f$ SELECT * FROM pf_t ORDER BY id $f$ LANGUAGE sql;
SELECT * FROM pf_rt();
CREATE FUNCTION pf_rnt() RETURNS TABLE(a int, b text) AS $f$ BEGIN a := 1; b := $$x$$; RETURN NEXT; a := 2; b := $$y$$; RETURN NEXT; END $f$ LANGUAGE plpgsql;
SELECT * FROM pf_rnt();
CREATE FUNCTION pf_rqe(tbl text) RETURNS SETOF int AS $f$ BEGIN RETURN QUERY EXECUTE format($$SELECT id FROM %I ORDER BY id$$, tbl); END $f$ LANGUAGE plpgsql;
SELECT * FROM pf_rqe($$pf_t$$);
SELECT * FROM pf_rn(2) WITH ORDINALITY;
SELECT * FROM pf_t t, pf_rn(2) r WHERE t.id = r;
DROP TABLE pf_t CASCADE;
DROP FUNCTION pf_rq();
DROP FUNCTION pf_rn(int);
DROP FUNCTION pf_rnt();
DROP FUNCTION pf_rqe(text);

-- ### h/pf-diagnostics
CREATE TABLE pf_d (id int PRIMARY KEY, v int);
CREATE FUNCTION pf_gd() RETURNS text AS $f$ DECLARE n int; m int; f boolean; BEGIN
  INSERT INTO pf_d SELECT g, g FROM generate_series(1,5) g;
  GET DIAGNOSTICS n = ROW_COUNT;
  UPDATE pf_d SET v = v + 1 WHERE id > 2;
  GET DIAGNOSTICS m = ROW_COUNT;
  UPDATE pf_d SET v = 0 WHERE id > 100;
  f := FOUND;
  RETURN n || $$/$$ || m || $$/$$ || f;
END $f$ LANGUAGE plpgsql;
SELECT pf_gd();
DO $f$ DECLARE n int; BEGIN EXECUTE $$DELETE FROM pf_d WHERE id < 3$$; GET DIAGNOSTICS n = ROW_COUNT; RAISE NOTICE $$deleted %$$, n; END $f$;
DO $f$ DECLARE n int; BEGIN PERFORM 1 FROM pf_d; IF FOUND THEN RAISE NOTICE $$found$$; END IF; SELECT 1 INTO n WHERE false; IF NOT FOUND THEN RAISE NOTICE $$nf$$; END IF; END $f$;
DROP TABLE pf_d;

-- ### h/pf-foreach
CREATE FUNCTION pf_fe(a int[]) RETURNS int AS $f$ DECLARE s int := 0; x int; BEGIN FOREACH x IN ARRAY a LOOP s := s + x; END LOOP; RETURN s; END $f$ LANGUAGE plpgsql;
SELECT pf_fe(ARRAY[1,2,3,4]);
SELECT pf_fe(ARRAY[]::int[]);
CREATE FUNCTION pf_fe2(a int[]) RETURNS text AS $f$ DECLARE s text := $$$$; x int[]; BEGIN FOREACH x SLICE 1 IN ARRAY a LOOP s := s || x::text; END LOOP; RETURN s; END $f$ LANGUAGE plpgsql;
SELECT pf_fe2(ARRAY[[1,2],[3,4]]);
DROP FUNCTION pf_fe(int[]);
DROP FUNCTION pf_fe2(int[]);

-- ### h/pf-cursors
CREATE TABLE pf_c (id int, n text);
INSERT INTO pf_c VALUES (1,$$a$$),(2,$$b$$),(3,$$c$$);
CREATE FUNCTION pf_cur() RETURNS text AS $f$ DECLARE c CURSOR FOR SELECT id, n FROM pf_c ORDER BY id; r record; s text := $$$$; BEGIN
  OPEN c;
  LOOP FETCH c INTO r; EXIT WHEN NOT FOUND; s := s || r.id || r.n; END LOOP;
  CLOSE c;
  RETURN s;
END $f$ LANGUAGE plpgsql;
SELECT pf_cur();
CREATE FUNCTION pf_cur2(lim int) RETURNS text AS $f$ DECLARE c CURSOR (m int) FOR SELECT id FROM pf_c WHERE id <= m ORDER BY id; i int; s text := $$$$; BEGIN
  OPEN c(lim);
  FETCH c INTO i; s := s || i;
  FETCH NEXT FROM c INTO i; s := s || i;
  CLOSE c;
  FOR i IN SELECT id FROM pf_c ORDER BY id LOOP s := s || i; END LOOP;
  RETURN s;
END $f$ LANGUAGE plpgsql;
SELECT pf_cur2(3);
CREATE FUNCTION pf_cur3() RETURNS int AS $f$ DECLARE c CURSOR FOR SELECT id FROM pf_c ORDER BY id; r record; t int := 0; BEGIN
  FOR r IN c LOOP t := t + r.id; END LOOP; RETURN t;
END $f$ LANGUAGE plpgsql;
SELECT pf_cur3();
CREATE FUNCTION pf_cur4() RETURNS int AS $f$ DECLARE c refcursor; i int; BEGIN
  OPEN c FOR SELECT id FROM pf_c ORDER BY id DESC;
  FETCH c INTO i; RETURN i;
END $f$ LANGUAGE plpgsql;
SELECT pf_cur4();
CREATE FUNCTION pf_cur5() RETURNS int AS $f$ DECLARE c refcursor; i int; BEGIN
  OPEN c FOR EXECUTE $$SELECT id FROM pf_c ORDER BY id DESC$$;
  MOVE c; FETCH c INTO i; RETURN i;
END $f$ LANGUAGE plpgsql;
SELECT pf_cur5();
CREATE FUNCTION pf_cur6() RETURNS text AS $f$ DECLARE c CURSOR FOR SELECT id FROM pf_c ORDER BY id; i int; BEGIN
  CLOSE c; RETURN $$x$$;
END $f$ LANGUAGE plpgsql;
SELECT pf_cur6();
DROP TABLE pf_c;

-- ### h/pf-records
CREATE TABLE pf_r (id int PRIMARY KEY, name text, amt numeric);
INSERT INTO pf_r VALUES (1,$$a$$,1.5),(2,$$b$$,2.5);
CREATE FUNCTION pf_rec() RETURNS text AS $f$ DECLARE r record; t pf_r%ROWTYPE; x pf_r.name%TYPE; BEGIN
  SELECT * INTO r FROM pf_r WHERE id = 1;
  SELECT * INTO t FROM pf_r WHERE id = 2;
  x := t.name;
  RETURN r.id || r.name || t.amt || x;
END $f$ LANGUAGE plpgsql;
SELECT pf_rec();
CREATE FUNCTION pf_rec2() RETURNS text AS $f$ DECLARE r pf_r; BEGIN
  SELECT * INTO r FROM pf_r WHERE id = 1;
  RETURN to_jsonb(r)::text;
END $f$ LANGUAGE plpgsql;
SELECT pf_rec2();
CREATE FUNCTION pf_rec3() RETURNS text AS $f$ DECLARE r record; BEGIN
  FOR r IN SELECT * FROM pf_r ORDER BY id LOOP RAISE NOTICE $$%$$, r; END LOOP;
  RETURN $$ok$$;
END $f$ LANGUAGE plpgsql;
SELECT pf_rec3();
CREATE FUNCTION pf_rec4() RETURNS pf_r AS $f$ DECLARE r pf_r; BEGIN
  SELECT * INTO r FROM pf_r WHERE id = 2; RETURN r;
END $f$ LANGUAGE plpgsql;
SELECT * FROM pf_rec4();
CREATE FUNCTION pf_rec5() RETURNS text AS $f$ DECLARE r record; BEGIN
  r := ROW(1, $$x$$);
  RETURN r::text;
END $f$ LANGUAGE plpgsql;
SELECT pf_rec5();
CREATE FUNCTION pf_rec6() RETURNS text AS $f$ DECLARE r pf_r; BEGIN
  r.id := 5; r.name := $$q$$;
  RETURN r.id || r.name || coalesce(r.amt::text, $$null$$);
END $f$ LANGUAGE plpgsql;
SELECT pf_rec6();
CREATE FUNCTION pf_rec7() RETURNS text AS $f$ DECLARE r record; BEGIN
  SELECT * INTO STRICT r FROM pf_r;
  RETURN $$no$$;
EXCEPTION WHEN too_many_rows THEN RETURN $$many$$; WHEN no_data_found THEN RETURN $$none$$;
END $f$ LANGUAGE plpgsql;
SELECT pf_rec7();
DROP TABLE pf_r CASCADE;

-- ### h/pf-exceptions
CREATE FUNCTION pf_ex() RETURNS text AS $f$ DECLARE m text; d text; s text; c text; BEGIN
  BEGIN
    BEGIN
      RAISE EXCEPTION $$inner %$$, 1 USING ERRCODE = $$22012$$, DETAIL = $$dd$$, HINT = $$hh$$;
    EXCEPTION WHEN division_by_zero THEN
      GET STACKED DIAGNOSTICS m = MESSAGE_TEXT, d = PG_EXCEPTION_DETAIL, s = RETURNED_SQLSTATE;
      RAISE EXCEPTION $$outer(%)$$, m USING ERRCODE = $$P0001$$;
    END;
  EXCEPTION WHEN raise_exception THEN
    GET STACKED DIAGNOSTICS c = MESSAGE_TEXT;
    RETURN c || $$|$$ || SQLSTATE || $$|$$ || SQLERRM;
  END;
END $f$ LANGUAGE plpgsql;
SELECT pf_ex();
CREATE FUNCTION pf_ex2() RETURNS text AS $f$ DECLARE h text; d text; BEGIN
  RAISE EXCEPTION $$boom$$ USING DETAIL = $$the detail$$, HINT = $$the hint$$;
EXCEPTION WHEN OTHERS THEN
  GET STACKED DIAGNOSTICS h = PG_EXCEPTION_HINT, d = PG_EXCEPTION_DETAIL;
  RETURN h || $$/$$ || d;
END $f$ LANGUAGE plpgsql;
SELECT pf_ex2();
CREATE FUNCTION pf_ex3() RETURNS int AS $f$ BEGIN RAISE EXCEPTION $$custom$$ USING ERRCODE = $$22012$$, DETAIL = $$dtl$$, HINT = $$hnt$$; END $f$ LANGUAGE plpgsql;
SELECT pf_ex3();
CREATE FUNCTION pf_ex4() RETURNS int AS $f$ BEGIN RAISE unique_violation USING MESSAGE = $$mm$$; END $f$ LANGUAGE plpgsql;
SELECT pf_ex4();
CREATE FUNCTION pf_ex5() RETURNS int AS $f$ BEGIN RAISE SQLSTATE $$12345$$; END $f$ LANGUAGE plpgsql;
SELECT pf_ex5();
CREATE FUNCTION pf_ex6() RETURNS int AS $f$ BEGIN ASSERT 1 = 2, $$assert msg$$; RETURN 1; END $f$ LANGUAGE plpgsql;
SELECT pf_ex6();
CREATE FUNCTION pf_ex7() RETURNS int AS $f$ DECLARE i int; BEGIN i := 1 / 0; RETURN i; EXCEPTION WHEN division_by_zero THEN RETURN -1; END $f$ LANGUAGE plpgsql;
SELECT pf_ex7();
CREATE TABLE pf_e (id int PRIMARY KEY);
CREATE FUNCTION pf_ex8() RETURNS text AS $f$ BEGIN INSERT INTO pf_e VALUES (1); INSERT INTO pf_e VALUES (1); RETURN $$no$$; EXCEPTION WHEN unique_violation THEN RETURN $$dup$$; END $f$ LANGUAGE plpgsql;
SELECT pf_ex8();
SELECT count(*) FROM pf_e;
DROP TABLE pf_e;

-- ### h/pf-notices
DO $f$ BEGIN RAISE NOTICE $$n1$$; RAISE NOTICE $$n2 %$$, 5; RAISE WARNING $$w1$$; RAISE INFO $$i1$$; RAISE LOG $$l1$$; RAISE DEBUG $$d1$$; END $f$;
CREATE FUNCTION pf_nt() RETURNS int AS $f$ BEGIN RAISE NOTICE $$from func$$; RAISE WARNING $$warn from func$$; RETURN 1; END $f$ LANGUAGE plpgsql;
SELECT pf_nt();
SELECT pf_nt() FROM generate_series(1,2);

-- ### h/pf-dynamic
CREATE TABLE pf_x (id int, v text);
CREATE FUNCTION pf_dyn(tbl text, i int, val text) RETURNS int AS $f$ DECLARE n int; BEGIN
  EXECUTE format($$INSERT INTO %I (id, v) VALUES ($1, $2)$$, tbl) USING i, val;
  EXECUTE format($$SELECT count(*) FROM %I WHERE id >= $1$$, tbl) INTO n USING 0;
  RETURN n;
END $f$ LANGUAGE plpgsql;
SELECT pf_dyn($$pf_x$$, 1, $$a$$);
SELECT pf_dyn($$pf_x$$, 2, $$b$$);
CREATE FUNCTION pf_dyn2() RETURNS text AS $f$ DECLARE r record; s text := $$$$; BEGIN
  FOR r IN EXECUTE $$SELECT id, v FROM pf_x ORDER BY id$$ LOOP s := s || r.id || r.v; END LOOP;
  RETURN s;
END $f$ LANGUAGE plpgsql;
SELECT pf_dyn2();
CREATE FUNCTION pf_dyn3(x text) RETURNS text AS $f$ DECLARE q text; r text; BEGIN
  q := format($$SELECT %L || %s$$, x, quote_literal(x));
  EXECUTE q INTO r;
  RETURN r;
END $f$ LANGUAGE plpgsql;
SELECT pf_dyn3($$it's$$);
CREATE FUNCTION pf_dyn4() RETURNS text AS $f$ DECLARE n int; BEGIN
  EXECUTE $q$UPDATE pf_x SET v = 'z' WHERE id = 1$q$;
  GET DIAGNOSTICS n = ROW_COUNT;
  RETURN n::text;
END $f$ LANGUAGE plpgsql;
SELECT pf_dyn4();
DROP TABLE pf_x;

-- ### h/pf-write-in-select
CREATE TABLE pf_w (id serial PRIMARY KEY, msg text);
CREATE FUNCTION pf_log(m text) RETURNS int AS $f$ BEGIN INSERT INTO pf_w (msg) VALUES (m); RETURN 1; END $f$ LANGUAGE plpgsql;
BEGIN;
SELECT pf_log($$a$$);
SELECT count(*) FROM pf_w;
ROLLBACK;
SELECT count(*) FROM pf_w;
SELECT pf_log($$b$$);
SELECT count(*) FROM pf_w;
BEGIN;
INSERT INTO pf_w (msg) VALUES ($$x$$);
SELECT pf_log($$c$$) FROM generate_series(1,3);
SELECT count(*) FROM pf_w;
ROLLBACK;
SELECT count(*) FROM pf_w;
DROP TABLE pf_w;

-- ### h/pf-catalog
CREATE FUNCTION pf_c1(a int, b text DEFAULT $$x$$, OUT c int) AS $f$ BEGIN c := a; END $f$ LANGUAGE plpgsql;
CREATE FUNCTION pf_c2(a int) RETURNS TABLE(x int, y text) AS $f$ BEGIN RETURN QUERY SELECT a, $$z$$::text; END $f$ LANGUAGE plpgsql;
CREATE FUNCTION pf_c3(VARIADIC a int[]) RETURNS SETOF int AS $f$ SELECT unnest(a) $f$ LANGUAGE sql STABLE;
CREATE FUNCTION pf_c3(a text) RETURNS text AS $f$ SELECT a $f$ LANGUAGE sql IMMUTABLE STRICT;
SELECT proname, pronargs, pronargdefaults, proretset, prokind, provolatile, proisstrict, prorettype::regtype::text, proargtypes::text, proallargtypes::text, proargmodes::text, proargnames::text FROM pg_proc WHERE proname LIKE $$pf_c%$$ ORDER BY proname, pronargs, proargtypes::text;
SELECT proname, pg_get_function_arguments(oid), pg_get_function_identity_arguments(oid), pg_get_function_result(oid) FROM pg_proc WHERE proname LIKE $$pf_c%$$ ORDER BY proname, 4;
SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = $$pf_c1$$;
SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = $$pf_c2$$;
SELECT proname, oid::regproc::text FROM pg_proc WHERE proname = $$pf_c2$$;
SELECT p.proname, pg_catalog.pg_get_function_result(p.oid) AS result, pg_catalog.pg_get_function_arguments(p.oid) AS args, CASE p.prokind WHEN $$a$$ THEN $$agg$$ WHEN $$w$$ THEN $$window$$ WHEN $$p$$ THEN $$proc$$ ELSE $$func$$ END AS type FROM pg_catalog.pg_proc p LEFT JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE p.proname OPERATOR(pg_catalog.~) $$^(pf_c.*)$$ COLLATE pg_catalog.default AND pg_catalog.pg_function_is_visible(p.oid) ORDER BY 1, 2, 4;
DROP FUNCTION pf_c1(int, text);
DROP FUNCTION pf_c2(int);
DROP FUNCTION pf_c3(int[]);
DROP FUNCTION pf_c3(text);
DROP FUNCTION IF EXISTS pf_c3(text);
DROP FUNCTION pf_c3(text);

-- ### h/pf-procedures
CREATE TABLE pf_pt (id int, v text);
CREATE PROCEDURE pf_proc(a int, INOUT b text) AS $f$ BEGIN INSERT INTO pf_pt VALUES (a, b); b := b || $$!$$; END $f$ LANGUAGE plpgsql;
CALL pf_proc(1, $$x$$);
SELECT * FROM pf_pt;
CREATE PROCEDURE pf_proc2(a int) AS $f$ BEGIN INSERT INTO pf_pt VALUES (a, $$p2$$); END $f$ LANGUAGE plpgsql;
CALL pf_proc2(2);
SELECT count(*) FROM pf_pt;
DO $f$ BEGIN CALL pf_proc2(3); END $f$;
SELECT count(*) FROM pf_pt;
DROP PROCEDURE pf_proc(int, text);
DROP PROCEDURE pf_proc2(int);
DROP TABLE pf_pt;

-- ### h/pf-misc-plpgsql
CREATE FUNCTION pf_lbl() RETURNS int AS $f$ DECLARE s int := 0; BEGIN
  <<outer>> FOR i IN 1..3 LOOP
    FOR j IN 1..3 LOOP
      CONTINUE outer WHEN j = 2;
      s := s + 1;
    END LOOP;
  END LOOP;
  RETURN s;
END $f$ LANGUAGE plpgsql;
SELECT pf_lbl();
CREATE FUNCTION pf_nb() RETURNS text AS $f$ DECLARE x int := 1; BEGIN
  <<blk>> DECLARE x int := 2; BEGIN
    RETURN x::text || blk.x::text;
  END;
END $f$ LANGUAGE plpgsql;
SELECT pf_nb();
CREATE FUNCTION pf_cs(n int) RETURNS text AS $f$ BEGIN
  CASE WHEN n < 0 THEN RETURN $$neg$$; WHEN n = 0 THEN RETURN $$zero$$; ELSE RETURN $$pos$$; END CASE;
END $f$ LANGUAGE plpgsql;
SELECT pf_cs(-1), pf_cs(0), pf_cs(5);
CREATE FUNCTION pf_arr() RETURNS int AS $f$ DECLARE a int[] := ARRAY[1,2,3]; BEGIN
  a[2] := 20; RETURN a[1] + a[2] + a[3];
END $f$ LANGUAGE plpgsql;
SELECT pf_arr();
CREATE FUNCTION pf_ccm() RETURNS text AS $f$ DECLARE a CONSTANT int := 5; b int NOT NULL := 6; BEGIN RETURN (a + b)::text; END $f$ LANGUAGE plpgsql;
SELECT pf_ccm();
CREATE FUNCTION pf_ret_null() RETURNS int AS $f$ BEGIN RETURN NULL; END $f$ LANGUAGE plpgsql;
SELECT pf_ret_null() IS NULL;
CREATE FUNCTION pf_noret() RETURNS int AS $f$ BEGIN NULL; END $f$ LANGUAGE plpgsql;
SELECT pf_noret();
CREATE FUNCTION pf_strict(a int) RETURNS int AS $f$ BEGIN RETURN a + 1; END $f$ LANGUAGE plpgsql STRICT;
SELECT pf_strict(NULL), pf_strict(1);
CREATE FUNCTION pf_ws(n int) RETURNS int AS $f$ DECLARE i int := 0; BEGIN WHILE i < n LOOP i := i + 1; EXIT WHEN i > 100; END LOOP; RETURN i; END $f$ LANGUAGE plpgsql;
SELECT pf_ws(7);
CREATE FUNCTION pf_intos() RETURNS text AS $f$ DECLARE a int; b text; BEGIN SELECT 1, $$q$$ INTO a, b; RETURN a || b; END $f$ LANGUAGE plpgsql;
SELECT pf_intos();
CREATE FUNCTION pf_strict_into() RETURNS text AS $f$ DECLARE a int; BEGIN SELECT 1 INTO STRICT a WHERE false; RETURN $$x$$; EXCEPTION WHEN no_data_found THEN RETURN $$nd$$; END $f$ LANGUAGE plpgsql;
SELECT pf_strict_into();
CREATE FUNCTION pf_bad_cast(t text) RETURNS int AS $f$ BEGIN RETURN t::int; EXCEPTION WHEN invalid_text_representation THEN RETURN -1; END $f$ LANGUAGE plpgsql;
SELECT pf_bad_cast($$abc$$), pf_bad_cast($$12$$);
CREATE FUNCTION pf_num() RETURNS text AS $f$ DECLARE n numeric := 1.5; i int := 0; BEGIN i := n; RETURN i::text; END $f$ LANGUAGE plpgsql;
SELECT pf_num();
