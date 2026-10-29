// fsyncbench measures durable-write latency of the log disk for several
// write strategies and levels of concurrency.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const fileSize = 256 << 20

func prepare(path string, zero bool) {
	os.Remove(path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, fileSize); err != nil {
		log.Fatal(err)
	}
	if zero {
		buf := make([]byte, 1<<20)
		for off := int64(0); off < fileSize; off += int64(len(buf)) {
			f.WriteAt(buf, off)
		}
	}
	f.Sync()
	f.Close()
}

func aligned(n int) []byte {
	b := make([]byte, n+4096)
	off := 4096 - int(uintptr(unsafe.Pointer(&b[0]))&4095)
	return b[off : off+n]
}

type result struct {
	ops      int
	p50, p99 time.Duration
}

// run: conc goroutines each append size-byte writes to their own region
// and make each durable according to mode.
func run(path, mode string, conc, size int, dur time.Duration) result {
	flags := os.O_RDWR
	switch mode {
	case "dsync":
		flags |= syscall.O_DSYNC
	case "direct":
		flags |= syscall.O_DSYNC | syscall.O_DIRECT
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	var mu sync.Mutex
	var lats []time.Duration
	var wg sync.WaitGroup
	deadline := time.Now().Add(dur)
	region := int64(fileSize / conc)
	for g := 0; g < conc; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			wsize := size
			if mode == "direct" {
				wsize = (size + 4095) &^ 4095
			}
			buf := aligned(wsize)
			for i := range buf {
				buf[i] = byte(i)
			}
			base := int64(g) * region
			off := int64(0)
			var mine []time.Duration
			for time.Now().Before(deadline) {
				if off+int64(wsize) > region {
					off = 0
				}
				t := time.Now()
				if _, err := f.WriteAt(buf, base+off); err != nil {
					log.Fatal(err)
				}
				if mode == "fdatasync" {
					if err := syscall.Fdatasync(int(f.Fd())); err != nil {
						log.Fatal(err)
					}
				}
				mine = append(mine, time.Since(t))
				if mode == "direct" {
					off += int64(wsize)
				} else {
					off += int64(size)
				}
			}
			mu.Lock()
			lats = append(lats, mine...)
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	return result{len(lats), lats[len(lats)/2], lats[len(lats)*99/100]}
}

func main() {
	dir := flag.String("dir", ".", "directory on the log disk")
	dur := flag.Duration("dur", 3*time.Second, "per test")
	size := flag.Int("size", 512, "bytes per durable write")
	flag.Parse()
	path := filepath.Join(*dir, "fsyncbench.dat")
	defer os.Remove(path)
	fmt.Printf("%-28s %5s %9s %9s %10s\n", "case", "conc", "p50", "p99", "writes/s")
	for _, c := range []struct {
		name, mode string
		zero       bool
	}{
		{"fdatasync, fallocated", "fdatasync", false},
		{"fdatasync, pre-zeroed", "fdatasync", true},
		{"O_DSYNC, pre-zeroed", "dsync", true},
		{"O_DIRECT|O_DSYNC, pre-zeroed", "direct", true},
	} {
		for _, conc := range []int{1, 2, 4, 8, 16} {
			prepare(path, c.zero)
			r := run(path, c.mode, conc, *size, *dur)
			fmt.Printf("%-28s %5d %9s %9s %10.0f\n", c.name, conc, r.p50.Round(time.Microsecond), r.p99.Round(time.Microsecond), float64(r.ops)/dur.Seconds())
		}
	}
}
