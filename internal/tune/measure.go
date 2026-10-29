package tune

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// DiskSpeed is what the disk under the data directory did in a short
// benchmark of the operations the commit log uses.
type DiskSpeed struct {
	SeqWriteMBps float64 // 1 MiB writes, fdatasync every 64 MiB
	// fdatasync after a 4 KiB write: into preallocated (unwritten) space,
	// as a fresh log segment, and into space already written with zeros,
	// as a pre-zeroed one
	SyncFreshP50, SyncFreshP99   time.Duration
	SyncZeroedP50, SyncZeroedP99 time.Duration
	SyncFreshOps, SyncZeroedOps  int
}

const area = 64 << 20

// MeasureDisk benchmarks dir's disk for about budget (split between the
// three tests) with a temporary file it removes.
func MeasureDisk(dir string, budget time.Duration) (DiskSpeed, error) {
	var r DiskSpeed
	part := budget / 3
	f, err := os.CreateTemp(dir, ".stasktune-*")
	if err != nil {
		return r, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	fd := int(f.Fd())

	// sequential write bandwidth
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i*131 + 7) // not all zeros: some file systems special-case them
	}
	var n int64
	t0 := time.Now()
	for time.Since(t0) < part && n < 4<<30 {
		if _, err := f.Write(buf); err != nil {
			return r, fmt.Errorf("write: %w", err)
		}
		if n += int64(len(buf)); n%(64<<20) == 0 {
			if err := syscall.Fdatasync(fd); err != nil {
				return r, err
			}
		}
	}
	if err := syscall.Fdatasync(fd); err != nil {
		return r, err
	}
	r.SeqWriteMBps = float64(n) / (1 << 20) / time.Since(t0).Seconds()

	syncs := func(name string, zero bool) (p50, p99 time.Duration, ops int, err error) {
		g, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return 0, 0, 0, err
		}
		defer os.Remove(g.Name())
		defer g.Close()
		gfd := int(g.Fd())
		if zero {
			z := make([]byte, 1<<20)
			for off := int64(0); off < area; off += int64(len(z)) {
				if _, err := g.WriteAt(z, off); err != nil {
					return 0, 0, 0, err
				}
			}
		} else if err := syscall.Fallocate(gfd, 0, 0, area); err != nil {
			return 0, 0, 0, fmt.Errorf("fallocate: %w", err)
		}
		if err := syscall.Fdatasync(gfd); err != nil {
			return 0, 0, 0, err
		}
		page := buf[:4096]
		var lat []time.Duration
		start := time.Now()
		for off := int64(0); off < area && time.Since(start) < part; off += 4096 {
			t := time.Now()
			if _, err := g.WriteAt(page, off); err != nil {
				return 0, 0, 0, err
			}
			if err := syscall.Fdatasync(gfd); err != nil {
				return 0, 0, 0, err
			}
			lat = append(lat, time.Since(t))
		}
		slices.Sort(lat)
		if len(lat) == 0 {
			return 0, 0, 0, fmt.Errorf("no samples")
		}
		return lat[len(lat)/2], lat[min(len(lat)-1, len(lat)*99/100)], len(lat), nil
	}
	if r.SyncFreshP50, r.SyncFreshP99, r.SyncFreshOps, err = syncs(".stasktune-fresh", false); err != nil {
		return r, err
	}
	r.SyncZeroedP50, r.SyncZeroedP99, r.SyncZeroedOps, err = syncs(".stasktune-zeroed", true)
	return r, err
}
