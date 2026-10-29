package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/wal"
)

// Scrub: a background read of every log segment and checkpoint that checks
// the CRC of each record (segments) or chunk (checkpoints), so that a bad
// sector is found while the replicas and backups that could repair it still
// exist, and not at the next recovery. Like PostgreSQL's pg_checksums or
// pg_amcheck, it reads everything it covers; it is rate limited so that it
// competes little with the foreground work, and it is started on demand
// (POST /v1/admin/scrub, docs/05 §19). It only reads.

// ScrubReport is the state of the last (or running) scrub.
type ScrubReport struct {
	Running     bool      `json:"running"`
	Started     time.Time `json:"started"`
	Finished    time.Time `json:"finished,omitempty"`
	Segments    int       `json:"segments"`    // log segments checked
	Records     int64     `json:"records"`     // log records whose CRC matched
	Checkpoints int       `json:"checkpoints"` // checkpoint files checked
	Bytes       int64     `json:"bytes"`       // bytes read
	Skipped     int       `json:"skipped"`     // segments removed by log reclamation while the scrub ran
	Errors      []string  `json:"errors"`      // one line per corruption found
	Cancelled   bool      `json:"cancelled,omitempty"`
}

// OK is true when the scrub finished and found nothing wrong.
func (r ScrubReport) OK() bool { return !r.Running && !r.Cancelled && len(r.Errors) == 0 }

var scrubCRCErrors atomic.Int64 // corruptions found since start, for /metrics

// ScrubErrorsTotal is the number of corruptions scrubs have found.
func ScrubErrorsTotal() int64 { return scrubCRCErrors.Load() }

type scrubState struct {
	mu   sync.Mutex
	last ScrubReport
	stop chan struct{}
}

var scrubs sync.Map // *Engine -> *scrubState

func scrubOf(e *Engine) *scrubState {
	v, _ := scrubs.LoadOrStore(e, &scrubState{})
	return v.(*scrubState)
}

// ScrubStatus returns the running or last scrub.
func (e *Engine) ScrubStatus() ScrubReport {
	st := scrubOf(e)
	st.mu.Lock()
	defer st.mu.Unlock()
	r := st.last
	r.Errors = append([]string(nil), r.Errors...)
	return r
}

// StartScrub starts a scrub in the background, reading at most rate bytes
// per second (<= 0: unthrottled). It returns false if one is running.
func (e *Engine) StartScrub(rate int64) bool {
	st := scrubOf(e)
	st.mu.Lock()
	if st.last.Running {
		st.mu.Unlock()
		return false
	}
	st.last = ScrubReport{Running: true, Started: time.Now()}
	st.stop = make(chan struct{})
	stop := st.stop
	st.mu.Unlock()
	go func() {
		sc := &scrubber{e: e, st: st, rate: rate, stop: stop, t0: time.Now()}
		sc.run()
		st.mu.Lock()
		st.last.Running = false
		st.last.Finished = time.Now()
		st.mu.Unlock()
		if n := len(sc.errs()); n > 0 {
			if e.logf != nil {
				e.logf("scrub: %d corruption(s) found: %s", n, strings.Join(sc.errs(), "; "))
			}
		}
	}()
	return true
}

// StopScrub cancels a running scrub.
func (e *Engine) StopScrub() {
	st := scrubOf(e)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last.Running && st.stop != nil {
		select {
		case <-st.stop:
		default:
			close(st.stop)
		}
	}
}

type scrubber struct {
	e     *Engine
	st    *scrubState
	rate  int64
	stop  chan struct{}
	t0    time.Time
	bytes int64
}

func (sc *scrubber) errs() []string {
	sc.st.mu.Lock()
	defer sc.st.mu.Unlock()
	return sc.st.last.Errors
}

func (sc *scrubber) update(fn func(r *ScrubReport)) {
	sc.st.mu.Lock()
	fn(&sc.st.last)
	sc.st.mu.Unlock()
}

func (sc *scrubber) bad(format string, a ...any) {
	scrubCRCErrors.Add(1)
	msg := fmt.Sprintf(format, a...)
	sc.update(func(r *ScrubReport) { r.Errors = append(r.Errors, msg) })
}

