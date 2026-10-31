package cluster

// Row locks in a Raft cluster (docs/14 row RLF). The lock table of
// internal/sql is per core.Engine, that is per process, so the only place
// where it can arbitrate writers is the leader. A follower is read-only and
// refuses every statement that takes row locks with SQLSTATE 25006, as a
// PostgreSQL standby does; clients reach the leader with a multi-host
// connection string (target_session_attrs=read-write). These tests drive
// three real staskd processes through the PostgreSQL protocol.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// sessionVia is a session that starts on node i: when i is a follower and
// refuses to take locks (25006), it moves to the leader, as a multi-host
// client does. moved reports that it had to.
func (c *tcluster) sessionVia(t *testing.T, i int) (conn *pgx.Conn, moved bool) {
	ctx := context.Background()
	conn, err := pgOnce(ctx, c.nodeDSN(i))
	if err != nil {
		t.Fatalf("node %d: %v", i, err)
	}
	_, err = conn.Exec(ctx, "SELECT id FROM rl WHERE id = 1 FOR UPDATE")
	if err == nil {
		if !c.isLeaderNode(i) {
			t.Errorf("node %d is not the leader and took a row lock", i)
		}
		return conn, false
	}
	if pgCode(err) != "25006" {
		t.Fatalf("node %d: FOR UPDATE: %v", i, err)
	}
	conn.Close(ctx)
	conn, err = pgOnce(ctx, c.dsn("read-write"))
	if err != nil {
		t.Fatalf("read-write: %v", err)
	}
	return conn, true
}

