package reconcile

import (
	"fmt"
	"time"

	"stask/internal/core"
)

// maxSlotScan bounds how much log a slot comparison reads.
const maxSlotScan = 2_000_000

// slot compares a logical slot with the log: its confirmed position and lag,
// and for each table whether the slot still has changes to deliver for it
// (the question a consumer that "looks stuck" needs answered: is it behind
// on this table, or only on the log in general).
func (r *runner) slot(name string, tabs []*core.TableState) (*SlotReport, error) {
	var info *core.SlotInfo
	for _, s := range r.e.Slots() {
		if s.Name == name {
			s := s
			info = &s
		}
	}
	if info == nil {
		return nil, fmt.Errorf("no such replication slot: %s", name)
	}
	d := r.e.DurableLSN()
	sr := &SlotReport{Name: info.Name, Plugin: info.Plugin, ConfirmedLSN: info.LSN, DurableLSN: d, Invalid: info.Invalid,
		LagBytes: r.e.SlotLag(*info), Status: StInSync}
	if d > info.LSN {
		sr.BehindRecs = d - info.LSN
	}
	if info.Invalid != "" {
		sr.Status = "INVALID"
		sr.Detail = "the slot was invalidated (" + info.Invalid + "): a consumer must resynchronise from a snapshot; changes before the invalidation are gone"
		return sr, nil
	}
	any := false
	for _, ts := range tabs {
		sp, err := r.e.SlotPendingFor(*info, ts.Schema().ID, maxSlotScan)
		if err != nil {
			return nil, err
		}
		if sp.LogMissing {
			sr.Status = "LOG_MISSING"
			sr.Detail = "the log after the slot's position was reclaimed: the consumer cannot continue"
			return sr, nil
		}
		st := SlotTable{Table: ts.Schema().Name, PendingTx: sp.TableRecords, PendingOp: sp.TableOps, Status: StInSync}
		if sp.OldestTableTS > 0 {
			st.OldestMs = max(0, time.Now().UnixNano()-sp.OldestTableTS) / 1e6
		}
		if sp.TableOps > 0 {
			st.Status = StBehind
			any = true
		}
		sr.Truncated = sr.Truncated || sp.Truncated
		sr.Tables = append(sr.Tables, st)
	}
	if any {
		sr.Status = StBehind
		sr.Detail = "the consumer has not confirmed changes that are already committed"
	} else if sr.BehindRecs > 0 {
		sr.Detail = "the slot is behind the log only by records of other tables (or DDL)"
	}
	if sr.Truncated {
		sr.Detail = join(sr.Detail, fmt.Sprintf("pending counts stop at %d log records (lower bounds)", maxSlotScan))
	}
	return sr, nil
}
