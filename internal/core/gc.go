package core

import (
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/wal"
)

// Log reclamation (GC).
//
// Values live only in the log, so space is reclaimed by segment cleaning:
//
//  1. Horizon H = min(oldest pinned snapshot, durable LSN as of now-Retention).
//     A version superseded at an LSN <= H is invisible at every snapshot
//     >= H, so no current or future reader at or above H needs it.
//  2. A sealed segment is a victim when the share of still-current (head)
//     bytes in it is small.
//  3. Every version in the victim that is still needed is copied to the log
//     tail in KindMove records, through the committer so that copies
//     serialize with commits: heads (the index then points at the copy) and
//     versions superseded after H (newer versions still reference them by
//     their old location, so an in-memory remap old -> new location is kept
//     until the floor passes them). Tombstones older than H are dropped from
//     the index instead.
//  4. The victim is deleted once a checkpoint that cloned the index after
//     step 3 is durable and starts beyond it, no log follower still reads
//     it, and every read that may hold a pointer into it has finished
//     (read epochs). Before deleting, the reclaimed floor is raised to H:
//     reads at a snapshot below the floor fail with ErrSnapshotTooOld
//     instead of silently missing versions.
//
// The remap is not persisted: after a restart, as-of reads that would need a
// relocated non-head version fail with ErrSnapshotTooOld. Reads at current
// snapshots only need heads, whose locations are in checkpoints and replay.
var (
	ErrSnapshotTooOld = errors.New("snapshot is older than the retained history (versions were reclaimed)")
	errTruncated      = errors.New("history truncated by log reclamation")
)

type segStat struct {
	live int64 // bytes of head versions stored in the segment
	// bytes of versions stored here by the LSN that superseded them
	// (ascending, at most maxSups entries per retention class; when full,
	// the two closest neighbours merge under the newer LSN, so entries
	// spread over the whole range): those superseded after the horizon are
	// still needed, so draining copies them instead of reclaiming them. A
	// class is a table's history_retention (0: the engine-wide one), as a
	// segment mixes tables whose history is kept for different times.
	sups []supClass
}

type supClass struct {
	ret int64
	l   []supBytes
}

type supBytes struct {
	lsn   uint64 // newest superseding LSN merged into this entry
	bytes int64
}

const maxSups = 64

// needed is the number of bytes draining the segment would copy: heads and
// versions superseded after the horizon of their class.
func (s *segStat) needed(hs *horizons) int64 {
	n := s.live
	for _, c := range s.sups {
		h := hs.ofClass(c.ret)
		for i := len(c.l) - 1; i >= 0 && c.l[i].lsn > h; i-- {
			n += c.l[i].bytes
		}
	}
	return n
}

type remapEntry struct {
	to  wal.Loc
	sup uint64 // LSN of the version that superseded the relocated one
}

type lsnMark struct {
	at  int64 // unix nanos
	lsn uint64
}

type paddedCount struct {
	n atomic.Int64
	_ [56]byte
}

type drainedSeg struct {
	seg      uint32
	needCkpt int64 // a checkpoint with at least this generation must be durable
}

