package sql

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Row-lock waiting of UPDATE / DELETE with READ COMMITTED re-evaluation
// (writelock.go). Each test drives two or more sessions like two clients.

// blocked runs sql on s in the background and fails the test if it returns
// within d; the result comes through the returned channel.
func blocked(t *testing.T, s *Session, sql string, d time.Duration) chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() {
		_, err := tryRun(s, sql)
		ch <- err
	}()
	select {
	case err := <-ch:
		t.Fatalf("%q did not wait (err=%v)", sql, err)
	case <-time.After(d):
	}
	return ch
}

func waitDone(t *testing.T, ch chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never woke")
	}
	return nil
}

func rowLockEngine(t *testing.T) (*Session, *Session, *Session) {
	e := newEngine(t)
	a, b, c := NewSession(e), NewSession(e), NewSession(e)
	run(t, a, "CREATE TABLE acct (id int PRIMARY KEY, bal int, st text)")
	run(t, a, "INSERT INTO acct VALUES (1,100,'open'),(2,200,'open'),(3,300,'open')")
	return a, b, c
}

func TestUpdateWaitsAndReevaluates(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 1")
	run(t, b, "BEGIN")
	ch := blocked(t, b, "UPDATE acct SET bal = bal + 10 WHERE id = 1", 150*time.Millisecond)
	// a plain read does not wait and sees the committed value
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 1")); got != "100" {
		t.Fatalf("reader: %s", got)
	}
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	// b computed bal + 10 on a's committed 101, not on 100 (no lost update)
	if err := func() error { _, err := tryRun(b, "COMMIT"); return err }(); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 1")); got != "111" {
		t.Fatalf("lost update: %s", got)
	}
}

func TestUpdateWhereReevaluated(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET st = 'closed' WHERE id = 2")
	run(t, b, "BEGIN")
	ch := blocked(t, b, "UPDATE acct SET bal = 0 WHERE st = 'open' AND id IN (1,2)", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	run(t, b, "COMMIT")
	// row 2 no longer matched after the wait; row 1 did
	if got := render(run(t, c, "SELECT id, bal FROM acct ORDER BY id")); got != "1,0;2,200;3,300" {
		t.Fatalf("got %s", got)
	}
}

func TestDeleteWaitsAfterDelete(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "DELETE FROM acct WHERE id = 3")
	run(t, b, "BEGIN")
	ch := blocked(t, b, "UPDATE acct SET bal = 1 WHERE id = 3", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	run(t, b, "COMMIT")
	if got := render(run(t, c, "SELECT count(*) FROM acct")); got != "2" {
		t.Fatalf("got %s", got)
	}
}

func TestUpdateWaitsRollback(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 5 WHERE id = 1")
	ch := blocked(t, b, "UPDATE acct SET bal = bal + 1 WHERE id = 1", 150*time.Millisecond) // autocommit
	run(t, a, "ROLLBACK")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 1")); got != "101" {
		t.Fatalf("got %s", got)
	}
}

func TestUpdateWaitsForSelectForUpdate(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "SELECT * FROM acct WHERE id = 2 FOR UPDATE")
	ch := blocked(t, b, "DELETE FROM acct WHERE id = 2", 150*time.Millisecond)
	// FOR KEY SHARE and plain reads pass; an UPDATE of another row passes
	run(t, c, "UPDATE acct SET bal = 1 WHERE id = 1")
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	// FOR SHARE blocks a writer, two FOR SHARE do not block each other
	run(t, a, "BEGIN")
	run(t, a, "SELECT * FROM acct WHERE id = 1 FOR SHARE")
	run(t, c, "BEGIN")
	run(t, c, "SELECT * FROM acct WHERE id = 1 FOR SHARE")
	ch = blocked(t, b, "UPDATE acct SET bal = 2 WHERE id = 1", 100*time.Millisecond)
	run(t, a, "COMMIT")
	select {
	case err := <-ch:
		t.Fatalf("woke while the other FOR SHARE is held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	run(t, c, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatableReadWriterGets40001(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, b, "BEGIN ISOLATION LEVEL REPEATABLE READ")
	run(t, b, "SELECT 1")
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 7 WHERE id = 1")
	ch := blocked(t, b, "UPDATE acct SET bal = 8 WHERE id = 1", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); code(err) != "40001" {
		t.Fatalf("want 40001, got %v", err)
	}
	run(t, b, "ROLLBACK")
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 1")); got != "7" {
		t.Fatal(got)
	}
}

