package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Read-time verification (finding F1). The log is also the value store:
// a row is read from the log by the offset the index keeps. Before this, a
// byte flipped in a sealed segment (bit rot, a bad sector, a bad copy) was
// served as a value until a scrub happened to run. Like PostgreSQL page
// checksums (verified whenever a page is read from disk into shared
// buffers) and RocksDB verify_checksums, every frame that a read touches
// is now checked against its CRC32C (hardware accelerated, hash/crc32
// Castagnoli) once, before any byte of it is returned.
//
// Cost control without a format change: the frame CRC covers the whole
// frame, so the first read of a frame checks all of it (zero-copy over the
// mapping when the segment is mapped) and records that in a per-segment
// bitmap in RAM (one bit per 8 bytes of segment, 1/64 of its size, made on
// first use). Later reads of the same frame, a value or a version chain
// step, cost one atomic load. The bitmap is dropped when a scrub finds
// damage in the segment (InvalidateVerified), so a frame that rotted after
// it was checked is not served again.

// ReadCRCErrors counts reads refused because a frame failed its CRC
// (stask_read_crc_errors_total).
var ReadCRCErrors atomic.Int64

// CorruptError says where a frame failed verification. errors.Is(err,
// ErrCorrupt) is true.
type CorruptError struct {
	Seg uint32
	Off int64
	Why string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("data corruption detected: log segment %d offset %d: %s (SQLSTATE XX001; restore this node from a replica or backup, docs/33 4.4)", e.Seg, e.Off, e.Why)
}

func (e *CorruptError) Is(target error) bool { return target == ErrCorrupt }

// vbits is the verified-frames bitmap of one segment. Frames are at least
// 12 bytes, so one bit per 8 bytes of offset never maps two frames to one bit.
type vbits struct{ w []atomic.Uint32 }

func (v *vbits) has(off int64) bool {
	i := off >> 3
	if i>>5 >= int64(len(v.w)) {
		return false
	}
	return v.w[i>>5].Load()&(1<<(uint(i)&31)) != 0
}

func (v *vbits) set(off int64) {
	i := off >> 3
	if i>>5 < int64(len(v.w)) {
		v.w[i>>5].Or(1 << (uint(i) & 31))
	}
}

func (l *Log) vbitsOf(seg uint32) *vbits {
	if v, ok := l.verified.Load(seg); ok {
		return v.(*vbits)
	}
	v := &vbits{w: make([]atomic.Uint32, (l.segMax>>3+31)>>5)}
	a, _ := l.verified.LoadOrStore(seg, v)
	return a.(*vbits)
}

// InvalidateVerified forgets which frames of seg were verified, so that
// every read of seg checks its frame again.
func (l *Log) InvalidateVerified(seg uint32) { l.verified.Delete(seg) }

// SetReadVerify turns the read-time check on or off (default on). Off is
// for measuring its cost only.
func (l *Log) SetReadVerify(on bool) { l.noVerify.Store(!on) }

var framePool = sync.Pool{New: func() any { b := make([]byte, 0, 64<<10); return &b }}

var lastCorruptLog atomic.Int64

func (l *Log) corrupt(seg uint32, off int64, why string) error {
	ReadCRCErrors.Add(1)
	err := &CorruptError{Seg: seg, Off: off, Why: why}
	if l.Logf != nil {
		now := time.Now().Unix()
		if last := lastCorruptLog.Load(); now != last && lastCorruptLog.CompareAndSwap(last, now) {
			l.Logf("read: %v", err)
		}
	}
	return err
}

// crcMapped checksums b, a slice of a mapping. A disk error under a mapped
// page raises SIGBUS: it becomes ok=false and the caller rereads with pread.
func crcMapped(b []byte) (sum uint32, ok bool) {
	old := debug.SetPanicOnFault(true)
	defer func() {
		debug.SetPanicOnFault(old)
		if recover() != nil {
			ok = false
		}
	}()
	return crc32.Checksum(b, crcTable), true
}

// verifyFrame checks the frame framed at (seg, off) against its CRC unless
// that was done already. m is the segment mapping with a reference held
// by the caller, or nil.
func (l *Log) verifyFrame(seg uint32, off int64, m *segMap) error {
	if l.noVerify.Load() {
		return nil
	}
	vb := l.vbitsOf(seg)
	if vb.has(off) {
		return nil
	}
	if m != nil && off >= 0 && off+frameHdr <= int64(len(m.data)) {
		var hdr [frameHdr]byte
		if copyMapped(hdr[:], m.data[off:off+frameHdr]) {
			n := int64(binary.LittleEndian.Uint32(hdr[:]))
			if n == 0 || n > maxRecord {
				return l.corrupt(seg, off, "invalid frame length")
			}
			if end := off + frameHdr + n; end <= int64(len(m.data)) {
				if sum, ok := crcMapped(m.data[off+frameHdr : end]); ok {
					if sum != binary.LittleEndian.Uint32(hdr[4:]) {
						return l.corrupt(seg, off, "record CRC32C mismatch")
					}
					vb.set(off)
					return nil
				}
			}
		}
	}
	var hdr [frameHdr]byte
	if _, err := l.ReadAt(seg, off, hdr[:]); err != nil {
		return err
	}
	n := int64(binary.LittleEndian.Uint32(hdr[:]))
	if n == 0 || n > maxRecord {
		return l.corrupt(seg, off, "invalid frame length")
	}
	bp := framePool.Get().(*[]byte)
	defer func() {
		if cap(*bp) <= 1<<20 {
			framePool.Put(bp)
		}
	}()
	if int64(cap(*bp)) < n {
		*bp = make([]byte, n)
	}
	buf := (*bp)[:n]
	if _, err := l.ReadAt(seg, off+frameHdr, buf); err != nil {
		return err
	}
	if crc32.Checksum(buf, crcTable) != binary.LittleEndian.Uint32(hdr[4:]) {
		return l.corrupt(seg, off, "record CRC32C mismatch")
	}
	vb.set(off)
	return nil
}

// VerifyFrame checks the frame at (seg, off) as a read would.
func (l *Log) VerifyFrame(seg uint32, off int64) error {
	if l.noVerify.Load() {
		return nil
	}
	m := l.mapping(seg)
	if m != nil {
		defer m.release()
	}
	return l.verifyFrame(seg, off, m)
}

// ReadValue reads len(p) bytes of a value at valOff of the frame at
// (loc.Seg, loc.Off), after verifying that frame.
func (l *Log) ReadValue(loc Loc, valOff int64, p []byte) error {
	if !l.noVerify.Load() && !l.vbitsOf(loc.Seg).has(int64(loc.Off)) {
		if err := l.VerifyFrame(loc.Seg, int64(loc.Off)); err != nil {
			return err
		}
	}
	_, err := l.ReadAt(loc.Seg, valOff, p)
	return err
}

// ReadValue is Log.ReadValue through the reader cached mapping.
func (r *SegReader) ReadValue(loc Loc, valOff int64, p []byte) error {
	if !r.l.noVerify.Load() && !r.l.vbitsOf(loc.Seg).has(int64(loc.Off)) {
		if r.m == nil || r.seg != loc.Seg {
			r.Close()
			r.m, r.seg = r.l.mapping(loc.Seg), loc.Seg
		}
		if err := r.l.verifyFrame(loc.Seg, int64(loc.Off), r.m); err != nil {
			return err
		}
	}
	_, err := r.ReadAt(loc.Seg, valOff, p)
	return err
}
