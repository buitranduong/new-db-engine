// Command gcbench measures what one GC cycle costs for candidate layouts of
// the primary index at N keys: heap size, wall time and CPU of a forced
// collection. It decides how far the index must go to stop the GC mark
// phase from competing with statements (docs/08_memory.md §5).
package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"time"

	"github.com/google/btree"
)

type loc struct {
	Seg, Off uint32
	Idx      uint16
	Ok       bool
}

// current Version: one heap object per key, with a pointer
type verPtr struct {
	LSN, TS, ValOff int64
	prev            *verPtr
	Loc, Prev       loc
	ValLen          uint32
	Deleted         bool
}

// same fields, no pointer: a noscan object
type verNoPtr struct {
	LSN, TS, ValOff int64
	prevSlot        uint32
	Loc, Prev       loc
	ValLen          uint32
	Deleted         bool
}

type itemCur struct {
	key string
	v   *verPtr
}
type itemNoPtrVer struct {
	key string
	v   *verNoPtr
}
type ikey struct {
	b [16]byte
	n uint8
}

func (a ikey) less(b ikey) bool {
	return string(a.b[:a.n]) < string(b.b[:b.n])
}

type itemInlineKey struct {
	k ikey
	v *verNoPtr
}

// Version fields inline in the B-tree item, nothing on the heap per key
type itemFlat struct {
	k              ikey
	LSN            uint64
	TS             int64
	Loc, Prev      loc
	ValOff, ValLen uint32
	Deleted        bool
}

// itemFlat plus a pointer that stays nil unless the key is long
type itemFlatLong struct {
	itemFlat
	long *string
}

type itemSlot struct {
	k    ikey
	slot uint32
}

func mk(i int) string { return strconv.Itoa(i) }
func ik(s string) (k ikey) {
	k.n = uint8(copy(k.b[:], s))
	return
}

var cpuSample = []metrics.Sample{{Name: "/cpu/classes/gc/total:cpu-seconds"}}

func gcCost(label string, keep any) {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var best, cpuBest float64
	for r := 0; r < 3; r++ {
		metrics.Read(cpuSample)
		c0 := cpuSample[0].Value.Float64()
		t0 := time.Now()
		runtime.GC()
		wall := time.Since(t0).Seconds()
		metrics.Read(cpuSample)
		cpu := cpuSample[0].Value.Float64() - c0
		if r == 0 || wall < best {
			best, cpuBest = wall, cpu
		}
	}
	fmt.Printf("%-34s heap %6.0f MB  objects %9d  GC wall %6.0f ms  GC cpu %6.0f ms\n",
		label, float64(ms.HeapAlloc)/1e6, ms.HeapObjects, best*1000, cpuBest*1000)
	runtime.KeepAlive(keep)
}

func main() {
	n := flag.Int("n", 20_000_000, "keys")
	which := flag.String("layout", "all", "cur|noptrver|inlinekey|slot|all")
	flag.Parse()
	debug.SetGCPercent(-1) // only the forced collections
	run := func(name string) bool { return *which == "all" || *which == name }
	if run("cur") {
		t := btree.NewG[itemCur](32, func(a, b itemCur) bool { return a.key < b.key })
		for i := 0; i < *n; i++ {
			t.ReplaceOrInsert(itemCur{mk(i), &verPtr{LSN: int64(i)}})
		}
		gcCost("cur: string key, *Version (ptr)", t)
		t = nil
	}
	if run("noptrver") {
		t := btree.NewG[itemNoPtrVer](32, func(a, b itemNoPtrVer) bool { return a.key < b.key })
		for i := 0; i < *n; i++ {
			t.ReplaceOrInsert(itemNoPtrVer{mk(i), &verNoPtr{LSN: int64(i)}})
		}
		gcCost("string key, *Version (noscan)", t)
		t = nil
	}
	if run("inlinekey") {
		t := btree.NewG[itemInlineKey](32, func(a, b itemInlineKey) bool { return a.k.less(b.k) })
		for i := 0; i < *n; i++ {
			t.ReplaceOrInsert(itemInlineKey{ik(mk(i)), &verNoPtr{LSN: int64(i)}})
		}
		gcCost("inline key, *Version (noscan)", t)
		t = nil
	}
	if run("flat") {
		t := btree.NewG[itemFlat](32, func(a, b itemFlat) bool { return a.k.less(b.k) })
		for i := 0; i < *n; i++ {
			t.ReplaceOrInsert(itemFlat{k: ik(mk(i)), LSN: uint64(i)})
		}
		gcCost("flat: inline key + Version", t)
		t = nil
	}
	if run("flatlong") {
		t := btree.NewG[itemFlatLong](32, func(a, b itemFlatLong) bool { return a.k.less(b.k) })
		for i := 0; i < *n; i++ {
			t.ReplaceOrInsert(itemFlatLong{itemFlat: itemFlat{k: ik(mk(i)), LSN: uint64(i)}})
		}
		gcCost("flat + nil *string long key", t)
		t = nil
	}
	if run("slot") {
		t := btree.NewG[itemSlot](32, func(a, b itemSlot) bool { return a.k.less(b.k) })
		const chunk = 1 << 16
		var slab [][]verNoPtr
		for i := 0; i < *n; i++ {
			if i%chunk == 0 {
				slab = append(slab, make([]verNoPtr, chunk))
			}
			slab[i/chunk][i%chunk].LSN = int64(i)
			t.ReplaceOrInsert(itemSlot{ik(mk(i)), uint32(i)})
		}
		gcCost("inline key, Version slot (no ptrs)", []any{t, slab})
	}
}
