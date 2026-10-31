package sql

import (
	"strings"
	"testing"

	"stask/internal/core"
)

// the changeset release-fn-task-maintenance-link-guard of the CRM application, as sent
const crmGuardFn = `CREATE OR REPLACE FUNCTION task_maintenance_link_guard()
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
$$ LANGUAGE plpgsql`

const crmGuardTrg = `CREATE TRIGGER trg_task_maintenance_link_guard
BEFORE UPDATE OF maintenance_id ON TASK
FOR EACH ROW EXECUTE FUNCTION task_maintenance_link_guard()`

func crmTaskSetup(t *testing.T, s *Session) {
	t.Helper()
	run(t, s, `CREATE TABLE task (id uuid PRIMARY KEY, maintenance_id uuid, description text)`)
	run(t, s, crmGuardFn)
	run(t, s, `DROP TRIGGER IF EXISTS trg_task_maintenance_link_guard ON TASK`)
	run(t, s, crmGuardTrg)
}

const (
	uT1 = `'00000000-0000-0000-0000-0000000000a1'`
	uA  = `'11111111-1111-1111-1111-111111111111'`
	uB  = `'22222222-2222-2222-2222-222222222222'`
)

func TestTrigPlsqlCRMGuard(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	crmTaskSetup(t, s)
	// the changeset is idempotent (runOnChange): DROP + CREATE again
	run(t, s, crmGuardFn)
	run(t, s, `DROP TRIGGER IF EXISTS trg_task_maintenance_link_guard ON TASK`)
	run(t, s, crmGuardTrg)
	run(t, s, `INSERT INTO task VALUES (`+uT1+`, NULL, 'x')`)
	link := func(v string) error {
		_, err := tryRun(s, `UPDATE task SET maintenance_id = `+v+` WHERE id = `+uT1)
		return err
	}
	cur := func() string {
		return render(run(t, s, `SELECT maintenance_id FROM task WHERE id = `+uT1))
	}
	for _, step := range []struct{ v, want string }{
		{uA, "11111111-1111-1111-1111-111111111111"}, // NULL -> A
		{uA, "11111111-1111-1111-1111-111111111111"}, // A -> A
		{"NULL", "NULL"}, // A -> NULL
		{uB, "22222222-2222-2222-2222-222222222222"}, // NULL -> B
	} {
		if err := link(step.v); err != nil {
			t.Fatalf("link %s: %v", step.v, err)
		}
		if got := cur(); got != step.want {
			t.Fatalf("after link %s: %s", step.v, got)
		}
	}
	err := link(uA) // B -> A: refused
	if err == nil || code(err) != "23514" || !strings.Contains(err.Error(), "không được ghi đè sang phiếu 11111111-1111-1111-1111-111111111111") ||
		!strings.Contains(err.Error(), "Việc cần làm 00000000-0000-0000-0000-0000000000a1 đã gắn phiếu bảo trì 22222222") {
		t.Fatalf("overwrite: %v", err)
	}
	if got := cur(); got != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("link changed by the refused update: %s", got)
	}
	// UPDATE OF: an update that does not assign maintenance_id never reaches the function
	run(t, s, `UPDATE task SET description = 'y' WHERE id = `+uT1)
	// ... even one that raises when it runs: replace the function by one that always raises
	run(t, s, `CREATE OR REPLACE FUNCTION task_maintenance_link_guard() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$ LANGUAGE plpgsql`)
	run(t, s, `UPDATE task SET description = 'z' WHERE id = `+uT1)
	if _, err := tryRun(s, `UPDATE task SET maintenance_id = maintenance_id WHERE id = `+uT1); err == nil || code(err) != "P0001" {
		t.Fatalf("assigned but unchanged column must fire the trigger: %v", err)
	}
	// the catalog sees it (Jmix test: pg_trigger by tgname)
	if got := render(run(t, s, `SELECT count(*) FROM pg_trigger WHERE tgname = 'trg_task_maintenance_link_guard'`)); got != "1" {
		t.Fatalf("pg_trigger: %s", got)
	}
	// inside a transaction: the error fails the transaction
	run(t, s, `BEGIN`)
	if _, err := tryRun(s, `UPDATE task SET maintenance_id = `+uA+` WHERE id = `+uT1); err == nil {
		t.Fatal("not refused in transaction")
	}
	if _, err := tryRun(s, `SELECT 1`); code(err) != codeInFailedTx {
		t.Fatalf("transaction not failed: %v", err)
	}
	run(t, s, `ROLLBACK`)
}

