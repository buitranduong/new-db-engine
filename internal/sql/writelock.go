package sql

import (
	"errors"

	"stask/internal/core"
	"stask/internal/schema"
)

// Row locks of UPDATE and DELETE (docs/05 "Khoá mềm", docs/14 row RL1).
//
// PostgreSQL makes a writer wait for the transaction that wrote (or locked)
// the row first, then, at READ COMMITTED, re-evaluates the row on its newest
// version (EvalPlanQual): WHERE is checked again, SET is computed again, a
// row deleted meanwhile is skipped. STASK keeps the optimistic storage engine
// and adds the waiting in the soft lock table (locks.go):
//
//   - UPDATE and DELETE take a row lock on every row they selected before they
//     write anything: FOR NO KEY UPDATE (UPDATE that does not change the key)
//     or FOR UPDATE (DELETE, key change), held until the transaction ends.
//     They wait for conflicting holders (writers, SELECT ... FOR UPDATE/SHARE);
//     deadlocks give 40P01 and lock_timeout 55P03, as for SELECT ... FOR UPDATE;
//   - once a row is locked, no lock-taking writer can change it, so if a
//     version newer than the statement's snapshot exists the writer moves the
//     snapshot (core.Tx.Advance) and computes the statement again, nothing
//     having been written yet by it: rows changed by the other transaction are
//     seen as they are now. Where the snapshot cannot move (REPEATABLE READ,
//     SERIALIZABLE) the statement fails with 40001, as PostgreSQL does;
//   - the same for SELECT ... FOR UPDATE/SHARE, which used to fail with 40001
//     when a selected row was written after its snapshot.
//
// Not covered, they stay optimistic (conflict at COMMIT, 40001): INSERT (also
// ON CONFLICT DO UPDATE), MERGE, COPY, foreign-key and cascade writes, writes
// through views, statements on more than writeLockMaxRows rows (memory of the
// lock table), and rows locked inside a savepoint that is rolled back stay
// locked until the transaction ends.

// writeLockMaxRows bounds the row locks of one statement.
const writeLockMaxRows = 50000

// writeLocksOff turns the row locks of UPDATE/DELETE off (tests that stand in
// for writers that take none).
var writeLocksOff bool

// relockMax bounds how often one statement is computed again after waits.
const relockMax = 1000

// errRelock is returned by lockRow when the row it locked changed since the
// snapshot and the snapshot can move: run() computes the SELECT again.
var errRelock = errors.New("stask: row changed while waiting for its lock")

func serializeErr() error {
	return errf("40001", "could not serialize access due to concurrent update")
}

// sharedCursor reports whether an open cursor reads through the
// transaction's own snapshot, which must then stay put.
func (s *Session) sharedCursor() bool {
	if s.curs == nil || s.tx == nil {
		return false
	}
	for _, c := range s.curs.byName {
		if c.tx == s.tx {
			return true
		}
	}
	return false
}

// canAdvance says whether tx may take a newer snapshot now.
func (s *Session) canAdvance(tx *core.Tx) bool {
	if s.explicit {
		return tx == s.tx && tx.ReadCommitted() && !s.sharedCursor()
	}
	return true // single statement, Advance checks that nothing was written
}

// lockWriteRows locks rows of t for writing. retry=true: a row changed while
// waiting and the snapshot moved: compute the rows again and call it again.
func (s *Session) lockWriteRows(tx *core.Tx, t *schema.Table, rows []schema.Row, mode uint8) (retry bool, err error) {
	if writeLocksOff || len(rows) == 0 || len(rows) > writeLockMaxRows {
		return false, nil
	}
	o := s.txOwner()
	for _, r := range rows {
		key := t.KeyOf(r)
		if _, err := s.acquire(o, lockKey{spRow, t.Name, key}, mode, waitBlock); err != nil {
			return false, err
		}
		changed, err := tx.RowChanged(t.Name, key)
		if err != nil {
			return false, err
		}
		if changed {
			if s.canAdvance(tx) && tx.Advance() {
				return true, nil
			}
			if s.explicit {
				return false, serializeErr()
			}
			return false, core.ErrConflict
		}
	}
	return false, nil
}

// setTouchesKey reports whether an UPDATE assigns a key column.
func setTouchesKey(t *schema.Table, set []Assign) bool {
	for _, a := range set {
		ci, ok := t.Col(a.Col)
		if !ok {
			continue
		}
		if ci == t.KeyIndex() {
			return true
		}
		for _, k := range t.KeyCols {
			if k == a.Col {
				return true
			}
		}
	}
	return false
}

func updateLockMode(t *schema.Table, set []Assign) uint8 {
	if setTouchesKey(t, set) {
		return lmUpdate
	}
	return lmNoKeyUpdate
}

// lockedRows runs compute (the rows an UPDATE or DELETE will write) until
// the rows are locked and current.
func (s *Session) lockedRows(tx *core.Tx, t *schema.Table, mode uint8, compute func() ([]schema.Row, error)) error {
	for n := 0; ; n++ {
		rows, err := compute()
		if err != nil {
			return err
		}
		retry, err := s.lockWriteRows(tx, t, rows, mode)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
		if n >= relockMax {
			return serializeErr()
		}
	}
}

// run executes a statement; a SELECT with a locking clause is computed again
// when a row it locked changed since its snapshot (see the top of the file).
func (s *Session) run(tx *core.Tx, st Stmt, params []Datum) (*Result, error) {
	sel, ok := st.(*Select)
	if !ok || len(sel.Lock) == 0 {
		return s.run1(tx, st, params)
	}
	for n := 0; ; n++ {
		res, err := s.run1(tx, st, params)
		if !errors.Is(err, errRelock) {
			return res, err
		}
		if n >= relockMax || !tx.Advance() {
			return nil, serializeErr()
		}
	}
}
