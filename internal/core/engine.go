// Package core is the authoritative core: one log, one primary index, one
// commit protocol. Every derived representation is fed from here.
//
// Invariants (see docs/03_transaction_recovery.md):
//
//	I1 a transaction is acknowledged only after its log record is fdatasync'ed;
//	I2 a version becomes visible to readers only after it is durable, and
//	   versions become visible in LSN order (durable LSN is a prefix);
//	I3 the primary index is a cache of the log: it can always be rebuilt
//	   from checkpoint + log tail;
//	I4 derived views never feed back into the core.
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/schema"
	"stask/internal/wal"
)

var (
	ErrConflict   = errors.New("write-write or read-write conflict: retry the transaction")
	ErrExists     = errors.New("key already exists")
	ErrNoTable    = errors.New("no such table")
	ErrAppendOnly = errors.New("table is append-only: only inserts are allowed")
	ErrTxDone     = errors.New("transaction already finished")
	ErrClosed     = errors.New("engine closed")
	// ErrUnknownOutcome is returned when the log failed mid-commit: the
	// transaction was not acknowledged and may or may not survive restart.
	ErrUnknownOutcome = errors.New("storage failure during commit: outcome unknown, writes disabled until restart")
)

type Options struct {
	Dir string
	// ArchiveDir: copy each sealed log segment there (archive.go).
	// RestoreArchive: before recovery, take the segments the log lacks
	// from this archive, and stop replay at RecoveryTargetLSN or at the
	// first commit after RecoveryTargetTS (unix ns), if set.
	ArchiveDir         string
	RestoreArchive     string
	RecoveryTargetLSN  uint64
	RecoveryTargetTS   int64
	SegmentSize        int64
	MaxBatch           int
	CheckpointMinBytes int64
	NoCheckpoint       bool
	Fault              wal.Fault
	PreZero            bool
	ZeroRate           int64
	Logf               func(string, ...any)

	// Compression of sealed log segments (wal.CodecNone, CodecS2,
	// CodecZstd); the newest CompressKeepRaw sealed segments stay raw
	// (negative = default 1, 0 = compress every sealed segment).
	Compression     uint8
	CompressKeepRaw int

	// CommitWait caps how long the committer waits, after a sync, for the
	// clients it just answered to come back so they join the next batch
	// (0 = never wait). It only waits while fewer requests are prepared
	// than expected, adapts to how fast clients actually return, and never
	// waits longer than one sync: one shared batch then costs a client at
	// most wait+sync, versus two syncs when it misses the batch.
	CommitWait time.Duration

	// Log reclamation (see gc.go)
	GC             bool
	GCInterval     time.Duration // between cleaning rounds (default 10s)
	GCRetention    time.Duration // history kept for as-of reads (minimum 2s)
	GCMaxLiveRatio float64       // clean segments whose current data is at most this share (default 0.5)
	GCMaxMoveBytes int64         // bytes relocated per round (default 1GB)
	GCRateBytes    int64         // relocation rate cap in bytes/s (default 64MB/s, <0 = unlimited)
	// MaxSnapshotAge caps how long a transaction's snapshot holds back the
	// horizon once it is stale (0 = no cap): past it the snapshot stops
	// counting and its transaction fails with ErrSnapshotTooOld (gc.go).
	MaxSnapshotAge time.Duration

	// Replica opens a read-only copy fed by Apply (see replica.go): no
	// committer and no log reclamation of its own.
	Replica bool

	// SlotMaxLagBytes invalidates a CDC slot that holds back more log than
	// this (0 = no limit; see cdc.go).
	SlotMaxLagBytes int64
}

// Version is one committed state of one key. The index keeps it as a
// pointer-free entry (index.go) without TS and Prev: a Version read from the
// index has Full false and Engine.fill loads them from the log record.
type Version struct {
	LSN     uint64
	TS      int64 // commit time (unix ns); valid when Full
	ValOff  int64
	Loc     wal.Loc
	Prev    wal.Loc // the version this one replaced; valid when Full
	ValLen  uint32
	Deleted bool
	Full    bool
}

// sameVersion reports whether a and b are the same stored version (a
// relocated copy is not the same: its location differs).
func sameVersion(a, b *Version) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Loc == b.Loc && a.LSN == b.LSN
}

type TableState struct {
	// sch is replaced (never changed in place) by ALTER TABLE: callers
	// load it once per operation.
	sch atomic.Pointer[schema.Table]
	// durable is the schema as of the last durable (or replayed) DDL:
	// the one a checkpoint records. sch is newer between the committer
	// placing an ALTER TABLE and its sync.
	durable atomic.Pointer[schema.Table]
	// The index is immutable once published: the installer applies each
	// batch to a copy-on-write clone and publishes it, so readers never
	// lock and scans and checkpoints need no copy.
	idx  atomic.Pointer[tree]
	keys keyArena // long keys of every version of idx
	live atomic.Int64
	e    *Engine

	// DROP TABLE and TRUNCATE (which replaces the table by an empty one
	// with a new ID). dropping is set by the committer when it places the
	// drop, so writes prepared after it fail; replaced tells such a write
	// that the name now has a new table (a retry finds it). droppedLSN is
	// set once the drop is durable. The index is kept until no snapshot
	// can read the table any more (the GC horizon passes droppedLSN), as
	// is the schema, for decoding older changes (CDC).
	dropping   atomic.Bool
	replaced   atomic.Bool
	droppedLSN atomic.Uint64

	// secondary indexes (secindex.go); writers counts the writes in
	// submit, blocked is set while CREATE INDEX builds, ddlMu serializes
	// index DDL
	sec     atomic.Pointer[[]*secIndex]
	writers atomic.Int64
	blocked atomic.Pointer[chan struct{}]
	ddlMu   sync.Mutex
}

// droppedErr is the error for a write to a table dropped meanwhile.
func (t *TableState) droppedErr() error {
	if t.replaced.Load() {
		return ErrConflict // TRUNCATE: a retry writes to the new table
	}
	return fmt.Errorf("%w: %s (dropped)", ErrNoTable, t.Schema().Name)
}

// Schema is the table's current schema.
func (t *TableState) Schema() *schema.Table { return t.sch.Load() }

func (t *TableState) durableSchema() *schema.Table { return t.durable.Load() }

func (t *TableState) index() *tree { return t.idx.Load() }

// head is the newest durable version of key: from a batch that is durable
// and acknowledged but not yet installed (the overlay), else the index.
func (t *TableState) head(key string) *Version {
	if ov := t.e.overlay.Load(); ov != nil {
		k := pk{t, key}
		for i := len(*ov) - 1; i >= 0; i-- {
			if v, ok := (*ov)[i].pending[k]; ok {
				return v
			}
		}
	}
	en, ok := t.index().get(key)
	if !ok {
		return nil
	}
	v := en.version()
	return &v
}

// LiveRows is the number of keys whose latest version is not a delete, as
// of the installed point: the installer counts a commit shortly after it is
// acknowledged (reads see it at once through the overlay).
func (t *TableState) LiveRows() int64 { return t.live.Load() }

type Engine struct {
	// foreign keys (fk.go): the cache of who references whom, and the
	// committer's record of the newest commit that shared or guarded a key
	fkCache atomic.Pointer[fkIndex]
	fkShare map[pk]uint64
	fkGuard map[pk]uint64
	fkSweep int

	seqMu  sync.Mutex
	seqs   map[string]*seqState // sequences in use (sequence.go)
	slotMu sync.Mutex
	slots  map[string]*slot           // CDC slots (cdc.go)
	cdcCat atomic.Pointer[cdcCatalog] // publications, replica identities (cdccat.go)
	opts   Options
	// archived is the newest segment in Options.ArchiveDir (-1: none)
	archived atomic.Int64
	log      *wal.Log
	logf     func(string, ...any)

	tmu         sync.RWMutex
	tables      map[string]*TableState
	byID        map[uint32]*TableState
	tableNames  atomic.Pointer[map[string]*TableState] // read-only copy of tables for lock-free lookup
	nextTableID uint32

	durable    atomic.Uint64
	posMu      sync.Mutex
	durablePos wal.Pos
	notify     chan struct{}
	// Batches are acknowledged once durable and installed into the index
	// afterwards; until then point reads find them in the overlay, and
	// scans and checkpoints wait for (or use) the installed point.
	overlay       atomic.Pointer[[]*commitBatch] // durable, not yet installed; oldest first
	overlayMu     sync.Mutex                     // serializes overlay writers
	installed     atomic.Uint64                  // LSN up to which the index is complete
	installedPos  wal.Pos                        // log position after it (posMu)
	installNotify chan struct{}                  // closed when installed advances (posMu)

	reqCh        chan *commitReq
	commitExited chan struct{} // closed when the committer stops taking requests
	syncEWMA     atomic.Int64  // smoothed fdatasync duration (ns)
	installSeq   atomic.Uint64 // number of commit batches installed in the index
	stop         chan struct{}
	wg           sync.WaitGroup
	nextLSN      uint64
	lastTS       int64
	failErr      atomic.Pointer[error]

	tsRing []atomic.Int64 // lsn -> commit timestamp for recent commits

	ckptMu       sync.Mutex
	ckptWalBase  int64
	lastCkptSize int64

	gc        gcState
	gcRoundMu sync.Mutex

	repl replState // replica only

	Stats Stats
}

