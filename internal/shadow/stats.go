package shadow

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Stats collects apply statistics of one process lifetime.
type Stats struct {
	mu                    sync.Mutex
	start                 time.Time
	Txs, Rows             int64
	Anomalies             int64
	schemaChanges         atomic.Int64
	sourceLSN, appliedLSN atomic.Uint64
	lastCommitUs          atomic.Int64
	reconnects            atomic.Int64
	streaming             atomic.Bool
	lat                   []int64 // commit-to-applied latency, microseconds
	winLat                []int64
	winRows               int64
	winStart              time.Time
}

func newStats() *Stats { n := time.Now(); return &Stats{start: n, winStart: n} }

func (s *Stats) record(rows int, latUs int64) {
	s.mu.Lock()
	s.Txs++
	s.Rows += int64(rows)
	s.winRows += int64(rows)
	if len(s.lat) < 4_000_000 {
		s.lat = append(s.lat, latUs)
	}
	s.winLat = append(s.winLat, latUs)
	s.mu.Unlock()
}

// Percentile returns the p-th percentile (0..100) of xs (unsorted ok), in the
// same unit as xs; 0 when empty.
func Percentile(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]int64(nil), xs...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	idx := int(math.Ceil(p/100*float64(len(c)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(c) {
		idx = len(c) - 1
	}
	return c[idx]
}

// Window returns rows/s and p50/p99 latency (ms) since the last call.
func (s *Stats) Window() (rowsPerSec float64, p50, p99 float64, txs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el := time.Since(s.winStart).Seconds()
	if el > 0 {
		rowsPerSec = float64(s.winRows) / el
	}
	p50 = float64(Percentile(s.winLat, 50)) / 1000
	p99 = float64(Percentile(s.winLat, 99)) / 1000
	txs = len(s.winLat)
	s.winLat, s.winRows, s.winStart = s.winLat[:0], 0, time.Now()
	return
}

// Summary is the cumulative result of this process.
type Summary struct {
	Txs, Rows, Anomalies int64
	Seconds              float64
	RowsPerSec           float64
	LatP50ms, LatP99ms   float64
	LatMaxms             float64
}

func (s *Stats) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec := time.Since(s.start).Seconds()
	out := Summary{Txs: s.Txs, Rows: s.Rows, Anomalies: s.Anomalies, Seconds: sec,
		LatP50ms: float64(Percentile(s.lat, 50)) / 1000, LatP99ms: float64(Percentile(s.lat, 99)) / 1000,
		LatMaxms: float64(Percentile(s.lat, 100)) / 1000}
	if sec > 0 {
		out.RowsPerSec = float64(s.Rows) / sec
	}
	return out
}

// rateLimiter spaces row applications at no more than rps rows per second
// (0 = unlimited).
type rateLimiter struct {
	rps    float64
	nextAt time.Time
}

// delay registers n rows and returns how long the caller should wait.
func (r *rateLimiter) delay(n int, now time.Time) time.Duration {
	if r.rps <= 0 || n <= 0 {
		return 0
	}
	if r.nextAt.Before(now) {
		r.nextAt = now
	}
	r.nextAt = r.nextAt.Add(time.Duration(float64(n) / r.rps * float64(time.Second)))
	if d := r.nextAt.Sub(now); d > 0 {
		return d
	}
	return 0
}
