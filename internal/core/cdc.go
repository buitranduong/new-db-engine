package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"stask/internal/schema"
	"stask/internal/wal"
)

// Change data capture: durable slots that decode committed transactions
// from the log, like PostgreSQL's logical replication slots
// (https://www.postgresql.org/docs/16/logicaldecoding-explanation.html).
//
// The log already is the database, so decoding needs nothing on the write
// path: a slot is a position in the log (just after the last transaction
// its consumer confirmed) plus two pins that keep what decoding needs:
//
//   - the segments from its position on (a Shipper, as a replica's), so the
//     records are still there;
//   - a snapshot at its LSN (as a transaction's), so the version an update
//     replaced is still readable: a put whose previous version was live is
//     an update, one without (or replacing a tombstone) is an insert.
//
// Like PostgreSQL's slots (max_slot_wal_keep_size), a slot that falls
// further behind than Options.SlotMaxLagBytes is invalidated rather than
// letting the log grow without bound.
//
// Slots live in <dir>/slots/<name>.json, written atomically. They exist on
// the leader only.

// SlotInfo is the persistent state of a slot.
type SlotInfo struct {
	Name    string `json:"name"`
	Plugin  string `json:"plugin"`
	Seg     uint32 `json:"seg"` // log position just after the last consumed transaction
	Off     int64  `json:"off"`
	LSN     uint64 `json:"lsn"` // commit LSN of the last consumed transaction (or the creation point)
	Created int64  `json:"created"`
	// Invalid is set when the slot fell too far behind and lost its log.
	Invalid string `json:"invalid,omitempty"`
}

// Pos is the slot's log position.
func (s SlotInfo) Pos() wal.Pos { return wal.Pos{Seg: s.Seg, Off: s.Off} }

type slot struct {
	mu       sync.Mutex // one decoder at a time
	adv      sync.Mutex // serializes changes of the position (advance, invalidation)
	info     SlotInfo   // guarded by e.slotMu
	ship     *Shipper   // guarded by e.slotMu
	pin      uint64     // pinned snapshot (0: none), guarded by e.slotMu
	mirrored time.Time  // last position written to the log (guarded by e.slotMu)
}

var (
	ErrSlotExists   = errors.New("replication slot already exists")
	ErrNoSlot       = errors.New("replication slot does not exist")
	ErrSlotInvalid  = errors.New("replication slot was invalidated")
	ErrSlotBusy     = errors.New("replication slot is active for another consumer")
	slotNameRe      = regexp.MustCompile(`^[a-z0-9_]{1,63}$`)
	errStopDecoding = errors.New("stop decoding")
)

func (e *Engine) slotDir() string { return filepath.Join(e.opts.Dir, "slots") }

// loadSlots registers the slots on disk (Open, before the log cleaner
// starts, so their log and snapshots are still there).
func (e *Engine) loadSlots() error {
	e.slots = map[string]*slot{}
	ents, err := os.ReadDir(e.slotDir())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	have := map[string]SlotInfo{}
	for _, en := range ents {
		n := en.Name()
		if strings.HasSuffix(n, ".tmp") {
			os.Remove(filepath.Join(e.slotDir(), n))
			continue
		}
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(e.slotDir(), n))
		if err != nil {
			return err
		}
		var info SlotInfo
		if err := json.Unmarshal(b, &info); err != nil {
			return fmt.Errorf("slot %s: %w", n, err)
		}
		have[info.Name] = info
	}
	e.slotsFromTable(have)
	for _, info := range have {
		if _, err := os.Stat(filepath.Join(e.slotDir(), info.Name+".json")); err != nil {
			if err := e.saveSlot(info); err != nil { // adopted from the table
				return err
			}
		}
		s := &slot{info: info}
		if info.Invalid == "" {
			if s.ship, err = e.NewShipper(info.Pos()); err != nil {
				s.info.Invalid = err.Error()
				e.logf("cdc: slot %s: %v", info.Name, err)
			} else {
				s.pin = max(info.LSN, 1)
				e.pin(s.pin)
			}
		}
		e.slots[info.Name] = s
	}
	return nil
}

