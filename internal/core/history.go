package core

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/schema"
	"stask/internal/wal"
)

// History retention (docs/03 section 8.5, docs/15 section 0).
//
// The log keeps every version of every key and the chain of versions is the
// Prev pointer in each record (engine.go History). What decides how much of
// it survives is the GC horizon (gc.go). This file makes that horizon
// per-table and time-based, and makes what the GC relies on survive a
// restart:
//
//   - A table's retention is schema.Table.HistoryRetention (ALTER TABLE t SET
//     (history_retention = '60 days')); tables without one use
//     Options.GCRetention (staskd -history-retention). A table's horizon is
//     min(LSN committed at now-retention, oldest pinned snapshot).
//   - The time -> LSN marks that turn "now - retention" into an LSN are
//     rebuilt after a restart from the first record of every segment (it
//     used to restart the clock: nothing was reclaimed for a whole
//     retention window after each restart, which with 60 days means never).
//   - The remap (old location of a relocated old version -> its copy) is
//     persisted in <dir>/gc-remap.bin before the segment it points out of
//     is removed, and the floors in <dir>/gc-state.json. Without them every
//     relocated old version was lost to the first restart: the chain of its
//     key was cut there.

// ---- clock and parsing ----

func (e *Engine) now() time.Time {
	if e.opts.Now != nil {
		return e.opts.Now()
	}
	return time.Now()
}

