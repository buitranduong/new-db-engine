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
	tx.moveSnap()
}

func (tx *Tx) moveSnap() {
	snap, _ := tx.e.pinDurableIn(int(tx.pinShard))
	old := tx.snap
	tx.snap = snap
	tx.e.unpinIn(int(tx.pinShard), old)
	tx.lastT, tx.lastKey, tx.lastHead = nil, "", nil
}

// Advance moves the transaction to a new snapshot after a row lock wait
// (writelock.go in package sql): a READ COMMITTED transaction at any point, a
// single-statement (implicit) transaction that has written nothing yet. It
// reports false when the snapshot must stay (REPEATABLE READ, SERIALIZABLE,
// an implicit transaction with writes).
func (tx *Tx) Advance() bool {
	if tx.done {
		return false
	}
	if tx.rc || (tx.implicit && tx.iso == SnapshotIsolation && len(tx.writes) == 0) {
		tx.moveSnap()
		return true
	}
	return false
}

// RowChanged reports whether a version of key newer than the transaction's
// snapshot was committed, for a row this transaction has not written itself.
// A writer that waited for a row lock uses it to see whether the row it
// selected is still the current one (PostgreSQL then re-evaluates it).
func (tx *Tx) RowChanged(table, key string) (bool, error) {
	if err := tx.check(); err != nil {
		return false, err
	}
	t, err := tx.e.Table(table)
	if err != nil {
		return false, err
	}
	if _, own := tx.widx[pk{t, key}]; own {
		return false, nil
	}
	h := t.head(key)
	return h != nil && h.LSN > tx.snap, nil
}
