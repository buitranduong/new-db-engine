package histstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrDiskFull is returned by Append while the store's disk is below
// MinFreeBytes or a write failed. Nothing is lost by it: the caller retries
// (Consumer stalls the log follower, which pins the log, for up to
// StallMax, and after that records a Gap and carries on).
var ErrDiskFull = errors.New("histstore: disk full or write failing")

// Health is what an operator needs to see (also exported by /metrics).
type Health struct {
	Broken      bool   // writes are failing; entries are being held back
	Reason      string // last write error
	Since       time.Time
	InGap       bool  // entries are being dropped (disk stayed full past StallMax)
	Dropped     int64 // entries dropped in gaps since start
	Gaps        int   // gaps recorded in cover.json
	FreeBytes   uint64
	MinFree     int64
	Unsynced    int // entries held for rewrite after a failure
	Alerts      int64
	LastAlert   string
	DiskBytes   int64
	SegmentsNum int
}

func statfsFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// freeLocked checks the free space of the store's disk, at most twice a
// second. hmu is not needed: only called under amu or Maintain's lock, and
// the fields it sets are read through Health under hmu.
func (s *Store) freeCheck() error {
	min := s.opt.MinFreeBytes
	if min < 0 {
		return nil
	}
	s.hmu.Lock()
	defer s.hmu.Unlock()
	if time.Since(s.lastFreeAt) < 500*time.Millisecond {
		if s.lastFreeErr != nil {
			return s.lastFreeErr
		}
		return nil
	}
	s.lastFreeAt = time.Now()
	free, err := s.opt.FreeFn(s.opt.Dir)
	if err != nil {
		s.lastFreeErr = nil // cannot tell: do not stop on a statfs failure
		return nil
	}
	s.lastFree = free
	if int64(free) < min {
		s.lastFreeErr = fmt.Errorf("%w: %d bytes free on %s, need %d", ErrDiskFull, free, s.opt.Dir, min)
	} else {
		s.lastFreeErr = nil
	}
	return s.lastFreeErr
}

// alert logs loudly, at most once every 30 s per distinct message prefix.
func (s *Store) alert(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	s.hmu.Lock()
	s.alerts++
	s.lastAlert = msg
	quiet := time.Since(s.lastAlertLog) < 30*time.Second
	if !quiet {
		s.lastAlertLog = time.Now()
	}
	s.hmu.Unlock()
	if !quiet {
		s.opt.Logf("ALERT histstore: %s", msg)
	}
}

// breakLocked records a failed write (amu held). The data in the buffer and
// the unsynced entries are held for a rewrite; the durable cursor is where
// the follower would resume after a restart, so lastLSN goes back to it.
func (s *Store) breakLocked(err error) {
	if s.broken == nil {
		s.hmu.Lock()
		s.brokenSince = time.Now()
		s.hmu.Unlock()
	}
	s.broken = err
	s.lastLSN = s.cursorW.LSN
	s.alert("write failed, history is being held back (the log follower stalls): %v", err)
}

// healLocked retries after a failure (amu held): reopen the open file (this
// cuts a torn tail exactly as after a crash), rewrite the entries that were
// not yet on disk, fsync. Rate limited. A failed heal leaves the store broken.
func (s *Store) healLocked() error {
	if s.broken == nil {
		return nil
	}
	if time.Since(s.lastHeal) < 500*time.Millisecond {
		return fmt.Errorf("%w: %v", ErrDiskFull, s.broken)
	}
	s.lastHeal = time.Now()
	s.lastFreeAt = time.Time{} // look again
	if err := s.freeCheck(); err != nil {
		return err
	}
	if s.af != nil {
		s.af.Close()
		s.af, s.aw = nil, nil
	}
	s.mu.Lock()
	s.haveActive = false
	s.mu.Unlock()
	un := s.unsynced
	s.unsynced = nil
	keep := s.lastLSN
	if len(un) > 0 {
		s.lastLSN = s.cursorW.LSN
		if err := s.appendLocked(un); err != nil {
			s.unsynced, s.lastLSN = un, keep
			return fmt.Errorf("%w: %v", ErrDiskFull, err)
		}
		s.unsynced = un
		s.dirty = true
	}
	if err := s.syncActiveLocked(); err != nil {
		s.unsynced, s.lastLSN = un, keep
		return fmt.Errorf("%w: %v", ErrDiskFull, err)
	}
	s.lastLSN = max(s.lastLSN, keep)
	s.broken = nil
	s.opt.Logf("histstore: writes are working again (%d held entries rewritten)", len(un))
	return nil
}