func TestUpdateDeadlock(t *testing.T) {
	a, b, _ := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, b, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 1 WHERE id = 1")
	run(t, b, "UPDATE acct SET bal = 2 WHERE id = 2")
	ch := blocked(t, a, "UPDATE acct SET bal = 3 WHERE id = 2", 100*time.Millisecond)
	_, err := tryRun(b, "UPDATE acct SET bal = 4 WHERE id = 1")
	if code(err) != "40P01" {
		t.Fatalf("want 40P01, got %v", err)
	}
	run(t, b, "ROLLBACK")
	if err := waitDone(t, ch); err != nil {
		t.Fatalf("survivor: %v", err)
	}
	run(t, a, "COMMIT")
}

func TestUpdateLockTimeout(t *testing.T) {
	a, b, _ := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 1 WHERE id = 1")
	run(t, b, "SET lock_timeout = '100ms'")
	t0 := time.Now()
	_, err := tryRun(b, "UPDATE acct SET bal = 2 WHERE id = 1")
	if code(err) != "55P03" {
		t.Fatalf("want 55P03, got %v", err)
	}
	if d := time.Since(t0); d < 90*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	run(t, a, "ROLLBACK")
	if n := locksOf(a.E).held; n != 0 {
		t.Fatalf("%d locks leaked", n)
	}
}

func TestUpdateGeneralPathWaits(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 1")
	// a subquery in WHERE takes the general executor
	ch := blocked(t, b, "UPDATE acct SET bal = bal * 2 WHERE id IN (SELECT 1)", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 1")); got != "202" {
		t.Fatalf("got %s", got)
	}
}

func TestSelectForUpdateReevaluates(t *testing.T) {
	a, b, _ := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = 555 WHERE id = 1")
	run(t, b, "BEGIN")
	done := make(chan *Result, 1)
	errs := make(chan error, 1)
	go func() {
		r, err := tryRun(b, "SELECT bal FROM acct WHERE id = 1 FOR UPDATE")
		errs <- err
		done <- r
	}()
	time.Sleep(150 * time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, errs); err != nil { // used to be 40001
		t.Fatal(err)
	}
	if got := render(<-done); got != "555" {
		t.Fatalf("FOR UPDATE saw the stale row: %s", got)
	}
	run(t, b, "COMMIT")
}

