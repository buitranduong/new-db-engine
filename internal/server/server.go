// Package server exposes the platform over HTTP/JSON.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/buildinfo"
	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/proj"
	"stask/internal/repl"
	"stask/internal/schema"
	"stask/internal/sql"
)

type Server struct {
	P     *planner.Platform
	Fault *Fault
	// Cluster, if set, answers GET /v1/cluster with the node's role in a
	// Raft cluster (internal/cluster); clients and the router find the
	// leader with it.
	Cluster func() any
	// ClusterMetrics, if set, adds the node's Raft state to /metrics (name
	// without the stask_ prefix -> value).
	ClusterMetrics func() map[string]float64
	// ExtraMetrics write more Prometheus text (history store, replication
	// leader); each is called on every scrape.
	ExtraMetrics []func(io.Writer)
	// ReplicaInfo, if set (a replication leader), lists its replicas.
	ReplicaInfo func() []repl.ReplicaInfo

	txMu  sync.Mutex
	txs   map[uint64]*txEntry
	txSeq atomic.Uint64
	start time.Time
}

type txEntry struct {
	tx   *core.Tx
	mu   sync.Mutex
	used time.Time
}

func New(p *planner.Platform, f *Fault) *Server {
	s := &Server{P: p, Fault: f, txs: map[uint64]*txEntry{}, start: time.Now()}
	go s.reapTx()
	return s
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/tables", s.createTable)
	m.HandleFunc("GET /v1/tables", s.listTables)
	m.HandleFunc("POST /v1/write", s.write)
	m.HandleFunc("GET /v1/get", s.get)
	m.HandleFunc("GET /v1/history", s.history)
	m.HandleFunc("POST /v1/query", s.query)
	m.HandleFunc("POST /v1/tx/begin", s.txBegin)
	m.HandleFunc("POST /v1/tx/{id}/{op}", s.txOp)
	m.HandleFunc("GET /v1/status", s.status)
	m.HandleFunc("GET /v1/health", s.health)
	m.HandleFunc("GET /v1/ready", s.ready)
	m.HandleFunc("GET /v1/cluster", s.cluster)
	m.HandleFunc("POST /v1/admin/checkpoint", s.checkpoint)
	m.HandleFunc("POST /v1/admin/gc", s.gcRound)
	m.HandleFunc("GET /metrics", s.metrics)
	m.HandleFunc("POST /v1/admin/scrub", s.scrub)
	m.HandleFunc("GET /v1/admin/scrub", s.scrub)
	s.cdcRoutes(m)
	if s.Fault != nil {
		m.HandleFunc("POST /v1/admin/fault", s.setFault)
	}
	return s.minLSN(m)
}

// Read-your-writes (docs/06 section 2a). A write or a transaction commit
// answers with the header X-Stask-Lsn. A client that sends X-Stask-Min-Lsn
// on a later request (to any node, usually a replica) has the node wait
// until it has applied that LSN before the request runs, at most
// X-Stask-Min-Lsn-Timeout milliseconds (default 5000); after that the
// answer is 503 with Retry-After and nothing has run.
const (
	HeaderNotLeader  = "X-Stask-Not-Leader"
	HeaderLSN        = "X-Stask-Lsn"
	HeaderMinLSN     = "X-Stask-Min-Lsn"
	HeaderMinTimeout = "X-Stask-Min-Lsn-Timeout"
)

func (s *Server) minLSN(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(HeaderMinLSN); v != "" {
			lsn, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				reply(w, http.StatusBadRequest, map[string]any{"error": "bad " + HeaderMinLSN})
				return
			}
			timeout := 5 * time.Second
			if t := r.Header.Get(HeaderMinTimeout); t != "" {
				if ms, err := strconv.ParseInt(t, 10, 64); err == nil && ms >= 0 {
					timeout = time.Duration(ms) * time.Millisecond
				}
			}
			if err := s.P.E.WaitDurable(lsn, timeout); err != nil {
				w.Header().Set("Retry-After", "1")
				w.Header().Set(HeaderLSN, strconv.FormatUint(s.P.E.DurableLSN(), 10))
				reply(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error(), "durable_lsn": s.P.E.DurableLSN()})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	return d.Decode(v)
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func reply(w http.ResponseWriter, code int, v any) {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	json.NewEncoder(b).Encode(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b.Bytes())
	bufPool.Put(b)
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrExists):
		code = http.StatusConflict
	case errors.Is(err, core.ErrNoTable):
		code = http.StatusNotFound
	case errors.Is(err, core.ErrSnapshotTooOld):
		code = http.StatusGone
	case errors.Is(err, core.ErrUnknownOutcome):
		code = http.StatusServiceUnavailable
	case errors.Is(err, core.ErrReadOnly):
		// nothing ran: not the leader (or not yet), try the leader
		w.Header().Set(HeaderNotLeader, "1")
		code = http.StatusServiceUnavailable
	}
	reply(w, code, map[string]any{"error": err.Error()})
}

