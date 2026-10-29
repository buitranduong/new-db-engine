package sql

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// CPU groups (Linux cgroup v2). Nice values weigh threads one by one, so
// the heavy lane's share of a saturated CPU shrinks as more foreground
// threads compete, down to almost nothing: an aggregate then starves.
// Instead, as ScyllaDB's scheduling groups do, staskd can put its threads
// into two threaded cgroups whose cpu.weight splits a saturated CPU between
// the groups whatever their thread counts: foreground (sessions, commit
// pipeline, GC) and heavy (the heavy lane). An idle group's share goes to
// the other, so a lone aggregate still uses every CPU.
//
// It needs a cgroup v2 directory the process may write, with the cpu
// controller available (e.g. staskd started under
// systemd-run --user --scope -p Delegate=yes). Without one, the heavy lane
// falls back to nice values.

var (
	heavyGroup string // cgroup.threads of the heavy group; "" = groups off
	heavyMax   string // cpu.max of the heavy group
)

// StartCPUGroups moves every thread of the process into a foreground group
// of weight fgWeight and prepares a heavy group of weight heavyWeight that
// heavy-lane threads join (StartHeavyLane must run after it). It reports
// why it could not.
func StartCPUGroups(fgWeight, heavyWeight int) error {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "0::") {
		return fmt.Errorf("not cgroup v2: %q", line)
	}
	dir := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
	if c, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers")); err != nil || !slices.Contains(strings.Fields(string(c)), "cpu") {
		return fmt.Errorf("cpu controller not available in %s", dir)
	}
	write := func(f, v string) error {
		if err := os.WriteFile(f, []byte(v), 0); err != nil {
			return fmt.Errorf("write %s: %w", f, err)
		}
		return nil
	}
	fg, heavy := filepath.Join(dir, "stask-fg"), filepath.Join(dir, "stask-heavy")
	for _, g := range []string{fg, heavy} {
		if err := os.Mkdir(g, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		if err := write(filepath.Join(g, "cgroup.type"), "threaded"); err != nil {
			return err
		}
	}
	if err := write(filepath.Join(dir, "cgroup.subtree_control"), "+cpu"); err != nil {
		return err
	}
	if err := write(filepath.Join(fg, "cpu.weight"), strconv.Itoa(fgWeight)); err != nil {
		return err
	}
	if err := write(filepath.Join(heavy, "cpu.weight"), strconv.Itoa(heavyWeight)); err != nil {
		return err
	}
	// threads created later inherit the group of the thread that creates
	// them, so moving every current thread moves the process
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if err := write(filepath.Join(fg, "cgroup.threads"), t.Name()); err != nil && !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such process") {
			return err
		}
	}
	heavyGroup = filepath.Join(heavy, "cgroup.threads")
	heavyMax = filepath.Join(heavy, "cpu.max")
	return nil
}

// joinHeavyGroup moves the calling OS thread (locked by the caller) into
// the heavy group; it reports whether it did.
func joinHeavyGroup() bool {
	if heavyGroup == "" {
		return false
	}
	if err := os.WriteFile(heavyGroup, []byte(strconv.Itoa(syscall.Gettid())), 0); err != nil {
		fmt.Printf("heavy lane: join cpu group: %v\n", err)
		return false
	}
	return true
}

// capHeavy limits the heavy group to cores CPUs (0 = no limit). Weights
// alone do not protect foreground work on a saturated machine: whenever
// foreground threads wait (for clients, for fsync) the heavy group takes
// the CPU, and so does work that the foreground needs moments later. As
// CockroachDB's elastic CPU limiter does for background work, a hard cap
// applies while the CPU is overloaded.
//
// The cap is enforced per period: a group that spends its quota early is
// stopped until the period ends, and a stopped heavy thread may hold work
// that foreground statements wait for (a multi-row write in the commit
// pipeline). A short period (HeavyCapPeriod) bounds such a stall.
func capHeavy(cores float64) {
	if heavyMax == "" {
		return
	}
	period := max(1000, int(HeavyCapPeriod.Microseconds()))
	v := fmt.Sprintf("max %d", period)
	if cores > 0 {
		v = fmt.Sprintf("%d %d", max(1000, int(cores*float64(period))), period)
	}
	if err := os.WriteFile(heavyMax, []byte(v), 0); err != nil {
		fmt.Printf("heavy lane: cpu.max: %v\n", err)
	}
}
