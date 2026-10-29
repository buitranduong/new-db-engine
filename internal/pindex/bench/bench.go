// Package bench is the shared workload of the P3.1 comparison: the same
// phases run against pindex and against the in-RAM index of internal/core, so
// the numbers differ only by the index.
package bench

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Target is an ordered index under test. Keys are 12 bytes, values 28 bytes
// (the size of a head-version entry without its key).
type Target interface {
	Put(k, v []byte) error
	Get(k []byte) (bool, error)
	// Scan walks up to max keys from the first key >= from.
	Scan(from []byte, max int) (int, error)
	Checkpoint() error // no-op for the RAM index
	Info() string
	Close() error
}

type Config struct {
	N        int // keys
	Reads    int // point reads per read phase
	Scans    int // short scans (100 keys each)
	Parallel int // goroutines for the parallel read phase
	CkptEach int // checkpoint every this many inserts (0 = only at the end)
	Out      func(string)
}

func SeqKey(i uint64) []byte {
	var k [12]byte
	binary.BigEndian.PutUint64(k[:], i)
	return k[:]
}

func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

// RandKey is a bijective scramble of i, so the keys arrive in random order
// but are all distinct.
func RandKey(i uint64) []byte {
	var k [12]byte
	binary.BigEndian.PutUint64(k[:], mix(i))
	binary.BigEndian.PutUint32(k[8:], uint32(i))
	return k[:]
}

func rss() (cur, peak int64) {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		var v int64
		if strings.HasPrefix(l, "VmRSS:") {
			fmt.Sscanf(l, "VmRSS: %d kB", &v)
			cur = v << 10
		} else if strings.HasPrefix(l, "VmHWM:") {
			fmt.Sscanf(l, "VmHWM: %d kB", &v)
			peak = v << 10
		}
	}
	return
}

func phase(cfg Config, name string, n int, f func() error) {
	t0 := time.Now()
	if err := f(); err != nil {
		cfg.Out(fmt.Sprintf("%-28s ERROR %v", name, err))
		return
	}
	d := time.Since(t0)
	r, p := rss()
	cfg.Out(fmt.Sprintf("%-28s %9d ops  %8.2f s  %10.0f ops/s  %7.0f ns/op   rss %5d MB (peak %5d)",
		name, n, d.Seconds(), float64(n)/d.Seconds(), float64(d.Nanoseconds())/float64(max(n, 1)), r>>20, p>>20))
}

func load(cfg Config, t Target, key func(uint64) []byte) error {
	v := make([]byte, 28)
	for i := 0; i < cfg.N; i++ {
		binary.LittleEndian.PutUint64(v, uint64(i))
		if err := t.Put(key(uint64(i)), v); err != nil {
			return err
		}
		if cfg.CkptEach > 0 && (i+1)%cfg.CkptEach == 0 {
			if err := t.Checkpoint(); err != nil {
				return err
			}
		}
	}
	return t.Checkpoint()
}

func reads(cfg Config, t Target, seed int64, span int) error {
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < cfg.Reads; i++ {
		ok, err := t.Get(SeqKey(uint64(rng.Intn(span))))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("missing key")
		}
	}
	return nil
}

// RunSeq loads keys in ascending order, then measures reads (hot: a
// contiguous tenth of the key space that fits the cache; cold: uniform over
// all keys), parallel hot reads, short scans and a full scan.
func RunSeq(cfg Config, mk func() (Target, error)) {
	t, err := mk()
	if err != nil {
		cfg.Out("open: " + err.Error())
		return
	}
	defer t.Close()
	phase(cfg, "insert sequential", cfg.N, func() error { return load(cfg, t, SeqKey) })
	cfg.Out("  " + t.Info())
	hot := cfg.N / 10
	phase(cfg, "get hot (10% range) warm-up", cfg.Reads, func() error { return reads(cfg, t, 1, hot) })
	phase(cfg, "get hot (10% range)", cfg.Reads, func() error { return reads(cfg, t, 2, hot) })
	phase(cfg, "get cold (uniform)", cfg.Reads, func() error { return reads(cfg, t, 3, cfg.N) })
	phase(cfg, fmt.Sprintf("get hot x%d goroutines", cfg.Parallel), cfg.Reads*cfg.Parallel, func() error {
		var wg sync.WaitGroup
		errs := make(chan error, cfg.Parallel)
		for g := 0; g < cfg.Parallel; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				if err := reads(cfg, t, int64(10+g), hot); err != nil {
					errs <- err
				}
			}(g)
		}
		wg.Wait()
		close(errs)
		return <-errs
	})
	phase(cfg, "scan 100 keys (random start)", cfg.Scans*100, func() error {
		rng := rand.New(rand.NewSource(5))
		for i := 0; i < cfg.Scans; i++ {
			n, err := t.Scan(SeqKey(uint64(rng.Intn(cfg.N-200))), 100)
			if err != nil || n != 100 {
				return fmt.Errorf("scan got %d: %v", n, err)
			}
		}
		return nil
	})
	phase(cfg, "scan all", cfg.N, func() error {
		n, err := t.Scan(nil, cfg.N+1)
		if err == nil && n != cfg.N {
			err = fmt.Errorf("scan all got %d, want %d", n, cfg.N)
		}
		return err
	})
	cfg.Out("  " + t.Info())
}

// RunRand loads the keys in random order and reads them back.
func RunRand(cfg Config, mk func() (Target, error)) {
	t, err := mk()
	if err != nil {
		cfg.Out("open: " + err.Error())
		return
	}
	defer t.Close()
	phase(cfg, "insert random", cfg.N, func() error { return load(cfg, t, RandKey) })
	cfg.Out("  " + t.Info())
	phase(cfg, "get random (uniform)", cfg.Reads, func() error {
		rng := rand.New(rand.NewSource(7))
		for i := 0; i < cfg.Reads; i++ {
			ok, err := t.Get(RandKey(uint64(rng.Intn(cfg.N))))
			if err != nil || !ok {
				return fmt.Errorf("get: %v %v", ok, err)
			}
		}
		return nil
	})
	runtime.GC()
	cfg.Out("  " + t.Info())
}
