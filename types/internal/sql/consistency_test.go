package sql

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/core"
)

// TestScanConsistencyUnderLoad checks that parallel aggregates, key-range
// reads and repeatable-read transactions see consistent snapshots while
// transfers, delete+reinsert and log reclamation run concurrently: the
// total balance and the row count never change.
func TestScanConsistencyUnderLoad(t *testing.T) {
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 256 << 10, NoCheckpoint: true, Logf: func(string, ...any) {},
		GCRetention: -1, GCMaxLiveRatio: 0.9, GCRateBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	const rows, bal = 5000, 100
	setup := NewSession(e)
	run(t, setup, `CREATE TABLE acct (id BIGINT PRIMARY KEY, bal INT, note TEXT)`)
	for lo := 0; lo < rows; lo += 500 {
		q := `INSERT INTO acct VALUES `
		for i := lo; i < lo+500; i++ {
			if i > lo {
				q += ","
			}
			q += fmt.Sprintf("(%d, %d, 'row %d')", i, bal, i)
		}
		run(t, setup, q)
	}
	want := fmt.Sprintf("%d,%d", rows, rows*bal)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var commits, conflicts atomic.Int64
	fail := func(f string, a ...any) {
		t.Errorf(f, a...)
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(e)
			r := rand.New(rand.NewPCG(uint64(w), 1))
			for {
				select {
				case <-stop:
					return
				default:
				}
				a, b := r.IntN(rows), r.IntN(rows)
				if a == b {
					continue
				}
				amt := r.IntN(10) + 1
				q := fmt.Sprintf(`BEGIN; UPDATE acct SET bal = bal - %d WHERE id = %d; UPDATE acct SET bal = bal + %d WHERE id = %d; COMMIT`, amt, a, amt, b)
				if r.IntN(4) == 0 {
					// transfer, then delete and reinsert a with its balance
					if _, err := tryRun(s, fmt.Sprintf(`BEGIN; UPDATE acct SET bal = bal - %d WHERE id = %d; UPDATE acct SET bal = bal + %d WHERE id = %d`, amt, a, amt, b)); err != nil {
						tryRun(s, `ROLLBACK`)
						if code(err) != "40001" && code(err) != "40P01" {
							fail("transfer: %v", err)
							return
						}
						conflicts.Add(1)
						continue
					}
					res, err := tryRun(s, fmt.Sprintf(`SELECT bal FROM acct WHERE id = %d`, a))
					if err == nil {
						v := res.Rows[0][0].I
						_, err = tryRun(s, fmt.Sprintf(`DELETE FROM acct WHERE id = %d; INSERT INTO acct VALUES (%d, %d, 'again'); COMMIT`, a, a, v))
					}
					if err != nil {
						tryRun(s, `ROLLBACK`)
						if code(err) != "40001" && code(err) != "40P01" {
							fail("reinsert: %v", err)
							return
						}
						conflicts.Add(1)
						continue
					}
					commits.Add(1)
					continue
				}
				if _, err := tryRun(s, q); err != nil {
					tryRun(s, `ROLLBACK`)
					if code(err) != "40001" && code(err) != "40P01" {
						fail("transfer: %v", err)
						return
					}
					conflicts.Add(1)
					continue
				}
				commits.Add(1)
			}
		}(w)
	}
	wg.Add(1)
	go func() { // log reclamation, as the cleaner would
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if _, err := e.GCRound(); err != nil {
				fail("gc: %v", err)
				return
			}
		}
	}()
	var checks atomic.Int64
	for rd := 0; rd < 3; rd++ {
		wg.Add(1)
		go func(rd int) {
			defer wg.Done()
			s := NewSession(e)
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch rd {
				case 0: // parallel aggregate, autocommit
					if got := render(run(t, s, `SELECT count(*), sum(bal) FROM acct`)); got != want {
						fail("aggregate saw %s, want %s", got, want)
						return
					}
				case 1: // grouped parallel aggregate: per-group totals add up
					res := run(t, s, `SELECT id % 7, count(*), sum(bal) FROM acct GROUP BY id % 7`)
					var n, sum int64
					for _, r := range res.Rows {
						n += r[1].I
						sum += r[2].I
					}
					if got := fmt.Sprintf("%d,%d", n, sum); got != want {
						fail("grouped aggregate saw %s, want %s", got, want)
						return
					}
				case 2: // repeatable read: two aggregates and key ranges agree
					run(t, s, `BEGIN ISOLATION LEVEL REPEATABLE READ`)
					a := render(run(t, s, `SELECT count(*), sum(bal) FROM acct`))
					var n, sum int64
					for lo := 0; lo < rows; lo += 1000 {
						res := run(t, s, fmt.Sprintf(`SELECT count(*), sum(bal) FROM acct WHERE id >= %d AND id < %d`, lo, lo+1000))
						n += res.Rows[0][0].I
						sum += res.Rows[0][1].I
					}
					b := render(run(t, s, `SELECT count(*), sum(bal) FROM acct`))
					run(t, s, `COMMIT`)
					if r := fmt.Sprintf("%d,%d", n, sum); a != want || b != want || r != want {
						fail("repeatable read saw %s / ranges %s / %s, want %s", a, r, b, want)
						return
					}
				}
				checks.Add(1)
			}
		}(rd)
	}
	// at least 4s, and until log reclamation has run (on a busy machine the
	// writers need longer to fill segments)
	for start := time.Now(); time.Since(start) < 4*time.Second ||
		e.Stats.GCReclaimedSegs.Load() == 0 && time.Since(start) < 30*time.Second; {
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-stop:
	default:
		close(stop)
	}
	wg.Wait()
	t.Logf("commits %d conflicts %d checks %d gc %+v", commits.Load(), conflicts.Load(), checks.Load(), e.GCStats())
	if commits.Load() < 100 || checks.Load() < 10 {
		t.Fatalf("too little work: commits %d checks %d", commits.Load(), checks.Load())
	}
	if e.Stats.GCReclaimedSegs.Load() == 0 {
		t.Fatalf("log reclamation did not run: %+v", e.GCStats())
	}
}
