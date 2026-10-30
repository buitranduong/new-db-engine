package core

import (
	"strings"

	"stask/internal/schema"
)

// ScanKeys visits the rows of table visible at the transaction's snapshot
// whose primary key k satisfies lo <(=) k <(=) hi, in key order, until fn
// returns false. hi == "" means no upper bound. Like ScanFrom it does not
// see the transaction's own writes (callers check HasWrites) and holds no
// lock between rows. The order is the byte order of the key, which is the
// SQL order only for a one-column string key under bytewise collation.
func (tx *Tx) ScanKeys(table, lo string, loIncl bool, hi string, hiIncl bool, fn func(key string, r schema.Row) bool) error {
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
	tr.ascend(lo, func(en *entry) bool {
		if tx.intr != nil {
			if n++; n == interruptRows {
				n = 0
				if tx.intr() {
					ierr = ErrInterrupted
					return false
				}
			}
		}
		key := tr.keyOf(en)
		if !loIncl && key == lo {
			return true
		}
		if hi != "" && (key > hi || key == hi && !hiIncl) {
			return false
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
		r, err := e.rowOf(t, v)
		if err != nil {
			ierr = err
			return false
		}
		return fn(strings.Clone(key), r)
	})
	return ierr
}
