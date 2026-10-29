package tune

import (
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync/atomic"
)

// GCTuner caps the heap's growth between collections at a fixed number of
// bytes instead of a fixed percentage of the live heap. A high GOGC keeps
// GC CPU low on a small heap, but on a large one (an index and columnar
// view of tens of millions of rows) it lets the process grow to several
// times the data it holds. After every collection the tuner sets
//
//	GOGC = clamp(headroom / live * 100, MinPercent, MaxPercent)
//
// the approach of Uber's GOGCTuner and bytedance's gctuner, which re-arm
// a finalizer to run once per GC cycle. The soft memory limit still
// applies on top.
type GCTuner struct {
	Headroom   int64 // bytes the heap may grow past the live heap
	MinPercent int
	MaxPercent int

	percent atomic.Int64
	live    atomic.Int64
	stopped atomic.Bool
}

type gcSentinel struct{ t *GCTuner }

// Start applies the tuner from now on.
func (t *GCTuner) Start() {
	t.tune()
	t.arm()
}

func (t *GCTuner) Stop() { t.stopped.Store(true) }

// Percent and Live report the last setting and the live heap it was based on.
func (t *GCTuner) Percent() int { return int(t.percent.Load()) }
func (t *GCTuner) Live() int64  { return t.live.Load() }

func (t *GCTuner) arm() {
	s := &gcSentinel{t}
	runtime.SetFinalizer(s, func(s *gcSentinel) {
		if s.t.stopped.Load() {
			return
		}
		s.t.tune()
		s.t.arm()
	})
}

var liveSample = []metrics.Sample{{Name: "/gc/heap/live:bytes"}}

func (t *GCTuner) tune() {
	s := make([]metrics.Sample, 1)
	copy(s, liveSample)
	metrics.Read(s)
	live := int64(0)
	if s[0].Value.Kind() == metrics.KindUint64 {
		live = int64(s[0].Value.Uint64())
	}
	t.live.Store(live)
	t.percent.Store(int64(t.PercentFor(live)))
	debug.SetGCPercent(int(t.percent.Load()))
}

// PercentFor is the GOGC the tuner picks for a live heap of the given size.
func (t *GCTuner) PercentFor(live int64) int {
	p := t.MaxPercent
	if live > 0 && t.Headroom > 0 {
		p = int(min(int64(t.MaxPercent), t.Headroom*100/live))
	}
	return max(p, t.MinPercent)
}
