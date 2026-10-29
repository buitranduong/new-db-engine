// Package spill holds the operators that let a query larger than its
// memory budget run slower instead of failing (docs/12 P2.4): an external
// merge sort, a Grace hash join with recursive repartitioning, and a hash
// aggregate / distinct that sends the rows of groups it has no room for to
// hash partitions on disk and processes them afterwards.
//
// The operators are generic over the row type and know nothing of SQL:
// the caller supplies a Codec (row <-> bytes) and, per operator, the
// functions that give a row its key. Every operator keeps its in-memory
// part under MemLimit bytes (as measured by Codec.Size) and touches the
// disk only past it, so a query that fits pays nothing.
//
// Temp files are created in a per-Manager directory, unlinked right after
// creation (a crash leaves nothing behind but, in the instant between
// create and unlink, an empty file that CleanStale removes), written as
// CRC-32C framed records and verified when read back. Manager.Check is
// polled every few thousand records so statement cancel and
// statement_timeout take effect inside spilling loops.
//
// References: PostgreSQL work_mem / temp_file_limit, tuplesort.c (external
// merge) and nodeHash.c (batched hash join)
// https://www.postgresql.org/docs/current/runtime-config-resource.html ;
// DuckDB external hash aggregate
// https://duckdb.org/2024/03/29/external-aggregation.html and its buffer
// manager https://duckdb.org/2024/07/09/memory-management.html ;
// ClickHouse max_bytes_before_external_group_by / _sort and
// join_algorithm=grace_hash
// https://clickhouse.com/docs/operations/settings/settings .
package spill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

var (
	// ErrQuota: the query wrote more temp data than Manager.MaxBytes.
	ErrQuota = errors.New("spill: temporary file limit exceeded")
	// ErrCorrupt: a temp file record failed its checksum or was cut short.
	ErrCorrupt = errors.New("spill: temporary file is corrupt")
	// ErrSkew: a partition is still over MemLimit after the deepest
	// repartitioning (one key holds more rows than fit in memory).
	ErrSkew = errors.New("spill: a single key does not fit in memory")
)

// Source pushes rows to yield until it is exhausted or yield fails.
type Source[T any] func(yield func(T) error) error

// Codec converts rows to bytes and back and estimates their memory.
type Codec[T any] struct {
	Enc  func(dst []byte, v T) []byte
	Dec  func(b []byte) (T, error) // must not keep b
	Size func(v T) int64           // bytes the row takes in memory
}

// Stats are process-wide counters, for tests and monitoring.
type Stats struct {
	Files        int64 // temp files created
	BytesWritten int64
	Spills       int64 // operator runs that went to disk
}

var stats struct{ files, bytes, spills atomic.Int64 }

// Snapshot returns the counters.
func Snapshot() Stats {
	return Stats{stats.files.Load(), stats.bytes.Load(), stats.spills.Load()}
}

// Manager owns the temp files of one query.
type Manager struct {
	Parent   string       // directory the per-query directory is made in
	MaxBytes int64        // total temp data the query may hold on disk (0 = no limit)
	Check    func() error // polled; a non-nil result aborts the operator

	ticks atomic.Int64
	mu    sync.Mutex
	dir   string
	used  int64
	open  map[*Run]struct{}
}

var dirSeq atomic.Int64

// NewManager makes a manager that puts files under parent.
func NewManager(parent string, maxBytes int64, check func() error) *Manager {
	return &Manager{Parent: parent, MaxBytes: maxBytes, Check: check, open: map[*Run]struct{}{}}
}

func (m *Manager) poll() error {
	if m.Check != nil {
		return m.Check()
	}
	return nil
}

func (m *Manager) dirLocked() (string, error) {
	if m.dir != "" {
		return m.dir, nil
	}
	if err := os.MkdirAll(m.Parent, 0o700); err != nil {
		return "", err
	}
	d := filepath.Join(m.Parent, fmt.Sprintf("stask-spill-%d-%d", os.Getpid(), dirSeq.Add(1)))
	if err := os.Mkdir(d, 0o700); err != nil {
		return "", err
	}
	m.dir = d
	return d, nil
}

func (m *Manager) charge(n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.MaxBytes > 0 && m.used+n > m.MaxBytes {
		return fmt.Errorf("%w (%d MB)", ErrQuota, m.MaxBytes>>20)
	}
	m.used += n
	return nil
}

// Used is the temp data currently on disk.
func (m *Manager) Used() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used
}

// Close closes every file still open and removes the query directory.
func (m *Manager) Close() error {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.open))
	for r := range m.open {
		runs = append(runs, r)
	}
	dir := m.dir
	m.mu.Unlock()
	for _, r := range runs {
		r.Close()
	}
	if dir != "" {
		return os.RemoveAll(dir)
	}
	return nil
}

// CleanStale removes the directories earlier processes left in parent: of
// processes that no longer exist, and with own set also of this pid (call
// it at startup, before any query, when a restart may reuse the pid).
// It returns the number removed.
func CleanStale(parent string, own bool) int {
	ents, err := os.ReadDir(parent)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		rest, ok := strings.CutPrefix(e.Name(), "stask-spill-")
		if !ok || !e.IsDir() {
			continue
		}
		pidS, _, _ := strings.Cut(rest, "-")
		pid, err := strconv.Atoi(pidS)
		if err != nil {
			continue
		}
		if pid == os.Getpid() && !own {
			continue
		}
		if pid != os.Getpid() && syscall.Kill(pid, 0) == nil {
			continue // still running
		}
		if os.RemoveAll(filepath.Join(parent, e.Name())) == nil {
			n++
		}
	}
	return n
}
