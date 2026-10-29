package core

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"stask/internal/wal"
)

// Consumer is a derived representation fed one-way from the log (I4).
type Consumer interface {
	Name() string
	// Bootstrap loads the state visible at snapshot snap (via ScanSnapshot).
	Bootstrap(e *Engine, snap uint64) error
	// Apply decodes one durable record. It must not retain rec's memory.
	Apply(e *Engine, rec *wal.Record)
	// Flush makes everything applied so far visible to queries.
	Flush(lsn uint64)
}

// Persister is a Consumer whose state can be saved and loaded, so a start
// replays only the log after the saved LSN instead of rebuilding the view
// from a snapshot of the data.
type Persister interface {
	// Peek reads the saved state's header: the LSN it reflects and the log
	// position right after that LSN. ok is false if there is none or it is
	// unreadable.
	Peek(e *Engine) (lsn uint64, pos wal.Pos, ok bool)
	// Load replaces the state with the saved one; on error the state must be
	// left empty (the follower then bootstraps).
	Load(e *Engine) error
	// Save writes the state, which reflects the log up to lsn, crash-safely.
	// It runs on the follower's goroutine, so the state does not change.
	Save(e *Engine, lsn uint64, pos wal.Pos) error
}

// Follower drives a Consumer and publishes how far it has applied.
type Follower struct {
	c       Consumer
	e       *Engine
	applied atomic.Uint64
	ready   atomic.Bool
	notify  atomic.Pointer[chan struct{}]
	bootNs  atomic.Int64
	pos     atomic.Pointer[wal.Pos] // next log position to read (pins segments against GC)

	restored  atomic.Bool   // the state was loaded from a saved view
	savedLSN  atomic.Uint64 // LSN of the last save (or of the load)
	saveReq   atomic.Bool
	saveKick  chan struct{}
	saveCount atomic.Int64
}

// Restored reports whether the consumer started from a saved view.
func (f *Follower) Restored() bool { return f.restored.Load() }

// SavedLSN is the LSN of the newest saved view this follower wrote or loaded.
func (f *Follower) SavedLSN() uint64 { return f.savedLSN.Load() }

// RequestSave asks for a save at the next quiet point.
func (f *Follower) RequestSave() {
	f.saveReq.Store(true)
	select {
	case f.saveKick <- struct{}{}:
	default:
	}
}

// tryRestore loads the consumer's saved state if it is consistent with the
// log. It returns the LSN and log position to continue from.
func (f *Follower) tryRestore() (uint64, wal.Pos, bool) {
	p, ok := f.c.(Persister)
	o := &f.e.opts
	if !ok || o.NoViewFile || o.RestoreArchive != "" || o.RecoveryTargetLSN != 0 || o.RecoveryTargetTS != 0 {
		return 0, wal.Pos{}, false
	}
	lsn, pos, ok := p.Peek(f.e)
	if !ok {
		return 0, wal.Pos{}, false
	}
	dl, dpos, _ := f.e.DurablePoint()
	if lsn > dl || dpos.Less(pos) || !slices.Contains(f.e.log.Segments(), pos.Seg) {
		f.e.logf("%s: saved view at lsn=%d does not match the log (durable %d): rebuilding", f.c.Name(), lsn, dl)
		return 0, wal.Pos{}, false
	}
	// keep the segments from pos on while the view loads
	f.pos.Store(&pos)
	if pos.Less(dpos) {
		// the first record there must follow lsn directly (or be one of its
		// frames): the log is the one the view was saved from
		bad := false
		stop := errors.New("stop")
		limit := int64(-1)
		if pos.Seg == dpos.Seg {
			limit = dpos.Off
		}
		_, _, err := f.e.log.Scan(pos.Seg, pos.Off, limit, func(r *wal.Record) error {
			bad = r.LSN > lsn+1
			return stop
		})
		if bad || (err != nil && err != stop) {
			f.e.logf("%s: saved view at lsn=%d does not match the log: rebuilding", f.c.Name(), lsn)
			return 0, wal.Pos{}, false
		}
	}
	if err := p.Load(f.e); err != nil {
		f.e.logf("%s: saved view not used: %v: rebuilding", f.c.Name(), err)
		return 0, wal.Pos{}, false
	}
	return lsn, pos, true
}

func (f *Follower) save(lsn uint64, pos wal.Pos, why string) {
	p, ok := f.c.(Persister)
	if !ok || f.e.opts.NoViewFile {
		return
	}
	if lsn <= f.savedLSN.Load() && f.saveCount.Load() > 0 {
		return
	}
	t0 := time.Now()
	if err := p.Save(f.e, lsn, pos); err != nil {
		f.e.logf("%s: saving the view failed: %v", f.c.Name(), err)
		return
	}
	f.savedLSN.Store(lsn)
	f.saveCount.Add(1)
	f.e.logf("%s: view saved at lsn=%d (%s) in %v", f.c.Name(), lsn, why, time.Since(t0))
}

func (f *Follower) saveEvery() time.Duration {
	switch d := f.e.opts.ViewSaveEvery; {
	case d == 0:
		return 10 * time.Minute
	case d < 0:
		return 1<<63 - 1
	default:
		return d
	}
}

func (f *Follower) position() (wal.Pos, bool) {
	if p := f.pos.Load(); p != nil {
		return *p, true
	}
	return wal.Pos{}, false
}

