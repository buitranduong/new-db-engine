package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// buildCtl builds staskctl once per test.
func buildCtl(t *testing.T) string {
	ctl := filepath.Join(t.TempDir(), "staskctl")
	if out, err := exec.Command("go", "build", "-o", ctl, "../../cmd/staskctl").CombinedOutput(); err != nil {
		t.Fatalf("go build staskctl: %v\n%s", err, out)
	}
	return ctl
}

// newJoiner prepares a node that is new to the cluster: its own ports, no
// proxies in front of it (the leader dials the address it is given), and
// proxies from it to the existing nodes. It is not started.
func (c *tcluster) newJoiner() *tnode {
	cur := c.snap()
	idx := len(cur)
	base := freeBase(c.t, 3+len(cur))
	nd := &tnode{id: uint64(idx + 1), dir: filepath.Join(filepath.Dir(cur[0].dir), fmt.Sprintf("n%d", idx+1)),
		log:  filepath.Join(filepath.Dir(cur[0].dir), fmt.Sprintf("n%d.log", idx+1)),
		http: fmt.Sprintf("127.0.0.1:%d", base), raft: fmt.Sprintf("127.0.0.1:%d", base+1), pg: fmt.Sprintf("127.0.0.1:%d", base+2)}
	var peers []string
	for j, o := range cur {
		p, err := newProxy(fmt.Sprintf("127.0.0.1:%d", base+3+j), o.raft)
		if err != nil {
			c.t.Fatal(err)
		}
		c.mu.Lock()
		c.proxies[[2]int{idx, j}] = p
		c.mu.Unlock()
		peers = append(peers, fmt.Sprintf("%d=%s@%s", o.id, p.ln.Addr().String(), o.http))
	}
	peers = append(peers, fmt.Sprintf("%d=%s@%s", nd.id, nd.raft, nd.http))
	nd.args = append([]string{"-dir", nd.dir, "-addr", nd.http, "-pg-addr", nd.pg, "-segment-mb", "8", "-prezero=false", "-gc=false",
		"-raft-id", fmt.Sprint(nd.id), "-raft-listen", nd.raft, "-raft-peers", strings.Join(peers, ","), "-raft-join"}, c.extra...)
	next := append(append(make([]*tnode, 0, len(cur)+1), cur...), nd)
	c.mu.Lock()
	c.nodes = next
	c.mu.Unlock()
	return nd
}

func (c *tcluster) httpList() string {
	var u []string
	for _, nd := range c.snap() {
		u = append(u, "http://"+nd.http)
	}
	return strings.Join(u, ",")
}

