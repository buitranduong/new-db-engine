package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"stask/internal/wal"
)

// Replication (physical, asynchronous).
//
// A replica holds a byte-identical copy of the leader's log: every byte is
// written at the same segment and offset as on the leader, so version
// locations (Loc, ValOff, Prev) mean the same on both, and the replica
// builds its index by replaying records exactly as recovery does. The
// leader ships only durable bytes, so a replica's log is always a prefix of
// the leader's: nothing it has can be lost by a leader crash.
//
// Starting from nothing, a replica receives a base backup (a checkpoint and
// every segment up to the durable point) and then recovers from it like a
// restarted engine; after that, and after its own restarts, it asks for the
// log from where its own ends.
//
// Log reclamation runs on the leader only. Its relocations reach the
// replica as KindMove records; dropped tombstones are not logged, so the
// replica drops the heads still stored in a segment when it removes that
// segment, which it does once the leader has removed it and the replica
// has applied everything the leader had made durable by then (see Trim).
// As after a restart of the leader, as-of reads that would need a relocated
// non-head version fail with ErrSnapshotTooOld on a replica.

var ErrReadOnly = errors.New("read-only replica: writes go to the leader")

// IsReplica reports whether the engine was opened as a replica.
func (e *Engine) IsReplica() bool { return e.opts.Replica }

// ---- leader side ----

// Shipper reads the log on behalf of one replica. While open it pins the
// segments from its position on against reclamation.
type Shipper struct {
	e   *Engine
	pos atomic.Pointer[wal.Pos]
}

func (s *Shipper) position() (wal.Pos, bool) {
	if p := s.pos.Load(); p != nil {
		return *p, true
	}
	return wal.Pos{}, false
}

// NewShipper registers a shipper positioned at from: segments at or after
// from.Seg are kept until Close or a later SetPos. It fails if the log no
// longer has from.
func (e *Engine) NewShipper(from wal.Pos) (*Shipper, error) {
	s := &Shipper{e: e}
	s.pos.Store(&from)
	e.gc.rmu.Lock()
	e.gc.readers = append(e.gc.readers, s)
	e.gc.rmu.Unlock()
	// registered first, then checked: a segment present now stays
	if segs := e.log.Segments(); from.Seg != 0 || from.Off != 0 {
		if len(segs) == 0 || from.Seg < segs[0] || e.log.Removed(from.Seg) {
			s.Close()
			return nil, fmt.Errorf("log position %d:%d was reclaimed: the replica needs a new base backup", from.Seg, from.Off)
		}
	}
	return s, nil
}

// Position is where the shipper's replica has acknowledged up to.
func (s *Shipper) Position() (wal.Pos, bool) { return s.position() }

// SetPos releases the segments before p (the replica has made them durable).
func (s *Shipper) SetPos(p wal.Pos) { s.pos.Store(&p) }

func (s *Shipper) Close() {
	s.e.gc.rmu.Lock()
	defer s.e.gc.rmu.Unlock()
	for i, r := range s.e.gc.readers {
		if r == logReader(s) {
			s.e.gc.readers = append(s.e.gc.readers[:i], s.e.gc.readers[i+1:]...)
			return
		}
	}
}

// OldestSegment is the first segment the log still has.
func (e *Engine) OldestSegment() uint32 {
	if segs := e.log.Segments(); len(segs) > 0 {
		return segs[0]
	}
	return 0
}

// ErrIdle: Read's stop channel fired before new log was durable.
var ErrIdle = errors.New("no new log")