func (s *Server) createTable(w http.ResponseWriter, r *http.Request) {
	var t schema.Table
	if err := decode(r, &t); err != nil {
		fail(w, err)
		return
	}
	out, err := s.P.E.CreateTable(t)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, out)
}

func (s *Server) listTables(w http.ResponseWriter, r *http.Request) {
	var out []any
	for _, t := range s.P.E.UserTables() {
		out = append(out, map[string]any{"schema": t.Schema(), "live_rows": t.LiveRows()})
	}
	reply(w, 200, out)
}

type writeReq struct {
	Ops []struct {
		Op    string         `json:"op"` // put | insert | delete
		Table string         `json:"table"`
		Row   map[string]any `json:"row,omitempty"`
		Key   any            `json:"key,omitempty"`
	} `json:"ops"`
}

func keyString(k any) (string, error) {
	switch v := k.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	}
	return "", fmt.Errorf("key must be a string or integer")
}

func (s *Server) toOps(req *writeReq) ([]core.WriteOp, error) {
	ops := make([]core.WriteOp, 0, len(req.Ops))
	for _, o := range req.Ops {
		t, err := s.P.E.UserTable(o.Table)
		if err != nil {
			return nil, err
		}
		switch o.Op {
		case "put", "insert", "":
			row, err := t.Schema().FromJSON(o.Row)
			if err != nil {
				return nil, err
			}
			ops = append(ops, core.WriteOp{Table: o.Table, Row: row, IfAbsent: o.Op == "insert"})
		case "delete":
			k, err := keyString(o.Key)
			if err != nil {
				return nil, err
			}
			ops = append(ops, core.WriteOp{Table: o.Table, Key: k})
		default:
			return nil, fmt.Errorf("unknown op %q", o.Op)
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("no ops")
	}
	return ops, nil
}

// write commits all ops atomically. The returned lsn is the session token
// for read-your-writes on derived views.
func (s *Server) write(w http.ResponseWriter, r *http.Request) {
	var req writeReq
	if err := decode(r, &req); err != nil {
		fail(w, err)
		return
	}
	ops, err := s.toOps(&req)
	if err != nil {
		fail(w, err)
		return
	}
	lsn, err := s.P.E.Write(ops)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set(HeaderLSN, strconv.FormatUint(lsn, 10))
	reply(w, 200, map[string]any{"lsn": lsn})
}

// get: strong point read from the core; optional as_of_lsn / as_of_time
// (RFC3339 or unix ms) for historical reads.
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	table, key := q.Get("table"), q.Get("key")
	t, err := s.P.E.UserTable(table)
	if err != nil {
		fail(w, err)
		return
	}
	var row schema.Row
	var v *core.Version
	switch {
	case q.Get("as_of_lsn") != "":
		lsn, perr := strconv.ParseUint(q.Get("as_of_lsn"), 10, 64)
		if perr != nil {
			fail(w, perr)
			return
		}
		row, v, err = s.P.E.GetAt(table, key, lsn)
	case q.Get("as_of_time") != "":
		at := q.Get("as_of_time")
		var ns int64
		if tm, perr := time.Parse(time.RFC3339Nano, at); perr == nil {
			ns = tm.UnixNano()
		} else if msv, perr := strconv.ParseInt(at, 10, 64); perr == nil {
			ns = msv * int64(time.Millisecond)
		} else {
			fail(w, fmt.Errorf("bad as_of_time"))
			return
		}
		row, v, err = s.P.E.GetAsOfTime(table, key, ns)
	default:
		row, v, err = s.P.E.Get(table, key)
	}
	if err != nil {
		fail(w, err)
		return
	}
	if row == nil {
		reply(w, 404, map[string]any{"found": false})
		return
	}
	ts, err := s.P.E.VersionTS(v)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]any{"found": true, "row": t.Schema().ToJSON(row), "lsn": v.LSN, "commit_ts": ts})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	t, err := s.P.E.UserTable(q.Get("table"))
	if err != nil {
		fail(w, err)
		return
	}
	h, err := s.P.E.History(q.Get("table"), q.Get("key"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, len(h))
	for i, e := range h {
		out[i] = map[string]any{"lsn": e.LSN, "commit_ts": e.TS, "deleted": e.Deleted}
		if e.Row != nil {
			out[i]["row"] = t.Schema().ToJSON(e.Row)
		}
	}
	reply(w, 200, map[string]any{"versions": out})
}

