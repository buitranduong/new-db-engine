package core

import (
	"encoding/binary"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"stask/internal/wal"
)

// The primary index of a table maps each key to its head version. It is a
// copy-on-write B+tree whose leaves hold fixed-size entries with no Go
// pointers: a leaf is one noscan object that the garbage collector marks
// without reading, so a GC cycle costs per leaf (~66 keys), not per key.
// With a string key and a *Version per entry, every key cost the GC three
// objects to trace and a mark phase over 20M keys took ~1.3 s of wall time
// (docs/08_memory.md). Pebble's memtable keeps its skiplist pointer-free
// the same way, addressing nodes and keys by arena offsets.
//
// Keys up to inlineKey bytes are stored in the entry; longer keys live in
// the table's key arena and the entry holds their reference. Only the
// fields a read needs are kept: the commit time and the previous-version
// location are in the log record at Loc and are loaded on demand (fill).

// inlineKey is the longest key stored in an entry itself.
const inlineKey = 12

// longKey marks an entry whose key is in the arena (ref in k[0:8]).
const longKey = 0xff

const entDeleted = 1

// ikey is a key as stored in the index: inline bytes, or an arena reference.
type ikey struct {
	k    [inlineKey]byte
	klen uint8
}

// entry is one key's head version (40 bytes, no pointers).
type entry struct {
	ikey
	flags  uint8
	idx    uint16
	lsn    uint64
	seg    uint32
	off    uint32
	valOff uint32
	valLen uint32
}

func (e *entry) version() Version {
	return Version{LSN: e.lsn, Loc: wal.Loc{Ok: true, Seg: e.seg, Off: e.off, Idx: e.idx},
		ValOff: int64(e.valOff), ValLen: e.valLen, Deleted: e.flags&entDeleted != 0}
}

func (e *entry) setVersion(v *Version) {
	e.lsn, e.seg, e.off, e.idx = v.LSN, v.Loc.Seg, v.Loc.Off, v.Loc.Idx
	e.valOff, e.valLen = uint32(v.ValOff), v.ValLen
	e.flags = 0
	if v.Deleted {
		e.flags = entDeleted
	}
}

// keyArena holds the keys too long to inline, for every version of one
// table's index (clones share it). It only grows: a long key that leaves
// the index stays until the next restart, which rebuilds the arena from the
// checkpoint (Dead counts those bytes).
type keyArena struct {
	mu     sync.Mutex
	chunks atomic.Pointer[[][]byte]
	fill   int // bytes used in the last chunk
	Live   atomic.Int64
	Dead   atomic.Int64
}

const arenaChunk = 1 << 20

// add stores key and returns its reference: chunk<<32 | offset.
func (a *keyArena) add(key string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var cs [][]byte
	if p := a.chunks.Load(); p != nil {
		cs = *p
	}
	need := binary.MaxVarintLen32 + len(key)
	if len(cs) == 0 || a.fill+need > len(cs[len(cs)-1]) {
		ncs := make([][]byte, len(cs)+1)
		copy(ncs, cs)
		ncs[len(cs)] = make([]byte, max(arenaChunk, need))
		cs = ncs
		a.fill = 0
		a.chunks.Store(&cs)
	}
	c := cs[len(cs)-1]
	off := a.fill
	n := binary.PutUvarint(c[off:], uint64(len(key)))
	copy(c[off+n:], key)
	a.fill += n + len(key)
	a.Live.Add(int64(len(key)))
	return uint64(len(cs)-1)<<32 | uint64(off)
}

// get returns the key at ref without copying; the bytes never change.
func (a *keyArena) get(ref uint64) string {
	c := (*a.chunks.Load())[ref>>32][uint32(ref):]
	n, k := binary.Uvarint(c)
	return unsafe.String(&c[k], int(n))
}

func (a *keyArena) Bytes() int64 {
	var n int64
	if p := a.chunks.Load(); p != nil {
		for _, c := range *p {
			n += int64(len(c))
		}
	}
	return n
}

