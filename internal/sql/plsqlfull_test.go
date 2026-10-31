package sql

import (
	"strings"
	"testing"

	"stask/internal/core"
)

// Tests of the PL/pgSQL completeness work (docs/14 appendix Z). The PostgreSQL side of the same
// behaviour is checked by scripts/pgdiff/h_plsqlfull*.sql.

func plfOpen(t *testing.T, dir string) *core.Engine {
	t.Helper()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPlFullOverloadsRestart(t *testing.T) {
	dir := t.TempDir()
	e := plfOpen(t, dir)
	s := NewSession(e)
	mustq(t, s, `CREATE FUNCTION ov(a int) RETURNS text AS $f$ SELECT 'int' $f$ LANGUAGE sql`)
	mustq(t, s, `CREATE FUNCTION ov(a text) RETURNS text AS $f$ SELECT 'text' $f$ LANGUAGE sql`)
	mustq(t, s, `CREATE FUNCTION ov(a int, b int DEFAULT 5) RETURNS text AS $f$ SELECT 'int2:' || b $f$ LANGUAGE sql`)
	wantErr(t, s, `CREATE FUNCTION ov(a int) RETURNS text AS $f$ SELECT 'dup' $f$ LANGUAGE sql`, "42723")
	check := func(s *Session) {
		t.Helper()
		ddlWantRows(t, s, `SELECT ov(1, 2)`, "int2:2")
		ddlWantRows(t, s, `SELECT ov('x'::text)`, "text")
		ddlWantRows(t, s, `SELECT ov(1.5)`, "")
	}
	// ov(1.5): no overload takes numeric
	wantErr(t, s, `SELECT ov(1.5)`, "42883")
	ddlWantRows(t, s, `SELECT ov(1, 2)`, "int2:2")
	ddlWantRows(t, s, `SELECT ov('x'::text)`, "text")
	ddlWantRows(t, s, `SELECT count(*) FROM pg_proc WHERE proname = 'ov'`, "3")
	mustq(t, s, `CREATE OR REPLACE FUNCTION ov(a text) RETURNS text AS $f$ SELECT 'text2' $f$ LANGUAGE sql`)
	ddlWantRows(t, s, `SELECT ov('x'::text)`, "text2")
	_ = check
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = plfOpen(t, dir)
	defer e.Close()
	s = NewSession(e)
	ddlWantRows(t, s, `SELECT ov('x'::text)`, "text2")
	ddlWantRows(t, s, `SELECT ov(1, 2)`, "int2:2")
	ddlWantRows(t, s, `SELECT count(*) FROM pg_proc WHERE proname = 'ov'`, "3")
	mustq(t, s, `DROP FUNCTION ov(int, int)`)
	wantErr(t, s, `DROP FUNCTION ov(int, int)`, "42883")
	mustq(t, s, `DROP FUNCTION IF EXISTS ov(int, int)`)
	ddlWantRows(t, s, `SELECT count(*) FROM pg_proc WHERE proname = 'ov'`, "2")
	mustq(t, s, `DROP FUNCTION ov(text)`)
	mustq(t, s, `DROP FUNCTION ov(int)`)
	wantErr(t, s, `SELECT ov(1)`, "42883")
}

// a function stored before overloading existed (a bare funcDef) is read, replaced and dropped as before
func TestPlFullLegacyFunctionFormat(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	legacy := `{"name":"old1","args":[{"n":"a","t":"int","pg":"int4"}],"ret":"int","lang":"sql","body":"SELECT a + 1"}`
	if err := e.PutObj(core.Obj{Kind: "func", Name: "old1", Data: legacy}, true); err != nil {
		t.Fatal(err)
	}
	noteFuncTypes("old1", decodeFuncs(legacy)) // what loadUserFuncs does when an engine with such a database opens
	ddlWantRows(t, s, `SELECT old1(41)`, "42")
	mustq(t, s, `CREATE OR REPLACE FUNCTION old1(a int) RETURNS int AS $f$ SELECT a + 2 $f$ LANGUAGE sql`)
	ddlWantRows(t, s, `SELECT old1(40)`, "42")
	mustq(t, s, `DROP FUNCTION old1(int)`)
	wantErr(t, s, `SELECT old1(1)`, "42883")
}

func TestPlFullParamsAndNamedArgs(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE FUNCTION mm(a int, b int, OUT lo int, OUT hi int) AS $f$ BEGIN lo := least(a,b); hi := greatest(a,b); END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT * FROM mm(5, 3)`, "3,5")
	ddlWantRows(t, s, `SELECT hi FROM mm(1, 9)`, "9")
	ddlWantRows(t, s, `SELECT mm(7, 2)`, "(2,7)")
	mustq(t, s, `CREATE FUNCTION df(a int, b int DEFAULT 10, c text DEFAULT 'z') RETURNS text AS $f$ BEGIN RETURN a || '-' || b || '-' || c; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT df(1)`, "1-10-z")
	ddlWantRows(t, s, `SELECT df(1, 2, 'q')`, "1-2-q")
	ddlWantRows(t, s, `SELECT df(a => 5, c => 'k')`, "5-10-k")
	ddlWantRows(t, s, `SELECT df(c := 'k', a := 6)`, "6-10-k")
	wantErr(t, s, `SELECT df()`, "42883")
	mustq(t, s, `CREATE FUNCTION va(VARIADIC xs int[]) RETURNS int AS $f$ SELECT sum(x)::int FROM unnest(xs) x $f$ LANGUAGE sql`)
	ddlWantRows(t, s, `SELECT va(1, 2, 3)`, "6")
	// a function returning integer reports int4
	mustq(t, s, `CREATE FUNCTION fi() RETURNS integer AS $f$ BEGIN RETURN 5; END $f$ LANGUAGE plpgsql`)
	if oid := mustq(t, s, `SELECT fi()`).Cols[0].OID; oid != 23 {
		t.Fatalf("integer function column OID %d, want 23", oid)
	}
	// the PostgreSQL type text shows in the catalog
	ddlWantRows(t, s, `SELECT pg_get_function_arguments(oid) FROM pg_proc WHERE proname = 'df'`, "a integer, b integer DEFAULT 10, c text DEFAULT 'z'::text")
	ddlWantRows(t, s, `SELECT pg_get_function_identity_arguments(oid) FROM pg_proc WHERE proname = 'df'`, "a integer, b integer, c text")
	ddlWantRows(t, s, `SELECT pg_get_function_result(oid) FROM pg_proc WHERE proname = 'mm'`, "record")
	ddlWantRows(t, s, `SELECT prorettype::regtype::text FROM pg_proc WHERE proname = 'fi'`, "integer")
	ddlWantRows(t, s, `SELECT pronamespace::regnamespace::text FROM pg_proc WHERE proname = 'fi'`, "public")
}

func TestPlFullSetReturning(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE sr (id int PRIMARY KEY, name text)`)
	mustq(t, s, `INSERT INTO sr VALUES (1,'a'),(2,'b'),(3,'c')`)
	mustq(t, s, `CREATE FUNCTION srq() RETURNS TABLE(id int, name text) AS $f$ BEGIN RETURN QUERY SELECT t.id, t.name FROM sr t ORDER BY t.id; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT * FROM srq()`, "1,a;2,b;3,c")
	ddlWantRows(t, s, `SELECT name FROM srq() WHERE id > 1`, "b;c")
	mustq(t, s, `CREATE FUNCTION srn(n int) RETURNS SETOF int AS $f$ BEGIN FOR i IN 1..n LOOP RETURN NEXT i * i; END LOOP; RETURN; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT * FROM srn(4)`, "1;4;9;16")
	ddlWantRows(t, s, `SELECT sum(x) FROM srn(4) x`, "30")
	ddlWantRows(t, s, `SELECT srn(3)`, "1;4;9")
	ddlWantRows(t, s, `SELECT * FROM srn(2) WITH ORDINALITY`, "1,1;4,2")
	ddlWantRows(t, s, `SELECT t.id FROM sr t, srn(2) r WHERE t.id = r`, "1")
	mustq(t, s, `CREATE FUNCTION srt() RETURNS SETOF sr AS $f$ SELECT * FROM sr ORDER BY id $f$ LANGUAGE sql`)
	ddlWantRows(t, s, `SELECT * FROM srt()`, "1,a;2,b;3,c")
	mustq(t, s, `CREATE FUNCTION srd(tbl text) RETURNS SETOF int AS $f$ BEGIN RETURN QUERY EXECUTE format('SELECT id FROM %I ORDER BY id DESC', tbl); END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT * FROM srd('sr')`, "3;2;1")
}

