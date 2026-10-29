// Package wal implements the authoritative log.
//
// The log is not a recovery side-file next to a separate data store: row
// values live here and the primary index points into it. A transaction is
// committed exactly when its record is durable (written + fdatasync).
//
// Layout: numbered segment files, preallocated with fallocate so that
// running out of disk space surfaces when a segment is created (a clean,
// retryable failure) rather than in the middle of a record.
//
// Frame:   [u32 payloadLen][u32 crc32c(payload)][payload]
// Payload: [u8 kind][uvarint lsn][varint ts] body
// A zero length marks the end of the written part of a segment.
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	KindTx  uint8 = 1
	KindDDL uint8 = 2
	// KindMove relocates still-needed versions out of a segment that is
	// being reclaimed. It changes where versions live, never what they are:
	// each op carries the original LSN and commit time of the version.
	KindMove uint8 = 3
	// KindTxPart is a non-final frame of a transaction too large for one
	// frame. Its frames are consecutive in the log, share one LSN and end
	// with a KindTx frame; a chain without that frame never committed.
	KindTxPart uint8 = 4

	OpPut uint8 = 1
	OpDel uint8 = 2

	frameHdr  = 8
	maxRecord = 64 << 20

	// Limits for one transaction frame; larger transactions are split
	// (Loc.Idx is 16 bits, and readers cap a frame at maxRecord).
	MaxFrameOps   = 4096
	MaxFrameBytes = 4 << 20
)

var (
	ErrTorn    = errors.New("wal: torn or corrupt record")
	ErrCorrupt = errors.New("wal: corruption before the tail of the log")
	ErrRemoved = errors.New("wal: segment was reclaimed")
	crcTable   = crc32.MakeTable(crc32.Castagnoli)
)

type Pos struct {
	Seg uint32
	Off int64
}

func (a Pos) Less(b Pos) bool { return a.Seg < b.Seg || (a.Seg == b.Seg && a.Off < b.Off) }

// Loc addresses one op inside one record; used for version chains. It is
// 12 bytes (one is kept per version of every key): offsets fit in 32 bits
// because segments are at most MaxSegment bytes.
type Loc struct {
	Seg uint32
	Off uint32
	Idx uint16
	Ok  bool
}

// MaxSegment is the largest segment size: Loc offsets are 32-bit.
const MaxSegment = 1 << 32

type Op struct {
	Kind   uint8
	Table  uint32
	Key    []byte
	Val    []byte
	Prev   Loc
	ValOff int64 // absolute offset of Val in the segment (set by decoder)
	// KindMove only: identity of the relocated version
	OrigLSN uint64
	OrigTS  int64
}

type Record struct {
	Kind uint8
	LSN  uint64
	TS   int64
	Ops  []Op
	DDL  []byte
	Pos  Pos
	Size int
}

// AppendRecord frames a record onto dst. valRel receives, per op, the
// offset of its value relative to the start of the frame.
func AppendRecord(dst []byte, kind uint8, lsn uint64, ts int64, ops []Op, ddl []byte, valRel []int) ([]byte, []int) {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0, 0, 0, 0, 0)
	dst = append(dst, kind)
	dst = binary.AppendUvarint(dst, lsn)
	dst = binary.AppendVarint(dst, ts)
	if kind == KindTx || kind == KindTxPart || kind == KindMove {
		dst = binary.AppendUvarint(dst, uint64(len(ops)))
		for _, op := range ops {
			if kind == KindMove {
				dst = binary.AppendUvarint(dst, op.OrigLSN)
				dst = binary.AppendVarint(dst, op.OrigTS)
			}
			dst = append(dst, op.Kind)
			dst = binary.AppendUvarint(dst, uint64(op.Table))
			dst = binary.AppendUvarint(dst, uint64(len(op.Key)))
			dst = append(dst, op.Key...)
			if op.Prev.Ok {
				dst = binary.AppendUvarint(dst, uint64(op.Prev.Seg)+1)
				dst = binary.AppendUvarint(dst, uint64(op.Prev.Off))
				dst = binary.AppendUvarint(dst, uint64(op.Prev.Idx))
			} else {
				dst = binary.AppendUvarint(dst, 0)
			}
			dst = binary.AppendUvarint(dst, uint64(len(op.Val)))
			valRel = append(valRel, len(dst)-start)
			dst = append(dst, op.Val...)
		}
	} else {
		dst = append(dst, ddl...)
	}
	payload := dst[start+frameHdr:]
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(dst[start+4:], crc32.Checksum(payload, crcTable))
	return dst, valRel
}