func (a *keyArena) ikeyOf(key string) (k ikey) {
	if len(key) <= inlineKey {
		k.klen = uint8(copy(k.k[:], key))
		return
	}
	k.klen = longKey
	binary.LittleEndian.PutUint64(k.k[:], a.add(key))
	return
}

const (
	leafMax  = 66 // 16 + 66*40 = 2656 bytes: fills the 2688-byte size class
	innerMax = 64
)

type leafNode struct {
	owner uint64
	n     int32
	ents  [leafMax]entry
}

// innerNode: child j holds the keys >= keys[j] (keys[0] is only used when
// the node splits); exactly one of leaves/inners is in use.
type innerNode struct {
	owner    uint64
	n        int
	leafKids bool
	keys     [innerMax]ikey
	leaves   [innerMax]*leafNode
	inners   [innerMax]*innerNode
}

// tree is one immutable version of an index once published. Mutations are
// made on a clone: nodes owned by another generation are copied first.
type tree struct {
	gen   uint64
	leaf  *leafNode  // root when the tree has one level
	inner *innerNode // root otherwise
	n     int
	ar    *keyArena
}

var treeGen atomic.Uint64

func newTree(ar *keyArena) *tree { return &tree{gen: treeGen.Add(1), ar: ar} }

// clone returns a mutable copy in O(1); t must no longer be mutated.
func (t *tree) clone() *tree {
	c := *t
	c.gen = treeGen.Add(1)
	return &c
}

func (t *tree) Len() int { return t.n }

func (t *tree) keyStr(k *ikey) string {
	if k.klen != longKey {
		return unsafe.String(&k.k[0], int(k.klen))
	}
	return t.ar.get(binary.LittleEndian.Uint64(k.k[:]))
}

// keyOf returns e's key without copying: it stays valid as long as the
// tree it was read from (leaves of a published tree never change).
func (t *tree) keyOf(e *entry) string { return t.keyStr(&e.ikey) }

// search returns the first position in l whose key is >= key.
func (t *tree) search(l *leafNode, key string) (int, bool) {
	lo, hi := 0, int(l.n)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if c := strings.Compare(t.keyOf(&l.ents[m]), key); c < 0 {
			lo = m + 1
		} else if c == 0 {
			return m, true
		} else {
			hi = m
		}
	}
	return lo, false
}

