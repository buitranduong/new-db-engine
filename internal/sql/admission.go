package sql

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Admission control by priority. Statements that are not heavy (those run
// on the heavy lane) take a slot while they execute; a slot is held only
// while the statement works, not while its commit waits for fsync. When
// every slot is taken, statements queue, and a freed slot goes to a
// waiting read whenever there is one: reads have absolute priority. Writes
// (UPDATE, INSERT, DELETE) form one class served in arrival order; they
// accept delay, and only delay, under overload: nothing is dropped and a
// transaction's integrity does not depend on when its statements run.
// Reads inside write transactions are reads too (they hold nothing while
// waiting: conflicts are checked at commit).
//
// With slots > 0 (staskd -admission-slots) the number of slots is fixed;
// otherwise it follows the Go scheduler's latency (see StartScanControl).

// Priority classes, highest first.
const (
	ClassRead = iota
	ClassWrite
	numClasses
)

type waiter struct {
	ready chan struct{}
	since time.Time
}

type admission struct {
	mu      sync.Mutex
	slots   int
	inUse   int
	queues  [numClasses][]*waiter
	waiting int

	minSlots, maxSlots int
	// statistics
	admitted [numClasses]atomic.Int64
	queued   [numClasses]atomic.Int64
	waitNs   [numClasses]atomic.Int64
}

var adm atomic.Pointer[admission] // nil: admission off

// StartAdmission turns admission control on (staskd does; tests do not).
// slots > 0 fixes the number of slots; 0 lets the controller adapt it.
func StartAdmission(slots int) {
	n := runtime.NumCPU()
	if slots > 0 {
		adm.Store(&admission{slots: slots, minSlots: slots, maxSlots: slots})
		return
	}
	adm.Store(&admission{slots: 2 * n, minSlots: max(2, n/2), maxSlots: 16 * n})
}

func classOf(st Stmt) int {
	switch st.(type) {
	case *Update, *Insert, *Delete:
		return ClassWrite
	}
	return ClassRead
}

// admit waits for a slot for a statement of class c and returns the
// function that gives it back.
func admit(c int) func() {
	a := adm.Load()
	if a == nil {
		return func() {}
	}
	a.admitted[c].Add(1)
	a.mu.Lock()
	if a.inUse < a.slots && a.waiting == 0 {
		a.inUse++
		a.mu.Unlock()
		return a.release
	}
	w := &waiter{ready: make(chan struct{}), since: time.Now()}
	a.queues[c] = append(a.queues[c], w)
	a.waiting++
	a.mu.Unlock()
	a.queued[c].Add(1)
	<-w.ready
	a.waitNs[c].Add(int64(time.Since(w.since)))
	return a.release
}

func (a *admission) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inUse--
	a.grantLocked()
}

// grantLocked hands free slots to waiters: reads first, then writes, each
// in arrival order.
func (a *admission) grantLocked() {
	for a.inUse < a.slots && a.waiting > 0 {
		c := ClassRead
		if len(a.queues[c]) == 0 {
			c = ClassWrite
		}
		w := a.queues[c][0]
		a.queues[c][0] = nil
		a.queues[c] = a.queues[c][1:]
		a.waiting--
		a.inUse++
		close(w.ready)
	}
}

// adjust is called by the controller with the latest scheduler latency
// p99 (seconds).
func (a *admission) adjust(p99 float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case p99 > SchedTarget.Seconds():
		a.slots = max(a.minSlots, a.slots-max(1, a.slots/8))
	case a.waiting > 0:
		a.slots = min(a.maxSlots, a.slots+1)
		a.grantLocked()
	}
}

// AdmissionStats reports per class how many statements were admitted, how
// many had to queue and their total queueing time, plus the current slots.
type AdmissionStats struct {
	Slots, InUse, Waiting int
	Admitted, Queued      [numClasses]int64
	WaitMs                [numClasses]float64
}

func GetAdmissionStats() (AdmissionStats, bool) {
	a := adm.Load()
	if a == nil {
		return AdmissionStats{}, false
	}
	var s AdmissionStats
	a.mu.Lock()
	s.Slots, s.InUse, s.Waiting = a.slots, a.inUse, a.waiting
	a.mu.Unlock()
	for c := 0; c < numClasses; c++ {
		s.Admitted[c] = a.admitted[c].Load()
		s.Queued[c] = a.queued[c].Load()
		s.WaitMs[c] = float64(a.waitNs[c].Load()) / 1e6
	}
	return s, true
}
