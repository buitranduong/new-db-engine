package core

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"unsafe"

	"stask/internal/pindex/bench"
)

// ramTarget runs the P3.1 workload (internal/pindex/bench) against the
// current in-RAM primary index, with the shared read lock the engine takes
// around it, so the numbers compare with cmd/pibench.
//
//	PINDEX_CMP=1 PINDEX_N=20000000 go test -run TestPindexCompare -v ./internal/core
type ramTarget struct {
	mu sync.RWMutex
	tr *tree
}

func str(k []byte) string { return unsafe.String(&k[0], len(k)) }

func (r *ramTarget) Put(k, v []byte) error {
	r.mu.Lock()
	r.tr.put(str(k), &Version{LSN: binary.LittleEndian.Uint64(v)})
	r.mu.Unlock()
	return nil
}

func (r *ramTarget) Get(k []byte) (bool, error) {
	r.mu.RLock()
	_, ok := r.tr.get(str(k))
	r.mu.RUnlock()
	return ok, nil
}

func (r *ramTarget) Scan(from []byte, max int) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	f := ""
	if from != nil {
		f = str(from)
	}
	r.tr.ascend(f, func(*entry) bool { n++; return n < max })
	return n, nil
}

func (r *ramTarget) Checkpoint() error { return nil }
func (r *ramTarget) Close() error      { return nil }
func (r *ramTarget) Info() string {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fmt.Sprintf("core tree: heap %d MB, %d keys", m.HeapAlloc>>20, r.tr.Len())
}

func TestPindexCompare(t *testing.T) {
	if os.Getenv("PINDEX_CMP") == "" {
		t.Skip("set PINDEX_CMP=1")
	}
	n := 20_000_000
	if s := os.Getenv("PINDEX_N"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	cfg := bench.Config{N: n, Reads: 2_000_000, Scans: 50_000, Parallel: 4,
		Out: func(s string) { fmt.Println(s) }}
	mk := func() (bench.Target, error) {
		var ar keyArena
		return &ramTarget{tr: newTree(&ar)}, nil
	}
	fmt.Printf("core in-RAM tree n=%d\n", n)
	if m := os.Getenv("PINDEX_MODE"); m == "" || m == "seq" {
		bench.RunSeq(cfg, mk)
	}
	if m := os.Getenv("PINDEX_MODE"); m == "" || m == "rand" {
		bench.RunRand(cfg, mk)
	}
}
