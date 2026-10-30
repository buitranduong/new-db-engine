package sql

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func seedN(t *testing.T, s *Session, n int) {
	t.Helper()
	run(t, s, "CREATE TABLE nums (id int PRIMARY KEY, v text, status text)")
	var sb strings.Builder
	sb.WriteString("INSERT INTO nums VALUES ")
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'v%d','free')", i, i)
	}
	run(t, s, sb.String())
}

func TestCursorBasics(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	seedN(t, s, 10)
	if _, err := tryRun(s, "DECLARE c CURSOR FOR SELECT id FROM nums"); code(err) != "25P01" {
		t.Fatalf("declare outside tx: %v", err)
	}
	run(t, s, "BEGIN")
	run(t, s, "DECLARE c SCROLL CURSOR FOR SELECT id, v FROM nums WHERE id > 2 ORDER BY id")
	if got := render(run(t, s, "FETCH 3 FROM c")); got != "3,v3;4,v4;5,v5" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH PRIOR FROM c")); got != "4,v4" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH LAST FROM c")); got != "10,v10" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH NEXT FROM c")); got != "" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH BACKWARD 2 FROM c")); got != "10,v10;9,v9" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH ABSOLUTE 2 FROM c")); got != "4,v4" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH RELATIVE -1 FROM c")); got != "3,v3" {
		t.Fatal(got)
	}
	if r := run(t, s, "MOVE FORWARD ALL IN c"); r.Tag != "MOVE 7" {
		t.Fatal(r.Tag)
	}
	if got := render(run(t, s, "SELECT name, is_scrollable FROM pg_cursors")); got != "c,t" {
		t.Fatal(got)
	}
	run(t, s, "CLOSE c")
	if _, err := tryRun(s, "FETCH 1 FROM c"); code(err) != "34000" {
		t.Fatal(err)
	}
	run(t, s, "DECLARE d NO SCROLL CURSOR FOR SELECT id FROM nums")
	if _, err := tryRun(s, "FETCH PRIOR FROM d"); code(err) != "55000" {
		t.Fatalf("no scroll: %v", err)
	}
	run(t, s, "ROLLBACK")
	if _, err := tryRun(s, "FETCH 1 FROM d"); code(err) != "34000" {
		t.Fatal("cursor survived rollback")
	}
}

func TestCursorHoldAndSnapshot(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	w := NewSession(e)
	seedN(t, s, 5000)
	run(t, s, "BEGIN")
	run(t, s, "DECLARE c CURSOR FOR SELECT id FROM nums")
	run(t, s, "DECLARE h CURSOR WITH HOLD FOR SELECT id FROM nums ORDER BY id")
	run(t, w, "DELETE FROM nums WHERE id <= 2500")
	run(t, s, "INSERT INTO nums VALUES (9999,'x','free')") // later own write: not seen by the cursor
	if got := run(t, s, "FETCH ALL FROM c"); len(got.Rows) != 5000 {
		t.Fatalf("snapshot: %d rows", len(got.Rows))
	}
	run(t, s, "FETCH 10 FROM h")
	run(t, s, "COMMIT")
	if _, err := tryRun(s, "FETCH 1 FROM c"); code(err) != "34000" {
		t.Fatal("plain cursor survived commit")
	}
	if got := render(run(t, s, "FETCH 2 FROM h")); got != "11;12" {
		t.Fatal(got)
	}
	run(t, s, "CLOSE ALL")
	// WITH HOLD outside a transaction block
	run(t, s, "DECLARE h2 CURSOR WITH HOLD FOR SELECT id FROM nums WHERE id > 4990 ORDER BY id")
	if got := render(run(t, s, "FETCH 2 FROM h2")); got != "4991;4992" {
		t.Fatal(got)
	}
}

