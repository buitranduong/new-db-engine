package sql

import (
	"testing"
	"time"
)

func TestTimeoutSettings(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0": 0, "1500": 1500 * time.Millisecond, "'5s'": 5 * time.Second, "2min": 2 * time.Minute,
		"1.5 s": 1500 * time.Millisecond, "1h": time.Hour, "250ms": 250 * time.Millisecond, "1d": 24 * time.Hour,
	} {
		if d, err := ParseTimeout(in); err != nil || d != want {
			t.Errorf("ParseTimeout(%q) = %v, %v; want %v", in, d, err, want)
		}
	}
	for _, bad := range []string{"-1", "5 parsecs", "abc", "30d"} {
		if _, err := ParseTimeout(bad); err == nil {
			t.Errorf("ParseTimeout(%q): no error", bad)
		}
	}
	s := NewSession(newEngine(t))
	s.SetTimeoutDefaults(0, time.Minute, 0)
	for _, c := range [][2]string{
		{"SET statement_timeout = '5s'", "5s"}, {"SET statement_timeout TO 1500", "1500ms"},
		{"SET statement_timeout = 0", "0"}, {"SET statement_timeout = '90min'", "90min"},
	} {
		run(t, s, c[0])
		if got := render(run(t, s, "SHOW statement_timeout")); got != c[1] {
			t.Errorf("%s: SHOW = %s, want %s", c[0], got, c[1])
		}
	}
	if got := render(run(t, s, "SHOW idle_in_transaction_session_timeout")); got != "1min" {
		t.Errorf("server default: %s", got)
	}
	run(t, s, "SET idle_in_transaction_session_timeout = '2s'")
	if s.IdleInTransactionTimeout() != 2*time.Second {
		t.Errorf("idle in tx: %v", s.IdleInTransactionTimeout())
	}
	run(t, s, "SET idle_in_transaction_session_timeout TO DEFAULT")
	if s.IdleInTransactionTimeout() != time.Minute {
		t.Errorf("default: %v", s.IdleInTransactionTimeout())
	}
	if _, err := tryRun(s, "SET idle_session_timeout = '-5s'"); code(err) != codeInvalidParameter {
		t.Errorf("negative timeout: %v", err)
	}
}

// slowAgg is a full-scan aggregate that takes seconds: md5 of a 5 kB
// string per row.
const slowAgg = `SELECT n % 7, count(*), sum(n) FROM big WHERE md5(repeat(s, 500)) <> '' GROUP BY n % 7`

func loadBig(t *testing.T, s *Session, rows int) {
	t.Helper()
	run(t, s, "CREATE TABLE big (id BIGINT PRIMARY KEY, n BIGINT, s TEXT)")
	for i := 0; i < rows; i += 50_000 {
		run(t, s, "INSERT INTO big (id, n, s) SELECT g, g, 'abcdefghij' FROM generate_series($1::bigint, $2::bigint) AS g",
			IntD(int64(i+1)), IntD(int64(min(i+50_000, rows))))
	}
}

// runCancelled runs q and calls cancel after delay; it returns the error
// and how long after the cancel the statement returned.
func runCancelled(t *testing.T, s *Session, q string, delay time.Duration, cancel func()) (error, time.Duration) {
	t.Helper()
	type out struct {
		err error
		at  time.Time
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		_, err := tryRun(s, q)
		done <- out{err, time.Now()}
	}()
	select {
	case o := <-done:
		t.Fatalf("statement finished in %v before the cancel (err=%v): load more rows", time.Since(start), o.err)
	case <-time.After(delay):
	}
	at := time.Now()
	if cancel != nil {
		cancel()
	}
	o := <-done
	return o.err, o.at.Sub(at)
}

func TestCancelFullScanAggregate(t *testing.T) {
	s := NewSession(newEngine(t))
	loadBig(t, s, 300_000)
	defer func(w int, h int32) { ScanWorkers = w; scanHelpers.Store(h) }(ScanWorkers, scanHelpers.Load())
	for _, workers := range []int{1, 4} { // sequential scan; parallel scan
		ScanWorkers = workers
		err, after := runCancelled(t, s, slowAgg, 300*time.Millisecond, s.Cancel)
		e := AsError(err)
		t.Logf("workers=%d: %v, %v after the cancel", workers, err, after)
		if err == nil || e.Code != codeQueryCanceled || e.Msg != "canceling statement due to user request" {
			t.Fatalf("workers=%d: got %v", workers, err)
		}
		if after > slack*100*time.Millisecond {
			t.Fatalf("workers=%d: cancel took %v", workers, after)
		}
	}
	// the session goes on; a cancel when idle does nothing
	s.Cancel()
	if got := render(run(t, s, "SELECT count(*) FROM big WHERE id <= 10")); got != "10" {
		t.Fatalf("after cancel: %s", got)
	}
	// inside a transaction block the error aborts the transaction
	run(t, s, "BEGIN")
	if err, _ := runCancelled(t, s, slowAgg, 200*time.Millisecond, s.Cancel); code(err) != codeQueryCanceled {
		t.Fatalf("in tx: %v", err)
	}
	if _, err := tryRun(s, "SELECT 1"); code(err) != codeInFailedTx {
		t.Fatalf("after cancel in tx: %v", err)
	}
	run(t, s, "ROLLBACK")
}

func TestCancelJoin(t *testing.T) {
	s := NewSession(newEngine(t))
	loadBig(t, s, 5000)
	// 25M pairs on the general executor
	err, after := runCancelled(t, s, "SELECT count(*) FROM big a JOIN big b ON a.n + b.n > 0", 300*time.Millisecond, s.Cancel)
	if code(err) != codeQueryCanceled || after > slack*100*time.Millisecond {
		t.Fatalf("join: %v after %v", err, after)
	}
}

func TestStatementTimeout(t *testing.T) {
	s := NewSession(newEngine(t))
	loadBig(t, s, 300_000)
	run(t, s, "SET statement_timeout = '250ms'")
	start := time.Now()
	_, err := tryRun(s, slowAgg)
	took := time.Since(start)
	e := AsError(err)
	if err == nil || e.Code != codeQueryCanceled || e.Msg != "canceling statement due to statement timeout" {
		t.Fatalf("got %v after %v", err, took)
	}
	if took < 250*time.Millisecond || took > 250*time.Millisecond+slack*100*time.Millisecond {
		t.Fatalf("timed out after %v, want ~250ms", took)
	}
	// fast statements are unaffected, and the timer of one never hits
	// the next
	for i := 0; i < 100; i++ {
		run(t, s, "SELECT count(*) FROM big WHERE id = 1")
	}
	run(t, s, "SET statement_timeout = 0")
	time.Sleep(300 * time.Millisecond)
	run(t, s, "SELECT count(*) FROM big WHERE id <= 5")
}
