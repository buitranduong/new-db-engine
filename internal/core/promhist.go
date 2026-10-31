package core

import (
	"sync/atomic"
	"time"
)

// PromHist is a fixed-bucket latency histogram of atomic counters (no lock,
// no allocation per observation). Bucket i counts observations up to
// LatBounds[i]; the last bucket is +Inf.
type PromHist struct {
	b     [len(LatBounds) + 1]atomic.Int64
	count atomic.Int64
	sumNs atomic.Int64
}

// LatBounds are the upper bounds of the buckets, in seconds.
var LatBounds = [...]float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Observe records one duration.
func (h *PromHist) Observe(d time.Duration) {
	s := d.Seconds()
	i := 0
	for i < len(LatBounds) && s > LatBounds[i] {
		i++
	}
	h.b[i].Add(1)
	h.count.Add(1)
	h.sumNs.Add(int64(d))
}

// Snapshot returns the cumulative bucket counts (Prometheus style, the last
// one is the total), the count and the sum in seconds.
func (h *PromHist) Snapshot() (cum []int64, count int64, sum float64) {
	cum = make([]int64, len(h.b))
	var run int64
	for i := range h.b {
		run += h.b[i].Load()
		cum[i] = run
	}
	return cum, h.count.Load(), float64(h.sumNs.Load()) / 1e9
}

// Quantile estimates the q-quantile (0 < q < 1) in seconds from the buckets
// (upper bound of the bucket holding it); 0 without observations.
func (h *PromHist) Quantile(q float64) float64 {
	cum, n, _ := h.Snapshot()
	if n == 0 {
		return 0
	}
	want := int64(q * float64(n))
	for i, c := range cum {
		if c > want {
			if i < len(LatBounds) {
				return LatBounds[i]
			}
			return LatBounds[len(LatBounds)-1] * 2
		}
	}
	return 0
}