func TestPlFullCursorsAndLoops(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE cu (id int, n text)`)
	mustq(t, s, `INSERT INTO cu VALUES (1,'a'),(2,'b'),(3,'c')`)
	mustq(t, s, `CREATE FUNCTION cu1() RETURNS text AS $f$ DECLARE c CURSOR FOR SELECT id, n FROM cu ORDER BY id; r record; s text := ''; BEGIN
	  OPEN c; LOOP FETCH c INTO r; EXIT WHEN NOT FOUND; s := s || r.id || r.n; END LOOP; CLOSE c; RETURN s; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT cu1()`, "1a2b3c")
	mustq(t, s, `CREATE FUNCTION cu2(lim int) RETURNS text AS $f$ DECLARE c CURSOR (m int) FOR SELECT id FROM cu WHERE id <= m ORDER BY id; i int; s text := ''; BEGIN
	  OPEN c(lim); FETCH c INTO i; s := s || i; FETCH NEXT FROM c INTO i; s := s || i; CLOSE c;
	  FOR i IN SELECT id FROM cu ORDER BY id LOOP s := s || i; END LOOP; RETURN s; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT cu2(3)`, "12123")
	mustq(t, s, `CREATE FUNCTION cu3() RETURNS int AS $f$ DECLARE c CURSOR FOR SELECT id FROM cu ORDER BY id; r record; t int := 0; BEGIN
	  FOR r IN c LOOP t := t + r.id; END LOOP; RETURN t; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT cu3()`, "6")
	mustq(t, s, `CREATE FUNCTION cu4() RETURNS int AS $f$ DECLARE c refcursor; i int; BEGIN
	  OPEN c FOR SELECT id FROM cu ORDER BY id DESC; FETCH c INTO i; RETURN i; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT cu4()`, "3")
	mustq(t, s, `CREATE FUNCTION cu5() RETURNS text AS $f$ DECLARE c CURSOR FOR SELECT id FROM cu; BEGIN CLOSE c; RETURN 'x'; END $f$ LANGUAGE plpgsql`)
	wantErr(t, s, `SELECT cu5()`, "22004")
	// a refcursor returned to the caller is fetched with SQL in the same transaction
	mustq(t, s, `CREATE FUNCTION cu6() RETURNS refcursor AS $f$ DECLARE c refcursor := 'mycur'; BEGIN OPEN c FOR SELECT id FROM cu ORDER BY id; RETURN c; END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `BEGIN`)
	ddlWantRows(t, s, `SELECT cu6()`, "mycur")
	ddlWantRows(t, s, `FETCH 2 FROM mycur`, "1;2")
	mustq(t, s, `ROLLBACK`)
	// FOREACH
	mustq(t, s, `CREATE FUNCTION fe(a int[]) RETURNS int AS $f$ DECLARE s int := 0; x int; BEGIN FOREACH x IN ARRAY a LOOP s := s + x; END LOOP; RETURN s; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT fe(ARRAY[1,2,3,4])`, "10")
	ddlWantRows(t, s, `SELECT fe(ARRAY[]::int[])`, "0")
	// labels and CONTINUE outer, qualified variables
	mustq(t, s, `CREATE FUNCTION lb() RETURNS int AS $f$ DECLARE s int := 0; BEGIN
	  <<outer>> FOR i IN 1..3 LOOP FOR j IN 1..3 LOOP CONTINUE outer WHEN j = 2; s := s + 1; END LOOP; END LOOP; RETURN s; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT lb()`, "3")
	mustq(t, s, `CREATE FUNCTION ar() RETURNS int AS $f$ DECLARE a int[] := ARRAY[1,2,3]; BEGIN a[2] := 20; RETURN a[1] + a[2] + a[3]; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT ar()`, "24")
}

