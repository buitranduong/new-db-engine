package proj

// segDir is the row directory of one table: key -> row id of the latest
// version (base positions first, then tail positions). Open addressing over
// uint32 slots (id+1; 0 = empty). It is private to the writer goroutine and
// never published: readers see the effect of the directory only through the
// Changed / TailDead bitmaps.
type segDir struct {
	slots []uint32
	mask  uint64
	n     int
	key   func(id uint32) []byte
}

func segHash(k []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range k {
		h ^= uint64(c)
		h *= 1099511628211
	}
	h ^= h >> 29
	h *= 0x9E3779B97F4A7C15
	return h ^ h>>32
}

func newSegDir(n int, key func(uint32) []byte) *segDir {
	sz := 1 << 4
	for sz < n*10/6+16 {
		sz <<= 1
	}
	return &segDir{slots: make([]uint32, sz), mask: uint64(sz - 1), key: key}
}

func (d *segDir) grow() {
	old := d.slots
	d.slots = make([]uint32, len(old)*2)
	d.mask = uint64(len(d.slots) - 1)
	for _, s := range old {
		if s != 0 {
			i := segHash(d.key(s-1)) & d.mask
			for d.slots[i] != 0 {
				i = (i + 1) & d.mask
			}
			d.slots[i] = s
		}
	}
}

// Get returns the row id of the latest version of k.
func (d *segDir) Get(k []byte) (uint32, bool) {
	i := segHash(k) & d.mask
	for {
		s := d.slots[i]
		if s == 0 {
			return 0, false
		}
		if string(d.key(s-1)) == string(k) {
			return s - 1, true
		}
		i = (i + 1) & d.mask
	}
}

// Put sets k -> id (insert or replace). k must be the key of id.
func (d *segDir) Put(k []byte, id uint32) {
	if d.n*10 >= len(d.slots)*7 {
		d.grow()
	}
	i := segHash(k) & d.mask
	for {
		s := d.slots[i]
		if s == 0 {
			d.slots[i] = id + 1
			d.n++
			return
		}
		if string(d.key(s-1)) == string(k) {
			d.slots[i] = id + 1
			return
		}
		i = (i + 1) & d.mask
	}
}

// Bytes is the resident size of the directory.
func (d *segDir) Bytes() int64 { return int64(len(d.slots)) * 4 }
