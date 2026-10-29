package sql

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Admission control by priority (docs/12 P0.8). Three classes:
//
//   - ClassRead: point reads, transaction control, DDL and everything that
//     is not a write. Reads have absolute priority: they never queue for a
//     slot, so an overload of the other classes cannot delay them at the
//     admission point. What they still share with the others is the CPU,
//     and that is bounded because the other classes may hold only a
//     limited number of slots (and the heavy lane runs at low priority).
//   - ClassWrite: UPDATE, INSERT, DELETE that are not heavy. They take one
//     of `slots` slots while they execute (not while their commit waits for
//     fsync) and queue in arrival order beyond that. They accept delay, and
//     only delay: nothing is dropped, and a transaction's integrity does
//     not depend on when its statements run.
//   - ClassHeavy: statements on the heavy lane (ranges, scans, multi-row
//     writes). They have their own, smaller pool (a quarter of the slots,
//     at least one) so that bulk work can neither starve point writes nor
//     take the CPU the reads need, and it degrades first: it queues behind
//     its own pool only.
//
// This follows CockroachDB's admission control in spirit: work is admitted
// by priority and the slot count follows CPU overload (kvSlotAdjuster),
// with elastic (background) work held to what the foreground leaves. See
// docs/12 P0.8 for the links. Unlike CockroachDB, user reads here do not
// take a slot at all; the slots exist only to bound the CPU that
// lower-priority classes can hold at once.
//
// With slots > 0 (staskd -admission-slots) the number of slots is fixed;
// otherwise it follows the Go scheduler's latency (see StartScanControl).

// Priority classes, highest first.
const (
	ClassRead = iota
	ClassWrite
	ClassHeavy
	numClasses
)

// ClassNames names the classes in reports.
var ClassNames = [numClasses]string{"read", "write", "heavy"}

type waiter struct {
	ready chan struct{}
	since time.Time
}

type admission struct {
	mu      sync.Mutex
	slots   int // write slots
	inUse   int
	hInUse  int
	queues  [numClasses][]*waiter
	waiting int

	minSlots, maxSlots int
	// statistics
	admitted  [numClasses]atomic.Int64 // statements that asked
	queued    [numClasses]atomic.Int64 // of those, the ones that had to wait
	served    [numClasses]atomic.Int64 // statements finished (slot given back)
	waitNs    [numClasses]atomic.Int64
	waitMaxNs [numClasses]atomic.Int64
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

// heavySlots is the size of the heavy pool.
func (a *admission) heavySlots() int { return max(1, a.slots/4) }

// admit waits for a slot for a statement of class c and returns the
// function that gives it back. Reads return at once.
func admit(c int) func() {
	a := adm.Load()
	if a == nil {
		return func() {}
	}
	a.admitted[c].Add(1)
	if c == ClassRead {
		a.served[c].Add(1)
		return func() {}
	}
	a.mu.Lock()
	if len(a.queues[c]) == 0 && a.hasRoomLocked(c) {
		a.takeLocked(c)
		a.mu.Unlock()
		return func() { a.release(c) }
	}
	w := &waiter{ready: make(chan struct{}), since: time.Now()}
	a.queues[c] = append(a.queues[c], w)
	a.waiting++
	a.mu.Unlock()
	a.queued[c].Add(1)
	<-w.ready
	d := int64(time.Since(w.since))
	a.waitNs[c].Add(d)
	for {
		m := a.waitMaxNs[c].Load()
		if d <= m || a.waitMaxNs[c].CompareAndSwap(m, d) {
			break
		}
	}
	return func() { a.release(c) }
}

func (a *admission) hasRoomLocked(c int) bool {
	if c == ClassHeavy {
		return a.hInUse < a.heavySlots()
	}
	return a.inUse < a.slots
}

func (a *admission) takeLocked(c int) {
	if c == ClassHeavy {
		a.hInUse++
	} else {
		a.inUse++
	}
}

func (a *admission) release(c int) {
	a.served[c].Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	if c == ClassHeavy {
		a.hInUse--
	} else {
		a.inUse--
	}
	a.grantLocked()
}

// grantLocked hands free slots to waiters of each pool in arrival order.
func (a *admission) grantLocked() {
	for _, c := range [...]int{ClassWrite, ClassHeavy} {
		for len(a.queues[c]) > 0 && a.hasRoomLocked(c) {
			w := a.queues[c][0]
			a.queues[c][0] = nil
			a.queues[c] = a.queues[c][1:]
			a.waiting--
			a.takeLocked(c)
			close(w.ready)
		}
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

// AdmissionStats reports per class how many statements asked, how many had
// to queue, how many finished, and their queueing time, plus the current
// slots. Admitted-Served is what is queued or running.
type AdmissionStats struct {
	Slots, HeavySlots, InUse, HeavyInUse, Waiting int
	Admitted, Queued, Served                      [numClasses]int64
	WaitMs, WaitMaxMs                             [numClasses]float64
}

func GetAdmissionStats() (AdmissionStats, bool) {
	a := adm.Load()
	if a == nil {
		return AdmissionStats{}, false
	}
	var s AdmissionStats
	a.mu.Lock()
	s.Slots, s.HeavySlots, s.InUse, s.HeavyInUse, s.Waiting = a.slots, a.heavySlots(), a.inUse, a.hInUse, a.waiting
	a.mu.Unlock()
	for c := 0; c < numClasses; c++ {
		s.Admitted[c] = a.admitted[c].Load()
		s.Queued[c] = a.queued[c].Load()
		s.Served[c] = a.served[c].Load()
		s.WaitMs[c] = float64(a.waitNs[c].Load()) / 1e6
		s.WaitMaxMs[c] = float64(a.waitMaxNs[c].Load()) / 1e6
	}
	return s, true
}

// AdmissionReport is GetAdmissionStats as a JSON-ready map for /v1/status:
// per class asked, queued, served, share served (served/asked, so what is
// still queued or running counts against it) and the queueing time.
func AdmissionReport() map[string]any {
	s, ok := GetAdmissionStats()
	if !ok {
		return nil
	}
	classes := map[string]any{}
	for c := 0; c < numClasses; c++ {
		share := 1.0
		if s.Admitted[c] > 0 {
			share = float64(s.Served[c]) / float64(s.Admitted[c])
		}
		classes[ClassNames[c]] = map[string]any{
			"asked": s.Admitted[c], "queued": s.Queued[c], "served": s.Served[c],
			"served_share": share, "wait_ms_total": s.WaitMs[c], "wait_ms_max": s.WaitMaxMs[c],
		}
	}
	return map[string]any{"slots": s.Slots, "heavy_slots": s.HeavySlots, "in_use": s.InUse,
		"heavy_in_use": s.HeavyInUse, "waiting": s.Waiting, "classes": classes}
}
