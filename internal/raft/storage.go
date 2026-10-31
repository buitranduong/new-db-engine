package raft

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// fdatasync flushes the data and the metadata needed to read it back (the
// size), not the timestamps: cheaper than fsync on a file that only grows.
func fdatasync(f *os.File) error { return syscall.Fdatasync(int(f.Fd())) }

// MemStorage keeps everything in memory. In a test, dropping a Node and
// building a new one on the same MemStorage models a process restart.
type MemStorage struct {
	mu   sync.Mutex
	hs   HardState
	snap Snapshot
	ents []Entry
}

// compactEntries applies Storage.Compact to a slice of entries.
func compactEntries(ents []Entry, base *Snapshot, snap Snapshot) []Entry {
	if snap.Index <= base.Index {
		return ents
	}
	var rest []Entry
	if k := snap.Index - base.Index; k < uint64(len(ents)) {
		rest = append(rest, ents[k:]...)
	}
	*base = snap
	return rest
}

func (s *MemStorage) Load() (HardState, Snapshot, []Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hs, s.snap, append([]Entry(nil), s.ents...), nil
}

func (s *MemStorage) Reset(snap Snapshot) error {
	s.mu.Lock()
	s.snap, s.ents = snap, nil
	s.mu.Unlock()
	return nil
}

func (s *MemStorage) Compact(snap Snapshot) error {
	s.mu.Lock()
	s.ents = compactEntries(s.ents, &s.snap, snap)
	s.mu.Unlock()
	return nil
}

func (s *MemStorage) SetHardState(h HardState) error {
	s.mu.Lock()
	s.hs = h
	s.mu.Unlock()
	return nil
}

func (s *MemStorage) Append(es []Entry) error {
	if len(es) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if es[0].Index <= s.snap.Index || es[0].Index > s.snap.Index+uint64(len(s.ents))+1 {
		return fmt.Errorf("raft: append at %d leaves a gap (base %d, have %d)", es[0].Index, s.snap.Index, len(s.ents))
	}
	s.ents = append(s.ents[:es[0].Index-s.snap.Index-1], es...)
	return nil
}

// FileStorage is a durable Storage: one append-only file of records, each
// [length u32][crc32 u32][type u8][payload], fsynced before the call
// returns. A torn or corrupt tail (a crash in mid-write) is cut off when
// the file is opened; a bad record before the tail is an error.
type FileStorage struct {
	mu   sync.Mutex
	f    *os.File
	path string
	hs   HardState
	snap Snapshot
	ents []Entry
	nofs bool // test hook: skip fsync
}

const (
	recHard   = 1
	recAppend = 2
	recSnap   = 3 // [type][index u64][term u64]: the log is compacted to here
	// recVoters [type][n u32][id u64 ...] follows a recSnap and gives the
	// configuration at the snapshot's index. Added with membership change;
	// a log without it (written before) uses the node's initial peers.
	recVoters = 4
)

func OpenFileStorage(path string) (*FileStorage, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	s := &FileStorage{f: f, path: path}
	r := bufio.NewReader(f)
	var good int64
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			break
		}
		n := binary.LittleEndian.Uint32(hdr[:4])
		if n == 0 || n > 1<<30 {
			break
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil || crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(hdr[4:]) {
			break
		}
		if err := s.replay(body); err != nil {
			f.Close()
			return nil, err
		}
		good += 8 + int64(n)
	}
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func (s *FileStorage) replay(b []byte) error {
	switch b[0] {
	case recHard:
		if len(b) != 17 {
			return errors.New("raft: bad hard-state record")
		}
		s.hs = HardState{Term: binary.LittleEndian.Uint64(b[1:]), Vote: binary.LittleEndian.Uint64(b[9:])}
	case recAppend:
		es, err := decodeEntries(b[1:])
		if err != nil || len(es) == 0 || es[0].Index <= s.snap.Index || es[0].Index > s.snap.Index+uint64(len(s.ents))+1 {
			return errors.New("raft: bad append record")
		}
		s.ents = append(s.ents[:es[0].Index-s.snap.Index-1], es...)
	case recSnap:
		if len(b) != 17 {
			return errors.New("raft: bad snapshot record")
		}
		s.ents = compactEntries(s.ents, &s.snap, Snapshot{Index: binary.LittleEndian.Uint64(b[1:]), Term: binary.LittleEndian.Uint64(b[9:])})
	case recVoters:
		if len(b) < 5 || uint64(len(b)) != 5+8*uint64(binary.LittleEndian.Uint32(b[1:])) {
			return errors.New("raft: bad voters record")
		}
		s.snap.Voters = nil
		for i := 5; i < len(b); i += 8 {
			s.snap.Voters = append(s.snap.Voters, binary.LittleEndian.Uint64(b[i:]))
		}
	default:
		return errors.New("raft: unknown record")
	}
	return nil
}

func encodeEntries(es []Entry) []byte {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, uint32(len(es)))
	for _, e := range es {
		b = binary.LittleEndian.AppendUint64(b, e.Index)
		b = binary.LittleEndian.AppendUint64(b, e.Term)
		fl := byte(0)
		if e.Noop {
			fl = 1
		}
		if e.Conf {
			fl |= 2
		}
		b = append(b, fl)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(e.Data)))
		b = append(b, e.Data...)
	}
	return b
}

