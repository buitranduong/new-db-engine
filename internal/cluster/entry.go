package cluster

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"sync"

	"stask/internal/wal"
)

// A Raft entry carries one committed batch of the leader's log: the bytes
// that went into its log at positions (seg, off), exactly as written, so a
// follower can hand them to Engine.Apply and have a byte-identical log.
//
//	[u8 version=1][u64 incarnation][u64 lastLSN][u32 chunks]
//	chunk: [u32 seg][u64 off][u32 len][bytes]
//
// A batch lies in one segment except when it is the first after a roll, so
// it has one chunk, or two when the rest of the previous segment goes along.
// incarnation names the process that proposed the entry: a leader already
// has its own entries in its engine.

type chunk struct {
	pos  wal.Pos
	data []byte
}

type batch struct {
	inc     uint64
	lastLSN uint64
	chunks  []chunk
}

func (b *batch) end() wal.Pos {
	c := b.chunks[len(b.chunks)-1]
	return wal.Pos{Seg: c.pos.Seg, Off: c.pos.Off + int64(len(c.data))}
}

func encodeBatch(b *batch) []byte {
	n := 21
	for _, c := range b.chunks {
		n += 16 + len(c.data)
	}
	out := make([]byte, 0, n)
	out = append(out, 1)
	out = binary.LittleEndian.AppendUint64(out, b.inc)
	out = binary.LittleEndian.AppendUint64(out, b.lastLSN)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.chunks)))
	for _, c := range b.chunks {
		out = binary.LittleEndian.AppendUint32(out, c.pos.Seg)
		out = binary.LittleEndian.AppendUint64(out, uint64(c.pos.Off))
		out = binary.LittleEndian.AppendUint32(out, uint32(len(c.data)))
		out = append(out, c.data...)
	}
	return out
}

var errBadBatch = errors.New("cluster: malformed batch entry")

func decodeBatch(d []byte) (*batch, error) {
	if len(d) < 21 || d[0] != 1 {
		return nil, errBadBatch
	}
	b := &batch{inc: binary.LittleEndian.Uint64(d[1:]), lastLSN: binary.LittleEndian.Uint64(d[9:])}
	n := binary.LittleEndian.Uint32(d[17:])
	d = d[21:]
	for i := uint32(0); i < n; i++ {
		if len(d) < 16 {
			return nil, errBadBatch
		}
		c := chunk{pos: wal.Pos{Seg: binary.LittleEndian.Uint32(d), Off: int64(binary.LittleEndian.Uint64(d[4:]))}}
		l := binary.LittleEndian.Uint32(d[12:])
		d = d[16:]
		if uint32(len(d)) < l {
			return nil, errBadBatch
		}
		c.data = d[:l:l]
		d = d[l:]
		b.chunks = append(b.chunks, c)
	}
	if len(b.chunks) == 0 {
		return nil, errBadBatch
	}
	return b, nil
}

// applied is the state machine's position in the Raft log: entry Index is
// applied and its batch ends at (LSN, Pos). It is written after the
// engine has made the batch durable, without fsync: an older value after a
// crash only makes the node cut its engine log shorter and apply a few
// entries again (they are in the Raft log, which compaction never trims
// beyond a synced applied record).
type applied struct {
	Index uint64
	LSN   uint64
	Pos   wal.Pos
}

// appliedFile keeps the two most recent records in two 32-byte slots, each
// with a checksum, so a torn write leaves the other one intact.
type appliedFile struct {
	mu   sync.Mutex
	f    *os.File
	next int // slot of the next write: they alternate, so the newest synced record survives a torn write
}

const slotSize = 32

func openApplied(path string) (*appliedFile, applied, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, applied{}, err
	}
	var buf [2 * slotSize]byte
	n, _ := f.ReadAt(buf[:], 0)
	var best applied
	bestSlot := 1
	for i := 0; i < 2 && (i+1)*slotSize <= n; i++ {
		s := buf[i*slotSize : (i+1)*slotSize]
		if crc32.ChecksumIEEE(s[:28]) != binary.LittleEndian.Uint32(s[28:]) {
			continue
		}
		a := applied{binary.LittleEndian.Uint64(s), binary.LittleEndian.Uint64(s[8:]),
			wal.Pos{Seg: binary.LittleEndian.Uint32(s[16:]), Off: int64(binary.LittleEndian.Uint64(s[20:]))}}
		if a.Index > best.Index || a.Index == best.Index && a.LSN > best.LSN {
			best, bestSlot = a, i
		}
	}
	return &appliedFile{f: f, next: 1 - bestSlot}, best, nil
}

func (af *appliedFile) write(a applied, sync bool) error {
	var s [slotSize]byte
	binary.LittleEndian.PutUint64(s[:], a.Index)
	binary.LittleEndian.PutUint64(s[8:], a.LSN)
	binary.LittleEndian.PutUint32(s[16:], a.Pos.Seg)
	binary.LittleEndian.PutUint64(s[20:], uint64(a.Pos.Off))
	// bytes 20..28 hold Off, so the checksum covers [0,28)
	binary.LittleEndian.PutUint32(s[28:], crc32.ChecksumIEEE(s[:28]))
	af.mu.Lock()
	defer af.mu.Unlock()
	if _, err := af.f.WriteAt(s[:], int64(af.next)*slotSize); err != nil {
		return err
	}
	af.next = 1 - af.next
	if sync {
		return af.f.Sync()
	}
	return nil
}

func (af *appliedFile) Close() error { return af.f.Close() }
