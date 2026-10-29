package core

import (
	"fmt"
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

// Follower drives a Consumer and publishes how far it has applied.
type Follower struct {
	c       Consumer
	e       *Engine
	applied atomic.Uint64
	ready   atomic.Bool
	notify  atomic.Pointer[chan struct{}]
	bootNs  atomic.Int64
	pos     atomic.Pointer[wal.Pos] // next log position to read (pins segments against GC)
}

func (f *Follower) position() (wal.Pos, bool) {
	if p := f.pos.Load(); p != nil {
		return *p, true
	}
	return wal.Pos{}, false
}

func (e *Engine) Follow(c Consumer) *Follower {
	f := &Follower{c: c, e: e}
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
	snap, pos := f.e.pinDurable()
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
	last := snap
	ops, lastAdv := 0, time.Now()
	for {
		target, tpos, ch := f.e.DurablePoint()
		if target <= last {
			select {
			case <-ch:
				continue
			case <-f.e.stop:
				return
			}
		}
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
		select {
		case <-f.e.stop:
			return
		default:
		}
	}
}

const (
	advanceOps   = 16384
	advanceEvery = 20 * time.Millisecond
)
