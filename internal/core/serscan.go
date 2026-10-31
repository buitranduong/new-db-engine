package core

// Serializable scans. The prototype has no predicate or range locks, so a
// serializable transaction that scans a table is validated at table
// granularity: the scan is recorded, and the commit fails with
// ErrConflict (40001) if any other transaction committed a write to that
// table after the transaction's snapshot. That is conservative (PostgreSQL's
// SSI also reports false positives, but fewer): no anomaly can slip through,
// and a transaction that only reads tables nobody writes meanwhile, or that
// only does point reads, is not affected. Point reads are still validated
// per key (Tx.reads). The writes of the transaction itself do not count.
// A read-only serializable transaction reads one snapshot, which is
// consistent by itself, so its scans are not validated.

// noteScan records that a serializable transaction scanned t.
func (tx *Tx) noteScan(t *TableState) {
	if tx.iso != Serializable {
		return
	}
	for _, s := range tx.scans {
		if s == t {
			return
		}
	}
	tx.scans = append(tx.scans, t)
}

// noteTableWrites runs in the committer when a request gets its LSN.
func (e *Engine) noteTableWrites(ops []pendingOp, lsn uint64) {
	if e.tabLast == nil {
		e.tabLast = map[*TableState]uint64{}
	}
	var last *TableState
	for i := range ops {
		if t := ops[i].t; t != last {
			e.tabLast[t] = lsn
			last = t
		}
	}
}

// checkScans runs in the committer: a table scanned by the request that
// another request wrote after the snapshot fails it.
func (e *Engine) checkScans(r *commitReq) error {
	for _, t := range r.scans {
		if e.tabLast[t] > r.snap || t.dropping.Load() {
			return ErrConflict
		}
	}
	return nil
}
