package core

// Savepoint is the write set of a transaction at one moment (SAVEPOINT):
// RollbackTo puts the transaction back to it. Reads stay recorded, so a
// serializable transaction still validates what it read before rolling back.
type Savepoint struct {
	writes []pendingOp
	widx   map[pk]int
}

// Savepoint captures the transaction's writes.
func (tx *Tx) Savepoint() *Savepoint {
	sp := &Savepoint{writes: append([]pendingOp(nil), tx.writes...), widx: make(map[pk]int, len(tx.widx))}
	for k, v := range tx.widx {
		sp.widx[k] = v
	}
	return sp
}

// RollbackTo discards the writes made since sp (which stays valid for
// another rollback).
func (tx *Tx) RollbackTo(sp *Savepoint) {
	tx.writes = append(tx.writes[:0:0], sp.writes...)
	tx.widx = make(map[pk]int, len(sp.widx))
	for k, v := range sp.widx {
		tx.widx[k] = v
	}
	tx.lastT, tx.lastKey, tx.lastHead = nil, "", nil
}
