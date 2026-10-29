package core

import (
	"errors"
	"math/rand/v2"

	"stask/internal/schema"
)

type Isolation int

const (
	// SnapshotIsolation: reads see the snapshot taken at Begin; commit
	// fails if any written key changed after the snapshot (first committer wins).
	SnapshotIsolation Isolation = iota
	// Serializable additionally validates every point read (including reads
	// of absent keys) at commit. Range scans are rejected in this mode
	// because the prototype has no predicate/range locking.
	Serializable
)

var ErrScanSerializable = errors.New("range scans are not supported in serializable transactions")

// ErrInterrupted: a scan stopped because the transaction's interrupt
// function (SetInterrupt) reported true.
var ErrInterrupted = errors.New("statement interrupted")

// interruptRows is how many rows a scan visits between polls of the
// interrupt function (a power of two): a poll is a function call and an
// atomic load, nothing next to decoding 256 rows, and keeps a cancel
// prompt even when each row costs microseconds (expressions in WHERE).
const interruptRows = 256

type Tx struct {
	e    *Engine
	snap uint64
	// shard of the pin of snap
	pinShard uint8
	iso      Isolation
	writes   []pendingOp
	widx     map[pk]int
	reads    []readKey
	done     bool
	// installSeq at Begin, and the head of the key read last: an insert
	// after reading its key then needs no second index lookup at commit
	seq      uint64
	lastT    *TableState
	lastKey  string
	lastHead *Version
	// writes made with InsertUnique are not in widx
	unindexed bool
	onCommit  func(lsn uint64)
	intr      func() bool
	// constraints (fk.go): parents shared by the children written, unique
	// keys written (index name, 0, value -> primary key); implicit marks a
	// single-statement transaction, whose commit does the unique checks
	shares   map[pk]struct{}
	uniq     map[string]string
	implicit bool
}

// SetImplicit marks the transaction as one statement that commits at once:
// the statement-time unique probe is skipped, the commit checks it.
func (tx *Tx) SetImplicit() { tx.implicit = true }

// HasWrites reports whether the transaction has written to table.
func (tx *Tx) HasWrites(table string) bool {
	for i := range tx.writes {
		if tx.writes[i].t.Schema().Name == table {
			return true
		}
	}
	return false
}

// IndexScan reads table through the secondary index idx (see
// Engine.IndexScan) at the transaction's snapshot. It does not see the
// transaction's own writes (check HasWrites) and is not available to
// serializable transactions, which cannot scan. ok is false if the index
// cannot serve this snapshot.
func (tx *Tx) IndexScan(table, idx, lo, hi string, desc bool, fn func(pk string, r schema.Row) bool) (bool, error) {
	if err := tx.check(); err != nil {
		return false, err
	}
	if tx.iso == Serializable {
		return false, ErrScanSerializable
	}
	t, err := tx.e.Table(table)
	if err != nil {
		return false, err
	}
	n := 0
	return tx.e.IndexScan(t, idx, tx.snap, lo, hi, desc, func(pk string, r schema.Row) bool {
		if tx.intr != nil {
			if n++; n == interruptRows {
				n = 0
				if tx.intr() {
					return false
				}
			}
		}
		return fn(pk, r)
	})
}

// Begin starts a transaction. Its snapshot is pinned against log
// reclamation until Commit or Abort, so every transaction must end.
func (e *Engine) Begin(iso Isolation) *Tx {
	seq := e.installSeq.Load()
	sh := int(rand.Uint32() % pinShards)
	snap, _ := e.pinDurableIn(sh)
	return &Tx{e: e, snap: snap, pinShard: uint8(sh), iso: iso, widx: map[pk]int{}, seq: seq}
}

func (tx *Tx) finish() {
	if !tx.done {
		tx.done = true
		tx.e.unpinIn(int(tx.pinShard), tx.snap)
	}
}

func (tx *Tx) Snapshot() uint64 { return tx.snap }

// SetInterrupt makes the transaction's scans poll fn (every interruptRows
// rows, and before each chunk of a parallel scan) and stop with ErrInterrupted once
// it returns true (statement cancel and timeout). nil: never interrupted.
func (tx *Tx) SetInterrupt(fn func() bool) { tx.intr = fn }

// check fails operations on a finished transaction, and on one whose
// snapshot was expired by Options.MaxSnapshotAge.
func (tx *Tx) check() error {
	if tx.done {
		return ErrTxDone
	}
	if tx.e.snapshotExpired(tx.snap) {
		return ErrSnapshotTooOld
	}
	return nil
}

