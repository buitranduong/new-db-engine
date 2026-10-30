package pindex

import (
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// epoch is the reclamation scheme of the optimistic read path (Options.
// Optimistic). A reader announces itself with enter, reads pages without
// pinning them (no write to any shared cache line but its own shard's
// counter), and calls exit. Whoever is about to reuse memory a reader might
// still be looking at (a buffer frame being re-assigned, a page id being
// reallocated) first makes it unreachable and then calls quiesce, which
// returns once every reader that had entered before the call has left.
//
// Correctness in one paragraph. A reader finds a page by loading it from the
// page table after enter. quiesce is called after the page was unlinked from
// the table. A reader that entered before quiesce's check is counted and
// waited for. A reader whose enter is ordered after the check also loads
// from the table after the unlink, so it cannot reach the unlinked page.
// Readers count in one of two parity slots chosen by a monotonically
// increasing generation; enter re-reads the generation after incrementing
// and retries if it moved, so a reader registered in slot p was registered
// while p was the current slot, and quiesce (which flips the slot, then
// waits for the old one to drain) waits for every reader of the old slot
// without being starved by new readers.
type epoch struct {
	cur atomic.Uint32
	c   [2][epShards]epCounter
	mu  sync.Mutex // one quiesce at a time

	quiesces atomic.Int64
}

const epShards = 64

type epCounter struct {
	n atomic.Int64
	_ [56]byte // own cache line
}

type epTok struct{ par, sh uint8 }

func (e *epoch) enter() epTok {
	sh := uint8(rand.Uint32() % epShards)
	for {
		v := e.cur.Load()
		par := uint8(v & 1)
		e.c[par][sh].n.Add(1)
		if e.cur.Load() == v {
			return epTok{par, sh}
		}
		e.c[par][sh].n.Add(-1)
	}
}

func (e *epoch) exit(t epTok) { e.c[t.par][t.sh].n.Add(-1) }

// quiesce waits until every reader that entered before it was called has
// exited. It must not be called by a goroutine inside the epoch.
func (e *epoch) quiesce() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quiesces.Add(1)
	old := e.cur.Load() & 1
	e.cur.Add(1)
	for spins := 0; ; spins++ {
		var s int64
		for i := range e.c[old] {
			s += e.c[old][i].n.Load()
		}
		if s == 0 {
			return
		}
		if spins < 64 {
			runtime.Gosched()
		} else {
			time.Sleep(20 * time.Microsecond)
		}
	}
}
