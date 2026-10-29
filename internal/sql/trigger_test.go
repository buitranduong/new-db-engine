package sql

import (
	"strings"
	"testing"

	"stask/internal/core"
)

func trigSetup(t *testing.T, s *Session) {
	t.Helper()
	run(t, s, `CREATE TABLE acct (id BIGINT PRIMARY KEY, name TEXT, bal BIGINT, upd BIGINT)`)
	run(t, s, `CREATE TABLE audit (id BIGINT PRIMARY KEY, op TEXT, oid BIGINT, ob BIGINT, nb BIGINT)`)
	run(t, s, `CREATE SEQUENCE audit_seq`)
}

const auditAll = `
CREATE TRIGGER acct_ins AFTER INSERT ON acct FOR EACH ROW
BEGIN INSERT INTO audit VALUES (nextval('audit_seq'), 'ins', NEW.id, NULL, NEW.bal); END;
CREATE TRIGGER acct_upd AFTER UPDATE ON acct FOR EACH ROW WHEN (OLD.bal IS DISTINCT FROM NEW.bal)
BEGIN INSERT INTO audit VALUES (nextval('audit_seq'), 'upd', NEW.id, OLD.bal, NEW.bal); END;
CREATE TRIGGER acct_del AFTER DELETE ON acct FOR EACH ROW
BEGIN INSERT INTO audit VALUES (nextval('audit_seq'), 'del', OLD.id, OLD.bal, NULL); END`

func TestTriggerAfter(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	run(t, s, auditAll)
	run(t, s, `INSERT INTO acct VALUES (1,'a',100,0),(2,'b',200,0)`)
	run(t, s, `UPDATE acct SET bal = bal + 5 WHERE id = 1`)
	run(t, s, `UPDATE acct SET name = 'bb' WHERE id = 2`) // WHEN false: no audit
	run(t, s, `DELETE FROM acct WHERE id = 2`)
	got := render(run(t, s, `SELECT op, oid, ob, nb FROM audit ORDER BY id`))
	want := "ins,1,NULL,100;ins,2,NULL,200;upd,1,100,105;del,2,200,NULL"
	if got != want {
		t.Fatalf("audit:\n got %s\nwant %s", got, want)
	}
	// INSERT ... SELECT, UPDATE ... FROM, DELETE ... USING, upsert
	run(t, s, `INSERT INTO acct SELECT id + 10, name, bal, 0 FROM acct`)
	run(t, s, `UPDATE acct SET bal = x.v FROM (SELECT 1 AS id, 7 AS v) x WHERE acct.id = x.id`)
	run(t, s, `DELETE FROM acct USING (SELECT 11 AS id) d WHERE acct.id = d.id`)
	run(t, s, `INSERT INTO acct VALUES (1,'a',1,0) ON CONFLICT (id) DO UPDATE SET bal = EXCLUDED.bal + 1000`)
	run(t, s, `UPDATE acct SET bal = bal + 1 WHERE id IN (SELECT id FROM acct WHERE id = 1)`)
	got = render(run(t, s, `SELECT op, oid, ob, nb FROM audit WHERE id > 4 ORDER BY id`))
	want = "ins,11,NULL,105;upd,1,105,7;del,11,105,NULL;upd,1,7,1001;upd,1,1001,1002"
	if got != want {
		t.Fatalf("audit 2:\n got %s\nwant %s", got, want)
	}
	// a key change is one UPDATE
	run(t, s, `UPDATE acct SET id = 99 WHERE id = 1`)
	if got := render(run(t, s, `SELECT op, oid FROM audit ORDER BY id DESC LIMIT 1`)); got != "upd,99" && got != "upd,1" {
		t.Fatalf("key change: %s", got)
	}
	// DO NOTHING: no row, no AFTER INSERT
	n := render(run(t, s, `SELECT count(*) FROM audit`))
	run(t, s, `INSERT INTO acct VALUES (99,'x',1,0) ON CONFLICT DO NOTHING`)
	if render(run(t, s, `SELECT count(*) FROM audit`)) != n {
		t.Fatal("DO NOTHING fired a trigger")
	}
}

