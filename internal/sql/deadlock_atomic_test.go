package sql

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// B2 hunt (docs/14 row B2H): a transaction that lost a deadlock (40P01) must leave nothing behind, even when a sloppy
// client keeps sending statements and COMMIT. PostgreSQL answers every later statement with 25P02 and turns COMMIT into
// ROLLBACK.

func TestDeadlockVictimLeavesNothing(t *testing.T) {
	for round := 0; round < 40; round++ {
		e := newEngine(t)
		a, b, c := NewSession(e), NewSession(e), NewSession(e)
		run(t, a, "CREATE TABLE acct (id int PRIMARY KEY, bal int)")
		run(t, a, "CREATE TABLE hist (id int PRIMARY KEY, who text)")
		run(t, a, "CREATE TABLE jr (id int PRIMARY KEY, who text)")
		run(t, a, "INSERT INTO acct VALUES (1,100),(2,200),(3,300)")
		// A: acct 1 then acct 2 (+ rows); B: acct 2 then acct 1 (+ rows).
		run(t, a, "BEGIN")
		run(t, b, "BEGIN")
		if round%2 == 1 {
			run(t, a, "SAVEPOINT s1")
		}
		run(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 1")
		run(t, a, "INSERT INTO hist VALUES (1,'a')")
		run(t, b, "UPDATE acct SET bal = bal + 10 WHERE id = 2")
		run(t, b, "INSERT INTO hist VALUES (2,'b')")
		run(t, b, "UPDATE acct SET bal = bal + 10 WHERE id = 3")
		ch := blocked(t, a, "UPDATE acct SET bal = bal + 1 WHERE id = 2", 100*time.Millisecond)
		_, err := tryRun(b, "UPDATE acct SET bal = bal + 10 WHERE id = 1")
		if code(err) != "40P01" {
			t.Fatalf("round %d: want deadlock, got %v", round, err)
		}
		// the sloppy client goes on
		for _, q := range []string{"INSERT INTO jr VALUES (2,'b')", "UPDATE acct SET bal = 0 WHERE id = 3", "SELECT 1"} {
			if _, err := tryRun(b, q); code(err) != "25P02" {
				t.Fatalf("round %d: %q after the deadlock: want 25P02, got %v", round, q, err)
			}
		}
		res, err := tryRun(b, "COMMIT")
		if err != nil || res.Tag != "ROLLBACK" {
			t.Fatalf("round %d: COMMIT of an aborted transaction: %v %v", round, res, err)
		}
		if err := waitDone(t, ch); err != nil {
			t.Fatalf("round %d: survivor: %v", round, err)
		}
		run(t, a, "INSERT INTO jr VALUES (1,'a')")
		run(t, a, "COMMIT")
		got := render(run(t, c, "SELECT id, bal FROM acct ORDER BY id")) + "|" + render(run(t, c, "SELECT id, who FROM hist ORDER BY id")) +
			"|" + render(run(t, c, "SELECT id, who FROM jr ORDER BY id"))
		if want := "1,101;2,201;3,300|1,a|1,a"; got != want {
			t.Fatalf("round %d: got %s want %s", round, got, want)
		}
	}
}

// Many sessions, opposite-order transfers with history and journal rows, sloppy clients that keep going after an error
// and always send COMMIT: every transaction is all or nothing and the balances match the journal.
func TestDeadlockStormAllOrNothing(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	run(t, s0, "CREATE TABLE acct (id int PRIMARY KEY, bal int)")
	run(t, s0, "CREATE TABLE hist (id int PRIMARY KEY, acct int)")
	run(t, s0, "CREATE TABLE jr (id int PRIMARY KEY, a int, b int)")
	const nacct = 6
	for i := 1; i <= nacct; i++ {
		run(t, s0, fmt.Sprintf("INSERT INTO acct VALUES (%d,0)", i))
	}
	var mu sync.Mutex
	var committed []string
	var wg sync.WaitGroup
	var deadlocks, aborted int
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(e)
			defer s.Close()
			for i := 0; i < 60; i++ {
				id := w*1000 + i + 1
				x, y := 1+(w+i)%nacct, 1+(w*3+i*5+1)%nacct
				if x == y {
					y = y%nacct + 1
				}
				stmts := []string{
					"BEGIN",
					fmt.Sprintf("UPDATE acct SET bal = bal - 1 WHERE id = %d", x),
					fmt.Sprintf("INSERT INTO hist VALUES (%d, %d)", id*10, x),
					fmt.Sprintf("UPDATE acct SET bal = bal + 1 WHERE id = %d", y),
					fmt.Sprintf("INSERT INTO hist VALUES (%d, %d)", id*10+1, y),
					fmt.Sprintf("INSERT INTO jr VALUES (%d, %d, %d)", id, x, y),
				}
				failed := false
				for _, q := range stmts {
					if _, err := tryRun(s, q); err != nil {
						failed = true
						if code(err) == "40P01" {
							mu.Lock()
							deadlocks++
							mu.Unlock()
						}
					}
				}
				res, err := tryRun(s, "COMMIT") // sloppy: always COMMIT, never ROLLBACK
				mu.Lock()
				switch {
				case err == nil && res.Tag == "COMMIT" && !failed:
					committed = append(committed, fmt.Sprint(id))
				case err == nil && res.Tag == "COMMIT" && failed:
					t.Errorf("tx %d reported COMMIT after a statement error", id)
				default:
					aborted++
					if err != nil {
						_, _ = tryRun(s, "ROLLBACK")
					}
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	c := NewSession(e)
	jr := render(run(t, c, "SELECT id FROM jr ORDER BY id"))
	got := map[string]bool{}
	for _, id := range strings.Split(jr, ";") {
		if id != "" {
			got[id] = true
		}
	}
	if len(got) != len(committed) {
		t.Fatalf("journal has %d rows, %d transactions were acknowledged", len(got), len(committed))
	}
	for _, id := range committed {
		if !got[id] {
			t.Fatalf("acknowledged tx %s has no journal row", id)
		}
	}
	if n := render(run(t, c, "SELECT count(*) FROM hist")); n != fmt.Sprint(2*len(got)) {
		t.Fatalf("hist rows %s, want %d", n, 2*len(got))
	}
	if n := render(run(t, c, "SELECT count(*) FROM hist h WHERE NOT EXISTS (SELECT 1 FROM jr WHERE jr.id = h.id/10)")); n != "0" {
		t.Fatalf("history rows without a journal row: %s", n)
	}
	bad := render(run(t, c, "SELECT a.id, a.bal FROM acct a WHERE a.bal <> (SELECT count(*) FROM jr WHERE b = a.id) - (SELECT count(*) FROM jr WHERE a = a.id)"))
	if bad != "" {
		t.Fatalf("balance != journal: %s", bad)
	}
	t.Logf("committed=%d aborted=%d deadlocks=%d", len(committed), aborted, deadlocks)
}

// The harness shape: a hot sequence row bumped last in every transaction, its value used as the key of a journal row,
// next to opposite-order updates of other rows (deadlocks) and sloppy clients. The sequence must be gapless, every
// acknowledged transaction must have exactly its journal row and its effects, and no effect may exist without one.
func TestDeadlockHotSeqJournal(t *testing.T) {
	e := newEngine(t)
	s0 := NewSession(e)
	run(t, s0, "CREATE TABLE seq (id int PRIMARY KEY, v int)")
	run(t, s0, "INSERT INTO seq VALUES (1,0)")
	run(t, s0, "CREATE TABLE ord (id int PRIMARY KEY, st text)")
	run(t, s0, "CREATE TABLE item (id int PRIMARY KEY, o int, st text)")
	run(t, s0, "CREATE TABLE jr (seq int PRIMARY KEY, txid int, kind text)")
	const norder = 12
	for i := 1; i <= norder; i++ {
		run(t, s0, fmt.Sprintf("INSERT INTO ord VALUES (%d,'open')", i))
		run(t, s0, fmt.Sprintf("INSERT INTO item VALUES (%d,%d,'reserved'),(%d,%d,'reserved')", i*10, i, i*10+1, i))
	}
	var mu sync.Mutex
	acked := map[int]bool{}
	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(e)
			defer s.Close()
			for i := 0; i < 50; i++ {
				txid := w*1000 + i + 1
				o := 1 + (w*7+i*3)%norder
				a, b := o*10, o*10+1
				if (w+i)%2 == 1 {
					a, b = b, a // opposite item order: deadlocks between two cancels of the same order are impossible, but with the order row they cross
				}
				other := 1 + (o+w)%norder
				stmts := []string{
					"BEGIN",
					fmt.Sprintf("UPDATE ord SET st = 'cancelled' WHERE id = %d AND st = 'open'", o),
					fmt.Sprintf("UPDATE item SET st = 'available' WHERE id = %d AND st = 'reserved'", a),
					fmt.Sprintf("UPDATE item SET st = 'available' WHERE id = %d AND st = 'reserved'", other*10+1),
					fmt.Sprintf("UPDATE item SET st = 'available' WHERE id = %d AND st = 'reserved'", b),
					"UPDATE seq SET v = v + 1 WHERE id = 1 RETURNING v",
				}
				failed := false
				var seqv string
				for k, q := range stmts {
					res, err := tryRun(s, q)
					if err != nil {
						failed = true
					} else if k == len(stmts)-1 {
						seqv = render(res)
					}
				}
				if !failed {
					if _, err := tryRun(s, fmt.Sprintf("INSERT INTO jr VALUES (%s, %d, 'x')", seqv, txid)); err != nil {
						failed = true
					}
				}
				res, err := tryRun(s, "COMMIT")
				if err != nil {
					_, _ = tryRun(s, "ROLLBACK")
					continue
				}
				if res.Tag == "COMMIT" && failed {
					t.Errorf("COMMIT after an error (tx %d)", txid)
				}
				if res.Tag == "COMMIT" {
					mu.Lock()
					acked[txid] = true
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	c := NewSession(e)
	n := render(run(t, c, "SELECT count(*) FROM jr"))
	mx := render(run(t, c, "SELECT max(seq) FROM jr"))
	v := render(run(t, c, "SELECT v FROM seq"))
	if n != mx || v != mx {
		t.Fatalf("journal count %s max %s seq %s: not gapless", n, mx, v)
	}
	if len(acked) != 0 {
		var nj int
		fmt.Sscan(n, &nj)
		if nj != len(acked) {
			t.Fatalf("journal %d rows, acknowledged %d", nj, len(acked))
		}
	}
	t.Logf("journal=%s acked=%d", n, len(acked))
}