func (e *Engine) saveSlot(info SlotInfo) error {
	if err := os.MkdirAll(e.slotDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(info)
	final := filepath.Join(e.slotDir(), info.Name+".json")
	tmp := final + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDirPath(e.slotDir())
}

func syncDirPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// CreateSlot creates a slot whose first change is the first transaction
// committed after this call.
func (e *Engine) CreateSlot(name, plugin string) (SlotInfo, error) {
	if e.ro.Load() {
		return SlotInfo{}, ErrReadOnly
	}
	if !slotNameRe.MatchString(name) {
		return SlotInfo{}, fmt.Errorf("invalid replication slot name %q (lower case letters, digits and _, at most 63)", name)
	}
	e.slotMu.Lock()
	defer e.slotMu.Unlock()
	if _, ok := e.slots[name]; ok {
		return SlotInfo{}, ErrSlotExists
	}
	lsn, pos := e.pinDurable()
	ship, err := e.NewShipper(pos)
	if err != nil {
		e.unpin(lsn)
		return SlotInfo{}, err
	}
	info := SlotInfo{Name: name, Plugin: plugin, Seg: pos.Seg, Off: pos.Off, LSN: lsn, Created: time.Now().UnixNano()}
	if err := e.saveSlot(info); err != nil {
		ship.Close()
		e.unpin(lsn)
		return SlotInfo{}, err
	}
	e.slots[name] = &slot{info: info, ship: ship, pin: lsn}
	b, _ := json.Marshal(info)
	if err := e.cdcMirror("slot:"+name, b); err != nil {
		e.logf("cdc: slot %s not mirrored to the log: %v", name, err)
	}
	return info, nil
}

// DropSlot removes a slot and releases what it kept. Lock order
// everywhere: slot.mu, then slot.adv, then e.slotMu.
func (e *Engine) DropSlot(name string) error {
	e.slotMu.Lock()
	s, ok := e.slots[name]
	e.slotMu.Unlock()
	if !ok {
		return ErrNoSlot
	}
	if !s.mu.TryLock() {
		return ErrSlotBusy
	}
	defer s.mu.Unlock()
	s.adv.Lock()
	defer s.adv.Unlock()
	e.slotMu.Lock()
	if e.slots[name] != s {
		e.slotMu.Unlock()
		return ErrNoSlot
	}
	delete(e.slots, name)
	e.releaseSlot(s)
	e.slotMu.Unlock()
	if err := os.Remove(filepath.Join(e.slotDir(), name+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if e.tableExists(cdcTableName) {
		if err := e.cdcMirror("slot:"+name, nil); err != nil {
			e.logf("cdc: dropping slot %s not mirrored to the log: %v", name, err)
		}
	}
	return syncDirPath(e.slotDir())
}

// releaseSlot drops what s keeps (e.slotMu held).
func (e *Engine) releaseSlot(s *slot) {
	if s.ship != nil {
		s.ship.Close()
		s.ship = nil
	}
	if s.pin != 0 {
		e.unpin(s.pin)
		s.pin = 0
	}
}

// Slots lists the slots by name.
func (e *Engine) Slots() []SlotInfo {
	e.slotMu.Lock()
	defer e.slotMu.Unlock()
	out := make([]SlotInfo, 0, len(e.slots))
	for _, s := range e.slots {
		out = append(out, s.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SlotLag is how many bytes of log a slot holds back (behind the durable point).
func (e *Engine) SlotLag(info SlotInfo) int64 {
	_, dpos, _ := e.DurablePoint()
	return int64(dpos.Seg-info.Seg)*e.log.SegMax() + dpos.Off - info.Off
}

// checkSlots invalidates slots that hold back more than SlotMaxLagBytes of
// log (run before each cleaning round). A consumer still reading an
// invalidated slot fails once its log is reclaimed.
func (e *Engine) checkSlots() {
	if e.opts.SlotMaxLagBytes <= 0 {
		return
	}
	e.slotMu.Lock()
	var lagging []*slot
	for _, s := range e.slots {
		if s.info.Invalid == "" && e.SlotLag(s.info) > e.opts.SlotMaxLagBytes {
			lagging = append(lagging, s)
		}
	}
	e.slotMu.Unlock()
	for _, s := range lagging {
		s.adv.Lock()
		e.slotMu.Lock()
		lag := e.SlotLag(s.info)
		if s.info.Invalid != "" || lag <= e.opts.SlotMaxLagBytes {
			e.slotMu.Unlock()
			s.adv.Unlock()
			continue
		}
		s.info.Invalid = fmt.Sprintf("fell %d MiB behind (limit %d MiB)", lag>>20, e.opts.SlotMaxLagBytes>>20)
		info := s.info
		e.releaseSlot(s)
		e.slotMu.Unlock()
		e.logf("cdc: slot %s invalidated: %s", info.Name, info.Invalid)
		if err := e.saveSlot(info); err != nil {
			e.logf("cdc: slot %s: %v", info.Name, err)
		}
		s.adv.Unlock()
	}
}

// ChangeKind is the kind of a decoded change.
type ChangeKind byte

const (
	ChangeBegin  ChangeKind = 'B'
	ChangeCommit ChangeKind = 'C'
	ChangeInsert ChangeKind = 'I'
	ChangeUpdate ChangeKind = 'U'
	ChangeDelete ChangeKind = 'D'
	// ChangeTruncate: TRUNCATE of Table (a transaction of its own)
	ChangeTruncate ChangeKind = 'T'
)

// Change is one decoded change. A transaction is ChangeBegin, its row
// changes in log order, then ChangeCommit.
type Change struct {
	Kind ChangeKind
	LSN  uint64 // commit LSN of the transaction
	TS   int64  // commit time (unix ns)
	// Pos is where the transaction's first frame (Begin, row changes) or its
	// commit frame (Commit) starts; End (Begin, Commit) is just after the
	// commit frame: where a consumer that has applied the transaction
	// confirms to. CommitPos (Begin) is where the commit frame starts.
	Pos, End, CommitPos wal.Pos
	Table               *schema.Table
	Key                 string
	Row                 schema.Row // Insert, Update: the new row
	// OldRow (Update, Delete of a table with REPLICA IDENTITY FULL): the
	// row before the change; nil if it could not be read (OldUnknown)
	OldRow schema.Row
	// OldUnknown: the replaced version could not be read (reclaimed before
	// a restart lost its relocation): an Update may then have been an
	// insert after a delete, a Delete may delete a key that was absent.
	OldUnknown bool
}

// ChangeOpts bounds a decoding call.
type ChangeOpts struct {
	// UpTo: only transactions whose commit frame ends at or before UpTo
	// (zero: no bound).
	UpTo wal.Pos
	// MaxChanges: stop after the transaction in which this many row
	// changes were reached (0: no bound), as PostgreSQL's upto_nchanges.
	MaxChanges int
	// Consume: advance the slot past the transactions delivered (only if
	// fn never failed).
	Consume bool
	// Wait: wait for new transactions until it fires (nil: stop at the end
	// of the durable log).
	Wait <-chan struct{}
	// CaughtUp, if set, is called when decoding has reached the end of the
	// durable log, before waiting (a streaming consumer flushes there).
	CaughtUp func()
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// SlotChanges decodes the committed transactions after the slot's
// position and calls fn for every change; it returns the number of row
// changes delivered. Transactions are delivered whole: decoding stops
// between transactions (MaxChanges, UpTo, Wait firing, or fn returning
// ErrStopDecoding at a Commit). Any other error from fn ends the call
// without advancing the slot.
func (e *Engine) SlotChanges(name string, o ChangeOpts, fn func(*Change) error) (int, error) {
	e.slotMu.Lock()
	s, ok := e.slots[name]
	e.slotMu.Unlock()
	if !ok {
		return 0, ErrNoSlot
	}
	if !s.mu.TryLock() {
		return 0, ErrSlotBusy
	}
	defer s.mu.Unlock()
	e.slotMu.Lock()
	info, ship := s.info, s.ship
	e.slotMu.Unlock()
	if info.Invalid != "" {
		return 0, fmt.Errorf("%w: %s", ErrSlotInvalid, info.Invalid)
	}
	wait := o.Wait
	if wait == nil {
		wait = closedChan
	}
	pos := info.Pos()
	done, doneLSN := pos, info.LSN // after the last whole transaction delivered
	n := 0
	var chain []*wal.Record
	stop := false
	err := func() error {
		for !stop {
			if o.CaughtUp != nil {
				if _, dpos, _ := e.DurablePoint(); !pos.Less(dpos) {
					o.CaughtUp()
				}
			}
			data, next, _, err := ship.Read(pos, 4<<20, wait)
			if errors.Is(err, ErrIdle) {
				return nil
			}
			if err != nil {
				return err
			}
			for off := 0; off < len(data) && !stop; {
				rp := wal.Pos{Seg: pos.Seg, Off: pos.Off + int64(off)}
				r, err := wal.DecodeRecord(data[off:], rp)
				if err != nil {
					return fmt.Errorf("decode at %d:%d: %w", rp.Seg, rp.Off, err)
				}
				end := wal.Pos{Seg: rp.Seg, Off: rp.Off + int64(r.Size)}
				off += r.Size
				if o.UpTo != (wal.Pos{}) && o.UpTo.Less(end) {
					stop = true
					break
				}
				switch r.Kind {
				case wal.KindTxPart:
					chain = append(chain, cloneRecord(r))
					continue
				case wal.KindTx:
					m, err := e.emitTx(append(chain, r), end, fn)
					chain = nil
					if errors.Is(err, errStopDecoding) {
						stop = true
					} else if err != nil {
						return err
					}
					n += m
					done, doneLSN = end, r.LSN
					if o.MaxChanges > 0 && n >= o.MaxChanges {
						stop = true
					}
				case wal.KindDDL:
					// only TRUNCATE is delivered (as in PostgreSQL)
					m, err := e.emitTruncate(r, end, fn)
					if errors.Is(err, errStopDecoding) {
						stop = true
					} else if err != nil {
						return err
					}
					n += m
					done, doneLSN = end, r.LSN
				default: // Move: nothing to deliver
					if len(chain) == 0 {
						done, doneLSN = end, r.LSN
					}
				}
			}
			pos = next
		}
		return nil
	}()
	if err != nil {
		return n, err
	}
	if o.Consume && done != info.Pos() {
		if err := e.advanceSlot(s, done, doneLSN); err != nil {
			return n, err
		}
	}
	return n, nil
}

// ConfirmSlot advances a slot past the transactions whose commit frame
// ends at or before upto (what its consumer has applied), also while
// another call streams from it; it returns the slot's new position.
func (e *Engine) ConfirmSlot(name string, upto wal.Pos) (wal.Pos, error) {
	e.slotMu.Lock()
	s, ok := e.slots[name]
	var info SlotInfo
	if ok {
		info = s.info
	}
	e.slotMu.Unlock()
	if !ok {
		return wal.Pos{}, ErrNoSlot
	}
	if info.Invalid != "" {
		return wal.Pos{}, fmt.Errorf("%w: %s", ErrSlotInvalid, info.Invalid)
	}
	if _, dpos, _ := e.DurablePoint(); dpos.Less(upto) {
		upto = dpos
	}
	done, doneLSN := info.Pos(), info.LSN
	inChain := false
	for seg := info.Seg; seg <= upto.Seg; seg++ {
		off, limit := int64(0), int64(-1)
		if seg == info.Seg {
			off = info.Off
		}
		if seg == upto.Seg {
			limit = upto.Off
		}
		if limit >= 0 && limit <= off {
			break
		}
		_, _, err := e.log.Scan(seg, off, limit, func(r *wal.Record) error {
			switch {
			case r.Kind == wal.KindTxPart:
				inChain = true
			case r.Kind == wal.KindTx || !inChain:
				inChain = false
				done, doneLSN = wal.Pos{Seg: seg, Off: r.Pos.Off + int64(r.Size)}, r.LSN
			}
			return nil
		})
		if err != nil {
			return info.Pos(), err
		}
	}
	if done == info.Pos() {
		return done, nil
	}
	return done, e.advanceSlot(s, done, doneLSN)
}

// ErrStopDecoding, returned by a SlotChanges callback at a Commit, ends
// decoding after that transaction (it still counts as delivered).
var ErrStopDecoding = errStopDecoding

// advanceSlot persists the slot at pos and moves its pins there (never
// backwards).
func (e *Engine) advanceSlot(s *slot, pos wal.Pos, lsn uint64) error {
	s.adv.Lock()
	defer s.adv.Unlock()
	e.slotMu.Lock()
	info := s.info
	e.slotMu.Unlock()
	if info.Invalid != "" {
		return fmt.Errorf("%w: %s", ErrSlotInvalid, info.Invalid)
	}
	if !info.Pos().Less(pos) {
		return nil
	}
	info.Seg, info.Off, info.LSN = pos.Seg, pos.Off, lsn
	if err := e.saveSlot(info); err != nil {
		return err
	}
	e.slotMu.Lock()
	s.info = info
	mirror := lsn > e.cdcMirrorLSN.Load() && time.Since(s.mirrored) >= time.Second
	if mirror {
		s.mirrored = time.Now()
	}
	if s.ship != nil {
		s.ship.SetPos(pos)
	}
	old := s.pin
	if lsn > old {
		e.pin(lsn)
		s.pin = lsn
	} else {
		old = 0
	}
	e.slotMu.Unlock()
	if old != 0 {
		e.unpin(old)
	}
	if mirror { // for the replicas (failover); a failure here is not the subscriber's
		b, _ := json.Marshal(info)
		if err := e.cdcMirror("slot:"+info.Name, b); err != nil {
			e.logf("cdc: slot %s position not mirrored to the log: %v", info.Name, err)
		}
	}
	return nil
}

// emitTx delivers one transaction (its frames, the last one KindTx).
func (e *Engine) emitTx(frames []*wal.Record, end wal.Pos, fn func(*Change) error) (int, error) {
	last := frames[len(frames)-1]
	// transactions of system tables only (sequences) are not delivered
	user := false
	for _, r := range frames {
		for _, op := range r.Ops {
			if t := e.TableByID(op.Table); t == nil || !t.Schema().System {
				user = true
			}
		}
	}
	if !user {
		return 0, nil
	}
	c := &Change{Kind: ChangeBegin, LSN: last.LSN, TS: last.TS, Pos: frames[0].Pos, End: end, CommitPos: last.Pos}
	if err := fn(c); err != nil {
		return 0, err
	}
	n := 0
	// An earlier operation of this transaction on the same key is what a
	// later one replaces: Prev of every operation names the version before
	// the transaction, so the in-transaction state is tracked here.
	type txKey struct {
		table uint32
		key   string
	}
	type txState struct {
		live bool
		row  schema.Row
	}
	seen := map[txKey]txState{}
	for _, r := range frames {
		for _, op := range r.Ops {
			t := e.TableByID(op.Table)
			if t == nil {
				return n, fmt.Errorf("record lsn=%d references unknown table %d", r.LSN, op.Table)
			}
			if t.Schema().System {
				continue
			}
			c := &Change{LSN: last.LSN, TS: last.TS, Pos: r.Pos, Table: t.Schema(), Key: string(op.Key)}
			live, known := true, true
			tk := txKey{op.Table, string(op.Key)}
			earlier, inTx := seen[tk]
			switch {
			case inTx:
				live = earlier.live
			case op.Prev.Ok:
				live, known = e.prevLive(op.Prev)
			default:
				live = false
			}
			c.OldUnknown = !known
			if live && known && e.ReplicaIdentityFull(t.Schema().Name) {
				if inTx {
					c.OldRow = earlier.row
				} else {
					c.OldRow = e.prevRow(t.Schema(), op.Prev)
				}
				c.OldUnknown = c.OldRow == nil
			}
			if op.Kind == wal.OpDel {
				seen[tk] = txState{}
			}
			if op.Kind == wal.OpDel {
				if !live {
					continue // deleting an absent key changes nothing
				}
				c.Kind = ChangeDelete
			} else {
				row, err := t.Schema().DecodeAt(op.Val, r.LSN)
				if err != nil {
					return n, fmt.Errorf("record lsn=%d: %w", r.LSN, err)
				}
				c.Row = row
				c.Kind = ChangeInsert
				if live {
					c.Kind = ChangeUpdate
				}
				seen[tk] = txState{live: true, row: row}
			}
			if err := fn(c); err != nil {
				return n, err
			}
			n++
		}
	}
	c = &Change{Kind: ChangeCommit, LSN: last.LSN, TS: last.TS, Pos: last.Pos, End: end}
	return n, fn(c)
}

// emitTruncate delivers a TRUNCATE (a DDL record of a table replacing
// another) as a transaction of one change on the replaced table.
func (e *Engine) emitTruncate(r *wal.Record, end wal.Pos, fn func(*Change) error) (int, error) {
	var t schema.Table
	if err := json.Unmarshal(r.DDL, &t); err != nil {
		return 0, err
	}
	if t.Replaces == 0 {
		return 0, nil
	}
	old := e.TableByID(t.Replaces)
	if old == nil {
		return 0, fmt.Errorf("record lsn=%d truncates unknown table %d", r.LSN, t.Replaces)
	}
	if err := fn(&Change{Kind: ChangeBegin, LSN: r.LSN, TS: r.TS, Pos: r.Pos, End: end, CommitPos: r.Pos}); err != nil {
		return 0, err
	}
	if err := fn(&Change{Kind: ChangeTruncate, LSN: r.LSN, TS: r.TS, Pos: r.Pos, Table: old.Schema()}); err != nil {
		return 0, err
	}
	return 1, fn(&Change{Kind: ChangeCommit, LSN: r.LSN, TS: r.TS, Pos: r.Pos, End: end})
}

// prevRow reads the row of the live version at loc (nil if it cannot).
func (e *Engine) prevRow(t *schema.Table, loc wal.Loc) schema.Row {
	defer e.enterRead().Add(-1)
	if e.goneSeg(loc.Seg) {
		to, ok := e.resolve(loc)
		if !ok {
			return nil
		}
		loc = to
	}
	rec, err := e.log.ReadRecord(loc.Seg, int64(loc.Off))
	if err != nil || int(loc.Idx) >= len(rec.Ops) {
		return nil
	}
	lsn := rec.LSN
	if rec.Kind == wal.KindMove {
		lsn = rec.Ops[loc.Idx].OrigLSN
	}
	row, err := t.DecodeAt(rec.Ops[loc.Idx].Val, lsn)
	if err != nil {
		return nil
	}
	return row
}

// prevLive reports whether the version at loc is a live row; known=false
// when it can no longer be read.
func (e *Engine) prevLive(loc wal.Loc) (live, known bool) {
	defer e.enterRead().Add(-1)
	if e.goneSeg(loc.Seg) {
		to, ok := e.resolve(loc)
		if !ok {
			return true, false
		}
		loc = to
	}
	k, err := e.log.OpKindAt(loc.Seg, int64(loc.Off), loc.Idx)
	if err != nil {
		return true, false
	}
	return k != wal.OpDel, true
}