// Health reports the state for monitoring.
func (s *Store) Health() Health {
	s.amu.Lock()
	broken, un := s.broken, len(s.unsynced)
	s.amu.Unlock()
	c := s.Cover()
	s.hmu.Lock()
	h := Health{Broken: broken != nil, Since: s.brokenSince, InGap: s.inGap, Dropped: s.dropped, Gaps: len(c.Gaps),
		FreeBytes: s.lastFree, MinFree: s.opt.MinFreeBytes, Unsynced: un, Alerts: s.alerts, LastAlert: s.lastAlert}
	s.hmu.Unlock()
	if broken != nil {
		h.Reason = broken.Error()
	}
	s.mu.RLock()
	h.SegmentsNum = len(s.segs)
	s.mu.RUnlock()
	h.DiskBytes = s.DiskBytes()
	return h
}

// EnterGap gives up on the entries that cannot be written (the disk stayed
// full for longer than the caller is willing to stall): they and everything
// not yet on disk are dropped, and the span is recorded in cover.json so
// that reads which depend on it fail with ErrSnapshotTooOld instead of
// returning a wrong answer. es are the entries being appended now.
func (s *Store) EnterGap(es []Entry) {
	from := int64(1<<63 - 1)
	for i := range es {
		from = min(from, es[i].TS)
	}
	s.amu.Lock()
	for i := range s.unsynced {
		from = min(from, s.unsynced[i].TS)
	}
	dropped := int64(len(es) + len(s.unsynced))
	s.unsynced = nil
	s.amu.Unlock()
	s.hmu.Lock()
	already := s.inGap
	s.inGap = true
	s.dropped += dropped
	s.hmu.Unlock()
	if already {
		return
	}
	s.alert("disk stayed full: dropping history from commit time %s on; reads in that span will be refused until it is rebuilt", time.Unix(0, from).UTC().Format(time.RFC3339))
	if err := s.UpdateCover(func(c *Cover) { c.Gaps = append(c.Gaps, Gap{From: from}) }); err != nil {
		s.opt.Logf("histstore: cover.json not written (kept in memory): %v", err)
	}
}

// Drop counts entries dropped while in a gap.
func (s *Store) Drop(n int) {
	s.hmu.Lock()
	s.dropped += int64(n)
	s.hmu.Unlock()
}

// InGap reports whether entries are being dropped.
func (s *Store) InGap() bool {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	return s.inGap
}

// TryResume ends a gap if writing works again; firstTS is the commit time
// of the first entry that will be stored after it.
func (s *Store) TryResume(firstTS int64) bool {
	s.amu.Lock()
	err := s.healLocked()
	if err == nil {
		err = s.freeCheck()
	}
	s.amu.Unlock()
	if err != nil {
		return false
	}
	s.hmu.Lock()
	s.inGap = false
	s.hmu.Unlock()
	s.opt.Logf("histstore: writes are possible again: the gap ends at commit time %s", time.Unix(0, firstTS).UTC().Format(time.RFC3339))
	if err := s.UpdateCover(func(c *Cover) {
		if n := len(c.Gaps); n > 0 && c.Gaps[n-1].To == 0 {
			c.Gaps[n-1].To = firstTS
		}
	}); err != nil {
		s.opt.Logf("histstore: cover.json not written (kept in memory): %v", err)
	}
	return true
}

// cleanTemp removes half-written .tmp and .run files after a failed
// maintenance step (they hold disk space and are never used again).
func (s *Store) cleanTemp() {
	ents, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return
	}
	for _, de := range ents {
		if n := de.Name(); strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, ".run") {
			os.Remove(filepath.Join(s.opt.Dir, n))
		}
	}
}
