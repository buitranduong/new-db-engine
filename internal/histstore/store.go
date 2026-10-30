package histstore

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configure a Store. The zero value of every field but Dir and
// Retention takes the default noted.
type Options struct {
	Dir       string
	Retention time.Duration // how long a version stays readable; whole segments are dropped, so up to one merged group longer
	Bucket    time.Duration // time span of one base segment (default 1h)
	// MergeFactor is how many consecutive buckets are merged into one
	// segment once the group is complete (default 24: hourly segments become
	// daily ones). Expiry works on the merged segment, so a version is kept
	// up to Retention + Bucket*MergeFactor at most.
	MergeFactor int
	BlockBytes  int           // raw bytes per block before compression (default 32 KiB)
	SortMem     int64         // memory budget of the sort that seals a bucket (default 256 MiB)
	SyncEvery   time.Duration // fsync interval of the open file (default 1s)
	Interval    time.Duration // maintenance (seal, merge, expire) interval (default 5s)
	Manual      bool          // no background goroutine: the caller runs Maintain (tests)
	Now         func() time.Time
	Logf        func(string, ...any)
}

func (o *Options) fill() {
	if o.Bucket <= 0 {
		o.Bucket = time.Hour
	}
	if o.MergeFactor <= 0 {
		o.MergeFactor = 24
	}
	if o.BlockBytes <= 0 {
		o.BlockBytes = 32 << 10
	}
	if o.SortMem <= 0 {
		o.SortMem = 256 << 20
	}
	if o.SyncEvery <= 0 {
		o.SyncEvery = time.Second
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
}

// Cursor is the position in the source log up to which the store is
// durable; its meaning belongs to the caller (the consumer stores the LSN
// and the log position of the last record it applied).
type Cursor struct {
	LSN uint64 `json:"lsn"`
	Seg uint32 `json:"seg"`
	Off int64  `json:"off"`
}

// Store is the history store. Safe for concurrent use.
type Store struct {
	opt Options

	mu         sync.RWMutex // segs, pending, cur, active identity
	segs       []*segment   // by first bucket
	pending    []int64      // closed open files waiting to be sealed, ascending
	cur        int64        // bucket the next entry goes to
	curOK      bool
	haveActive bool // an active open file exists (under mu)

	amu     sync.Mutex // the active open file (writes, flush, fsync, scans)
	af      *os.File
	aw      *bufio.Writer
	abucket int64
	dirty   bool // written since the last fsync

	lastLSN uint64
	cursor  Cursor
	cursorW Cursor // last cursor persisted

	maint   sync.Mutex // one Maintain at a time
	kick    chan struct{}
	stop    chan struct{}
	wg      sync.WaitGroup
	closed  bool
	Stats   Stats
	statsMu sync.Mutex
}

// Stats counts maintenance work (for tests and docs/19 measurements).
type Stats struct {
	Sealed, Merged, Expired int
	SealedBytes             int64
	SealedEntries           int64
}

func (s *Store) bucketOf(ts int64) int64 {
	b := int64(s.opt.Bucket)
	q := ts / b
	if ts%b < 0 {
		q--
	}
	return q
}

// bucketStart/End in unix ns.
func (s *Store) bucketStart(b int64) int64 { return b * int64(s.opt.Bucket) }
func (s *Store) bucketEnd(b int64) int64   { return (b + 1) * int64(s.opt.Bucket) }

func openName(b int64) string { return fmt.Sprintf("open-%d.log", b) }
func segName(level int, first, last int64) string {
	return fmt.Sprintf("seg-%d-%d-%d.hseg", level, first, last)
}

func parseSegName(n string) (level int, first, last int64, ok bool) {
	if !strings.HasPrefix(n, "seg-") || !strings.HasSuffix(n, ".hseg") {
		return
	}
	p := strings.Split(strings.TrimSuffix(strings.TrimPrefix(n, "seg-"), ".hseg"), "-")
	// bucket ids can be negative only for times before 1970; they are not supported
	if len(p) != 3 {
		return
	}
	l, e1 := strconv.Atoi(p[0])
	f, e2 := strconv.ParseInt(p[1], 10, 64)
	la, e3 := strconv.ParseInt(p[2], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return
	}
	return l, f, la, true
}

func parseOpenName(n string) (int64, bool) {
	if !strings.HasPrefix(n, "open-") || !strings.HasSuffix(n, ".log") {
		return 0, false
	}
	b, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(n, "open-"), ".log"), 10, 64)
	return b, err == nil
}

