package cluster

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pgInsert writes n rows (id prefix-k, 4 KB of padding) through the cluster in batches.
func (c *tcluster) pgInsert(prefix string, from, to int) {
	pad := strings.Repeat("x", 4000)
	for lo := from; lo < to; lo += 100 {
		hi := min(lo+100, to)
		var sb strings.Builder
		sb.WriteString("INSERT INTO big VALUES ")
		for k := lo; k < hi; k++ {
			if k > lo {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "('%s-%d', %d, '%s')", prefix, k, k, pad)
		}
		c.pgExecRW(sb.String())
	}
}

func pgCount(t *testing.T, addr, q string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgOnce(ctx, fmt.Sprintf("postgres://app@%s/stask?sslmode=disable&connect_timeout=3", addr))
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	defer conn.Close(ctx)
	var n int
	if err := conn.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// walSegs lists the segment numbers in dir (raw or compressed names).
func walSegs(dir string) []int {
	ents, _ := os.ReadDir(dir)
	var out []int
	for _, e := range ents {
		n := strings.TrimSuffix(strings.TrimSuffix(e.Name(), "z"), ".wal")
		if v, err := strconv.Atoi(n); err == nil && strings.HasSuffix(strings.TrimSuffix(e.Name(), "z"), ".wal") {
			out = append(out, v)
		}
	}
	return out
}

// TestHAClusterPITR: point-in-time recovery from a cluster. Every node
// archives the committed log segments (-archive-dir, one directory per
// node); a cluster backup (staskctl backup -cluster) is the base; the
// database is restored as a standalone node with the archive of ANY node and
// -recovery-target-time, and holds exactly what was committed before the
// target: not the rows written after it, and the archives of different
// nodes are byte-identical.
func TestHAClusterPITR(t *testing.T) {
	arch := t.TempDir()
	c := newClusterPre(t, 3, func(c *tcluster) {
		for i, nd := range c.nodes {
			nd.args = append(nd.args, "-archive-dir", filepath.Join(arch, fmt.Sprintf("a%d", i+1)))
		}
	}, "-segment-mb", "2")
	ctl := buildCtl(t)
	c.createTable()
	c.waitLeader(40 * time.Second)
	c.pgExecRW("CREATE TABLE big (id text PRIMARY KEY, v int, pad text)")
	c.pgInsert("a", 0, 500)

	var raftAddrs []string
	for _, nd := range c.nodes {
		raftAddrs = append(raftAddrs, nd.raft)
	}
	bout := t.TempDir()
	b, err := exec.Command(ctl, "backup", "-cluster", "-from", strings.Join(raftAddrs, ","), "-out", bout).Output()
	if err != nil {
		t.Fatalf("staskctl backup: %v", err)
	}
	backup := strings.TrimSpace(string(b))

	c.pgInsert("b", 0, 500)
	time.Sleep(100 * time.Millisecond)
	t1 := time.Now()
	time.Sleep(100 * time.Millisecond)
	c.pgInsert("b", 500, 1000)
	time.Sleep(100 * time.Millisecond)
	t2 := time.Now()
	time.Sleep(100 * time.Millisecond)
	c.pgInsert("c", 0, 1200) // after the target; rolls the segments past it
	c.settle()

	// wait until every node archived every segment below its newest one
	end := time.Now().Add(60 * time.Second)
	for {
		done := true
		for i, nd := range c.nodes {
			segs := walSegs(filepath.Join(nd.dir, "wal"))
			if len(segs) == 0 {
				done = false
				continue
			}
			top := 0
			for _, s := range segs {
				top = max(top, s)
			}
			have := map[int]bool{}
			for _, s := range walSegs(filepath.Join(arch, fmt.Sprintf("a%d", i+1))) {
				have[s] = true
			}
			for s := 0; s < top; s++ {
				if !have[s] {
					done = false
				}
			}
			if top < 3 {
				done = false // the test needs several sealed segments
			}
		}
		if done {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the archives did not catch up with the sealed segments")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// identical bytes on every node
	a1, a2, a3 := filepath.Join(arch, "a1"), filepath.Join(arch, "a2"), filepath.Join(arch, "a3")
	common := 0
	for _, s := range walSegs(a1) {
		name := fmt.Sprintf("%08d.wal", s)
		x, err1 := os.ReadFile(filepath.Join(a1, name))
		for _, o := range []string{a2, a3} {
			y, err2 := os.ReadFile(filepath.Join(o, name))
			if err1 != nil || err2 != nil {
				continue
			}
			common++
			if !bytes.Equal(x, y) {
				t.Errorf("archived segment %s differs between nodes (%s)", name, o)
			}
		}
	}
	if common == 0 {
		t.Fatal("no archived segment exists on two nodes")
	}

	restore := func(name string, target time.Time, archive string) (string, func()) {
		dir := filepath.Join(t.TempDir(), name)
		if out, err := exec.Command(ctl, "restore", "-backup", backup, "-dir", dir, "-standalone").CombinedOutput(); err != nil {
			t.Fatalf("staskctl restore -standalone: %v\n%s", err, out)
		}
		base := freeBase(t, 2)
		httpA, pgA := fmt.Sprintf("127.0.0.1:%d", base), fmt.Sprintf("127.0.0.1:%d", base+1)
		lf, _ := os.Create(dir + ".log")
		cmd := exec.Command(c.bin, "-dir", dir, "-addr", httpA, "-pg-addr", pgA, "-segment-mb", "2", "-prezero=false", "-gc=false",
			"-restore-archive", archive, "-recovery-target-time", target.UTC().Format(time.RFC3339Nano))
		cmd.Stdout, cmd.Stderr = lf, lf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stop := func() { cmd.Process.Kill(); cmd.Wait(); lf.Close() }
		t.Cleanup(func() {
			if t.Failed() {
				b, _ := os.ReadFile(dir + ".log")
				if len(b) > 4000 {
					b = b[len(b)-4000:]
				}
				t.Logf("---- %s log ----\n%s", name, b)
			}
		})
		end := time.Now().Add(60 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			conn, err := pgOnce(ctx, fmt.Sprintf("postgres://app@%s/stask?sslmode=disable&connect_timeout=2", pgA))
			if err == nil {
				conn.Close(ctx)
				cancel()
				break
			}
			cancel()
			if time.Now().After(end) {
				stop()
				t.Fatalf("restored node %s did not come up: %v", name, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		return pgA, stop
	}

	for _, tc := range []struct {
		name    string
		target  time.Time
		archive string
		a, b, c int
	}{
		{"t1", t1, a1, 500, 500, 0},
		{"t2", t2, a2, 500, 1000, 0},
		{"t2-other-node", t2, a3, 500, 1000, 0},
	} {
		addr, stop := restore(tc.name, tc.target, tc.archive)
		ga := pgCount(t, addr, "SELECT count(*) FROM big WHERE id LIKE 'a-%'")
		gb := pgCount(t, addr, "SELECT count(*) FROM big WHERE id LIKE 'b-%'")
		gc := pgCount(t, addr, "SELECT count(*) FROM big WHERE id LIKE 'c-%'")
		if ga != tc.a || gb != tc.b || gc != tc.c {
			t.Errorf("%s: restored rows a=%d b=%d c=%d, want %d %d %d", tc.name, ga, gb, gc, tc.a, tc.b, tc.c)
		}
		t.Logf("%s: restored from a cluster backup + the archive of one node: a=%d b=%d c=%d (target %s)", tc.name, ga, gb, gc, tc.target.UTC().Format(time.RFC3339Nano))
		if tc.name == "t2" {
			// the restored database takes writes
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			conn, err := pgOnce(ctx, fmt.Sprintf("postgres://app@%s/stask?sslmode=disable", addr))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, "INSERT INTO big VALUES ('after', 1, 'p')"); err != nil {
				t.Errorf("write to the restored database: %v", err)
			}
			conn.Close(ctx)
			cancel()
			if n := pgCount(t, addr, "SELECT count(*) FROM big WHERE id = 'after'"); n != 1 {
				t.Errorf("the restored database lost its own write")
			}
		}
		stop()
	}
	// a target before the backup's checkpoint is refused or yields nothing
	// newer than the backup: not asserted here (single-node behaviour).
}