type gcState struct {
	mu   sync.Mutex // guards segs
	segs map[uint32]*segStat

	pinMu sync.Mutex // guards marks
	marks []lsnMark
	// pinned snapshots, sharded: every transaction pins one at Begin and
	// unpins it at its end, tens of thousands of times a second. With one
	// mutex, a holder or the waiter it hands the lock to waiting for a CPU
	// (a parallel aggregate keeps them all busy) stalls every Begin behind
	// it: a lock convoy that held point statements for the whole aggregate.
	// (PostgreSQL 14 removed the same kind of shared snapshot bottleneck
	// from GetSnapshotData.)
	// pins[sysShard] holds the long-lived pins of CDC slots and log
	// followers, which the snapshot age cap never expires.
	pins [pinShards + 1]pinShard
	// snapCut: transaction snapshots below it outlived Options.MaxSnapshotAge
	// and no longer hold back the horizon; their transactions fail with
	// ErrSnapshotTooOld (only grows)
	snapCut atomic.Uint64

	hist    histState
	remapMu sync.RWMutex
	remap   map[wal.Loc]remapEntry // relocated non-head versions: referenced location -> copy

	floor atomic.Uint64
	epoch atomic.Uint64
	// segments being deleted: new reads no longer follow pointers into them
	condemned atomic.Pointer[map[uint32]bool]
	active    [2][64]paddedCount

	rmu     sync.Mutex
	readers []logReader // log followers and replica shippers

	drained []drainedSeg // cleaner goroutine only

	ckptStarted  atomic.Int64 // generation of the last checkpoint that started cloning
	ckptDone     atomic.Int64 // generation of the last durable checkpoint
	ckptStartSeg atomic.Uint32
}

type GCStats struct {
	Enabled           bool   `json:"enabled"`
	Floor             uint64 `json:"floor_lsn"`
	Horizon           uint64 `json:"horizon_lsn"`
	ReclaimedSegments int64  `json:"reclaimed_segments"`
	MovedVersions     int64  `json:"moved_versions"`
	MovedBytes        int64  `json:"moved_bytes"`
	DroppedTombstones int64  `json:"dropped_tombstones"`
	Remapped          int    `json:"remapped_versions"`
	Rounds            int64  `json:"rounds"`
	LiveBytes         int64  `json:"live_bytes"`
	PinnedSnapshots   int    `json:"pinned_snapshots"`
	// time since the oldest stale transaction snapshot (older than the
	// durable point) was last taken: how long it has held back the horizon
	OldestSnapshotAgeMs int64 `json:"oldest_snapshot_age_ms"`
	// transaction snapshots below this LSN were expired by MaxSnapshotAge
	SnapshotCut    uint64 `json:"snapshot_cut_lsn"`
	MaxSnapshotAge string `json:"max_snapshot_age"`
	// history retention (history.go): the engine-wide window, the tables
	// that set their own, and the time marks / relocation entries held
	Retention      string            `json:"history_retention"`
	TableRetention map[string]string `json:"table_history_retention,omitempty"`
	TimeMarks      int               `json:"time_marks"`
}

// versionOverhead is what a version costs besides its value (accounting).
const versionOverhead = 64

func versionBytes(v *Version) int64 { return int64(v.ValLen) + versionOverhead }

func (g *gcState) stat(seg uint32) *segStat {
	s := g.segs[seg]
	if s == nil {
		s = &segStat{}
		g.segs[seg] = s
	}
	return s
}

// ---- read epochs ----

// enterRead marks the start of an operation that may dereference version
// locations. The caller must call Add(-1) on the result when done.
func (e *Engine) enterRead() *atomic.Int64 {
	for {
		ep := e.gc.epoch.Load()
		c := &e.gc.active[ep&1][rand.Uint32()&63].n
		c.Add(1)
		if e.gc.epoch.Load() == ep {
			return c
		}
		c.Add(-1)
	}
}