// Open opens (and recovers) the store in o.Dir. Recovery: leftover temporary
// files are removed, a sealed segment wins over the open file it was built
// from, segments covered by a merged one are dropped, and the torn tail of
// the newest open file is cut.
func Open(o Options) (*Store, error) {
	o.fill()
	if o.Dir == "" {
		return nil, fmt.Errorf("histstore: no directory")
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{opt: o, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	ents, err := os.ReadDir(o.Dir)
	if err != nil {
		return nil, err
	}
	var opens []int64
	type sg struct {
		name        string
		level       int
		first, last int64
	}
	var sgs []sg
	for _, de := range ents {
		n := de.Name()
		switch {
		case strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, ".run"):
			os.Remove(filepath.Join(o.Dir, n))
		case strings.HasPrefix(n, "seg-"):
			if l, f, la, ok := parseSegName(n); ok {
				sgs = append(sgs, sg{n, l, f, la})
			}
		case strings.HasPrefix(n, "open-"):
			if b, ok := parseOpenName(n); ok {
				opens = append(opens, b)
			}
		}
	}
	// drop segments covered by a wider one (a merge that crashed before deleting its inputs)
	for _, a := range sgs {
		covered := false
		for _, b := range sgs {
			if b.name != a.name && b.first <= a.first && a.last <= b.last && (b.last-b.first > a.last-a.first) {
				covered = true
				break
			}
		}
		if covered {
			os.Remove(filepath.Join(o.Dir, a.name))
			continue
		}
		sgm, err := openSegment(filepath.Join(o.Dir, a.name), a.level, a.first, a.last)
		if err != nil {
			s.closeSegs()
			return nil, err
		}
		s.segs = append(s.segs, sgm)
		if sgm.maxLSN > s.lastLSN {
			s.lastLSN = sgm.maxLSN
		}
	}
	sort.Slice(s.segs, func(i, j int) bool { return s.segs[i].first < s.segs[j].first })
	maxSeg := int64(-1)
	for _, g := range s.segs {
		maxSeg = max(maxSeg, g.last)
	}
	sort.Slice(opens, func(i, j int) bool { return opens[i] < opens[j] })
	var live []int64
	for _, b := range opens {
		if b <= maxSeg { // sealed already; the open file is what the seal was built from
			os.Remove(filepath.Join(o.Dir, openName(b)))
			continue
		}
		live = append(live, b)
	}
	// the newest open file continues; the older ones are sealed by Maintain
	if len(live) > 0 {
		last := live[len(live)-1]
		s.pending = append(s.pending, live[:len(live)-1]...)
		s.cur, s.curOK = last, true
		if err := s.openActive(last); err != nil {
			s.closeSegs()
			return nil, err
		}
	} else if maxSeg >= 0 {
		s.cur, s.curOK = maxSeg+1, true
	}
	for _, b := range live {
		_, err := scanOpen(filepath.Join(o.Dir, openName(b)), func(es []Entry) {
			for i := range es {
				if es[i].LSN > s.lastLSN {
					s.lastLSN = es[i].LSN
				}
			}
		})
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	if c, ok := s.loadState(); ok {
		s.cursor, s.cursorW = c, c
	}
	if !o.Manual {
		s.wg.Add(1)
		go s.loop()
	}
	return s, nil
}

func (s *Store) closeSegs() {
	for _, g := range s.segs {
		g.close()
	}
}

// openActive opens bucket b's open file for appending, cutting a torn tail.
func (s *Store) openActive(b int64) error {
	p := filepath.Join(s.opt.Dir, openName(b))
	good, err := scanOpen(p, func([]Entry) {})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err == nil && st.Size() > good {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return err
		}
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	s.af, s.aw = f, bufio.NewWriterSize(f, 256<<10)
	s.mu.Lock()
	s.abucket = b
	s.haveActive = true
	s.mu.Unlock()
	return nil
}

// scanOpen streams the intact frames of an open file and returns the length
// of the intact prefix.
func scanOpen(path string, fn func([]Entry)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var off int64
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return off, nil // EOF or torn header
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n == 0 || n > 1<<28 {
			return off, nil
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return off, nil
		}
		if crc32.Checksum(p, crcTab) != binary.LittleEndian.Uint32(hdr[4:]) {
			return off, nil
		}
		es, ok := decodeFrame(p)
		if !ok {
			return off, nil
		}
		fn(es)
		off += 8 + int64(n)
	}
}

// ---- state ----

func (s *Store) statePath() string { return filepath.Join(s.opt.Dir, "state.json") }

func (s *Store) loadState() (Cursor, bool) {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return Cursor{}, false
	}
	var c Cursor
	if json.Unmarshal(b, &c) != nil {
		return Cursor{}, false
	}
	return c, true
}