func (tx *Tx) Get(table, key string) (schema.Row, bool, error) {
	if err := tx.check(); err != nil {
		return nil, false, err
	}
	t, err := tx.e.Table(table)
	if err != nil {
		return nil, false, err
	}
	if i, ok := tx.widx[pk{t, key}]; ok {
		w := tx.writes[i]
		if w.del {
			return nil, false, nil
		}
		r, err := decodeOwn(t, w)
		return r, err == nil, err
	}
	if tx.iso == Serializable {
		tx.reads = append(tx.reads, readKey{t: t, key: key})
	}
	r, h, err := tx.e.getAtHead(t, key, tx.snap)
	if err == nil {
		tx.lastT, tx.lastKey, tx.lastHead = t, key, h
	}
	return r, r != nil, err
}

func (tx *Tx) Scan(table, start, end string, limit int) ([]schema.Row, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	if tx.iso == Serializable {
		return nil, ErrScanSerializable
	}
	// note: does not merge the transaction's own uncommitted writes
	return tx.e.ScanRange(table, start, end, limit, tx.snap)
}

// Iso is the transaction's isolation level.
func (tx *Tx) Iso() Isolation { return tx.iso }

// Done reports whether the transaction has committed or aborted.
func (tx *Tx) Done() bool { return tx.done }

// ScanAll visits every row of table visible to the transaction: its
// snapshot, overlaid with its own uncommitted writes. Rows from the snapshot
// come in key order, followed by rows the transaction inserted.
func (tx *Tx) ScanAll(table string, fn func(key string, r schema.Row) bool) error {
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
	own := map[string]*pendingOp{}
	for i := range tx.writes {
		if w := &tx.writes[i]; w.t == t {
			own[w.key] = w
		}
	}
	seen := map[string]bool{}
	stopped, interrupted, n := false, false, 0
	err = tx.e.ScanSnapshot(table, tx.snap, func(key string, r schema.Row, _ *Version) bool {
		if tx.intr != nil {
			if n++; n == interruptRows {
				n = 0
				if tx.intr() {
					interrupted = true
					return false
				}
			}
		}
		if w := own[key]; w != nil {
			seen[key] = true
			if w.del {
				return true
			}
			var derr error
			if r, derr = decodeOwn(t, *w); derr != nil {
				return true
			}
		}
		if !fn(key, r) {
			stopped = true
			return false
		}
		return true
	})
	if err == nil && interrupted {
		err = ErrInterrupted
	}
	if err != nil || stopped {
		return err
	}
	for i := range tx.writes {
		w := &tx.writes[i]
		if w.t != t || w.del || seen[w.key] {
			continue
		}
		r, err := decodeOwn(t, *w)
		if err != nil {
			return err
		}
		if !fn(w.key, r) {
			return nil
		}
	}
	return nil
}

// ScanAllParallel is ScanAll from several goroutines (see
// Engine.ScanParallel). ok=false, with nothing scanned, when the
// transaction has writes to the table: callers then use ScanAll.
func (tx *Tx) ScanAllParallel(table string, need []bool, helpers int, spawn func(func()) bool,
	keep func(w int) bool, fn func(w, seq, idx int, r schema.Row) bool) (ok bool, err error) {
	if err := tx.check(); err != nil {
		return false, err
	}
	if tx.iso == Serializable {
		return false, ErrScanSerializable
	}
	t, err := tx.e.Table(table)
	if err != nil {
		return false, err
	}
	for i := range tx.writes {
		if tx.writes[i].t == t {
			return false, nil
		}
	}
	return true, tx.e.scanParallel(table, tx.snap, need, helpers, spawn, keep, fn, tx.intr)
}

func (tx *Tx) add(op WriteOp) error {
	if tx.done {
		return ErrTxDone
	}
	if tx.unindexed {
		return errors.New("tx: InsertUnique cannot be mixed with other writes")
	}
	p0, err := tx.e.pendingOf(op)
	if err != nil {
		return err
	}
	fc, err := tx.preConstraints(op, &p0)
	if err != nil {
		return err
	}
	p := [1]pendingOp{p0}
	if p[0].t == tx.lastT && p[0].key == tx.lastKey {
		p[0].seen, p[0].seenOk = tx.lastHead, true
	}
	k := pk{p[0].t, p[0].key}
	if i, ok := tx.widx[k]; ok {
		if tx.writes[i].del {
			// the key is absent in this transaction's view: re-inserting it
			// is a plain write (concurrent writers still conflict at commit)
			p[0].ifAbsent = false
		} else {
			p[0].ifAbsent = p[0].ifAbsent || tx.writes[i].ifAbsent
		}
		tx.writes[i] = p[0]
		return tx.postConstraints(op, &p[0], fc)
	}
	tx.widx[k] = len(tx.writes)
	tx.writes = append(tx.writes, p[0])
	return tx.postConstraints(op, &p[0], fc)
}

