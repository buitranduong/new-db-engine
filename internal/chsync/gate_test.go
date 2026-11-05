package chsync

import (
	"sync"
	"testing"
	"time"
)

// A reader never overlaps the writer; a reader arriving while the writer
// waits (within Patience) gets in; the writer gets in once Patience is over
// even while readers keep arriving.
func TestGate(t *testing.T) {
	g := &Gate{Patience: 50 * time.Millisecond}
	var mu sync.Mutex
	readers, writing := 0, false
	check := func() {
		mu.Lock()
		defer mu.Unlock()
		if writing && readers > 0 {
			t.Error("reader and writer overlap")
		}
	}
	g.RLock() // a long read
	mu.Lock()
	readers++
	mu.Unlock()
	wrote := make(chan struct{})
	go func() {
		g.Lock()
		mu.Lock()
		writing = true
		mu.Unlock()
		check()
		mu.Lock()
		writing = false
		mu.Unlock()
		g.Unlock()
		close(wrote)
	}()
	time.Sleep(10 * time.Millisecond)
	in := make(chan struct{})
	go func() { g.RLock(); close(in); g.RUnlock() }()
	select {
	case <-in:
	case <-time.After(30 * time.Millisecond):
		t.Fatal("a reader within patience was held up behind the waiting writer")
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // readers keep coming
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				g.RLock()
				mu.Lock()
				readers++
				mu.Unlock()
				check()
				time.Sleep(time.Millisecond)
				mu.Lock()
				readers--
				mu.Unlock()
				g.RUnlock()
			}
		}()
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	readers--
	mu.Unlock()
	g.RUnlock() // the long read ends after patience: the writer must get in now
	select {
	case <-wrote:
	case <-time.After(time.Second):
		t.Fatal("writer starved")
	}
	close(stop)
	wg.Wait()
}
