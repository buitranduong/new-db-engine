package server

import (
	"fmt"
	"io"
	"net/http"
	"sort"

	"stask/internal/core"
	"stask/internal/sql"
)

// Operational metrics (docs/21 section 5): commit latency, queue depth,
// replication and view lag, memory per structure, lanes, cluster state. All
// read from counters or cheap accessors.
func (s *Server) opsMetrics(w io.Writer) {
	e := s.P.E
	p := func(name, typ, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, v)
	}
	b2f := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	// commit latency histogram
	cum, n, sum := e.Stats.CommitLat.Snapshot()
	fmt.Fprint(w, "# HELP stask_commit_latency_seconds Time from submitting a commit to its acknowledgement.\n# TYPE stask_commit_latency_seconds histogram\n")
	for i, b := range core.LatBounds {
		fmt.Fprintf(w, "stask_commit_latency_seconds_bucket{le=\"%g\"} %d\n", b, cum[i])
	}
	fmt.Fprintf(w, "stask_commit_latency_seconds_bucket{le=\"+Inf\"} %d\nstask_commit_latency_seconds_sum %g\nstask_commit_latency_seconds_count %d\n", cum[len(cum)-1], sum, n)
	p("stask_commit_queue_depth", "gauge", "Commit requests waiting for the committer (capacity 4096).", e.QueueDepth())

	p("stask_read_only", "gauge", "1 while the node refuses writes (replica, or cluster node that is not the leader).", b2f(e.ReadOnly()))
	p("stask_is_replica", "gauge", "1 on an asynchronous replica.", b2f(e.IsReplica()))
	if e.IsReplica() {
		p("stask_replica_lag_seconds", "gauge", "Age of the newest commit this replica has applied (the leader's clock).", e.ReplicaLag().Seconds())
	}
	if f := s.ReplicaInfo; f != nil {
		fmt.Fprint(w, "# HELP stask_replication_lag_bytes Log the leader holds durable that this replica has not been sent.\n# TYPE stask_replication_lag_bytes gauge\n")
		infos := f()
		for _, r := range infos {
			fmt.Fprintf(w, "stask_replication_lag_bytes{replica=%q} %d\n", r.ID, r.LagBytes)
		}
		fmt.Fprint(w, "# HELP stask_replication_connected 1 while the replica is connected.\n# TYPE stask_replication_connected gauge\n")
		for _, r := range infos {
			fmt.Fprintf(w, "stask_replication_connected{replica=%q} %d\n", r.ID, b2f(r.Connected))
		}
	}
	slots := e.Slots()
	if len(slots) > 0 {
		fmt.Fprint(w, "# HELP stask_cdc_slot_lag_bytes Log a CDC slot holds back.\n# TYPE stask_cdc_slot_lag_bytes gauge\n")
		for _, sl := range slots {
			fmt.Fprintf(w, "stask_cdc_slot_lag_bytes{slot=%q} %d\n", sl.Name, e.SlotLag(sl))
		}
	}
	if s.ClusterMetrics != nil {
		m := s.ClusterMetrics()
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			p("stask_"+k, "gauge", "Raft cluster state ("+k+").", m[k])
		}
	}

	// derived views: lag behind the log
	fmt.Fprint(w, "# HELP stask_view_lag_seconds How far a derived view is behind the log.\n# TYPE stask_view_lag_seconds gauge\n")
	vs := s.P.Views()
	for _, v := range vs {
		fmt.Fprintf(w, "stask_view_lag_seconds{view=%q} %g\n", v.Name, v.LagMs/1e3)
	}
	fmt.Fprint(w, "# HELP stask_view_ready 1 once the view has caught up after start.\n# TYPE stask_view_ready gauge\n")
	for _, v := range vs {
		fmt.Fprintf(w, "stask_view_ready{view=%q} %d\n", v.Name, b2f(v.Ready))
	}

	// memory per structure, per table
	fmt.Fprint(w, "# HELP stask_structure_bytes Memory of an in-RAM structure of a table (primary index, columnar columns).\n# TYPE stask_structure_bytes gauge\n")
	for _, t := range e.UserTables() {
		name := t.Schema().Name
		is := t.IndexStats()
		fmt.Fprintf(w, "stask_structure_bytes{table=%q,structure=\"primary_index\"} %d\n", name, is.Bytes+is.ArenaBytes)
		if s.P.Col.Has(t.Schema().ID) {
			fmt.Fprintf(w, "stask_structure_bytes{table=%q,structure=\"columnar\"} %d\n", name, s.P.Col.MemBytes(t.Schema().ID))
		}
	}

	// lanes (admission control)
	if a := sql.AdmissionReport(); a != nil {
		p("stask_lane_slots", "gauge", "Statements the admission control lets run at once.", a["slots"])
		p("stask_lane_heavy_slots", "gauge", "Slots of the heavy lane.", a["heavy_slots"])
		p("stask_lane_in_use", "gauge", "Admitted statements running.", a["in_use"])
		p("stask_lane_heavy_in_use", "gauge", "Heavy-lane slots in use.", a["heavy_in_use"])
		p("stask_lane_waiting", "gauge", "Statements waiting for admission.", a["waiting"])
		if cl, ok := a["classes"].(map[string]any); ok {
			names := make([]string, 0, len(cl))
			for k := range cl {
				names = append(names, k)
			}
			sort.Strings(names)
			fmt.Fprint(w, "# HELP stask_lane_wait_seconds_total Time statements of a class spent waiting for admission.\n# TYPE stask_lane_wait_seconds_total counter\n")
			for _, k := range names {
				if m, ok := cl[k].(map[string]any); ok {
					fmt.Fprintf(w, "stask_lane_wait_seconds_total{class=%q} %v\n", k, toF(m["wait_ms_total"])/1e3)
				}
			}
			fmt.Fprint(w, "# HELP stask_lane_queued_total Statements of a class that had to queue.\n# TYPE stask_lane_queued_total counter\n")
			for _, k := range names {
				if m, ok := cl[k].(map[string]any); ok {
					fmt.Fprintf(w, "stask_lane_queued_total{class=%q} %v\n", k, m["queued"])
				}
			}
		}
	}
	for _, f := range s.ExtraMetrics {
		f(w)
	}
}

func toF(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// GET /v1/ready: 200 once the node can do its job (engine open and not
// failed, derived views caught up, and in a cluster a leader is known),
// 503 with the reasons otherwise. /v1/health stays a liveness probe.
// ?write=1 additionally requires the node to take writes (use it for a load
// balancer that sends writes to this node: only the leader answers 200).
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	var why []string
	if err := s.P.E.Failed(); err != nil {
		_ = err
		why = append(why, "engine failed (see /v1/health and the log)")
	}
	for _, v := range s.P.Views() {
		if !v.Ready {
			why = append(why, "view "+v.Name+" is not caught up")
		}
	}
	if s.ClusterMetrics != nil {
		m := s.ClusterMetrics()
		if m["raft_leader_known"] == 0 {
			why = append(why, "no leader known")
		}
	}
	if r.URL.Query().Get("write") != "" && s.P.E.ReadOnly() {
		why = append(why, "node is read-only (not the leader)")
	}
	if len(why) > 0 {
		reply(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reasons": why, "read_only": s.P.E.ReadOnly()})
		return
	}
	reply(w, 200, map[string]any{"ready": true, "read_only": s.P.E.ReadOnly()})
}