func (tx *Tx) Put(table string, r schema.Row) error {
	return tx.add(WriteOp{Table: table, Row: r})
}

// PutAs and InsertAs write a row built for schema t: if the table's schema
// is no longer t (ALTER TABLE), they fail with ErrConflict.
func (tx *Tx) PutAs(t *schema.Table, r schema.Row) error {
	return tx.add(WriteOp{Table: t.Name, Row: r, Schema: t})
}

func (tx *Tx) InsertAs(t *schema.Table, r schema.Row) error {
	return tx.add(WriteOp{Table: t.Name, Row: r, IfAbsent: true, Schema: t})
}

func (tx *Tx) Insert(table string, r schema.Row) error {
	return tx.add(WriteOp{Table: table, Row: r, IfAbsent: true})
}

// InsertUnique inserts a row whose key the caller guarantees this
// transaction has not written yet (for example, the only statement of an
// autocommit transaction, which checks its own rows); existing keys still
// fail at commit with ErrExists. It skips the per-key write index.
func (tx *Tx) InsertUnique(table string, r schema.Row) error { return tx.InsertUniqueAs(table, nil, r) }

// InsertUniqueAs is InsertUnique of a row built for schema sch (see PutAs).
func (tx *Tx) InsertUniqueAs(table string, sch *schema.Table, r schema.Row) error {
	if tx.done {
		return ErrTxDone
	}
	p, err := tx.e.pendingOf(WriteOp{Table: table, Row: r, IfAbsent: true, Schema: sch})
	if err != nil {
		return err
	}
	if len(p.sch.ForeignKeys) > 0 {
		if err := tx.checkParents(&p, r); err != nil {
			return err
		}
	}
	tx.unindexed = true
	tx.writes = append(tx.writes, p)
	return nil
}

// Grow reserves room for n more writes.
func (tx *Tx) Grow(n int) {
	if n > cap(tx.writes)-len(tx.writes) {
		tx.writes = append(make([]pendingOp, 0, len(tx.writes)+n), tx.writes...)
	}
}

func (tx *Tx) Delete(table, key string) error {
	return tx.add(WriteOp{Table: table, Key: key})
}

func (tx *Tx) Abort() { tx.finish() }

// OnCommit sets fn to run when the transaction commits, in commit order
// across transactions: commits with writes call fn on the engine's
// installer goroutine in LSN order once installed, which may be just after
// Commit returns (commits are answered once durable); fn must be quick and
// must not block. A read-only transaction calls it from Commit with its
// snapshot LSN. It is not called on abort or failure.
func (tx *Tx) OnCommit(fn func(lsn uint64)) { tx.onCommit = fn }

// Commit returns the commit LSN. Read-only transactions do not touch the
// log; a serializable read-only transaction still validates its reads.
func (tx *Tx) Commit() (uint64, error) {
	if tx.done {
		return 0, ErrTxDone
	}
	tx.finish()
	if tx.e.snapshotExpired(tx.snap) {
		return 0, ErrSnapshotTooOld
	}
	if len(tx.writes) == 0 {
		for _, rk := range tx.reads {
			if v := rk.t.head(rk.key); v != nil && v.LSN > tx.snap {
				tx.e.Stats.Conflicts.Add(1)
				return 0, ErrConflict
			}
		}
		if tx.onCommit != nil {
			tx.onCommit(tx.snap)
		}
		return tx.snap, nil
	}
	r := &commitReq{ops: tx.writes, reads: tx.reads, snap: tx.snap, checkWrites: true, seenSeq: tx.seq, hasSeen: true,
		onCommit: tx.onCommit}
	for k := range tx.shares {
		r.shares = append(r.shares, k)
	}
	if err := tx.e.submit(r); err != nil {
		return 0, err
	}
	return r.lsn, nil
}

// decodeOwn decodes a write of this transaction with the table's current
// schema (the write was encoded with w.sch).
func decodeOwn(t *TableState, w pendingOp) (schema.Row, error) {
	r, err := w.sch.Decode(w.val)
	if err != nil {
		return nil, err
	}
	return t.Schema().Convert(w.sch, r), nil
}
