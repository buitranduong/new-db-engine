package sql

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// With every slot taken, a freed slot goes to a waiting read whenever there
// is one; writes go in arrival order after them.
func TestAdmissionOrder(t *testing.T) {
	a := &admission{slots: 1, minSlots: 1, maxSlots: 1}
	adm.Store(a)
	defer adm.Store(nil)

	hold := admit(ClassRead) // the only slot
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	queued := 0
	enqueue := func(c int, name string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel := admit(c)
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			rel()
		}()
		queued++
		for { // wait until it is queued, so arrival order is known
			a.mu.Lock()
			n := a.waiting
			a.mu.Unlock()
			if n == queued {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	enqueue(ClassWrite, "w1")
	enqueue(ClassRead, "r1")
	enqueue(ClassWrite, "w2")
	enqueue(ClassRead, "r2")
	enqueue(ClassWrite, "w3")
	enqueue(ClassRead, "r3")
	hold()
	wg.Wait()
	if got := strings.Join(order, " "); got != "r1 r2 r3 w1 w2 w3" {
		t.Fatalf("grant order %s", got)
	}
	if s, _ := GetAdmissionStats(); s.InUse != 0 || s.Waiting != 0 {
		t.Fatalf("slots leaked: %+v", s)
	}
}

// Slots shrink when scheduler latency is high and grow while statements
// wait and latency is low, within bounds.
func TestAdmissionAdjust(t *testing.T) {
	a := &admission{slots: 16, minSlots: 4, maxSlots: 20}
	for i := 0; i < 50; i++ {
		a.adjust(SchedTarget.Seconds() * 2)
	}
	if a.slots != 4 {
		t.Fatalf("slots after high latency: %d", a.slots)
	}
	a.adjust(0) // nobody waiting: unchanged
	if a.slots != 4 {
		t.Fatalf("slots grew without waiters: %d", a.slots)
	}
	for i := 0; i < 50; i++ { // a statement waits each time: every slot is busy
		a.waiting, a.inUse = 1, a.slots
		a.queues[ClassRead] = []*waiter{{ready: make(chan struct{}), since: time.Now()}}
		a.adjust(0)
	}
	if a.slots != 20 {
		t.Fatalf("slots after low latency with waiters: %d", a.slots)
	}
}