func TestCursorStreamingWindow(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	n := 3*cursorWindow + 77
	seedN(t, s, n)
	run(t, s, "BEGIN")
	run(t, s, "DECLARE c SCROLL CURSOR FOR SELECT id FROM nums WHERE id % 2 = 1")
	cu := s.curs.byName["c"]
	if _, ok := cu.src.(*streamSource); !ok {
		t.Fatalf("not streaming: %T", cu.src)
	}
	seen := 0
	for {
		r := run(t, s, "FETCH 1000 FROM c")
		seen += len(r.Rows)
		if len(r.Rows) < 1000 {
			break
		}
	}
	if want := (n + 1) / 2; seen != want {
		t.Fatalf("saw %d want %d", seen, want)
	}
	if len(cu.win) > 2*cursorWindow {
		t.Fatalf("window %d", len(cu.win))
	}
	// backward beyond the window re-reads the snapshot; compare with a plain scan (same key order)
	all := run(t, s, "SELECT id FROM nums WHERE id % 2 = 1")
	if got := render(run(t, s, "FETCH ABSOLUTE 3 FROM c")); got != all.Rows[2][0].String() {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH LAST FROM c")); got != all.Rows[len(all.Rows)-1][0].String() {
		t.Fatal(got)
	}
	if got := render(run(t, s, "FETCH ABSOLUTE 5 FROM c")); got != all.Rows[4][0].String() {
		t.Fatal(got)
	}
	run(t, s, "ROLLBACK")
}

func TestForUpdateConflict(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	seedN(t, a, 5)
	run(t, a, "BEGIN")
	run(t, a, "SELECT * FROM nums WHERE id = 1 FOR UPDATE")
	// b writes the row and commits: a's commit must fail
	run(t, b, "UPDATE nums SET v = 'changed' WHERE id = 1")
	if _, err := tryRun(a, "COMMIT"); code(err) != "40001" {
		t.Fatalf("commit after concurrent write: %v", err)
	}
	// NOWAIT
	run(t, a, "BEGIN")
	run(t, a, "SELECT * FROM nums WHERE id = 2 FOR UPDATE")
	run(t, b, "BEGIN")
	if _, err := tryRun(b, "SELECT * FROM nums WHERE id = 2 FOR UPDATE NOWAIT"); code(err) != "55P03" {
		t.Fatalf("nowait: %v", err)
	}
	run(t, b, "ROLLBACK")
	// SKIP LOCKED skips the locked row and is not counted against LIMIT
	run(t, b, "BEGIN")
	got := render(run(t, b, "SELECT id FROM nums ORDER BY id LIMIT 2 FOR UPDATE SKIP LOCKED"))
	if got != "1;3" {
		t.Fatalf("skip locked: %q", got)
	}
	run(t, b, "ROLLBACK")
	run(t, a, "ROLLBACK")
	// FOR SHARE does not conflict with FOR SHARE, does with FOR UPDATE
	run(t, a, "BEGIN")
	run(t, b, "BEGIN")
	run(t, a, "SELECT * FROM nums WHERE id = 3 FOR SHARE")
	run(t, b, "SELECT * FROM nums WHERE id = 3 FOR SHARE NOWAIT")
	if _, err := tryRun(b, "SELECT * FROM nums WHERE id = 3 FOR UPDATE NOWAIT"); code(err) != "55P03" {
		t.Fatal(err)
	}
	run(t, a, "ROLLBACK")
	run(t, b, "ROLLBACK")
}

