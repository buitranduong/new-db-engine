package core

import (
	"sync"
	"testing"
	"time"
)

func TestPromHist(t *testing.T) {
	var h PromHist
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				h.Observe(time.Duration(i%7) * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	cum, n, sum := h.Snapshot()
	if n != 8000 || cum[len(cum)-1] != 8000 || sum <= 0 {
		t.Fatalf("count %d total %d sum %v", n, cum[len(cum)-1], sum)
	}
	for i := 1; i < len(cum); i++ {
		if cum[i] < cum[i-1] {
			t.Fatal("buckets are not cumulative")
		}
	}
	h.Observe(time.Hour)
	if _, n2, _ := h.Snapshot(); n2 != 8001 {
		t.Fatal("observation above the last bound lost")
	}
	if q := h.Quantile(0.5); q < 0.001 || q > 0.005 {
		t.Fatalf("median bound %v", q)
	}
}