// Read waits (until stop) for durable log beyond from and returns up to
// max bytes of whole records starting at from, the position after them and
// the durable LSN at the time of reading. Records never straddle the
// returned range; at the end of a sealed segment the next position is the
// start of the next segment (with no data).
func (s *Shipper) Read(from wal.Pos, max int64, stop <-chan struct{}) ([]byte, wal.Pos, uint64, error) {
	e := s.e
	for {
		d, dpos, ch := e.DurablePoint()
		if from.Less(dpos) {
			limit := int64(-1)
			if from.Seg == dpos.Seg {
				limit = dpos.Off
			}
			end := from.Off
			_, _, err := e.log.Scan(from.Seg, from.Off, limit, func(r *wal.Record) error {
				if end > from.Off && r.Pos.Off+int64(r.Size)-from.Off > max {
					return errStopScan
				}
				end = r.Pos.Off + int64(r.Size)
				return nil
			})
			if err != nil && err != errStopScan {
				return nil, from, 0, err
			}
			if end == from.Off {
				if from.Seg < dpos.Seg {
					return nil, wal.Pos{Seg: from.Seg + 1}, d, nil // end of a sealed segment
				}
				return nil, from, 0, fmt.Errorf("unreadable record at %d:%d", from.Seg, from.Off)
			}
			buf := make([]byte, end-from.Off)
			if _, err := e.log.ReadAt(from.Seg, from.Off, buf); err != nil {
				return nil, from, 0, err
			}
			return buf, wal.Pos{Seg: from.Seg, Off: end}, d, nil
		}
		select {
		case <-ch:
		case <-stop:
			return nil, from, 0, ErrIdle
		case <-e.stop:
			return nil, from, 0, ErrClosed
		}
	}
}

var errStopScan = errors.New("stop scan")

// BaseBackup sends what a new replica starts from: the newest checkpoint
// (taken now) and every segment's records up to a durable point at or after
// the checkpoint's start. The shipper must be positioned at the oldest
// segment (pinning everything). It returns the position the replica's log
// ends at.
func (e *Engine) BaseBackup(s *Shipper, sendCkpt func(name string, data []byte) error,
	sendSeg func(seg uint32, data []byte) error) (wal.Pos, error) {
	if err := e.Checkpoint(); err != nil {
		return wal.Pos{}, err
	}
	var name string
	var data []byte
	for try := 0; ; try++ { // a newer checkpoint may replace it meanwhile
		path, _ := latestCheckpoint(e.ckptDir())
		if path == "" {
			return wal.Pos{}, errors.New("no checkpoint")
		}
		var err error
		if data, err = os.ReadFile(path); err == nil {
			name = filepath.Base(path)
			break
		}
		if !os.IsNotExist(err) || try > 10 {
			return wal.Pos{}, err
		}
	}
	_, dpos, _ := e.DurablePoint()
	for _, seg := range e.log.Segments() {
		if seg > dpos.Seg {
			break
		}
		var buf []byte
		pos := wal.Pos{Seg: seg}
		for pos.Seg == seg && pos.Less(dpos) {
			b, next, _, err := s.Read(pos, 16<<20, nil)
			if err != nil {
				return wal.Pos{}, err
			}
			buf = append(buf, b...)
			pos = next
		}
		if err := sendSeg(seg, buf); err != nil {
			return wal.Pos{}, err
		}
	}
	if err := sendCkpt(name, data); err != nil {
		return wal.Pos{}, err
	}
	return dpos, nil
}

// ---- replica side ----

// WritePos is where the next shipped bytes must start (Apply's goroutine
// only).
func (e *Engine) WritePos() wal.Pos { return e.log.WritePos() }

// replState is the replica's apply state (one goroutine applies).
type replState struct {
	chain []*wal.Record // frames of a transaction whose final frame has not arrived
}

