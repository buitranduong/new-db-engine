package proj

// intPosIndex maps an integer primary key to its row position in a
// columnar table: open addressing over a []uint32 of positions, the key
// itself read back from the table's key column. It holds no pointers and
// no key strings, so it costs ~6 bytes per row instead of the ~55 of a
// map[string]uint32 with its keys, and adds nothing for the garbage
// collector to scan (the per-pointer cost that dominates large Go heaps).
type intPosIndex struct {
	slots []uint32 // 0 = empty, tomb = deleted, else position+1
	used  int      // live + tombstones
	live  int
}

const tomb = ^uint32(0)

func hashInt(k int64) uint64 { return mix64(0, uint64(k)) }

// find returns the slot holding key k, or -1. keyAt reads the key of a
// position.
func (x *intPosIndex) find(k int64, keyAt *pvec[int64]) int {
	if len(x.slots) == 0 {
		return -1
	}
	mask := uint64(len(x.slots) - 1)
	for i := hashInt(k) & mask; ; i = (i + 1) & mask {
		s := x.slots[i]
		if s == 0 {
			return -1
		}
		if s != tomb && keyAt.at(int(s-1)) == k {
			return int(i)
		}
	}
}

func (x *intPosIndex) get(k int64, keyAt *pvec[int64]) (uint32, bool) {
	if i := x.find(k, keyAt); i >= 0 {
		return x.slots[i] - 1, true
	}
	return 0, false
}

func (x *intPosIndex) del(k int64, keyAt *pvec[int64]) (uint32, bool) {
	i := x.find(k, keyAt)
	if i < 0 {
		return 0, false
	}
	p := x.slots[i] - 1
	x.slots[i] = tomb
	x.live--
	return p, true
}

// set records that key k (not present) is at pos; keyAt must already
// hold k at pos.
func (x *intPosIndex) set(k int64, pos uint32, keyAt *pvec[int64]) {
	if (x.used+1)*4 > len(x.slots)*3 {
		x.rehash(keyAt)
	}
	mask := uint64(len(x.slots) - 1)
	i := hashInt(k) & mask
	for x.slots[i] != 0 && x.slots[i] != tomb {
		i = (i + 1) & mask
	}
	if x.slots[i] == 0 {
		x.used++
	}
	x.slots[i] = pos + 1
	x.live++
}

func (x *intPosIndex) rehash(keyAt *pvec[int64]) {
	// at most 5/8 full afterwards: short linear probes, and inserts until
	// the 3/4 limit before the next rehash
	n := 1 << 10
	for n*5 < (x.live+1)*8 {
		n *= 2
	}
	old := x.slots
	x.slots, x.used = make([]uint32, n), 0
	mask := uint64(n - 1)
	for _, s := range old {
		if s == 0 || s == tomb {
			continue
		}
		i := hashInt(keyAt.at(int(s-1))) & mask
		for x.slots[i] != 0 {
			i = (i + 1) & mask
		}
		x.slots[i] = s
		x.used++
	}
}

// reserve sizes an empty index for n keys, so loading them never rehashes.
func (x *intPosIndex) reserve(n int) {
	sz := 1 << 10
	for sz*5 < (n+1)*8 {
		sz *= 2
	}
	x.slots, x.used, x.live = make([]uint32, sz), 0, 0
}
