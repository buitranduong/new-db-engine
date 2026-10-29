package sql

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/core"
)

// Operational visibility (docs/05 §19): the sessions of pg_stat_activity,
// the counters behind /metrics and the slow-statement log.
//
// Cost: per statement two clock reads, a handful of atomic additions and one
// uncontended mutex round trip on the session's own record (the reader of
// pg_stat_activity is the only other party). Measured in docs/05 §19.

// ---- counters ----

const (
	kindSelect = iota
	kindInsert
	kindUpdate
	kindDelete
	kindTx
	kindDDL
	kindOther
	numKinds
)

var kindNames = [numKinds]string{"select", "insert", "update", "delete", "transaction", "ddl", "other"}

func kindOfStmt(st Stmt) int {
	switch st.(type) {
	case *Select, *SetOp:
		return kindSelect
	case *Insert:
		return kindInsert
	case *Update:
		return kindUpdate
	case *Delete:
		return kindDelete
	case *Begin, *Commit, *Rollback:
		return kindTx
	case *CreateTable, *AlterTable, *DropTable, *Truncate, *CreateView, *DropView, *CreateSequence, *AlterSequence,
		*DropSequence, *CreatePublication, *DropPublication, *AlterReplicaIdentity, *CreateRole, *AlterRole, *DropRole, *Grant:
		return kindDDL
	}
	return kindOther
}

// durBuckets are the upper bounds (seconds) of the statement duration histogram.
var durBuckets = [...]float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 30}

var stats struct {
	stmts    [numKinds]atomic.Int64
	errs     atomic.Int64
	denied   atomic.Int64
	slow     atomic.Int64
	buckets  [len(durBuckets) + 1]atomic.Int64 // per bucket, not cumulative; the last is +Inf
	sumNs    atomic.Int64
	sessions atomic.Int64 // sessions opened since start
	authFail atomic.Int64
}

// CountAuthFailure is called by the wire protocol when a login is refused.
func CountAuthFailure() { stats.authFail.Add(1) }

// ---- slow-statement log ----

var (
	slowNs  atomic.Int64
	slowLog atomic.Pointer[func(string, ...any)]
)

// SetSlowQueryLog logs every statement that takes at least threshold
// (PostgreSQL's log_min_duration_statement; 0 turns it off) through logf.
func SetSlowQueryLog(threshold time.Duration, logf func(string, ...any)) {
	slowLog.Store(&logf)
	slowNs.Store(int64(threshold))
}

const maxLoggedQuery = 4096

// ---- sessions ----

// Activity is one row of pg_stat_activity.
type Activity struct {
	e       *core.Engine
	pid     int32
	user    string
	app     string
	addr    string
	port    int64
	started time.Time

	mu         sync.Mutex
	state      string // active, idle, idle in transaction, idle in transaction (aborted)
	query      string
	queryStart time.Time
	xactStart  time.Time
	stateStart time.Time
}

var (
	actMu sync.Mutex
	acts  = map[*Activity]struct{}{}
)

// Track registers the session in pg_stat_activity (the wire protocol calls
// it once the session is authenticated); Close removes it.
func (s *Session) Track(user, app, addr string, port int64) {
	now := time.Now()
	a := &Activity{e: s.E, pid: s.PID, user: user, app: app, addr: addr, port: port, started: now, state: "idle", stateStart: now}
	s.act = a
	stats.sessions.Add(1)
	actMu.Lock()
	acts[a] = struct{}{}
	actMu.Unlock()
}

func (s *Session) untrack() {
	if s.act != nil {
		actMu.Lock()
		delete(acts, s.act)
		actMu.Unlock()
		s.act = nil
	}
}

// SetQuery records the text of the statement about to run, for
// pg_stat_activity and the slow log (the wire protocol knows the text; the
// executor gets a syntax tree).
func (s *Session) SetQuery(q string) {
	if a := s.act; a != nil {
		a.mu.Lock()
		a.query = q
		a.mu.Unlock()
	}
}