// waitReaders returns once every read that entered before the call has
// finished. Reads entering afterwards observe everything published before it.
func (e *Engine) waitReaders() {
	ep := e.gc.epoch.Add(1) - 1
	for {
		var n int64
		for i := range e.gc.active[ep&1] {
			n += e.gc.active[ep&1][i].n.Load()
		}
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (e *Engine) checkSnapshot(snap uint64) error {
	if snap < e.gc.floor.Load() {
		return ErrSnapshotTooOld
	}
	return nil
}

// ---- snapshot pins ----

// pinDurable atomically reads the durable point and pins its LSN, so that
// the horizon cannot pass it until unpin.
const (
	pinShards = 32        // transaction pins, spread at random
	sysShard  = pinShards // slots and followers
)

type pinShard struct {
	mu   sync.Mutex
	pins map[uint64]pinRec
	_    [48]byte // one cache line each
}

// pinRec counts the pins of one LSN and when the last of them was taken
// (monoNow). A transaction pins the durable LSN, so a smaller LSN was last
// pinned no later than a larger one: its youngest holder is the oldest.
type pinRec struct {
	n  int
	at int64
}

var clockBase = time.Now()

// monoNow is a monotonic clock in nanoseconds.
func monoNow() int64 { return int64(time.Since(clockBase)) }

// pinDurableIn pins the durable LSN in shard sh. The LSN is read under the
// shard's lock: a horizon computed concurrently either read the durable LSN
// before this did (so it is at most this one) or scans the shard after this
// pin (see minPinned).
func (e *Engine) pinDurableIn(sh int) (uint64, wal.Pos) {
	now := monoNow()
	p := &e.gc.pins[sh]
	p.mu.Lock()
	lsn, pos, _ := e.DurablePoint()
	r := p.pins[lsn]
	r.n++
	r.at = max(r.at, now)
	p.pins[lsn] = r
	p.mu.Unlock()
	return lsn, pos
}

// pinDurable and unpin use sysShard: slots and view followers, a few pins
// held for long.
func (e *Engine) pinDurable() (uint64, wal.Pos) { return e.pinDurableIn(sysShard) }
func (e *Engine) unpin(lsn uint64)              { e.unpinIn(sysShard, lsn) }

// pin pins a given LSN (a slot's position).
func (e *Engine) pin(lsn uint64) {
	p := &e.gc.pins[sysShard]
	p.mu.Lock()
	r := p.pins[lsn]
	r.n++
	p.pins[lsn] = r
	p.mu.Unlock()
}

func (e *Engine) unpinIn(sh int, lsn uint64) {
	p := &e.gc.pins[sh]
	p.mu.Lock()
	if r := p.pins[lsn]; r.n <= 1 {
		delete(p.pins, lsn)
	} else {
		r.n--
		p.pins[lsn] = r
	}
	p.mu.Unlock()
}

// minPinned returns min(h, every pinned LSN that still counts). h must
// have been read from the durable LSN before the call.
//
// With Options.MaxSnapshotAge set, a transaction snapshot that is stale
// (below the durable LSN, so no new transaction can pin it any more) and
// was last pinned longer ago than the cap is expired: snapCut moves past
// it before it stops counting, so its transaction's next read or commit
// fails with ErrSnapshotTooOld instead of missing reclaimed versions
// (TiDB's tidb_gc_max_wait_time likewise forces the GC safe point past
// transactions that block it too long).
func (e *Engine) minPinned(h uint64) uint64 {
	cut := e.gc.snapCut.Load()
	if maxAge := e.opts.MaxSnapshotAge; maxAge > 0 {
		stale := e.durable.Load()
		old := monoNow() - int64(maxAge)
		newCut := cut
		for i := 0; i < pinShards; i++ {
			p := &e.gc.pins[i]
			p.mu.Lock()
			for lsn, r := range p.pins {
				if lsn >= newCut && lsn < stale && r.at <= old {
					newCut = lsn + 1
				}
			}
			p.mu.Unlock()
		}
		for newCut > cut && !e.gc.snapCut.CompareAndSwap(cut, newCut) {
			cut = e.gc.snapCut.Load()
		}
		cut = max(cut, newCut)
	}
	for i := range e.gc.pins {
		p := &e.gc.pins[i]
		p.mu.Lock()
		for lsn := range p.pins {
			if i == sysShard || lsn >= cut {
				h = min(h, lsn)
			}
		}
		p.mu.Unlock()
	}
	return h
}

// oldestSnapshotAge is how long ago the oldest stale transaction snapshot
// that still holds back the horizon was last pinned (0: none).
func (e *Engine) oldestSnapshotAge() time.Duration {
	cut, stale, now := e.gc.snapCut.Load(), e.durable.Load(), monoNow()
	var age int64
	for i := 0; i < pinShards; i++ {
		p := &e.gc.pins[i]
		p.mu.Lock()
		for lsn, r := range p.pins {
			if lsn >= cut && lsn < stale {
				age = max(age, now-r.at)
			}
		}
		p.mu.Unlock()
	}
	return time.Duration(age)
}

// snapshotExpired reports whether snap was expired by MaxSnapshotAge.
func (e *Engine) snapshotExpired(snap uint64) bool { return snap < e.gc.snapCut.Load() }

func (e *Engine) pinnedCount() int {
	n := 0
	for i := range e.gc.pins {
		p := &e.gc.pins[i]
		p.mu.Lock()
		n += len(p.pins)
		p.mu.Unlock()
	}
	return n
}

// horizon is the lowest of the horizons of all tables (the one of the
// longest retention): see horizons in history.go.
func (e *Engine) horizon() uint64 { return e.horizons().min }

// retention is the engine-wide history window. At least 2s are kept so that
// a reader that has just looked up the durable LSN can still use it as a
// snapshot; a negative GCRetention (tests only) keeps nothing beyond pinned
// snapshots.
func (e *Engine) retention() time.Duration {
	if e.opts.GCRetention < 0 {
		return 0
	}
	return max(e.opts.GCRetention, 2*time.Second)
}

// ---- accounting ----

// initGC rebuilds per-segment statistics after recovery.
func (e *Engine) initGC() {
	g := &e.gc
	g.segs = map[uint32]*segStat{}
	for i := range g.pins {
		g.pins[i].pins = map[uint64]pinRec{}
	}
	g.remap = map[wal.Loc]remapEntry{}
	segLSN := e.restoreHistoryState()
	for _, t := range e.Tables() { // dropped tables' versions count as superseded now
		t.index().ascend("", func(en *entry) bool {
			g.stat(en.seg).live += int64(en.valLen) + versionOverhead
			return true
		})
	}
	// When versions were superseded is not known after a restart. Assume
	// at the end of the segment (the first LSN of the next one): the
	// estimate only decides which segments to try, a drain checks every
	// version against its chain, and assuming "just now" (as before) would
	// stop reclaiming for a whole retention window after every restart.
	segs := e.log.Segments()
	for i, seg := range segs {
		st := g.stat(seg)
		if rest := e.log.SegMax() - st.live; rest > 0 {
			end := e.durable.Load()
			if i+1 < len(segs) && segLSN[segs[i+1]] > 0 {
				end = segLSN[segs[i+1]]
			}
			st.sups = []supClass{{ret: e.longestClass(), l: []supBytes{{end, rest}}}}
		}
	}
}

// account updates segment statistics for one installed index change:
// v replaced old as the head (either may be nil).
func (g *gcState) account(ret int64, old, v *Version) {
	if old != nil {
		b := versionBytes(old)
		if v != nil && v.LSN != old.LSN { // a relocated copy supersedes nothing
			g.supersede(old.Loc.Seg, b, v.LSN, ret)
		} else {
			g.stat(old.Loc.Seg).live -= b
		}
	}
	if v != nil {
		g.stat(v.Loc.Seg).live += versionBytes(v)
	}
}

// supersede moves b live bytes of seg to those superseded at lsn.
func (g *gcState) supersede(seg uint32, b int64, lsn uint64, ret int64) {
	s := g.stat(seg)
	s.live -= b
	ci := -1
	for i := range s.sups {
		if s.sups[i].ret == ret {
			ci = i
		}
	}
	if ci < 0 {
		s.sups = append(s.sups, supClass{ret: ret})
		ci = len(s.sups) - 1
	}
	c := &s.sups[ci]
	if n := len(c.l); n > 0 && c.l[n-1].lsn >= lsn {
		c.l[n-1].bytes += b
		return
	}
	if n := len(c.l); n == maxSups {
		best := 0
		for i := 1; i < n-1; i++ {
			if c.l[i+1].lsn-c.l[i].lsn < c.l[best+1].lsn-c.l[best].lsn {
				best = i
			}
		}
		c.l[best+1].bytes += c.l[best].bytes
		c.l = append(c.l[:best], c.l[best+1:]...)
	}
	c.l = append(c.l, supBytes{lsn, b})
}

// resolve maps a referenced version location to where the version now is.
func (e *Engine) resolve(loc wal.Loc) (wal.Loc, bool) {
	e.gc.remapMu.RLock()
	r, ok := e.gc.remap[loc]
	e.gc.remapMu.RUnlock()
	if ok {
		return r.to, true
	}
	return loc, false
}

// supersededAfter reports whether the version stored at p is still needed
// because a version newer than the horizon h references it; it returns
// that version's LSN and the location it uses for the reference.
func (e *Engine) supersededAfter(head *Version, p wal.Loc, h uint64) (uint64, wal.Loc, bool) {
	defer e.enterRead().Add(-1)
	for v := head; v != nil && v.LSN > h; {
		if e.fill(v) != nil {
			return v.LSN, p, true // unknown: keep (relocate) the version
		}
		if v.Prev.Ok {
			if r, _ := e.resolve(v.Prev); r == p {
				return v.LSN, v.Prev, true
			}
		}
		var err error
		if v, err = e.previous(v); err != nil {
			// truncated: everything older was superseded at or below the floor
			return 0, wal.Loc{}, false
		}
	}
	return 0, wal.Loc{}, false
}

func (e *Engine) GCStats() GCStats {
	st := GCStats{
		Enabled:           e.opts.GC,
		Floor:             e.gc.floor.Load(),
		Horizon:           e.horizon(),
		ReclaimedSegments: e.Stats.GCReclaimedSegs.Load(),
		MovedVersions:     e.Stats.GCMovedVersions.Load(),
		MovedBytes:        e.Stats.GCMovedBytes.Load(),
		DroppedTombstones: e.Stats.GCDroppedTombstones.Load(),
		Rounds:            e.Stats.GCRounds.Load(),
	}
	e.gc.mu.Lock()
	for _, s := range e.gc.segs {
		st.LiveBytes += s.live
	}
	e.gc.mu.Unlock()
	st.PinnedSnapshots = e.pinnedCount()
	st.OldestSnapshotAgeMs = e.oldestSnapshotAge().Milliseconds()
	st.SnapshotCut = e.gc.snapCut.Load()
	if e.opts.MaxSnapshotAge > 0 {
		st.MaxSnapshotAge = e.opts.MaxSnapshotAge.String()
	} else {
		st.MaxSnapshotAge = "off"
	}
	e.gc.remapMu.RLock()
	st.Remapped = len(e.gc.remap)
	e.gc.remapMu.RUnlock()
	st.Retention = FormatRetention(e.retention())
	for _, t := range e.Tables() {
		if r := t.Schema().HistoryRetention; r > 0 {
			if st.TableRetention == nil {
				st.TableRetention = map[string]string{}
			}
			st.TableRetention[t.Schema().Name] = FormatRetention(time.Duration(r))
		}
	}
	e.gc.pinMu.Lock()
	st.TimeMarks = len(e.gc.marks)
	e.gc.pinMu.Unlock()
	return st
}

// ---- cleaner ----

type moveOp struct {
	t     *TableState
	key   string
	head  *Version // expected current head; nil for a version superseded after the horizon
	ident wal.Loc  // non-head: the location newer versions reference it by
	sup   uint64   // non-head: LSN of the version that superseded it
	lsn   uint64   // identity of the version
	ts    int64
	del   bool
	prev  wal.Loc
	val   []byte
	drop  bool // drop the (tombstone) head from the index instead of moving it
}

// logReader reads the log sequentially from a position; segments from
// there on must not be removed.
type logReader interface {
	position() (wal.Pos, bool)
}

type remapSet struct {
	from wal.Loc
	e    remapEntry
}

func (e *Engine) minReaderSeg() (uint32, bool) {
	e.gc.rmu.Lock()
	defer e.gc.rmu.Unlock()
	var m uint32
	ok := false
	for _, f := range e.gc.readers {
		if p, started := f.position(); started && (!ok || p.Seg < m) {
			m, ok = p.Seg, true
		}
	}
	return m, ok
}

func (e *Engine) cleaner() {
	defer e.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
		}
		e.addMark()
		if e.Failed() != nil || time.Since(last) < e.opts.GCInterval {
			continue
		}
		last = time.Now()
		if _, err := e.GCRound(); err != nil {
			e.logf("gc: %v", err)
		}
	}
}

