package main

// Recovery stress (B2 hunt, docs/14 row B2H): kill -9 a real staskd at random moments while clients run multi-statement
// transactions that use savepoints, error aborts (duplicate key, deadlock victims, sloppy clients that go on after an
// error and then COMMIT), explicit ROLLBACKs, and hot-row contention. After every restart the oracle is all or nothing
// per transaction:
//
//   - a transaction's journal row (written in the same transaction) exists exactly when its effects exist;
//   - the effects are exactly the savepoint scopes that were kept (hist rows == journal.n, per account counts == balances);
//   - every transaction the client saw acknowledged is in the journal.
//
// Opt-in: STASKD_BIN=/path/to/staskd go test ./cmd/stask-m2 -run TestKillStress -v   (KILLSTRESS_CYCLES, default 6).

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// freePort is a free port that no earlier call in this test binary got:
// the listener is closed on return, so two calls in a row can otherwise
// return the same port (cmd/staskctl TestEncryptedBackup did).
func freePort(t *testing.T) int {
	for {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if _, dup := usedPorts.LoadOrStore(p, true); !dup {
			return p
		}
	}
}

var usedPorts sync.Map

func TestKillStress(t *testing.T) {
	bin := os.Getenv("STASKD_BIN")
	if bin == "" {
		t.Skip("set STASKD_BIN to run the kill -9 stress")
	}
	cycles := 6
	if v, _ := strconv.Atoi(os.Getenv("KILLSTRESS_CYCLES")); v > 0 {
		cycles = v
	}
	dir := t.TempDir()
	pgPort, httpPort := freePort(t), freePort(t)
	dsn := fmt.Sprintf("postgres://postgres:wm@127.0.0.1:%d/postgres?sslmode=disable", pgPort)
	var cmd *exec.Cmd
	start := func() {
		cmd = exec.Command(bin, "-dir", dir, "-addr", fmt.Sprintf("127.0.0.1:%d", httpPort), "-pg-addr", fmt.Sprintf("127.0.0.1:%d", pgPort), "-segment-mb", "8")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if c, err := Connect(context.Background(), dsn); err == nil {
				_, err = c.Exec(context.Background(), "SELECT 1")
				c.Close(context.Background())
				if err == nil {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("staskd did not start")
	}
	kill := func() {
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()
	}
	t.Cleanup(func() {
		if cmd != nil && cmd.ProcessState == nil {
			kill()
		}
	})
	start()
	ctx := context.Background()
	c0, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	const nacct = 8
	for _, q := range []string{
		"CREATE TABLE acct (id int PRIMARY KEY, bal int NOT NULL)",
		"CREATE TABLE hist (id bigint PRIMARY KEY, tx bigint NOT NULL, acct int NOT NULL)",
		"CREATE TABLE jr (txid bigint PRIMARY KEY, n int NOT NULL)",
	} {
		if _, err := c0.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= nacct; i++ {
		if _, err := c0.Exec(ctx, fmt.Sprintf("INSERT INTO acct VALUES (%d, 0)", i)); err != nil {
			t.Fatal(err)
		}
	}
	c0.Close(ctx)

	var txid atomic.Int64
	var ackMu sync.Mutex
	acked := map[int64]int{} // txid -> kept scopes, for transactions whose COMMIT returned COMMIT
	var stats struct{ commits, rollbacks, errors, deadlocks, sloppy atomic.Int64 }

	worker := func(stop <-chan struct{}, w int, seed int64) {
		rng := rand.New(rand.NewSource(seed))
		var c *pgx.Conn
		defer func() {
			if c != nil {
				c.Close(context.Background())
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c == nil || c.IsClosed() {
				var err error
				if c, err = Connect(ctx, dsn); err != nil {
					c = nil
					time.Sleep(50 * time.Millisecond)
					continue
				}
			}
			id := txid.Add(1)
			ex := func(q string) (string, error) {
				ct, err := c.Exec(ctx, q)
				if err != nil {
					stats.errors.Add(1)
					if pe, ok := err.(interface{ SQLState() string }); ok && pe.SQLState() == "40P01" {
						stats.deadlocks.Add(1)
					}
					return "", err
				}
				return ct.String(), nil
			}
			sloppy := rng.Intn(4) == 0
			if _, err := ex("BEGIN"); err != nil {
				continue
			}
			kept, hid, broken := 0, 0, false
			scopes := 1 + rng.Intn(4)
			for k := 0; k < scopes && !broken; k++ {
				a := 1 + rng.Intn(nacct)
				if rng.Intn(2) == 0 {
					a = 1 + rng.Intn(2) // hot pair, opposite orders across workers: deadlocks
				}
				fail := rng.Intn(4) == 0
				if _, err := ex("SAVEPOINT sp"); err != nil {
					broken = true
					break
				}
				var errs [3]error
				_, errs[0] = ex(fmt.Sprintf("UPDATE acct SET bal = bal + 1 WHERE id = %d", a))
				hid++
				_, errs[1] = ex(fmt.Sprintf("INSERT INTO hist VALUES (%d, %d, %d)", id*100+int64(hid), id, a))
				if fail && errs[0] == nil && errs[1] == nil {
					_, errs[2] = ex(fmt.Sprintf("INSERT INTO hist VALUES (%d, %d, %d)", id*100+int64(hid), id, a)) // duplicate key
				}
				if errs[0] != nil || errs[1] != nil || errs[2] != nil {
					if sloppy {
						stats.sloppy.Add(1)
						broken = true // keep going without ROLLBACK TO: later statements must give 25P02, COMMIT must roll back
						break
					}
					if _, err := ex("ROLLBACK TO SAVEPOINT sp"); err != nil {
						broken = true
						break
					}
					continue
				}
				if _, err := ex("RELEASE SAVEPOINT sp"); err != nil {
					broken = true
					break
				}
				kept++
			}
			if !broken && rng.Intn(10) == 0 {
				ex("ROLLBACK")
				stats.rollbacks.Add(1)
				continue
			}
			if broken && !sloppy {
				ex("ROLLBACK")
				continue
			}
			if broken { // sloppy: statements after the error, then COMMIT
				ex("INSERT INTO jr VALUES (" + strconv.FormatInt(id, 10) + ", 99)")
			} else {
				ex(fmt.Sprintf("INSERT INTO jr VALUES (%d, %d)", id, kept))
			}
			tag, err := ex("COMMIT")
			if err == nil && tag == "COMMIT" && !broken {
				ackMu.Lock()
				acked[id] = kept
				ackMu.Unlock()
				stats.commits.Add(1)
			} else if err == nil && tag == "COMMIT" && broken {
				t.Errorf("tx %d: COMMIT of an aborted transaction reported COMMIT", id)
			}
		}
	}

	check := func(cycle int) {
		c, err := Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		defer c.Close(ctx)
		q := func(sql string) [][]string {
			rows, err := QueryText(ctx, c, sql)
			if err != nil {
				t.Fatalf("cycle %d: %s: %v", cycle, sql, err)
			}
			return rows
		}
		jr := map[int64]int{}
		for _, r := range q("SELECT txid, n FROM jr") {
			a, _ := strconv.ParseInt(r[0], 10, 64)
			n, _ := strconv.Atoi(r[1])
			jr[a] = n
		}
		per := map[int64]int{}
		perAcct := map[int]int{}
		for _, r := range q("SELECT tx, acct FROM hist") {
			a, _ := strconv.ParseInt(r[0], 10, 64)
			ac, _ := strconv.Atoi(r[1])
			per[a]++
			perAcct[ac]++
		}
		for id, n := range per {
			j, ok := jr[id]
			if !ok {
				t.Fatalf("cycle %d: tx %d has %d history rows but no journal row (partially applied)", cycle, id, n)
			}
			if j != n {
				t.Fatalf("cycle %d: tx %d journal says %d kept scopes, %d history rows exist", cycle, id, j, n)
			}
		}
		for id, j := range jr {
			if j == 99 {
				t.Fatalf("cycle %d: tx %d (sloppy, aborted) committed", cycle, id)
			}
			if per[id] != j {
				t.Fatalf("cycle %d: journal tx %d says %d, history has %d", cycle, id, j, per[id])
			}
		}
		for _, r := range q("SELECT id, bal FROM acct") {
			a, _ := strconv.Atoi(r[0])
			b, _ := strconv.Atoi(r[1])
			if perAcct[a] != b {
				t.Fatalf("cycle %d: acct %d balance %d, history rows %d", cycle, a, b, perAcct[a])
			}
		}
		ackMu.Lock()
		defer ackMu.Unlock()
		for id, n := range acked {
			if j, ok := jr[id]; !ok || j != n {
				t.Fatalf("cycle %d: acknowledged tx %d lost or changed (journal %v/%v, want %d)", cycle, id, j, ok, n)
			}
		}
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for cycle := 0; cycle < cycles; cycle++ {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 10; w++ {
			wg.Add(1)
			go func(w int) { defer wg.Done(); worker(stop, w, rng.Int63()) }(w)
		}
		time.Sleep(time.Duration(300+rng.Intn(2500)) * time.Millisecond)
		kill()
		close(stop)
		wg.Wait()
		start()
		check(cycle)
	}
	t.Logf("cycles=%d commits=%d rollbacks=%d stmt_errors=%d deadlocks=%d sloppy=%d jr rows verified", cycles, stats.commits.Load(), stats.rollbacks.Load(), stats.errors.Load(), stats.deadlocks.Load(), stats.sloppy.Load())
}
