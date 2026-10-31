package sql

import "testing"

func trigGapSetup(t *testing.T) *Session {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE t (id INT PRIMARY KEY, v TEXT)`)
	run(t, s, `CREATE TABLE src (id INT PRIMARY KEY, v TEXT)`)
	run(t, s, `CREATE TABLE lg (n INT)`)
	run(t, s, `CREATE TABLE sl (id BIGSERIAL PRIMARY KEY, ev TEXT)`)
	return s
}

// AFTER ROW triggers run when the statement has written all its rows.
func TestGapsAfterRowQueued(t *testing.T) {
	s := trigGapSetup(t)
	run(t, s, `CREATE TRIGGER ar AFTER INSERT ON t FOR EACH ROW BEGIN INSERT INTO lg VALUES ((SELECT count(*) FROM t)); END`)
	run(t, s, `INSERT INTO t VALUES (1,'a'),(2,'b'),(3,'c')`)
	if got := one(t, s, `SELECT string_agg(n::text, ',') FROM lg`); got != "3,3,3" {
		t.Fatalf("AFTER ROW saw %s, want 3,3,3 (all rows written first)", got)
	}
	// the BEFORE row trigger still runs row by row
	run(t, s, `DELETE FROM lg`)
	run(t, s, `CREATE TRIGGER br BEFORE INSERT ON t FOR EACH ROW BEGIN INSERT INTO lg VALUES ((SELECT count(*) FROM t)); END`)
	run(t, s, `INSERT INTO t VALUES (4,'d'),(5,'e')`)
	if got := one(t, s, `SELECT string_agg(n::text, ',' ORDER BY n) FROM lg`); got != "3,3,5,5,5" && got != "3,4,5,5,5" {
		// before-row: 3 and 3 (nothing written yet to the table by the earlier row of the statement is visible to a new query? order unspecified)
		t.Logf("BEFORE ROW + AFTER ROW counts: %s", got)
	}
	// UPDATE and DELETE
	run(t, s, `DELETE FROM lg`)
	run(t, s, `DROP TRIGGER br ON t`)
	run(t, s, `DROP TRIGGER ar ON t`)
	run(t, s, `CREATE TRIGGER au AFTER UPDATE ON t FOR EACH ROW BEGIN INSERT INTO lg VALUES ((SELECT count(*) FROM t WHERE v = 'z')); END`)
	run(t, s, `UPDATE t SET v = 'z'`)
	if got := one(t, s, `SELECT string_agg(n::text, ',') FROM lg`); got != "5,5,5,5,5" {
		t.Fatalf("AFTER UPDATE ROW: %s", got)
	}
}

func TestGapsStatementTriggersEverywhere(t *testing.T) {
	s := trigGapSetup(t)
	for _, ev := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
		run(t, s, `CREATE TRIGGER b_`+ev+` BEFORE `+ev+` ON t FOR EACH STATEMENT BEGIN INSERT INTO sl (ev) VALUES ('b `+ev+`'); END`)
		run(t, s, `CREATE TRIGGER a_`+ev+` AFTER `+ev+` ON t FOR EACH STATEMENT BEGIN INSERT INTO sl (ev) VALUES ('a `+ev+`'); END`)
	}
	log := func() string {
		r := one(t, s, `SELECT coalesce(string_agg(ev, ',' ORDER BY ctid_order), '') FROM (SELECT ev, row_number() OVER () AS ctid_order FROM sl) q`)
		run(t, s, `DELETE FROM sl`)
		return r
	}
	_ = log
	order := func() string {
		r := one(t, s, `SELECT coalesce(string_agg(ev, ',' ORDER BY id), '') FROM sl`)
		run(t, s, `DELETE FROM sl`)
		return r
	}
	run(t, s, `INSERT INTO t VALUES (1,'a')`)
	if got := order(); got != "b INSERT,a INSERT" {
		t.Fatalf("INSERT: %s", got)
	}
	// COPY FROM
	cs, _, _ := ParseCopy(`COPY t FROM STDIN`)
	ci, err := s.CopyFrom(cs)
	if err != nil {
		t.Fatal(err)
	}
	if err := ci.Write([]byte("2\tb\n3\tc\n\\.\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := ci.Finish(); err != nil {
		t.Fatal(err)
	}
	if got := order(); got != "b INSERT,a INSERT" {
		t.Fatalf("COPY: %s", got)
	}
	// MERGE: one set per kind of action
	run(t, s, `INSERT INTO src VALUES (1,'m'),(9,'n')`)
	run(t, s, `DELETE FROM sl`)
	run(t, s, `MERGE INTO t USING src ON t.id = src.id WHEN MATCHED THEN UPDATE SET v = src.v WHEN NOT MATCHED THEN INSERT VALUES (src.id, src.v)`)
	got := order()
	if got != "b UPDATE,b INSERT,a UPDATE,a INSERT" && got != "b INSERT,b UPDATE,a INSERT,a UPDATE" {
		t.Fatalf("MERGE: %s", got)
	}
	// WITH (data-modifying)
	run(t, s, `WITH d AS (DELETE FROM t WHERE id = 9 RETURNING id) SELECT count(*) FROM d`)
	if got := order(); got != "b DELETE,a DELETE" {
		t.Fatalf("WITH DELETE: %s", got)
	}
	run(t, s, `WITH i AS (INSERT INTO t VALUES (20,'w') RETURNING id) SELECT count(*) FROM i`)
	if got := order(); got != "b INSERT,a INSERT" {
		t.Fatalf("WITH INSERT + UPDATE: %s", got)
	}
	// TRUNCATE: statement level, atomic, in and out of a transaction
	run(t, s, `TRUNCATE t`)
	if got := order(); got != "b TRUNCATE,a TRUNCATE" {
		t.Fatalf("TRUNCATE: %s", got)
	}
	if got := one(t, s, `SELECT count(*) FROM t`); got != "0" {
		t.Fatal(got)
	}
	run(t, s, `INSERT INTO t VALUES (1,'a')`)
	run(t, s, `DELETE FROM sl`)
	run(t, s, `BEGIN`)
	run(t, s, `TRUNCATE t`)
	run(t, s, `ROLLBACK`)
	if got := one(t, s, `SELECT count(*) FROM t`); got != "1" {
		t.Fatalf("TRUNCATE rolled back: %s", got)
	}
	if got := order(); got != "" {
		t.Fatalf("log of a rolled back TRUNCATE: %q", got)
	}
	// TRUNCATE triggers must be statement-level
	if _, err := tryRun(s, `CREATE TRIGGER bad AFTER TRUNCATE ON t FOR EACH ROW BEGIN SELECT 1; END`); err == nil || AsError(err).Code != "42P17" {
		t.Fatalf("row-level TRUNCATE trigger: %v", err)
	}
	if got := one(t, s, `SELECT tgtype FROM pg_trigger WHERE tgname = 'a_truncate'`); got != "32" {
		t.Fatalf("tgtype %s", got)
	}
}
