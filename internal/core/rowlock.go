package core

import (
	"strings"

	"stask/internal/schema"
)

// Row-lock support for the SQL layer (docs/05, SELECT ... FOR UPDATE) and
// resumable scans for server-side cursors. Nothing here changes the log or
// the commit protocol: LockRead only adds a read that the committer already
// validates, and ScanFrom is ScanSnapshot starting after a key.

// LockRead records that the transaction relies on row key of table staying
// as it is in the snapshot: at commit the transaction fails with ErrConflict
// if the row was written by a transaction that committed after the snapshot
// (the same check a serializable read gets). changed reports that this has
// already happened (the row's newest version is newer than the snapshot);
// the read is then not recorded and the caller decides (error, retry, skip).
func (tx *Tx) LockRead(table, key string) (changed bool, err error) {
	if err := tx.check(); err != nil {
		return false, err
	}
	t, err := tx.e.Table(table)
	if err != nil {
		return false, err
	}
	if h := t.head(key); h != nil && h.LSN > tx.snap {
		return true, nil
	}
	tx.reads = append(tx.reads, readKey{t: t, key: key})
	return false, nil
}

// ScanFrom visits the rows of table visible at the transaction's snapshot
// whose key sorts after the key after ("" = from the start), in key order,
// until fn returns false. It does not see the transaction's own writes
// (check HasWrites) and holds no lock between calls, so a cursor can pause
// between batches and resume with the last key it saw.
func (tx *Tx) ScanFrom(table, after string, fn func(key string, r schema.Row) bool) error {
	if err := tx.check(); err != nil {
		return err
	}
	if tx.iso == Serializable {
		return ErrScanSerializable
	}
	e := tx.e
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	e.waitInstalled(tx.snap)
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshotT(t, tx.snap); err != nil {
		return err
	}
	tr := t.index()
	var ierr error
	n := 0
	tr.ascend(after, func(en *entry) bool {
		if tx.intr != nil {
			if n++; n == interruptRows {
				n = 0
				if tx.intr() {
					ierr = ErrInterrupted
					return false
				}
			}
		}
		h := en.version()
		v, err := e.visible(&h, tx.snap)
		if err != nil {
			ierr = err
			return false
		}
		if v == nil || v.Deleted {
			return true
		}
		key := tr.keyOf(en)
		if after != "" && key == after {
			return true
		}
		r, err := e.rowOf(t, v)
		if err != nil {
			ierr = err
			return false
		}
		return fn(strings.Clone(key), r)
	})
	return ierr
}