// Apply makes data, shipped from the leader for log position at, durable
// in the replica's log and installs it into the index. at must be the
// replica's write position (or the start of the next segment).
func (e *Engine) Apply(at wal.Pos, data []byte) error {
	if !e.opts.Replica {
		return errors.New("apply: not a replica")
	}
	cur := e.log.WritePos()
	if at != cur {
		if at.Seg == cur.Seg+1 && at.Off == 0 {
			if err := e.log.Roll(); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("apply: data for %d:%d, log is at %d:%d", at.Seg, at.Off, cur.Seg, cur.Off)
		}
	}
	if len(data) == 0 {
		return nil
	}
	if int64(len(data)) > e.log.Remaining() {
		return fmt.Errorf("apply: %d bytes overflow segment %d", len(data), at.Seg)
	}
	// decode and check the records before anything reaches the log: data
	// that does not continue it must not be written
	lastLSN := e.durable.Load()
	if n := len(e.repl.chain); n > 0 {
		lastLSN = e.repl.chain[n-1].LSN - 1 // a chain's frames share the LSN of its final frame
	}
	var recs []*wal.Record
	next := lastLSN + 1
	for off := 0; off < len(data); {
		r, err := wal.DecodeRecord(data[off:], wal.Pos{Seg: at.Seg, Off: at.Off + int64(off)})
		if err != nil {
			return fmt.Errorf("apply: record at %d:%d: %w", at.Seg, at.Off+int64(off), err)
		}
		if r.LSN != next {
			return fmt.Errorf("apply: lsn %d at %d:%d, expected %d", r.LSN, at.Seg, at.Off+int64(off), next)
		}
		if r.Kind != wal.KindTxPart {
			next++
		}
		off += r.Size
		recs = append(recs, r)
	}
	if _, err := e.log.Append(data); err != nil {
		return err
	}
	if err := e.log.Sync(); err != nil {
		return &wal.SyncError{Err: err}
	}
	e.gc.mu.Lock()
	clones := map[*TableState]*tree{}
	var applyErr error
	for _, r := range recs {
		if r.Kind == wal.KindTxPart {
			e.repl.chain = append(e.repl.chain, cloneRecord(r))
			continue
		}
		for _, part := range append(e.repl.chain, r) {
			if err := e.applyRecord(part, clones); err != nil {
				applyErr = err
				break
			}
		}
		e.repl.chain = nil
		if applyErr != nil {
			break
		}
		lastLSN = r.LSN
		e.lastTS = max(e.lastTS, r.TS)
		e.tsRing[r.LSN&(tsRingSize-1)].Store(r.TS)
	}
	for t, tree := range clones {
		t.idx.Store(tree)
	}
	e.gc.mu.Unlock()
	if applyErr != nil {
		return applyErr
	}
	end := wal.Pos{Seg: at.Seg, Off: at.Off + int64(len(data))}
	if len(e.repl.chain) > 0 {
		// the durable point stays before an unfinished transaction
		end = e.repl.chain[0].Pos
	}
	e.nextLSN = lastLSN + 1
	e.Stats.Commits.Add(int64(len(recs)))
	e.publish(lastLSN, end)
	e.publishInstalled(lastLSN, end)
	return nil
}

// applyRecord installs one record into clones of the touched indexes, as
// replay does during recovery.
func (e *Engine) applyRecord(r *wal.Record, clones map[*TableState]*tree) error {
	if r.Kind == wal.KindDDL {
		// the table indexes are built from the rows applied so far
		for t, tr := range clones {
			t.idx.Store(tr)
			delete(clones, t)
		}
		return e.replay(r) // registers the table
	}
	for i, op := range r.Ops {
		t := e.TableByID(op.Table)
		if t == nil {
			return fmt.Errorf("record lsn=%d references unknown table %d", r.LSN, op.Table)
		}
		tr := clones[t]
		if tr == nil {
			tr = t.index().clone()
			clones[t] = tr
		}
		key := string(op.Key)
		loc := wal.Loc{Ok: true, Seg: r.Pos.Seg, Off: uint32(r.Pos.Off), Idx: uint16(i)}
		if r.Kind != wal.KindMove {
			// skip a version newer than this record; an equal LSN is the same
			// transaction (several operations on one key apply in order)
			old, had := tr.get(key)
			if had && old.lsn > r.LSN {
				continue
			}
			wasLive := had && old.flags&entDeleted == 0
			v := &Version{LSN: r.LSN, TS: r.TS, Loc: loc, ValOff: op.ValOff, ValLen: uint32(len(op.Val)),
				Deleted: op.Kind == wal.OpDel, Prev: op.Prev}
			var oldV *Version
			if had && len(t.secList()) > 0 {
				ov := old.version()
				oldV = &ov
			}
			tr.put(key, v)
			if len(t.secList()) > 0 {
				e.secReplay(t, r.LSN, key, op.Val, v.Deleted, oldV)
			}
			switch {
			case !v.Deleted && !wasLive:
				t.live.Add(1)
			case v.Deleted && wasLive:
				t.live.Add(-1)
			}
			continue
		}
		// a move: relocate only if the moved version is still the head
		if old, had := tr.get(key); had && old.lsn == op.OrigLSN {
			tr.put(key, &Version{LSN: op.OrigLSN, TS: op.OrigTS, Loc: loc, ValOff: op.ValOff,
				ValLen: uint32(len(op.Val)), Deleted: op.Kind == wal.OpDel, Prev: op.Prev})
		}
	}
	return nil
}