func TestTriggerBeforeSetAndRaise(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	run(t, s, `CREATE TRIGGER b_stamp BEFORE INSERT OR UPDATE ON acct FOR EACH ROW
	  BEGIN SET NEW.upd = NEW.upd + 1, NEW.name = upper(NEW.name); END`)
	run(t, s, `CREATE TRIGGER c_neg BEFORE INSERT OR UPDATE ON acct
	  BEGIN SELECT RAISE(ABORT, 'negative balance') WHERE NEW.bal < 0; END`)
	run(t, s, `CREATE TRIGGER d_nodel BEFORE DELETE ON acct WHEN (OLD.bal > 0)
	  BEGIN SELECT CASE WHEN OLD.name = 'KEEP' THEN RAISE(ABORT, 'cannot delete KEEP') END; END`)
	run(t, s, `INSERT INTO acct VALUES (1,'ann',10,0),(2,'keep',20,0)`)
	if got := render(run(t, s, `SELECT name, upd FROM acct ORDER BY id`)); got != "ANN,1;KEEP,1" {
		t.Fatalf("set: %s", got)
	}
	r := run(t, s, `UPDATE acct SET bal = 11 WHERE id = 1 RETURNING upd`)
	if render(r) != "2" {
		t.Fatalf("returning sees the BEFORE change: %s", render(r))
	}
	_, err := tryRun(s, `UPDATE acct SET bal = -1 WHERE id = 1`)
	if err == nil || code(err) != "P0001" || !strings.Contains(err.Error(), "negative balance") {
		t.Fatalf("raise: %v", err)
	}
	if got := render(run(t, s, `SELECT bal FROM acct WHERE id = 1`)); got != "11" {
		t.Fatalf("aborted update visible: %s", got)
	}
	if _, err := tryRun(s, `INSERT INTO acct VALUES (3,'z',-5,0)`); code(err) != "P0001" {
		t.Fatalf("raise on insert: %v", err)
	}
	if _, err := tryRun(s, `DELETE FROM acct WHERE id = 2`); code(err) != "P0001" {
		t.Fatalf("raise on delete: %v", err)
	}
	if got := render(run(t, s, `SELECT count(*) FROM acct`)); got != "2" {
		t.Fatalf("count %s", got)
	}
}

// What a trigger wrote rolls back with the transaction, and with the
// statement that failed.
func TestTriggerRollback(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	run(t, s, auditAll)
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO acct VALUES (1,'a',1,0)`)
	if got := render(run(t, s, `SELECT count(*) FROM audit`)); got != "1" {
		t.Fatalf("trigger row not visible in its transaction: %s", got)
	}
	run(t, s, `ROLLBACK`)
	if got := render(run(t, s, `SELECT count(*) FROM audit`)); got != "0" {
		t.Fatalf("trigger row survived ROLLBACK: %s", got)
	}
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO acct VALUES (1,'a',1,0)`)
	run(t, s, `COMMIT`)
	if got := render(run(t, s, `SELECT count(*) FROM audit`)); got != "1" {
		t.Fatalf("COMMIT: %s", got)
	}
	// the statement fails after its first row fired: nothing stays (autocommit)
	if _, err := tryRun(s, `INSERT INTO acct VALUES (5,'e',1,0),(1,'dup',1,0)`); code(err) != codeUniqueViolation {
		t.Fatalf("dup: %v", err)
	}
	if got := render(run(t, s, `SELECT count(*) FROM audit`)); got != "1" {
		t.Fatalf("audit after failed statement: %s", got)
	}
	// a failing trigger body fails the statement
	run(t, s, `CREATE TRIGGER bad AFTER DELETE ON acct BEGIN INSERT INTO audit VALUES (1000, 'dup', 0, 0, 0); END`)
	run(t, s, `INSERT INTO audit VALUES (1000, 'x', 0, 0, 0)`)
	if _, err := tryRun(s, `DELETE FROM acct WHERE id = 1`); err == nil {
		t.Fatal("expected a failure from the trigger body")
	}
	if got := render(run(t, s, `SELECT count(*) FROM acct`)); got != "1" {
		t.Fatalf("row deleted although its trigger failed: %s", got)
	}
}