type queryReq struct {
	Kind        string              `json:"kind"` // aggregate | search | timeseries | scan
	Consistency planner.Consistency `json:"consistency"`
	Aggregate   *proj.AggQuery      `json:"aggregate,omitempty"`
	Search      *proj.SearchQuery   `json:"search,omitempty"`
	Fetch       bool                `json:"fetch,omitempty"`
	TimeSeries  *proj.TSQuery       `json:"timeseries,omitempty"`
	Scan        *struct {
		Table string `json:"table"`
		Start string `json:"start"`
		End   string `json:"end"`
		Limit int    `json:"limit"`
	} `json:"scan,omitempty"`
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	var q queryReq
	if err := decode(r, &q); err != nil {
		fail(w, err)
		return
	}
	switch {
	case q.Kind == "aggregate" && q.Aggregate != nil:
		res, pl, err := s.P.Aggregate(q.Aggregate, q.Consistency)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"result": res, "plan": pl})
	case q.Kind == "timeseries" && q.TimeSeries != nil:
		res, pl, err := s.P.TimeSeries(q.TimeSeries, q.Consistency)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"result": res, "plan": pl})
	case q.Kind == "search" && q.Search != nil:
		hits, exp, pl, err := s.P.Search(q.Search, q.Consistency, q.Fetch)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]any{"hits": hits, "expanded": exp, "plan": pl})
	case q.Kind == "scan" && q.Scan != nil:
		t0 := time.Now()
		t, err := s.P.E.UserTable(q.Scan.Table)
		if err != nil {
			fail(w, err)
			return
		}
		limit := q.Scan.Limit
		if limit <= 0 {
			limit = 100
		}
		snap := s.P.E.DurableLSN()
		rows, err := s.P.E.ScanRange(q.Scan.Table, q.Scan.Start, q.Scan.End, limit, snap)
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, len(rows))
		for i, row := range rows {
			out[i] = t.Schema().ToJSON(row)
		}
		reply(w, 200, map[string]any{"rows": out, "plan": planner.Plan{Engine: "core", Mode: "strong",
			Reason: "ordered range scan on primary index", DurableLSN: snap, ExecMs: float64(time.Since(t0).Microseconds()) / 1000}})
	default:
		fail(w, fmt.Errorf("query needs kind and the matching body"))
	}
}

// ---- interactive transactions ----

func (s *Server) txBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Isolation string `json:"isolation"`
	}
	decode(r, &req)
	iso := core.SnapshotIsolation
	switch req.Isolation {
	case "", "snapshot", "si":
	case "serializable":
		iso = core.Serializable
	default:
		fail(w, fmt.Errorf("unknown isolation %q", req.Isolation))
		return
	}
	id := s.txSeq.Add(1)
	tx := s.P.E.Begin(iso)
	s.txMu.Lock()
	s.txs[id] = &txEntry{tx: tx, used: time.Now()}
	s.txMu.Unlock()
	reply(w, 200, map[string]any{"tx": id, "snapshot_lsn": tx.Snapshot()})
}

