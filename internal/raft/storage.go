package raft

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

// MemStorage keeps everything in memory. In a test, dropping a Node and
// building a new one on the same MemStorage models a process restart.
type MemStorage struct {
	mu   sync.Mutex
	hs   HardState
	ents []Entry
}

func (s *MemStorage) Load() (HardState, []Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hs, append([]Entry(nil), s.ents...), nil
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
	if es[0].Index == 0 || es[0].Index > uint64(len(s.ents))+1 {
		return fmt.Errorf("raft: append at %d leaves a gap (have %d)", es[0].Index, len(s.ents))
	}
	s.ents = append(s.ents[:es[0].Index-1], es...)
	return nil
}

// FileStorage is a durable Storage: one append-only file of records, each
// [length u32][crc32 u32][type u8][payload], fsynced before the call
// returns. A torn or corrupt tail (a crash in mid-write) is cut off when
// the file is opened; a bad record before the tail is an error.
type FileStorage struct {
	mu   sync.Mutex
	f    *os.File
	hs   HardState
	ents []Entry
	nofs bool // test hook: skip fsync
}

const (
	recHard   = 1
	recAppend = 2
)

func OpenFileStorage(path string) (*FileStorage, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	s := &FileStorage{f: f}
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
		if err != nil || len(es) == 0 || es[0].Index == 0 || es[0].Index > uint64(len(s.ents))+1 {
			return errors.New("raft: bad append record")
		}
		s.ents = append(s.ents[:es[0].Index-1], es...)
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
		e := Entry{Index: binary.LittleEndian.Uint64(b), Term: binary.LittleEndian.Uint64(b[8:]), Noop: b[16] == 1}
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
	return s.f.Sync()
}

func (s *FileStorage) Load() (HardState, []Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hs, append([]Entry(nil), s.ents...), nil
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
	if es[0].Index == 0 || es[0].Index > uint64(len(s.ents))+1 {
		return fmt.Errorf("raft: append at %d leaves a gap (have %d)", es[0].Index, len(s.ents))
	}
	if err := s.write(append([]byte{recAppend}, encodeEntries(es)...)); err != nil {
		return err
	}
	s.ents = append(s.ents[:es[0].Index-1], es...)
	return nil
}

func (s *FileStorage) Close() error { return s.f.Close() }
