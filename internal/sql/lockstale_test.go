package sql

import (
	"testing"
	"time"
)

// A waiter learns of a release only when it wakes; its edge to the former holder must not make a
// deadlock out of a second session's wait (false 40P01 of TestMergeConcurrent under load).
func TestStaleWaitEdgeIsNotADeadlock(t *testing.T) {
	lt := &lockTable{entries: map[lockKey]*lockEntry{}, gen: make(chan struct{})}
	a, b := newOwner(&lockNode{}), newOwner(&lockNode{})
	k1, k2 := lockKey{spRow, "t", "1"}, lockKey{spRow, "t", "2"}
	lt.grant(a, k2, lmUpdate)
	// a waits for k1, held by b ...
	lt.grant(b, k1, lmUpdate)
	a.node.waiting, a.node.wkey, a.node.wmode = []*lockNode{b.node}, k1, lmUpdate
	// ... b releases k1 (a has not woken up yet) and now wants k2, held by a
	delete(lt.entries, k1)
	_, err := lt.acquire(b, k2, lmUpdate, waitBlock, waitLimit{deadline: time.Now().Add(80 * time.Millisecond), timeout: true}, nil)
	if code(err) != "55P03" {
		t.Fatalf("want the lock timeout (no deadlock), got %v", err)
	}
	// a real cycle is still found
	lt.grant(b, k1, lmUpdate)
	b.node.waiting, b.node.wkey, b.node.wmode = nil, lockKey{}, 0
	a.node.waiting, a.node.wkey, a.node.wmode = []*lockNode{b.node}, k1, lmUpdate
	if _, err := lt.acquire(b, k2, lmUpdate, waitBlock, waitLimit{}, nil); code(err) != "40P01" {
		t.Fatalf("want deadlock, got %v", err)
	}
}
