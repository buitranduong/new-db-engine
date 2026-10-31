package pgwire

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"stask/internal/core"
)

func startServer(t *testing.T, password string) string {
	return startServerMax(t, password, 0)
}

func startServerMax(t *testing.T, password string, maxConns int64) string {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{E: e, Password: password, MaxConns: maxConns, Logf: t.Logf}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close(); e.Close() })
	return "postgres://app:" + password + "@" + ln.Addr().String() + "/stask?sslmode=disable"
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestPgxExtendedProtocol(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE orders (id BIGINT PRIMARY KEY, customer TEXT NOT NULL, amount DOUBLE PRECISION, placed TIMESTAMPTZ, paid BOOLEAN)`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 3, 4, 5, 6, 7, 8_000_000, time.UTC)
	for i := int64(1); i <= 20; i++ {
		// typed binary parameters through the extended protocol
		if _, err := conn.Exec(ctx, `INSERT INTO orders (id, customer, amount, placed, paid) VALUES ($1, $2, $3, $4, $5)`,
			i, []string{"An", "Bình", "Chi"}[i%3], float64(i)*1.5, at.Add(time.Duration(i)*time.Hour), i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	var (
		id     int64
		cust   string
		amount float64
		placed time.Time
		paid   bool
	)
	if err := conn.QueryRow(ctx, `SELECT id, customer, amount, placed, paid FROM orders WHERE id = $1`, int64(4)).Scan(&id, &cust, &amount, &placed, &paid); err != nil {
		t.Fatal(err)
	}
	if id != 4 || cust != "Bình" || amount != 6 || !placed.Equal(at.Add(4*time.Hour)) || !paid {
		t.Fatalf("row: %d %s %v %v %v", id, cust, amount, placed, paid)
	}
	rows, err := conn.Query(ctx, `SELECT customer, count(*), sum(amount) FROM orders WHERE placed >= $1 GROUP BY customer ORDER BY customer`, at.Add(10*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var c string
		var n int64
		var s float64
		if err := rows.Scan(&c, &n, &s); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if rows.Err() != nil || len(got) != 3 {
		t.Fatalf("group by: %v %v", got, rows.Err())
	}
	// NULL parameters and results
	if _, err := conn.Exec(ctx, `INSERT INTO orders (id, customer, amount) VALUES ($1, $2, $3)`, int64(99), "Null", nil); err != nil {
		t.Fatal(err)
	}
	var amt *float64
	if err := conn.QueryRow(ctx, `SELECT amount FROM orders WHERE id = 99`).Scan(&amt); err != nil || amt != nil {
		t.Fatalf("null: %v %v", amt, err)
	}
	// errors carry SQLSTATE and the connection stays usable
	_, err = conn.Exec(ctx, `INSERT INTO orders (id, customer) VALUES ($1, $2)`, int64(1), "dup")
	if sqlState(err) != "23505" {
		t.Fatalf("want 23505, got %v", err)
	}
	var n int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 21 {
		t.Fatalf("count after error: %d %v", n, err)
	}
	// transactions via the driver API
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM orders WHERE paid`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 11 {
		t.Fatalf("in tx: %d %v", n, err)
	}
	tx.Rollback(ctx)
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 21 {
		t.Fatalf("after rollback: %d %v", n, err)
	}
	// batch (pipelined Parse/Bind/Execute ... Sync)
	b := &pgx.Batch{}
	for i := int64(100); i < 110; i++ {
		b.Queue(`INSERT INTO orders (id, customer) VALUES ($1, 'batch')`, i)
	}
	if err := conn.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM orders WHERE customer = $1`, "batch").Scan(&n); err != nil || n != 10 {
		t.Fatalf("batch: %d %v", n, err)
	}
}

func TestPgxPoolConcurrentTransfers(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, startServer(t, "s3cret")+"&pool_max_conns=32")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE TABLE acct (id INT PRIMARY KEY, bal BIGINT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO acct VALUES ($1, 1000)`, i); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	retries := 0
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				from, to := (w+i)%10, (w+i+3)%10
				for {
					err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
						if _, err := tx.Exec(ctx, `UPDATE acct SET bal = bal - 7 WHERE id = $1`, from); err != nil {
							return err
						}
						_, err := tx.Exec(ctx, `UPDATE acct SET bal = bal + 7 WHERE id = $1`, to)
						return err
					})
					if st := sqlState(err); st == "40001" || st == "40P01" { // a deadlock is retried like a serialization failure, as with PostgreSQL
						mu.Lock()
						retries++
						mu.Unlock()
						continue
					}
					if err != nil {
						t.Error(err)
						return
					}
					break
				}
			}
		}(w)
	}
	wg.Wait()
	var total, n int64
	if err := pool.QueryRow(ctx, `SELECT sum(bal), count(*) FROM acct`).Scan(&total, &n); err != nil {
		t.Fatal(err)
	}
	if total != 10000 || n != 10 {
		t.Fatalf("money not conserved: total=%d n=%d", total, n)
	}
	t.Logf("1600 transfers, %d serialization retries", retries)

	// wrong password is rejected
	bad, err := pgx.Connect(ctx, "postgres://app:wrong@"+pool.Config().ConnConfig.Host+":"+itoa(int(pool.Config().ConnConfig.Port))+"/stask?sslmode=disable")
	if err == nil {
		bad.Close(ctx)
		t.Fatal("wrong password accepted")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestMaxConnections(t *testing.T) {
	ctx := context.Background()
	url := startServerMax(t, "", 2)
	a, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	b, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pgx.Connect(ctx, url)
	if sqlState(err) != "53300" {
		t.Fatalf("third connection: want 53300, got %v", err)
	}
	b.Close(ctx)
	time.Sleep(50 * time.Millisecond)
	c, err := pgx.Connect(ctx, url) // a slot was freed
	if err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
}

// TestListenNotify: an idle listening connection receives notifications
// asynchronously (pgx WaitForNotification), in commit order, with the
// notifier's backend pid; a listener inside a transaction gets them after.
func TestListenNotify(t *testing.T) {
	ctx := context.Background()
	url := startServer(t, "")
	l, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(ctx)
	w, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	if _, err := l.Exec(ctx, `LISTEN events`); err != nil {
		t.Fatal(err)
	}
	wait := func() *pgconn.Notification {
		t.Helper()
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := l.WaitForNotification(c)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := w.Exec(ctx, `CREATE TABLE t (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	// extended protocol, with a write in the same transaction
	tx, err := w.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Exec(ctx, `INSERT INTO t VALUES (1)`)
	tx.Exec(ctx, `SELECT pg_notify($1, $2)`, "events", "first")
	tx.Exec(ctx, `NOTIFY events, 'second'`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second"} {
		n := wait()
		if n.Channel != "events" || n.Payload != want || n.PID != w.PgConn().PID() {
			t.Fatalf("got %+v (notifier pid %d), want payload %q", n, w.PgConn().PID(), want)
		}
	}
	// a listener in a transaction receives after it ends
	ltx, err := l.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(ctx, `NOTIFY events, 'during'`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := ltx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := wait(); n.Payload != "during" {
		t.Fatalf("after the listener's transaction: %+v", n)
	}
}
