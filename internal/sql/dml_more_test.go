package sql

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Compatibility tests of the DML area (docs/14 row compat-dml): ON CONFLICT on a unique index, MERGE
// row locks, ErrorResponse fields of constraint errors.

func TestOnConflictUniqueIndex(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, "CREATE TABLE u (id int PRIMARY KEY, name text UNIQUE, a int, b int, n int DEFAULT 5, UNIQUE (a, b))")
	run(t, s, "INSERT INTO u VALUES (1,'x',1,1), (2,'y',1,2)")
	// DO NOTHING on a unique index, with and without a target
	for _, q := range []string{
		"INSERT INTO u VALUES (3,'x',9,9) ON CONFLICT (name) DO NOTHING",
		"INSERT INTO u VALUES (3,'z',1,2) ON CONFLICT (b, a) DO NOTHING",
		"INSERT INTO u VALUES (3,'x',9,9) ON CONFLICT DO NOTHING",
		"INSERT INTO u VALUES (3,'z',1,2) ON CONFLICT ON CONSTRAINT u_a_b_key DO NOTHING",
	} {
		if r := run(t, s, q); r.Tag != "INSERT 0 0" {
			t.Fatalf("%s: tag %s", q, r.Tag)
		}
	}
	if got := render(run(t, s, "INSERT INTO u VALUES (3,'x',9,9) ON CONFLICT (name) DO UPDATE SET n = u.n + 10, a = excluded.a RETURNING id, name, n, a")); got != "1,x,15,9" {
		t.Fatalf("do update: %s", got)
	}
	if got := render(run(t, s, "INSERT INTO u VALUES (4,'q',9,2) ON CONFLICT ON CONSTRAINT u_name_key DO UPDATE SET n = 0 RETURNING id")); got != "4" {
		t.Fatalf("no conflict inserts: %s", got)
	}
	// a violation of an index that is not the arbiter is still 23505
	_, err := tryRun(s, "INSERT INTO u VALUES (5,'x',7,7) ON CONFLICT (id) DO NOTHING")
	if sqlCode(err) != "23505" {
		t.Fatalf("want 23505, got %v", err)
	}
	// the same row twice in one command
	_, err = tryRun(s, "INSERT INTO u VALUES (6,'w',0,0),(7,'w',0,1) ON CONFLICT (name) DO UPDATE SET n = 1")
	if sqlCode(err) != "21000" {
		t.Fatalf("want 21000, got %v", err)
	}
	for q, want := range map[string]string{
		"INSERT INTO u VALUES (8,'v',0,5) ON CONFLICT DO UPDATE SET n = 1":             "42601",
		"INSERT INTO u VALUES (8,'v',0,5) ON CONFLICT (n) DO NOTHING":                  "42P10",
		"INSERT INTO u VALUES (8,'v',0,5) ON CONFLICT ON CONSTRAINT nosuch DO NOTHING": "42704",
	} {
		if _, err := tryRun(s, q); sqlCode(err) != want {
			t.Fatalf("%s: %v, want %s", q, err, want)
		}
	}
}

func TestUpdateDeleteOnly(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, "CREATE TABLE o (id int PRIMARY KEY, v int)")
	run(t, s, "INSERT INTO o VALUES (1,1),(2,2)")
	if r := run(t, s, "UPDATE ONLY o SET v = 9 WHERE id = 1"); r.Tag != "UPDATE 1" {
		t.Fatal(r.Tag)
	}
	if r := run(t, s, "DELETE FROM ONLY o WHERE id = 2"); r.Tag != "DELETE 1" {
		t.Fatal(r.Tag)
	}
}

func TestMergeUnreachableWhen(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, "CREATE TABLE tg (id int PRIMARY KEY, v int)")
	for _, q := range []string{
		"MERGE INTO tg USING (VALUES (1)) s(id) ON tg.id = s.id WHEN NOT MATCHED THEN INSERT VALUES (s.id, 1) WHEN NOT MATCHED THEN DO NOTHING",
		"MERGE INTO tg USING (VALUES (1)) s(id) ON tg.id = s.id WHEN MATCHED THEN DELETE WHEN MATCHED AND tg.v > 1 THEN DO NOTHING",
	} {
		if _, err := tryRun(s, q); sqlCode(err) != "42601" {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// a conditional clause first is fine
	run(t, s, "MERGE INTO tg USING (VALUES (1)) s(id) ON tg.id = s.id WHEN MATCHED AND tg.v > 1 THEN DELETE WHEN MATCHED THEN DO NOTHING WHEN NOT MATCHED THEN INSERT VALUES (s.id, 1)")
}

// MERGE waits for the writer of a matched row and works on its newest version (READ COMMITTED).
func TestMergeWaitsAndReevaluates(t *testing.T) {
	a, b, c := rowLockEngine(t)
	const upd = "MERGE INTO acct t USING (VALUES (1, 10), (9, 5)) s(id, d) ON t.id = s.id " +
		"WHEN MATCHED THEN UPDATE SET bal = t.bal + s.d WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.d, 'new')"
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 1")
	ch := blocked(t, b, upd, 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatalf("merge after the writer committed: %v", err)
	}
	for id, want := range map[int]string{1: "111", 9: "5"} { // 100 + 1 + 10; the new row
		if got := render(run(t, c, fmt.Sprintf("SELECT bal FROM acct WHERE id = %d", id))); got != want {
			t.Fatalf("id %d: balance %s, want %s", id, got, want)
		}
	}
	// a matched row deleted meanwhile is not matched any more: the NOT MATCHED clause inserts it
	run(t, a, "BEGIN")
	run(t, a, "DELETE FROM acct WHERE id = 2")
	ch = blocked(t, b, "MERGE INTO acct t USING (VALUES (2, 7)) s(id, d) ON t.id = s.id "+
		"WHEN MATCHED THEN UPDATE SET bal = t.bal + s.d WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.d, 'again')", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, c, "SELECT bal, st FROM acct WHERE id = 2")); got != "7,again" {
		t.Fatalf("got %s", got)
	}
	// WHEN MATCHED .. DELETE waits too, and the condition sees the new value
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 0 WHERE id = 3")
	ch = blocked(t, b, "MERGE INTO acct t USING (VALUES (3)) s(id) ON t.id = s.id WHEN MATCHED AND t.bal > 100 THEN DELETE", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, c, "SELECT count(*) FROM acct WHERE id = 3")); got != "1" {
		t.Fatalf("row 3 must survive (bal is 0 now): %s", got)
	}
}