func TestForUpdateWaits(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	seedN(t, a, 3)
	run(t, a, "BEGIN")
	run(t, a, "SELECT * FROM nums WHERE id = 1 FOR UPDATE")
	done := make(chan error, 1)
	go func() {
		// autocommit: waits, then sees the new row version
		_, err := tryRun(b, "SELECT v FROM nums WHERE id = 1 FOR UPDATE")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("did not wait: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	run(t, a, "UPDATE nums SET v = 'new' WHERE id = 1")
	run(t, a, "COMMIT")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never woke")
	}
	// lock_timeout
	run(t, a, "BEGIN")
	run(t, a, "SELECT * FROM nums WHERE id = 2 FOR UPDATE")
	run(t, b, "SET lock_timeout = '100ms'")
	t0 := time.Now()
	if _, err := tryRun(b, "SELECT * FROM nums WHERE id = 2 FOR UPDATE"); code(err) != "55P03" {
		t.Fatalf("lock_timeout: %v", err)
	}
	if d := time.Since(t0); d < 90*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	run(t, a, "ROLLBACK")
}

func TestDeadlock(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	seedN(t, a, 3)
	run(t, a, "BEGIN")
	run(t, b, "BEGIN")
	run(t, a, "SELECT * FROM nums WHERE id = 1 FOR UPDATE")
	run(t, b, "SELECT * FROM nums WHERE id = 2 FOR UPDATE")
	errs := make(chan error, 2)
	go func() { _, err := tryRun(a, "SELECT * FROM nums WHERE id = 2 FOR UPDATE"); errs <- err }()
	time.Sleep(100 * time.Millisecond)
	_, err := tryRun(b, "SELECT * FROM nums WHERE id = 1 FOR UPDATE")
	if code(err) != "40P01" {
		t.Fatalf("deadlock: %v", err)
	}
	run(t, b, "ROLLBACK")
	if err := <-errs; err != nil {
		t.Fatalf("survivor: %v", err)
	}
	run(t, a, "ROLLBACK")
}

// Property: many sessions each take N free rows with FOR UPDATE SKIP LOCKED
// and mark them; no row is taken twice and none is lost.
func TestSkipLockedNoDuplicates(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	total := 400
	seedN(t, s0, total)
	var mu sync.Mutex
	taken := map[string]int{}
	var conflicts atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(e)
			defer s.Close()
			for {
				res, err := tryRun(s, "BEGIN")
				_ = res
				if err != nil {
					t.Error(err)
					return
				}
				r, err := tryRun(s, "SELECT id FROM nums WHERE status = 'free' ORDER BY id LIMIT 5 FOR UPDATE SKIP LOCKED")
				if err != nil {
					t.Errorf("select: %v", err)
					return
				}
				if len(r.Rows) == 0 {
					tryRun(s, "ROLLBACK")
					return
				}
				var ids []string
				for _, row := range r.Rows {
					ids = append(ids, row[0].String())
				}
				if _, err := tryRun(s, "UPDATE nums SET status = 'taken' WHERE id IN ("+strings.Join(ids, ",")+")"); err != nil {
					t.Errorf("update: %v", err)
					return
				}
				if _, err := tryRun(s, "COMMIT"); err != nil {
					if code(err) == "40001" {
						conflicts.Add(1)
						continue
					}
					t.Errorf("commit: %v", err)
					return
				}
				mu.Lock()
				for _, id := range ids {
					taken[id]++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	for id, n := range taken {
		if n != 1 {
			t.Fatalf("row %s taken %d times", id, n)
		}
	}
	if len(taken) != total {
		t.Fatalf("took %d of %d rows (%d commit conflicts)", len(taken), total, conflicts.Load())
	}
	t.Logf("commit conflicts: %d", conflicts.Load())
	if n := locksOf(e).held; n != 0 {
		t.Fatalf("%d locks leaked", n)
	}
}

func TestAdvisoryLocks(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	run(t, a, "SELECT pg_advisory_lock(42)")
	if got := render(run(t, b, "SELECT pg_try_advisory_lock(42)")); got != "f" {
		t.Fatal(got)
	}
	if got := render(run(t, b, "SELECT pg_try_advisory_lock(1, 2)")); got != "t" {
		t.Fatal(got)
	}
	// same session: re-entrant, needs two unlocks
	run(t, a, "SELECT pg_advisory_lock(42)")
	if got := render(run(t, a, "SELECT pg_advisory_unlock(42)")); got != "t" {
		t.Fatal(got)
	}
	if got := render(run(t, b, "SELECT pg_try_advisory_lock(42)")); got != "f" {
		t.Fatal("released after one unlock")
	}
	run(t, a, "SELECT pg_advisory_unlock(42)")
	if got := render(run(t, a, "SELECT pg_advisory_unlock(42)")); got != "f" {
		t.Fatal(got)
	}
	if got := render(run(t, b, "SELECT pg_try_advisory_lock(42)")); got != "t" {
		t.Fatal(got)
	}
	run(t, b, "SELECT pg_advisory_unlock_all()")
	// blocking, released by session close
	run(t, a, "SELECT pg_advisory_lock(7)")
	done := make(chan error, 1)
	go func() { _, err := tryRun(b, "SELECT pg_advisory_lock(7)"); done <- err }()
	select {
	case <-done:
		t.Fatal("did not block")
	case <-time.After(100 * time.Millisecond):
	}
	a.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not released on close")
	}
	// transaction-level
	c := NewSession(e)
	run(t, c, "BEGIN")
	run(t, c, "SELECT pg_advisory_xact_lock_shared(9)")
	if got := render(run(t, b, "SELECT pg_try_advisory_lock_shared(9)")); got != "t" {
		t.Fatal(got)
	}
	if got := render(run(t, b, "SELECT pg_try_advisory_lock(9)")); got != "f" {
		t.Fatal(got)
	}
	run(t, c, "COMMIT")
	run(t, b, "SELECT pg_advisory_unlock_shared(9)")
	if got := render(run(t, b, "SELECT pg_try_advisory_lock(9)")); got != "t" {
		t.Fatal(got)
	}
	// statement_timeout cancels a wait
	run(t, c, "SET statement_timeout = '150ms'")
	if _, err := tryRun(c, "SELECT pg_advisory_lock(9)"); code(err) != "57014" {
		t.Fatalf("timeout: %v", err)
	}
}

func TestLockTable(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	seedN(t, a, 2)
	if _, err := tryRun(a, "LOCK TABLE nums"); code(err) != "25P01" {
		t.Fatalf("outside tx: %v", err)
	}
	run(t, a, "BEGIN")
	run(t, b, "BEGIN")
	run(t, a, "LOCK TABLE nums IN ROW EXCLUSIVE MODE")
	run(t, b, "LOCK TABLE nums IN ROW SHARE MODE NOWAIT")
	if _, err := tryRun(b, "LOCK TABLE nums IN SHARE MODE NOWAIT"); code(err) != "55P03" {
		t.Fatal(err)
	}
	run(t, b, "ROLLBACK")
	run(t, a, "ROLLBACK")
	for _, m := range []string{"ACCESS SHARE", "ROW SHARE", "ROW EXCLUSIVE", "SHARE UPDATE EXCLUSIVE", "SHARE", "SHARE ROW EXCLUSIVE", "EXCLUSIVE", "ACCESS EXCLUSIVE"} {
		run(t, a, "BEGIN")
		run(t, a, "LOCK TABLE nums IN "+m+" MODE")
		run(t, a, "COMMIT")
	}
}

func TestSetRoleAndUsers(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	s.Track("app", "x", "", 0)
	if got := render(run(t, s, "SELECT current_user, session_user")); got != "app,app" {
		t.Fatal(got)
	}
	run(t, s, "CREATE ROLE bob LOGIN")
	run(t, s, "SET ROLE bob")
	if got := render(run(t, s, "SELECT current_user, session_user, current_role")); got != "bob,app,bob" {
		t.Fatal(got)
	}
	if got := render(run(t, s, "SHOW role")); got != "bob" {
		t.Fatal(got)
	}
	run(t, s, "RESET ROLE")
	if got := render(run(t, s, "SELECT current_user")); got != "app" {
		t.Fatal(got)
	}
	if _, err := tryRun(s, "SET ROLE nobody"); code(err) != "22023" {
		t.Fatal(err)
	}
	run(t, s, "BEGIN")
	run(t, s, "SET LOCAL ROLE bob")
	run(t, s, "COMMIT")
	if got := render(run(t, s, "SELECT current_user")); got != "app" {
		t.Fatalf("SET LOCAL leaked: %s", got)
	}
	run(t, s, "SET SESSION AUTHORIZATION bob")
	if got := render(run(t, s, "SELECT current_user, session_user")); got != "bob,bob" {
		t.Fatal(got)
	}
	run(t, s, "RESET SESSION AUTHORIZATION")
	if got := render(run(t, s, "SELECT session_user")); got != "app" {
		t.Fatal(got)
	}
	// a limited role cannot SET ROLE to someone else
	t2 := NewSession(e)
	t2.SetRole("bob")
	t2.Track("bob", "", "", 0)
	if _, err := tryRun(t2, "SET ROLE stask"); code(err) != "42501" {
		t.Fatalf("escalation: %v", err)
	}
	run(t, t2, "SET ROLE bob")
	if got := run(t, s, "SHOW ALL"); len(got.Rows) < 10 || len(got.Cols) != 3 {
		t.Fatalf("show all: %d rows", len(got.Rows))
	}
	run(t, s, "DISCARD ALL")
}

func TestLimitedRoleCursor(t *testing.T) {
	e := newEngine(t)
	s := NewSession(e)
	seedN(t, s, 3)
	run(t, s, "CREATE ROLE ro LOGIN")
	run(t, s, "GRANT SELECT ON nums TO ro")
	r := NewSession(e)
	r.SetRole("ro")
	run(t, r, "BEGIN")
	run(t, r, "DECLARE c CURSOR FOR SELECT id FROM nums")
	if got := render(run(t, r, "FETCH ALL FROM c")); got != "1;2;3" {
		t.Fatal(got)
	}
	run(t, r, "ROLLBACK")
	run(t, r, "BEGIN")
	run(t, r, "SELECT * FROM nums WHERE id = 1") // read ok
	run(t, r, "ROLLBACK")
}

// The pattern of the SIM warehouse: take n free numbers for an order in one statement.
func TestSkipLockedUpdateSubselect(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	total := 300
	seedN(t, s0, total)
	var mu sync.Mutex
	taken := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(e)
			defer s.Close()
			for {
				r, err := tryRun(s, "UPDATE nums SET status = 'taken', v = 'w"+fmt.Sprint(w)+"' WHERE id IN (SELECT id FROM nums WHERE status = 'free' ORDER BY id LIMIT 4 FOR UPDATE SKIP LOCKED) RETURNING id")
				if err != nil {
					t.Errorf("update: %v", err)
					return
				}
				if len(r.Rows) == 0 {
					return
				}
				mu.Lock()
				for _, row := range r.Rows {
					taken[row[0].String()]++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	for id, n := range taken {
		if n != 1 {
			t.Fatalf("row %s updated %d times", id, n)
		}
	}
	if len(taken) != total {
		t.Fatalf("took %d of %d", len(taken), total)
	}
	got := run(t, s0, "SELECT count(*) FROM nums WHERE status = 'free'")
	if render(got) != "0" {
		t.Fatalf("free left: %s", render(got))
	}
}

// Many transactions racing for the same rows with plain FOR UPDATE (waiting): none is lost.
func TestForUpdateWaitingCounter(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	run(t, s0, "CREATE TABLE ctr (id int PRIMARY KEY, n int)")
	run(t, s0, "INSERT INTO ctr VALUES (1, 0)")
	var wg sync.WaitGroup
	const workers, per = 6, 20
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewSession(e)
			defer s.Close()
			for i := 0; i < per; i++ {
				for {
					run(t, s, "BEGIN")
					r, err := tryRun(s, "SELECT n FROM ctr WHERE id = 1 FOR UPDATE")
					if err == nil {
						n := r.Rows[0][0].I
						_, err = tryRun(s, fmt.Sprintf("UPDATE ctr SET n = %d WHERE id = 1", n+1))
					}
					if err == nil {
						_, err = tryRun(s, "COMMIT")
					} else {
						tryRun(s, "ROLLBACK")
					}
					if err == nil {
						break
					}
					if c := code(err); c != "40001" {
						t.Errorf("unexpected %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if got := render(run(t, s0, "SELECT n FROM ctr")); got != fmt.Sprint(workers*per) {
		t.Fatalf("counter %s, want %d (lost updates)", got, workers*per)
	}
	if n := locksOf(e).held; n != 0 {
		t.Fatalf("%d locks leaked", n)
	}
}

func TestSkipLockedInSubquery(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	seedN(t, a, 6)
	run(t, a, "BEGIN")
	run(t, a, "SELECT id FROM nums WHERE id IN (1, 2) FOR UPDATE")
	r := run(t, b, "UPDATE nums SET status = 'taken' WHERE id IN (SELECT id FROM nums ORDER BY id LIMIT 3 FOR UPDATE SKIP LOCKED) RETURNING id")
	got := map[string]bool{}
	for _, row := range r.Rows {
		got[row[0].String()] = true
	}
	if len(got) != 3 || got["1"] || got["2"] {
		t.Fatalf("locked rows not skipped: %v", got)
	}
	run(t, a, "ROLLBACK")
	// join: lock only the table named in OF
	run(t, a, "CREATE TABLE owners (id int PRIMARY KEY, name text)")
	run(t, a, "INSERT INTO owners VALUES (1, 'x'), (2, 'y')")
	run(t, a, "BEGIN")
	run(t, a, "SELECT id FROM owners WHERE id = 1 FOR UPDATE")
	run(t, b, "BEGIN")
	got2 := render(run(t, b, "SELECT n.id, o.name FROM nums n JOIN owners o ON o.id = n.id ORDER BY n.id FOR UPDATE OF n NOWAIT"))
	if got2 != "1,x;2,y" {
		t.Fatal(got2)
	}
	if _, err := tryRun(b, "SELECT n.id FROM nums n JOIN owners o ON o.id = n.id ORDER BY n.id FOR UPDATE OF o NOWAIT"); code(err) != "55P03" {
		t.Fatalf("join lock: %v", err)
	}
	run(t, b, "ROLLBACK")
	run(t, a, "ROLLBACK")
}
