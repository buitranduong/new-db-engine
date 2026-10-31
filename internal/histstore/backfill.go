package histstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// BackfillResult says what a Backfill did.
type BackfillResult struct {
	Added, Skipped int64 // entries written / entries left out (their bucket is sealed, live or unwanted)
	Buckets        int
	MinTS          int64
}

// Backfill adds versions that happened before the store followed the log
// (read from the log that is still on disk, or from the log of a base
// backup). feed calls add for every entry, in any order. Entries
// go to the open file of their time bucket, which Maintain seals in the
// normal way; a bucket that is already sealed, or is the one being written
// now, is left alone (the store already holds it), as are entries past
// the retention. Call it before the consumer starts following, or from
// Bootstrap. Nothing here can overwrite or lose existing data: it only
// appends frames to open files and adds files for buckets that had none.
func (s *Store) Backfill(feed func(add func(Entry) error) error) (BackfillResult, error) {
	var res BackfillResult
	res.MinTS = 1<<63 - 1
	cutoff := int64(0)
	if s.opt.Retention > 0 {
		cutoff = s.opt.Now().UnixNano() - int64(s.opt.Retention)
	}
	s.mu.RLock()
	covered := func(b int64) bool {
		for _, g := range s.segs {
			if g.first <= b && b <= g.last {
				return true
			}
		}
		return false
	}
	haveActive, abucket := s.haveActive, s.abucket
	s.mu.RUnlock()

	const spillBytes = 64 << 20
	bufs := map[int64][]Entry{}
	sizes := map[int64]int{}
	total := 0
	written := map[int64]bool{}
	var maxLSN uint64
	flushBucket := func(b int64) error {
		es := bufs[b]
		if len(es) == 0 {
			return nil
		}
		f, err := os.OpenFile(filepath.Join(s.opt.Dir, openName(b)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		var frame []byte
		for i := 0; i < len(es); i += 4096 {
			frame = appendFrame(frame[:0], es[i:min(i+4096, len(es))])
			if _, err = f.Write(frame); err != nil {
				break
			}
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		written[b] = true
		total -= sizes[b]
		delete(bufs, b)
		delete(sizes, b)
		return nil
	}
	add := func(e Entry) error {
		if e.TS < cutoff {
			res.Skipped++
			return nil
		}
		b := s.bucketOf(e.TS)
		if covered(b) || haveActive && b >= abucket {
			res.Skipped++
			return nil
		}
		e.Val = append([]byte(nil), e.Val...)
		bufs[b] = append(bufs[b], e)
		n := 40 + len(e.Key) + len(e.Val)
		sizes[b] += n
		total += n
		res.Added++
		res.MinTS = min(res.MinTS, e.TS)
		maxLSN = max(maxLSN, e.LSN)
		if total > spillBytes {
			big, bs := int64(0), 0
			for k, v := range sizes {
				if v > bs {
					big, bs = k, v
				}
			}
			return flushBucket(big)
		}
		return nil
	}
	if err := feed(add); err != nil {
		return res, err
	}
	var keys []int64
	for b := range bufs {
		keys = append(keys, b)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, b := range keys {
		if err := flushBucket(b); err != nil {
			return res, err
		}
	}
	if len(written) == 0 {
		if res.Added == 0 {
			res.MinTS = 0
		}
		return res, nil
	}
	if err := syncDir(s.opt.Dir); err != nil {
		return res, err
	}
	var bs []int64
	for b := range written {
		bs = append(bs, b)
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	res.Buckets = len(bs)
	s.mu.Lock()
	newest := bs[len(bs)-1]
	// the newest bucket continues as the one live entries go to (they are never put in an earlier
	// bucket than s.cur), unless a file is being written already
	asCur := !s.haveActive && (!s.curOK || newest >= s.cur)
	have := map[int64]bool{}
	for _, b := range s.pending {
		have[b] = true
	}
	for _, b := range bs {
		if asCur && b == newest || have[b] {
			continue
		}
		s.pending = append(s.pending, b)
	}
	sort.Slice(s.pending, func(i, j int) bool { return s.pending[i] < s.pending[j] })
	if asCur {
		s.cur, s.curOK = newest, true
	}
	s.mu.Unlock()
	s.amu.Lock()
	s.lastLSN = max(s.lastLSN, maxLSN)
	s.amu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	s.opt.Logf("histstore: backfilled %d versions into %d buckets (%d left out)", res.Added, res.Buckets, res.Skipped)
	return res, nil
}

// String for logs.
func (r BackfillResult) String() string {
	return fmt.Sprintf("added %d skipped %d buckets %d", r.Added, r.Skipped, r.Buckets)
}