// Trim removes the replica's segments before seg, which the leader has
// reclaimed. The caller guarantees that everything the leader had made
// durable when it reported seg as its oldest is applied: every head the
// leader relocated out of them has then been relocated here too, and the
// heads still left in them are tombstones the leader dropped. Those are
// dropped here, a checkpoint that no longer references the segments is
// written, and the segments are removed once no read can use them.
func (e *Engine) Trim(before uint32) (int, error) {
	if !e.opts.Replica {
		return 0, errors.New("trim: not a replica")
	}
	minReader, haveReader := e.minReaderSeg()
	var del []uint32
	for _, s := range e.log.Segments() {
		if s >= before || s >= e.log.WritePos().Seg || (haveReader && s >= minReader) {
			break
		}
		del = append(del, s)
	}
	if len(del) == 0 {
		return 0, nil
	}
	gone := map[uint32]bool{}
	for _, s := range del {
		gone[s] = true
	}
	e.gc.mu.Lock()
	dropped, kept := 0, 0
	for _, t := range e.allTables() {
		var stale []string
		cur := t.index()
		cur.ascend("", func(en *entry) bool {
			if gone[en.seg] {
				if en.flags&entDeleted == 0 {
					kept++
				} else {
					stale = append(stale, strings.Clone(cur.keyOf(en)))
				}
			}
			return true
		})
		if len(stale) == 0 {
			continue
		}
		tr := cur.clone()
		for _, key := range stale {
			tr.del(key)
			dropped++
		}
		t.idx.Store(tr)
	}
	e.gc.mu.Unlock()
	if kept > 0 {
		// the leader still had heads there when it reported the removal:
		// the caller applied too little; keep everything
		return 0, fmt.Errorf("trim: %d live heads still in segments %v", kept, del)
	}
	e.Stats.GCDroppedTombstones.Add(int64(dropped))
	if err := e.Checkpoint(); err != nil {
		return 0, err
	}
	// history of versions superseded before the oldest pinned snapshot may
	// be gone: reads below it fail instead of missing versions
	h := e.minPinned(e.durable.Load())
	if h > e.gc.floor.Load() {
		e.gc.floor.Store(h)
	}
	e.gc.condemned.Store(&gone)
	defer e.gc.condemned.Store(nil)
	e.waitReaders()
	for _, s := range del {
		if err := e.log.Remove(s); err != nil {
			return 0, err
		}
		e.gc.mu.Lock()
		delete(e.gc.segs, s)
		e.gc.mu.Unlock()
		e.Stats.GCReclaimedSegs.Add(1)
	}
	e.logf("replica: removed segment(s) %v reclaimed by the leader, dropped %d tombstone(s)", del, dropped)
	return len(del), nil
}

// ReplicaLag is how long ago the newest applied commit was made on the
// leader (by the leader's clock).
func (e *Engine) ReplicaLag() time.Duration {
	ts, ok := e.CommitTS(e.durable.Load())
	if !ok {
		return 0
	}
	return time.Since(time.Unix(0, ts))
}