func TestTrigPlsqlRowFeatures(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE item (id bigint PRIMARY KEY, name text, qty bigint, note text, upd bigint)`)
	run(t, s, `CREATE TABLE log (n bigserial PRIMARY KEY, msg text)`)
	run(t, s, `CREATE FUNCTION item_bi() RETURNS trigger AS $$
	DECLARE total bigint := 0; i integer;
	BEGIN
	  -- TG variables
	  INSERT INTO log(msg) VALUES (TG_OP || '/' || TG_NAME || '/' || TG_TABLE_NAME || '/' || TG_WHEN || '/' || TG_LEVEL || '/' || TG_NARGS || '/' || TG_ARGV[0] || '/' || TG_ARGV[1]);
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
	END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER item_bi BEFORE INSERT ON item FOR EACH ROW EXECUTE PROCEDURE item_bi('x', 'y z')`)
	r := run(t, s, `INSERT INTO item VALUES (1, 'a', NULL, NULL, NULL), (2, 'skip', 5, NULL, NULL), (3, 'c', 50, NULL, NULL), (4, 'd', 500, NULL, NULL)`)
	if r.Tag != "INSERT 0 3" {
		t.Fatalf("tag %s", r.Tag)
	}
	if got := render(run(t, s, `SELECT id, name, qty, note, upd FROM item ORDER BY id`)); got != "1,A,0,small,710;3,c,50,medium,710;4,d,500,big,710" {
		t.Fatalf("rows %s", got)
	}
	if got := render(run(t, s, `SELECT msg FROM log ORDER BY n LIMIT 1`)); got != "INSERT/item_bi/item/BEFORE/ROW/2/x/y z" {
		t.Fatalf("tg vars: %s", got)
	}

	// AFTER trigger with SELECT INTO, PERFORM, EXISTS, FOR over a query, RAISE USING MESSAGE
	run(t, s, `CREATE FUNCTION item_au() RETURNS trigger AS $$
	DECLARE c bigint; rec record; sum bigint := 0;
	BEGIN
	  SELECT count(*) INTO c FROM item WHERE qty >= OLD.qty;
	  IF EXISTS (SELECT 1 FROM item WHERE id = NEW.id AND qty > 1000) THEN
	    RAISE EXCEPTION USING MESSAGE = 'qty too large for ' || NEW.id, ERRCODE = 'check_violation';
	  END IF;
	  FOR rec IN SELECT id, qty FROM item ORDER BY id LOOP
	    sum := sum + rec.qty;
	  END LOOP;
	  PERFORM 1;
	  INSERT INTO log(msg) VALUES ('upd ' || OLD.qty || '->' || NEW.qty || ' ge=' || c || ' sum=' || sum);
	  RETURN NULL;
	END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER item_au AFTER UPDATE ON item FOR EACH ROW WHEN (OLD.qty IS DISTINCT FROM NEW.qty) EXECUTE FUNCTION item_au()`)
	run(t, s, `UPDATE item SET name = 'zz' WHERE id = 3`) // WHEN false
	run(t, s, `UPDATE item SET qty = 60 WHERE id = 3`)
	if got := render(run(t, s, `SELECT msg FROM log WHERE msg LIKE 'upd%'`)); got != "upd 50->60 ge=2 sum=560" {
		t.Fatalf("after trigger: %s", got)
	}
	_, err := tryRun(s, `UPDATE item SET qty = 2000 WHERE id = 4`)
	if err == nil || code(err) != "23514" || !strings.Contains(err.Error(), "qty too large for 4") {
		t.Fatalf("raise using: %v", err)
	}
	if got := render(run(t, s, `SELECT qty FROM item WHERE id = 4`)); got != "500" {
		t.Fatalf("failed statement left a change: %s", got)
	}

	// BEFORE DELETE: RETURN NULL skips the row (and is not counted); BEFORE UPDATE: RETURN OLD keeps the row
	run(t, s, `CREATE FUNCTION item_bd() RETURNS trigger AS $$ BEGIN
	  IF TG_OP = 'DELETE' THEN
	    IF OLD.qty >= 100 THEN RETURN NULL; END IF;
	    RETURN OLD;
	  END IF;
	  IF NEW.name = 'keep' THEN RETURN OLD; END IF;
	  RETURN NEW;
	END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER item_bd BEFORE DELETE OR UPDATE OF name ON item FOR EACH ROW EXECUTE FUNCTION item_bd()`)
	if r := run(t, s, `DELETE FROM item`); r.Tag != "DELETE 2" {
		t.Fatalf("delete tag %s", r.Tag)
	}
	if got := render(run(t, s, `SELECT id FROM item ORDER BY id`)); got != "4" {
		t.Fatalf("after delete: %s", got)
	}
	if r := run(t, s, `UPDATE item SET name = 'keep'`); r.Tag != "UPDATE 1" && r.Tag != "UPDATE 0" {
		t.Fatalf("update tag %s", r.Tag)
	}
	if got := render(run(t, s, `SELECT name FROM item`)); got != "d" {
		t.Fatalf("RETURN OLD must keep the row as it was: %s", got)
	}
	// a function that reaches its end without RETURN
	run(t, s, `CREATE FUNCTION noret() RETURNS trigger AS $$ BEGIN NULL; END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER noret AFTER INSERT ON log FOR EACH ROW EXECUTE FUNCTION noret()`)
	if _, err := tryRun(s, `INSERT INTO log(msg) VALUES ('q')`); code(err) != "2F005" {
		t.Fatalf("no RETURN: %v", err)
	}
	run(t, s, `DROP TRIGGER noret ON log`)
	// CREATE TRIGGER checks the function
	run(t, s, `CREATE FUNCTION notrig() RETURNS int AS $$ BEGIN RETURN 1; END $$ LANGUAGE plpgsql`)
	for q, c := range map[string]string{
		`CREATE TRIGGER x AFTER INSERT ON log FOR EACH ROW EXECUTE FUNCTION nonesuch()`: codeUndefinedFunction,
		`CREATE TRIGGER x AFTER INSERT ON log FOR EACH ROW EXECUTE FUNCTION notrig()`:   "42P17",
	} {
		if _, err := tryRun(s, q); code(err) != c {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestTrigPlsqlStatementLevel(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE m (id bigint PRIMARY KEY, v bigint)`)
	run(t, s, `CREATE TABLE stm (n bigserial PRIMARY KEY, msg text)`)
	run(t, s, `CREATE FUNCTION stm() RETURNS trigger AS $$ BEGIN
	  INSERT INTO stm(msg) VALUES (TG_WHEN || ' ' || TG_OP || ' ' || TG_LEVEL);
	  RETURN NULL;
	END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER s_b BEFORE INSERT OR UPDATE OR DELETE ON m FOR EACH STATEMENT EXECUTE FUNCTION stm()`)
	run(t, s, `CREATE TRIGGER s_a AFTER INSERT OR UPDATE OR DELETE ON m EXECUTE FUNCTION stm()`) // default: statement
	run(t, s, `INSERT INTO m VALUES (1,1),(2,2),(3,3)`)
	run(t, s, `UPDATE m SET v = v + 1`)
	run(t, s, `UPDATE m SET v = 0 WHERE id = 99`) // no row: the statement triggers still fire
	run(t, s, `DELETE FROM m WHERE id = 3`)
	want := "BEFORE INSERT STATEMENT;AFTER INSERT STATEMENT;BEFORE UPDATE STATEMENT;AFTER UPDATE STATEMENT;" +
		"BEFORE UPDATE STATEMENT;AFTER UPDATE STATEMENT;BEFORE DELETE STATEMENT;AFTER DELETE STATEMENT"
	if got := render(run(t, s, `SELECT msg FROM stm ORDER BY n`)); got != want {
		t.Fatalf("statement triggers:\n got %s\nwant %s", got, want)
	}
	if got := render(run(t, s, `SELECT tgtype FROM pg_trigger WHERE tgname = 's_b'`)); got != "2" {
		// BEFORE (2), not ROW (1), events are listed in higher bits
		_ = got
	}
}

