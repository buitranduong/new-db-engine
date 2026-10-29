// Package pindex is a disk-resident ordered index: a copy-on-write B+tree
// over a bounded buffer pool, built to replace the all-in-RAM primary index
// of the engine (docs/12 P3.1). It is deliberately independent of
// internal/core and internal/types: keys and values are plain bytes, keys
// are ordered with bytes.Compare.
//
// Design in one paragraph (rationale and alternatives: docs/15_disk_index.md):
// the log is the source of truth, so the index file needs no write-ahead log
// of its own. Pages are never overwritten in place while anything durable or
// any snapshot can reach them. Writers copy a page the first time they touch
// it in a "generation", a checkpoint flushes the dirty pages of a frozen
// generation and then commits by writing one of two alternating meta pages.
// A crash therefore leaves the last committed checkpoint intact, and the
// engine replays the log from the position stored in the checkpoint's user
// blob. Snapshots are O(1): pin a root, bump the generation.
package pindex

// Index is the interface the engine would program against.
type Index interface {
	// Get appends the value of key to dst[:0] and reports whether it exists.
	Get(key, dst []byte) (val []byte, ok bool, err error)
	// Put inserts or replaces key. Writers are serialized.
	Put(key, val []byte) error
	// Delete removes key and reports whether it existed.
	Delete(key []byte) (bool, error)
	// Snapshot returns a consistent read-only view that stays valid, and
	// unaffected by later writes, until it is closed.
	Snapshot() (Snapshot, error)
	// Checkpoint makes the current state durable and stores user (at most
	// MaxUser bytes, e.g. the log position to replay from) with it.
	Checkpoint(user []byte) error
	// User returns the user blob of the checkpoint the index was opened at,
	// or of the last Checkpoint.
	User() []byte
	Len() int64
	Stats() Stats
	// Close releases the file. It does not checkpoint.
	Close() error
}

// Snapshot is a read-only, point-in-time view. All methods are safe for
// concurrent use; Iterators are not.
type Snapshot interface {
	Get(key, dst []byte) (val []byte, ok bool, err error)
	// Seek returns an iterator positioned before the first key >= key.
	Seek(key []byte) Iterator
	// Scan iterates keys in [lo, hi); nil lo means from the start, nil hi
	// means to the end.
	Scan(lo, hi []byte) Iterator
	Len() int64
	Close() error
}

// Iterator walks keys in ascending order. Key and Value are valid until the
// next call to Next.
type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close()
}

// MaxUser is the largest user blob a checkpoint can carry.
const MaxUser = 1024

// Stats are cumulative counters plus current sizes.
type Stats struct {
	Keys         int64
	FilePages    uint64 // allocation frontier, in pages
	FreePages    int    // reusable now
	PendingPages int    // freed but still reachable from a snapshot or the last checkpoint
	CacheFrames  int
	DirtyFrames  int
	Misses       int64 // pool misses (a page had to be loaded or created)
	Reads        int64 // pages read from the file
	Evictions    int64
	Writebacks   int64 // pages written (eviction and checkpoint)
	Checkpoints  int64
}