// Saved is the persisted cursor, if any.
func (s *Store) Saved() (Cursor, bool) { return s.loadState() }

func (s *Store) writeState(c Cursor) error {
	b, _ := json.Marshal(c)
	tmp := s.statePath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, s.statePath()); err != nil {
		return err
	}
	return syncDir(s.opt.Dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ---- write path ----

// LastLSN is the highest LSN held (any entry).
func (s *Store) LastLSN() uint64 {
	s.amu.Lock()
	defer s.amu.Unlock()
	return s.lastLSN
}

// Append adds entries (in log order). Entries go to the bucket of their TS,
// but never to an earlier bucket than the current one, so a bucket is closed
// for good once a later one has started. Entries at or below an LSN already
// held are still written when equal to the last LSN (a frame of a split
// transaction may be replayed) and skipped when older.
func (s *Store) Append(es []Entry) error {
	s.amu.Lock()
	defer s.amu.Unlock()
	floor := s.lastLSN
	var batch []Entry
	var frame []byte
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		frame = appendFrame(frame[:0], batch)
		batch = batch[:0]
		if _, err := s.aw.Write(frame); err != nil {
			return err
		}
		s.dirty = true
		return nil
	}
	for i := range es {
		e := &es[i]
		if e.LSN < floor {
			continue
		}
		b := s.bucketOf(e.TS)
		s.mu.RLock()
		if s.curOK && s.cur > b {
			b = s.cur
		}
		s.mu.RUnlock()
		if s.af == nil || s.abucket != b {
			if err := flush(); err != nil {
				return err
			}
			if err := s.rotateLocked(b); err != nil {
				return err
			}
		}
		batch = append(batch, *e)
		if e.LSN > s.lastLSN {
			s.lastLSN = e.LSN
		}
	}
	return flush()
}