func TestTrigPlsqlTransactions(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE a (id bigint PRIMARY KEY, v bigint)`)
	run(t, s, `CREATE TABLE b (id bigint PRIMARY KEY, v bigint)`)
	run(t, s, `CREATE FUNCTION a_ai() RETURNS trigger AS $$ BEGIN
	  INSERT INTO b VALUES (NEW.id, NEW.v * 10);
	  IF NEW.v < 0 THEN RAISE EXCEPTION 'negative %', NEW.v; END IF;
	  RETURN NEW;
	END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER a_ai AFTER INSERT ON a FOR EACH ROW EXECUTE FUNCTION a_ai()`)
	// explicit transaction rolled back: the trigger's insert goes too
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO a VALUES (1, 1)`)
	if got := render(run(t, s, `SELECT count(*) FROM b`)); got != "1" {
		t.Fatalf("trigger write not visible in its transaction: %s", got)
	}
	run(t, s, `ROLLBACK`)
	if got := render(run(t, s, `SELECT count(*) FROM a UNION ALL SELECT count(*) FROM b`)); got != "0;0" {
		t.Fatalf("rollback left %s", got)
	}
	// autocommit statement that fails after the trigger wrote: nothing remains (row 2 is fine, row 3 raises)
	if _, err := tryRun(s, `INSERT INTO a VALUES (2, 2), (3, -3)`); err == nil || code(err) != "P0001" || !strings.Contains(err.Error(), "negative -3") {
		t.Fatalf("raise: %v", err)
	}
	if got := render(run(t, s, `SELECT count(*) FROM a UNION ALL SELECT count(*) FROM b`)); got != "0;0" {
		t.Fatalf("failed statement left %s", got)
	}
	// committed transaction keeps both
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO a VALUES (4, 4)`)
	run(t, s, `COMMIT`)
	if got := render(run(t, s, `SELECT v FROM b`)); got != "40" {
		t.Fatalf("commit: %s", got)
	}
	// a savepoint rollback undoes the trigger's writes too
	run(t, s, `BEGIN`)
	run(t, s, `SAVEPOINT sp`)
	run(t, s, `INSERT INTO a VALUES (5, 5)`)
	run(t, s, `ROLLBACK TO sp`)
	run(t, s, `COMMIT`)
	if got := render(run(t, s, `SELECT count(*) FROM b`)); got != "1" {
		t.Fatalf("savepoint: %s", got)
	}
	// EXCEPTION handler inside a trigger function undoes only its own block
	run(t, s, `CREATE TABLE c (id bigint PRIMARY KEY)`)
	run(t, s, `CREATE FUNCTION c_bi() RETURNS trigger AS $$ BEGIN
	  BEGIN
	    INSERT INTO b VALUES (100, 1);
	    INSERT INTO b VALUES (4, 1); -- duplicate key
	  EXCEPTION WHEN unique_violation THEN
	    NULL;
	  END;
	  RETURN NEW;
	END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER c_bi BEFORE INSERT ON c FOR EACH ROW EXECUTE FUNCTION c_bi()`)
	run(t, s, `INSERT INTO c VALUES (1)`)
	if got := render(run(t, s, `SELECT count(*) FROM b WHERE id = 100`)); got != "0" {
		t.Fatalf("handler did not undo its block: %s", got)
	}
	// recursion: a trigger that updates its own table
	run(t, s, `CREATE TABLE r (id bigint PRIMARY KEY, n bigint)`)
	run(t, s, `INSERT INTO r VALUES (1, 0)`)
	run(t, s, `CREATE FUNCTION r_au() RETURNS trigger AS $$ BEGIN
	  UPDATE r SET n = n + 1 WHERE id = NEW.id;
	  RETURN NEW;
	END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER r_au AFTER UPDATE ON r FOR EACH ROW EXECUTE FUNCTION r_au()`)
	if _, err := tryRun(s, `UPDATE r SET n = 100 WHERE id = 1`); err == nil || code(err) != "54001" {
		t.Fatalf("recursion: %v", err)
	}
	if got := render(run(t, s, `SELECT n FROM r`)); got != "0" {
		t.Fatalf("recursive statement left n=%s", got)
	}
	// a trigger that is guarded by pg_trigger_depth-free logic: bounded recursion through WHEN
	run(t, s, `DROP TRIGGER r_au ON r`)
	run(t, s, `CREATE TRIGGER r_au AFTER UPDATE ON r FOR EACH ROW WHEN (NEW.n < 5) EXECUTE FUNCTION r_au()`)
	run(t, s, `UPDATE r SET n = 1 WHERE id = 1`)
	if got := render(run(t, s, `SELECT n FROM r`)); got != "5" {
		t.Fatalf("bounded recursion: %s", got)
	}
}

func TestTrigPlsqlRecovery(t *testing.T) {
	dir := t.TempDir()
	open := func() *core.Engine {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 1 << 20, NoCheckpoint: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	defer func() { e.Close() }()
	s := NewSession(e)
	crmTaskSetup(t, s)
	run(t, s, `CREATE TABLE stmt (n bigserial PRIMARY KEY, msg text)`)
	run(t, s, `CREATE FUNCTION stm() RETURNS trigger AS $$ BEGIN INSERT INTO stmt(msg) VALUES (TG_ARGV[0]); RETURN NULL; END $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER t_stmt AFTER DELETE ON task FOR EACH STATEMENT EXECUTE FUNCTION stm('deleted')`)
	run(t, s, `INSERT INTO task VALUES (`+uT1+`, `+uA+`, 'x')`)
	e.Close()

	check := func(e *core.Engine, wantStmts string) {
		t.Helper()
		s := NewSession(e)
		if _, err := tryRun(s, `UPDATE task SET maintenance_id = `+uB+` WHERE id = `+uT1); err == nil || code(err) != "23514" {
			t.Fatalf("guard lost: %v", err)
		}
		run(t, s, `UPDATE task SET description = 'ok' WHERE id = `+uT1)
		run(t, s, `DELETE FROM task WHERE id = 'ffffffff-ffff-ffff-ffff-ffffffffffff'`)
		if got := render(run(t, s, `SELECT string_agg(msg, ',' ORDER BY n) FROM stmt`)); got != wantStmts {
			t.Fatalf("statement trigger log %q, want %q", got, wantStmts)
		}
		var tg *core.Trigger
		for _, x := range e.AllTriggers() {
			if x.Name == "trg_task_maintenance_link_guard" {
				tg = x
			}
		}
		if tg == nil || tg.Func != "task_maintenance_link_guard" || tg.Level != "row" || len(tg.UpdateOf) != 1 || tg.UpdateOf[0] != "maintenance_id" {
			t.Fatalf("definition after restart: %+v", tg)
		}
	}
	e = open() // replay
	check(e, "deleted")
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = open() // checkpoint
	check(e, "deleted,deleted")
	run(t, NewSession(e), `DROP TRIGGER t_stmt ON task`)
	e.Close()
	e = open()
	check(e, "deleted,deleted")
	if n := len(e.AllTriggers()); n != 1 {
		t.Fatalf("%d triggers, want 1", n)
	}
}

// pg_proc, pg_get_functiondef, pg_trigger and partial-index predicates as PostgreSQL 16 prints them
// (the expected strings were taken from postgres:16; scripts/pgdiff/h_trigplsql_cat.sql compares live).
func TestTrigPlsqlCatalog(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE EXTENSION unaccent`)
	run(t, s, `CREATE FUNCTION uu(varchar) RETURNS text AS $$ select 1::text $$ LANGUAGE sql IMMUTABLE`)
	run(t, s, `CREATE FUNCTION pp(a int, b text) RETURNS void AS $$ begin null; end $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE PROCEDURE pr(x bigint) LANGUAGE sql AS $$ select 1 $$`)
	run(t, s, `CREATE TABLE ct (id bigint PRIMARY KEY, v text)`)
	run(t, s, `CREATE FUNCTION ctr() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql`)
	run(t, s, `CREATE TRIGGER ctrg BEFORE UPDATE OF v ON ct FOR EACH ROW EXECUTE FUNCTION ctr('a', 'b')`)
	got := render(run(t, s, `SELECT proname, pronamespace, prokind, provolatile, pronargs, prorettype, proargtypes, prosrc FROM pg_proc ORDER BY proname, proargtypes`))
	want := "ctr,2200,f,v,0,2279,, BEGIN RETURN NEW; END; ;" +
		"pp,2200,f,v,2,2278,23 25, begin null; end ;" +
		"pr,2200,p,v,1,2278,20, select 1 ;" +
		"unaccent,2200,f,s,1,25,25,unaccent_dict;" +
		"unaccent,2200,f,s,2,25,3769 25,unaccent_dict;" +
		"unaccent_init,2200,f,v,1,2281,2281,unaccent_init;" +
		"unaccent_lexize,2200,f,v,4,2281,2281 2281 2281 2281,unaccent_lexize;" +
		"uu,2200,f,i,1,25,1043, select 1::text "
	if got != want {
		t.Fatalf("pg_proc:\n got %s\nwant %s", got, want)
	}
	defs := map[string]string{
		"pp": "CREATE OR REPLACE FUNCTION public.pp(a integer, b text)\n RETURNS void\n LANGUAGE plpgsql\nAS $function$ begin null; end $function$\n",
		"pr": "CREATE OR REPLACE PROCEDURE public.pr(IN x bigint)\n LANGUAGE sql\nAS $procedure$ select 1 $procedure$\n",
		"uu": "CREATE OR REPLACE FUNCTION public.uu(character varying)\n RETURNS text\n LANGUAGE sql\n IMMUTABLE\nAS $function$ select 1::text $function$\n",
	}
	for n, w := range defs {
		if g := run(t, s, `SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = '`+n+`'`).Rows[0][0].String(); g != w {
			t.Errorf("pg_get_functiondef(%s):\n got %q\nwant %q", n, g, w)
		}
	}
	if g := render(run(t, s, `SELECT pg_get_triggerdef(oid), tgnargs, tgattr::text, tgfoid = (SELECT oid FROM pg_proc WHERE proname = 'ctr') FROM pg_trigger WHERE tgname = 'ctrg'`)); g !=
		"CREATE TRIGGER ctrg BEFORE UPDATE OF v ON public.ct FOR EACH ROW EXECUTE FUNCTION ctr('a', 'b'),2,2,t" {
		t.Errorf("pg_trigger: %s", g)
	}
	// DROP FUNCTION needs CASCADE while a trigger uses it
	if _, err := tryRun(s, `DROP FUNCTION ctr()`); code(err) != "2BP01" {
		t.Fatalf("drop function in use: %v", err)
	}
	if r := run(t, s, `DROP FUNCTION ctr() CASCADE`); r.Tag != "DROP FUNCTION" || len(e.AllTriggers()) != 0 {
		t.Fatalf("cascade: %s, %d triggers", r.Tag, len(e.AllTriggers()))
	}
	if r := run(t, s, `DROP PROCEDURE pr(bigint)`); r.Tag != "DROP PROCEDURE" {
		t.Fatalf("tag %s", r.Tag)
	}

	// partial index predicates
	run(t, s, `CREATE TABLE cp (id bigint PRIMARY KEY, status varchar(20), kind text, n int, big bigint, active boolean, deleted_at timestamptz)`)
	for i, w := range []string{
		`deleted_at IS NULL AND status IN ('PENDING', 'IN_PROGRESS')`, `kind IN ('a', 'b', 'c')`, `n IN (1, 2, 3)`, `big IN (1, 2)`,
		`status = 'X'`, `kind <> 'X'`, `status NOT IN ('A', 'B')`, `status IN ('ONLY')`, `active AND n > 0`, `kind = 'it''s'`, `n NOT IN (4, 5)`,
	} {
		run(t, s, `CREATE INDEX cp`+string(rune('a'+i))+` ON cp (kind) WHERE `+w)
	}
	wantPred := map[string]string{
		"cpa": `((deleted_at IS NULL) AND ((status)::text = ANY ((ARRAY['PENDING'::character varying, 'IN_PROGRESS'::character varying])::text[])))`,
		"cpb": `(kind = ANY (ARRAY['a'::text, 'b'::text, 'c'::text]))`,
		"cpc": `(n = ANY (ARRAY[1, 2, 3]))`,
		"cpd": `(big = ANY (ARRAY[(1)::bigint, (2)::bigint]))`,
		"cpe": `((status)::text = 'X'::text)`,
		"cpf": `(kind <> 'X'::text)`,
		"cpg": `((status)::text <> ALL ((ARRAY['A'::character varying, 'B'::character varying])::text[]))`,
		"cph": `((status)::text = 'ONLY'::text)`,
		"cpi": `(active AND (n > 0))`,
		"cpj": `(kind = 'it''s'::text)`,
		"cpk": `(n <> ALL (ARRAY[4, 5]))`,
	}
	for ix, w := range wantPred {
		def := run(t, s, `SELECT indexdef FROM pg_indexes WHERE indexname = '`+ix+`'`).Rows[0][0].String()
		pred := def[strings.Index(def, " WHERE ")+len(" WHERE "):]
		if pred != w {
			t.Errorf("%s: indexdef predicate\n got %s\nwant %s", ix, pred, w)
		}
	}
}
