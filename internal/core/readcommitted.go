package core

// READ COMMITTED (PostgreSQL's default level). A transaction at this level
// takes a new snapshot for each statement (Refresh), so a statement sees
// what committed before it started plus the transaction's own writes. A
// write remembers the snapshot of the statement that first wrote its key
// (pendingOp.base), and the commit fails only if that key changed after it
// (first committer wins from the statement's snapshot, not the
// transaction's): STASK has no row locks that make a writer wait, so a
// concurrent update of a row the transaction wrote still ends in 40001, as
// at REPEATABLE READ, but the window is the statement, not the transaction.

// SetReadCommitted makes the transaction read committed. Call it before
// the first write.
func (tx *Tx) SetReadCommitted(on bool) { tx.rc = on && tx.iso == SnapshotIsolation }

// ReadCommitted reports whether Refresh moves the snapshot.
func (tx *Tx) ReadCommitted() bool { return tx.rc }

// Refresh moves a read committed transaction to a new snapshot (the next
// statement's). It does nothing at other levels.
func (tx *Tx) Refresh() {
	if !tx.rc || tx.done {
		return
	}
	snap, _ := tx.e.pinDurableIn(int(tx.pinShard))
	old := tx.snap
	tx.snap = snap
	tx.e.unpinIn(int(tx.pinShard), old)
	tx.lastT, tx.lastKey, tx.lastHead = nil, "", nil
}
