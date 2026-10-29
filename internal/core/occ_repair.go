package core

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/schema"
)

// Per-key repair of a long autocommit statement (docs/PROCESS.md D24).
//
// First-committer-wins validation fails a whole transaction when any key it
// wrote changed after its snapshot. For a statement that writes 200k rows
// while small transactions commit all the time, that is almost certain, so
// retrying the statement never ends. CommitRepairable reports exactly which
// writes lost instead and leaves the transaction open: the caller computes
// the statement again for those keys only, on a fresh snapshot (the READ
// COMMITTED re-evaluation of PostgreSQL, per row), hands the new writes to
// RepairFrom, and commits again. A repaired write conflicts only with a
// head newer than the fresh snapshot, so each round is short and the next
// has few or no conflicts.
//
// Nothing about the log or the committer's output changes: the committed
// frames are those of an ordinary transaction.

// ConflictKey names a write that lost validation.
type ConflictKey struct {
	Table string
	Key   string
}

// RepairableError is the ErrConflict of a commit whose conflicts are all
// write-write on known keys. The transaction is still open.
type RepairableError struct{ Keys []ConflictKey }

func (e *RepairableError) Error() string {
	return fmt.Sprintf("%v (%d keys changed after the snapshot)", ErrConflict, len(e.Keys))
}
func (e *RepairableError) Unwrap() error { return ErrConflict }

// RepairStats are process-wide counters, for tests and diagnostics.
var RepairStats struct {
	Rounds       atomic.Int64 // CommitRepairable calls that returned a RepairableError
	KeysRepaired atomic.Int64
}

// RepairDebug, set by STASK_OCCDEBUG=1, logs every repairable conflict.
var RepairDebug = os.Getenv("STASK_OCCDEBUG") != ""

func opBase(op *pendingOp, snap uint64) uint64 {
	if op.base != 0 {
		return op.base
	}
	return snap
}

// CommitRepairable is Commit, except that a failure caused only by writes
// whose keys changed after the snapshot returns a *RepairableError and
// leaves the transaction open (its snapshot stays pinned); the caller must
// then Abort it or repair it (RepairFrom) and call CommitRepairable again.
// Any other outcome finishes the transaction as Commit does.
func (tx *Tx) CommitRepairable() (uint64, error) {
	if tx.done {
		return 0, ErrTxDone
	}
	if len(tx.writes) == 0 || tx.iso != SnapshotIsolation || tx.unindexed || len(tx.reads) > 0 || len(tx.shares) > 0 {
		return tx.Commit()
	}
	if tx.e.snapshotExpired(tx.snap) {
		tx.finish()
		return 0, ErrSnapshotTooOld
	}
	r := &commitReq{ops: tx.writes, snap: tx.snap, checkWrites: true, seenSeq: tx.seq, hasSeen: true,
		onCommit: tx.onCommit, repair: true, rv: &repairVal{}, gate: tx.gate}
	if tx.repaired {
		// heads are looked up again by submit, after a fresh installSeq
		r.hasSeen = false
		incr := tx.valid != nil && tx.valid.complete && !tx.hasUnique()
		for i := range tx.writes {
			w := &tx.writes[i]
			if incr && w.base == 0 {
				continue // unchanged since the snapshot, so its head is still the one looked up
			}
			w.seen, w.seenOk = nil, false
			if incr {
				r.rv.rep = append(r.rv.rep, i)
			}
		}
		if incr {
			r.rv.incr, r.rv.widx, r.rv.inSeq = true, tx.widx, tx.valid.seq
		}
	}
	t0 := time.Now()
	err := tx.e.submit(r)
	tx.valid = &repairVal{seq: r.rv.seq, complete: r.rv.complete}
	if err == nil {
		tx.finish()
		return r.lsn, nil
	}
	if errors.Is(err, ErrConflict) && len(r.conflicts) > 0 {
		keys := make([]ConflictKey, len(r.conflicts))
		for i, ci := range r.conflicts {
			op := &tx.writes[ci]
			keys[i] = ConflictKey{op.t.Schema().Name, op.key}
		}
		RepairStats.Rounds.Add(1)
		if RepairDebug {
			fmt.Fprintf(os.Stderr, "occ: %d writes, %d changed after snapshot %d (validated in %v), first %v\n",
				len(tx.writes), len(keys), tx.snap, time.Since(t0), keys[:min(len(keys), 3)])
		}
		return 0, &RepairableError{Keys: keys}
	}
	tx.finish()
	return 0, err
}

// RepairFrom replaces this transaction's writes to the conflicting keys by
// the writes fresh made for them, and makes them conflict only with
// versions newer than fresh's snapshot. A conflicting key fresh did not
// write is dropped (the statement no longer affects that row). It fails,
// leaving this transaction unchanged, if fresh wrote anything else or if
// either transaction has foreign-key state. fresh is not used afterwards.
func (tx *Tx) RepairFrom(fresh *Tx, conflicts []ConflictKey) error {
	if tx.done || tx.unindexed || fresh.unindexed || len(tx.shares) > 0 || len(fresh.shares) > 0 {
		return errors.New("occ: transaction cannot be repaired")
	}
	type slot struct {
		idx  int
		have bool
	}
	want := make(map[pk]*slot, len(conflicts))
	for _, c := range conflicts {
		t, err := tx.e.Table(c.Table)
		if err != nil {
			return err
		}
		k := pk{t, c.Key}
		i, ok := tx.widx[k]
		if !ok {
			return errors.New("occ: conflicting key is not a write of the transaction")
		}
		want[k] = &slot{idx: i}
	}
	for i := range fresh.writes {
		w := &fresh.writes[i]
		if _, ok := want[pk{w.t, w.key}]; !ok {
			return errors.New("occ: the recomputation wrote a row that did not conflict")
		}
		if w.sch != w.t.Schema() {
			return ErrConflict
		}
	}
	drop := 0
	for i := range fresh.writes {
		w := fresh.writes[i]
		s := want[pk{w.t, w.key}]
		w.base, w.seen, w.seenOk = fresh.snap, nil, false
		tx.writes[s.idx] = w
		s.have = true
	}
	for _, s := range want {
		if !s.have {
			tx.writes[s.idx].t = nil // dropped below
			drop++
		}
	}
	if drop > 0 {
		out := tx.writes[:0]
		for _, w := range tx.writes {
			if w.t != nil {
				out = append(out, w)
			}
		}
		clear(tx.writes[len(out):])
		tx.writes = out
		tx.widx = make(map[pk]int, len(out))
		for i := range out {
			tx.widx[pk{out[i].t, out[i].key}] = i
		}
	}
	tx.repaired = true
	RepairStats.KeysRepaired.Add(int64(len(conflicts)))
	return nil
}