type Stats struct {
	Commits      atomic.Int64
	Conflicts    atomic.Int64
	Batches      atomic.Int64
	Syncs        atomic.Int64
	DiskLoads    atomic.Int64
	Checkpoints  atomic.Int64
	CkptBytes    atomic.Int64
	PayloadBytes atomic.Int64
	SyncNsMax    atomic.Int64
	SyncsOver50  atomic.Int64 // fdatasync calls slower than 50ms
	SyncNsTotal  atomic.Int64
	Recovery     RecoveryStats

	CompressedSegs    atomic.Int64
	CompressRawBytes  atomic.Int64
	CompressFileBytes atomic.Int64

	GCRounds            atomic.Int64
	GCReclaimedSegs     atomic.Int64
	ArchivedSegs        atomic.Int64
	ArchiveFailures     atomic.Int64
	GCMovedVersions     atomic.Int64
	GCMovedBytes        atomic.Int64
	GCDroppedTombstones atomic.Int64
}

type RecoveryStats struct {
	CheckpointLSN   uint64        `json:"checkpoint_lsn"`
	CheckpointKeys  int           `json:"checkpoint_keys"`
	CheckpointLoad  time.Duration `json:"checkpoint_load_ns"`
	ReplayRecords   int           `json:"replay_records"`
	ReplayBytes     int64         `json:"replay_bytes"`
	Replay          time.Duration `json:"replay_ns"`
	TornTail        bool          `json:"torn_tail"`
	TargetReached   bool          `json:"target_reached,omitempty"` // point-in-time recovery stopped at its target
	Total           time.Duration `json:"total_ns"`
	RecoveredLSN    uint64        `json:"recovered_lsn"`
	DiscardedTailAt string        `json:"discarded_tail_at,omitempty"`
}

const tsRingSize = 1 << 20

func Open(opts Options) (*Engine, error) {
	if opts.SegmentSize == 0 {
		opts.SegmentSize = 256 << 20
	}
	if opts.MaxBatch == 0 {
		opts.MaxBatch = 512
	}
	if opts.CheckpointMinBytes == 0 {
		opts.CheckpointMinBytes = 512 << 20
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.GCInterval == 0 {
		opts.GCInterval = 10 * time.Second
	}
	if opts.GCMaxLiveRatio == 0 {
		opts.GCMaxLiveRatio = 0.5
	}
	if opts.GCMaxMoveBytes == 0 {
		opts.GCMaxMoveBytes = 1 << 30
	}
	if opts.GCRateBytes == 0 {
		opts.GCRateBytes = 64 << 20
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	if opts.RestoreArchive != "" {
		if err := restoreFromArchive(filepath.Join(opts.Dir, "wal"), opts.RestoreArchive, opts.SegmentSize, opts.Logf); err != nil {
			return nil, fmt.Errorf("restore from archive: %w", err)
		}
	}
	l, err := wal.Open(filepath.Join(opts.Dir, "wal"), opts.SegmentSize)
	if err != nil {
		return nil, err
	}
	l.Fault = opts.Fault
	l.PreZero, l.ZeroRate = opts.PreZero, opts.ZeroRate
	e := &Engine{
		opts:          opts,
		log:           l,
		logf:          opts.Logf,
		tables:        map[string]*TableState{},
		byID:          map[uint32]*TableState{},
		notify:        make(chan struct{}),
		installNotify: make(chan struct{}),
		reqCh:         make(chan *commitReq, 4096),
		commitExited:  make(chan struct{}),
		stop:          make(chan struct{}),
		tsRing:        make([]atomic.Int64, tsRingSize),
	}
	if err := e.recover(); err != nil {
		l.Close()
		return nil, err
	}
	e.initGC()
	e.slots = map[string]*slot{}
	if !opts.Replica {
		if err := e.loadSlots(); err != nil {
			l.Close()
			return nil, fmt.Errorf("cdc slots: %w", err)
		}
		if err := e.loadCDCCatalog(); err != nil {
			l.Close()
			return nil, err
		}
	}
	if err := e.initArchive(); err != nil { // before GC can remove a segment
		l.Close()
		return nil, err
	}
	if !opts.Replica {
		e.wg.Add(1)
		go e.committer()
	}
	if opts.GC && !opts.Replica {
		e.wg.Add(1)
		go e.cleaner()
	}
	if opts.Compression != wal.CodecNone {
		if opts.CompressKeepRaw < 0 {
			opts.CompressKeepRaw = 1
		}
		e.opts.CompressKeepRaw = opts.CompressKeepRaw
		e.wg.Add(1)
		go e.compressor()
	}
	if !opts.NoCheckpoint {
		e.wg.Add(1)
		go e.checkpointer()
	}
	if opts.ArchiveDir != "" && !opts.Replica {
		e.wg.Add(1)
		go e.archiver()
	}
	return e, nil
}

func (e *Engine) Close() error {
	close(e.stop)
	e.wg.Wait()
	return e.log.Close()
}

func (e *Engine) Log() *wal.Log { return e.log }

func (e *Engine) addTable(t *schema.Table) (*TableState, error) {
	e.tmu.RLock()
	old := e.byID[t.ID]
	e.tmu.RUnlock()
	if old != nil && t.DroppedLSN == 0 && !old.dropping.Load() {
		return e.alterTable(old, t)
	}
	if err := t.Init(); err != nil {
		return nil, err
	}
	ts := &TableState{e: e}
	ts.sch.Store(t)
	ts.durable.Store(t)
	e.fkCache.Store(nil)
	ts.idx.Store(newTree(&ts.keys))
	if t.DroppedLSN == 0 {
		// a new table is empty; a table from a checkpoint gets its
		// entries from rebuildSec once its rows are loaded
		for _, def := range t.Indexes {
			ts.addSec(newSec(def))
		}
	}
	e.tmu.Lock()
	if t.DroppedLSN != 0 { // from a checkpoint: kept for older changes only
		ts.dropping.Store(true)
		ts.droppedLSN.Store(t.DroppedLSN)
	} else {
		e.tables[t.Name] = ts
	}
	e.byID[t.ID] = ts
	if t.ID >= e.nextTableID {
		e.nextTableID = t.ID + 1
	}
	e.storeNames()
	e.tmu.Unlock()
	return ts, nil
}

// storeNames publishes the name map; tmu is held.
func (e *Engine) storeNames() {
	names := make(map[string]*TableState, len(e.tables))
	for n, x := range e.tables {
		names[n] = x
	}
	e.tableNames.Store(&names)
}

// alterTable installs an ALTER TABLE (durable or replayed): the new
// schema, already initialized and published by the committer unless
// replayed, and the name if it changed.
func (e *Engine) alterTable(ts *TableState, t *schema.Table) (*TableState, error) {
	if ts.Schema() != t {
		if err := t.Init(); err != nil {
			return nil, err
		}
		ts.sch.Store(t)
	}
	ts.durable.Store(t)
	e.fkCache.Store(nil)
	if err := e.reconcileSec(ts, t, nil); err != nil {
		e.logf("alter table %s: indexes: %v", t.Name, err)
	}
	e.tmu.Lock()
	defer e.tmu.Unlock()
	if e.tables[t.Name] != ts {
		for n, x := range e.tables {
			if x == ts {
				delete(e.tables, n)
			}
		}
		e.tables[t.Name] = ts
		e.storeNames()
	}
	return ts, nil
}

// AlterTable changes a table's schema: f edits a copy of the current one.
// Columns may be renamed or changed in place but not removed or
// reordered; new columns are appended (rows stored before read them as
// their Missing value). The key column may be renamed. Writes encoded
// with the old schema fail with ErrConflict (autocommit retries them).
func (e *Engine) AlterTable(name string, f func(*schema.Table) error) error {
	for attempt := 0; ; attempt++ {
		t, err := e.Table(name)
		if err != nil {
			return err
		}
		base := t.Schema()
		d := *base
		d.Columns = slices.Clone(base.Columns)
		d.Search, d.Checks = slices.Clone(base.Search), slices.Clone(base.Checks)
		d.Dropped = slices.Clone(base.Dropped)
		d.CloneConstraints()
		if err := f(&d); err != nil {
			return err
		}
		if d.ID != base.ID {
			return errors.New("ALTER TABLE cannot change the table id")
		}
		// LSNs of added and dropped columns are set to the ALTER's when placed
		for i := range d.Columns {
			if d.Columns[i].Num == 0 {
				d.Columns[i].AddedLSN = math.MaxUint64
			}
		}
		for _, c := range base.Columns {
			if !slices.ContainsFunc(d.Columns, func(x schema.Column) bool { return x.Num == c.Num }) {
				c.DroppedLSN, c.Missing = math.MaxUint64, nil
				d.Dropped = append(d.Dropped, c)
			}
		}
		if err := d.Init(); err != nil {
			return err
		}
		err = e.submit(&commitReq{ddl: &d, alter: t, alterBase: base})
		if errors.Is(err, ErrConflict) && attempt < 100 {
			continue
		}
		if err == nil && d.Name != base.Name {
			err = e.renameCDCTable(base.Name, d.Name)
		}
		return err
	}
}

// dropTable removes t from the names once its drop (at lsn) is durable or
// replayed. A TRUNCATE then registers the replacing table under the name.
func (e *Engine) dropTable(t *TableState, lsn uint64) {
	e.fkCache.Store(nil)
	e.tmu.Lock()
	t.dropping.Store(true)
	t.droppedLSN.Store(lsn)
	if e.tables[t.Schema().Name] == t {
		delete(e.tables, t.Schema().Name)
	}
	e.storeNames()
	e.tmu.Unlock()
}

func (e *Engine) Table(name string) (*TableState, error) {
	var t *TableState
	if m := e.tableNames.Load(); m != nil {
		t = (*m)[name]
	}
	if t == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoTable, name)
	}
	return t, nil
}

