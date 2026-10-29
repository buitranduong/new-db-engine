package spill

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

const (
	bufSize   = 128 << 10
	pollEvery = 2048
)

// Run is a finished temp file of records. It can be read any number of
// times, each by its own Reader, until Close.
type Run struct {
	m      *Manager
	f      *os.File
	size   int64
	n      int64
	closed bool
}

// Records is the number of records in the run.
func (r *Run) Records() int64 { return r.n }

// Close releases the file and its share of the disk quota.
func (r *Run) Close() {
	r.m.mu.Lock()
	if r.closed {
		r.m.mu.Unlock()
		return
	}
	r.closed = true
	delete(r.m.open, r)
	r.m.used -= r.size
	r.m.mu.Unlock()
	r.f.Close()
}

// Writer appends records to a new temp file.
type Writer struct {
	m   *Manager
	run *Run
	bw  *bufio.Writer
	hdr [binary.MaxVarintLen64 + 4]byte
	err error
}

// Create opens a new temp file.
func (m *Manager) Create() (*Writer, error) {
	m.mu.Lock()
	dir, err := m.dirLocked()
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "run-*")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name()) // gone on any exit; the open handle keeps the data
	r := &Run{m: m, f: f}
	m.mu.Lock()
	m.open[r] = struct{}{}
	m.mu.Unlock()
	stats.files.Add(1)
	return &Writer{m: m, run: r, bw: bufio.NewWriterSize(f, bufSize)}, nil
}

// Write appends one record.
func (w *Writer) Write(rec []byte) error {
	if w.err != nil {
		return w.err
	}
	if w.m.ticks.Add(1)%pollEvery == 0 {
		if w.err = w.m.poll(); w.err != nil {
			return w.err
		}
	}
	n := binary.PutUvarint(w.hdr[:], uint64(len(rec)))
	binary.LittleEndian.PutUint32(w.hdr[n:], crc32.Checksum(rec, castagnoli))
	total := int64(n + 4 + len(rec))
	if w.err = w.m.charge(total); w.err != nil {
		return w.err
	}
	w.run.size += total
	stats.bytes.Add(total)
	if _, w.err = w.bw.Write(w.hdr[:n+4]); w.err != nil {
		return w.err
	}
	_, w.err = w.bw.Write(rec)
	w.run.n++
	return w.err
}

// Finish flushes and returns the run. After a failed Write, Finish
// releases the file and returns that error.
func (w *Writer) Finish() (*Run, error) {
	if w.err == nil {
		w.err = w.bw.Flush()
	}
	if w.err != nil {
		w.run.Close()
		return nil, w.err
	}
	return w.run, nil
}

// Abort drops a writer that will not be finished.
func (w *Writer) Abort() { w.run.Close() }

// Reader reads a run front to back.
type Reader struct {
	run *Run
	br  *bufio.Reader
	buf []byte
}

// Open returns a reader at the start of the run.
func (r *Run) Open() *Reader {
	return &Reader{run: r, br: bufio.NewReaderSize(io.NewSectionReader(r.f, 0, r.size), bufSize)}
}

// Next returns the next record, valid until the following call; ok=false
// at the end.
func (rd *Reader) Next() (rec []byte, ok bool, err error) {
	if rd.run.m.ticks.Add(1)%pollEvery == 0 {
		if err := rd.run.m.poll(); err != nil {
			return nil, false, err
		}
	}
	n, err := binary.ReadUvarint(rd.br)
	if err == io.EOF {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, ErrCorrupt
	}
	if n > 1<<31 {
		return nil, false, ErrCorrupt
	}
	var sum [4]byte
	if _, err := io.ReadFull(rd.br, sum[:]); err != nil {
		return nil, false, ErrCorrupt
	}
	if int(n) > cap(rd.buf) {
		rd.buf = make([]byte, n)
	}
	rd.buf = rd.buf[:n]
	if _, err := io.ReadFull(rd.br, rd.buf); err != nil {
		return nil, false, ErrCorrupt
	}
	if crc32.Checksum(rd.buf, castagnoli) != binary.LittleEndian.Uint32(sum[:]) {
		return nil, false, ErrCorrupt
	}
	return rd.buf, true, nil
}

// source pushes the decoded rows of a run.
func source[T any](r *Run, c Codec[T]) Source[T] {
	return func(yield func(T) error) error {
		rd := r.Open()
		for {
			rec, ok, err := rd.Next()
			if err != nil || !ok {
				return err
			}
			v, err := c.Dec(rec)
			if err != nil {
				return err
			}
			if err := yield(v); err != nil {
				return err
			}
		}
	}
}

// parts is a set of partition files being written.
type parts struct {
	ws  []*Writer
	buf []byte
}

func (m *Manager) newParts(n int) (*parts, error) {
	p := &parts{ws: make([]*Writer, n)}
	for i := range p.ws {
		w, err := m.Create()
		if err != nil {
			p.abort()
			return nil, err
		}
		p.ws[i] = w
	}
	return p, nil
}

func (p *parts) abort() {
	for _, w := range p.ws {
		if w != nil {
			w.Abort()
		}
	}
}

func (p *parts) add(i int, rec []byte) error { return p.ws[i].Write(rec) }

// finish returns the runs; on error all are released.
func (p *parts) finish() ([]*Run, error) {
	runs := make([]*Run, len(p.ws))
	for i, w := range p.ws {
		r, err := w.Finish()
		if err != nil {
			for _, x := range runs {
				if x != nil {
					x.Close()
				}
			}
			p.abort()
			return nil, err
		}
		runs[i] = r
	}
	return runs, nil
}

func closeRuns(rs []*Run) {
	for _, r := range rs {
		if r != nil {
			r.Close()
		}
	}
}