var retentionRE = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]+)\s*$`)

// ParseRetention reads a history retention: a Go duration ("1440h"), or a
// number with a day-scale unit ("60d", "60 days", "8w", "2 months" = 30
// days each). "" and "default" mean no table-specific retention (0).
func ParseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "default") || s == "0" {
		return 0, nil
	}
	if m := retentionRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, err
		}
		unit := strings.ToLower(m[2])
		var u time.Duration
		switch unit {
		case "d", "day", "days":
			u = 24 * time.Hour
		case "w", "week", "weeks":
			u = 7 * 24 * time.Hour
		case "mon", "month", "months":
			u = 30 * 24 * time.Hour
		case "y", "year", "years":
			u = 365 * 24 * time.Hour
		case "h", "hour", "hours", "hr", "hrs":
			u = time.Hour
		case "m", "min", "mins", "minute", "minutes":
			u = time.Minute
		case "s", "sec", "secs", "second", "seconds":
			u = time.Second
		}
		if u != 0 {
			if n*float64(u) > float64(math.MaxInt64) {
				return 0, fmt.Errorf("retention %q is too long", s)
			}
			return time.Duration(n * float64(u)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid history retention %q (examples: 60d, '60 days', 36h)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("history retention must not be negative")
	}
	return d, nil
}

// FormatRetention is d in days when it is a whole number of them.
func FormatRetention(d time.Duration) string {
	if d <= 0 {
		return "default"
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return d.String()
}

// SetHistoryRetention sets (d > 0) or clears (d == 0) a table's retention.
func (e *Engine) SetHistoryRetention(table string, d time.Duration) error {
	return e.AlterTable(table, func(t *schema.Table) error {
		t.HistoryRetention = int64(d)
		return nil
	})
}

// retentionOf is the effective history window of t (0: nothing but pinned
// snapshots, which only a negative GCRetention in tests gives).
func (e *Engine) retentionOf(t *TableState) time.Duration {
	if r := time.Duration(t.Schema().HistoryRetention); r > 0 {
		return max(r, 2*time.Second)
	}
	return e.retention()
}

// RetentionOf is the effective history retention of a table.
func (e *Engine) RetentionOf(table string) (time.Duration, error) {
	t, err := e.Table(table)
	if err != nil {
		return 0, err
	}
	return e.retentionOf(t), nil
}

func (e *Engine) maxRetention() time.Duration {
	m := e.retention()
	for _, t := range e.allTables() {
		m = max(m, e.retentionOf(t))
	}
	return m
}

// ---- horizons ----

// horizons are the GC horizons of one round: of tables without their own
// retention (def), of each table with one (by), and the lowest of them (min,
// what a segment is judged by before it is drained, and the floor).
type horizons struct {
	min, def uint64
	by       map[*TableState]uint64
	cls      map[int64]uint64 // by table retention setting
}

func (hs *horizons) ofClass(ret int64) uint64 {
	if ret <= 0 {
		return hs.def
	}
	if h, ok := hs.cls[ret]; ok {
		return h
	}
	return hs.min // unknown class (the table was altered): the safe side
}

// retClass is the accounting class of the table's versions (gc.go segStat).
func (t *TableState) retClass() int64 { return t.Schema().HistoryRetention }

// longestClass is the class that holds the longest history: what a segment
// whose composition is unknown (after a restart) is assumed to hold.
func (e *Engine) longestClass() int64 {
	best, bestR := int64(0), e.retention()
	for _, t := range e.allTables() {
		if r := e.retentionOf(t); r > bestR {
			best, bestR = t.retClass(), r
		}
	}
	return best
}

func (hs *horizons) of(t *TableState) uint64 {
	if h, ok := hs.by[t]; ok {
		return h
	}
	return hs.def
}

func (e *Engine) horizons() *horizons {
	e.gc.pinMu.Lock()
	defer e.gc.pinMu.Unlock()
	p := e.minPinned(e.durable.Load())
	now := e.now()
	cache := map[time.Duration]uint64{}
	hOf := func(r time.Duration) uint64 {
		if r <= 0 {
			return p
		}
		h, ok := cache[r]
		if !ok {
			h = min(p, e.lsnAtMarks(now.Add(-r).UnixNano()))
			cache[r] = h
		}
		return h
	}
	hs := &horizons{by: map[*TableState]uint64{}, cls: map[int64]uint64{}}
	hs.def = hOf(e.retention())
	hs.min = hs.def
	for _, t := range e.allTables() {
		if t.Schema().HistoryRetention > 0 {
			h := hOf(e.retentionOf(t))
			hs.by[t] = h
			hs.cls[t.retClass()] = h
			hs.min = min(hs.min, h)
		}
	}
	return hs
}

// lsnAtMarks is the durable LSN at the newest mark not after cut (unix
// nanos), 0 when the marks do not reach back that far. pinMu is held.
func (e *Engine) lsnAtMarks(cut int64) uint64 {
	if i := sortSearchMarks(e.gc.marks, cut); i > 0 {
		return e.gc.marks[i-1].lsn
	}
	return 0
}

// MarkNow records a time mark now (the cleaner or marker does it every
// second; tests that run a fake clock call it after moving it).
func (e *Engine) MarkNow() { e.addMark() }

// marker keeps the time marks when there is no cleaner to do it.
func (e *Engine) marker() {
	defer e.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
			e.addMark()
		}
	}
}

// markSpacing is the least time between marks: the finer the marks, the
// fewer versions an AS OF TIMESTAMP read has to date one by one. 60 days
// at 10s is 518400 marks, 8 MB in memory and in <dir>/gc-marks.bin.
func (e *Engine) markSpacing(maxRet time.Duration) time.Duration {
	return max(10*time.Second, maxRet/1000000)
}

// addMark records (time, durable LSN): every commit up to that LSN was made
// at or before that time. The LSN is read before the clock for that reason.
// A mark is added at most every markSpacing; ticks in between are free.
func (e *Engine) addMark() {
	maxRet := e.maxRetention()
	d := e.durable.Load()
	now := e.now()
	n := now.UnixNano()
	e.gc.pinMu.Lock()
	defer e.gc.pinMu.Unlock()
	k := len(e.gc.marks)
	if k > 0 && n-e.gc.marks[k-1].at < int64(e.markSpacing(maxRet)) {
		return
	}
	if k > 0 && (n < e.gc.marks[k-1].at || d < e.gc.marks[k-1].lsn) {
		return // the clock went back
	}
	e.gc.marks = append(e.gc.marks, lsnMark{n, d})
	e.appendMarkFile(lsnMark{n, d})
	// keep only one mark older than the longest retention cut
	cut := now.Add(-maxRet).UnixNano()
	i := 0
	for i+1 < len(e.gc.marks) && e.gc.marks[i+1].at <= cut {
		i++
	}
	if i >= 4096 {
		e.gc.marks = e.gc.marks[i:]
		e.gc.hist.marksDropped += i
		if e.gc.hist.marksDropped > 4096 && e.gc.hist.marksDropped > len(e.gc.marks) {
			e.rewriteMarkFile()
		}
	}
}

const markRec = 8 + 8 + 4

func (e *Engine) marksPath() string { return filepath.Join(e.opts.Dir, "gc-marks.bin") }

func appendMarkRec(dst []byte, m lsnMark) []byte {
	var b [markRec]byte
	binary.LittleEndian.PutUint64(b[:], uint64(m.at))
	binary.LittleEndian.PutUint64(b[8:], m.lsn)
	binary.LittleEndian.PutUint32(b[16:], crc32.ChecksumIEEE(b[:16]))
	return append(dst, b[:]...)
}

// appendMarkFile adds one mark (pinMu held). Not synced: a mark only says
// what was durable when it was taken, losing the last ones costs resolution.
func (e *Engine) appendMarkFile(m lsnMark) {
	f, err := os.OpenFile(e.marksPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	f.Write(appendMarkRec(nil, m))
	f.Close()
}

// rewriteMarkFile drops the expired marks from the file (pinMu held).
func (e *Engine) rewriteMarkFile() {
	var buf []byte
	for _, m := range e.gc.marks {
		buf = appendMarkRec(buf, m)
	}
	if writeFileSync(e.marksPath(), buf) == nil {
		e.gc.hist.marksDropped = 0
	}
}

// raiseFloors makes reads below the horizons fail instead of missing
// versions (the GC is about to remove, or has removed, log they may need).
func (e *Engine) raiseFloors(hs *horizons) {
	if hs.min > e.gc.floor.Load() {
		e.gc.floor.Store(hs.min)
	}
	for _, t := range e.allTables() {
		if th := hs.of(t); th > t.floor.Load() {
			t.floor.Store(th)
		}
	}
}

// noteCommit is called by a replica for each commit it applies: the leader's
// commit time of a record is exactly what a time mark needs, and its own
// clock is not (clocks differ, replicas lag).
func (e *Engine) noteCommit(lsn uint64, ts int64) {
	if ts-e.gc.hist.lastMarkAt.Load() < int64(e.markSpacing(0)) {
		return
	}
	e.gc.pinMu.Lock()
	k := len(e.gc.marks)
	if k == 0 || ts > e.gc.marks[k-1].at && lsn > e.gc.marks[k-1].lsn {
		m := lsnMark{ts, lsn}
		e.gc.marks = append(e.gc.marks, m)
		e.gc.hist.lastMarkAt.Store(ts)
		e.appendMarkFile(m)
		cut := ts - int64(e.maxRetention())
		i := 0
		for i+1 < len(e.gc.marks) && e.gc.marks[i+1].at <= cut {
			i++
		}
		if i >= 4096 {
			e.gc.marks = e.gc.marks[i:]
			e.gc.hist.marksDropped += i
		}
	}
	e.gc.pinMu.Unlock()
}

// checkSnapshotT refuses a read of t at a snapshot older than the history
// kept for it.
func (e *Engine) checkSnapshotT(t *TableState, snap uint64) error {
	if snap < t.floor.Load() {
		return ErrSnapshotTooOld
	}
	return e.checkSnapshot(snap)
}

// ---- persisted state ----

type histState struct {
	mu      sync.Mutex
	pending []remapSet // remap entries not yet in the file
	inFile  int        // entries in the file (some may have expired)
	// marks in gc-marks.bin that have expired from memory
	marksDropped int
	lastMarkAt   atomic.Int64 // replica: commit time of the newest mark
}

const remapRec = 12 + 12 + 8 + 4

func (e *Engine) remapPath() string   { return filepath.Join(e.opts.Dir, "gc-remap.bin") }
func (e *Engine) gcStatePath() string { return filepath.Join(e.opts.Dir, "gc-state.json") }

// setRemapLocked records a relocation (gc.remapMu is held).
func (g *gcState) setRemapLocked(r remapSet) {
	g.remap[r.from] = r.e
	g.hist.mu.Lock()
	g.hist.pending = append(g.hist.pending, r)
	g.hist.mu.Unlock()
}

func putLoc(b []byte, l wal.Loc) {
	binary.LittleEndian.PutUint32(b, l.Seg)
	binary.LittleEndian.PutUint32(b[4:], l.Off)
	binary.LittleEndian.PutUint16(b[8:], l.Idx)
	if l.Ok {
		b[10] = 1
	}
}

func getLoc(b []byte) wal.Loc {
	return wal.Loc{Seg: binary.LittleEndian.Uint32(b), Off: binary.LittleEndian.Uint32(b[4:]),
		Idx: binary.LittleEndian.Uint16(b[8:]), Ok: b[10] == 1}
}

func appendRemapRec(dst []byte, r remapSet) []byte {
	var b [remapRec]byte
	putLoc(b[:], r.from)
	putLoc(b[12:], r.e.to)
	binary.LittleEndian.PutUint64(b[24:], r.e.sup)
	binary.LittleEndian.PutUint32(b[32:], crc32.ChecksumIEEE(b[:32]))
	return append(dst, b[:]...)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

type gcStateFile struct {
	Floor  uint64            `json:"floor"`
	Tables map[string]uint64 `json:"table_floors,omitempty"`
}

// persistHistoryState makes the floors and the relocations since the last
// call durable. The GC calls it before it removes a segment: a relocated
// old version is reachable after a restart through this file only.
func (e *Engine) persistHistoryState() error {
	st := gcStateFile{Floor: e.gc.floor.Load(), Tables: map[string]uint64{}}
	for _, t := range e.allTables() {
		if f := t.floor.Load(); f > 0 {
			st.Tables[strconv.Itoa(int(t.Schema().ID))] = f
		}
	}
	b, _ := json.Marshal(st)
	if err := writeFileSync(e.gcStatePath(), b); err != nil {
		return err
	}
	e.gc.remapMu.RLock()
	e.gc.hist.mu.Lock()
	pend := e.gc.hist.pending
	e.gc.hist.pending = nil
	e.gc.hist.mu.Unlock()
	e.gc.remapMu.RUnlock()
	if len(pend) > 0 {
		var buf []byte
		for _, r := range pend {
			buf = appendRemapRec(buf, r)
		}
		f, err := os.OpenFile(e.remapPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err == nil {
			if _, err = f.Write(buf); err == nil {
				err = f.Sync()
			}
			f.Close()
		}
		if err != nil {
			e.gc.hist.mu.Lock()
			e.gc.hist.pending = append(pend, e.gc.hist.pending...)
			e.gc.hist.mu.Unlock()
			return fmt.Errorf("gc: persist remap: %w", err)
		}
		e.gc.hist.mu.Lock()
		e.gc.hist.inFile += len(pend)
		e.gc.hist.mu.Unlock()
	}
	syncDir(e.opts.Dir)
	return nil
}

// pruneRemapFile rewrites the remap file from memory once more than half
// of it has expired (entries are dropped from memory when the floor passes
// the version that superseded the relocated one).
func (e *Engine) pruneRemapFile() {
	e.gc.remapMu.RLock()
	n := len(e.gc.remap)
	e.gc.remapMu.RUnlock()
	e.gc.hist.mu.Lock()
	inFile := e.gc.hist.inFile
	e.gc.hist.mu.Unlock()
	if inFile < 4096 || inFile < 2*n {
		return
	}
	var buf []byte
	e.gc.remapMu.RLock()
	e.gc.hist.mu.Lock()
	for from, r := range e.gc.remap {
		buf = appendRemapRec(buf, remapSet{from, r})
	}
	e.gc.hist.pending = nil // all of it is in the new file
	e.gc.hist.mu.Unlock()
	e.gc.remapMu.RUnlock()
	if err := writeFileSync(e.remapPath(), buf); err == nil {
		e.gc.hist.mu.Lock()
		e.gc.hist.inFile = n
		e.gc.hist.mu.Unlock()
	}
}

// restoreHistoryState runs at open: it rebuilds the time marks from the
// log, loads the floors and the remap, and returns the first LSN of each
// segment.
func (e *Engine) restoreHistoryState() map[uint32]uint64 {
	g := &e.gc
	segLSN := map[uint32]uint64{}
	d := e.durable.Load()
	marks := []lsnMark{{0, 0}} // before everything: nothing was committed
	add := func(m lsnMark) {
		if k := len(marks); k > 0 && (m.at < marks[k-1].at || m.lsn < marks[k-1].lsn) {
			return
		}
		if m.lsn <= d {
			marks = append(marks, m)
		}
	}
	// marks of earlier runs, then the first record of each segment (a
	// commit time with its LSN, it fills in what the file lacks)
	var fileMarks []lsnMark
	if f, err := os.Open(e.marksPath()); err == nil {
		rd := bufio.NewReaderSize(f, 1<<20)
		var rec [markRec]byte
		for {
			if _, err := io.ReadFull(rd, rec[:]); err != nil ||
				binary.LittleEndian.Uint32(rec[16:]) != crc32.ChecksumIEEE(rec[:16]) {
				break
			}
			fileMarks = append(fileMarks, lsnMark{int64(binary.LittleEndian.Uint64(rec[:])), binary.LittleEndian.Uint64(rec[8:])})
		}
		f.Close()
	}
	var hdr []lsnMark
	for _, seg := range e.log.Segments() {
		_, lsn, ts, err := e.log.ReadHeader(seg, 0)
		if err != nil || lsn == 0 {
			continue
		}
		segLSN[seg] = lsn
		hdr = append(hdr, lsnMark{ts, lsn})
	}
	// merge by LSN
	for i, j := 0, 0; i < len(fileMarks) || j < len(hdr); {
		if j >= len(hdr) || i < len(fileMarks) && fileMarks[i].lsn <= hdr[j].lsn {
			add(fileMarks[i])
			i++
		} else {
			add(hdr[j])
			j++
		}
	}
	g.hist.marksDropped = 0
	if len(fileMarks) > len(marks) {
		g.hist.marksDropped = len(fileMarks) - len(marks)
	}
	if !e.opts.Replica { // a replica's marks are in the leader's time
		marks = append(marks, lsnMark{e.now().UnixNano(), d})
	}
	g.marks = marks
	g.hist.lastMarkAt.Store(marks[len(marks)-1].at)

	if b, err := os.ReadFile(e.gcStatePath()); err == nil {
		var st gcStateFile
		if json.Unmarshal(b, &st) == nil {
			g.floor.Store(st.Floor)
			for id, f := range st.Tables {
				if n, err := strconv.Atoi(id); err == nil {
					if t := e.TableByID(uint32(n)); t != nil {
						t.floor.Store(f)
					}
				}
			}
		}
	}
	if f, err := os.Open(e.remapPath()); err == nil {
		defer f.Close()
		rd := bufio.NewReaderSize(f, 1<<20)
		floor := g.floor.Load()
		var rec [remapRec]byte
		good := int64(0)
		n := 0
		for {
			if _, err := io.ReadFull(rd, rec[:]); err != nil {
				break
			}
			if binary.LittleEndian.Uint32(rec[32:]) != crc32.ChecksumIEEE(rec[:32]) {
				break
			}
			good += remapRec
			n++
			sup := binary.LittleEndian.Uint64(rec[24:])
			if sup <= floor {
				continue
			}
			g.remap[getLoc(rec[:])] = remapEntry{to: getLoc(rec[12:]), sup: sup}
		}
		g.hist.inFile = n
		if st, err := f.Stat(); err == nil && st.Size() != good && !e.opts.Replica {
			os.Truncate(e.remapPath(), good) // a torn tail: appended after
		}
	}
	return segLSN
}

// noteMoved records, on a replica applying a relocation record, where the
// relocated old version of key (original LSN orig, now at to) used to be:
// the leader keeps that in memory and does not ship it, so it is found by
// walking the key's chain from its head, where the old copy is still in the
// replica's log. head is the key's newest version, in the batch being
// applied.
func (e *Engine) noteMoved(head *Version, orig uint64, to wal.Loc) {
	defer e.enterRead().Add(-1)
	v := head
	for hops := 0; v != nil && v.LSN > orig && hops < 1<<20; hops++ {
		if e.fill(v) != nil || !v.Prev.Ok {
			return
		}
		ref, loc := v.Prev, v.Prev
		if e.goneSeg(loc.Seg) {
			r, ok := e.resolve(loc)
			if !ok {
				return
			}
			loc = r
		}
		p, err := e.loadVersion(loc)
		if err != nil {
			return
		}
		if p.LSN == orig {
			if loc != to {
				e.gc.remapMu.Lock()
				e.gc.setRemapLocked(remapSet{ref, remapEntry{to: to, sup: v.LSN}})
				e.gc.remapMu.Unlock()
			}
			return
		}
		v = p
	}
}

// ---- time to LSN ----

// TimeSnap dates a read AS OF TIMESTAMP in LSNs. Every commit up to Lo was
// made at or before NS; every commit from Hi on after it. The commits in
// between (a few seconds' worth: the marks are 10s apart) are dated one by
// one from the commit time of each version, so the read is exact.
type TimeSnap struct {
	Lo, Hi uint64
	NS     int64
}

// markMargin is the longest a transaction may take between being dated and
// becoming durable for a mark taken meanwhile to still bound it.
const markMargin = 2 * time.Second

// TimeSnapshot dates ns (unix nanos). ErrSnapshotTooOld if no mark reaches
// back that far.
func (e *Engine) TimeSnapshot(ns int64) (TimeSnap, error) {
	d := e.durable.Load()
	e.gc.pinMu.Lock()
	marks := e.gc.marks
	i := sortSearchMarks(marks, ns)
	if i == 0 {
		e.gc.pinMu.Unlock()
		return TimeSnap{}, ErrSnapshotTooOld
	}
	ts := TimeSnap{Lo: marks[i-1].lsn, Hi: d + 1, NS: ns}
	if j := sortSearchMarks(marks, ns+int64(markMargin)); j < len(marks) {
		ts.Hi = min(ts.Hi, marks[j].lsn+1)
	}
	e.gc.pinMu.Unlock()
	return ts, nil
}

// sortSearchMarks is the index of the first mark made after ns.
func sortSearchMarks(marks []lsnMark, ns int64) int {
	lo, hi := 0, len(marks)
	for lo < hi {
		m := (lo + hi) / 2
		if marks[m].at <= ns {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

// visibleAt walks the chain from v to the newest version made at or before
// the time of ts.
func (e *Engine) visibleAt(v *Version, ts TimeSnap) (*Version, error) {
	var err error
	for v != nil {
		if v.LSN <= ts.Lo {
			return v, nil
		}
		if v.LSN < ts.Hi {
			if err = e.fill(v); err != nil {
				return nil, err
			}
			if v.TS <= ts.NS {
				return v, nil
			}
		}
		if v, err = e.previous(v); err != nil {
			if err == errTruncated {
				err = ErrSnapshotTooOld
			}
			return nil, err
		}
	}
	return nil, nil
}

// GetAtTime reads key as it was at ts.
func (e *Engine) GetAtTime(table, key string, ts TimeSnap) (schema.Row, *Version, error) {
	t, err := e.Table(table)
	if err != nil {
		return nil, nil, err
	}
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshotT(t, ts.Lo); err != nil {
		return nil, nil, err
	}
	v, err := e.visibleAt(t.head(key), ts)
	if err != nil || v == nil || v.Deleted {
		return nil, v, err
	}
	r, err := e.rowOf(t, v)
	return r, v, err
}

// ScanSnapshotTime is ScanSnapshot at a time.
func (e *Engine) ScanSnapshotTime(table string, ts TimeSnap, fn func(key string, r schema.Row, v *Version) bool) error {
	t, err := e.Table(table)
	if err != nil {
		return err
	}
	e.waitInstalled(min(ts.Hi-1, e.durable.Load()))
	defer e.enterRead().Add(-1)
	if err := e.checkSnapshotT(t, ts.Lo); err != nil {
		return err
	}
	tr := t.index()
	var ierr error
	tr.ascend("", func(en *entry) bool {
		h := en.version()
		v, err := e.visibleAt(&h, ts)
		if err != nil {
			ierr = err
			return false
		}
		if v == nil || v.Deleted {
			return true
		}
		r, err := e.rowOf(t, v)
		if err != nil {
			ierr = err
			return false
		}
		return fn(strings.Clone(tr.keyOf(en)), r, v)
	})
	return ierr
}

// HistoryAsc is the versions of key that are still kept, oldest first.
func (e *Engine) HistoryAsc(table, key string) ([]HistoryEntry, error) {
	h, err := e.History(table, key, math.MaxInt32)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return h, nil
}
