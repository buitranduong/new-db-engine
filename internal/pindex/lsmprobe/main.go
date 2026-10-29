// lsmprobe measures Pebble (LSM) on the same workload as cmd/pibench, to
// ground the B+tree vs LSM decision in docs/15. It is a separate module so
// that the engine's go.mod does not gain the dependency.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
)

func seqKey(i uint64) []byte {
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

func randKey(i uint64) []byte {
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

func phase(name string, n int, f func() error) {
	t0 := time.Now()
	if err := f(); err != nil {
		fmt.Printf("%-28s ERROR %v\n", name, err)
		return
	}
	d := time.Since(t0)
	r, p := rss()
	fmt.Printf("%-28s %9d ops  %8.2f s  %10.0f ops/s  %7.0f ns/op   rss %5d MB (peak %5d)\n",
		name, n, d.Seconds(), float64(n)/d.Seconds(), float64(d.Nanoseconds())/float64(n), r>>20, p>>20)
}

func main() {
	n := flag.Int("n", 20_000_000, "keys")
	cacheMB := flag.Int64("cache-mb", 1024, "block cache")
	reads := flag.Int("reads", 2_000_000, "")
	scans := flag.Int("scans", 50_000, "")
	dir := flag.String("dir", "/dev/shm/lsmprobe", "")
	mode := flag.String("mode", "seq", "seq or rand")
	flag.Parse()
	os.RemoveAll(*dir)
	defer os.RemoveAll(*dir)
	opts := &pebble.Options{
		Cache:                    pebble.NewCache(*cacheMB << 20),
		MemTableSize:             64 << 20,
		DisableWAL:               true, // the engine's log is the source of truth
		MaxConcurrentCompactions: func() int { return 2 },
		Levels:                   make([]pebble.LevelOptions, 7),
	}
	for i := range opts.Levels {
		opts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
	}
	opts.EnsureDefaults()
	db, err := pebble.Open(*dir, opts)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	key := seqKey
	if *mode == "rand" {
		key = randKey
	}
	fmt.Printf("pebble n=%d cache=%d MB mode=%s (WAL off, bloom 10 bits)\n", *n, *cacheMB, *mode)
	phase("insert "+*mode+" (batch 1000)", *n, func() error {
		v := make([]byte, 28)
		b := db.NewBatch()
		for i := 0; i < *n; i++ {
			binary.LittleEndian.PutUint64(v, uint64(i))
			b.Set(key(uint64(i)), v, nil)
			if (i+1)%1000 == 0 {
				if err := b.Commit(pebble.NoSync); err != nil {
					return err
				}
				b.Reset()
			}
		}
		return b.Commit(pebble.NoSync)
	})
	phase("flush", 1, func() error { return db.Flush() })
	m := db.Metrics()
	fmt.Printf("  levels: %s", "")
	for l := 0; l < 7; l++ {
		fmt.Printf("L%d=%dMB/%d ", l, m.Levels[l].Size>>20, m.Levels[l].NumFiles)
	}
	fmt.Printf(" compactions=%d write-amp(est)=%.1f\n", m.Compact.Count, func() float64 { t := m.Total(); return t.WriteAmp() }())
	get := func(span int, seed int64, kf func(uint64) []byte) func() error {
		return func() error {
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < *reads; i++ {
				_, c, err := db.Get(kf(uint64(rng.Intn(span))))
				if err != nil {
					return err
				}
				c.Close()
			}
			return nil
		}
	}
	if *mode == "seq" {
		phase("get hot (10% range) warm-up", *reads, get(*n/10, 1, seqKey))
		phase("get hot (10% range)", *reads, get(*n/10, 2, seqKey))
		phase("get cold (uniform)", *reads, get(*n, 3, seqKey))
		phase("scan 100 keys (random start)", *scans*100, func() error {
			rng := rand.New(rand.NewSource(5))
			for i := 0; i < *scans; i++ {
				it, _ := db.NewIter(nil)
				c := 0
				for ok := it.SeekGE(seqKey(uint64(rng.Intn(*n - 200)))); ok && c < 100; ok = it.Next() {
					c++
				}
				it.Close()
			}
			return nil
		})
		phase("scan all", *n, func() error {
			it, _ := db.NewIter(nil)
			c := 0
			for ok := it.First(); ok; ok = it.Next() {
				c++
			}
			it.Close()
			if c != *n {
				return fmt.Errorf("scan got %d", c)
			}
			return nil
		})
	} else {
		phase("get random (uniform)", *reads, get(*n, 7, randKey))
	}
	phase("full compaction", 1, func() error {
		return db.Compact(seqKey(0), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, true)
	})
	if *mode == "seq" {
		phase("get hot after compaction", *reads, get(*n/10, 2, seqKey))
		phase("get cold after compaction", *reads, get(*n, 3, seqKey))
	} else {
		phase("get random after compaction", *reads, get(*n, 7, randKey))
	}
}