// GCRound runs one cleaning round and returns the number of segments deleted.
func (e *Engine) GCRound() (int, error) {
	e.gcRoundMu.Lock()
	defer e.gcRoundMu.Unlock()
	e.Stats.GCRounds.Add(1)
	e.checkSlots()
	if len(e.gc.drained) == 0 {
		victims := e.pickVictims()
		if len(victims) == 0 {
			return 0, nil
		}
		for _, seg := range victims {
			if err := e.drain(seg); err != nil {
				return 0, err
			}
		}
		need := e.gc.ckptStarted.Load() + 1
		for _, seg := range victims {
			e.gc.drained = append(e.gc.drained, drainedSeg{seg, need})
		}
		if err := e.Checkpoint(); err != nil {
			return 0, err
		}
	}
	return e.deleteDrained()
}

func (e *Engine) pickVictims() []uint32 {
	_, dpos, _ := e.DurablePoint()
	hs := e.horizons()
	e.releaseDropped(hs.min)
	type cand struct {
		seg  uint32
		live int64
	}
	var cs []cand
	e.gc.mu.Lock()
	for _, seg := range e.log.Segments() {
		if seg >= dpos.Seg { // only sealed segments
			continue
		}
		s := e.gc.stat(seg)
		if need := s.needed(hs); float64(need) <= e.opts.GCMaxLiveRatio*float64(e.log.SegMax()) {
			cs = append(cs, cand{seg, need})
		}
	}
	e.gc.mu.Unlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].live < cs[j].live })
	var out []uint32
	var moved int64
	for _, c := range cs {
		if len(out) > 0 && moved+c.live > e.opts.GCMaxMoveBytes {
			break
		}
		out = append(out, c.seg)
		moved += c.live
	}
	return out
}