func (s *Server) txOp(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, err)
		return
	}
	s.txMu.Lock()
	te := s.txs[id]
	s.txMu.Unlock()
	if te == nil {
		reply(w, 404, map[string]any{"error": "no such transaction (finished or expired)"})
		return
	}
	te.mu.Lock()
	defer te.mu.Unlock()
	te.used = time.Now()
	finish := func() {
		s.txMu.Lock()
		delete(s.txs, id)
		s.txMu.Unlock()
	}
	switch r.PathValue("op") {
	case "get":
		var req struct {
			Table string `json:"table"`
			Key   any    `json:"key"`
		}
		if err := decode(r, &req); err != nil {
			fail(w, err)
			return
		}
		k, err := keyString(req.Key)
		if err != nil {
			fail(w, err)
			return
		}
		t, err := s.P.E.UserTable(req.Table)
		if err != nil {
			fail(w, err)
			return
		}
		row, ok, err := te.tx.Get(req.Table, k)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			reply(w, 200, map[string]any{"found": false})
			return
		}
		reply(w, 200, map[string]any{"found": true, "row": t.Schema().ToJSON(row)})
	case "scan":
		var req struct {
			Table string `json:"table"`
			Start string `json:"start"`
			End   string `json:"end"`
			Limit int    `json:"limit"`
		}
		if err := decode(r, &req); err != nil {
			fail(w, err)
			return
		}
		t, err := s.P.E.UserTable(req.Table)
		if err != nil {
			fail(w, err)
			return
		}
		rows, err := te.tx.Scan(req.Table, req.Start, req.End, max(req.Limit, 1))
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, len(rows))
		for i, row := range rows {
			out[i] = t.Schema().ToJSON(row)
		}
		reply(w, 200, map[string]any{"rows": out})
	case "write":
		var req writeReq
		if err := decode(r, &req); err != nil {
			fail(w, err)
			return
		}
		ops, err := s.toOps(&req)
		if err != nil {
			fail(w, err)
			return
		}
		for _, op := range ops {
			switch {
			case op.Row == nil:
				err = te.tx.Delete(op.Table, op.Key)
			case op.IfAbsent:
				err = te.tx.Insert(op.Table, op.Row)
			default:
				err = te.tx.Put(op.Table, op.Row)
			}
			if err != nil {
				fail(w, err)
				return
			}
		}
		reply(w, 200, map[string]any{"ok": true})
	case "commit":
		finish()
		lsn, err := te.tx.Commit()
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set(HeaderLSN, strconv.FormatUint(lsn, 10))
		reply(w, 200, map[string]any{"lsn": lsn})
	case "abort":
		finish()
		te.tx.Abort()
		reply(w, 200, map[string]any{"ok": true})
	default:
		fail(w, fmt.Errorf("unknown tx op"))
	}
}

func (s *Server) reapTx() {
	for range time.Tick(5 * time.Second) {
		s.txMu.Lock()
		for id, te := range s.txs {
			if te.mu.TryLock() {
				if time.Since(te.used) > 30*time.Second {
					te.tx.Abort()
					delete(s.txs, id)
				}
				te.mu.Unlock()
			}
		}
		s.txMu.Unlock()
	}
}

// ---- status / admin ----