// Counter incremented by many sessions, in autocommit and in READ COMMITTED
// blocks: nothing is lost and no client sees 40001.
func TestConcurrentIncrementNoLostUpdate(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	run(t, s0, "CREATE TABLE ctr (id int PRIMARY KEY, n int)")
	run(t, s0, "INSERT INTO ctr VALUES (1,0),(2,0)")
	const workers, per = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(e)
			defer s.Close()
			for i := 0; i < per; i++ {
				var err error
				if w%2 == 0 {
					_, err = tryRun(s, "UPDATE ctr SET n = n + 1 WHERE id = 1")
				} else {
					run(t, s, "BEGIN")
					_, err = tryRun(s, "UPDATE ctr SET n = n + 1 WHERE id = 1")
					if err == nil {
						_, err = tryRun(s, "UPDATE ctr SET n = n + 1 WHERE id = 2")
					}
					if err == nil {
						_, err = tryRun(s, "COMMIT")
					} else {
						tryRun(s, "ROLLBACK")
					}
				}
				if err != nil {
					t.Errorf("worker %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	want := fmt.Sprintf("%d,%d", workers*per, workers/2*per)
	if got := render(run(t, s0, "SELECT n FROM ctr WHERE id = 1")) + "," + render(run(t, s0, "SELECT n FROM ctr WHERE id = 2")); got != want {
		t.Fatalf("counters %s, want %s", got, want)
	}
	if n := locksOf(e).held; n != 0 {
		t.Fatalf("%d locks leaked", n)
	}
}

// A streaming cursor keeps its own snapshot while the READ COMMITTED
// transaction's moves with each statement.
func TestCursorSnapshotRefresh(t *testing.T) {
	e := newEngine(t)
	s, w := NewSession(e), NewSession(e)
	seedN(t, s, 10)
	run(t, s, "BEGIN")
	run(t, s, "DECLARE c CURSOR FOR SELECT id FROM nums")
	run(t, w, "INSERT INTO nums VALUES (100,'x','free')")
	// the statement after DECLARE sees the committed insert (it used to keep the old snapshot)
	if got := render(run(t, s, "SELECT count(*) FROM nums")); got != "11" {
		t.Fatalf("transaction snapshot stayed put: %s", got)
	}
	// the cursor still reads the snapshot it was declared on
	if got := run(t, s, "FETCH ALL FROM c"); len(got.Rows) != 10 {
		t.Fatalf("cursor snapshot moved: %d rows", len(got.Rows))
	}
	run(t, s, "CLOSE c")
	run(t, s, "COMMIT")
}

// INSERT .. ON CONFLICT DO UPDATE waits for the writer of the conflicting
// row and updates its newest version (PostgreSQL READ COMMITTED: the
// conflicting row is locked, no serialization failure).
func TestUpsertWaitsAndReevaluates(t *testing.T) {
	a, b, c := rowLockEngine(t)
	run(t, a, "BEGIN")
	run(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 1")
	ch := blocked(t, b, "INSERT INTO acct VALUES (1, 10, 'x'), (9, 5, 'new') ON CONFLICT (id) DO UPDATE SET bal = acct.bal + excluded.bal", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatalf("upsert after the writer committed: %v", err)
	}
	for id, want := range map[int]string{1: "111", 9: "5"} { // 100 + 1 + 10; the new row
		if got := render(run(t, c, fmt.Sprintf("SELECT bal FROM acct WHERE id = %d", id))); got != want {
			t.Fatalf("id %d: balance %s, want %s", id, got, want)
		}
	}
	// the row was deleted while the upsert waited: it inserts
	run(t, a, "BEGIN")
	run(t, a, "DELETE FROM acct WHERE id = 2")
	ch = blocked(t, b, "INSERT INTO acct SELECT 2, 7, 'again' ON CONFLICT (id) DO UPDATE SET bal = acct.bal + excluded.bal", 150*time.Millisecond)
	run(t, a, "COMMIT")
	if err := waitDone(t, ch); err != nil {
		t.Fatal(err)
	}
	if got := render(run(t, c, "SELECT bal FROM acct WHERE id = 2")); got != "7" {
		t.Fatalf("balance %s, want 7", got)
	}
}

// REPEATABLE READ keeps the serialization failure, as PostgreSQL.
func TestUpsertRepeatableReadFails(t *testing.T) {
	a, b, _ := rowLockEngine(t)
	run(t, b, "BEGIN ISOLATION LEVEL REPEATABLE READ")
	run(t, b, "SELECT bal FROM acct WHERE id = 1")
	run(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 1")
	_, err := tryRun(b, "INSERT INTO acct VALUES (1, 10, 'x') ON CONFLICT (id) DO UPDATE SET bal = acct.bal + excluded.bal")
	if sqlCode(err) != "40001" {
		t.Fatalf("err %v, want 40001", err)
	}
}

// Concurrent upserts of the same rows lose no increment and never fail.
func TestUpsertConcurrentCounters(t *testing.T) {
	a, _, _ := rowLockEngine(t)
	e := a.E
	const workers, iters = 6, 40
	var wg sync.WaitGroup
	errs := make(chan error, workers*iters)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(e)
			for i := 0; i < iters; i++ {
				q := "INSERT INTO acct VALUES (1, 1, 'a'), (2, 1, 'a'), (50, 1, 'a') ON CONFLICT (id) DO UPDATE SET bal = acct.bal + excluded.bal"
				if w%2 == 1 { // reverse order and inside a READ COMMITTED block
					q = "INSERT INTO acct VALUES (50, 1, 'a'), (2, 1, 'a'), (1, 1, 'a') ON CONFLICT (id) DO UPDATE SET bal = acct.bal + excluded.bal"
					if _, err := tryRun(s, "BEGIN"); err != nil {
						errs <- err
						return
					}
				}
				_, err := tryRun(s, q)
				if w%2 == 1 {
					if err == nil {
						_, err = tryRun(s, "COMMIT")
					} else {
						tryRun(s, "ROLLBACK")
					}
				}
				if err != nil && sqlCode(err) != "40P01" { // a real deadlock may be reported, retried by clients
					errs <- err
					return
				}
				if err != nil {
					i-- // retry the deadlock victim
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for id, want := range map[int]string{1: "340", 2: "440", 50: "240"} {
		if got := render(run(t, a, fmt.Sprintf("SELECT bal FROM acct WHERE id = %d", id))); got != want {
			t.Fatalf("id %d: %s, want %s", id, got, want)
		}
	}
}

// locklessWrite runs f with the row locks of writes off: it stands in for a
// writer that takes none (INSERT, MERGE, COPY ...), to test the commit-time
// conflict check against a transaction that holds a row lock.
func locklessWrite(f func()) {
	old := writeLocksOff
	writeLocksOff = true
	defer func() { writeLocksOff = old }()
	f()
}
