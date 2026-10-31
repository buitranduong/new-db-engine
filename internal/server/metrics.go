package server

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"stask/internal/core"
	"stask/internal/sql"
)

// GET /metrics: the counters of /v1/status and of the SQL layer in the
// Prometheus text exposition format, behind the same token as the rest of
// the API (a read-only token is enough). Everything is read from atomic
// counters; nothing is computed per scrape except the runtime memory
// statistics Go itself keeps. Docs: 05 §19.
//
// POST /v1/admin/scrub starts the CRC scrub (core/scrub.go); GET reports
// the running or last one.

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	e := s.P.E
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := func(name, typ, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, v)
	}
	b2f := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	p("stask_up", "gauge", "1 while the engine accepts writes, 0 after a write failure.", b2f(e.Failed() == nil))
	p("stask_uptime_seconds", "counter", "Seconds since the server started.", time.Since(s.start).Seconds())
	p("stask_durable_lsn", "gauge", "Newest LSN that is durable.", e.DurableLSN())
	p("stask_commits_total", "counter", "Transactions committed.", e.Stats.Commits.Load())
	p("stask_conflicts_total", "counter", "Transactions aborted by write conflicts.", e.Stats.Conflicts.Load())
	p("stask_commit_batches_total", "counter", "Group commits.", e.Stats.Batches.Load())
	p("stask_fsyncs_total", "counter", "fdatasync calls on the log.", e.Stats.Syncs.Load())
	p("stask_fsync_seconds_total", "counter", "Time spent in fdatasync.", float64(e.Stats.SyncNsTotal.Load())/1e9)
	p("stask_fsync_slow_total", "counter", "fdatasync calls slower than 50 ms.", e.Stats.SyncsOver50.Load())
	p("stask_wal_written_bytes_total", "counter", "Bytes appended to the log.", e.Log().BytesWritten())
	p("stask_wal_segments", "gauge", "Log segments on disk.", len(e.Log().Segments()))
	p("stask_wal_disk_bytes", "gauge", "Disk space of the log segments.", e.Log().DiskBytes())
	p("stask_checkpoints_total", "counter", "Checkpoints written.", e.Stats.Checkpoints.Load())
	p("stask_checkpoint_bytes_total", "counter", "Bytes written to checkpoints.", e.Stats.CkptBytes.Load())
	gc := e.GCStats()
	p("stask_gc_rounds_total", "counter", "Log reclamation rounds.", gc.Rounds)
	p("stask_gc_reclaimed_segments_total", "counter", "Log segments reclaimed.", gc.ReclaimedSegments)
	p("stask_gc_pinned_snapshots", "gauge", "Transaction snapshots holding back reclamation.", gc.PinnedSnapshots)
	p("stask_gc_oldest_snapshot_age_seconds", "gauge", "Age of the oldest stale transaction snapshot.", float64(gc.OldestSnapshotAgeMs)/1e3)
	if s.ExtraMetrics != nil {
		s.ExtraMetrics(p)
	}
	s.txMu.Lock()
	open := len(s.txs)
	s.txMu.Unlock()
	p("stask_http_open_transactions", "gauge", "Transactions open over the HTTP API.", open)
	tables := 0
	var rows int64
	for _, t := range e.UserTables() {
		tables++
		rows += int64(t.LiveRows())
	}
	p("stask_tables", "gauge", "User tables.", tables)
	p("stask_live_rows", "gauge", "Live rows in all user tables.", rows)

	sc := e.ScrubStatus()
	p("stask_scrub_running", "gauge", "1 while a CRC scrub runs.", b2f(sc.Running))
	p("stask_scrub_errors_total", "counter", "Corruptions scrubs have found since start.", core.ScrubErrorsTotal())
	if !sc.Started.IsZero() {
		p("stask_scrub_last_start_timestamp_seconds", "gauge", "Start of the last scrub.", sc.Started.Unix())
		p("stask_scrub_last_ok", "gauge", "1 if the last finished scrub found nothing wrong.", b2f(sc.OK()))
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms) // brief stop-the-world; Prometheus scrapes are seconds apart
	p("stask_go_heap_alloc_bytes", "gauge", "Go heap in use.", ms.HeapAlloc)
	p("stask_go_goroutines", "gauge", "Goroutines.", runtime.NumGoroutine())
	sql.WriteMetrics(w, e)
}

func (s *Server) scrub(w http.ResponseWriter, r *http.Request) {
	e := s.P.E
	q := r.URL.Query()
	if r.Method == http.MethodGet {
		reply(w, 200, e.ScrubStatus())
		return
	}
	if q.Get("cancel") != "" {
		e.StopScrub()
		reply(w, 200, e.ScrubStatus())
		return
	}
	rate := int64(64) // MB/s
	if v := q.Get("rate_mb"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			reply(w, http.StatusBadRequest, map[string]any{"error": "rate_mb must be a number of MB/s (0 = unthrottled)"})
			return
		}
		rate = n
	}
	if !e.StartScrub(rate << 20) {
		reply(w, http.StatusConflict, map[string]any{"error": "a scrub is already running", "status": e.ScrubStatus()})
		return
	}
	if q.Get("wait") != "" {
		for e.ScrubStatus().Running {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		st := e.ScrubStatus()
		code := 200
		if !st.OK() {
			code = http.StatusInternalServerError // corruption found
		}
		reply(w, code, st)
		return
	}
	reply(w, http.StatusAccepted, e.ScrubStatus())
}
