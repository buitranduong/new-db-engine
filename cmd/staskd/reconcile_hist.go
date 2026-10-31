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
		Newest: func(table uint32, key string, maxLSN uint64) ([]byte, uint64, bool, bool, error) {
			all, err := hs.Versions(table, key, 0, math.MaxInt64)
			if err != nil {
				return nil, 0, false, false, err
			}
			for i := len(all) - 1; i >= 0; i-- {
				if all[i].LSN <= maxLSN {
					return all[i].Val, all[i].LSN, all[i].Del, true, nil
				}
			}
			return nil, 0, false, false, nil
		},
	}
}
