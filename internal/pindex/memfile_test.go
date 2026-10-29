package pindex

import (
	"errors"
	"io"
	"math/rand"
	"sync"
)

var errInjected = errors.New("injected I/O failure")

type wr struct {
	off  int64
	data []byte
}

// memFile models a disk with a volatile write cache: writes are visible to
// reads at once but only durable after Sync. crash() returns the file as it
// could look after power loss: everything synced, plus any subset of the
// unsynced writes, each possibly torn at a 512-byte sector boundary.
type memFile struct {
	mu     sync.Mutex
	synced []byte
	cur    []byte
	pend   []wr
	failAt int // operations left before every write/sync fails; <0 = never
}

func newMemFile() *memFile { return &memFile{failAt: -1} }

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off >= int64(len(m.cur)) {
		return 0, io.EOF
	}
	n := copy(p, m.cur[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func apply(b []byte, off int64, d []byte) []byte {
	if end := int(off) + len(d); end > len(b) {
		b = append(b, make([]byte, end-len(b))...)
	}
	copy(b[off:], d)
	return b
}

func (m *memFile) tick() error {
	if m.failAt == 0 {
		return errInjected
	}
	if m.failAt > 0 {
		m.failAt--
	}
	return nil
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.tick(); err != nil {
		return 0, err
	}
	d := append([]byte(nil), p...)
	m.pend = append(m.pend, wr{off, d})
	m.cur = apply(m.cur, off, d)
	return len(p), nil
}

func (m *memFile) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.tick(); err != nil {
		return err
	}
	for _, w := range m.pend {
		m.synced = apply(m.synced, w.off, w.data)
	}
	m.pend = nil
	return nil
}

func (m *memFile) Close() error { return nil }
func (m *memFile) Size() (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.cur)), nil
}

func (m *memFile) crash(rng *rand.Rand) *memFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := append([]byte(nil), m.synced...)
	for _, w := range m.pend {
		switch rng.Intn(3) {
		case 0: // lost
		case 1:
			b = apply(b, w.off, w.data)
		case 2: // torn: a random subset of 512-byte sectors landed
			for s := 0; s < len(w.data); s += 512 {
				if rng.Intn(2) == 0 {
					b = apply(b, w.off+int64(s), w.data[s:min(s+512, len(w.data))])
				}
			}
		}
	}
	return &memFile{synced: b, cur: append([]byte(nil), b...), failAt: -1}
}
