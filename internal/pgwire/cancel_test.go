package pgwire

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"stask/internal/core"
)

func startServerWith(t *testing.T, eopts core.Options, setup func(*Server)) (string, *core.Engine, *Server) {
	t.Helper()
	eopts.Dir, eopts.NoCheckpoint, eopts.Logf = t.TempDir(), true, t.Logf
	if eopts.SegmentSize == 0 {
		eopts.SegmentSize = 4 << 20
	}
	e, err := core.Open(eopts)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{E: e, Logf: t.Logf}
	if setup != nil {
		setup(s)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close(); e.Close() })
	return "postgres://app@" + ln.Addr().String() + "/stask?sslmode=disable", e, s
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// a full-scan aggregate that runs for seconds over 300k rows
const slowAgg = `SELECT n % 7, count(*), sum(n) FROM big WHERE md5(repeat(s, 500)) <> '' GROUP BY n % 7`

func loadBig(t *testing.T, c *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE big (id BIGINT PRIMARY KEY, n BIGINT, s TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 300_000; i += 50_000 {
		if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO big (id, n, s) SELECT g, g, 'abcdefghij' FROM generate_series(%d, %d) AS g", i+1, i+50_000)); err != nil {
			t.Fatal(err)
		}
	}
}

// sendCancel sends a CancelRequest for pid/secret on a new connection.
func sendCancel(t *testing.T, addr string, pid, secret uint32) {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var b [16]byte
	binary.BigEndian.PutUint32(b[0:], 16)
	binary.BigEndian.PutUint32(b[4:], 80877102)
	binary.BigEndian.PutUint32(b[8:], pid)
	binary.BigEndian.PutUint32(b[12:], secret)
	if _, err := nc.Write(b[:]); err != nil {
		t.Fatal(err)
	}
	nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	nc.Read(b[:1]) // the server closes the connection without a reply
}

// docs/12 P5.3: a long aggregate is cancelled within 100 ms; a CancelRequest
// with the wrong key does nothing.
func TestCancelRequest(t *testing.T) {
	url, _, _ := startServerWith(t, core.Options{}, nil)
	c := connect(t, url)
	loadBig(t, c)
	addr := c.PgConn().Conn().RemoteAddr().String()
	pid, key := c.PgConn().PID(), c.PgConn().SecretKey()

	type out struct {
		err error
		at  time.Time
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		_, err := c.Exec(context.Background(), slowAgg)
		done <- out{err, time.Now()}
	}()
	time.Sleep(150 * time.Millisecond)
	sendCancel(t, addr, pid, key^0x5a5a5a5a) // wrong key
	sendCancel(t, addr, pid+1000, key)       // no such session
	time.Sleep(250 * time.Millisecond)
	select {
	case o := <-done:
		t.Fatalf("statement ended %v after start (before the real cancel): %v", o.at.Sub(start), o.err)
	default:
	}
	at := time.Now()
	if err := c.PgConn().CancelRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := <-done
	t.Logf("cancelled %v after the CancelRequest: %v", o.at.Sub(at), o.err)
	if sqlState(o.err) != "57014" {
		t.Fatalf("got %v", o.err)
	}
	if d := o.at.Sub(at); d > slack*100*time.Millisecond {
		t.Fatalf("cancel took %v", d)
	}
	var n int64
	if err := c.QueryRow(context.Background(), "SELECT count(*) FROM big WHERE id <= 3").Scan(&n); err != nil || n != 3 {
		t.Fatalf("after cancel: %d %v", n, err)
	}
}

func TestStatementTimeoutDefault(t *testing.T) {
	url, _, _ := startServerWith(t, core.Options{}, func(s *Server) { s.StatementTimeout = 300 * time.Millisecond })
	c := connect(t, url)
	loadBig(t, c)
	start := time.Now()
	_, err := c.Exec(context.Background(), slowAgg)
	var pe *pgconn.PgError
	if sqlState(err) != "57014" || !asPgError(err, &pe) || pe.Message != "canceling statement due to statement timeout" {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond+slack*200*time.Millisecond {
		t.Fatalf("timed out after %v", d)
	}
	var v string
	if err := c.QueryRow(context.Background(), "SHOW statement_timeout").Scan(&v); err != nil || v != "300ms" {
		t.Fatalf("SHOW: %q %v", v, err)
	}
}

// A session idle inside BEGIN is terminated with 25P03, and its snapshot
// no longer holds back the GC horizon.
func TestIdleInTransactionTimeout(t *testing.T) {
	url, e, _ := startServerWith(t, core.Options{GCRetention: -1}, nil)
	ctx := context.Background()
	w := connect(t, url)
	if _, err := w.Exec(ctx, "CREATE TABLE kv (k BIGINT PRIMARY KEY, v BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(ctx, "INSERT INTO kv VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}
	c := connect(t, url)
	for _, q := range []string{"SET idle_in_transaction_session_timeout = '300ms'", "BEGIN", "SELECT v FROM kv WHERE k = 1"} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Exec(ctx, "UPDATE kv SET v = 2 WHERE k = 1"); err != nil {
		t.Fatal(err)
	}
	st := e.GCStats()
	if st.PinnedSnapshots == 0 || st.Horizon >= e.DurableLSN() {
		t.Fatalf("the open transaction must hold back the horizon: %+v durable %d", st, e.DurableLSN())
	}
	// activity within the timeout keeps the session
	time.Sleep(200 * time.Millisecond)
	if _, err := c.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	_, err := c.Exec(ctx, "SELECT 1")
	if sqlState(err) != "25P03" {
		t.Fatalf("got %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st = e.GCStats()
		if st.PinnedSnapshots == 0 && st.Horizon == e.DurableLSN() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pin not released: %+v durable %d", st, e.DurableLSN())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// outside a transaction the setting does not apply
	c2 := connect(t, url)
	if _, err := c2.Exec(ctx, "SET idle_in_transaction_session_timeout = '100ms'"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := c2.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("idle outside a transaction: %v", err)
	}
}

func TestIdleSessionTimeout(t *testing.T) {
	url, _, _ := startServerWith(t, core.Options{}, func(s *Server) { s.IdleSessionTimeout = 300 * time.Millisecond })
	ctx := context.Background()
	c := connect(t, url)
	if _, err := c.Exec(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // inside a transaction: not applied
	if _, err := c.Exec(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := c.Exec(ctx, "SELECT 1"); sqlState(err) != "57P05" {
		t.Fatalf("got %v", err)
	}
	// SET idle_session_timeout = 0 turns it off for the session
	c2 := connect(t, url)
	if _, err := c2.Exec(ctx, "SET idle_session_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := c2.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
}

func TestKeepAliveSettings(t *testing.T) {
	url, _, _ := startServerWith(t, core.Options{}, func(s *Server) {
		s.KeepAliveIdle, s.KeepAliveInterval, s.KeepAliveCount = 30*time.Second, 5*time.Second, 3
		s.TCPUserTimeout = 20 * time.Second
		s.Logf = func(f string, a ...any) {
			if msg := fmt.Sprintf(f, a...); strings.Contains(msg, "keepalive") || strings.Contains(msg, "user timeout") {
				t.Errorf("%s", msg)
			}
		}
	})
	c := connect(t, url)
	if _, err := c.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
}

func asPgError(err error, pe **pgconn.PgError) bool { return errors.As(err, pe) }
