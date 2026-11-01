package sql

import (
	"strconv"
	"testing"
	"time"
)

// TestReleaseAllIsLinear: a transaction that locked many rows (a 5000-row import batch) must release them in time linear
// in the number of locks. releaseAll holds the global lock-table mutex, so a quadratic release (docs/14 row OLTP1: 200-360 ms
// for 5000 locks) stalls every other transaction's lock request for that long.
func TestReleaseAllIsLinear(t *testing.T) {
	lt := &lockTable{entries: map[lockKey]*lockEntry{}, gen: make(chan struct{})}
	const n = 200000
	o := newOwner(&lockNode{})
	other := newOwner(&lockNode{})
	lt.mu.Lock()
	for i := 0; i < n; i++ {
		lt.grant(o, lockKey{spRow, strconv.Itoa(i), "sims"}, lmNoKeyUpdate)
	}
	lt.grant(other, lockKey{spRow, "x", "m2_seq"}, lmNoKeyUpdate)
	lt.mu.Unlock()
	t0 := time.Now()
	lt.releaseAll(o)
	d := time.Since(t0)
	if len(lt.entries) != 1 || lt.held != 1 || len(o.keys) != 0 || len(o.held) != 0 {
		t.Fatalf("after releaseAll: entries=%d held=%d keys=%d ownerHeld=%d", len(lt.entries), lt.held, len(o.keys), len(o.held))
	}
	// linear: tens of ms; the quadratic version needs minutes for 200000 locks
	if d > 3*time.Second {
		t.Fatalf("releaseAll of %d locks took %v", n, d)
	}
	t.Logf("releaseAll of %d locks: %v", n, d)
}