// health answers without credentials (see Auth.Wrap), so it says only
// whether the engine accepts writes: 200 {"ok":true} or 503 {"ok":false}.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if s.P.E.Failed() != nil {
		reply(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	reply(w, 200, map[string]any{"ok": true})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	e := s.P.E
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	tables := map[string]any{}
	for _, t := range e.UserTables() {
		info := map[string]any{"live_rows": t.LiveRows(), "index": t.IndexStats(), "columnar": t.Schema().Columnar}
		if t.Schema().Columnar && !s.P.ColF.Ready() {
			info["columnar_bootstrapping"] = true
		}
		if s.P.Col.Has(t.Schema().ID) {
			stored, live := s.P.Col.Stats(t.Schema().ID)
			info["columnar_rows"], info["columnar_live"] = stored, live
			info["columnar_bytes"] = s.P.Col.MemBytes(t.Schema().ID)
		}
		if s.P.Srch.Has(t.Schema().ID) {
			docs, terms := s.P.Srch.Stats(t.Schema().ID)
			info["search_docs"], info["search_terms"] = docs, terms
		}
		if s.P.TS.Has(t.Schema().ID) {
			info["timeseries"] = s.P.TS.Stats(t.Schema().ID)
		}
		tables[t.Schema().Name] = info
	}
	s.txMu.Lock()
	openTx := len(s.txs)
	s.txMu.Unlock()
	failed := ""
	if err := e.Failed(); err != nil {
		failed = err.Error()
	}
	reply(w, 200, map[string]any{
		"durable_lsn":        e.DurableLSN(),
		"uptime_s":           time.Since(s.start).Seconds(),
		"write_failed":       failed,
		"commits":            e.Stats.Commits.Load(),
		"conflicts":          e.Stats.Conflicts.Load(),
		"batches":            e.Stats.Batches.Load(),
		"syncs":              e.Stats.Syncs.Load(),
		"payload_bytes":      e.Stats.PayloadBytes.Load(),
		"wal_bytes":          e.Log().BytesWritten(),
		"sync_max_ms":        float64(e.Stats.SyncNsMax.Load()) / 1e6,
		"syncs_over_50ms":    e.Stats.SyncsOver50.Load(),
		"sync_ns_total":      e.Stats.SyncNsTotal.Load(),
		"wal_prezero_bytes":  e.Log().ZeroBytes(),
		"wal_segments":       len(e.Log().Segments()),
		"checkpoints":        e.Stats.Checkpoints.Load(),
		"checkpoint_bytes":   e.Stats.CkptBytes.Load(),
		"disk_version_loads": e.Stats.DiskLoads.Load(),
		"recovery":           e.Stats.Recovery,
		"gc":                 e.GCStats(),
		"compression": map[string]any{
			"segments":    e.Stats.CompressedSegs.Load(),
			"raw_bytes":   e.Stats.CompressRawBytes.Load(),
			"file_bytes":  e.Stats.CompressFileBytes.Load(),
			"block_loads": e.Log().ZBlockLoads.Load(),
			"wal_disk":    e.Log().DiskBytes(),
		},
		"admission": sql.AdmissionReport(),
		"views":     s.P.Views(),
		"tables":    tables,
		"heap_mb":   ms.HeapAlloc >> 20,
		"open_tx":   openTx,
		"build":     buildinfo.Read(),
	})
}

func (s *Server) gcRound(w http.ResponseWriter, r *http.Request) {
	n, err := s.P.E.GCRound()
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]any{"reclaimed_segments": n, "gc": s.P.E.GCStats()})
}

func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request) {
	if err := s.P.E.Checkpoint(); err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]any{"ok": true})
}

// Fault injects storage failures; enabled only with -enable-faults.
type Fault struct {
	createENOSPC atomic.Bool
	writeENOSPC  atomic.Bool
	syncEIO      atomic.Bool
}

type errno string

func (e errno) Error() string { return string(e) }

const (
	errNoSpace = errno("no space left on device (injected)")
	errIO      = errno("input/output error (injected)")
)

func (f *Fault) BeforeCreate() error {
	if f.createENOSPC.Load() {
		return errNoSpace
	}
	return nil
}

func (f *Fault) BeforeWrite(n int) (int, error) {
	if f.writeENOSPC.Load() {
		return n / 2, errNoSpace // partial write, then failure
	}
	return n, nil
}

func (f *Fault) BeforeSync() error {
	if f.syncEIO.Load() {
		return errIO
	}
	return nil
}

func (s *Server) setFault(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CreateENOSPC *bool `json:"create_enospc"`
		WriteENOSPC  *bool `json:"write_enospc"`
		SyncEIO      *bool `json:"sync_eio"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.CreateENOSPC != nil {
		s.Fault.createENOSPC.Store(*req.CreateENOSPC)
	}
	if req.WriteENOSPC != nil {
		s.Fault.writeENOSPC.Store(*req.WriteENOSPC)
	}
	if req.SyncEIO != nil {
		s.Fault.syncEIO.Store(*req.SyncEIO)
	}
	reply(w, 200, map[string]any{"create_enospc": s.Fault.createENOSPC.Load(), "write_enospc": s.Fault.writeENOSPC.Load(), "sync_eio": s.Fault.syncEIO.Load()})
}

func (s *Server) cluster(w http.ResponseWriter, r *http.Request) {
	if s.Cluster == nil {
		reply(w, http.StatusNotFound, map[string]any{"error": "not a cluster node"})
		return
	}
	reply(w, 200, s.Cluster())
}