// waitVoters waits until every running node in idx reports exactly the voters
// (sorted ids), is caught up with the leader's commit and agrees on the
// durable LSN.
func (c *tcluster) waitVoters(idx []int, want string, within time.Duration) {
	c.t.Helper()
	end := time.Now().Add(within)
	var last string
	for time.Now().Before(end) {
		_, ls := c.leader()
		ok := ls.ID != 0
		last = ""
		for _, i := range idx {
			s, up := c.status(c.nodes[i])
			if !up || fmt.Sprint(s.Voters) != want || s.Applied != ls.Commit {
				ok = false
			}
			last += fmt.Sprintf(" [node %d up=%v voters=%v applied=%d]", c.nodes[i].id, up, s.Voters, s.Applied)
		}
		if ok {
			l0 := c.durableLSN(idx[0])
			same := true
			for _, i := range idx {
				if c.durableLSN(i) != l0 {
					same = false
				}
			}
			if same {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("voters %s not reached on %v within %v; leader commit %v;%s", want, idx, within, func() uint64 { _, s := c.leader(); return s.Commit }(), last)
}

func (c *tcluster) waitGet(i int, key string, v int) {
	c.t.Helper()
	end := time.Now().Add(30 * time.Second)
	for time.Now().Before(end) {
		if got, ok, err := c.get(i, key); err == nil && ok && got == v {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatalf("node %d never had %s=%d", c.nodes[i].id, key, v)
}

// TestHAMembership: a 3-node cluster takes writes; a fourth node is started
// with -raft-join and added while writers run (staskctl cluster add); it
// holds every acknowledged write; then a node is removed, and the cluster of
// three (majority 2) commits with another voter killed, which it could not
// if the removed node still counted. Finally the leader itself is removed.
func TestHAMembership(t *testing.T) {
	c := newCluster(t, 3)
	ctl := buildCtl(t)
	c.createTable()
	c.waitLeader(40 * time.Second)
	for i := 0; i < 30; i++ {
		if o := c.write(fmt.Sprintf("pre-%d", i), i, 20*time.Second); o != acked {
			t.Fatalf("pre-write %d: %v", i, o)
		}
	}
	w := c.startWriters(3, "m")
	for w.count() < 60 {
		time.Sleep(20 * time.Millisecond)
	}
	// ---- add node 4
	n4 := c.newJoiner()
	c.start(n4)
	end := time.Now().Add(30 * time.Second)
	for {
		if s, ok := c.status(n4); ok {
			if s.Member || len(s.Voters) != 3 {
				t.Fatalf("a joining node must start as a non-voter of the old cluster: member=%v voters=%v", s.Member, s.Voters)
			}
			break
		}
		if time.Now().After(end) {
			t.Fatal("node 4 did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// adding an unreachable address is refused
	out, err := exec.Command(ctl, "cluster", "add", "-http", c.httpList(), "-id", "9", "-raft", "127.0.0.1:1").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not reachable") {
		t.Fatalf("add of an unreachable node: err=%v out=%s", err, out)
	}
	if out, err := exec.Command(ctl, "cluster", "add", "-http", c.httpList(), "-id", "4", "-raft", n4.raft, "-node-http", n4.http).CombinedOutput(); err != nil {
		t.Fatalf("staskctl cluster add: %v\n%s", err, out)
	}
	// adding it again is refused
	if out, err := exec.Command(ctl, "cluster", "add", "-http", c.httpList(), "-id", "4", "-raft", n4.raft).CombinedOutput(); err == nil {
		t.Fatalf("a second add of node 4 was accepted: %s", out)
	}
	for k := w.count() + 60; w.count() < k; {
		time.Sleep(20 * time.Millisecond)
	}
	w.finish()
	c.waitVoters([]int{0, 1, 2, 3}, "[1 2 3 4]", 60*time.Second)
	present, up := c.verify(w)
	t.Logf("added node 4 under load: %d acknowledged writes on all 4 nodes, %d unknown outcomes (%d present)", present, len(w.unknown), up)
	for i := 0; i < 30; i++ {
		c.waitGet(3, fmt.Sprintf("pre-%d", i), i)
	}
	// with 4 voters the majority is 3: the new node takes part in commits.
	// Kill two of the old nodes: no commit is possible.
	li, _ := c.waitLeader(30 * time.Second)
	var others []int
	for i := 0; i < 3; i++ {
		if i != li {
			others = append(others, i)
		}
	}
	c.kill(c.nodes[others[0]])
	if o := c.write("four-3of4", 1, 20*time.Second); o != acked {
		t.Fatalf("3 of 4 voters up: write %v", o)
	}
	c.kill(c.nodes[others[1]])
	if o := c.writeTo(li, "four-2of4", 1); o == acked {
		t.Fatal("a write was acknowledged with only 2 of 4 voters up")
	}
	c.start(c.nodes[others[0]])
	c.start(c.nodes[others[1]])
	c.waitVoters([]int{0, 1, 2, 3}, "[1 2 3 4]", 60*time.Second)

	// ---- remove a follower
	li, _ = c.waitLeader(30 * time.Second)
	victim := -1
	for i := 0; i < 4; i++ {
		if i != li {
			victim = i
			break
		}
	}
	if out, err := exec.Command(ctl, "cluster", "remove", "-http", c.httpList(), "-id", fmt.Sprint(c.nodes[victim].id)).CombinedOutput(); err != nil {
		t.Fatalf("staskctl cluster remove: %v\n%s", err, out)
	}
	var keep []int
	for i := 0; i < 4; i++ {
		if i != victim {
			keep = append(keep, i)
		}
	}
	vs := make([]string, 0, 3)
	for _, i := range keep {
		vs = append(vs, fmt.Sprint(c.nodes[i].id))
	}
	sort.Strings(vs)
	want := "[" + strings.Join(vs, " ") + "]"
	c.waitVoters(keep, want, 60*time.Second)
	c.kill(c.nodes[victim])
	// two of the three remaining voters are enough: kill one more
	li, _ = c.waitLeader(30 * time.Second)
	for _, i := range keep {
		if i != li {
			c.kill(c.nodes[i])
			break
		}
	}
	if o := c.writeTo(li, "three-2of3", 7); o != acked {
		// the leader may have changed; use the generic path
		if o2 := c.write("three-2of3", 7, 30*time.Second); o2 != acked {
			t.Fatalf("2 of 3 voters up after the removal: write %v", o2)
		}
	}
	t.Logf("removed node %d: the 3 voters left commit with one of them down", c.nodes[victim].id)
	// bring the killed voter back, then remove the leader itself
	for _, i := range keep {
		if !c.running(c.nodes[i]) {
			c.start(c.nodes[i])
		}
	}
	c.waitVoters(keep, want, 60*time.Second)
	li, _ = c.waitLeader(30 * time.Second)
	if out, err := exec.Command(ctl, "cluster", "remove", "-http", c.httpList(), "-id", fmt.Sprint(c.nodes[li].id)).CombinedOutput(); err != nil {
		t.Fatalf("remove of the leader: %v\n%s", err, out)
	}
	var rest []int
	for _, i := range keep {
		if i != li {
			rest = append(rest, i)
		}
	}
	vs = vs[:0]
	for _, i := range rest {
		vs = append(vs, fmt.Sprint(c.nodes[i].id))
	}
	sort.Strings(vs)
	want2 := "[" + strings.Join(vs, " ") + "]"
	c.waitVoters(rest, want2, 60*time.Second)
	if o := c.write("after-leader-removal", 1, 40*time.Second); o != acked {
		t.Fatalf("write after the leader removed itself: %v", o)
	}
	for _, i := range rest {
		c.waitGet(i, "after-leader-removal", 1)
		c.waitGet(i, "three-2of3", 7)
	}
	t.Logf("removed the leader (node %d): voters %s", c.nodes[li].id, want2)
	// a restart keeps the configuration (from the Raft log / snapshot)
	c.kill(c.nodes[rest[0]])
	c.start(c.nodes[rest[0]])
	c.waitVoters(rest, want2, 60*time.Second)
}

// TestHAMembershipSnapshotJoin: the Raft log is compacted (small
// -raft-compact-entries), so a node that joins gets a snapshot, which carries the
// configuration and the addresses; a node that was down while the cluster
// changed its membership and compacted catches up the same way.
func TestHAMembershipSnapshotJoin(t *testing.T) {
	c := newCluster(t, 3, "-raft-compact-entries", "40", "-raft-compact-keep", "10")
	ctl := buildCtl(t)
	c.createTable()
	c.waitLeader(40 * time.Second)
	for i := 0; i < 300; i++ {
		if o := c.write(fmt.Sprintf("s-%d", i), i, 20*time.Second); o != acked {
			t.Fatalf("write %d: %v", i, o)
		}
	}
	// Compaction runs in the background after the writes are applied, so wait
	// for it (a fast machine finishes the 300 writes before it has run) and keep
	// feeding entries, which is what triggers it.
	var ls Status
	for i, end := 0, time.Now().Add(60*time.Second); ; i++ {
		_, ls = c.waitLeader(30 * time.Second)
		if ls.First > 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the Raft log was not compacted (first index %d): the test does not exercise the snapshot path", ls.First)
		}
		if o := c.write(fmt.Sprintf("s-extra-%d", i), 1000+i, 20*time.Second); o != acked {
			t.Fatalf("extra write %d: %v", i, o)
		}
		time.Sleep(200 * time.Millisecond)
	}
	n4 := c.newJoiner()
	c.start(n4)
	end := time.Now().Add(30 * time.Second)
	for {
		if _, ok := c.status(n4); ok {
			break
		}
		if time.Now().After(end) {
			t.Fatal("node 4 did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out, err := exec.Command(ctl, "cluster", "add", "-http", c.httpList(), "-id", "4", "-raft", n4.raft, "-node-http", n4.http).CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	c.waitVoters([]int{0, 1, 2, 3}, "[1 2 3 4]", 90*time.Second)
	for i := 0; i < 300; i += 7 {
		c.waitGet(3, fmt.Sprintf("s-%d", i), i)
	}
	// node 4 learned the addresses of the others from the snapshot, and the
	// cluster still works after a restart of node 4
	c.term(n4)
	c.start(n4)
	c.waitVoters([]int{0, 1, 2, 3}, "[1 2 3 4]", 60*time.Second)
	if o := c.write("after-join", 1, 30*time.Second); o != acked {
		t.Fatalf("write after the join: %v", o)
	}
	c.waitGet(3, "after-join", 1)
	if b, _ := os.ReadFile(n4.log); !strings.Contains(string(b), "installed the snapshot") {
		t.Fatalf("node 4 did not join through a snapshot; log:\n%s", b)
	}
}
