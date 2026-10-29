package core

// Temporary latency breakdown for tuning (STASK_LATDEBUG=1).

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime/metrics"
	"sync/atomic"
	"time"
)

type LatHist struct{ b [400]atomic.Int64 }

func (h *LatHist) Add(d time.Duration) {
	us := max(d.Microseconds(), 1)
	h.b[min(int(math.Log2(float64(us))*20), 399)].Add(1)
}

func (h *LatHist) q(p float64) float64 {
	var n int64
	for i := range h.b {
		n += h.b[i].Load()
	}
	if n == 0 {
		return 0
	}
	t := int64(math.Ceil(p * float64(n)))
	var acc int64
	for i := range h.b {
		if acc += h.b[i].Load(); acc >= t {
			return math.Pow(2, float64(i+1)/20) / 1000
		}
	}
	return 0
}

func (h *LatHist) reset() {
	for i := range h.b {
		h.b[i].Store(0)
	}
}

var SyncBytes, SyncCount atomic.Int64

var GatherStarted, GatherFull, GatherTimeout, GatherAdapt, GatherExpect, GatherHave atomic.Int64

var (
	LatDebug                                                                                                       = os.Getenv("STASK_LATDEBUG") != ""
	LatPrepare, LatFlush, LatHold, LatWake, LatQueue, LatBatch, LatSubmit, LatServer, LatSync, LatInstall, LatRead LatHist
)

func init() {
	if !LatDebug {
		return
	}
	go func() {
		for range time.Tick(5 * time.Second) {
			for _, x := range []struct {
				n string
				h *LatHist
			}{{"hold", &LatHold}, {"wake", &LatWake}, {"prepare", &LatPrepare}, {"flush", &LatFlush}, {"queue", &LatQueue}, {"batch", &LatBatch}, {"submit", &LatSubmit}, {"server", &LatServer}, {"sync", &LatSync}, {"instal", &LatInstall}, {"read", &LatRead}} {
				log.Printf("latdebug %-6s p50=%.3f p90=%.3f p99=%.3f p999=%.3f ms", x.n, x.h.q(.5), x.h.q(.9), x.h.q(.99), x.h.q(.999))
				x.h.reset()
			}
			if c := SyncCount.Swap(0); c > 0 {
				log.Printf("latdebug syncs=%d avgKB=%.1f MB/s=%.1f", c, float64(SyncBytes.Load())/float64(c)/1024, float64(SyncBytes.Swap(0))/5/1e6)
			}
			metrics.Read(rtSamples)
			for i, sm := range rtSamples {
				h := sm.Value.Float64Histogram()
				log.Printf("latdebug %s %s", sm.Name, histDelta(h, rtPrev[i]))
				rtPrev[i] = append(rtPrev[i][:0], h.Counts...)
			}
			log.Printf("latdebug gather started=%d full=%d timeout=%d adapt=%dus lastExpect=%d lastHave=%d",
				GatherStarted.Swap(0), GatherFull.Swap(0), GatherTimeout.Swap(0), GatherAdapt.Load()/1000, GatherExpect.Load(), GatherHave.Load())
		}
	}()
}

var rtSamples = []metrics.Sample{{Name: "/sched/latencies:seconds"}, {Name: "/sched/pauses/total/gc:seconds"}}
var rtPrev = make([][]uint64, 2)

// histDelta summarizes the counts added since prev of a runtime histogram.
func histDelta(h *metrics.Float64Histogram, prev []uint64) string {
	d := make([]uint64, len(h.Counts))
	var n uint64
	for i, c := range h.Counts {
		if i < len(prev) {
			c -= prev[i]
		}
		d[i] = c
		n += c
	}
	if n == 0 {
		return "n=0"
	}
	q := func(p float64) float64 {
		t := uint64(math.Ceil(p * float64(n)))
		var acc uint64
		for i, c := range d {
			if acc += c; acc >= t {
				return h.Buckets[i+1] * 1000
			}
		}
		return 0
	}
	return fmt.Sprintf("n=%d p50=%.3f p99=%.3f p999=%.3f max=%.3f ms", n, q(.5), q(.99), q(.999), q(1))
}
