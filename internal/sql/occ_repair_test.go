package sql

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/core"
)

// Tests of the per-row repair of long autocommit writes (occ_repair.go):
// deterministic cases driven by occTestHook, which runs a concurrent write
// between the statement's evaluation and its commit, the cases that must not
// be repaired, and property / liveness tests with concurrent writers.

func lowRepairMin(t *testing.T, n int) {
	old, oldOff := repairMinRows, writeLocksOff
	// the hooks write from inside the statement, as a writer that takes no row
	// lock would (with locks they would wait for the statement itself)
	repairMinRows, writeLocksOff = n, true
	t.Cleanup(func() { repairMinRows, writeLocksOff = old, oldOff })
}

// hookOnce runs f before the first commit attempt of a statement.
func hookOnce(t *testing.T, f func()) {
	var done atomic.Bool
	occTestHook = func(round int) {
		if done.CompareAndSwap(false, true) {
			f()
		}
	}
	t.Cleanup(func() { occTestHook = nil })
}

func sqlCode(err error) string {
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

func repairRounds() int64 { return core.RepairStats.Rounds.Load() }

func kvSetup(t *testing.T, n int) (*Session, *Session) {
	t.Helper()
	e := newEngine(t)
	s, s2 := NewSession(e), NewSession(e)
	run(t, s, `CREATE TABLE kv (id BIGINT PRIMARY KEY, v BIGINT, w BIGINT, tag TEXT)`)
	run(t, s, `CREATE INDEX kv_v ON kv (v)`)
	run(t, s, `CREATE TABLE src (id BIGINT PRIMARY KEY, d BIGINT)`)
	run(t, s, fmt.Sprintf(`INSERT INTO kv SELECT g, g * 10, 0, 'init' FROM generate_series(1, %d) g`, n))
	return s, s2
}

func TestRepairUpsertSemantics(t *testing.T) {
	lowRepairMin(t, 16)
	s, s2 := kvSetup(t, 100)
	run(t, s, `INSERT INTO src SELECT g, 1000 FROM generate_series(1, 120) g`)
	hookOnce(t, func() {
		run(t, s2, `UPDATE kv SET v = v + 1 WHERE id = 5`)
		run(t, s2, `UPDATE kv SET v = v + 1 WHERE id = 6`)
		run(t, s2, `DELETE FROM kv WHERE id = 7`)
		run(t, s2, `INSERT INTO kv VALUES (110, 3, 0, 'x')`) // the statement meant to insert 110
	})
	r0 := repairRounds()
	res := run(t, s, `INSERT INTO kv SELECT id, d, 7, 'big' FROM src ON CONFLICT (id) DO UPDATE SET v = kv.v + excluded.v, w = excluded.w`)
	if res.Tag != "INSERT 0 120" {
		t.Fatalf("tag %q", res.Tag)
	}
	if repairRounds() == r0 {
		t.Fatal("the statement was not repaired")
	}
	for id, want := range map[int]string{
		5:   "1051,7,init", // DO UPDATE on the newest version (50 + 1)
		6:   "1061,7,init",
		7:   "1000,7,big", // deleted meanwhile: inserted
		110: "1003,7,x",   // inserted meanwhile: DO UPDATE on it
		1:   "1010,7,init",
		100: "2000,7,init",
		120: "1000,7,big",
	} {
		got := render(run(t, s, fmt.Sprintf(`SELECT v, w, tag FROM kv WHERE id = %d`, id)))
		if got != want {
			t.Errorf("id %d: got %s want %s", id, got, want)
		}
	}
	if n := render(run(t, s, `SELECT count(*) FROM kv`)); n != "120" {
		t.Fatalf("count %s", n)
	}
	// secondary index agrees with the table
	if got := render(run(t, s, `SELECT id FROM kv WHERE v = 1051`)); got != "5" {
		t.Fatalf("index lookup: %s", got)
	}
	if got := render(run(t, s, `SELECT id FROM kv WHERE v = 60`)); got != "" {
		t.Fatalf("stale index entry: %s", got)
	}
}

func TestRepairUpdateFromSemantics(t *testing.T) {
	lowRepairMin(t, 16)
	s, s2 := kvSetup(t, 100)
	run(t, s, `UPDATE kv SET v = id`)
	run(t, s, `INSERT INTO src SELECT g, 500 FROM generate_series(1, 100) g`)
	hookOnce(t, func() {
		run(t, s2, `UPDATE kv SET w = 9 WHERE id = 3`)        // no longer matches kv.w = 0: skipped
		run(t, s2, `UPDATE kv SET v = v + 1000 WHERE id = 4`) // still matches; SET sees the new v
		run(t, s2, `DELETE FROM kv WHERE id = 5`)             // gone: skipped
		run(t, s2, `UPDATE kv SET tag = 'touched' WHERE id = 6`)
	})
	r0 := repairRounds()
	res := run(t, s, `UPDATE kv SET v = kv.v + src.d, w = 1 FROM src WHERE kv.id = src.id AND kv.w = 0`)
	if res.Tag != "UPDATE 98" {
		t.Fatalf("tag %q", res.Tag)
	}
	if repairRounds() == r0 {
		t.Fatal("the statement was not repaired")
	}
	for id, want := range map[int]string{
		3: "3,9,init", 4: "1504,1,init", 6: "506,1,touched", 1: "501,1,init", 100: "600,1,init",
	} {
		if got := render(run(t, s, fmt.Sprintf(`SELECT v, w, tag FROM kv WHERE id = %d`, id))); got != want {
			t.Errorf("id %d: got %s want %s", id, got, want)
		}
	}
	if got := render(run(t, s, `SELECT count(*) FROM kv WHERE id = 5`)); got != "0" {
		t.Fatalf("row 5 came back: %s", got)
	}
}

func TestRepairDeleteUsingSemantics(t *testing.T) {
	lowRepairMin(t, 16)
	s, s2 := kvSetup(t, 100)
	run(t, s, `UPDATE kv SET v = id`)
	run(t, s, `INSERT INTO src SELECT g, 0 FROM generate_series(1, 100) g`)
	hookOnce(t, func() {
		run(t, s2, `UPDATE kv SET v = 1000 WHERE id = 10`) // no longer matches: survives
		run(t, s2, `DELETE FROM kv WHERE id = 11`)
		run(t, s2, `UPDATE kv SET v = 5 WHERE id = 60`) // not in the statement's snapshot match: not deleted
	})
	res := run(t, s, `DELETE FROM kv USING src WHERE kv.id = src.id AND kv.v < 50`)
	if res.Tag != "DELETE 47" {
		t.Fatalf("tag %q", res.Tag)
	}
	if got := render(run(t, s, `SELECT count(*) FROM kv`)); got != "52" {
		t.Fatalf("count %s", got)
	}
	if got := render(run(t, s, `SELECT id FROM kv WHERE id <= 12 OR id = 60 ORDER BY id`)); got != "10;60" {
		t.Fatalf("survivors %s", got)
	}
}

func TestRepairPlainUpdateAndTagCount(t *testing.T) {
	lowRepairMin(t, 16)
	s, s2 := kvSetup(t, 100)
	hookOnce(t, func() {
		run(t, s2, `UPDATE kv SET v = -1 WHERE id = 8`) // stops matching v >= 0
		run(t, s2, `UPDATE kv SET v = v + 1 WHERE id = 9`)
	})
	res := run(t, s, `UPDATE kv SET w = w + 1, v = v + 100 WHERE v >= 0`)
	if res.Tag != "UPDATE 99" {
		t.Fatalf("tag %q", res.Tag)
	}
	if got := render(run(t, s, `SELECT v, w FROM kv WHERE id IN (8, 9) ORDER BY id`)); got != "-1,0;191,1" {
		t.Fatalf("got %s", got)
	}
}

func TestRepairExplicitTransactionStillConflicts(t *testing.T) {
	lowRepairMin(t, 16)
	s, s2 := kvSetup(t, 100)
	run(t, s, `BEGIN`)
	run(t, s, `UPDATE kv SET w = w + 1`)
	run(t, s2, `UPDATE kv SET v = v + 1 WHERE id = 5`)
	r0 := repairRounds()
	_, err := tryRun(s, `COMMIT`)
	if sqlCode(err) != "40001" {
		t.Fatalf("COMMIT: %v, want 40001", err)
	}
	if repairRounds() != r0 {
		t.Fatal("an explicit transaction was repaired")
	}
	if got := render(run(t, s, `SELECT sum(w), v FROM kv WHERE id = 5 GROUP BY v`)); got != "0,51" {
		t.Fatalf("state after the failed COMMIT: %s", got)
	}
}

func TestRepairConstraintsAfterReapply(t *testing.T) {
	lowRepairMin(t, 16)
	t.Run("check", func(t *testing.T) {
		e := newEngine(t)
		s, s2 := NewSession(e), NewSession(e)
		run(t, s, `CREATE TABLE kc (id BIGINT PRIMARY KEY, v BIGINT CHECK (v >= 0))`)
		run(t, s, `INSERT INTO kc SELECT g, 10 FROM generate_series(1, 100) g`)
		hookOnce(t, func() { run(t, s2, `UPDATE kc SET v = 3 WHERE id = 50`) })
		_, err := tryRun(s, `UPDATE kc SET v = v - 5`)
		if sqlCode(err) != "23514" {
			t.Fatalf("err %v, want a check violation", err)
		}
		if got := render(run(t, s, `SELECT count(*), sum(v) FROM kc`)); got != "100,993" {
			t.Fatalf("statement not atomic: %s", got)
		}
	})
	t.Run("unique", func(t *testing.T) {
		e := newEngine(t)
		s, s2 := NewSession(e), NewSession(e)
		run(t, s, `CREATE TABLE ku (id BIGINT PRIMARY KEY, u BIGINT)`)
		run(t, s, `CREATE UNIQUE INDEX ku_u ON ku (u)`)
		run(t, s, `INSERT INTO ku SELECT g, g FROM generate_series(1, 100) g`)
		// the repaired row takes a free value; a concurrent insert takes the value
		// the statement would give another row: 23505, nothing applied
		hookOnce(t, func() {
			run(t, s2, `UPDATE ku SET u = 2000 WHERE id = 50`)
			run(t, s2, `INSERT INTO ku VALUES (201, 1060)`)
		})
		_, err := tryRun(s, `UPDATE ku SET u = u + 1000`)
		if sqlCode(err) != "23505" {
			t.Fatalf("err %v, want a unique violation", err)
		}
		if got := render(run(t, s, `SELECT count(*), sum(u) FROM ku`)); got != "101,"+fmt.Sprint(5050-50+2000+1060) {
			t.Fatalf("statement not atomic: %s", got)
		}
		// without the clash it succeeds, the repaired row included
		run(t, s, `DELETE FROM ku WHERE id = 201`)
		hookOnce(t, func() { run(t, s2, `UPDATE ku SET u = 3000 WHERE id = 50`) })
		run(t, s, `UPDATE ku SET u = u + 1000`)
		if got := render(run(t, s, `SELECT u FROM ku WHERE id IN (49, 50) ORDER BY id`)); got != "1049;4000" {
			t.Fatalf("got %s", got)
		}
		if got := render(run(t, s, `SELECT id FROM ku WHERE u = 4000`)); got != "50" {
			t.Fatalf("unique index lookup: %s", got)
		}
	})
	t.Run("foreign key and trigger fall back to a rerun", func(t *testing.T) {
		e := newEngine(t)
		s, s2 := NewSession(e), NewSession(e)
		run(t, s, `CREATE TABLE fp (id BIGINT PRIMARY KEY)`)
		run(t, s, `CREATE TABLE fc (id BIGINT PRIMARY KEY, pid BIGINT REFERENCES fp(id), v BIGINT)`)
		run(t, s, `INSERT INTO fp SELECT g FROM generate_series(1, 10) g`)
		run(t, s, `INSERT INTO fc SELECT g, 1 + g % 10, 0 FROM generate_series(1, 100) g`)
		r0 := repairRounds()
		hookOnce(t, func() { run(t, s2, `UPDATE fc SET v = 100 WHERE id = 5`) })
		run(t, s, `UPDATE fc SET v = v + 1`)
		if got := render(run(t, s, `SELECT v FROM fc WHERE id IN (4, 5) ORDER BY id`)); got != "1;101" {
			t.Fatalf("fk table: %s", got)
		}
		run(t, s, `CREATE TABLE acct (id BIGINT PRIMARY KEY, name TEXT, bal BIGINT, upd BIGINT)`)
		run(t, s, `CREATE TRIGGER b_stamp BEFORE UPDATE ON acct FOR EACH ROW BEGIN SET NEW.upd = NEW.upd + 1; END`)
		run(t, s, `INSERT INTO acct SELECT g, 'n', 100, 0 FROM generate_series(1, 100) g`)
		hookOnce(t, func() { run(t, s2, `UPDATE acct SET bal = bal + 1 WHERE id = 5`) })
		run(t, s, `UPDATE acct SET bal = bal + 10`)
		// the trigger ran once per applied change: twice for the row the hook also changed
		if got := render(run(t, s, `SELECT bal, upd FROM acct WHERE id IN (4, 5) ORDER BY id`)); got != "110,1;111,2" {
			t.Fatalf("trigger table: %s", got)
		}
		if repairRounds() != r0 {
			t.Fatal("tables with foreign keys or triggers must not be repaired")
		}
	})
}

// A statement that is overtaken again and again fails with a clear error
// instead of running forever.
func TestRepairGivesUp(t *testing.T) {
	lowRepairMin(t, 16)
	s, s2 := kvSetup(t, 100)
	// the hook writes from the statement's own goroutine, which the gate would block
	oldRound := repairGateRound
	repairGateRound = 1 << 30
	t.Cleanup(func() { repairGateRound = oldRound })
	n := 0
	var inHook atomic.Bool
	occTestHook = func(round int) {
		if !inHook.CompareAndSwap(false, true) {
			return // the hook's own statements
		}
		defer inHook.Store(false)
		n++
		run(t, s2, fmt.Sprintf(`UPDATE kv SET v = v + 1 WHERE id = %d`, 1+n%100))
	}
	t.Cleanup(func() { occTestHook = nil })
	t0 := time.Now()
	_, err := tryRun(s, `UPDATE kv SET w = w + 1`)
	if sqlCode(err) != "40001" || !strings.Contains(err.Error(), "repair rounds") {
		t.Fatalf("err %v, want 40001 mentioning repair rounds", err)
	}
	if n > repairMaxRounds+1 || time.Since(t0) > 5*time.Second*time.Duration(slack) {
		t.Fatalf("%d rounds in %v", n, time.Since(t0))
	}
	occTestHook = nil
	if got := render(run(t, s, `SELECT sum(w) FROM kv`)); got != "0" {
		t.Fatalf("a failed statement left rows changed: sum(w) = %s", got)
	}
}

type kvModel struct {
	init map[int]int64 // value before the statements (absent: row did not exist)
	incs map[int]int64 // increments acknowledged by the small writers
}

// Property: small writers increment random rows without pause while long
// statements (upsert, conditional UPDATE .. FROM) run. The final state must
// equal a serial order of PostgreSQL READ COMMITTED: increments commute with
// the additive statements, so v = initial + acknowledged increments + the
// statements' additions, each applied exactly once to every row it affects,
// and a conditional statement skips the rows flagged before it started.
func TestRepairPropertyAgainstModel(t *testing.T) {
	lowRepairMin(t, 32)
	seeds := 4
	if slack > 1 {
		seeds = 2
	}
	var rounds0 = repairRounds()
	for seed := int64(1); seed <= int64(seeds); seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			const N = 3000
			s, _ := kvSetup(t, N)
			e := s.E
			// rows 1..N exist with v = 10*id
			src1, src2 := map[int]bool{}, map[int]bool{}
			for len(src1) < 2000 {
				src1[1+rng.Intn(N+500)] = true // 500 ids are new
			}
			for len(src2) < 2000 {
				src2[1+rng.Intn(N)] = true
			}
			for id := range src1 {
				run(t, s, fmt.Sprintf(`INSERT INTO src VALUES (%d, 1000)`, id))
			}
			run(t, s, `CREATE TABLE src2 (id BIGINT PRIMARY KEY, d BIGINT)`)
			for id := range src2 {
				run(t, s, fmt.Sprintf(`INSERT INTO src2 VALUES (%d, 70000)`, id))
			}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var mu sync.Mutex
			incs := map[int]int64{}
			flagged := map[int]bool{}
			for w := 0; w < 4; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					ws := NewSession(e)
					r := rand.New(rand.NewSource(seed*100 + int64(w)))
					for {
						select {
						case <-stop:
							return
						default:
						}
						id := 1 + r.Intn(N+500)
						if w == 3 { // the flagger
							res, err := tryRun(ws, `UPDATE kv SET w = -1 WHERE id = $1`, IntD(int64(id)))
							if err == nil && res.Tag == "UPDATE 1" {
								mu.Lock()
								flagged[id] = true
								mu.Unlock()
							}
							time.Sleep(200 * time.Microsecond)
							continue
						}
						res, err := tryRun(ws, `UPDATE kv SET v = v + 1 WHERE id = $1`, IntD(int64(id)))
						if err == nil && res.Tag == "UPDATE 1" {
							mu.Lock()
							incs[id]++
							mu.Unlock()
						}
					}
				}(w)
			}
			time.Sleep(20 * time.Millisecond)
			res1 := run(t, s, `INSERT INTO kv SELECT id, d, 0, 'big' FROM src ON CONFLICT (id) DO UPDATE SET v = kv.v + excluded.v`)
			mu.Lock()
			flaggedBefore := map[int]bool{}
			for id := range flagged {
				flaggedBefore[id] = true
			}
			mu.Unlock()
			res2 := run(t, s, `UPDATE kv SET v = kv.v + src2.d FROM src2 WHERE kv.id = src2.id AND kv.w <> -1`)
			close(stop)
			wg.Wait()
			if res1.Tag != fmt.Sprintf("INSERT 0 %d", len(src1)) {
				t.Fatalf("tag 1 %q, want %d rows", res1.Tag, len(src1))
			}
			// verify
			applied2 := 0
			rows := run(t, s, `SELECT id, v, w FROM kv ORDER BY id`).Rows
			seen := map[int]bool{}
			for _, r := range rows {
				id, v, w := int(r[0].I), r[1].I, r[2].I
				seen[id] = true
				base := int64(0)
				if id <= N {
					base = int64(id) * 10
				}
				if src1[id] {
					base += 1000
				}
				rest := v - base - incs[id]
				switch {
				case !src2[id]:
					if rest != 0 {
						t.Fatalf("id %d: v=%d, want %d (+%d increments)", id, v, base, incs[id])
					}
				case rest == 70000:
					applied2++
					if flaggedBefore[id] {
						t.Fatalf("id %d was flagged before the conditional statement and still updated", id)
					}
				case rest == 0:
					if w != -1 {
						t.Fatalf("id %d: conditional update lost although the row was never flagged (w=%d)", id, w)
					}
				default:
					t.Fatalf("id %d: v=%d base=%d incs=%d: lost or doubled update (rest %d)", id, v, base, incs[id], rest)
				}
			}
			for id := range src1 {
				if !seen[id] {
					t.Fatalf("id %d of the upsert is missing", id)
				}
			}
			if res2.Tag != fmt.Sprintf("UPDATE %d", applied2) {
				t.Fatalf("tag 2 %q, rows updated %d", res2.Tag, applied2)
			}
			// the index agrees with the table
			chk := run(t, s, `SELECT id, v FROM kv WHERE id % 97 = 0`).Rows
			for _, r := range chk {
				got := render(run(t, s, `SELECT id FROM kv WHERE v = $1 AND id = $2`, IntD(r[1].I), IntD(r[0].I)))
				if got != fmt.Sprint(r[0].I) {
					t.Fatalf("index lookup of v=%d id=%d: %q", r[1].I, r[0].I, got)
				}
			}
			if n := run(t, s, `SELECT count(*) FROM kv`).Rows[0][0].I; int(n) != len(seen) {
				t.Fatalf("count %d vs %d", n, len(seen))
			}
		})
	}
	if repairRounds() == rounds0 {
		t.Log("note: no statement needed a repair in this run")
	}
}

