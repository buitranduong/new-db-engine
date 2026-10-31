package pindex

import (
	"sync"
	"testing"
)

// slowWriteFile blocks every multi-page write (the checkpoint flusher's
// batched writes) until released; single-page writes (eviction write-back)
// pass.
type slowWriteFile struct {
	File
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *slowWriteFile) WriteAt(p []byte, off int64) (int, error) {
	if len(p) > PageSize {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.File.WriteAt(p, off)
}

// A checkpoint flush in progress must not pin the pool: while its I/O is
// slow the writer still has to find a frame to evict. (Before, flush pinned
// every dirty candidate up front, so a pool full of dirty pages of the
// checkpointed generation left the writer with ErrPoolFull.)
func TestFlushDoesNotPinWholePool(t *testing.T) {
	sf := &slowWriteFile{File: newMemFile(), entered: make(chan struct{}), release: make(chan struct{})}
	p := newPool(sf, 0) // minFrames
	n := len(p.frames)
	for id := 1; id <= n; id++ {
		f, err := p.alloc(uint64(id))
		if err != nil {
			t.Fatal(err)
		}
		initPage(f.data, pageLeaf, 1)
		f.gen.Store(1)
		f.dirty.Store(true)
		p.unpin(f)
	}
	done := make(chan error, 1)
	go func() { done <- p.flush(1) }()
	<-sf.entered
	f, err := p.alloc(uint64(n + 1))
	if err != nil {
		close(sf.release)
		<-done
		t.Fatalf("alloc while a flush is writing: %v (the flusher holds the pool pinned)", err)
	}
	p.unpin(f)
	close(sf.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
