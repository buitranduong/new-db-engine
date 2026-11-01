package core

// Opt-in span instrumentation for the write path (docs/34 H6). STASK_SPANS=<ms> turns it on: every span of the commit
// pipeline (prepare, stage, fdatasync, install, snapshot wait for install, whole submit) and of the SQL layer (row-lock
// waits and holds, see internal/sql) that lasts at least <ms> milliseconds is logged as one line with its wall-clock start,
// so that it can be matched to the harness slow-transaction and import-batch log (-slow-log). The aggregated histograms of
// LatDebug (queue, hold, sync, install, ...) are switched on too. Off: one predictable branch per span site.

import (
	"log"
	"os"
	"strconv"
	"time"
)

// SpanMin is the logging threshold (0 = spans off).
var SpanMin = func() time.Duration {
	v, _ := strconv.Atoi(os.Getenv("STASK_SPANS"))
	if v <= 0 {
		return 0
	}
	LatDebug = true
	return time.Duration(v) * time.Millisecond
}()

// SpanOn reports whether span logging is enabled.
func SpanOn() bool { return SpanMin > 0 }

// SpanLog writes one line when d reaches the threshold: "span <kind> t=<unix ms of start> d=<ms> <detail>".
func SpanLog(kind string, start time.Time, d time.Duration, format string, args ...any) {
	if SpanMin <= 0 || d < SpanMin {
		return
	}
	log.Printf("span %s t=%d d=%.1f "+format, append([]any{kind, start.UnixMilli(), float64(d.Microseconds()) / 1000}, args...)...)
}

var _ = os.Getenv