func TestTriggerRecursion(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE r (id BIGINT PRIMARY KEY, n BIGINT)`)
	run(t, s, `CREATE TRIGGER rec AFTER INSERT ON r BEGIN INSERT INTO r VALUES (NEW.id + 1, NEW.n + 1); END`)
	_, err := tryRun(s, `INSERT INTO r VALUES (1, 0)`)
	if code(err) != "54001" {
		t.Fatalf("infinite recursion: %v", err)
	}
	if got := render(run(t, s, `SELECT count(*) FROM r`)); got != "0" {
		t.Fatalf("rows left: %s", got)
	}
	// a bounded recursion works
	run(t, s, `DROP TRIGGER rec`)
	run(t, s, `CREATE TRIGGER rec AFTER INSERT ON r WHEN (NEW.n < 5) BEGIN INSERT INTO r VALUES (NEW.id + 1, NEW.n + 1); END`)
	run(t, s, `INSERT INTO r VALUES (1, 0)`)
	if got := render(run(t, s, `SELECT count(*) FROM r`)); got != "6" {
		t.Fatalf("bounded recursion: %s", got)
	}
	// two tables firing each other
	run(t, s, `CREATE TABLE p (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE TABLE q (id BIGINT PRIMARY KEY)`)
	run(t, s, `CREATE TRIGGER pq AFTER INSERT ON p BEGIN INSERT INTO q VALUES (NEW.id); END`)
	run(t, s, `CREATE TRIGGER qp AFTER INSERT ON q BEGIN INSERT INTO p VALUES (NEW.id + 1); END`)
	if _, err := tryRun(s, `INSERT INTO p VALUES (1)`); code(err) != "54001" {
		t.Fatalf("mutual recursion: %v", err)
	}
	// the session is usable and its depth was restored
	run(t, s, `DROP TRIGGER qp`)
	run(t, s, `INSERT INTO p VALUES (1)`)
}

func TestTriggerDDL(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	mk := `CREATE TRIGGER t1 AFTER INSERT ON acct BEGIN INSERT INTO audit VALUES (nextval('audit_seq'), 'i', NEW.id, 0, 0); END`
	run(t, s, mk)
	if _, err := tryRun(s, mk); code(err) != codeDuplicateObject {
		t.Fatalf("duplicate: %v", err)
	}
	if r := run(t, s, strings.Replace(mk, "CREATE TRIGGER", "CREATE TRIGGER IF NOT EXISTS", 1)); r.Notice == "" {
		t.Fatal("IF NOT EXISTS: no notice")
	}
	run(t, s, strings.Replace(mk, "CREATE TRIGGER", "CREATE OR REPLACE TRIGGER", 1))
	for q, c := range map[string]string{
		`CREATE TRIGGER x AFTER INSERT ON nope BEGIN SELECT 1; END`:                                   codeUndefinedTable,
		`CREATE TRIGGER x AFTER INSERT ON acct BEGIN INSERT INTO audit VALUES (NEW.zzz,1,1,1,1); END`: codeUndefinedColumn,
		`CREATE TRIGGER x AFTER INSERT ON acct BEGIN SELECT OLD.id; END`:                              "42P17",
		`CREATE TRIGGER x AFTER DELETE ON acct BEGIN SELECT NEW.id; END`:                              "42P17",
		`CREATE TRIGGER x AFTER INSERT ON acct WHEN (OLD.id = 1) BEGIN SELECT 1; END`:                 "42P17",
		`CREATE TRIGGER x AFTER INSERT ON acct BEGIN SET NEW.bal = 1; END`:                            codeFeature,
		`CREATE TRIGGER x BEFORE DELETE ON acct BEGIN SET NEW.bal = 1; END`:                           codeFeature,
		`CREATE TRIGGER x AFTER INSERT ON acct BEGIN END`:                                             codeSyntax,
		`CREATE TRIGGER x AFTER INSERT ON acct BEGIN CREATE TABLE z (a INT); END`:                     codeFeature,
		`CREATE TRIGGER x AFTER INSERT ON acct FOR EACH STATEMENT BEGIN SELECT 1; END`:                codeFeature,
		`CREATE TRIGGER x INSTEAD OF INSERT ON acct BEGIN SELECT 1; END`:                              codeFeature,
		`CREATE TRIGGER x AFTER UPDATE OF bal ON acct BEGIN SELECT 1; END`:                            codeFeature,
		`CREATE TRIGGER x AFTER INSERT ON acct EXECUTE FUNCTION f()`:                                  codeFeature,
		`DROP TRIGGER nope ON acct`:                                                                   codeUndefinedObject,
	} {
		if _, err := tryRun(s, q); err == nil || code(err) != c {
			t.Errorf("%s: code %q (%v), want %s", q, code(err), err, c)
		}
	}
	run(t, s, `CREATE VIEW vv AS SELECT id FROM acct`)
	if _, err := tryRun(s, `CREATE TRIGGER x AFTER INSERT ON vv BEGIN SELECT 1; END`); code(err) != codeWrongObjectType {
		t.Fatalf("trigger on a view: %v", err)
	}
	run(t, s, `DROP VIEW vv`)
	// hidden table
	if _, err := tryRun(s, `SELECT * FROM __stask_triggers`); err == nil {
		t.Fatal("system table visible")
	}
	if _, err := tryRun(s, `ALTER TABLE acct RENAME TO acct2`); code(err) != codeFeature {
		t.Fatalf("rename with triggers: %v", err)
	}
	if r := run(t, s, `DROP TRIGGER IF EXISTS nope ON acct`); r.Notice == "" {
		t.Fatal("no notice")
	}
	run(t, s, `DROP TRIGGER t1 ON acct`)
	run(t, s, `INSERT INTO acct VALUES (1,'a',1,0)`)
	if got := render(run(t, s, `SELECT count(*) FROM audit`)); got != "0" {
		t.Fatalf("dropped trigger fired: %s", got)
	}
	// DROP without ON finds the table; DROP TABLE takes its triggers along
	run(t, s, mk)
	run(t, s, `DROP TRIGGER t1`)
	run(t, s, mk)
	run(t, s, `DROP TABLE acct`)
	if n := len(e.AllTriggers()); n != 0 {
		t.Fatalf("%d triggers left after DROP TABLE", n)
	}
	run(t, s, `CREATE TABLE acct (id BIGINT PRIMARY KEY, name TEXT, bal BIGINT, upd BIGINT)`)
	run(t, s, `INSERT INTO acct VALUES (1,'a',1,0)`)
	if got := render(run(t, s, `SELECT count(*) FROM audit`)); got != "0" {
		t.Fatalf("trigger survived DROP TABLE: %s", got)
	}
}

// Parameters and the extended protocol: the statement firing the trigger
// has parameters of its own, the body's are separate.
func TestTriggerWithParams(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	run(t, s, auditAll)
	run(t, s, `INSERT INTO acct VALUES ($1, $2, $3, 0)`, IntD(7), TextD("p"), IntD(70))
	run(t, s, `UPDATE acct SET bal = $1 WHERE id = $2`, IntD(71), IntD(7))
	if got := render(run(t, s, `SELECT op, ob, nb FROM audit ORDER BY id`)); got != "ins,NULL,70;upd,70,71" {
		t.Fatalf("%s", got)
	}
	// string and NULL values travel intact
	run(t, s, `CREATE TABLE n (id BIGINT PRIMARY KEY, s TEXT)`)
	run(t, s, `CREATE TABLE nl (id BIGINT PRIMARY KEY, s TEXT)`)
	run(t, s, `CREATE TRIGGER cp AFTER INSERT ON n BEGIN INSERT INTO nl VALUES (NEW.id, NEW.s || ' new.s ''q'''); END`)
	run(t, s, `INSERT INTO n VALUES (1, 'a b'), (2, NULL)`)
	if got := render(run(t, s, `SELECT id, s FROM nl ORDER BY id`)); got != "1,a b new.s 'q';2,NULL" {
		t.Fatalf("%s", got)
	}
}

func TestTriggerRecovery(t *testing.T) {
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
	trigSetup(t, s)
	run(t, s, auditAll)
	run(t, s, `CREATE TRIGGER gone AFTER INSERT ON acct BEGIN SELECT 1; END`)
	run(t, s, `DROP TRIGGER gone`)
	run(t, s, `CREATE OR REPLACE TRIGGER acct_del AFTER DELETE ON acct
	  BEGIN INSERT INTO audit VALUES (nextval('audit_seq'), 'DEL', OLD.id, OLD.bal, NULL); END`)
	e.Close()

	check := func(e *core.Engine, id int64, wantOps string) {
		t.Helper()
		s := NewSession(e)
		run(t, s, `INSERT INTO acct VALUES ($1,'a',5,0)`, IntD(id))
		run(t, s, `DELETE FROM acct WHERE id = $1`, IntD(id))
		if got := render(run(t, s, `SELECT string_agg(op, ',' ORDER BY id) FROM audit`)); got != wantOps {
			t.Fatalf("ops %s, want %s", got, wantOps)
		}
	}
	e = open()
	check(e, 1, "ins,DEL")
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	run(t, NewSession(e), `CREATE TRIGGER late AFTER UPDATE ON acct BEGIN INSERT INTO audit VALUES (nextval('audit_seq'), 'late', 0, 0, 0); END`)
	e.Close()
	e = open() // checkpoint + replay
	check(e, 2, "ins,DEL,ins,DEL")
	run(t, NewSession(e), `INSERT INTO acct VALUES (50,'u',1,0)`)
	run(t, NewSession(e), `UPDATE acct SET name = 'v' WHERE id = 50`)
	if got := render(run(t, NewSession(e), `SELECT count(*) FROM audit WHERE op = 'late'`)); got != "1" {
		t.Fatalf("late trigger: %s", got)
	}
	e.Close()
	e = open()
	if n := len(e.AllTriggers()); n != 4 {
		t.Fatalf("%d triggers after reopen, want 4", n)
	}
}

// Hidden from CDC: the trigger's writes are changes of the audit table,
// the definitions are not.
func TestTriggerCDC(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	run(t, s, `CREATE PUBLICATION p FOR ALL TABLES`)
	run(t, s, `SELECT pg_create_logical_replication_slot('ts', 'test_decoding')`)
	run(t, s, auditAll)
	run(t, s, `INSERT INTO acct VALUES (1,'a',1,0)`)
	out := strings.Join(data(run(t, s, `SELECT * FROM pg_logical_slot_peek_changes('ts', NULL, NULL)`)), "\n")
	if strings.Contains(out, "__stask") {
		t.Fatalf("trigger table leaked into CDC:\n%s", out)
	}
	if !strings.Contains(out, "table public.acct: INSERT") || !strings.Contains(out, "table public.audit: INSERT") {
		t.Fatalf("changes missing:\n%s", out)
	}
}

func TestTriggerCopy(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	run(t, s, auditAll)
	run(t, s, `CREATE TRIGGER st BEFORE INSERT ON acct BEGIN SET NEW.upd = 42; END`)
	cs, ok, err := ParseCopy(`COPY acct FROM STDIN`)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ci, err := s.CopyFrom(cs)
	if err != nil {
		t.Fatal(err)
	}
	if err := ci.Write([]byte("1\ta\t5\t0\n2\tb\t6\t0\n\\.\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := ci.Finish(); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, s, `SELECT id, upd FROM acct ORDER BY id`)); got != "1,42;2,42" {
		t.Fatalf("before trigger on COPY: %s", got)
	}
	if got := render(run(t, s, `SELECT op, nb FROM audit ORDER BY id`)); got != "ins,5;ins,6" {
		t.Fatalf("after trigger on COPY: %s", got)
	}
}

func TestTriggerCatalog(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	trigSetup(t, s)
	run(t, s, auditAll)
	run(t, s, `CREATE TRIGGER b1 BEFORE INSERT OR UPDATE ON audit BEGIN SELECT 1; END`)
	got := render(run(t, s, `SELECT c.relname, t.tgname, t.tgtype FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid ORDER BY 1, 2`))
	if got != "acct,acct_del,9;acct,acct_ins,5;acct,acct_upd,17;audit,b1,23" {
		t.Fatalf("pg_trigger: %s", got)
	}
	if got := render(run(t, s, `SELECT relname FROM pg_class WHERE relhastriggers ORDER BY 1`)); got != "acct;audit" {
		t.Fatalf("relhastriggers: %s", got)
	}
	if got := render(run(t, s, `SELECT tablename FROM pg_tables WHERE hastriggers ORDER BY 1`)); got != "acct;audit" {
		t.Fatalf("pg_tables.hastriggers: %s", got)
	}
}
