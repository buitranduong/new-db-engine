// Package tune finds what the machine (or container) gives the server, and
// measures the disk under the data directory, to choose settings that use
// all of it (cmd/stasktune writes them as a staskd.conf).
package tune

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// cgroupDir is the process's cgroup v2 directory, or "".
func cgroupDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "0::") {
		return ""
	}
	return filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
}

// eachLimit calls f with the contents of file in the process's cgroup and
// every ancestor (a limit anywhere above applies too).
func eachLimit(file string, f func(string)) {
	d := cgroupDir()
	for d != "" && strings.HasPrefix(d, "/sys/fs/cgroup") {
		if b, err := os.ReadFile(filepath.Join(d, file)); err == nil {
			f(strings.TrimSpace(string(b)))
		}
		if d == "/sys/fs/cgroup" {
			break
		}
		d = filepath.Dir(d)
	}
}

// CPUs is the CPU capacity the process may use: the CPUs it may run on,
// lowered by any cgroup quota (cpu.max), as containers set it.
func CPUs() float64 {
	n := float64(runtime.NumCPU())
	eachLimit("cpu.max", func(s string) {
		q, p, ok := strings.Cut(s, " ")
		if !ok || q == "max" {
			return
		}
		qv, err1 := strconv.ParseFloat(q, 64)
		pv, err2 := strconv.ParseFloat(p, 64)
		if err1 == nil && err2 == nil && pv > 0 {
			n = min(n, qv/pv)
		}
	})
	return n
}

// WholeCPUs is CPUs rounded up, at least 1.
func WholeCPUs() int { return max(1, int(math.Ceil(CPUs()))) }

// Memory is the memory the process may use in bytes: physical RAM lowered
// by any cgroup limit (memory.max), as containers set it.
func Memory() int64 {
	var total int64
	var si syscall.Sysinfo_t
	if syscall.Sysinfo(&si) == nil {
		total = int64(si.Totalram) * int64(si.Unit)
	}
	eachLimit("memory.max", func(s string) {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && (total == 0 || v < total) {
			total = v
		}
	})
	return total
}

// CPUGroupsPossible reports whether staskd could create its CPU groups
// (a writable cgroup v2 directory with the cpu controller), and why not.
func CPUGroupsPossible() (bool, string) {
	d := cgroupDir()
	if d == "" {
		return false, "no cgroup v2"
	}
	c, err := os.ReadFile(filepath.Join(d, "cgroup.controllers"))
	if err != nil || !slices.Contains(strings.Fields(string(c)), "cpu") {
		return false, "cpu controller not delegated to " + d
	}
	if syscall.Access(filepath.Join(d, "cgroup.subtree_control"), 2) != nil {
		return false, d + " is not writable"
	}
	return true, ""
}

// Disk describes the block device and file system under a directory.
type Disk struct {
	Device     string // e.g. nvme0n1, dm-0
	Rotational bool
	FS         string
	FreeBytes  int64
	SizeBytes  int64
}

// DiskOf finds the device and file system holding dir.
func DiskOf(dir string) Disk {
	var d Disk
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) == nil {
		d.FreeBytes = int64(st.Bavail) * int64(st.Bsize)
		d.SizeBytes = int64(st.Blocks) * int64(st.Bsize)
	}
	var fst syscall.Stat_t
	if syscall.Stat(dir, &fst) == nil {
		dev := uint64(fst.Dev)
		major, minor := (dev>>8)&0xfff|(dev>>32)&^0xfff, dev&0xff|(dev>>12)&^0xff
		if link, err := os.Readlink("/sys/dev/block/" + strconv.FormatUint(major, 10) + ":" + strconv.FormatUint(minor, 10)); err == nil {
			d.Device = filepath.Base(link)
			q := "/sys/class/block/" + d.Device
			if _, err := os.Stat(q + "/partition"); err == nil { // a partition: the queue is the parent's
				q = filepath.Dir(link)
				q = "/sys/class/block/" + filepath.Base(q)
			}
			if b, err := os.ReadFile(q + "/queue/rotational"); err == nil {
				d.Rotational = strings.TrimSpace(string(b)) == "1"
			}
		}
	}
	// file system type: the longest /proc/mounts entry that contains dir
	if f, err := os.Open("/proc/mounts"); err == nil {
		defer f.Close()
		abs, _ := filepath.Abs(dir)
		best := -1
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 3 {
				continue
			}
			if (abs == fs[1] || strings.HasPrefix(abs, strings.TrimSuffix(fs[1], "/")+"/")) && len(fs[1]) > best {
				best, d.FS = len(fs[1]), fs[2]
			}
		}
	}
	return d
}