func redacted(st Stmt) string {
	switch st.(type) {
	case *CreateRole:
		return "CREATE ROLE <password hidden>"
	case *AlterRole:
		return "ALTER ROLE <password hidden>"
	}
	return ""
}

func (s *Session) beginStmt(st Stmt) time.Time {
	now := time.Now()
	if a := s.act; a != nil {
		a.mu.Lock()
		a.state, a.queryStart, a.stateStart = "active", now, now
		if a.xactStart.IsZero() {
			a.xactStart = now
		}
		if r := redacted(st); r != "" {
			a.query = r
		}
		a.mu.Unlock()
	}
	return now
}

func (s *Session) endStmt(st Stmt, t0 time.Time, err error) {
	now := time.Now()
	d := now.Sub(t0)
	stats.stmts[kindOfStmt(st)].Add(1)
	if err != nil {
		stats.errs.Add(1)
		if e, ok := err.(*Error); ok && e.Code == codeInsufficientPrivilege {
			stats.denied.Add(1)
		}
	}
	sec := d.Seconds()
	i := 0
	for i < len(durBuckets) && sec > durBuckets[i] {
		i++
	}
	stats.buckets[i].Add(1)
	stats.sumNs.Add(int64(d))
	var q string
	if a := s.act; a != nil {
		a.mu.Lock()
		switch {
		case !s.explicit:
			a.state, a.xactStart = "idle", time.Time{}
		case s.failed:
			a.state = "idle in transaction (aborted)"
		default:
			a.state = "idle in transaction"
		}
		a.stateStart = now
		q = a.query
		a.mu.Unlock()
	}
	if th := slowNs.Load(); th > 0 && int64(d) >= th {
		stats.slow.Add(1)
		if f := slowLog.Load(); f != nil {
			if q == "" {
				q = fmt.Sprintf("<%s>", kindNames[kindOfStmt(st)])
			}
			if len(q) > maxLoggedQuery {
				q = q[:maxLoggedQuery] + "..."
			}
			user, pid := "", s.PID
			if s.act != nil {
				user = s.act.user
			}
			(*f)("slow statement: duration: %.3f ms  pid=%d user=%q statement: %s", float64(d)/1e6, pid, user, q)
		}
	}
}

// activities lists the sessions of e, by pid.
func activities(e *core.Engine) []*Activity {
	actMu.Lock()
	out := make([]*Activity, 0, len(acts))
	for a := range acts {
		if a.e == e {
			out = append(out, a)
		}
	}
	actMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].pid < out[j].pid })
	return out
}

func timeOrNull(t time.Time) Datum {
	if t.IsZero() {
		return Null
	}
	return TimeD(t.UnixMilli())
}

func init() {
	defCat("pg_stat_activity", "datid:i datname:t pid:i leader_pid:i usesysid:i usename:t application_name:t "+
		"client_addr:t client_hostname:t client_port:i backend_start:T xact_start:T query_start:T state_change:T "+
		"wait_event_type:t wait_event:t state:t backend_xid:t backend_xmin:t query_id:i query:t backend_type:t",
		func(c *catSnap) [][]Datum {
			me := c.s.currentUser()
			super := c.s.isSuper()
			var rows [][]Datum
			for _, a := range activities(c.s.E) {
				a.mu.Lock()
				q := a.query
				if !super && a.user != me {
					q = "<insufficient privilege>"
				}
				rows = append(rows, []Datum{IntD(databaseOID), TextD("stask"), IntD(int64(a.pid)), Null, IntD(0),
					TextD(a.user), TextD(a.app), TextD(a.addr), Null, IntD(a.port), timeOrNull(a.started),
					timeOrNull(a.xactStart), timeOrNull(a.queryStart), timeOrNull(a.stateStart), Null, Null,
					TextD(a.state), Null, Null, Null, TextD(q), TextD("client backend")})
				a.mu.Unlock()
			}
			return rows
		})
}