// pace accounts n bytes read and sleeps to hold the rate; false if cancelled.
func (sc *scrubber) pace(n int64) bool {
	sc.bytes += n
	select {
	case <-sc.stop:
		return false
	default:
	}
	if sc.rate > 0 {
		want := time.Duration(float64(sc.bytes) / float64(sc.rate) * float64(time.Second))
		if d := want - time.Since(sc.t0); d > 5*time.Millisecond {
			select {
			case <-time.After(d):
			case <-sc.stop:
				return false
			}
		}
	}
	return true
}

func (sc *scrubber) run() {
	cancelled := !sc.checkpoints() || !sc.segments()
	sc.update(func(r *ScrubReport) { r.Cancelled = cancelled })
}

// checkpoints checks every checkpoint file in the data directory.
func (sc *scrubber) checkpoints() bool {
	dir := sc.e.ckptDir()
	ents, _ := os.ReadDir(dir)
	for _, en := range ents {
		n := en.Name()
		if !strings.HasPrefix(n, "ckpt-") || !strings.HasSuffix(n, ".ckpt") {
			continue
		}
		path := filepath.Join(dir, n)
		fi, err := os.Stat(path)
		if err != nil {
			continue // replaced by a newer checkpoint meanwhile
		}
		if !sc.pace(fi.Size()) {
			return false
		}
		// ReadCheckpoint verifies the header and the CRC of every chunk
		if _, err := ReadCheckpoint(path, func(CheckpointEntry) error { return nil }); err != nil {
			if _, serr := os.Stat(path); serr != nil {
				continue // deleted while being read
			}
			sc.bad("checkpoint %s: %v", n, err)
		}
		sc.update(func(r *ScrubReport) { r.Checkpoints++; r.Bytes += fi.Size() })
	}
	return true
}

// segments checks the CRC of every record of every log segment, and that
// what follows the last record of a sealed segment is all zeros (a damaged
// length field that reads as the end-of-log marker would otherwise hide
// everything after it).
func (sc *scrubber) segments() bool {
	l := sc.e.Log()
	for _, seg := range l.Segments() {
		wp := l.WritePos()
		if seg > wp.Seg {
			continue // spare, not yet written
		}
		limit := int64(-1)
		active := seg == wp.Seg
		if active {
			limit = wp.Off
		}
		var recs, read int64
		stopped := false
		end, torn, err := l.Scan(seg, 0, limit, func(r *wal.Record) error {
			recs++
			read += int64(r.Size)
			if read >= 1<<20 {
				if !sc.pace(read) {
					stopped = true
					return errScrubStop
				}
				read = 0
			}
			return nil
		})
		if stopped {
			return false
		}
		sc.pace(read)
		switch {
		case err != nil:
			if l.Removed(seg) || os.IsNotExist(err) || err == wal.ErrRemoved {
				sc.update(func(r *ScrubReport) { r.Skipped++ })
				continue
			}
			sc.bad("segment %d: %v", seg, err)
		case torn:
			sc.bad("segment %d: bad record (CRC or framing) at offset %d, after %d good records", seg, end, recs)
		case !active:
			if off, ok := sc.zeroTail(seg, end); !ok {
				sc.bad("segment %d: data after the last record, at offset %d", seg, off)
			}
		}
		sc.update(func(r *ScrubReport) { r.Segments++; r.Records += recs; r.Bytes += end })
	}
	return true
}

var errScrubStop = fmt.Errorf("scrub stopped")

// zeroTail reads seg from off to its end; ok is false (and off the
// position) if a non-zero byte is found. A segment shorter than SegMax is
// not an error.
func (sc *scrubber) zeroTail(seg uint32, off int64) (int64, bool) {
	l := sc.e.Log()
	buf := make([]byte, 1<<20)
	for off < l.SegMax() {
		n, _ := l.ReadAt(seg, off, buf[:min(int64(len(buf)), l.SegMax()-off)])
		if n <= 0 {
			return off, true
		}
		for i := 0; i < n; i++ {
			if buf[i] != 0 {
				return off + int64(i), false
			}
		}
		off += int64(n)
		if !sc.pace(int64(n)) {
			return off, true
		}
	}
	return off, true
}
