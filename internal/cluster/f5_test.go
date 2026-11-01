package cluster

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// F5: a leader that cannot allocate the next log segment used to stay leader
// (its heartbeats kept the healthy majority from electing another) and
// refused every write until an operator stopped it. It now fail-stops after
// -raft-alloc-fail-grace, like a Raft storage failure (CockroachDB fatals
// on a stalled disk, etcd panics, PostgreSQL PANICs on a WAL failure), and
// the other two nodes elect a leader that accepts writes. Process test:
// the disk-full condition is the injected create failure of -enable-faults.
func TestLeaderThatCannotAllocateSegmentStopsAndClusterGoesOn(t *testing.T) {
	c := newCluster(t, 3, "-enable-faults", "-segment-mb", "1", "-raft-alloc-fail-grace", "2s")
	c.createTable()
	li, _ := c.waitLeader(30 * time.Second)
	old := c.nodes[li]
	resp, err := c.client.Post("http://"+old.http+"/v1/admin/fault", "application/json", strings.NewReader(`{"create_enospc":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fault endpoint: %d", resp.StatusCode)
	}
	pad := strings.Repeat("x", 3000)
	var ackedKeys []string
	failed := 0
	deadline := time.Now().Add(40 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		key := pad + strconv.Itoa(i)
		if c.writeTo(li, key, i) == acked {
			ackedKeys = append(ackedKeys, key)
		} else {
			failed++
		}
		if !c.running(old) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if failed == 0 {
		t.Fatal("the segment never filled: the test did not reach the failure")
	}
	end := time.Now().Add(30 * time.Second)
	for c.running(old) && time.Now().Before(end) {
		time.Sleep(100 * time.Millisecond)
	}
	if c.running(old) {
		t.Fatalf("the leader that cannot allocate a log segment is still running after %d failed writes", failed)
	}
	nl, _ := c.waitLeader(30 * time.Second)
	if nl == li {
		t.Fatalf("leader is still node %d", li)
	}
	if o := c.write("after", 1, 20*time.Second); o != acked {
		t.Fatalf("write after the stop: outcome %v", o)
	}
	for _, k := range ackedKeys[:min(5, len(ackedKeys))] {
		for i := range c.nodes {
			if i == li {
				continue
			}
			if _, found, err := c.get(i, k); err != nil || !found {
				t.Fatalf("acknowledged key missing on node %d: found=%v err=%v", i, found, err)
			}
		}
	}
	t.Logf("%d writes acknowledged, %d refused; the leader stopped, node %d took over and accepts writes", len(ackedKeys), failed, nl)
}