// NumWrites is how many rows the transaction has written.
func (tx *Tx) NumWrites() int { return len(tx.writes) }

// repairVal carries the state of the validation of a repairable commit.
type repairVal struct {
	// in: the previous round validated every write (complete) at installSeq
	// seq, so only writes made since then and the repaired ones need checks
	incr  bool
	inSeq uint64
	widx  map[pk]int
	rep   []int // indexes of the repaired writes (base != 0)
	// out: the validation moment and whether every write was checked
	seq      uint64
	complete bool
}

// hasUnique reports whether a table written has a unique secondary index,
// whose violations an incremental validation would not see.
func (tx *Tx) hasUnique() bool {
	var last *schema.Table
	for i := range tx.writes {
		sch := tx.writes[i].sch
		if sch == last {
			continue
		}
		last = sch
		for _, ix := range sch.Indexes {
			if ix.Unique {
				return true
			}
		}
	}
	return false
}

// repairPre runs in the committer before the per-write checks of a
// repairable request. skip: the checks were done here (incremental round).
func (e *Engine) repairPre(b *commitBatch, inflight, recent []*commitBatch, r *commitReq,
	cur2 func(*TableState, string, *Version, bool) *Version) (skip bool, verr error) {
	v := r.rv
	v.seq = e.installSeq.Load()
	if !v.incr || len(recent) == 0 || recent[0].seq > v.inSeq+1 {
		return false, nil
	}
	// the writes still unchecked since the previous round's validation are
	// those of batches not installed then, and of the open batch
	hit := func(m map[pk]*Version) {
		for k := range m {
			if i, ok := v.widx[k]; ok && r.ops[i].base == 0 {
				r.conflicts = append(r.conflicts, i)
			}
		}
	}
	hit(b.pending)
	for _, x := range inflight {
		hit(x.pending)
	}
	for i := len(recent) - 1; i >= 0 && recent[i].seq > v.inSeq; i-- {
		hit(recent[i].pending)
	}
	for _, i := range v.rep {
		op := &r.ops[i]
		c := cur2(op.t, op.key, op.seen, true)
		if c != nil && c.LSN > op.base {
			r.conflicts = append(r.conflicts, i)
			continue
		}
		if op.ifAbsent && c != nil && !c.Deleted {
			return true, keyExists(op.t.Schema(), op.key)
		}
	}
	return true, nil
}

// repairPost finishes the validation of a repairable request.
func (e *Engine) repairPost(r *commitReq, verr error) error {
	r.rv.complete = verr == nil
	if len(r.conflicts) == 0 {
		return verr
	}
	slices.Sort(r.conflicts)
	r.conflicts = slices.Compact(r.conflicts)
	return ErrConflict // repairable: Tx.CommitRepairable reports the keys
}

// WriteGate holds back the other writers of some tables while one starved
// statement finishes its repair. A statement that is overtaken round after
// round (writers faster than its recomputation) closes the gate for a short
// time (ms), so that nothing can conflict with its last rounds. The gate
// opens by itself after a maximum hold time.
type WriteGate struct {
	tables map[*TableState]struct{}
	open   chan struct{}
	timer  *time.Timer
	once   sync.Once
}

// wait blocks r while it writes to a gated table.
func (g *WriteGate) wait(e *Engine, r *commitReq) {
	for {
		hit := false
		for i := range r.ops {
			if _, ok := g.tables[r.ops[i].t]; ok {
				hit = true
				break
			}
		}
		if !hit {
			return
		}
		<-g.open
		if g = e.gate.Load(); g == nil || r.gate == g {
			return
		}
	}
}

func (g *WriteGate) release(e *Engine) {
	g.once.Do(func() {
		g.timer.Stop()
		e.gate.CompareAndSwap(g, nil)
		close(g.open)
	})
}

// CloseGate holds back other writers of the tables this transaction writes
// until the returned function is called, or maxHold has passed; the
// transaction's own commits pass. Only one gate exists at a time: a second
// caller waits for the first to open.
func (tx *Tx) CloseGate(maxHold time.Duration) (release func()) {
	e := tx.e
	g := &WriteGate{tables: map[*TableState]struct{}{}, open: make(chan struct{})}
	for i := range tx.writes {
		g.tables[tx.writes[i].t] = struct{}{}
	}
	for !e.gate.CompareAndSwap(nil, g) {
		if cur := e.gate.Load(); cur != nil {
			<-cur.open
		}
	}
	g.timer = time.AfterFunc(maxHold, func() { g.release(e) })
	tx.gate = g
	if RepairDebug {
		fmt.Fprintf(os.Stderr, "occ: gate closed on %d table(s)\n", len(g.tables))
	}
	return func() { g.release(e) }
}
