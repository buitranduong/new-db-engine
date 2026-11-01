package sql

// Opt-in boundary spans of the R2 statement (docs/34 H1). STASK_HYSPANS=1 turns them on: for every statement that is an R2
// candidate (a SELECT over a derived table, hybrid on or off) the wire layer and the SQL layer record the duration of each
// stage in a ring of the last hySpanRing samples; GET /debug/hyspans (on the -pprof address) reports p50/p99/mean per stage
// plus the Go allocator and GC counters, ?reset=1 clears them. Off: one predictable branch per site, nothing allocated.

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// HySpanOn reports whether the boundary spans are recorded.
var HySpanOn = func() bool { return envOn("STASK_HYSPANS") }()

// Stage ids of the boundary spans.
const (
	HyWireParse  = iota // pgwire: Session.Parse of the simple-query text
	HyExec              // Session.Exec (admission, snapshot, hybrid or row executor, endStmt)
	HyTotal             // tryHybrid + hyRun (hybrid only)
	HyPlan              // hyRecognize (hybrid only)
	HyContract          // coordinator contract: wait for the follower + state lookup
	HyDim               // dimension scan of the core at the read snapshot
	HyKernel            // TopNPerGroup on the segment state
	HyGetAt             // sum over the page of Engine.GetAt
	HyCheck             // sum over the page of the invariant check against the core row
	HyMat               // sum over the page of output row materialisation (fromCol, conform)
	HyWireEncode        // pgwire: RowDescription + DataRow encoding
	HyWireFlush         // pgwire: ReadyForQuery + Flush to the socket
	HyWireTotal         // pgwire: whole simple Query message handling
	HyGetAtCount        // not a duration: number of GetAt calls (ns field holds the count)
	hyNSpans
)

var hyNames = [hyNSpans]string{"wire_parse", "exec", "hy_total", "hy_plan", "hy_contract", "hy_dim", "hy_kernel", "hy_getat_sum", "hy_check_sum", "hy_mat_sum", "wire_encode", "wire_flush", "wire_total", "getat_calls(count)"}

const hySpanRing = 1 << 15

type hyRing struct {
	n atomic.Uint64
	v [hySpanRing]atomic.Int64
}

var hyRings [hyNSpans]hyRing

// HyRec records one sample of stage k.
func HyRec(k int, d time.Duration) { hyRecN(k, int64(d)) }

func hyRecN(k int, v int64) {
	r := &hyRings[k]
	i := r.n.Add(1) - 1
	r.v[i%hySpanRing].Store(v)
}

// HyHit reports whether the statement the session ran last was an R2 candidate (for the wire layer).
func (s *Session) HyHit() bool { return s.hy.hit }

func envOn(k string) bool { v := strings.TrimSpace(os.Getenv(k)); return v != "" && v != "0" }

func init() {
	if !HySpanOn {
		return
	}
	http.HandleFunc("/debug/hyspans", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reset") == "1" {
			for i := range hyRings {
				hyRings[i].n.Store(0)
			}
			fmt.Fprintln(w, "reset")
			return
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Fprintf(w, "mallocs %d\ntotal_alloc %d\nnum_gc %d\npause_total_ns %d\nheap_alloc %d\n", ms.Mallocs, ms.TotalAlloc, ms.NumGC, ms.PauseTotalNs, ms.HeapAlloc)
		for k := 0; k < hyNSpans; k++ {
			r := &hyRings[k]
			n := int(min(r.n.Load(), hySpanRing))
			if n == 0 {
				continue
			}
			xs := make([]int64, n)
			var sum int64
			for i := range xs {
				xs[i] = r.v[i].Load()
				sum += xs[i]
			}
			sort.Slice(xs, func(a, b int) bool { return xs[a] < xs[b] })
			if k == HyGetAtCount {
				fmt.Fprintf(w, "span %-20s n=%d p50=%d p99=%d mean=%.1f\n", hyNames[k], n, xs[n/2], xs[min(n-1, n*99/100)], float64(sum)/float64(n))
				continue
			}
			fmt.Fprintf(w, "span %-20s n=%d p50_us=%.1f p99_us=%.1f mean_us=%.1f\n", hyNames[k], n, float64(xs[n/2])/1e3, float64(xs[min(n-1, n*99/100)])/1e3, float64(sum)/float64(n)/1e3)
		}
	})
}

// hySpanShape: the FROM is a derived table, or a join whose leftmost item is one (the R2 shape, with the dimensions joined outside).
func hySpanShape(n FromNode) bool {
	for {
		switch x := n.(type) {
		case *FromSubquery:
			return true
		case *FromJoin:
			n = x.L
		default:
			return false
		}
	}
}
