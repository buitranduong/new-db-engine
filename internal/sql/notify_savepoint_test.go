package sql

import (
	"strings"
	"testing"
	"time"
)

// TestNotifySavepointRollback: ROLLBACK TO SAVEPOINT drops the NOTIFY and
// the LISTEN made after the savepoint, keeps the ones before it (PostgreSQL
// AtSubAbort_Notify; found by the CDC and events suite, scenario f_notify).
func TestNotifySavepointRollback(t *testing.T) {
	e := newEngine(t)
	a, b := NewSession(e), NewSession(e)
	a.PID, b.PID = 1, 2
	run(t, b, `LISTEN jobs`)
	take := func() string {
		time.Sleep(150 * time.Millisecond)
		var out []string
		for _, n := range b.Listener().Take() {
			out = append(out, n.Channel+":"+n.Payload)
		}
		return strings.Join(out, ",")
	}
	run(t, a, `BEGIN; NOTIFY jobs, 'kept1'; SAVEPOINT s; NOTIFY jobs, 'dropped'; ROLLBACK TO s; NOTIFY jobs, 'kept2'; COMMIT`)
	if got := take(); got != "jobs:kept1,jobs:kept2" {
		t.Fatalf("after ROLLBACK TO: %q, want jobs:kept1,jobs:kept2", got)
	}
	// nested: a rolled back inner savepoint, a released outer one
	run(t, a, `BEGIN; SAVEPOINT o; NOTIFY jobs, 'o1'; SAVEPOINT i; NOTIFY jobs, 'i1'; ROLLBACK TO i; RELEASE o; COMMIT`)
	if got := take(); got != "jobs:o1" {
		t.Fatalf("nested: %q, want jobs:o1", got)
	}
	// LISTEN after the savepoint is undone too
	run(t, b, `BEGIN; SAVEPOINT s; LISTEN other; ROLLBACK TO s; COMMIT`)
	run(t, a, `NOTIFY other, 'unheard'`)
	if got := take(); got != "" {
		t.Fatalf("LISTEN rolled back with the savepoint still heard: %q", got)
	}
}
