package sql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"stask/internal/core"
)

// Statement cancellation and session timeouts (PostgreSQL's CancelRequest,
// statement_timeout, idle_in_transaction_session_timeout and
// idle_session_timeout).
//
// The running statement's state is one atomic word, seq<<3 | running<<2 |
// reason: Exec stores a new seq with running set, Cancel and the
// statement_timeout timer set a reason only while that same statement
// runs (a CAS on the whole word, so a late timer or a cancel that arrives
// between statements does nothing), and long-running work polls the
// reason bits: core scans every 1024 rows or per 2048-row chunk, columnar
// view aggregates per chunk, the general executor's join and filter loops
// every few thousand rows. A poll is one atomic load, so the per-row cost
// is nil and a cancel takes effect within one chunk (well under 100 ms).

// Interrupt reasons.
const (
	InterruptCancel  = 1 // CancelRequest
	InterruptTimeout = 2 // statement_timeout
)

const (
	stmtRunning    = 4
	stmtReasonMask = 3
)

// The timeout settings, in the order of stmtCtl.timeouts.
var timeoutVars = [...]string{"statement_timeout", "idle_in_transaction_session_timeout", "idle_session_timeout"}

type stmtCtl struct {
	state  atomic.Int64
	seq    int64       // session goroutine only
	intrFn func() bool // cached method value: Tx.SetInterrupt without an allocation per statement
	// statement_timeout, idle_in_transaction_session_timeout and
	// idle_session_timeout of the session, and the server defaults that
	// RESET / SET ... TO DEFAULT restore
	timeouts, defaults [3]time.Duration
}

// Cancel interrupts the statement the session is running, if any; it
// then fails with SQLSTATE 57014. Safe from any goroutine.
func (s *Session) Cancel() { s.ctl.interrupt(-1, InterruptCancel) }

// interrupt sets reason on statement seq (-1: whichever runs now).
func (c *stmtCtl) interrupt(seq int64, reason int64) {
	for {
		v := c.state.Load()
		if v&stmtRunning == 0 || v&stmtReasonMask != 0 || seq >= 0 && v>>3 != seq {
			return
		}
		if c.state.CompareAndSwap(v, v|reason) {
			return
		}
	}
}

// interrupted reports whether the running statement was cancelled or timed
// out; long-running loops poll it.
func (s *Session) interrupted() bool { return s.ctl.state.Load()&stmtReasonMask != 0 }

// checkInterrupt is interrupted as an error.
func (s *Session) checkInterrupt() error {
	if s.interrupted() {
		return core.ErrInterrupted
	}
	return nil
}

// begin starts a transaction whose scans stop when the running statement
// is interrupted.
func (s *Session) begin(iso core.Isolation) *core.Tx {
	if s.ctl.intrFn == nil {
		s.ctl.intrFn = s.interrupted
	}
	tx := s.E.Begin(iso)
	tx.SetInterrupt(s.ctl.intrFn)
	return tx
}

// Exec executes one statement. A statement interrupted by Cancel or by
// statement_timeout fails with SQLSTATE 57014 (and, inside BEGIN, aborts
// the transaction like any error).
func (s *Session) Exec(st Stmt, params []Datum) (*Result, error) {
	s.ctl.seq++
	seq := s.ctl.seq
	s.ctl.state.Store(seq<<3 | stmtRunning)
	var timer *time.Timer
	if d := s.ctl.timeouts[0]; d > 0 {
		timer = time.AfterFunc(d, func() { s.ctl.interrupt(seq, InterruptTimeout) })
	}
	t0 := s.beginStmt(st)
	res, err := s.exec(st, params)
	s.endStmt(st, t0, err)
	if timer != nil {
		timer.Stop()
	}
	if r := s.ctl.state.Swap(seq<<3) & stmtReasonMask; r != 0 && err != nil {
		return nil, interruptError(r)
	}
	return res, err
}

func interruptError(reason int64) *Error {
	if reason == InterruptTimeout {
		return errf(codeQueryCanceled, "canceling statement due to statement timeout")
	}
	return errf(codeQueryCanceled, "canceling statement due to user request")
}

// StatementTimeout, IdleInTransactionTimeout and IdleSessionTimeout are
// the session's settings (0 = off). The wire protocol enforces the idle
// ones, since only it knows when the session waits for the client.
func (s *Session) StatementTimeout() time.Duration         { return s.ctl.timeouts[0] }
func (s *Session) IdleInTransactionTimeout() time.Duration { return s.ctl.timeouts[1] }
func (s *Session) IdleSessionTimeout() time.Duration       { return s.ctl.timeouts[2] }

// SetTimeoutDefaults sets the server defaults of statement_timeout,
// idle_in_transaction_session_timeout and idle_session_timeout (0 = off),
// which the session starts with and RESET restores.
func (s *Session) SetTimeoutDefaults(statement, idleInTx, idle time.Duration) {
	s.ctl.defaults = [3]time.Duration{statement, idleInTx, idle}
	for i := range timeoutVars {
		s.ctl.timeouts[i] = s.ctl.defaults[i]
		s.vars[timeoutVars[i]] = FormatTimeout(s.ctl.defaults[i])
	}
}

// setTimeout handles SET of one of timeoutVars.
func (s *Session) setTimeout(name, value string) (*Result, error) {
	i := 0
	for i < len(timeoutVars) && timeoutVars[i] != name {
		i++
	}
	var d time.Duration
	if v := strings.ToLower(strings.TrimSpace(value)); v == "default" || v == "" {
		d = s.ctl.defaults[i]
	} else {
		var err error
		if d, err = ParseTimeout(value); err != nil {
			return nil, errf(codeInvalidParameter, "invalid value for parameter %q: %q", name, value)
		}
	}
	s.ctl.timeouts[i] = d
	s.vars[name] = FormatTimeout(d)
	return &Result{Tag: "SET"}, nil
}

// ParseTimeout parses a PostgreSQL time setting: a number of milliseconds,
// or a number with a unit (us, ms, s, min, h, d), as in '5s' or '1.5 min'.
// Negative values are rejected; 0 means off.
func ParseTimeout(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "'\""))
	i := 0
	for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.' || v[i] == '-' || v[i] == '+') {
		i++
	}
	num, unit := v[:i], strings.ToLower(strings.TrimSpace(v[i:]))
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid time value %q", v)
	}
	scale := map[string]time.Duration{"": time.Millisecond, "us": time.Microsecond, "ms": time.Millisecond,
		"s": time.Second, "min": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[unit]
	if scale == 0 {
		return 0, fmt.Errorf("invalid unit in %q (us, ms, s, min, h, d)", v)
	}
	ns := f * float64(scale)
	if f < 0 || ns > float64(math.MaxInt32)*float64(time.Millisecond) {
		return 0, fmt.Errorf("%q is outside the valid range (0 .. 2147483647 ms)", v)
	}
	// PostgreSQL rounds to the setting's unit, milliseconds
	return time.Duration(math.Round(ns/float64(time.Millisecond))) * time.Millisecond, nil
}

// FormatTimeout shows d as PostgreSQL's SHOW does: in the largest unit
// that divides it ("0" when off).
func FormatTimeout(d time.Duration) string {
	ms := d.Milliseconds()
	if ms == 0 {
		return "0"
	}
	for _, u := range []struct {
		ms   int64
		name string
	}{{86_400_000, "d"}, {3_600_000, "h"}, {60_000, "min"}, {1000, "s"}} {
		if ms%u.ms == 0 {
			return strconv.FormatInt(ms/u.ms, 10) + u.name
		}
	}
	return strconv.FormatInt(ms, 10) + "ms"
}