// drain moves every head still stored in seg to the log tail.
func (e *Engine) drain(seg uint32) error {
	hs := e.horizons()
	var ops []moveOp
	var size int64
	t0 := time.Now()
	var sent int64
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		r := &commitReq{moves: ops}
		if err := e.submit(r); err != nil {
			return err
		}
		// remaps and relocated heads take effect at install
		e.waitInstalled(e.durable.Load())
		sent += size
		ops, size = nil, 0
		// rate limit relocation so that it does not starve foreground commits
		if e.opts.GCRateBytes > 0 {
			if ahead := time.Duration(float64(sent)/float64(e.opts.GCRateBytes)*1e9) - time.Since(t0); ahead > 0 {
				time.Sleep(ahead)
			}
		}
		return nil
	}
	var ferr error
	memo := &chainMemo{m: map[memoKey]*memoChain{}}
	_, _, err := e.log.Scan(seg, 0, -1, func(rec *wal.Record) error {
		select {
		case <-e.stop:
			return ErrClosed
		default:
		}
		if rec.Kind != wal.KindTx && rec.Kind != wal.KindTxPart && rec.Kind != wal.KindMove {
			return nil
		}
		for i, op := range rec.Ops {
			t := e.TableByID(op.Table)
			if t == nil {
				continue
			}
			h := hs.of(t)
			if d := t.droppedLSN.Load(); d != 0 && d <= h {
				continue // dropped, and no snapshot reads it any more
			}
			key := string(op.Key)
			p := wal.Loc{Ok: true, Seg: seg, Off: uint32(rec.Pos.Off), Idx: uint16(i)}
			m := moveOp{t: t, key: key, lsn: rec.LSN, ts: rec.TS, del: op.Kind == wal.OpDel, prev: op.Prev}
			if rec.Kind == wal.KindMove {
				m.lsn, m.ts = op.OrigLSN, op.OrigTS
			}
			head := t.head(key)
			if head != nil && head.Loc == p {
				m.head = head
				m.drop = head.Deleted && head.LSN <= h
			} else {
				sup, ident, needed := memo.superseded(e, t, key, head, p, h)
				if !needed {
					continue // garbage
				}
				m.ident, m.sup = ident, sup
			}
			if !m.drop {
				m.val = append([]byte(nil), op.Val...)
				size += int64(len(op.Key) + len(op.Val) + 48)
			}
			ops = append(ops, m)
			if size >= 1<<20 || len(ops) >= 4096 {
				if ferr = flush(); ferr != nil {
					return ferr
				}
			}
		}
		return nil
	})
	if ferr != nil {
		return ferr
	}
	if err != nil {
		return err
	}
	return flush()
}