func TestClusterRowLocks(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	li, _ := c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE rl (id int PRIMARY KEY, n int)")
	c.pgExecRW("INSERT INTO rl VALUES (1, 0), (2, 0), (3, 0)")
	c.settle()
	li, _ = c.waitLeader(20 * time.Second)
	ctx := context.Background()

	t.Run("follower refuses row locks", func(t *testing.T) {
		for i := range c.nodes {
			if i == li {
				continue
			}
			conn, err := pgOnce(ctx, c.nodeDSN(i))
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				"SELECT * FROM rl WHERE id = 1 FOR UPDATE",
				"SELECT * FROM rl WHERE id = 1 FOR UPDATE NOWAIT",
				"SELECT * FROM rl WHERE id = 1 FOR NO KEY UPDATE SKIP LOCKED",
				"SELECT * FROM rl WHERE id = 1 FOR SHARE",
				"UPDATE rl SET n = n + 1 WHERE id = 1",
				"DELETE FROM rl WHERE id = 1",
			} {
				if _, err := conn.Exec(ctx, q); pgCode(err) != "25006" {
					t.Errorf("follower %d, %q: %v, want 25006", i, q, err)
				}
				// the aborted state of an explicit transaction is irrelevant in
				// autocommit; the next statement must work
			}
			// inside BEGIN, as an ORM (Hibernate PESSIMISTIC_WRITE) does
			if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, "SELECT * FROM rl WHERE id = 1 FOR UPDATE"); pgCode(err) != "25006" {
				t.Errorf("follower %d in BEGIN: %v, want 25006", i, err)
			}
			conn.Exec(ctx, "ROLLBACK")
			var n int
			if err := conn.QueryRow(ctx, "SELECT count(*) FROM rl").Scan(&n); err != nil || n != 3 {
				t.Errorf("follower %d plain SELECT: %d %v", i, n, err)
			}
			conn.Close(ctx)
		}
		// the leader takes them
		conn, err := pgOnce(ctx, c.nodeDSN(li))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		for _, q := range []string{"BEGIN", "SELECT * FROM rl WHERE id = 1 FOR UPDATE NOWAIT", "ROLLBACK"} {
			if _, err := conn.Exec(ctx, q); err != nil {
				t.Errorf("leader %q: %v", q, err)
			}
		}
	})

	// Sessions start on all three nodes (so on followers and on the leader);
	// each increments the counter, in autocommit and as read-modify-write
	// under SELECT ... FOR UPDATE. No increment may be lost.
	t.Run("counter increments from every node", func(t *testing.T) {
		const workers, per = 6, 40
		var wg sync.WaitGroup
		var moved atomic.Int32
		var done atomic.Int64
		errc := make(chan error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				conn, mv := c.sessionVia(t, w%3)
				defer conn.Close(ctx)
				if mv {
					moved.Add(1)
				}
				for k := 0; k < per; k++ {
					var err error
					if w%2 == 0 {
						_, err = conn.Exec(ctx, "UPDATE rl SET n = n + 1 WHERE id = 1")
					} else {
						err = rmwIncrement(ctx, conn)
					}
					if err != nil {
						errc <- fmt.Errorf("worker %d: %w", w, err)
						return
					}
					done.Add(1)
				}
			}(w)
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			t.Error(err)
		}
		if moved.Load() == 0 {
			t.Errorf("no session started on a follower")
		}
		var n int
		if err := c.leaderRow(ctx, "SELECT n FROM rl WHERE id = 1", &n); err != nil {
			t.Fatal(err)
		}
		if int64(n) != done.Load() {
			t.Fatalf("counter = %d, acknowledged increments = %d: updates lost", n, done.Load())
		}
		c.settle()
		for i := range c.nodes {
			rows, err := c.pgRowsTable(i, "rl")
			if err != nil || rows[1] != n {
				t.Errorf("node %d: counter %d, %v, want %d", i, rows[1], err, n)
			}
		}
	})

	t.Run("deadlock across nodes", func(t *testing.T) {
		a, _ := c.sessionVia(t, (li+1)%3)
		b, _ := c.sessionVia(t, (li+2)%3)
		defer a.Close(ctx)
		defer b.Close(ctx)
		for _, x := range []*pgx.Conn{a, b} {
			if _, err := x.Exec(ctx, "BEGIN"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := a.Exec(ctx, "UPDATE rl SET n = n + 1 WHERE id = 2"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(ctx, "UPDATE rl SET n = n + 1 WHERE id = 3"); err != nil {
			t.Fatal(err)
		}
		res := make(chan error, 2)
		go func() { _, err := a.Exec(ctx, "UPDATE rl SET n = n + 1 WHERE id = 3"); res <- err }()
		time.Sleep(300 * time.Millisecond)
		go func() { _, err := b.Exec(ctx, "UPDATE rl SET n = n + 1 WHERE id = 2"); res <- err }()
		var dead, ok int
		for k := 0; k < 2; k++ {
			select {
			case err := <-res:
				switch pgCode(err) {
				case "40P01":
					dead++
					// the victim ends its transaction, which frees the other
					a.Exec(ctx, "ROLLBACK")
					b.Exec(ctx, "ROLLBACK")
				case "":
					if err != nil {
						t.Fatal(err)
					}
					ok++
				default:
					t.Fatalf("unexpected: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("deadlock not detected within 20 s")
			}
		}
		if dead != 1 {
			t.Errorf("deadlocks reported = %d (ok %d), want exactly 1", dead, ok)
		}
	})

	t.Run("lock_timeout and NOWAIT against a lock held via another node", func(t *testing.T) {
		a, _ := c.sessionVia(t, (li+1)%3)
		b, _ := c.sessionVia(t, (li+2)%3)
		defer a.Close(ctx)
		defer b.Close(ctx)
		for _, q := range []string{"BEGIN", "SELECT * FROM rl WHERE id = 1 FOR UPDATE"} {
			if _, err := a.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := b.Exec(ctx, "SET lock_timeout = '300ms'"); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := b.Exec(ctx, "UPDATE rl SET n = n + 1 WHERE id = 1"); pgCode(err) != "55P03" {
			t.Errorf("UPDATE with lock_timeout: %v, want 55P03", err)
		}
		if d := time.Since(start); d < 250*time.Millisecond || d > 5*time.Second {
			t.Errorf("lock_timeout took %v", d)
		}
		if _, err := b.Exec(ctx, "SELECT * FROM rl WHERE id = 1 FOR UPDATE NOWAIT"); pgCode(err) != "55P03" {
			t.Errorf("FOR UPDATE NOWAIT: %v, want 55P03", err)
		}
		var n int
		if err := b.QueryRow(ctx, "SELECT count(*) FROM (SELECT * FROM rl WHERE id = 1 FOR UPDATE SKIP LOCKED) x").Scan(&n); err != nil || n != 0 {
			t.Errorf("SKIP LOCKED: %d %v, want 0 rows", n, err)
		}
		a.Exec(ctx, "ROLLBACK")
		if _, err := b.Exec(ctx, "UPDATE rl SET n = n + 1 WHERE id = 1"); err != nil {
			t.Errorf("after release: %v", err)
		}
	})
}

// rmwIncrement reads the counter under FOR UPDATE and writes read+1 back.
func rmwIncrement(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		return err
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT n FROM rl WHERE id = 1 FOR UPDATE").Scan(&n); err != nil {
		conn.Exec(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("UPDATE rl SET n = %d WHERE id = 1", n+1)); err != nil {
		conn.Exec(ctx, "ROLLBACK")
		return err
	}
	_, err := conn.Exec(ctx, "COMMIT")
	return err
}

func (c *tcluster) leaderRow(ctx context.Context, q string, dst ...any) error {
	conn, err := pgOnce(ctx, c.dsn("read-write"))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return conn.QueryRow(ctx, q).Scan(dst...)
}

// pgRowsTable reads id -> n of table tb on node i.
func (c *tcluster) pgRowsTable(i int, tb string) (map[int]int, error) {
	ctx := context.Background()
	conn, err := pgOnce(ctx, c.nodeDSN(i))
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT id, n FROM "+tb)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int]int{}
	for rows.Next() {
		var id, n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		m[id] = n
	}
	return m, rows.Err()
}