// ---- Prometheus text ----

// WriteMetrics writes the SQL layer's counters in the Prometheus text
// exposition format (https://prometheus.io/docs/instrumenting/exposition_formats/).
// Counters are per process, not per engine.
func WriteMetrics(w io.Writer, e *core.Engine) {
	fmt.Fprintln(w, "# HELP stask_sql_statements_total SQL statements executed, by kind.")
	fmt.Fprintln(w, "# TYPE stask_sql_statements_total counter")
	for i, n := range kindNames {
		fmt.Fprintf(w, "stask_sql_statements_total{kind=%q} %d\n", n, stats.stmts[i].Load())
	}
	fmt.Fprintln(w, "# HELP stask_sql_statement_errors_total SQL statements that failed.")
	fmt.Fprintln(w, "# TYPE stask_sql_statement_errors_total counter")
	fmt.Fprintf(w, "stask_sql_statement_errors_total %d\n", stats.errs.Load())
	fmt.Fprintln(w, "# HELP stask_sql_permission_denied_total Statements refused with SQLSTATE 42501.")
	fmt.Fprintln(w, "# TYPE stask_sql_permission_denied_total counter")
	fmt.Fprintf(w, "stask_sql_permission_denied_total %d\n", stats.denied.Load())
	fmt.Fprintln(w, "# HELP stask_sql_slow_statements_total Statements at or over the slow-statement threshold.")
	fmt.Fprintln(w, "# TYPE stask_sql_slow_statements_total counter")
	fmt.Fprintf(w, "stask_sql_slow_statements_total %d\n", stats.slow.Load())
	fmt.Fprintln(w, "# HELP stask_sql_statement_duration_seconds Statement execution time (the wait for the commit's fsync is not included).")
	fmt.Fprintln(w, "# TYPE stask_sql_statement_duration_seconds histogram")
	var cum int64
	for i, ub := range durBuckets {
		cum += stats.buckets[i].Load()
		fmt.Fprintf(w, "stask_sql_statement_duration_seconds_bucket{le=\"%g\"} %d\n", ub, cum)
	}
	cum += stats.buckets[len(durBuckets)].Load()
	fmt.Fprintf(w, "stask_sql_statement_duration_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "stask_sql_statement_duration_seconds_sum %g\n", float64(stats.sumNs.Load())/1e9)
	fmt.Fprintf(w, "stask_sql_statement_duration_seconds_count %d\n", cum)

	byState := map[string]int{"active": 0, "idle": 0, "idle in transaction": 0, "idle in transaction (aborted)": 0}
	for _, a := range activities(e) {
		a.mu.Lock()
		byState[a.state]++
		a.mu.Unlock()
	}
	states := make([]string, 0, len(byState))
	for k := range byState {
		states = append(states, k)
	}
	sort.Strings(states)
	fmt.Fprintln(w, "# HELP stask_pg_sessions PostgreSQL-protocol sessions, by state.")
	fmt.Fprintln(w, "# TYPE stask_pg_sessions gauge")
	for _, k := range states {
		fmt.Fprintf(w, "stask_pg_sessions{state=%q} %d\n", strings.ReplaceAll(k, " ", "_"), byState[k])
	}
	fmt.Fprintln(w, "# HELP stask_pg_sessions_opened_total PostgreSQL-protocol sessions opened.")
	fmt.Fprintln(w, "# TYPE stask_pg_sessions_opened_total counter")
	fmt.Fprintf(w, "stask_pg_sessions_opened_total %d\n", stats.sessions.Load())
	fmt.Fprintln(w, "# HELP stask_pg_auth_failures_total PostgreSQL-protocol logins refused.")
	fmt.Fprintln(w, "# TYPE stask_pg_auth_failures_total counter")
	fmt.Fprintf(w, "stask_pg_auth_failures_total %d\n", stats.authFail.Load())
}
