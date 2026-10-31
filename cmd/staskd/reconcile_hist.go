package main

import (
	"math"

	"stask/internal/core"
	"stask/internal/histstore"
	"stask/internal/reconcile"
)

// histReconcileView lets POST /v1/admin/reconcile compare the history store
// with the core (docs/33).
func histReconcileView(names []string, f *core.Follower, hs *histstore.Store) *reconcile.HistoryView {
	return &reconcile.HistoryView{
		Tables:   names,
		Follower: f,
		InGap:    hs.InGap,
		Scan: func(table uint32, maxLSN uint64, fn func(string, []byte, uint64, bool)) error {
			return hs.ScanTime(0, math.MaxInt64, func(e histstore.Entry) bool {
				if e.Table == table && e.LSN <= maxLSN {
					fn(e.Key, e.Val, e.LSN, e.Del)
				}
				return true
			})
		},
	}
}