// DecodeRecord parses one frame at the start of b, which sits at absolute
// position pos. It returns io.ErrUnexpectedEOF if b is too short to hold
// the whole frame, io.EOF on the end-of-log marker and ErrTorn on a bad frame.
// Keys and values alias b.
func DecodeRecord(b []byte, pos Pos) (*Record, error) {
	if len(b) < frameHdr {
		if allZero(b) {
			return nil, io.EOF
		}
		return nil, io.ErrUnexpectedEOF
	}
	n := binary.LittleEndian.Uint32(b)
	if n == 0 {
		return nil, io.EOF
	}
	if n > maxRecord {
		return nil, ErrTorn
	}
	if len(b) < frameHdr+int(n) {
		return nil, io.ErrUnexpectedEOF
	}
	payload := b[frameHdr : frameHdr+int(n)]
	if crc32.Checksum(payload, crcTable) != binary.LittleEndian.Uint32(b[4:]) {
		return nil, ErrTorn
	}
	r := &Record{Pos: pos, Size: frameHdr + int(n)}
	p := 0
	rd := func() uint64 {
		v, k := binary.Uvarint(payload[p:])
		if k <= 0 {
			p = -1 << 30
			return 0
		}
		p += k
		return v
	}
	r.Kind = payload[0]
	p = 1
	r.LSN = rd()
	ts, k := binary.Varint(payload[max(p, 0):])
	if p < 0 || k <= 0 {
		return nil, ErrTorn
	}
	r.TS = ts
	p += k
	switch r.Kind {
	case KindTx, KindTxPart, KindMove:
		nops := rd()
		if p < 0 || nops > uint64(len(payload)) {
			return nil, ErrTorn
		}
		r.Ops = make([]Op, nops)
		for i := range r.Ops {
			op := &r.Ops[i]
			if r.Kind == KindMove {
				op.OrigLSN = rd()
				if p < 0 {
					return nil, ErrTorn
				}
				ts, k := binary.Varint(payload[p:])
				if k <= 0 {
					return nil, ErrTorn
				}
				op.OrigTS = ts
				p += k
			}
			if p < 0 || p >= len(payload) {
				return nil, ErrTorn
			}
			op.Kind = payload[p]
			p++
			op.Table = uint32(rd())
			kl := rd()
			if p < 0 || uint64(len(payload)-p) < kl {
				return nil, ErrTorn
			}
			op.Key = payload[p : p+int(kl)]
			p += int(kl)
			if ps := rd(); ps != 0 {
				op.Prev = Loc{Ok: true, Seg: uint32(ps - 1), Off: uint32(rd()), Idx: uint16(rd())}
			}
			vl := rd()
			if p < 0 || uint64(len(payload)-p) < vl {
				return nil, ErrTorn
			}
			op.ValOff = pos.Off + frameHdr + int64(p)
			op.Val = payload[p : p+int(vl)]
			p += int(vl)
		}
	case KindDDL:
		if p < 0 {
			return nil, ErrTorn
		}
		r.DDL = payload[p:]
		p = len(payload)
	default:
		return nil, ErrTorn
	}
	if p != len(payload) {
		return nil, ErrTorn
	}
	return r, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Fault lets tests inject storage failures (disk full, I/O error).
type Fault interface {
	BeforeWrite(n int) (allow int, err error) // allow < n simulates a partial write
	BeforeSync() error
	BeforeCreate() error
}

type Log struct {
	dir    string
	segMax int64

	mu    sync.RWMutex
	files map[uint32]*os.File
	maps  map[uint32]*segMap // read-only mappings, made on first read
	segs  []uint32
	zsegs map[uint32]*zseg // sealed segments stored compressed (zseg.go)
	pins  map[uint32]int   // readers of raw segment files (see pin)

	zcacheOnce  sync.Once
	zcacheP     *blockCache  // decompressed blocks, made on first use
	ZBlockLoads atomic.Int64 // compressed blocks read and decompressed

	w    *os.File
	wSeg uint32
	wOff int64

	Fault        Fault
	bytesWritten atomic.Int64  // bytes handed to the OS (for WAF accounting)
	curSeg       atomic.Uint32 // mirror of wSeg for readers outside the writer goroutine

	// PreZero prepares the next segment in the background by writing zeros
	// into it. fdatasync on fully written blocks needs no extent conversion
	// (measured ~2x lower sync latency on ext4/SATA), at the price of
	// writing every segment twice.
	PreZero   bool
	ZeroRate  int64 // bytes/s cap for background zeroing
	spareMu   sync.Mutex
	spareSeg  uint32
	spareOk   bool
	spareDone chan struct{} // non-nil while a spare is being prepared
	zeroBytes atomic.Int64
}

func segName(dir string, seg uint32) string {
	return filepath.Join(dir, fmt.Sprintf("%08d.wal", seg))
}

func Open(dir string, segMax int64) (*Log, error) {
	if segMax <= 0 || segMax > MaxSegment {
		return nil, fmt.Errorf("wal: segment size %d out of range (at most %d)", segMax, int64(MaxSegment))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	l := &Log{dir: dir, segMax: segMax, files: map[uint32]*os.File{}, maps: map[uint32]*segMap{},
		zsegs: map[uint32]*zseg{}, pins: map[uint32]int{}}
	raw := map[uint32]bool{}
	for _, e := range ents {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".walz.tmp"):
			os.Remove(filepath.Join(dir, name)) // an unfinished compression
		case strings.HasSuffix(name, ".walz"):
			n, err := strconv.ParseUint(strings.TrimSuffix(name, ".walz"), 10, 32)
			if err != nil {
				continue
			}
			z, err := openZseg(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			l.zsegs[uint32(n)] = z
		case strings.HasSuffix(name, ".wal"):
			n, err := strconv.ParseUint(strings.TrimSuffix(name, ".wal"), 10, 32)
			if err != nil {
				continue
			}
			raw[uint32(n)] = true
		}
	}
	for seg := range l.zsegs {
		if raw[seg] {
			// a crash after the compressed copy was installed but before the
			// raw one was removed: both are complete, keep the compressed one
			os.Remove(segName(dir, seg))
		}
		raw[seg] = true
	}
	for seg := range raw {
		l.segs = append(l.segs, seg)
	}
	sort.Slice(l.segs, func(i, j int) bool { return l.segs[i] < l.segs[j] })
	return l, nil
}

func (l *Log) SegMax() int64 { return l.segMax }

func (l *Log) Segments() []uint32 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]uint32(nil), l.segs...)
}