// UserTable is Table for clients: system tables (sequences) do not exist.
func (e *Engine) UserTable(name string) (*TableState, error) {
	t, err := e.Table(name)
	if err == nil && t.Schema().System {
		return nil, fmt.Errorf("%w: %s", ErrNoTable, name)
	}
	return t, err
}

// UserTables is Tables without system tables.
func (e *Engine) UserTables() []*TableState {
	var out []*TableState
	for _, t := range e.Tables() {
		if !t.Schema().System {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) TableByID(id uint32) *TableState {
	e.tmu.RLock()
	defer e.tmu.RUnlock()
	return e.byID[id]
}

// Tables lists the tables in ID order, without dropped ones.
func (e *Engine) Tables() []*TableState {
	return e.listTables(false)
}

// allTables also lists dropped tables (checkpoint, GC, segment cleanup).
func (e *Engine) allTables() []*TableState {
	return e.listTables(true)
}

func (e *Engine) listTables(dropped bool) []*TableState {
	e.tmu.RLock()
	defer e.tmu.RUnlock()
	out := make([]*TableState, 0, len(e.byID))
	for i := uint32(0); i < e.nextTableID; i++ {
		if t := e.byID[i]; t != nil && (dropped || t.droppedLSN.Load() == 0) {
			out = append(out, t)
		}
	}
	return out
}

// DurableLSN is the highest LSN that is durable and visible.
func (e *Engine) DurableLSN() uint64 { return e.durable.Load() }

// DurablePoint returns the durable LSN and the log position right after it,
// read atomically, plus a channel closed on the next advance.
func (e *Engine) DurablePoint() (uint64, wal.Pos, <-chan struct{}) {
	e.posMu.Lock()
	defer e.posMu.Unlock()
	return e.durable.Load(), e.durablePos, e.notify
}

// CommitTS returns the commit time of lsn if it is still in the ring.
func (e *Engine) CommitTS(lsn uint64) (int64, bool) {
	d := e.durable.Load()
	if lsn == 0 || lsn > d || d-lsn >= tsRingSize-1 {
		return 0, false
	}
	return e.tsRing[lsn&(tsRingSize-1)].Load(), true
}

// LagSince is how long the commit right after `applied` has been waiting
// to be applied by a consumer (0 when caught up).
func (e *Engine) LagSince(applied uint64) time.Duration {
	if applied >= e.durable.Load() {
		return 0
	}
	ts, ok := e.CommitTS(applied + 1)
	if !ok {
		return time.Hour // far behind: older than the ring
	}
	return time.Duration(max(0, time.Now().UnixNano()-ts))
}

func (e *Engine) Failed() error {
	if p := e.failErr.Load(); p != nil {
		return *p
	}
	return nil
}

// ---- reads ----

func (e *Engine) loadVersion(loc wal.Loc) (*Version, error) {
	e.Stats.DiskLoads.Add(1)
	rec, err := e.log.ReadRecord(loc.Seg, int64(loc.Off))
	if err != nil {
		return nil, err
	}
	if int(loc.Idx) >= len(rec.Ops) {
		return nil, wal.ErrTorn
	}
	op := rec.Ops[loc.Idx]
	lsn, ts := rec.LSN, rec.TS
	if rec.Kind == wal.KindMove {
		lsn, ts = op.OrigLSN, op.OrigTS
	}
	return &Version{LSN: lsn, TS: ts, Loc: loc, ValOff: op.ValOff, ValLen: uint32(len(op.Val)),
		Deleted: op.Kind == wal.OpDel, Prev: op.Prev, Full: true}, nil
}

// fill sets v's TS and Prev from its log record. The record of a version
// read from the index is always there: the log cleaner relocates current
// versions before it removes their segment, and a read that started
// earlier finds the old copy through the remap.
func (e *Engine) fill(v *Version) error {
	if v.Full {
		return nil
	}
	p, err := e.loadVersion(v.Loc)
	if errors.Is(err, wal.ErrRemoved) {
		if to, ok := e.resolve(v.Loc); ok {
			p, err = e.loadVersion(to)
		}
	}
	if err != nil {
		return err
	}
	v.TS, v.Prev, v.Full = p.TS, p.Prev, true
	return nil
}

// VersionTS returns the commit time of a version returned by a read: from
// the ring of recent commits, else from the header of its log record, not
// the record. One key of a large transaction otherwise costs reading and
// decoding all of it (HTTP reads return the commit time; at 4KB values
// that was half the server's CPU).
func (e *Engine) VersionTS(v *Version) (int64, error) {
	if v.Full {
		return v.TS, nil
	}
	if ts, ok := e.CommitTS(v.LSN); ok {
		return ts, nil
	}
	defer e.enterRead().Add(-1)
	kind, _, ts, err := e.log.ReadHeader(v.Loc.Seg, int64(v.Loc.Off))
	if err == nil && kind != wal.KindMove {
		return ts, nil
	}
	// relocated by the log cleaner (each operation keeps its own commit
	// time), or its segment is gone (fill follows the remap)
	if err := e.fill(v); err != nil {
		return 0, err
	}
	return v.TS, nil
}

// Fill completes a Version returned by a read (its commit time and the
// location of the version it replaced).
func (e *Engine) Fill(v *Version) error {
	defer e.enterRead().Add(-1)
	return e.fill(v)
}

// previous returns the version v replaced, nil at the start of the chain,
// or errTruncated where the older history was reclaimed.
func (e *Engine) previous(v *Version) (*Version, error) {
	if err := e.fill(v); err != nil {
		if errors.Is(err, wal.ErrRemoved) {
			return nil, errTruncated
		}
		return nil, err
	}
	if !v.Prev.Ok {
		return nil, nil
	}
	loc := v.Prev
	if e.goneSeg(loc.Seg) {
		to, ok := e.resolve(loc)
		if !ok {
			return nil, errTruncated
		}
		loc = to
	}
	p, err := e.loadVersion(loc)
	if errors.Is(err, wal.ErrRemoved) {
		return nil, errTruncated
	}
	return p, err
}

// goneSeg reports whether versions in seg must be reached through the
// remap: the segment is removed, or about to be (reads that started before
// it was condemned may still read it until it is removed).
func (e *Engine) goneSeg(seg uint32) bool {
	if m := e.gc.condemned.Load(); m != nil && (*m)[seg] {
		return true
	}
	return e.log.Removed(seg)
}

// visible walks the chain from v to the newest version with LSN <= snap.
func (e *Engine) visible(v *Version, snap uint64) (*Version, error) {
	var err error
	for v != nil && v.LSN > snap {
		if v, err = e.previous(v); err != nil {
			if err == errTruncated {
				err = ErrSnapshotTooOld
			}
			return nil, err
		}
	}
	return v, nil
}

func (e *Engine) readValue(v *Version) ([]byte, error) {
	b := make([]byte, v.ValLen)
	if _, err := e.log.ReadAt(v.Loc.Seg, v.ValOff, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (e *Engine) rowOf(t *TableState, v *Version) (schema.Row, error) {
	b, err := e.readValue(v)
	if err != nil {
		return nil, err
	}
	return t.Schema().DecodeAt(b, v.LSN)
}

// GetAt reads key at snapshot snap. found=false for missing/deleted keys.
func (e *Engine) GetAt(table, key string, snap uint64) (schema.Row, *Version, error) {
	t, err := e.Table(table)
	if err != nil {
		return nil, nil, err
	}
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshot(snap); err != nil {
		return nil, nil, err
	}
	v, err := e.visible(t.head(key), snap)
	if err != nil || v == nil || v.Deleted {
		return nil, v, err
	}
	r, err := e.rowOf(t, v)
	return r, v, err
}

// KeyExistsError is returned by a commit that inserts a key that exists.
type KeyExistsError struct{ Table, Column, Key string }

func (e *KeyExistsError) Error() string {
	return fmt.Sprintf("%v: %s/%s", ErrExists, e.Table, e.Key)
}

func (e *KeyExistsError) Is(target error) bool { return target == ErrExists }

// getAtHead is GetAt that also returns the head it looked up.
func (e *Engine) getAtHead(t *TableState, key string, snap uint64) (schema.Row, *Version, error) {
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshot(snap); err != nil {
		return nil, nil, err
	}
	h := t.head(key)
	v, err := e.visible(h, snap)
	if err != nil || v == nil || v.Deleted {
		return nil, h, err
	}
	r, err := e.rowOf(t, v)
	return r, h, err
}

// Get is a strong (linearizable w.r.t. acknowledged commits) point read.
func (e *Engine) Get(table, key string) (schema.Row, *Version, error) {
	return e.GetAt(table, key, e.durable.Load())
}

// GetAsOfTime returns the version of key that was current at unix-nanos t.
func (e *Engine) GetAsOfTime(table, key string, t int64) (schema.Row, *Version, error) {
	ts, err := e.Table(table)
	if err != nil {
		return nil, nil, err
	}
	defer e.enterRead().Add(-1)
	v := ts.head(key)
	if v != nil {
		if err := e.fill(v); err != nil {
			return nil, nil, err
		}
	}
	for v != nil && v.TS > t {
		if v, err = e.previous(v); err != nil {
			if err == errTruncated {
				err = ErrSnapshotTooOld
			}
			return nil, nil, err
		}
	}
	if v == nil || v.Deleted || v.LSN > e.durable.Load() {
		return nil, v, nil
	}
	r, err := e.rowOf(ts, v)
	return r, v, err
}

type HistoryEntry struct {
	LSN     uint64     `json:"lsn"`
	TS      int64      `json:"ts"`
	Deleted bool       `json:"deleted"`
	Row     schema.Row `json:"-"`
}

// History walks the version chain of key, newest first.
func (e *Engine) History(table, key string, limit int) ([]HistoryEntry, error) {
	t, err := e.Table(table)
	if err != nil {
		return nil, err
	}
	defer e.enterRead().Add(-1)
	snap := e.durable.Load()
	v, err := e.visible(t.head(key), snap)
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	if v != nil {
		if err := e.fill(v); err != nil {
			return nil, err
		}
	}
	for v != nil && len(out) < limit {
		h := HistoryEntry{LSN: v.LSN, TS: v.TS, Deleted: v.Deleted}
		if !v.Deleted {
			if h.Row, err = e.rowOf(t, v); err != nil {
				return nil, err
			}
		}
		out = append(out, h)
		if v, err = e.previous(v); err == errTruncated {
			break // older history was reclaimed
		} else if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ScanRange returns up to limit visible rows with start <= key < end
// (end=="" means unbounded) at snapshot snap, in key order.
func (e *Engine) ScanRange(table, start, end string, limit int, snap uint64) ([]schema.Row, error) {
	t, err := e.Table(table)
	if err != nil {
		return nil, err
	}
	e.waitInstalled(snap)
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshot(snap); err != nil {
		return nil, err
	}
	var out []schema.Row
	from, skip, hasSkip := start, "", false
	type cnd struct {
		key string
		v   Version
	}
	for len(out) < limit {
		var cand []cnd
		want := (limit-len(out))*2 + 16
		tr := t.index()
		tr.ascend(from, func(en *entry) bool {
			key := tr.keyOf(en)
			if hasSkip && key == skip {
				return true
			}
			if end != "" && key >= end {
				return false
			}
			cand = append(cand, cnd{key, en.version()})
			return len(cand) < want
		})
		for i := range cand {
			v, err := e.visible(&cand[i].v, snap)
			if err != nil {
				return nil, err
			}
			if v == nil || v.Deleted {
				continue
			}
			r, err := e.rowOf(t, v)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
			if len(out) == limit {
				break
			}
		}
		if len(cand) < want {
			break
		}
		from = cand[len(cand)-1].key
		skip, hasSkip = from, true
	}
	return out, nil
}

// ScanSnapshot visits every visible row of table at snap. The table lock is
// held only for an O(1) copy-on-write clone of the index.
func (e *Engine) ScanSnapshot(table string, snap uint64, fn func(key string, r schema.Row, v *Version) bool) error {
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	e.waitInstalled(snap)
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshot(snap); err != nil {
		return err
	}
	tr := t.index()
	var ierr error
	tr.ascend("", func(en *entry) bool {
		h := en.version()
		v, err := e.visible(&h, snap)
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
		return fn(strings.Clone(tr.keyOf(en)), r, v)
	})
	return ierr
}

// ScanChunk is the number of keys a parallel scan hands out at a time.
const ScanChunk = 2048

// ScanParallel calls fn for every row visible at snap, from several
// goroutines at once. The index is cut into chunks of consecutive keys;
// chunk seq (numbered in key order) and the row's position idx in it give
// each row's place in a sequential scan. The calling goroutine scans too;
// spawn is offered up to helpers more workers (it returns false when it
// cannot run one now, which only costs parallelism). w identifies the
// worker, in [0, helpers]. Before each chunk a helper asks keep(w); false
// makes it leave the remaining chunks to the others (the caller never
// leaves). fn returning false stops the scan.
// Only the columns marked in need (nil = all) are decoded, and each worker
// reuses one row: r is valid only during the call, and fn must copy it to
// keep it. A scan therefore allocates almost nothing per row, which keeps
// the Go GC (whose pauses every goroutine shares) out of other requests.
func (e *Engine) ScanParallel(table string, snap uint64, need []bool, helpers int, spawn func(func()) bool,
	keep func(w int) bool, fn func(w, seq, idx int, r schema.Row) bool) error {
	return e.scanParallel(table, snap, need, helpers, spawn, keep, fn, nil)
}

// scanParallel is ScanParallel that also polls intr (nil = never) before
// each chunk and every interruptRows rows, and fails with ErrInterrupted
// once it reports true.
func (e *Engine) scanParallel(table string, snap uint64, need []bool, helpers int, spawn func(func()) bool,
	keep func(w int) bool, fn func(w, seq, idx int, r schema.Row) bool, intr func() bool) error {
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	e.waitInstalled(snap)
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshot(snap); err != nil {
		return err
	}
	// columns added after the scan starts are added after snap: rows
	// visible at snap decode with this schema
	sch := t.Schema()
	type chunk struct {
		seq   int
		items []entry
	}
	ch := make(chan chunk, 2*(helpers+1))
	var stop atomic.Bool
	var errMu sync.Mutex
	var ferr error
	fail := func(err error) {
		errMu.Lock()
		if ferr == nil {
			ferr = err
		}
		errMu.Unlock()
		stop.Store(true)
	}
	go func() {
		defer close(ch)
		seq := 0
		buf := make([]entry, 0, ScanChunk)
		t.index().ascend("", func(en *entry) bool {
			if buf = append(buf, *en); len(buf) == ScanChunk {
				ch <- chunk{seq, buf}
				seq++
				buf = make([]entry, 0, ScanChunk)
			}
			return !stop.Load()
		})
		if len(buf) > 0 {
			ch <- chunk{seq, buf}
		}
	}()
	work := func(w int) {
		defer func() {
			if p := recover(); p != nil {
				fail(fmt.Errorf("parallel scan: %v", p))
			}
			if w == 0 {
				for range ch { // the walker may still be sending
				}
			}
		}()
		var buf []byte
		var r schema.Row
		rd := e.log.Reader()
		defer rd.Close()
		for {
			if w > 0 && !stop.Load() && !keep(w) {
				return // the caller reads until the end
			}
			c, more := <-ch
			if !more {
				return
			}
			if stop.Load() {
				continue // drain so the walker can finish
			}
			if intr != nil && intr() {
				fail(ErrInterrupted)
				continue
			}
			for i := range c.items {
				// also within the chunk: rows that cost microseconds
				// each would otherwise delay a cancel by a whole chunk
				if i&(interruptRows-1) == interruptRows-1 && intr != nil && intr() {
					fail(ErrInterrupted)
					break
				}
				h := c.items[i].version()
				v, err := e.visible(&h, snap)
				if err != nil {
					fail(err)
					break
				}
				if v == nil || v.Deleted {
					continue
				}
				if cap(buf) < int(v.ValLen) {
					buf = make([]byte, v.ValLen)
				}
				buf = buf[:v.ValLen]
				if _, err = rd.ReadAt(v.Loc.Seg, v.ValOff, buf); err == nil {
					r, err = sch.DecodeInto(buf, v.LSN, r, need)
				}
				if err != nil {
					fail(err)
					break
				}
				if !fn(w, c.seq, i, r) {
					stop.Store(true)
					break
				}
			}
		}
	}
	var wg sync.WaitGroup
	for w := 1; w <= helpers; w++ {
		wg.Add(1)
		if !spawn(func() { defer wg.Done(); work(w) }) {
			wg.Done()
		}
	}
	work(0)
	wg.Wait()
	return ferr
}

// ---- writes ----

type WriteOp struct {
	Table    string
	Row      schema.Row // nil for delete
	Key      string     // for delete
	IfAbsent bool       // insert semantics
	// Schema, if set, is the schema Row was built for: if the table's is
	// another one (an ALTER TABLE came in between), the write fails with
	// ErrConflict, so a retry rebuilds the row.
	Schema *schema.Table
}

type pendingOp struct {
	t        *TableState
	sch      *schema.Table // the schema val was encoded with
	key      string
	val      []byte
	del      bool
	ifAbsent bool
	seen     *Version // head looked up before commit (see seenSeq)
	seenOk   bool
	sk       []secKey // index keys of the row, per sch.Indexes (secindex.go)
	guard    bool     // deletes or re-keys a row that foreign keys reference (fk.go)
}

type readKey struct {
	t    *TableState
	key  string
	seen *Version
}

// pk identifies a key across tables in the committer's overlay maps.
type pk struct {
	t   *TableState
	key string
}

type commitReq struct {
	enq         time.Time // LatDebug only
	ops         []pendingOp
	reads       []readKey
	seenSeq     uint64 // installSeq read before the seen heads were looked up
	hasSeen     bool
	snap        uint64
	checkWrites bool
	ddl         *schema.Table
	drop        *TableState // DROP TABLE; with ddl, TRUNCATE (ddl replaces it)
	alter       *TableState // ALTER TABLE: ddl is its new schema, computed from alterBase
	alterBase   *schema.Table
	moves       []moveOp // log reclamation: relocate or drop heads
	own         *reqOwn  // checkUnique: the request's ops by key
	shares      []pk     // parents of the children written (fk.go)
	lsn         uint64
	err         error
	done        chan struct{}
	// onCommit runs once the commit is installed, in commit (LSN) order
	// across all transactions, on the installer goroutine: it must be quick
	// and must not block.
	onCommit func(lsn uint64)
}

func (e *Engine) toPending(ops []WriteOp) ([]pendingOp, error) {
	out := make([]pendingOp, 0, len(ops))
	for _, op := range ops {
		p, err := e.pendingOf(op)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (e *Engine) pendingOf(op WriteOp) (pendingOp, error) {
	t, err := e.Table(op.Table)
	if err != nil {
		return pendingOp{}, err
	}
	sch := t.Schema()
	if op.Schema != nil && op.Schema != sch {
		return pendingOp{}, ErrConflict
	}
	p := pendingOp{t: t, sch: sch, ifAbsent: op.IfAbsent}
	if op.Row == nil {
		if sch.AppendOnly {
			return pendingOp{}, ErrAppendOnly
		}
		p.key, p.del = op.Key, true
		p.guard = len(e.fkReferrers(sch.Name)) > 0
		return p, nil
	}
	if len(op.Row) != len(sch.Columns) {
		// written for the schema before an ALTER TABLE: a retry rebuilds it
		if sch.Altered() {
			return pendingOp{}, ErrConflict
		}
		return pendingOp{}, fmt.Errorf("row has %d columns, table %s has %d", len(op.Row), sch.Name, len(sch.Columns))
	}
	if sch.AppendOnly {
		p.ifAbsent = true
	}
	p.key = sch.KeyOf(op.Row)
	p.val = sch.Encode(nil, op.Row)
	if len(sch.Indexes) > 0 {
		p.sk = secKeysOf(sch, op.Row)
	}
	return p, nil
}

func (e *Engine) submit(r *commitReq) error {
	if e.opts.Replica {
		return ErrReadOnly
	}
	if err := e.Failed(); err != nil {
		return err
	}
	r.done = make(chan struct{})
	if r.moves == nil && r.ddl == nil {
		// Look up current heads here, in parallel with other submitters, so
		// the committer only checks what was installed since (see prepare).
		if !r.hasSeen {
			r.seenSeq, r.hasSeen = e.installSeq.Load(), true
		}
		// heads already looked up (after seenSeq) by the transaction are kept;
		// looking up the rest later than seenSeq is only more conservative
		for i := range r.ops {
			if !r.ops[i].seenOk {
				r.ops[i].seen, r.ops[i].seenOk = r.ops[i].t.head(r.ops[i].key), true
			}
		}
		for i := range r.reads {
			r.reads[i].seen = r.reads[i].t.head(r.reads[i].key)
		}
	}
	if LatDebug {
		r.enq = time.Now()
		defer func() { LatSubmit.Add(time.Since(r.enq)) }()
	}
	if len(r.ops) > 0 {
		// CREATE INDEX waits for the writers of its table (secindex.go)
		var buf [4]*TableState
		held := enterAll(r.ops, buf[:0])
		defer func() {
			for _, t := range held {
				t.leave()
			}
		}()
	}
	select {
	case e.reqCh <- r:
	case <-e.stop:
		return ErrClosed
	}
	// The channel is buffered, so a request can be queued just as the
	// committer exits; it is then never processed and must not be waited on.
	select {
	case <-r.done:
		return r.err
	case <-e.commitExited:
		select {
		case <-r.done:
			return r.err
		default:
			return ErrClosed
		}
	}
}

// Write commits ops atomically as one autocommit transaction (no
// snapshot check: last writer wins, except IfAbsent ops). Returns the LSN.
func (e *Engine) Write(ops []WriteOp) (uint64, error) {
	p, err := e.toPending(ops)
	if err != nil {
		return 0, err
	}
	r := &commitReq{ops: p}
	if err := e.submit(r); err != nil {
		return 0, err
	}
	return r.lsn, nil
}

func (e *Engine) CreateTable(t schema.Table) (*schema.Table, error) {
	if err := t.Init(); err != nil {
		return nil, err
	}
	r := &commitReq{ddl: &t}
	if err := e.submit(r); err != nil {
		return nil, err
	}
	ts, err := e.Table(t.Name)
	if err != nil {
		return nil, err
	}
	return ts.Schema(), nil
}

// DropTable drops a table. Its rows stay readable by snapshots taken
// before the drop until the GC horizon passes it; it is also removed from
// publications and replica identity settings (as in PostgreSQL).
func (e *Engine) DropTable(name string) error {
	t, err := e.Table(name)
	if err != nil {
		return err
	}
	if err := e.submit(&commitReq{drop: t}); err != nil {
		return err
	}
	return e.forgetCDCTable(name)
}

// TruncateTable empties a table in O(1), as PostgreSQL does: the table is
// replaced by an empty one with a new ID (the old rows are reclaimed like
// those of a dropped table) and the same name, schema and OID. Snapshots
// taken before keep reading the old rows. Writes to the table placed
// after the truncate but prepared before it fail with ErrConflict, so an
// autocommit statement is retried on the new table.
func (e *Engine) TruncateTable(name string) error {
	t, err := e.Table(name)
	if err != nil {
		return err
	}
	d := *t.Schema()
	d.Replaces, d.RelID, d.DroppedLSN = t.Schema().ID, t.Schema().OID(), 0
	return e.submit(&commitReq{drop: t, ddl: &d})
}

// The commit pipeline has two stages that overlap:
//
//   - the committer validates each request as it arrives, assigns its LSN
//     and encodes it into the open batch;
//   - the syncer appends nothing: it makes a handed-off batch durable
//     (fdatasync), adds it to the read overlay, publishes it and answers;
//   - the installer installs durable batches into the index in LSN order
//     and then drops them from the overlay.
//
// While batch N is being synced, batch N+1 is prepared and batch N-1
// installed, so a cycle costs one fdatasync and none of the CPU work.
// Validation sees the open batch, the batches handed off but not installed
// (newest first), and installed heads, in that order.

// commitBatch is a set of prepared requests that are synced together.
type commitBatch struct {
	pending  map[pk]*Version // key -> newest version written by this batch
	placed   []*commitReq    // requests with a log record (or no-op moves)
	installs []placedVersion
	ddls     []*schema.Table
	ddlNames map[string]bool
	drops    []droppedTable
	remaps   []remapSet
	// index keys of the newest version of each indexed key written by this
	// batch (empty: deleted), for unique checks; adds are the index entries
	// of the requests prepared since the last flush (secindex.go)
	sk      map[pk][]secKey
	adds    []secAdd
	uq      map[*secIndex]map[string][]string // this prepare call's unique keys
	buf     []byte                            // encoded records not yet appended
	lastLSN uint64
	lastTS  int64
	seq     uint64 // installSeq value once installed (set by the installer)
	failed  bool   // sync failed: answered with an error, never installed

	// set at handoff
	file   *os.File
	endPos wal.Pos
	handed time.Time
}

type droppedTable struct {
	t   *TableState
	lsn uint64
}

func newCommitBatch(hint int) *commitBatch {
	return &commitBatch{pending: make(map[pk]*Version, hint), installs: make([]placedVersion, 0, hint), ddlNames: map[string]bool{}}
}

func (e *Engine) committer() {
	defer e.wg.Done()
	defer close(e.commitExited)
	// Three stages: this goroutine prepares batches, the syncer makes them
	// durable, the installer installs, publishes and answers them, so the
	// fdatasync of one batch overlaps the install of the previous one.
	toSync := make(chan *commitBatch)
	toInstall := make(chan *commitBatch) // unbuffered: a busy installer holds the syncer back
	synced := make(chan struct{}, 1)
	installed := make(chan *commitBatch, 4)
	syncerDone, installerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(syncerDone)
		defer close(toInstall)
		for b := range toSync {
			e.syncBatch(b)
			toInstall <- b
			synced <- struct{}{}
		}
	}()
	go func() {
		defer close(installerDone)
		for b := range toInstall {
			if !b.failed {
				e.installBatch(b)
			}
			installed <- b
		}
	}()

	open, syncing := newCommitBatch(0), (*commitBatch)(nil)
	var inflight []*commitBatch // handed off and not yet installed, oldest first
	var recent []*commitBatch   // last installed batches, oldest first
	ddlNext := uint32(0)
	expect := 0 // requests expected for the next batch: queued + just answered
	// Adaptive wait for clients answered by the previous sync: they are
	// typically one network round trip from committing again, and joining
	// this batch instead of the next saves them a whole sync. The wait
	// grows while it fills batches and shrinks when clients are too slow
	// (e.g. CPU saturation) for it to pay off.
	adapt := e.opts.CommitWait
	probe := 0
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	var timerC <-chan time.Time
	gatherFrom, gathered := 0, false

	handoff := func() {
		if timerC != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerC = nil
		}
		if gathered {
			got, missing := len(open.placed)-gatherFrom, expect-gatherFrom
			if LatDebug && got >= missing {
				GatherFull.Add(1)
			}
			if 2*got >= missing {
				adapt = min(max(adapt+adapt/8+5*time.Microsecond, adapt), e.opts.CommitWait)
			} else {
				adapt = max(adapt*3/4, 10*time.Microsecond)
			}
			gathered = false
		}
		if err := e.flushOpen(open); err != nil {
			e.fail(err, nil, open.placed, nil)
			open = newCommitBatch(0)
			return
		}
		syncing = open
		inflight = append(inflight, open)
		open = newCommitBatch(len(open.installs))
		toSync <- syncing
	}
	stopping := false
	for {
		if syncing == nil && len(open.placed) > 0 {
			if !gathered && timerC == nil && e.opts.CommitWait > 0 && len(open.placed) < min(expect, e.opts.MaxBatch) && !stopping {
				cur := adapt
				if probe++; probe%64 == 0 {
					cur = e.opts.CommitWait // periodically retry the full wait in case load changed
				}
				if wait := min(cur, time.Duration(e.syncEWMA.Load())); wait > 0 {
					if LatDebug {
						GatherStarted.Add(1)
						GatherAdapt.Store(int64(wait))
						GatherExpect.Store(int64(expect))
						GatherHave.Store(int64(len(open.placed)))
					}
					gathered, gatherFrom = true, len(open.placed)
					timer.Reset(max(wait, 10*time.Microsecond))
					timerC = timer.C
				}
			}
			if timerC == nil || len(open.placed) >= min(expect, e.opts.MaxBatch) || stopping {
				handoff()
				continue
			}
		}
		if stopping && syncing == nil && len(inflight) == 0 {
			close(toSync)
			<-syncerDone
			<-installerDone
			return
		}
		reqCh := e.reqCh
		if len(open.placed) >= e.opts.MaxBatch || stopping {
			reqCh = nil // full: wait for the syncer
		}
		var syncedC <-chan struct{}
		if syncing != nil {
			syncedC = synced
		}
		stopC := e.stop
		if stopping {
			stopC = nil
		}
		select {
		case r := <-reqCh:
			batch := []*commitReq{r}
		fill:
			for len(open.placed)+len(batch) < e.opts.MaxBatch {
				select {
				case r := <-e.reqCh:
					batch = append(batch, r)
				default:
					break fill
				}
			}
			e.tmu.RLock()
			ddlNext = max(ddlNext, e.nextTableID)
			e.tmu.RUnlock()
			e.prepare(open, inflight, recent, batch, &ddlNext)
		case <-syncedC:
			// answered on sync: whoever queued meanwhile, plus those clients
			answered := len(syncing.placed)
			syncing = nil
			expect = len(open.placed) + len(e.reqCh) + answered
		case b := <-installed:
			if b != inflight[0] {
				panic("commit: batches installed out of order")
			}
			inflight[0] = nil
			inflight = inflight[1:]
			if !b.failed {
				if len(recent) == recentBatches {
					recent[0] = nil
					recent = recent[1:]
				}
				// keep only what validation needs
				b.placed, b.installs, b.ddls, b.remaps, b.buf, b.drops = nil, nil, nil, nil, nil, nil
				b.sk, b.adds, b.uq = nil, nil, nil
				recent = append(recent, b)
			}
		case <-timerC:
			if LatDebug {
				GatherTimeout.Add(1)
			}
			timerC = nil // gathering is over: hand off on the next iteration
			if syncing == nil && len(open.placed) > 0 {
				handoff()
			}
		case <-stopC:
			// finish what was prepared; queued requests fail with ErrClosed
			stopping = true
		}
	}
}

type placedVersion struct {
	t      *TableState
	key    string
	v      *Version
	expect *Version      // drop: remove the head only if it is still this version
	sch    *schema.Table // the schema sk was computed for (tables with indexes)
	sk     []secKey
}

// flushOpen appends the open batch's remaining bytes and records what the
// syncer must make durable.
func (e *Engine) flushOpen(b *commitBatch) error {
	if len(b.buf) > 0 {
		if LatDebug {
			SyncBytes.Add(int64(len(b.buf)))
			SyncCount.Add(1)
		}
		if _, err := e.log.Append(b.buf); err != nil {
			return err
		}
		b.buf = b.buf[:0]
	}
	b.file = e.log.Writer()
	b.endPos = e.log.WritePos()
	b.handed = time.Now()
	return nil
}

func finishReq(r *commitReq, err error) {
	r.err = err
	close(r.done)
}

// recentBatches is how many installed batches the committer keeps to check
// heads that submitters looked up themselves.
const recentBatches = 64

// prepare validates and encodes requests into the open batch b. inflight
// holds the batches handed off but not installed (oldest first), recent the
// last installed ones.
func (e *Engine) prepare(b *commitBatch, inflight, recent []*commitBatch, batch []*commitReq, ddlNext *uint32) {
	if err := e.Failed(); err != nil {
		for _, r := range batch {
			finishReq(r, err)
		}
		return
	}
	now := time.Now().UnixNano()
	if now <= e.lastTS {
		now = e.lastTS + 1
	}
	var valRel []int
	var walOps []wal.Op
	lsn := e.nextLSN
	var rollErr error

	// cur is the newest version of a key: written by this batch, by the one
	// being synced, or installed. A submitter's own lookup (seen, made after
	// reading installSeq = seenSeq) is current unless a batch installed since
	// wrote the key; those are all in recent, so no index lookup is needed.
	var r *commitReq
	cur2 := func(t *TableState, key string, seen *Version, useSeen bool) *Version {
		k := pk{t, key}
		if v, ok := b.pending[k]; ok {
			return v
		}
		for i := len(inflight) - 1; i >= 0; i-- {
			if v, ok := inflight[i].pending[k]; ok {
				return v
			}
		}
		if useSeen && r != nil && r.hasSeen {
			oldest := uint64(0)
			if len(recent) > 0 {
				oldest = recent[0].seq
			}
			if r.seenSeq+1 >= oldest || r.seenSeq == e.installSeq.Load() {
				for i := len(recent) - 1; i >= 0 && recent[i].seq > r.seenSeq; i-- {
					if v, ok := recent[i].pending[k]; ok {
						return v
					}
				}
				return seen
			}
		}
		return t.head(key)
	}
	cur := func(t *TableState, key string) *Version { return cur2(t, key, nil, false) }
	// on a storage failure, everything placed in b so far shares its fate
	failAll := func(err error, current *commitReq) {
		e.fail(err, batch, b.placed, current)
		b.placed = nil
	}

	for _, req := range batch {
		r = req
		if LatDebug {
			LatQueue.Add(time.Since(r.enq))
		}
		if rollErr != nil {
			finishReq(r, rollErr)
			continue
		}
		if r.drop != nil && r.drop.dropping.Load() {
			finishReq(r, r.drop.droppedErr())
			continue
		}
		if r.alter != nil && (r.alter.dropping.Load() || r.alter.Schema() != r.alterBase) {
			finishReq(r, ErrConflict) // altered or dropped meanwhile: the caller retries
			continue
		}
		if r.ddl != nil && r.drop == nil && (r.alter == nil || r.ddl.Name != r.alterBase.Name) {
			// a table that is being dropped no longer holds its name
			e.tmu.RLock()
			old, exists := e.tables[r.ddl.Name]
			e.tmu.RUnlock()
			exists = exists && !old.dropping.Load()
			pendingDDL := false
			for _, x := range inflight {
				pendingDDL = pendingDDL || x.ddlNames[r.ddl.Name]
			}
			if exists || b.ddlNames[r.ddl.Name] || pendingDDL {
				finishReq(r, fmt.Errorf("table %q already exists", r.ddl.Name))
				continue
			}
		}
		if r.ddl != nil {
			b.ddlNames[r.ddl.Name] = true
		}
		if r.drop != nil {
			r.drop.replaced.Store(r.ddl != nil)
			r.drop.dropping.Store(true)
		} else if r.ddl == nil && r.moves == nil {
			// validation
			var verr error
			for _, op := range r.ops {
				if op.t.dropping.Load() {
					verr = op.t.droppedErr()
					break
				}
				if !op.del && op.sch != op.t.Schema() {
					verr = ErrConflict // encoded before an ALTER TABLE: a retry re-encodes
					break
				}
			}
			for _, rk := range r.reads {
				if verr == nil && rk.t.dropping.Load() {
					verr = ErrConflict
				}
			}
			for _, op := range r.ops {
				if verr != nil {
					break
				}
				c := cur2(op.t, op.key, op.seen, true)
				if r.checkWrites && c != nil && c.LSN > r.snap {
					verr = ErrConflict
					break
				}
				if op.ifAbsent && c != nil && !c.Deleted {
					verr = &KeyExistsError{Table: op.t.Schema().Name, Column: op.t.Schema().Key, Key: op.key}
					break
				}
				if len(op.sk) > 0 {
					if verr = e.checkUnique(b, inflight, r, op); verr != nil {
						break
					}
				}
			}
			if verr == nil {
				for _, rk := range r.reads {
					if c := cur2(rk.t, rk.key, rk.seen, true); c != nil && c.LSN > r.snap {
						verr = ErrConflict
						break
					}
				}
			}
			if verr == nil && r.checkWrites {
				verr = e.checkFK(r)
			}
			if verr != nil {
				if verr == ErrConflict {
					e.Stats.Conflicts.Add(1)
				}
				finishReq(r, verr)
				continue
			}
		}

		// size estimate, roll segment if needed (a transaction's first frame)
		est := 64
		for i, op := range r.ops {
			if i == wal.MaxFrameOps || est > wal.MaxFrameBytes {
				break
			}
			est += len(op.key) + len(op.val) + 48
		}
		if r.ddl != nil || r.drop != nil {
			est += 4096
		}
		for _, m := range r.moves {
			est += len(m.key) + len(m.val) + 64
		}
		if int64(len(b.buf)+est) > e.log.Remaining() {
			if len(b.buf) > 0 {
				if _, err := e.log.Append(b.buf); err != nil {
					failAll(err, r)
					return
				}
				b.buf = b.buf[:0]
			}
			// Roll syncs the segment being left, which holds the start of
			// this batch; the syncer syncs the segment current at handoff.
			if err := e.log.Roll(); err != nil {
				var se *wal.SyncError
				if errors.As(err, &se) {
					failAll(err, r)
					return
				}
				rollErr = fmt.Errorf("cannot allocate log segment (disk full?): %w", err)
				e.logf("commit: %v", rollErr)
				finishReq(r, rollErr)
				continue
			}
		}
		pos := wal.Pos{Seg: e.log.WritePos().Seg, Off: e.log.WritePos().Off + int64(len(b.buf))}

		r.lsn = lsn
		if r.moves != nil {
			walOps = walOps[:0]
			var mv []*moveOp
			var stillHead []bool
			for i := range r.moves {
				m := &r.moves[i]
				head := m.head != nil && sameVersion(cur(m.t, m.key), m.head)
				if m.drop && head {
					b.pending[pk{m.t, m.key}] = nil
					e.noteMoved(b, m.t, m.key, nil, 0, true)
					b.installs = append(b.installs, placedVersion{t: m.t, key: m.key, expect: m.head})
					continue
				}
				w := wal.Op{Kind: wal.OpPut, Table: m.t.Schema().ID, Key: []byte(m.key), Val: m.val,
					Prev: m.prev, OrigLSN: m.lsn, OrigTS: m.ts}
				if m.del {
					w.Kind = wal.OpDel
				}
				walOps = append(walOps, w)
				mv = append(mv, m)
				stillHead = append(stillHead, head)
			}
			if len(walOps) == 0 {
				r.lsn = 0
				b.placed = append(b.placed, r)
				continue
			}
			b.buf, valRel = wal.AppendRecord(b.buf, wal.KindMove, lsn, now, walOps, nil, valRel[:0])
			for i, m := range mv {
				loc := wal.Loc{Ok: true, Seg: pos.Seg, Off: uint32(pos.Off), Idx: uint16(i)}
				switch {
				case stillHead[i]:
					v := &Version{LSN: m.lsn, TS: m.ts, Loc: loc, ValOff: pos.Off + int64(valRel[i]),
						ValLen: uint32(len(m.val)), Deleted: m.del, Prev: m.prev, Full: true}
					b.pending[pk{m.t, m.key}] = v
					e.noteMoved(b, m.t, m.key, m.val, m.lsn, m.del)
					b.installs = append(b.installs, placedVersion{t: m.t, key: m.key, v: v})
				case m.head != nil:
					// superseded since the cleaner looked: newer versions reference
					// it by its old location, which is about to disappear
					b.remaps = append(b.remaps, remapSet{m.head.Loc, remapEntry{loc, lsn}})
				default:
					b.remaps = append(b.remaps, remapSet{m.ident, remapEntry{loc, m.sup}})
				}
				e.Stats.GCMovedVersions.Add(1)
				e.Stats.GCMovedBytes.Add(int64(len(m.key) + len(m.val)))
			}
		} else if r.ddl != nil || r.drop != nil {
			// one record: a new table (with Replaces for TRUNCATE) or the
			// dropped table with its DroppedLSN
			var d schema.Table
			if r.alter != nil {
				d = *r.ddl
				d.Dropped = slices.Clone(d.Dropped)
				for i := range d.Columns {
					if d.Columns[i].AddedLSN == math.MaxUint64 {
						d.Columns[i].AddedLSN = lsn
					}
				}
				for i := range d.Dropped {
					if d.Dropped[i].DroppedLSN == math.MaxUint64 {
						d.Dropped[i].DroppedLSN = lsn
					}
				}
				if err := d.Init(); err != nil {
					finishReq(r, err)
					continue
				}
				// from now on, writes encoded with the old schema conflict
				r.alter.sch.Store(&d)
			} else if r.ddl != nil {
				d = *r.ddl
				d.ID = *ddlNext
				*ddlNext++
			} else {
				d = *r.drop.Schema()
				d.DroppedLSN = lsn
			}
			js, _ := json.Marshal(&d)
			b.buf, _ = wal.AppendRecord(b.buf, wal.KindDDL, lsn, now, nil, js, nil)
			if r.ddl != nil {
				b.ddls = append(b.ddls, &d)
			}
			if r.drop != nil {
				b.drops = append(b.drops, droppedTable{r.drop, lsn})
			}
		} else {
			prevs := make([]*Version, len(r.ops))
			for i, op := range r.ops {
				prevs[i] = cur2(op.t, op.key, op.seen, true)
			}
			// one frame, or a chain of KindTxPart frames ending in KindTx
			for lo := 0; lo < len(r.ops); {
				hi, size := lo, 64
				for hi < len(r.ops) && hi-lo < wal.MaxFrameOps && (hi == lo || size <= wal.MaxFrameBytes) {
					size += len(r.ops[hi].key) + len(r.ops[hi].val) + 48
					hi++
				}
				if lo > 0 && int64(len(b.buf)+size) > e.log.Remaining() {
					// A chain cut short would hide every later record from
					// recovery, so failing to continue it stops writes.
					if _, err := e.log.Append(b.buf); err != nil {
						failAll(err, r)
						return
					}
					b.buf = b.buf[:0]
					if err := e.log.Roll(); err != nil {
						failAll(err, r)
						return
					}
				}
				pos := wal.Pos{Seg: e.log.WritePos().Seg, Off: e.log.WritePos().Off + int64(len(b.buf))}
				walOps = walOps[:0]
				for i := lo; i < hi; i++ {
					op := r.ops[i]
					w := wal.Op{Kind: wal.OpPut, Table: op.t.Schema().ID, Key: []byte(op.key), Val: op.val}
					if op.del {
						w.Kind = wal.OpDel
					}
					if prevs[i] != nil {
						w.Prev = prevs[i].Loc
					}
					walOps = append(walOps, w)
				}
				kind := wal.KindTx
				if hi < len(r.ops) {
					kind = wal.KindTxPart
				}
				b.buf, valRel = wal.AppendRecord(b.buf, kind, lsn, now, walOps, nil, valRel[:0])
				for i := lo; i < hi; i++ {
					op, j := r.ops[i], i-lo
					v := &Version{LSN: lsn, TS: now, Loc: wal.Loc{Ok: true, Seg: pos.Seg, Off: uint32(pos.Off), Idx: uint16(j)},
						ValOff: pos.Off + int64(valRel[j]), ValLen: uint32(len(op.val)), Deleted: op.del, Prev: walOps[j].Prev, Full: true}
					b.pending[pk{op.t, op.key}] = v
					pv := placedVersion{t: op.t, key: op.key, v: v}
					if len(op.sch.Indexes) > 0 {
						pv.sch, pv.sk = op.sch, op.sk
						e.placeSec(b, op, lsn)
					}
					b.installs = append(b.installs, pv)
					e.Stats.PayloadBytes.Add(int64(len(op.key) + len(op.val)))
				}
				lo = hi
			}
		}
		e.noteFK(r, lsn)
		e.tsRing[lsn&(tsRingSize-1)].Store(now)
		lsn++
		b.placed = append(b.placed, r)
	}
	if len(b.adds) > 0 {
		flushAdds(b.adds)
		b.adds = b.adds[:0]
	}
	b.uq = nil
	e.nextLSN = lsn
	e.lastTS = now
	b.lastLSN, b.lastTS = lsn-1, now
}

// complete makes a handed-off batch durable, installs it and answers.
// syncBatch makes a handed-off batch durable. On failure it answers the
// batch's requests and marks it failed (it must then not be installed).
func (e *Engine) syncBatch(b *commitBatch) {
	t0 := time.Now()
	if err := e.log.SyncTo(b.file); err != nil {
		e.fail(err, nil, b.placed, nil)
		b.failed = true
		return
	}
	sd := time.Since(t0)
	if LatDebug {
		LatSync.Add(sd)
	}
	if err := e.Failed(); err != nil {
		// A sync failed elsewhere (e.g. while rolling a segment) after this
		// batch was written; its fsync success proves nothing then.
		for _, r := range b.placed {
			finishReq(r, err)
		}
		b.failed = true
		return
	}
	e.Stats.SyncNsTotal.Add(int64(sd))
	if old := e.syncEWMA.Load(); old == 0 {
		e.syncEWMA.Store(int64(sd))
	} else {
		e.syncEWMA.Store(old + (int64(sd)-old)/8)
	}
	if int64(sd) > e.Stats.SyncNsMax.Load() {
		e.Stats.SyncNsMax.Store(int64(sd))
	}
	if sd > 50*time.Millisecond {
		e.Stats.SyncsOver50.Add(1)
		if sd > 200*time.Millisecond {
			e.logf("slow fdatasync: %v (batch of %d tx)", sd, len(b.placed))
		}
	}
	e.Stats.Syncs.Add(1)
	e.Stats.Batches.Add(1)

	// Durable: acknowledge now. New tables are registered first; rows are
	// found in the overlay until the installer has put them into the index.
	for _, d := range b.drops {
		e.dropTable(d.t, d.lsn)
	}
	for _, d := range b.ddls {
		if _, err := e.addTable(d); err != nil {
			e.logf("install ddl: %v", err)
		}
	}
	e.overlayPush(b)
	e.publish(b.lastLSN, b.endPos)
	e.Stats.Commits.Add(int64(len(b.placed)))
	if LatDebug {
		LatBatch.Add(time.Since(b.handed))
	}
	for _, r := range b.placed {
		finishReq(r, nil)
	}
}

// installBatch installs a durable batch (batches are installed in LSN
// order) and then drops it from the read overlay.
func (e *Engine) installBatch(b *commitBatch) {
	t0 := time.Now()
	e.gc.mu.Lock()
	// a dropped table's versions are superseded by the drop: reclaimable
	// once the horizon passes it (earlier batches are all installed)
	for _, d := range b.drops {
		d.t.index().ascend("", func(en *entry) bool {
			e.gc.supersede(en.seg, int64(en.valLen)+versionOverhead, d.lsn)
			return true
		})
	}
	// apply to clones of the touched tables' indexes, then publish them
	type work struct {
		t    *TableState
		tree *tree
	}
	var ws []work
	var cur work
	type supTask struct {
		pv  placedVersion
		old Version
	}
	var sups []supTask
	for _, pv := range b.installs {
		if cur.t != pv.t {
			found := false
			for _, w := range ws {
				if w.t == pv.t {
					cur, found = w, true
				}
			}
			if !found {
				cur = work{pv.t, pv.t.index().clone()}
				ws = append(ws, cur)
			}
		}
		tree := cur.tree
		if pv.v == nil { // drop a reclaimable tombstone
			if en, had := tree.get(pv.key); had {
				if old := en.version(); sameVersion(&old, pv.expect) {
					tree.del(pv.key)
					e.gc.account(&old, nil)
					e.Stats.GCDroppedTombstones.Add(1)
				}
			}
			continue
		}
		en, had := tree.put(pv.key, pv.v)
		old := en.version()
		wasLive := had && !old.Deleted
		if had && pv.sch != nil {
			sups = append(sups, supTask{pv, old})
		}
		if had {
			e.gc.account(&old, pv.v)
		} else {
			e.gc.account(nil, pv.v)
		}
		switch {
		case !pv.v.Deleted && !wasLive:
			pv.t.live.Add(1)
		case pv.v.Deleted && wasLive:
			pv.t.live.Add(-1)
		}
	}
	for _, w := range ws {
		w.t.idx.Store(w.tree)
	}
	e.gc.mu.Unlock()
	if len(sups) > 0 {
		// mark the entries of the versions just superseded dead
		marks := map[*secIndex][]deadRec{}
		for i := range sups {
			s := &sups[i]
			for si, ks := range e.supersede(s.pv.t, s.pv.key, &s.old, s.pv.v.LSN, s.pv.sch, s.pv.sk) {
				marks[si] = append(marks[si], ks...)
			}
		}
		for si, ks := range marks {
			si.markDead(ks)
		}
	}
	if len(b.remaps) > 0 {
		e.gc.remapMu.Lock()
		for _, r := range b.remaps {
			e.gc.remap[r.from] = r.e
		}
		e.gc.remapMu.Unlock()
	}
	if LatDebug {
		LatInstall.Add(time.Since(t0))
	}
	// submitters that read installSeq from here on see these versions
	b.seq = e.installSeq.Add(1)
	e.publishInstalled(b.lastLSN, b.endPos)
	e.overlayPop(b)
	for _, r := range b.placed {
		if r.onCommit != nil && r.lsn != 0 {
			r.onCommit(r.lsn)
		}
	}
}

// waitInstalled returns once every commit up to lsn is in the index.
func (e *Engine) waitInstalled(lsn uint64) {
	for e.installed.Load() < lsn {
		e.posMu.Lock()
		ch := e.installNotify
		done := e.installed.Load() >= lsn
		e.posMu.Unlock()
		if done {
			return
		}
		<-ch
	}
}

// InstalledPoint is the LSN and log position up to which the index is
// complete (a checkpoint taken from the index starts there).
func (e *Engine) InstalledPoint() (uint64, wal.Pos) {
	e.posMu.Lock()
	defer e.posMu.Unlock()
	return e.installed.Load(), e.installedPos
}

func (e *Engine) publishInstalled(lsn uint64, pos wal.Pos) {
	e.posMu.Lock()
	e.installedPos = pos
	e.installed.Store(lsn)
	close(e.installNotify)
	e.installNotify = make(chan struct{})
	e.posMu.Unlock()
}

func (e *Engine) overlayPush(b *commitBatch) {
	e.overlayMu.Lock()
	var next []*commitBatch
	if ov := e.overlay.Load(); ov != nil {
		next = append(next, *ov...)
	}
	next = append(next, b)
	e.overlay.Store(&next)
	e.overlayMu.Unlock()
}

func (e *Engine) overlayPop(b *commitBatch) {
	e.overlayMu.Lock()
	ov := e.overlay.Load()
	if ov == nil || len(*ov) == 0 || (*ov)[0] != b {
		panic("commit: overlay out of order")
	}
	if len(*ov) == 1 {
		e.overlay.Store(nil)
	} else {
		next := append([]*commitBatch(nil), (*ov)[1:]...)
		e.overlay.Store(&next)
	}
	e.overlayMu.Unlock()
}

func (e *Engine) publish(lsn uint64, pos wal.Pos) {
	e.posMu.Lock()
	e.durablePos = pos
	e.durable.Store(lsn)
	close(e.notify)
	e.notify = make(chan struct{})
	e.posMu.Unlock()
}

// fail switches the engine to fail-stop for writes. Requests that were
// placed in the failed batch get ErrUnknownOutcome; the rest get the error.
func (e *Engine) fail(cause error, batch, placed []*commitReq, current *commitReq) {
	e.logf("FATAL storage error, writes disabled until restart: %v", cause)
	err := fmt.Errorf("%w: %v", ErrUnknownOutcome, cause)
	e.failErr.Store(&err)
	done := map[*commitReq]bool{}
	for _, r := range placed {
		r.err = err
		close(r.done)
		done[r] = true
	}
	if current != nil && !done[current] {
		current.err = err
		close(current.done)
		done[current] = true
	}
	for _, r := range batch {
		select {
		case <-r.done:
		default:
			if !done[r] {
				r.err = err
				close(r.done)
			}
		}
	}
}