func TestMergeRepeatableReadFails(t *testing.T) {
	a, b, _ := rowLockEngine(t)
	run(t, b, "BEGIN ISOLATION LEVEL REPEATABLE READ")
	run(t, b, "SELECT bal FROM acct WHERE id = 1")
	run(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 1")
	_, err := tryRun(b, "MERGE INTO acct t USING (VALUES (1, 10)) s(id, d) ON t.id = s.id WHEN MATCHED THEN UPDATE SET bal = t.bal + s.d")
	if sqlCode(err) != "40001" {
		t.Fatalf("err %v, want 40001", err)
	}
}

// Concurrent MERGE counters lose no increment and never fail.
func TestMergeConcurrentCounters(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	run(t, s0, "CREATE TABLE ctr (id int PRIMARY KEY, n int)")
	run(t, s0, "INSERT INTO ctr VALUES (1, 0), (2, 0)")
	const workers, iters = 4, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewSession(e)
			for i := 0; i < iters; i++ {
				if _, err := tryRun(s, "MERGE INTO ctr t USING (VALUES (1), (2)) s(id) ON t.id = s.id WHEN MATCHED THEN UPDATE SET n = t.n + 1"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := render(run(t, s0, "SELECT sum(n) FROM ctr")); got != fmt.Sprint(2*workers*iters) {
		t.Fatalf("lost updates: %s", got)
	}
}

func fieldsOf(t *testing.T, s *Session, q string) *Error {
	t.Helper()
	_, err := tryRun(s, q)
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("%s: no SQL error: %v", q, err)
	}
	return se
}

// Constraint errors carry the ErrorResponse fields drivers read (schema, table, column, constraint) and
// the offending key in DETAIL, not in the message.
func TestConstraintErrorFields(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, "CREATE SCHEMA sx")
	run(t, s, "CREATE TABLE sx.p (id int PRIMARY KEY, n int NOT NULL CHECK (n > 0), u text UNIQUE)")
	run(t, s, "CREATE TABLE sx.c (id int PRIMARY KEY, pid int, CONSTRAINT c_pid_fk FOREIGN KEY (pid) REFERENCES sx.p (id))")
	run(t, s, "INSERT INTO sx.p VALUES (1, 1, 'a')")

	e := fieldsOf(t, s, "INSERT INTO sx.p VALUES (1, 1, 'b')")
	if e.Code != "23505" || e.Msg != `duplicate key value violates unique constraint "p_pkey"` || e.Detail != "Key (id)=(1) already exists." ||
		e.Constraint != "p_pkey" || e.Table != "p" || e.Schema != "sx" {
		t.Fatalf("pk: %+v", e)
	}
	e = fieldsOf(t, s, "INSERT INTO sx.p VALUES (2, 1, 'a')")
	if e.Code != "23505" || e.Constraint != "p_u_key" || e.Detail != "Key (u)=(a) already exists." || e.Table != "p" {
		t.Fatalf("unique: %+v", e)
	}
	e = fieldsOf(t, s, "INSERT INTO sx.p VALUES (3, NULL, 'z')")
	if e.Code != "23502" || e.Column != "n" || e.Table != "p" || e.Schema != "sx" || !strings.HasPrefix(e.Detail, "Failing row contains (3, null, z)") {
		t.Fatalf("not null: %+v", e)
	}
	e = fieldsOf(t, s, "INSERT INTO sx.p VALUES (3, -1, 'z')")
	if e.Code != "23514" || e.Constraint != "p_n_check" || e.Table != "p" || e.Msg != `new row for relation "p" violates check constraint "p_n_check"` {
		t.Fatalf("check: %+v", e)
	}
	e = fieldsOf(t, s, "INSERT INTO sx.c VALUES (1, 99)")
	if e.Code != "23503" || e.Constraint != "c_pid_fk" || e.Table != "c" || e.Detail != `Key (pid)=(99) is not present in table "p".` ||
		e.Msg != `insert or update on table "c" violates foreign key constraint "c_pid_fk"` {
		t.Fatalf("fk insert: %+v", e)
	}
	run(t, s, "INSERT INTO sx.c VALUES (1, 1)")
	e = fieldsOf(t, s, "DELETE FROM sx.p WHERE id = 1")
	if e.Code != "23503" || e.Table != "p" || e.Constraint != "c_pid_fk" || e.Detail != `Key (id)=(1) is still referenced from table "c".` ||
		e.Msg != `update or delete on table "p" violates foreign key constraint "c_pid_fk" on table "c"` {
		t.Fatalf("fk delete: %+v", e)
	}
}

// errText is the message of err with its DETAIL and HINT, for tests that look for the key in the error.
func errText(err error) string {
	var se *Error
	if errors.As(err, &se) {
		return se.Msg + "\n" + se.Detail + "\n" + se.Hint
	}
	return err.Error()
}
