package core

import (
	"slices"
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
	t, err := tx.e.Table(table)
	if err != nil {
		return err
	}
	return tx.scanKeysSnap(t, lo, loIncl, hi, hiIncl, fn)
}

// scanKeysSnap is the range walk over the snapshot only.
func (tx *Tx) scanKeysSnap(t *TableState, lo string, loIncl bool, hi string, hiIncl bool, fn func(key string, r schema.Row) bool) error {
	e := tx.e
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

// ScanKeysOwn is ScanKeys over what the transaction itself sees: its snapshot
// overlaid with its own uncommitted writes (as ScanAll does), restricted to the
// keys of the range and delivered in key order, own inserts merged in place. A
// serializable transaction notes the whole table as scanned, exactly as ScanAll
// does, so conflict detection does not change.
func (tx *Tx) ScanKeysOwn(table, lo string, loIncl bool, hi string, hiIncl bool, fn func(key string, r schema.Row) bool) error {
	if err := tx.check(); err != nil {
		return err
	}
	e := tx.e
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	tx.noteScan(t)
	inRange := func(key string) bool {
		if key < lo || key == lo && !loIncl {
			return false
		}
		return hi == "" || key < hi || key == hi && hiIncl
	}
	own := map[string]*pendingOp{}
	var ownKeys []string
	for i := range tx.writes {
		w := &tx.writes[i]
		if w.t != t || !inRange(w.key) {
			continue
		}
		if own[w.key] == nil {
			ownKeys = append(ownKeys, w.key)
		}
		own[w.key] = w // the last write of a key wins
	}
	if len(own) == 0 {
		if tx.iso == Serializable {
			return tx.scanKeysSnap(t, lo, loIncl, hi, hiIncl, fn)
		}
		return tx.ScanKeys(table, lo, loIncl, hi, hiIncl, fn)
	}
	slices.Sort(ownKeys)
	next := 0
	stopped := false
	emitOwn := func(key string) bool { // false: stop
		w := own[key]
		if w.del {
			return true
		}
		r, derr := decodeOwn(t, *w)
		if derr != nil {
			err = derr
			return false
		}
		if !fn(key, r) {
			stopped = true
			return false
		}
		return true
	}
	serr := tx.scanKeysSnap(t, lo, loIncl, hi, hiIncl, func(key string, r schema.Row) bool {
		for next < len(ownKeys) && ownKeys[next] < key {
			k := ownKeys[next]
			next++
			if !emitOwn(k) {
				return false
			}
		}
		if w := own[key]; w != nil {
			if next < len(ownKeys) && ownKeys[next] == key {
				next++
			}
			return emitOwn(key)
		}
		if !fn(key, r) {
			stopped = true
			return false
		}
		return true
	})
	if serr != nil {
		return serr
	}
	if err != nil || stopped {
		return err
	}
	for ; next < len(ownKeys); next++ {
		if !emitOwn(ownKeys[next]) {
			break
		}
	}
	return err
}