func (e *Engine) Follow(c Consumer) *Follower {
	f := &Follower{c: c, e: e, saveKick: make(chan struct{}, 1)}
	ch := make(chan struct{})
	f.notify.Store(&ch)
	e.gc.rmu.Lock()
	e.gc.readers = append(e.gc.readers, f)
	e.gc.rmu.Unlock()
	e.wg.Add(1)
	go f.run()
	return f
}

func (f *Follower) BootTime() time.Duration { return time.Duration(f.bootNs.Load()) }
func (f *Follower) Name() string            { return f.c.Name() }
func (f *Follower) Applied() uint64         { return f.applied.Load() }
func (f *Follower) Ready() bool             { return f.ready.Load() }
func (f *Follower) Lag() time.Duration {
	if !f.ready.Load() {
		return time.Hour
	}
	return f.e.LagSince(f.applied.Load())
}

// WaitApplied blocks until the consumer has applied lsn or timeout passes.
func (f *Follower) WaitApplied(lsn uint64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		ch := *f.notify.Load()
		if f.ready.Load() && f.applied.Load() >= lsn {
			return true
		}
		d := time.Until(deadline)
		if d <= 0 {
			return false
		}
		select {
		case <-ch:
		case <-time.After(d):
			return f.ready.Load() && f.applied.Load() >= lsn
		}
	}
}

func (f *Follower) advance(lsn uint64) {
	f.c.Flush(lsn)
	f.applied.Store(lsn)
	ch := make(chan struct{})
	old := f.notify.Swap(&ch)
	close(*old)
}

func (f *Follower) run() {
	defer f.e.wg.Done()
	t0 := time.Now()
	snap, pos, restored := f.tryRestore()
	if restored {
		f.restored.Store(true)
		f.savedLSN.Store(snap)
		f.saveCount.Store(1)
		f.bootNs.Store(int64(time.Since(t0)))
		f.ready.Store(true)
		f.advance(snap)
		f.e.logf("%s: loaded the saved view at lsn=%d in %v", f.c.Name(), snap, f.BootTime())
	} else {
		snap, pos = f.e.pinDurable()
		f.pos.Store(&pos)
		err := f.c.Bootstrap(f.e, snap)
		f.e.unpin(snap)
		if err != nil {
			f.e.logf("%s: bootstrap failed: %v", f.c.Name(), err)
			return
		}
		f.bootNs.Store(int64(time.Since(t0)))
		f.ready.Store(true)
		f.advance(snap)
		f.e.logf("%s: bootstrapped at lsn=%d in %v", f.c.Name(), snap, f.BootTime())
	}
	last := snap
	lastSave := time.Now()
	clean := true // pos and last are consistent
	saveDue := func() bool {
		return f.saveReq.Swap(false) || time.Since(lastSave) >= f.saveEvery()
	}
	closing := func() {
		if clean && !f.e.opts.NoViewSaveOnClose {
			f.save(last, pos, "close")
		}
	}
	ops, lastAdv := 0, time.Now()
	for {
		target, tpos, ch := f.e.DurablePoint()
		if target <= last {
			var tick <-chan time.Time
			if d := f.saveEvery(); d < 1<<62 {
				tick = time.After(max(time.Second, d-time.Since(lastSave)))
			}
			select {
			case <-ch:
				continue
			case <-f.saveKick:
				f.saveReq.Store(false)
				if clean {
					f.save(last, pos, "requested")
				}
				lastSave = time.Now()
				continue
			case <-tick:
				if clean && saveDue() {
					f.save(last, pos, "periodic")
					lastSave = time.Now()
				}
				continue
			case <-f.e.stop:
				closing()
				return
			}
		}
		clean = true
		// scan from pos to tpos, possibly across segments
		for pos.Less(tpos) {
			limit := int64(-1)
			if pos.Seg == tpos.Seg {
				limit = tpos.Off
			}
			end, _, err := f.e.log.Scan(pos.Seg, pos.Off, limit, func(r *wal.Record) error {
				if r.LSN > last {
					if r.Kind != wal.KindMove { // relocation changes no data
						f.c.Apply(f.e, r)
					}
					if r.Kind == wal.KindTxPart {
						return nil // more frames of this LSN follow (all below the durable point)
					}
					last = r.LSN
					ops += len(r.Ops)
					// Publish progress every advanceOps operations or
					// advanceEvery, whichever comes first: a bulk load has
					// hundreds of operations per record, and readers waiting
					// for the view judge it stalled when applied stops moving.
					if ops >= advanceOps || time.Since(lastAdv) >= advanceEvery {
						f.advance(last)
						ops, lastAdv = 0, time.Now()
					}
				}
				return nil
			})
			if err == nil && end == pos.Off && pos.Seg == tpos.Seg {
				// no progress below the durable point: unreadable record
				err = fmt.Errorf("unreadable record at %d:%d", pos.Seg, pos.Off)
			}
			if err != nil {
				f.e.logf("%s: log read failed: %v", f.c.Name(), err)
				clean = false
				select {
				case <-f.e.stop:
					return
				case <-time.After(time.Second):
				}
				break
			}
			if pos.Seg < tpos.Seg {
				pos = wal.Pos{Seg: pos.Seg + 1}
			} else {
				pos.Off = end
			}
			p := pos
			f.pos.Store(&p)
		}
		f.advance(last)
		ops, lastAdv = 0, time.Now()
		if clean && saveDue() {
			f.save(last, pos, "periodic")
			lastSave = time.Now()
		}
		select {
		case <-f.e.stop:
			closing()
			return
		default:
		}
	}
}

const (
	advanceOps   = 16384
	advanceEvery = 20 * time.Millisecond
)