// A 200k-row upsert completes while 8 goroutines write continuously to the
// same table (it never did when the whole statement was rerun on conflict).
func TestRepairLongStatementUnderWriters(t *testing.T) {
	n := 200000
	if slack > 1 {
		n = 20000
	}
	e := newEngine(t)
	s := NewSession(e)
	run(t, s, `CREATE TABLE kv (id BIGINT PRIMARY KEY, v BIGINT, w BIGINT, tag TEXT)`)
	run(t, s, `CREATE INDEX kv_v ON kv (v)`)
	run(t, s, `CREATE TABLE src (id BIGINT PRIMARY KEY, d BIGINT)`)
	run(t, s, fmt.Sprintf(`INSERT INTO kv SELECT g, 0, 0, 'i' FROM generate_series(1, %d) g`, n))
	run(t, s, fmt.Sprintf(`INSERT INTO src SELECT g, 5 FROM generate_series(1, %d) g`, n))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	counts := make([]map[int]int64, 8)
	var total atomic.Int64
	for w := 0; w < 8; w++ {
		counts[w] = map[int]int64{}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ws := NewSession(e)
			r := rand.New(rand.NewSource(int64(w)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := 1 + r.Intn(n)
				if res, err := tryRun(ws, `UPDATE kv SET v = v + 1 WHERE id = $1`, IntD(int64(id))); err == nil && res.Tag == "UPDATE 1" {
					counts[w][id]++
					total.Add(1)
				}
			}
		}(w)
	}
	time.Sleep(100 * time.Millisecond)
	r0, k0 := repairRounds(), core.RepairStats.KeysRepaired.Load()
	t0 := time.Now()
	res, err := tryRun(s, `INSERT INTO kv SELECT id, d, 1, 'big' FROM src ON CONFLICT (id) DO UPDATE SET v = kv.v + excluded.v, w = excluded.w`)
	took := time.Since(t0)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("after %v with %d concurrent commits: %v", took, total.Load(), err)
	}
	t.Logf("%d rows in %v while %d small commits ran: %d repair rounds, %d rows repaired", n, took,
		total.Load(), repairRounds()-r0, core.RepairStats.KeysRepaired.Load()-k0)
	if res.Tag != fmt.Sprintf("INSERT 0 %d", n) {
		t.Fatalf("tag %q", res.Tag)
	}
	sum := map[int]int64{}
	for _, m := range counts {
		for id, c := range m {
			sum[id] += c
		}
	}
	for _, r := range run(t, s, `SELECT id, v, w FROM kv`).Rows {
		id := int(r[0].I)
		if r[1].I != 5+sum[id] || r[2].I != 1 {
			t.Fatalf("id %d: v=%d w=%d, want v=%d w=1", id, r[1].I, r[2].I, 5+sum[id])
		}
	}
}