// child returns the child of n that holds key.
func (t *tree) child(n *innerNode, key string) int {
	lo, hi := 1, n.n
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if t.keyStr(&n.keys[m]) <= key {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo - 1
}

func (t *tree) get(key string) (*entry, bool) {
	l := t.leaf
	for n := t.inner; n != nil; {
		j := t.child(n, key)
		if n.leafKids {
			l = n.leaves[j]
			break
		}
		n = n.inners[j]
	}
	if l == nil {
		return nil, false
	}
	if i, ok := t.search(l, key); ok {
		return &l.ents[i], true
	}
	return nil, false
}

func (t *tree) ownLeaf(l *leafNode) *leafNode {
	if l.owner == t.gen {
		return l
	}
	c := new(leafNode)
	*c = *l
	c.owner = t.gen
	return c
}

func (t *tree) ownInner(n *innerNode) *innerNode {
	if n.owner == t.gen {
		return n
	}
	c := new(innerNode)
	*c = *n
	c.owner = t.gen
	return c
}

// put makes v the head of key and returns the entry it replaced.
func (t *tree) put(key string, v *Version) (old entry, had bool) {
	if t.inner == nil {
		if t.leaf == nil {
			t.leaf = &leafNode{owner: t.gen}
		}
		t.leaf = t.ownLeaf(t.leaf)
		var right *leafNode
		old, had, right = t.leafPut(t.leaf, key, v)
		if right != nil {
			r := &innerNode{owner: t.gen, n: 2, leafKids: true}
			r.keys[0], r.keys[1] = t.leaf.ents[0].ikey, right.ents[0].ikey
			r.leaves[0], r.leaves[1] = t.leaf, right
			t.inner, t.leaf = r, nil
		}
	} else {
		t.inner = t.ownInner(t.inner)
		var right *innerNode
		old, had, right = t.innerPut(t.inner, key, v)
		if right != nil {
			r := &innerNode{owner: t.gen, n: 2}
			r.keys[0], r.keys[1] = t.inner.keys[0], right.keys[0]
			r.inners[0], r.inners[1] = t.inner, right
			t.inner = r
		}
	}
	if !had {
		t.n++
	}
	return old, had
}

// leafPut inserts into the owned leaf l; a full leaf splits in the middle
// and the new right half is returned, except that an insert at the end
// leaves the left half full (ascending inserts, the rightmost leaf). Most
// overflows never get here: innerPut first shifts entries to a neighbour.
func (t *tree) leafPut(l *leafNode, key string, v *Version) (old entry, had bool, right *leafNode) {
	i, ok := t.search(l, key)
	if ok {
		old = l.ents[i]
		l.ents[i].setVersion(v)
		return old, true, nil
	}
	var e entry
	e.ikey = t.ar.ikeyOf(key)
	e.setVersion(v)
	n := int(l.n)
	if n < leafMax {
		copy(l.ents[i+1:n+1], l.ents[i:n])
		l.ents[i] = e
		l.n++
		return old, false, nil
	}
	var all [leafMax + 1]entry
	copy(all[:i], l.ents[:i])
	all[i] = e
	copy(all[i+1:], l.ents[i:])
	s := (leafMax + 1) / 2
	if i == leafMax {
		s = leafMax
	}
	right = &leafNode{owner: t.gen, n: int32(leafMax + 1 - s)}
	copy(right.ents[:], all[s:])
	l.n = int32(copy(l.ents[:], all[:s]))
	clear(l.ents[l.n:])
	return old, false, right
}

func (t *tree) innerPut(n *innerNode, key string, v *Version) (old entry, had bool, right *innerNode) {
	j := t.child(n, key)
	var sep ikey
	var nl *leafNode
	var ni *innerNode
	if n.leafKids {
		if n.leaves[j].n == leafMax {
			t.shift(n, j, key)
		}
		c := t.ownLeaf(n.leaves[j])
		n.leaves[j] = c
		if old, had, nl = t.leafPut(c, key, v); nl == nil {
			return old, had, nil
		}
		sep = nl.ents[0].ikey
	} else {
		c := t.ownInner(n.inners[j])
		n.inners[j] = c
		if old, had, ni = t.innerPut(c, key, v); ni == nil {
			return old, had, nil
		}
		sep = ni.keys[0]
	}
	// insert the new child at j+1
	at := j + 1
	if n.n < innerMax {
		n.insertKid(at, sep, nl, ni)
		return old, had, nil
	}
	s := (innerMax + 1) / 2
	if at == innerMax {
		s = innerMax
	}
	right = &innerNode{owner: t.gen, leafKids: n.leafKids}
	if at < s {
		right.n = copy(right.keys[:], n.keys[s-1:n.n])
		copy(right.leaves[:], n.leaves[s-1:n.n])
		copy(right.inners[:], n.inners[s-1:n.n])
		n.truncate(s - 1)
		n.insertKid(at, sep, nl, ni)
	} else {
		right.n = copy(right.keys[:], n.keys[s:n.n])
		copy(right.leaves[:], n.leaves[s:n.n])
		copy(right.inners[:], n.inners[s:n.n])
		n.truncate(s)
		right.insertKid(at-s, sep, nl, ni)
	}
	return old, had, right
}

// shift makes room in the full leaf j of n for key by moving entries to a
// neighbour with free space instead of splitting (as a B*-tree does): the
// entries before the insert point go to the left neighbour, or those after
// it to the right one, so key still belongs in leaf j. Leaves then split
// only when their neighbours are full too, which keeps them dense under
// any insert order: 84% full for random keys, 99% for ascending decimal
// ids (TestIndexFill), which land among existing shorter ids and left every
// split leaf half full (52% at 20M keys) when leaves only split. It costs
// one more leaf copy per overflow.
func (t *tree) shift(n *innerNode, j int, key string) {
	c := n.leaves[j]
	i, found := t.search(c, key)
	if found {
		return // a replace needs no room
	}
	// one entry before the insert point stays: it keeps leaf j's lower
	// bound below key
	if j > 0 && i > 1 && n.leaves[j-1].n < leafMax {
		m := min(leafMax-int(n.leaves[j-1].n), i-1)
		ln, c := t.ownLeaf(n.leaves[j-1]), t.ownLeaf(c)
		copy(ln.ents[ln.n:], c.ents[:m])
		ln.n += int32(m)
		copy(c.ents[:], c.ents[m:c.n])
		c.n -= int32(m)
		clear(c.ents[c.n:])
		n.leaves[j-1], n.leaves[j] = ln, c
		n.keys[j] = c.ents[0].ikey
		return
	}
	if j+1 < n.n && i < int(c.n) && n.leaves[j+1].n < leafMax {
		m := min(leafMax-int(n.leaves[j+1].n), int(c.n)-i)
		rn, c := t.ownLeaf(n.leaves[j+1]), t.ownLeaf(c)
		copy(rn.ents[m:], rn.ents[:rn.n])
		copy(rn.ents[:m], c.ents[int(c.n)-m:c.n])
		rn.n += int32(m)
		c.n -= int32(m)
		clear(c.ents[c.n:])
		n.leaves[j], n.leaves[j+1] = c, rn
		n.keys[j+1] = rn.ents[0].ikey
	}
}

func (n *innerNode) insertKid(at int, sep ikey, l *leafNode, c *innerNode) {
	copy(n.keys[at+1:n.n+1], n.keys[at:n.n])
	copy(n.leaves[at+1:n.n+1], n.leaves[at:n.n])
	copy(n.inners[at+1:n.n+1], n.inners[at:n.n])
	n.keys[at], n.leaves[at], n.inners[at] = sep, l, c
	n.n++
}

func (n *innerNode) removeKid(at int) {
	copy(n.keys[at:], n.keys[at+1:n.n])
	copy(n.leaves[at:], n.leaves[at+1:n.n])
	copy(n.inners[at:], n.inners[at+1:n.n])
	n.n--
	n.keys[n.n], n.leaves[n.n], n.inners[n.n] = ikey{}, nil, nil
}

func (n *innerNode) truncate(m int) {
	clear(n.keys[m:n.n])
	clear(n.leaves[m:n.n])
	clear(n.inners[m:n.n])
	n.n = m
}

// del removes key and returns its entry. Underfull nodes merge with a
// neighbour when the two fit in one node, so deletes do not leave the
// index sparse.
func (t *tree) del(key string) (old entry, had bool) {
	switch {
	case t.inner != nil:
		t.inner = t.ownInner(t.inner)
		if old, had = t.innerDel(t.inner, key); !had {
			return
		}
		for t.inner != nil && t.inner.n == 1 { // collapse the root
			if t.inner.leafKids {
				t.leaf, t.inner = t.inner.leaves[0], nil
			} else {
				t.inner = t.inner.inners[0]
			}
		}
	case t.leaf != nil:
		i, ok := t.search(t.leaf, key)
		if !ok {
			return
		}
		t.leaf = t.ownLeaf(t.leaf)
		old, had = t.leaf.ents[i], true
		t.leaf.removeAt(i)
		if t.leaf.n == 0 {
			t.leaf = nil
		}
	default:
		return
	}
	if old.klen == longKey {
		t.ar.Dead.Add(int64(len(t.keyOf(&old))))
	}
	t.n--
	return old, true
}

func (l *leafNode) removeAt(i int) {
	copy(l.ents[i:], l.ents[i+1:l.n])
	l.n--
	l.ents[l.n] = entry{}
}

func (t *tree) innerDel(n *innerNode, key string) (old entry, had bool) {
	j := t.child(n, key)
	if n.leafKids {
		l := n.leaves[j]
		i, ok := t.search(l, key)
		if !ok {
			return old, false
		}
		l = t.ownLeaf(l)
		n.leaves[j] = l
		old = l.ents[i]
		l.removeAt(i)
		switch {
		case l.n == 0:
			n.removeKid(j)
		case int(l.n) < leafMax/4:
			t.mergeLeaf(n, j)
		}
		return old, true
	}
	c := n.inners[j]
	// descend into an owned copy only if the key is there
	if _, ok := t.get2(c, key); !ok {
		return old, false
	}
	c = t.ownInner(c)
	n.inners[j] = c
	old, had = t.innerDel(c, key)
	switch {
	case c.n == 0:
		n.removeKid(j)
	case c.n < innerMax/4:
		t.mergeInner(n, j)
	}
	return old, had
}

// get2 looks key up in the subtree n.
func (t *tree) get2(n *innerNode, key string) (*entry, bool) {
	sub := tree{inner: n, ar: t.ar}
	return sub.get(key)
}

// mergeLeaf merges child j of n with a neighbour if they fit in one leaf.
func (t *tree) mergeLeaf(n *innerNode, j int) {
	a, b := j-1, j
	if a < 0 {
		a, b = 0, 1
	}
	if b >= n.n || int(n.leaves[a].n+n.leaves[b].n) > leafMax*3/4 {
		return
	}
	left := t.ownLeaf(n.leaves[a])
	rl := n.leaves[b]
	copy(left.ents[left.n:], rl.ents[:rl.n])
	left.n += rl.n
	n.leaves[a] = left
	n.removeKid(b)
}

func (t *tree) mergeInner(n *innerNode, j int) {
	a, b := j-1, j
	if a < 0 {
		a, b = 0, 1
	}
	if b >= n.n || n.inners[a].n+n.inners[b].n > innerMax*3/4 {
		return
	}
	left := t.ownInner(n.inners[a])
	r := n.inners[b]
	// the right node's first child is reached through the separator above it
	k := n.keys[b]
	for i := 0; i < r.n; i++ {
		if i > 0 {
			k = r.keys[i]
		}
		left.keys[left.n] = k
		left.leaves[left.n], left.inners[left.n] = r.leaves[i], r.inners[i]
		left.n++
	}
	n.inners[a] = left
	n.removeKid(b)
}

// ascend calls fn for the entries with key >= from in key order (all of
// them if from is ""), until fn returns false. The entries are read-only.
func (t *tree) ascend(from string, fn func(*entry) bool) {
	switch {
	case t.inner != nil:
		t.ascInner(t.inner, from, from == "", fn)
	case t.leaf != nil:
		t.ascLeaf(t.leaf, from, from == "", fn)
	}
}

func (t *tree) ascLeaf(l *leafNode, from string, all bool, fn func(*entry) bool) bool {
	i := 0
	if !all {
		i, _ = t.search(l, from)
	}
	for ; i < int(l.n); i++ {
		if !fn(&l.ents[i]) {
			return false
		}
	}
	return true
}

func (t *tree) ascInner(n *innerNode, from string, all bool, fn func(*entry) bool) bool {
	j := 0
	if !all {
		j = t.child(n, from)
	}
	for k := j; k < n.n; k++ {
		var ok bool
		if n.leafKids {
			ok = t.ascLeaf(n.leaves[k], from, all || k > j, fn)
		} else {
			ok = t.ascInner(n.inners[k], from, all || k > j, fn)
		}
		if !ok {
			return false
		}
	}
	return true
}

// treeBuilder builds a tree from entries in ascending key order, with
// full leaves (checkpoint load).
type treeBuilder struct {
	t      *tree
	leaves []*leafNode
	cur    *leafNode
}

func newTreeBuilder(ar *keyArena) *treeBuilder { return &treeBuilder{t: newTree(ar)} }

func (b *treeBuilder) add(e entry) {
	if b.cur == nil || b.cur.n == leafMax {
		b.cur = &leafNode{owner: b.t.gen}
		b.leaves = append(b.leaves, b.cur)
	}
	b.cur.ents[b.cur.n] = e
	b.cur.n++
	b.t.n++
}

func (b *treeBuilder) finish() *tree {
	t := b.t
	switch len(b.leaves) {
	case 0:
		return t
	case 1:
		t.leaf = b.leaves[0]
		return t
	}
	var level []*innerNode
	for i := 0; i < len(b.leaves); i += innerMax {
		n := &innerNode{owner: t.gen, leafKids: true}
		for _, l := range b.leaves[i:min(i+innerMax, len(b.leaves))] {
			n.keys[n.n], n.leaves[n.n] = l.ents[0].ikey, l
			n.n++
		}
		level = append(level, n)
	}
	for len(level) > 1 {
		var up []*innerNode
		for i := 0; i < len(level); i += innerMax {
			n := &innerNode{owner: t.gen}
			for _, c := range level[i:min(i+innerMax, len(level))] {
				n.keys[n.n], n.inners[n.n] = c.keys[0], c
				n.n++
			}
			up = append(up, n)
		}
		level = up
	}
	t.inner = level[0]
	return t
}

// IndexStats describes the memory of a table's primary index.
type IndexStats struct {
	Keys       int   `json:"keys"`
	Leaves     int   `json:"leaves"`
	Inner      int   `json:"inner_nodes"`
	Bytes      int64 `json:"bytes"`
	ArenaBytes int64 `json:"key_arena_bytes"`
	ArenaDead  int64 `json:"key_arena_dead_bytes"`
}

func (t *TableState) IndexStats() IndexStats {
	tr := t.index()
	s := IndexStats{Keys: tr.n, ArenaBytes: tr.ar.Bytes(), ArenaDead: tr.ar.Dead.Load()}
	var walk func(n *innerNode)
	walk = func(n *innerNode) {
		s.Inner++
		for i := 0; i < n.n; i++ {
			if n.leafKids {
				s.Leaves++
			} else {
				walk(n.inners[i])
			}
		}
	}
	if tr.inner != nil {
		walk(tr.inner)
	} else if tr.leaf != nil {
		s.Leaves = 1
	}
	s.Bytes = int64(s.Leaves)*int64(unsafe.Sizeof(leafNode{})) + int64(s.Inner)*int64(unsafe.Sizeof(innerNode{})) + s.ArenaBytes
	return s
}

// descend calls fn for the entries with key < upto in descending key order
// (all of them if upto is ""), until fn returns false.
func (t *tree) descend(upto string, fn func(*entry) bool) {
	switch {
	case t.inner != nil:
		t.descInner(t.inner, upto, upto == "", fn)
	case t.leaf != nil:
		t.descLeaf(t.leaf, upto, upto == "", fn)
	}
}

func (t *tree) descLeaf(l *leafNode, upto string, all bool, fn func(*entry) bool) bool {
	i := int(l.n) - 1
	if !all {
		p, _ := t.search(l, upto)
		i = p - 1
	}
	for ; i >= 0; i-- {
		if !fn(&l.ents[i]) {
			return false
		}
	}
	return true
}

func (t *tree) descInner(n *innerNode, upto string, all bool, fn func(*entry) bool) bool {
	j := n.n - 1
	if !all {
		j = t.child(n, upto)
	}
	for k := j; k >= 0; k-- {
		var ok bool
		if n.leafKids {
			ok = t.descLeaf(n.leaves[k], upto, all || k < j, fn)
		} else {
			ok = t.descInner(n.inners[k], upto, all || k < j, fn)
		}
		if !ok {
			return false
		}
	}
	return true
}