func TestPlFullDiagnosticsAndExceptions(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE dg (id int PRIMARY KEY, v int)`)
	mustq(t, s, `CREATE FUNCTION gd() RETURNS text AS $f$ DECLARE n int; m int; f boolean; BEGIN
	  INSERT INTO dg SELECT g, g FROM generate_series(1,5) g; GET DIAGNOSTICS n = ROW_COUNT;
	  UPDATE dg SET v = v + 1 WHERE id > 2; GET DIAGNOSTICS m = ROW_COUNT;
	  UPDATE dg SET v = 0 WHERE id > 100; f := FOUND;
	  RETURN n || '/' || m || '/' || f; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT gd()`, "5/3/false")
	mustq(t, s, `CREATE FUNCTION se() RETURNS text AS $f$ DECLARE m text; d text; h text; st text; BEGIN
	  RAISE EXCEPTION 'boom %', 1 USING ERRCODE = '22012', DETAIL = 'the detail', HINT = 'the hint';
	  EXCEPTION WHEN division_by_zero THEN
	    GET STACKED DIAGNOSTICS m = MESSAGE_TEXT, d = PG_EXCEPTION_DETAIL, h = PG_EXCEPTION_HINT, st = RETURNED_SQLSTATE;
	    RETURN m || '|' || d || '|' || h || '|' || st || '|' || SQLSTATE; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT se()`, "boom 1|the detail|the hint|22012|22012")
	// the error leaves the function with its detail and hint
	mustq(t, s, `CREATE FUNCTION re() RETURNS int AS $f$ BEGIN RAISE EXCEPTION 'custom' USING ERRCODE = '22012', DETAIL = 'dtl', HINT = 'hnt'; END $f$ LANGUAGE plpgsql`)
	_, err := qs(t, s, `SELECT re()`)
	e := AsError(err)
	if e.Code != "22012" || e.Msg != "custom" || e.Detail != "dtl" || e.Hint != "hnt" {
		t.Fatalf("error = %+v", e)
	}
	mustq(t, s, `CREATE FUNCTION as1() RETURNS int AS $f$ BEGIN ASSERT 1 = 2, 'assert msg'; RETURN 1; END $f$ LANGUAGE plpgsql`)
	wantErr(t, s, `SELECT as1()`, "P0004")
	mustq(t, s, `CREATE FUNCTION nr() RETURNS int AS $f$ BEGIN NULL; END $f$ LANGUAGE plpgsql`)
	wantErr(t, s, `SELECT nr()`, "2F005")
	// STRICT INTO
	mustq(t, s, `CREATE FUNCTION si() RETURNS text AS $f$ DECLARE a int; BEGIN SELECT id INTO STRICT a FROM dg; RETURN 'x'; EXCEPTION WHEN too_many_rows THEN RETURN 'many'; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT si()`, "many")
}

func TestPlFullNoticesQueued(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `DO $f$ BEGIN RAISE NOTICE 'n1'; RAISE NOTICE 'n2 %', 5; RAISE WARNING 'w1' USING DETAIL = 'dd', HINT = 'hh'; RAISE INFO 'i1'; RAISE LOG 'l1'; RAISE DEBUG 'd1'; END $f$`)
	got := s.TakeNotices()
	var lines []string
	for _, n := range got {
		lines = append(lines, n.Sev+":"+n.Msg+":"+n.Detail+":"+n.Hint)
	}
	want := "NOTICE:n1::|NOTICE:n2 5::|WARNING:w1:dd:hh|INFO:i1::"
	if strings.Join(lines, "|") != want {
		t.Fatalf("notices %q, want %q", strings.Join(lines, "|"), want)
	}
	if len(s.TakeNotices()) != 0 {
		t.Fatal("TakeNotices does not forget")
	}
	mustq(t, s, `CREATE FUNCTION nt() RETURNS int AS $f$ BEGIN RAISE NOTICE 'from func'; RETURN 1; END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `SELECT nt() FROM generate_series(1, 2)`)
	if n := s.TakeNotices(); len(n) != 2 || n[0].Msg != "from func" {
		t.Fatalf("notices of a function called per row: %+v", n)
	}
	// an error after a notice keeps the notice for the wire layer
	_, _ = qs(t, s, `DO $f$ BEGIN RAISE NOTICE 'before'; RAISE EXCEPTION 'after'; END $f$`)
	if n := s.TakeNotices(); len(n) != 1 || n[0].Msg != "before" {
		t.Fatalf("notice before an error: %+v", n)
	}
}

