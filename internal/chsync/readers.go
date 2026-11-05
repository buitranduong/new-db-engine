package chsync

import "sync"

// Readers are the LSNs ClickHouse is being read at (docs/35 section 8). A
// reader at R sees each key as of R: its newest delta version <= R, else
// its base row. That holds while the base has no version above R, so
// consolidation moves versions only up to the oldest R being read, and a
// read may start at R only if no consolidation went past R.
type Readers struct {
	mu   sync.Mutex
	at   map[uint64]int
	upTo uint64 // the highest X a consolidation started with
}

// Acquire registers a read at r; false if the base may hold versions above r.
func (rs *Readers) Acquire(r uint64) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.upTo > r {
		return false
	}
	if rs.at == nil {
		rs.at = map[uint64]int{}
	}
	rs.at[r]++
	return true
}

func (rs *Readers) Release(r uint64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.at[r]--; rs.at[r] <= 0 {
		delete(rs.at, r)
	}
}

// floor is the X a consolidation may start with: x at most, and no later
// than any read in progress.
func (rs *Readers) floor(x uint64) uint64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for r := range rs.at {
		x = min(x, r)
	}
	rs.upTo = max(rs.upTo, x)
	return x
}