// rotateLocked closes the active file (making it sealable) and starts the
// file of bucket b. amu is held.
func (s *Store) rotateLocked(b int64) error {
	if s.af != nil {
		if err := s.syncActiveLocked(); err != nil {
			return err
		}
		s.af.Close()
		s.af, s.aw = nil, nil
		s.mu.Lock()
		s.haveActive = false
		s.pending = append(s.pending, s.abucket)
		s.mu.Unlock()
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	if err := s.openActive(b); err != nil {
		return err
	}
	s.mu.Lock()
	s.cur, s.curOK = b, true
	s.mu.Unlock()
	return syncDir(s.opt.Dir)
}

func (s *Store) syncActiveLocked() error {
	if s.af == nil || !s.dirty {
		return nil
	}
	if err := s.aw.Flush(); err != nil {
		return err
	}
	if err := s.af.Sync(); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Flush pushes buffered writes to the OS (readers see them); no fsync.
func (s *Store) Flush() error {
	s.amu.Lock()
	defer s.amu.Unlock()
	if s.aw != nil {
		return s.aw.Flush()
	}
	return nil
}

// SetCursor records the source position everything appended so far
// corresponds to. It becomes durable at the next Sync.
func (s *Store) SetCursor(c Cursor) {
	s.amu.Lock()
	s.cursor = c
	s.amu.Unlock()
}

// Sync makes everything appended durable, then persists the cursor: the
// state file never claims more than the files hold.
func (s *Store) Sync() error {
	s.amu.Lock()
	err := s.syncActiveLocked()
	c := s.cursor
	s.amu.Unlock()
	if err != nil {
		return err
	}
	if c != s.cursorW && c.LSN > 0 {
		if err := s.writeState(c); err != nil {
			return err
		}
		s.cursorW = c
	}
	return nil
}

func (s *Store) loop() {
	defer s.wg.Done()
	syncT := time.NewTicker(s.opt.SyncEvery)
	defer syncT.Stop()
	mT := time.NewTicker(s.opt.Interval)
	defer mT.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-syncT.C:
			if err := s.Sync(); err != nil {
				s.opt.Logf("histstore: sync: %v", err)
			}
		case <-s.kick:
			if err := s.Maintain(); err != nil {
				s.opt.Logf("histstore: maintain: %v", err)
			}
		case <-mT.C:
			if err := s.Maintain(); err != nil {
				s.opt.Logf("histstore: maintain: %v", err)
			}
		}
	}
}

// Close syncs and closes the store.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	close(s.stop)
	s.wg.Wait()
	err := s.Sync()
	s.amu.Lock()
	if s.af != nil {
		s.af.Close()
		s.af = nil
	}
	s.amu.Unlock()
	s.mu.Lock()
	s.closeSegs()
	s.segs = nil
	s.mu.Unlock()
	return err
}

// MemBytes is the RAM the store holds: the sparse indexes of the segments
// (everything else is on disk). Docs/19 gives the formula.
func (s *Store) MemBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int64
	for _, g := range s.segs {
		n += g.indexBytes()
	}
	return n
}

// DiskBytes is the size of the sealed segments plus the open files.
func (s *Store) DiskBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int64
	for _, g := range s.segs {
		n += g.size
	}
	if ents, err := os.ReadDir(s.opt.Dir); err == nil {
		for _, de := range ents {
			if _, ok := parseOpenName(de.Name()); ok {
				if fi, err := de.Info(); err == nil {
					n += fi.Size()
				}
			}
		}
	}
	return n
}

// Segments describes the sealed segments (tests, docs/19).
type SegInfo struct {
	Level        int
	First, Last  int64
	Entries      int64
	Bytes        int64
	Blocks       int
	MinTS, MaxTS int64
}

func (s *Store) Segments() []SegInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []SegInfo
	for _, g := range s.segs {
		out = append(out, SegInfo{g.level, g.first, g.last, g.count, g.size, len(g.blocks), g.minTS, g.maxTS})
	}
	return out
}

// Floor is the oldest time (unix ns) the store still holds a version of,
// or 0 when it holds nothing. Unsealed buckets count from their start.
func (s *Store) Floor() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := int64(0), false
	take := func(v int64) {
		if !ok || v < f {
			f, ok = v, true
		}
	}
	for _, g := range s.segs {
		take(g.minTS)
	}
	for _, b := range s.pending {
		take(s.bucketStart(b))
	}
	if s.haveActive {
		take(s.bucketStart(s.abucket))
	}
	return f
}
