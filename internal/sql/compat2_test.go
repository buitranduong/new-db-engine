package sql

import "testing"

func TestErrorCodesAndSessionStatements(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE e1 (id BIGINT PRIMARY KEY, v TEXT)`)
	for q, want := range map[string]string{
		`SELECT nope FROM e1`:              codeUndefinedColumn,
		`SELECT id FROM e1 WHERE nope = 1`: codeUndefinedColumn,
		`SELECT id FROM e1 ORDER BY nope`:  codeUndefinedColumn,
		`SELEC 1`:                          codeSyntax,
		`SELECT 9223372036854775807 + 1`:   codeNumericOutOfRange,
		`SELECT -9223372036854775807 - 2`:  codeNumericOutOfRange,
		`SELECT 4611686018427387904 * 2`:   codeNumericOutOfRange,
		`SAVEPOINT x`:                      "25P01",
		`ROLLBACK TO SAVEPOINT x`:          "25P01",
		`SHOW no_such_setting`:             codeUndefinedObject,
		`RESET no_such_setting`:            codeUndefinedObject,
	} {
		if _, err := tryRun(s, q); code(err) != want {
			t.Errorf("%s: code %q, want %q (%v)", q, code(err), want, err)
		}
	}
	run(t, s, `SELECT id AS x FROM e1 ORDER BY x`) // output aliases still resolve
	if got := render(run(t, s, `SELECT 9223372036854775806 + 1`)); got != "9223372036854775807" {
		t.Errorf("max bigint: %s", got)
	}
}

func TestSavepoints(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE sp (id BIGINT PRIMARY KEY, v TEXT)`)
	count := func() string { return render(run(t, s, `SELECT count(*) FROM sp`)) }

	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO sp VALUES (1, 'a')`)
	run(t, s, `SAVEPOINT s1`)
	run(t, s, `INSERT INTO sp VALUES (2, 'b')`)
	run(t, s, `UPDATE sp SET v = 'z' WHERE id = 1`)
	run(t, s, `SAVEPOINT s2`)
	run(t, s, `INSERT INTO sp VALUES (3, 'c')`)
	if count() != "3" {
		t.Fatal("before rollback", count())
	}
	run(t, s, `ROLLBACK TO SAVEPOINT s2`)
	if count() != "2" {
		t.Errorf("after rollback to s2: %s", count())
	}
	run(t, s, `INSERT INTO sp VALUES (3, 'again')`) // the key is free again
	run(t, s, `ROLLBACK TO s1`)
	if got := render(run(t, s, `SELECT id, v FROM sp`)); got != "1,a" {
		t.Errorf("after rollback to s1: %s", got)
	}
	// s2 is gone, s1 stays and can be rolled back to again
	if _, err := tryRun(s, `ROLLBACK TO SAVEPOINT s2`); code(err) != "3B001" {
		t.Errorf("s2 after rollback to s1: %v", err)
	}
	run(t, s, `INSERT INTO sp VALUES (9, 'x')`)
	run(t, s, `ROLLBACK TO s1`)
	if count() != "1" {
		t.Errorf("second rollback to s1: %s", count())
	}
	// an error aborts the transaction until ROLLBACK TO
	if _, err := tryRun(s, `INSERT INTO sp VALUES (1, 'dup')`); code(err) != codeUniqueViolation {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := tryRun(s, `SELECT 1`); code(err) != codeInFailedTx {
		t.Errorf("failed transaction: %v", err)
	}
	run(t, s, `ROLLBACK TO SAVEPOINT s1`)
	run(t, s, `INSERT INTO sp VALUES (4, 'd')`)
	run(t, s, `RELEASE SAVEPOINT s1`)
	if _, err := tryRun(s, `ROLLBACK TO s1`); code(err) != "3B001" {
		t.Errorf("released: %v", err)
	}
	run(t, s, `COMMIT`)
	if got := render(run(t, s, `SELECT id FROM sp ORDER BY id`)); got != "1;4" {
		t.Errorf("committed: %s", got)
	}
}

func TestReadOnlySetLocalReset(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE ro (id BIGINT PRIMARY KEY)`)
	run(t, s, `BEGIN READ ONLY`)
	if _, err := tryRun(s, `INSERT INTO ro VALUES (1)`); code(err) != "25006" {
		t.Errorf("insert in read only: %v", err)
	}
	run(t, s, `ROLLBACK`)
	run(t, s, `BEGIN`)
	run(t, s, `SET TRANSACTION READ ONLY`)
	if _, err := tryRun(s, `CREATE TABLE ro2 (id BIGINT PRIMARY KEY)`); code(err) != "25006" {
		t.Errorf("DDL in read only: %v", err)
	}
	run(t, s, `ROLLBACK`)
	run(t, s, `INSERT INTO ro VALUES (1)`) // a new transaction is read write

	run(t, s, `SET app.tenant = 'a'`)
	run(t, s, `BEGIN`)
	run(t, s, `SET LOCAL app.tenant = 'b'`)
	run(t, s, `SELECT set_config('app.other', 'x', true)`)
	if got := render(run(t, s, `SELECT current_setting('app.tenant')`)); got != "b" {
		t.Errorf("inside: %s", got)
	}
	run(t, s, `COMMIT`)
	if got := render(run(t, s, `SELECT current_setting('app.tenant')`)); got != "a" {
		t.Errorf("after commit: %s", got)
	}
	if got := render(run(t, s, `SELECT current_setting('app.other', true)`)); got != "NULL" {
		t.Errorf("local set_config leaked: %s", got)
	}
	if got := render(run(t, s, `SELECT set_config('app.cc', '1', false), current_setting('app.cc')`)); got != "1,1" {
		t.Errorf("set_config: %s", got)
	}

	run(t, s, `SET application_name = 'x'`)
	run(t, s, `SET statement_timeout = '5s'`)
	run(t, s, `RESET application_name`)
	run(t, s, `RESET statement_timeout`)
	if got := render(run(t, s, `SHOW application_name`)); got != "" {
		t.Errorf("reset application_name: %q", got)
	}
	if got := render(run(t, s, `SHOW statement_timeout`)); got != "0" {
		t.Errorf("reset statement_timeout: %q", got)
	}
	run(t, s, `SET search_path TO other`)
	run(t, s, `DISCARD ALL`)
	if got := render(run(t, s, `SHOW search_path`)); got != "public" {
		t.Errorf("discard all: %q", got)
	}
	if got := render(run(t, s, `SELECT current_setting('app.tenant', true)`)); got != "NULL" {
		t.Errorf("discard all kept custom settings: %q", got)
	}
	run(t, s, `RESET ALL`)
	if got := render(run(t, s, `SHOW max_identifier_length`)); got != "63" {
		t.Errorf("max_identifier_length: %q", got)
	}
	run(t, s, `COMMENT ON TABLE ro IS 'remarks'`)
	run(t, s, `COMMENT ON COLUMN ro.id IS 'remarks'`)
}

