package sql

import (
	"testing"
	"time"
)

func TestTwoPhaseCommitAndRollback(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 5 WHERE id = 1")
	run(t, a, "INSERT INTO acct VALUES (9, 9, 'new')")
	if r := run(t, a, "PREPARE TRANSACTION 'g1'"); r.Tag != "PREPARE TRANSACTION" {
		t.Fatal(r.Tag)
	}
	// the session is out of the transaction, and nothing is visible yet
	if r := run(t, a, "SELECT 1"); r.Tag != "SELECT 1" {
		t.Fatal(r.Tag)
	}
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 1")); got != "100" {
		t.Fatalf("prepared write visible: %s", got)
	}
	if got := render(run(t, c, "SELECT count(*) FROM pg_prepared_xacts")); got != "1" {
		t.Fatal(got)
	}
	// a conflicting writer waits for the prepared transaction (its row locks stay held)
	ch := blocked(t, b, "UPDATE acct SET bal = bal + 1 WHERE id = 1", 150*time.Millisecond)
	// another session finishes it
	if r := run(t, c, "COMMIT PREPARED 'g1'"); r.Tag != "COMMIT PREPARED" {
		t.Fatal(r.Tag)
	}
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 1")); got != "6" { // 5 from the prepared transaction, +1 after it
		t.Fatalf("bal %s", got)
	}
	if got := render(run(t, c, "SELECT count(*) FROM pg_prepared_xacts")); got != "0" {
		t.Fatal(got)
	}

	run(t, a, "BEGIN")
	run(t, a, "DELETE FROM acct WHERE id = 2")
	run(t, a, "PREPARE TRANSACTION 'g2'")
	run(t, b, "ROLLBACK PREPARED 'g2'")
	if got := render(run(t, c, "SELECT count(*) FROM acct WHERE id = 2")); got != "1" {
		t.Fatalf("rolled back delete: %s", got)
	}
}

func TestTwoPhaseErrors(t *testing.T) {
	a, b, _ := rowLockEngine(t)
	if r := run(t, a, "PREPARE TRANSACTION 'x'"); r.Tag != "ROLLBACK" {
		t.Fatalf("outside a transaction: %s", r.Tag)
	}
	for q, want := range map[string]string{
		"COMMIT PREPARED 'nosuch'":   "42704",
		"ROLLBACK PREPARED 'nosuch'": "42704",
	} {
		if _, err := tryRun(a, q); sqlCode(err) != want {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 1 WHERE id = 3")
	run(t, a, "PREPARE TRANSACTION 'dup'")
	run(t, b, "BEGIN")
	run(t, b, "UPDATE acct SET bal = 1 WHERE id = 2")
	if _, err := tryRun(b, "PREPARE TRANSACTION 'dup'"); sqlCode(err) != "42710" {
		t.Fatalf("duplicate gid: %v", err)
	}
	run(t, b, "ROLLBACK")
	run(t, b, "BEGIN")
	if _, err := tryRun(b, "COMMIT PREPARED 'dup'"); sqlCode(err) != "25001" {
		t.Fatalf("inside a block: %v", err)
	}
	run(t, b, "ROLLBACK")
	// a failed transaction prepares as a ROLLBACK
	run(t, b, "BEGIN")
	if _, err := tryRun(b, "SELECT 1/0"); err == nil {
		t.Fatal("no error")
	}
	if r := run(t, b, "PREPARE TRANSACTION 'failed'"); r.Tag != "ROLLBACK" {
		t.Fatal(r.Tag)
	}
	run(t, b, "BEGIN")
	run(t, b, "LISTEN ch")
	if _, err := tryRun(b, "PREPARE TRANSACTION 'ln'"); sqlCode(err) != "0A000" {
		t.Fatalf("listen: %v", err)
	}
	run(t, b, "ROLLBACK")
	run(t, a, "ROLLBACK PREPARED 'dup'")
}
