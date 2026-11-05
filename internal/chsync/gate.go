package chsync

import (
	"sync"
	"time"
)

// Gate is the read-write lock between chsync applying a batch (the writer)
// and the gateway reading ClickHouse (readers). sync.RWMutex blocks new
// readers as soon as a writer waits, so one long read would hold up every
// read behind the next batch; Gate still lets readers in while the writer
// has waited less than Patience (default 200 ms), then lets the writer go
// first, so the sync lag stays bounded by the longest read in progress.
type Gate struct {
	Patience time.Duration

	mu      sync.Mutex
	cond    *sync.Cond
	readers int
	writing bool
	waiting time.Time // when the writer started waiting; zero = not waiting
}

func (g *Gate) lock() {
	g.mu.Lock()
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
}

func (g *Gate) patience() time.Duration {
	if g.Patience <= 0 {
		return 200 * time.Millisecond
	}
	return g.Patience
}

func (g *Gate) RLock() {
	g.lock()
	for g.writing || (!g.waiting.IsZero() && time.Since(g.waiting) >= g.patience()) {
		g.cond.Wait()
	}
	g.readers++
	g.mu.Unlock()
}

func (g *Gate) RUnlock() {
	g.lock()
	if g.readers--; g.readers == 0 {
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}

// Lock is taken by one writer at a time (the stream of one chsync).
func (g *Gate) Lock() {
	g.lock()
	g.waiting = time.Now()
	for g.writing || g.readers > 0 {
		g.cond.Wait()
	}
	g.writing, g.waiting = true, time.Time{}
	g.mu.Unlock()
}

func (g *Gate) Unlock() {
	g.lock()
	g.writing = false
	g.cond.Broadcast()
	g.mu.Unlock()
}