func TestPlFullRecords(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE rc (id int PRIMARY KEY, name text, amt numeric)`)
	mustq(t, s, `INSERT INTO rc VALUES (1,'a',1.5),(2,'b',2.5)`)
	mustq(t, s, `CREATE FUNCTION rc1() RETURNS text AS $f$ DECLARE r record; t rc%ROWTYPE; x rc.name%TYPE; BEGIN
	  SELECT * INTO r FROM rc WHERE id = 1; SELECT * INTO t FROM rc WHERE id = 2; x := t.name;
	  RETURN r.id || r.name || t.amt || x; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT rc1()`, "1a2.5b")
	mustq(t, s, `CREATE FUNCTION rc2() RETURNS text AS $f$ DECLARE r rc; BEGIN SELECT * INTO r FROM rc WHERE id = 1; RETURN to_jsonb(r)::text; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT rc2()`, `{"id": 1, "amt": 1.5, "name": "a"}`)
	mustq(t, s, `CREATE FUNCTION rc3() RETURNS rc AS $f$ DECLARE r rc; BEGIN SELECT * INTO r FROM rc WHERE id = 2; RETURN r; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT * FROM rc3()`, "2,b,2.5")
	mustq(t, s, `CREATE FUNCTION rc4() RETURNS text AS $f$ DECLARE r record; BEGIN r := ROW(1, 'x'); RETURN r::text; END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT rc4()`, "(1,x)")
	mustq(t, s, `CREATE FUNCTION rc5() RETURNS text AS $f$ DECLARE r rc; BEGIN r.id := 5; r.name := 'q'; RETURN r.id || r.name || coalesce(r.amt::text, 'null'); END $f$ LANGUAGE plpgsql`)
	ddlWantRows(t, s, `SELECT rc5()`, "5qnull")
	// a row alias on its own is its row
	ddlWantRows(t, s, `SELECT to_jsonb(t) FROM rc t WHERE id = 1`, `{"id": 1, "amt": 1.5, "name": "a"}`)
	ddlWantRows(t, s, `SELECT jsonb_agg(t ORDER BY id) FROM rc t`, `[{"id": 1, "amt": 1.5, "name": "a"}, {"id": 2, "amt": 2.5, "name": "b"}]`)
	ddlWantRows(t, s, `SELECT t::text FROM rc t WHERE id = 2`, "(2,b,2.5)")
}

func TestPlFullWritesInCallerTransaction(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE wl (id serial PRIMARY KEY, msg text)`)
	mustq(t, s, `CREATE FUNCTION wlog(m text) RETURNS int AS $f$ BEGIN INSERT INTO wl (msg) VALUES (m); RETURN 1; END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `CREATE FUNCTION wfail(m text) RETURNS int AS $f$ BEGIN INSERT INTO wl (msg) VALUES (m); RAISE EXCEPTION 'no'; END $f$ LANGUAGE plpgsql`)
	// autocommit: the writes of the functions called by one statement commit or roll back with it
	mustq(t, s, `SELECT wlog('a') FROM generate_series(1, 3)`)
	ddlWantRows(t, s, `SELECT count(*) FROM wl`, "3")
	wantErr(t, s, `SELECT wlog('b'), wfail('c')`, "P0001")
	ddlWantRows(t, s, `SELECT count(*) FROM wl`, "3")
	wantErr(t, s, `SELECT wlog('b') FROM generate_series(1, 2) UNION ALL SELECT wfail('c')`, "P0001")
	ddlWantRows(t, s, `SELECT count(*) FROM wl`, "3")
	// explicit transaction
	mustq(t, s, `BEGIN`)
	mustq(t, s, `SELECT wlog('x')`)
	ddlWantRows(t, s, `SELECT count(*) FROM wl`, "4")
	mustq(t, s, `ROLLBACK`)
	ddlWantRows(t, s, `SELECT count(*) FROM wl`, "3")
	if s.explicit || s.fnTx {
		t.Fatal("a transaction is left open")
	}
}

func TestPlFullProcedureOut(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE po (id int, v text)`)
	mustq(t, s, `CREATE PROCEDURE pp(a int, INOUT b text) AS $f$ BEGIN INSERT INTO po VALUES (a, b); b := b || '!'; END $f$ LANGUAGE plpgsql`)
	r := mustq(t, s, `CALL pp(1, 'x')`)
	if r.Tag != "CALL" || len(r.Rows) != 1 || r.Rows[0][0].String() != "x!" || r.Cols[0].Name != "b" {
		t.Fatalf("CALL with INOUT: %+v", r)
	}
	ddlWantRows(t, s, `SELECT * FROM po`, "1,x")
	mustq(t, s, `CREATE PROCEDURE p0(a int) AS $f$ BEGIN INSERT INTO po VALUES (a, 'p0'); END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `DO $f$ BEGIN CALL p0(3); END $f$`)
	ddlWantRows(t, s, `SELECT count(*) FROM po`, "2")
}

func TestPlFullViewInsteadOfAndTransition(t *testing.T) {
	dir := t.TempDir()
	e := plfOpen(t, dir)
	s := NewSession(e)
	mustq(t, s, `CREATE TABLE ib (id int PRIMARY KEY, name text, qty int)`)
	mustq(t, s, `INSERT INTO ib VALUES (1,'a',10),(2,'b',20)`)
	mustq(t, s, `CREATE VIEW iv AS SELECT id, upper(name) AS uname, qty * 2 AS dbl FROM ib`)
	mustq(t, s, `CREATE FUNCTION ivt() RETURNS trigger AS $f$ BEGIN
	  IF TG_OP = 'INSERT' THEN INSERT INTO ib VALUES (NEW.id, lower(NEW.uname), NEW.dbl / 2); RETURN NEW;
	  ELSIF TG_OP = 'UPDATE' THEN UPDATE ib SET name = lower(NEW.uname), qty = NEW.dbl / 2 WHERE id = OLD.id; RETURN NEW;
	  ELSE DELETE FROM ib WHERE id = OLD.id; RETURN OLD; END IF; END $f$ LANGUAGE plpgsql`)
	wantErr(t, s, `CREATE TRIGGER x INSTEAD OF INSERT ON ib FOR EACH ROW EXECUTE FUNCTION ivt()`, "42809")
	wantErr(t, s, `CREATE TRIGGER x AFTER INSERT ON iv FOR EACH ROW EXECUTE FUNCTION ivt()`, "42809")
	mustq(t, s, `CREATE TRIGGER ivt_i INSTEAD OF INSERT OR UPDATE OR DELETE ON iv FOR EACH ROW EXECUTE FUNCTION ivt()`)
	if tag := mustq(t, s, `INSERT INTO iv VALUES (3, 'CCC', 60)`).Tag; tag != "INSERT 0 1" {
		t.Fatalf("tag %s", tag)
	}
	ddlWantRows(t, s, `SELECT * FROM ib ORDER BY id`, "1,a,10;2,b,20;3,ccc,30")
	if tag := mustq(t, s, `UPDATE iv SET dbl = 100 WHERE id = 1`).Tag; tag != "UPDATE 1" {
		t.Fatalf("tag %s", tag)
	}
	ddlWantRows(t, s, `SELECT * FROM ib WHERE id = 1`, "1,a,50")
	if tag := mustq(t, s, `DELETE FROM iv WHERE id >= 2`).Tag; tag != "DELETE 2" {
		t.Fatalf("tag %s", tag)
	}
	ddlWantRows(t, s, `SELECT id FROM ib`, "1")
	// transition tables
	mustq(t, s, `CREATE TABLE tt (id int PRIMARY KEY, v int)`)
	mustq(t, s, `CREATE TABLE tlog (op text, n int, s int)`)
	mustq(t, s, `CREATE FUNCTION tti() RETURNS trigger AS $f$ BEGIN INSERT INTO tlog SELECT TG_OP, count(*), sum(v) FROM newrows; RETURN NULL; END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `CREATE FUNCTION ttu() RETURNS trigger AS $f$ BEGIN INSERT INTO tlog SELECT TG_OP, count(*), sum(n.v - o.v) FROM newrows n JOIN oldrows o ON o.id = n.id; RETURN NULL; END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `CREATE TRIGGER tti AFTER INSERT ON tt REFERENCING NEW TABLE AS newrows FOR EACH STATEMENT EXECUTE FUNCTION tti()`)
	mustq(t, s, `CREATE TRIGGER ttu AFTER UPDATE ON tt REFERENCING OLD TABLE AS oldrows NEW TABLE AS newrows FOR EACH STATEMENT EXECUTE FUNCTION ttu()`)
	wantErr(t, s, `CREATE TRIGGER bad BEFORE INSERT ON tt REFERENCING NEW TABLE AS x FOR EACH STATEMENT EXECUTE FUNCTION tti()`, "42P17")
	wantErr(t, s, `CREATE TRIGGER bad AFTER INSERT ON tt REFERENCING NEW TABLE AS x FOR EACH ROW EXECUTE FUNCTION tti()`, "0A000")
	mustq(t, s, `INSERT INTO tt VALUES (1, 10), (2, 20), (3, 30)`)
	mustq(t, s, `UPDATE tt SET v = v + 1 WHERE id < 3`)
	ddlWantRows(t, s, `SELECT * FROM tlog ORDER BY op`, "INSERT,3,60;UPDATE,2,2")
	// TRUNCATE triggers
	mustq(t, s, `CREATE TABLE tr (id int)`)
	mustq(t, s, `CREATE TABLE trl (msg text)`)
	mustq(t, s, `INSERT INTO tr VALUES (1)`)
	mustq(t, s, `CREATE FUNCTION trf() RETURNS trigger AS $f$ BEGIN INSERT INTO trl VALUES (TG_WHEN || ' ' || TG_OP || ' ' || TG_LEVEL); RETURN NULL; END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `CREATE TRIGGER trb BEFORE TRUNCATE ON tr FOR EACH STATEMENT EXECUTE FUNCTION trf()`)
	mustq(t, s, `CREATE TRIGGER tra AFTER TRUNCATE ON tr FOR EACH STATEMENT EXECUTE FUNCTION trf()`)
	mustq(t, s, `TRUNCATE tr`)
	ddlWantRows(t, s, `SELECT count(*) FROM tr`, "0")
	ddlWantRows(t, s, `SELECT msg FROM trl ORDER BY msg`, "AFTER TRUNCATE STATEMENT;BEFORE TRUNCATE STATEMENT")
	ddlWantRows(t, s, `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname = 'ttu'`,
		"CREATE TRIGGER ttu AFTER UPDATE ON public.tt REFERENCING OLD TABLE AS oldrows NEW TABLE AS newrows FOR EACH STATEMENT EXECUTE FUNCTION ttu()")
	ddlWantRows(t, s, `SELECT tgtype FROM pg_trigger WHERE tgname = 'ivt_i'`, "93")
	// everything above survives a restart (trigger definitions carry the new fields)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = plfOpen(t, dir)
	defer e.Close()
	s = NewSession(e)
	mustq(t, s, `INSERT INTO tt VALUES (4, 40)`)
	ddlWantRows(t, s, `SELECT count(*) FROM tlog WHERE op = 'INSERT'`, "2")
	mustq(t, s, `INSERT INTO iv VALUES (9, 'Z', 8)`)
	ddlWantRows(t, s, `SELECT id, name, qty FROM ib WHERE id = 9`, "9,z,4")
	mustq(t, s, `TRUNCATE tr`)
	ddlWantRows(t, s, `SELECT count(*) FROM trl`, "4")
	// DROP VIEW takes its triggers along
	mustq(t, s, `DROP VIEW iv`)
	ddlWantRows(t, s, `SELECT count(*) FROM pg_trigger WHERE tgname = 'ivt_i'`, "0")
}