func decodeEntries(b []byte) ([]Entry, error) {
	bad := errors.New("raft: bad entries")
	if len(b) < 4 {
		return nil, bad
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	var es []Entry
	for i := uint32(0); i < n; i++ {
		if len(b) < 21 {
			return nil, bad
		}
		e := Entry{Index: binary.LittleEndian.Uint64(b), Term: binary.LittleEndian.Uint64(b[8:]), Noop: b[16]&1 == 1, Conf: b[16]&2 != 0}
		l := binary.LittleEndian.Uint32(b[17:])
		b = b[21:]
		if uint32(len(b)) < l {
			return nil, bad
		}
		if l > 0 {
			e.Data = append([]byte(nil), b[:l]...)
		}
		b = b[l:]
		es = append(es, e)
	}
	return es, nil
}

func (s *FileStorage) write(body []byte) error {
	buf := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint32(buf, uint32(len(body)))
	binary.LittleEndian.PutUint32(buf[4:], crc32.ChecksumIEEE(body))
	buf = append(buf, body...)
	if _, err := s.f.Write(buf); err != nil {
		return err
	}
	if s.nofs {
		return nil
	}
	return fdatasync(s.f)
}

func (s *FileStorage) Load() (HardState, Snapshot, []Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hs, s.snap, append([]Entry(nil), s.ents...), nil
}

// Compact rewrites the file: a new file with the snapshot record, the hard
// state and the entries that remain, synced and renamed over the old one
// (a crash leaves either file whole).
func (s *FileStorage) Compact(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap.Index <= s.snap.Index {
		return nil
	}
	base := s.snap
	rest := compactEntries(append([]Entry(nil), s.ents...), &base, snap)
	return s.rewrite(snap, rest)
}

// Reset drops the whole log and makes snap its base (a snapshot installed
// over a log that may disagree with it).
func (s *FileStorage) Reset(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewrite(snap, nil)
}

// rewrite replaces the file by one holding snap, the hard state and rest.
func (s *FileStorage) rewrite(snap Snapshot, rest []Entry) error {
	tmp := s.path + ".new"
	nf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	old := s.f
	s.f = nf
	rec := func(body []byte) error {
		buf := make([]byte, 8, 8+len(body))
		binary.LittleEndian.PutUint32(buf, uint32(len(body)))
		binary.LittleEndian.PutUint32(buf[4:], crc32.ChecksumIEEE(body))
		_, err := nf.Write(append(buf, body...))
		return err
	}
	sb := []byte{recSnap}
	sb = binary.LittleEndian.AppendUint64(sb, snap.Index)
	sb = binary.LittleEndian.AppendUint64(sb, snap.Term)
	hb := []byte{recHard}
	hb = binary.LittleEndian.AppendUint64(hb, s.hs.Term)
	hb = binary.LittleEndian.AppendUint64(hb, s.hs.Vote)
	err = rec(sb)
	if err == nil && len(snap.Voters) > 0 {
		vb := binary.LittleEndian.AppendUint32([]byte{recVoters}, uint32(len(snap.Voters)))
		for _, v := range snap.Voters {
			vb = binary.LittleEndian.AppendUint64(vb, v)
		}
		err = rec(vb)
	}
	if err == nil {
		err = rec(hb)
	}
	for i := 0; err == nil && i < len(rest); i += 256 {
		err = rec(append([]byte{recAppend}, encodeEntries(rest[i:min(i+256, len(rest))])...))
	}
	if err == nil && !s.nofs {
		err = nf.Sync()
	}
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err == nil && !s.nofs {
		if d, derr := os.Open(filepath.Dir(s.path)); derr == nil {
			err = d.Sync()
			d.Close()
		}
	}
	if err != nil {
		s.f = old
		nf.Close()
		os.Remove(tmp)
		return err
	}
	old.Close()
	s.snap, s.ents = snap, rest
	return nil
}

func (s *FileStorage) SetHardState(h HardState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := []byte{recHard}
	b = binary.LittleEndian.AppendUint64(b, h.Term)
	b = binary.LittleEndian.AppendUint64(b, h.Vote)
	if err := s.write(b); err != nil {
		return err
	}
	s.hs = h
	return nil
}

func (s *FileStorage) Append(es []Entry) error {
	if len(es) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if es[0].Index <= s.snap.Index || es[0].Index > s.snap.Index+uint64(len(s.ents))+1 {
		return fmt.Errorf("raft: append at %d leaves a gap (base %d, have %d)", es[0].Index, s.snap.Index, len(s.ents))
	}
	if err := s.write(append([]byte{recAppend}, encodeEntries(es)...)); err != nil {
		return err
	}
	s.ents = append(s.ents[:es[0].Index-s.snap.Index-1], es...)
	return nil
}

func (s *FileStorage) Close() error { return s.f.Close() }

// Resettable is a Storage that can start over from a snapshot.
type Resettable interface {
	Storage
	Reset(Snapshot) error
}

// InstallSnapshot makes snap the base of st. If the saved log has entry
// snap.Index with snap.Term, the entries after it stay (they follow the
// snapshot); otherwise the log is dropped.
func InstallSnapshot(st Resettable, snap Snapshot) error {
	_, base, ents, err := st.Load()
	if err != nil {
		return err
	}
	if snap.Index <= base.Index {
		return nil
	}
	if k := snap.Index - base.Index; k <= uint64(len(ents)) && ents[k-1].Term == snap.Term {
		return st.Compact(snap)
	}
	return st.Reset(snap)
}
