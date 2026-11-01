package cluster

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// F10: a cluster member whose data directory was deleted restarts with the
// same id and no -raft-join. It used to stay empty for good (the leader
// believed it still had what it had acknowledged). It now refills from the
// leader: from the Raft log, or from a snapshot when that part is compacted
// (here compaction keeps 20 entries, so a snapshot is needed). Precedent:
// hashicorp/raft resets nextIndex on a rejected append; etcd and
// CockroachDB treat a wiped member as lost and replace it (documented
// procedure in docs/33 stays the safe one).
func TestWipedMemberRestartsWithSameID(t *testing.T) {
	c := newCluster(t, 3, "-raft-compact-entries", "50", "-raft-compact-keep", "20")
	c.createTable()
	for i := 0; i < 200; i++ {
		if o := c.write(fmt.Sprintf("k%d", i), i, 20*time.Second); o != acked {
			t.Fatalf("write %d: %v", i, o)
		}
	}
	c.settle()
	li, _ := c.leader()
	vi := (li + 1) % 3
	victim := c.nodes[vi]
	c.kill(victim)
	if err := os.RemoveAll(victim.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(victim.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c.start(victim)
	for i := 0; i < 20; i++ {
		if o := c.write(fmt.Sprintf("n%d", i), i, 20*time.Second); o != acked {
			t.Fatalf("write after the wipe %d: %v", i, o)
		}
	}
	end := time.Now().Add(60 * time.Second)
	for {
		v1, f1, e1 := c.get(vi, "k5")
		v2, f2, e2 := c.get(vi, "n19")
		if e1 == nil && e2 == nil && f1 && f2 && v1 == 5 && v2 == 19 {
			break
		}
		if time.Now().After(end) {
			b, _ := os.ReadFile(victim.log)
			t.Fatalf("the node with the deleted data directory is still empty after 60 s (k5 found=%v err=%v, n19 found=%v err=%v)\n%s", f1, e1, f2, e2, tailStr(string(b), 1500))
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.settle()
}

func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
