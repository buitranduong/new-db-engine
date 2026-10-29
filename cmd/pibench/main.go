// pibench runs the P3.1 workload against pindex.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"stask/internal/pindex"
	"stask/internal/pindex/bench"
)

type pt struct {
	t    *pindex.Tree
	path string
	snap pindex.Snapshot
}

func (p *pt) Put(k, v []byte) error { return p.t.Put(k, v) }
func (p *pt) Get(k []byte) (bool, error) {
	var buf [64]byte
	_, ok, err := p.t.Get(k, buf[:0])
	return ok, err
}
func (p *pt) Scan(from []byte, max int) (int, error) {
	s, err := p.t.Snapshot()
	if err != nil {
		return 0, err
	}
	defer s.Close()
	it := s.Seek(from)
	n := 0
	for n < max && it.Next() {
		n++
	}
	return n, it.Err()
}
func (p *pt) Checkpoint() error { return p.t.Checkpoint([]byte("bench")) }
func (p *pt) Info() string {
	s := p.t.Stats()
	fi, _ := os.Stat(p.path)
	var sz int64
	if fi != nil {
		sz = fi.Size()
	}
	return fmt.Sprintf("pindex: file %d MB, %d pages (free %d, pending %d), cache %d frames, misses %d, evictions %d, writebacks %d",
		sz>>20, s.FilePages, s.FreePages, s.PendingPages, s.CacheFrames, s.Misses, s.Evictions, s.Writebacks)
}
func (p *pt) Close() error { err := p.t.Close(); os.Remove(p.path); return err }

func main() {
	n := flag.Int("n", 20_000_000, "keys")
	cacheMB := flag.Int64("cache-mb", 1024, "buffer pool size")
	reads := flag.Int("reads", 2_000_000, "point reads per phase")
	scans := flag.Int("scans", 50_000, "short scans")
	par := flag.Int("par", 4, "parallel readers")
	dir := flag.String("dir", os.TempDir(), "directory for the index file")
	mode := flag.String("mode", "both", "seq, rand or both")
	ckpt := flag.Int("ckpt-each", 0, "checkpoint every N inserts")
	flag.Parse()
	cfg := bench.Config{N: *n, Reads: *reads, Scans: *scans, Parallel: *par, CkptEach: *ckpt,
		Out: func(s string) { fmt.Println(s) }}
	fmt.Printf("pindex n=%d cache=%d MB GOMAXPROCS=%d\n", *n, *cacheMB, runtime.GOMAXPROCS(0))
	mk := func() (bench.Target, error) {
		path := filepath.Join(*dir, fmt.Sprintf("pindex-bench-%d.db", os.Getpid()))
		os.Remove(path)
		t, err := pindex.Open(path, pindex.Options{CacheBytes: *cacheMB << 20})
		if err != nil {
			return nil, err
		}
		return &pt{t: t, path: path}, nil
	}
	if *mode == "seq" || *mode == "both" {
		bench.RunSeq(cfg, mk)
	}
	if *mode == "rand" || *mode == "both" {
		bench.RunRand(cfg, mk)
	}
}