func (l *Log) file(seg uint32) (*os.File, error) {
	l.mu.RLock()
	f := l.files[seg]
	l.mu.RUnlock()
	if f != nil {
		return f, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if f = l.files[seg]; f != nil {
		return f, nil
	}
	if l.removedLocked(seg) {
		return nil, ErrRemoved
	}
	f, err := os.OpenFile(segName(l.dir, seg), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	l.files[seg] = f
	return f, nil
}

func (l *Log) ReadAt(seg uint32, off int64, p []byte) (int, error) {
	if z := l.zsegOf(seg); z != nil {
		return l.zReadAt(z, p, off)
	}
	if m := l.mapping(seg); m != nil {
		ok := off >= 0 && off+int64(len(p)) <= int64(len(m.data)) && copyMapped(p, m.data[off:])
		m.release()
		if ok {
			return len(p), nil
		}
	}
	src, done, err := l.source(seg)
	if err != nil {
		return 0, err
	}
	defer done()
	return src.ReadAt(p, off)
}

// ReadHeader reads the kind, LSN and commit time of the record framed at
// (seg, off) and none of its operations: a record holds a whole
// transaction, up to maxRecord bytes. Like a value read at its offset, the
// header is not checked against the record's CRC.
func (l *Log) ReadHeader(seg uint32, off int64) (kind uint8, lsn uint64, ts int64, err error) {
	var b [frameHdr + 1 + 2*binary.MaxVarintLen64]byte
	if _, err = l.ReadAt(seg, off, b[:frameHdr]); err != nil {
		return
	}
	n := binary.LittleEndian.Uint32(b[:])
	if n == 0 || n > maxRecord {
		return 0, 0, 0, ErrTorn
	}
	h := b[:frameHdr+min(int(n), len(b)-frameHdr)]
	if _, err = l.ReadAt(seg, off, h); err != nil {
		return
	}
	p := h[frameHdr:]
	kind = p[0]
	lsn, k := binary.Uvarint(p[1:])
	if k <= 0 {
		return 0, 0, 0, ErrTorn
	}
	ts, k2 := binary.Varint(p[1+k:])
	if k2 <= 0 {
		return 0, 0, 0, ErrTorn
	}
	return kind, lsn, ts, nil
}

// ReadRecord reads the single record framed at (seg, off).
func (l *Log) ReadRecord(seg uint32, off int64) (*Record, error) {
	var hdr [frameHdr]byte
	if _, err := l.ReadAt(seg, off, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 || n > maxRecord {
		return nil, ErrTorn
	}
	buf := make([]byte, frameHdr+int(n))
	if _, err := l.ReadAt(seg, off, buf); err != nil {
		return nil, err
	}
	return DecodeRecord(buf, Pos{seg, off})
}

// Scan reads records of one segment starting at off, calling fn for each.
// limit bounds the bytes considered (-1 = whole segment). It returns the
// offset after the last valid record and whether scanning stopped on a torn
// frame rather than on the end-of-log marker.
func (l *Log) Scan(seg uint32, off, limit int64, fn func(*Record) error) (int64, bool, error) {
	f, done, err := l.source(seg)
	if err != nil {
		return off, false, err
	}
	defer done()
	end, err := f.size()
	if err != nil {
		return off, false, err
	}
	if limit >= 0 && limit < end {
		end = limit
	}
	// size the buffer to what will actually be read: followers call Scan
	// once per commit batch with a small range
	const chunk = 4 << 20
	buf := make([]byte, 0, max(min(chunk, end-off), 64))
	bufOff := off // file offset of buf[0]
	fill := func() error {
		// keep the unread tail, then read as much as fits
		rel := int(off - bufOff)
		buf = append(buf[:0], buf[rel:]...)
		bufOff = off
		want := min(int64(cap(buf)), end-bufOff)
		if int64(len(buf)) >= want {
			return nil
		}
		old := len(buf)
		buf = buf[:want]
		n, err := f.ReadAt(buf[old:], bufOff+int64(old))
		buf = buf[:old+n]
		if err != nil && err != io.EOF {
			return err
		}
		return nil
	}
	for {
		if off >= end {
			return off, false, nil
		}
		rec, err := DecodeRecord(buf[off-bufOff:], Pos{seg, off})
		if err == io.ErrUnexpectedEOF || (err == io.EOF && len(buf[off-bufOff:]) < frameHdr) {
			if bufOff+int64(len(buf)) >= end && off == bufOff {
				if err == io.EOF {
					return off, false, nil
				}
				return off, true, nil // frame extends past the readable end
			}
			if off == bufOff && len(buf) == cap(buf) {
				// one record larger than the buffer: grow
				nb := make([]byte, len(buf), 2*cap(buf)+maxRecord/16)
				copy(nb, buf)
				buf = nb
			}
			if err := fill(); err != nil {
				return off, false, err
			}
			continue
		}
		switch {
		case err == io.EOF:
			return off, false, nil
		case err != nil:
			return off, true, nil
		}
		if err := fn(rec); err != nil {
			return off, false, err
		}
		off += int64(rec.Size)
	}
}

// PrepareAppend positions the writer at (seg, off). Everything after off in
// that segment is discarded (hole-punched) so that bytes from an
// unacknowledged, partially persisted write can never be read back later.
// Later segments are removed.
func (l *Log) PrepareAppend(seg uint32, off int64) error {
	for _, s := range l.Segments() {
		if s > seg {
			l.mu.Lock()
			l.dropLocked(s)
			l.mu.Unlock()
			if err := os.Remove(segName(l.dir, s)); err != nil {
				return err
			}
		}
	}
	exists := false
	for _, s := range l.Segments() {
		if s == seg {
			exists = true
		}
	}
	if !exists {
		if err := l.create(seg); err != nil {
			return err
		}
	}
	f, err := l.file(seg)
	if err != nil {
		return err
	}
	if off < l.segMax && l.PreZero {
		// Overwrite the tail with zeros: this discards stale bytes like the
		// hole punch below, and leaves fully written blocks, so the first
		// segment gets the same low fdatasync latency as pre-zeroed spares.
		if err := l.zeroRange(f, off, l.segMax, 0); err != nil {
			return fmt.Errorf("zero tail: %w", err)
		}
	} else if off < l.segMax {
		const punch = 0x01 | 0x02 // FALLOC_FL_KEEP_SIZE | FALLOC_FL_PUNCH_HOLE
		if err := syscall.Fallocate(int(f.Fd()), punch, off, l.segMax-off); err != nil {
			return fmt.Errorf("punch hole: %w", err)
		}
		if err := syscall.Fallocate(int(f.Fd()), 0, off, l.segMax-off); err != nil {
			return fmt.Errorf("fallocate: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	l.mu.Lock()
	l.segs = l.segs[:0]
	for _, s := range l.segsOnDisk() {
		l.segs = append(l.segs, s)
	}
	l.mu.Unlock()
	l.w, l.wSeg, l.wOff = f, seg, off
	l.curSeg.Store(seg)
	if err := syncDir(l.dir); err != nil {
		return err
	}
	l.prepareSpare(seg + 1)
	return nil
}

// ZeroBytes is the number of bytes written to pre-zero segments.
func (l *Log) ZeroBytes() int64 { return l.zeroBytes.Load() }

func (l *Log) prepareSpare(seg uint32) {
	if !l.PreZero {
		return
	}
	l.spareMu.Lock()
	defer l.spareMu.Unlock()
	if l.spareDone != nil || (l.spareOk && l.spareSeg == seg) {
		return
	}
	done := make(chan struct{})
	l.spareDone = done
	go func() {
		err := l.createZeroed(seg)
		l.spareMu.Lock()
		l.spareDone = nil
		l.spareOk, l.spareSeg = err == nil, seg
		l.spareMu.Unlock()
		close(done)
	}()
}

func (l *Log) createZeroed(seg uint32) error {
	if err := l.create(seg); err != nil {
		return err
	}
	f, err := l.file(seg)
	if err != nil {
		return err
	}
	if err := l.zeroRange(f, 0, l.segMax, l.ZeroRate); err != nil {
		return err
	}
	return f.Sync()
}

// zeroRange writes zeros over [from, to) of f, at most rate bytes/s (0 = unlimited).
func (l *Log) zeroRange(f *os.File, from, to, rate int64) error {
	zero := make([]byte, 1<<20)
	t0 := time.Now()
	for off := from; off < to; off += int64(len(zero)) {
		n := min(int64(len(zero)), to-off)
		if _, err := f.WriteAt(zero[:n], off); err != nil {
			return err
		}
		l.zeroBytes.Add(n)
		if rate > 0 {
			if ahead := time.Duration(float64(off+n-from)/float64(rate)*float64(time.Second)) - time.Since(t0); ahead > 0 {
				time.Sleep(ahead)
			}
		}
	}
	return nil
}

func (l *Log) segsOnDisk() []uint32 {
	ents, _ := os.ReadDir(l.dir)
	var out []uint32
	for _, e := range ents {
		name := e.Name()
		if strings.HasSuffix(name, ".walz") {
			name = strings.TrimSuffix(name, "z") // compressed sealed segment
		}
		if n, err := strconv.ParseUint(strings.TrimSuffix(name, ".wal"), 10, 32); err == nil && strings.HasSuffix(name, ".wal") {
			if len(out) == 0 || out[len(out)-1] != uint32(n) {
				out = append(out, uint32(n))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (l *Log) create(seg uint32) error {
	if l.Fault != nil {
		if err := l.Fault.BeforeCreate(); err != nil {
			return err
		}
	}
	name := segName(l.dir, seg)
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, l.segMax); err != nil {
		f.Close()
		os.Remove(name)
		return fmt.Errorf("preallocate segment: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := syncDir(l.dir); err != nil {
		f.Close()
		return err
	}
	l.mu.Lock()
	l.files[seg] = f
	l.segs = append(l.segs, seg)
	l.mu.Unlock()
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// SyncError marks a failed fdatasync. The log must not be written to again.
type SyncError struct{ Err error }

func (e *SyncError) Error() string { return "fdatasync failed: " + e.Err.Error() }
func (e *SyncError) Unwrap() error { return e.Err }

// WritePos is the position the next Append will write at.
func (l *Log) WritePos() Pos { return Pos{l.wSeg, l.wOff} }

// WriteSeg is the segment being written; safe to call from any goroutine
// (WritePos is for the writer only).
func (l *Log) WriteSeg() uint32 { return l.curSeg.Load() }

func (l *Log) Remaining() int64 { return l.segMax - l.wOff }

// Roll makes the current segment durable and switches to a fresh one. A
// failure (e.g. ENOSPC on preallocation) leaves the writer unchanged.
func (l *Log) Roll() error {
	if err := l.Sync(); err != nil {
		// not retryable: after a failed fsync the page cache may no longer
		// hold the data, so a later "successful" fsync proves nothing
		return &SyncError{err}
	}
	next := l.wSeg + 1
	l.spareMu.Lock()
	wait := l.spareDone
	l.spareMu.Unlock()
	if wait != nil {
		<-wait // a spare is being zeroed; it is (almost always) the one we need
	}
	l.spareMu.Lock()
	ready := l.spareOk && l.spareSeg == next
	l.spareOk = false
	l.spareMu.Unlock()
	if !ready {
		l.mu.RLock()
		_, exists := l.files[next] // a spare whose zeroing failed is still a valid preallocated segment
		l.mu.RUnlock()
		if !exists {
			if err := l.create(next); err != nil {
				return err
			}
		}
	}
	defer l.prepareSpare(next + 1)
	f, err := l.file(next)
	if err != nil {
		return err
	}
	l.w, l.wSeg, l.wOff = f, l.wSeg+1, 0
	l.curSeg.Store(l.wSeg)
	return nil
}

// Append writes buf at the current position. The caller must have ensured
// len(buf) <= Remaining().
func (l *Log) Append(buf []byte) (Pos, error) {
	at := Pos{l.wSeg, l.wOff}
	n := len(buf)
	var ferr error
	if l.Fault != nil {
		var allow int
		allow, ferr = l.Fault.BeforeWrite(n)
		n = allow
	}
	if n > 0 {
		if _, err := l.w.WriteAt(buf[:n], l.wOff); err != nil {
			return at, err
		}
	}
	if ferr != nil {
		return at, ferr
	}
	l.wOff += int64(len(buf))
	l.bytesWritten.Add(int64(len(buf)))
	return at, nil
}

// Writer is the file Append currently writes to.
func (l *Log) Writer() *os.File { return l.w }

// SyncTo makes everything appended to f durable. Unlike Sync it may run
// concurrently with Append and Roll (the commit pipeline syncs one batch
// while the next is being written).
func (l *Log) SyncTo(f *os.File) error {
	if l.Fault != nil {
		if err := l.Fault.BeforeSync(); err != nil {
			return err
		}
	}
	return syscall.Fdatasync(int(f.Fd()))
}

func (l *Log) Sync() error {
	if l.Fault != nil {
		if err := l.Fault.BeforeSync(); err != nil {
			return err
		}
	}
	return syscall.Fdatasync(int(l.w.Fd()))
}

func (l *Log) Dir() string { return l.dir }

func (l *Log) BytesWritten() int64 { return l.bytesWritten.Load() }

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for seg := range l.files {
		l.dropLocked(seg)
	}
	for seg := range l.maps {
		l.dropLocked(seg)
	}
	for _, z := range l.zsegs {
		z.f.Close()
	}
	return nil
}

// DiskBytes reports the bytes of segment space in use (written part only is
// not tracked separately; segments are preallocated).
func (l *Log) DiskBytes() int64 {
	raw, comp := l.SegmentFileBytes()
	return raw + comp
}

// Removed reports whether seg was reclaimed: it is older than the newest
// segment but no longer present.
func (l *Log) Removed(seg uint32) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.removedLocked(seg)
}

func (l *Log) removedLocked(seg uint32) bool {
	if len(l.segs) == 0 || seg >= l.segs[len(l.segs)-1] {
		return false
	}
	i := sort.Search(len(l.segs), func(i int) bool { return l.segs[i] >= seg })
	return i == len(l.segs) || l.segs[i] != seg
}

// Remove deletes a sealed segment. The caller guarantees that nothing
// references it any more (no reader, recovery start or log follower).
func (l *Log) Remove(seg uint32) error {
	l.mu.Lock()
	if len(l.segs) == 0 || seg >= l.segs[len(l.segs)-1] {
		l.mu.Unlock()
		return fmt.Errorf("wal: refusing to remove active segment %d", seg)
	}
	l.dropLocked(seg)
	z := l.zsegs[seg]
	delete(l.zsegs, seg)
	i := sort.Search(len(l.segs), func(i int) bool { return l.segs[i] >= seg })
	if i < len(l.segs) && l.segs[i] == seg {
		l.segs = append(l.segs[:i], l.segs[i+1:]...)
	}
	l.mu.Unlock()
	if z != nil {
		z.f.Close()
		if err := os.Remove(zsegName(l.dir, seg)); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := os.Remove(segName(l.dir, seg)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(l.dir)
}

// segMap is a read-only shared mapping of a segment. Reads through it are
// a memory copy instead of a pread per value. Appends (pwrite) go through
// the same page cache, so the mapping sees them once written. Unmapping
// waits for the last reader: refs counts readers plus one for the Log,
// dropped when the segment is closed.
type segMap struct {
	data []byte
	refs atomic.Int64
}

func (m *segMap) acquire() bool {
	for {
		r := m.refs.Load()
		if r == 0 {
			return false
		}
		if m.refs.CompareAndSwap(r, r+1) {
			return true
		}
	}
}

func (m *segMap) release() {
	if m.refs.Add(-1) == 0 {
		syscall.Munmap(m.data)
	}
}

// mapping returns seg's mapping with a reference held, or nil when it
// cannot be mapped (reads then use pread).
func (l *Log) mapping(seg uint32) *segMap {
	l.mu.RLock()
	m := l.maps[seg]
	if m != nil && m.acquire() {
		l.mu.RUnlock()
		return m
	}
	compressed := l.zsegs[seg] != nil
	l.mu.RUnlock()
	if compressed {
		return nil
	}
	f, err := l.file(seg)
	if err != nil {
		return nil
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.files[seg] != f { // closed meanwhile
		return nil
	}
	if m = l.maps[seg]; m == nil {
		data, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return nil
		}
		m = &segMap{data: data}
		m.refs.Store(1)
		l.maps[seg] = m
	}
	if !m.acquire() {
		return nil
	}
	return m
}

// dropLocked closes seg's file and releases the Log's reference to its
// mapping. l.mu must be held for writing.
func (l *Log) dropLocked(seg uint32) {
	if f := l.files[seg]; f != nil {
		f.Close()
		delete(l.files, seg)
	}
	if m := l.maps[seg]; m != nil {
		delete(l.maps, seg)
		m.release()
	}
}

// SegReader reads like Log.ReadAt but keeps the mapping of the last segment
// it read, so a goroutine reading many values does not touch the Log's
// shared lock and reference counts for each one. Not safe for concurrent
// use; Close releases the mapping.
type SegReader struct {
	l   *Log
	seg uint32
	m   *segMap
}

func (l *Log) Reader() *SegReader { return &SegReader{l: l} }

func (r *SegReader) ReadAt(seg uint32, off int64, p []byte) (int, error) {
	if r.m == nil || r.seg != seg {
		r.Close()
		r.m, r.seg = r.l.mapping(seg), seg
	}
	if m := r.m; m != nil && off >= 0 && off+int64(len(p)) <= int64(len(m.data)) && copyMapped(p, m.data[off:]) {
		return len(p), nil
	}
	return r.l.ReadAt(seg, off, p)
}

func (r *SegReader) Close() {
	if r.m != nil {
		r.m.release()
		r.m = nil
	}
}

// copyMapped copies from a mapping. A disk error under a mapped page
// raises SIGBUS instead of returning EIO; here it becomes ok=false, and
// the caller rereads with pread, which reports the error.
func copyMapped(dst, src []byte) (ok bool) {
	old := debug.SetPanicOnFault(true)
	defer func() {
		debug.SetPanicOnFault(old)
		if recover() != nil {
			ok = false
		}
	}()
	copy(dst, src)
	return true
}

// Residency reports how much of the segments mapped for reading is in
// memory (page cache): reads of values in resident pages do not touch the
// disk. It costs one mincore call per mapped segment.
func (l *Log) Residency() (mapped, resident int64) {
	l.mu.RLock()
	ms := make([]*segMap, 0, len(l.maps))
	for _, m := range l.maps {
		if m.acquire() {
			ms = append(ms, m)
		}
	}
	l.mu.RUnlock()
	page := int64(os.Getpagesize())
	var vec []byte
	for _, m := range ms {
		n := (int64(len(m.data)) + page - 1) / page
		if int64(cap(vec)) < n {
			vec = make([]byte, n)
		}
		vec = vec[:n]
		if len(m.data) > 0 {
			_, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&m.data[0])), uintptr(len(m.data)), uintptr(unsafe.Pointer(&vec[0])))
			if e == 0 {
				mapped += int64(len(m.data))
				for _, b := range vec {
					if b&1 != 0 {
						resident += page
					}
				}
			}
		}
		m.release()
	}
	return mapped, min(resident, mapped)
}

// OpKindAt returns the kind (OpPut, OpDel) of op idx of the record at
// (seg, off), reading only the bytes up to that op: keys and values of the
// ops before it are skipped, not read. The checksum is not verified (the
// record was when it was written or recovered); a malformed record gives
// ErrTorn.
func (l *Log) OpKindAt(seg uint32, off int64, idx uint16) (uint8, error) {
	r := opReader{l: l, seg: seg, pos: off + frameHdr}
	var hdr [frameHdr]byte
	if _, err := l.ReadAt(seg, off, hdr[:]); err != nil {
		return 0, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 || n > maxRecord {
		return 0, ErrTorn
	}
	r.end = off + frameHdr + int64(n)
	kind := r.byte()
	r.uvarint() // lsn
	r.uvarint() // ts (zigzag)
	if kind != KindTx && kind != KindTxPart && kind != KindMove {
		return 0, ErrTorn
	}
	nops := r.uvarint()
	if r.err == nil && uint64(idx) >= nops {
		return 0, ErrTorn
	}
	for i := uint16(0); r.err == nil; i++ {
		if kind == KindMove {
			r.uvarint()
			r.uvarint()
		}
		ok := r.byte()
		if i == idx {
			return ok, r.err
		}
		r.uvarint() // table
		r.skip(r.uvarint())
		if r.uvarint() != 0 { // prev
			r.uvarint()
			r.uvarint()
		}
		r.skip(r.uvarint())
	}
	return 0, r.err
}

// opReader reads a frame's fields through a small window.
type opReader struct {
	l        *Log
	seg      uint32
	pos, end int64
	buf      [64]byte
	bpos     int64 // log offset of buf[0]
	blen     int
	err      error
}

func (r *opReader) byte() uint8 {
	if r.err != nil {
		return 0
	}
	if r.pos >= r.end {
		r.err = ErrTorn
		return 0
	}
	if r.pos < r.bpos || r.pos >= r.bpos+int64(r.blen) {
		n := int(min(int64(len(r.buf)), r.end-r.pos))
		if _, err := r.l.ReadAt(r.seg, r.pos, r.buf[:n]); err != nil {
			r.err = err
			return 0
		}
		r.bpos, r.blen = r.pos, n
	}
	b := r.buf[r.pos-r.bpos]
	r.pos++
	return b
}

func (r *opReader) uvarint() uint64 {
	var x uint64
	for s := uint(0); s < 70 && r.err == nil; s += 7 {
		b := r.byte()
		if b < 0x80 {
			return x | uint64(b)<<s
		}
		x |= uint64(b&0x7f) << s
	}
	if r.err == nil {
		r.err = ErrTorn
	}
	return 0
}

func (r *opReader) skip(n uint64) {
	if r.err == nil && n > uint64(r.end-r.pos) {
		r.err = ErrTorn
	}
	r.pos += int64(n)
}
