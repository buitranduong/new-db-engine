package core

import (
	"syscall"
)

// Writes refused for lack of disk space (finding F7). When the commit loop
// cannot allocate the next log segment it fails the commit with a clean
// error and the engine stays up (reads keep working). That state was
// invisible to /v1/health. NoteSegmentAllocFailure records it;
// WritesBlocked reports it while the data directory still has less free
// space than one segment, and forgets it once space is back (nothing else
// would clear it on a node that gets no writes).

// DiskFreeFunc returns the free bytes of the file system holding dir. A
// variable so that tests can stand in for a full disk.
var DiskFreeFunc = func(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// NoteSegmentAllocFailure records that the next log segment could not be
// allocated.
func (e *Engine) NoteSegmentAllocFailure(err error) {
	s := "writes are refused: " + err.Error()
	e.allocFail.Store(&s)
}

// WritesBlocked is "" when writes are accepted, else why they are refused
// because the log has no room.
func (e *Engine) WritesBlocked() string {
	p := e.allocFail.Load()
	if p == nil {
		return ""
	}
	if free, err := DiskFreeFunc(e.log.Dir()); err == nil && free >= e.log.SegMax() {
		e.allocFail.CompareAndSwap(p, nil)
		return ""
	}
	return *p
}
