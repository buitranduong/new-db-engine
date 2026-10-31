package server

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"stask/internal/reconcile"
)

var reconciling atomic.Bool

// reconcileHandler serves POST /v1/admin/reconcile (full-access credential,
// audited like the other admin calls):
//
//	table=a,b   only these tables (default: all)
//	sample=N    0 (default): LSNs and row counts only; 1: every key;
//	            N: the keys whose hash is a multiple of N
//	slot=NAME   also compare this CDC slot with each table
//	retries=N   extra passes when a structure moves under the check
//	pause_ms=N  sleep between chunks
//
// One run at a time. The run reads in chunks of a few thousand rows and holds
// no lock across chunks (docs/33).
func (s *Server) reconcileHandler(w http.ResponseWriter, r *http.Request) {
	if !reconciling.CompareAndSwap(false, true) {
		reply(w, http.StatusConflict, map[string]any{"error": "a reconcile run is already in progress"})
		return
	}
	defer reconciling.Store(false)
	q := r.URL.Query()
	o := reconcile.Options{Slot: q.Get("slot")}
	if v := q.Get("table"); v != "" {
		o.Tables = strings.Split(v, ",")
	}
	num := func(name string, dst *int) bool {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				reply(w, http.StatusBadRequest, map[string]any{"error": name + " must be a non-negative number"})
				return false
			}
			*dst = n
		}
		return true
	}
	var pause int
	if !num("sample", &o.Sample) || !num("retries", &o.Retries) || !num("pause_ms", &pause) || !num("limit", &o.Limit) {
		return
	}
	o.Pause = time.Duration(pause) * time.Millisecond
	env := reconcile.Env{P: s.P}
	if s.ReconcileHistory != nil {
		env.History = s.ReconcileHistory()
	}
	rep, err := reconcile.Run(r.Context(), env, o)
	if err != nil {
		reply(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	reply(w, http.StatusOK, rep)
}