func TestPlFullTriggerRecordVariables(t *testing.T) {
	s := NewSession(newEngine(t))
	mustq(t, s, `CREATE TABLE tm (id int PRIMARY KEY, name text, amt numeric)`)
	mustq(t, s, `CREATE TABLE ta (id serial PRIMARY KEY, op text, row_data jsonb, old_data jsonb)`)
	mustq(t, s, `CREATE FUNCTION tmf() RETURNS trigger AS $f$ BEGIN
	  IF TG_OP = 'DELETE' THEN INSERT INTO ta (op, old_data) VALUES (TG_OP, to_jsonb(OLD)); RETURN OLD;
	  ELSIF TG_OP = 'UPDATE' THEN INSERT INTO ta (op, row_data, old_data) VALUES (TG_OP, to_jsonb(NEW), to_jsonb(OLD)); RETURN NEW; END IF;
	  INSERT INTO ta (op, row_data) SELECT TG_OP, to_jsonb(t) FROM (SELECT NEW.*) t; RETURN NEW; END $f$ LANGUAGE plpgsql`)
	mustq(t, s, `CREATE TRIGGER tmt AFTER INSERT OR UPDATE OR DELETE ON tm FOR EACH ROW EXECUTE FUNCTION tmf()`)
	mustq(t, s, `INSERT INTO tm VALUES (1,'a',1.5)`)
	mustq(t, s, `UPDATE tm SET name = 'aa'`)
	mustq(t, s, `DELETE FROM tm`)
	ddlWantRows(t, s, `SELECT op, row_data, old_data FROM ta ORDER BY id`,
		`INSERT,{"id": 1, "amt": 1.5, "name": "a"},NULL;UPDATE,{"id": 1, "amt": 1.5, "name": "aa"},{"id": 1, "amt": 1.5, "name": "a"};DELETE,NULL,{"id": 1, "amt": 1.5, "name": "aa"}`)
}