func TestDollarQuotedStrings(t *testing.T) {
	s := NewSession(newEngine(t))
	for q, want := range map[string]string{
		`SELECT $$it's "raw" \n$$`:                   `it's "raw" \n`,
		`SELECT $tag$a $$ b$tag$ || 'x'`:             `a $$ b` + "x",
		`SELECT $1 + 1`:                              "",
		`SELECT $$$$ || 'e'`:                         "e",
		`SELECT current_setting($$server_version$$)`: "16.0",
	} {
		if q == `SELECT $1 + 1` {
			if got := render(run(t, s, q, IntD(1))); got != "2" {
				t.Errorf("%s = %q", q, got)
			}
			continue
		}
		if got := render(run(t, s, q)); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
}

func TestLockingClauses(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE lk (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO lk VALUES (1, 'a'), (2, 'b')`)
	for _, q := range []string{
		`SELECT v FROM lk WHERE id = 1 FOR UPDATE`,
		`SELECT v FROM lk WHERE id = 1 FOR UPDATE OF lk`,
		`SELECT v FROM lk l WHERE id = 1 FOR UPDATE OF l NOWAIT`,
		`SELECT v FROM lk WHERE id = 1 FOR NO KEY UPDATE SKIP LOCKED`,
		`SELECT v FROM lk WHERE id = 1 FOR SHARE`,
		`SELECT v FROM lk WHERE id = 1 FOR KEY SHARE`,
	} {
		if got := render(run(t, s, q)); got != "a" {
			t.Errorf("%s = %q", q, got)
		}
	}
}

func TestOnConflictOnConstraint(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE oc (id BIGINT PRIMARY KEY, v TEXT)`)
	run(t, s, `INSERT INTO oc VALUES (1, 'a')`)
	run(t, s, `INSERT INTO oc VALUES (1, 'b') ON CONFLICT ON CONSTRAINT oc_pkey DO NOTHING`)
	run(t, s, `INSERT INTO oc VALUES (1, 'c') ON CONFLICT ON CONSTRAINT oc_pkey DO UPDATE SET v = excluded.v`)
	if got := render(run(t, s, `SELECT v FROM oc`)); got != "c" {
		t.Errorf("got %q", got)
	}
}