func (e *Engine) deleteDrained() (int, error) {
	hs := e.horizons()
	minReader, haveReader := e.minReaderSeg()
	var keep []drainedSeg
	var del []uint32
	e.gc.mu.Lock()
	for _, d := range e.gc.drained {
		s := e.gc.stat(d.seg)
		switch {
		case s.live != 0:
			// a head stayed behind (should not happen); drop and re-pick later
			e.logf("gc: segment %d still holds %d live bytes after draining", d.seg, s.live)
		case e.gc.ckptDone.Load() < d.needCkpt || d.seg >= e.gc.ckptStartSeg.Load(),
			haveReader && d.seg >= minReader, e.archiveHolds(d.seg):
			keep = append(keep, d)
		default:
			del = append(del, d.seg)
		}
	}
	e.gc.mu.Unlock()
	e.gc.drained = keep
	if len(del) == 0 {
		return 0, nil
	}
	e.raiseFloors(hs)
	// the relocation table and floors must be durable before the segments go
	if err := e.persistHistoryState(); err != nil {
		return 0, err
	}
	// Condemn before waiting: a read that starts after this resolves
	// versions in these segments through the remap, so once the reads that
	// started before have finished, nothing can still be headed into them.
	cond := map[uint32]bool{}
	for _, seg := range del {
		cond[seg] = true
	}
	e.gc.condemned.Store(&cond)
	defer e.gc.condemned.Store(nil)
	e.waitReaders()
	floor := e.gc.floor.Load()
	e.gc.remapMu.Lock()
	for from, r := range e.gc.remap {
		if r.sup <= floor {
			delete(e.gc.remap, from) // no reader at or above the floor can reach it
		}
	}
	e.gc.remapMu.Unlock()
	e.pruneRemapFile()
	for _, seg := range del {
		if err := e.log.Remove(seg); err != nil {
			return 0, err
		}
		e.gc.mu.Lock()
		delete(e.gc.segs, seg)
		e.gc.mu.Unlock()
		e.Stats.GCReclaimedSegs.Add(1)
	}
	e.logf("gc: reclaimed %d segment(s) %v, floor lsn=%d", len(del), del, e.gc.floor.Load())
	return len(del), nil
}

// releaseDropped frees the index of each dropped table that no snapshot
// can read any more (its versions were accounted as superseded at the
// drop, so their segments are reclaimed like any other garbage).
func (e *Engine) releaseDropped(h uint64) {
	installed, _ := e.InstalledPoint()
	for _, t := range e.allTables() {
		if d := t.droppedLSN.Load(); d != 0 && d <= h && d <= installed && t.index().Len() > 0 {
			t.idx.Store(newTree(&t.keys))
			t.live.Store(0)
		}
	}
}
