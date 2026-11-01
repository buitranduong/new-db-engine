package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// reconcileHistory returns the status of the "history" structure of table t
// on node nd and the node's verdict.
func (c *tcluster) reconcileHistory(nd *tnode) (hist, verdict string, err error) {
	resp, err := c.client.Post("http://"+nd.http+"/v1/admin/reconcile?table=t&sample=1", "application/json", strings.NewReader(""))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("reconcile: %d %s", resp.StatusCode, b)
	}
	var rep struct {
		Verdict string
		Tables  []struct {
			Structures []struct{ Name, Status, Detail string }
		}
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return "", "", err
	}
	for _, tb := range rep.Tables {
		for _, s := range tb.Structures {
			if s.Name == "history" {
				return s.Status + " " + s.Detail, rep.Verdict, nil
			}
		}
	}
	return "", rep.Verdict, fmt.Errorf("no history structure in %s", b)
}

func (c *tcluster) historyLen(i int, key string) (int, error) {
	resp, err := c.client.Get("http://" + c.snap()[i].http + "/v1/history?table=t&limit=5000&key=" + key)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var r struct{ Versions []json.RawMessage }
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, err
	}
	return len(r.Versions), nil
}

func (c *tcluster) waitHistory(want int) {
	c.t.Helper()
	end := time.Now().Add(60 * time.Second)
	var last string
	for {
		ok := true
		last = ""
		for i, nd := range c.nodes {
			n, err := c.historyLen(i, "a")
			h, v, herr := c.reconcileHistory(nd)
			last += fmt.Sprintf("[node %d: %d versions; history=%q verdict=%s err=%v %v] ", nd.id, n, h, v, err, herr)
			if err != nil || herr != nil || n != want || !strings.HasPrefix(h, "IN_SYNC") {
				ok = false
			}
		}
		if ok {
			return
		}
		if time.Now().After(end) {
			c.t.Fatalf("history store not in sync on every node within 60 s: %s", last)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// F9: the history store on Raft nodes. It used to be refused at start. Every
// node keeps its own store from the committed log; after writes, a leader
// kill and the old leader's return (it restarts and cuts its log) all three
// stores equal the core (reconcile IN_SYNC with full sampling) and hold the
// whole chain of the key.
func TestClusterHistoryStore(t *testing.T) {
	c := newCluster(t, 3, "-hist-store", "-hist-store-tables", "t", "-hist-store-min-free-mb", "-1",
		"-raft-compact-entries", "50", "-raft-compact-keep", "20")
	c.createTable()
	for i := 1; i <= 50; i++ {
		if o := c.write("a", i, 20*time.Second); o != acked {
			t.Fatalf("write %d: %v", i, o)
		}
	}
	c.settle()
	c.waitHistory(50)
	li, _ := c.leader()
	c.kill(c.nodes[li])
	for i := 51; i <= 70; i++ {
		if o := c.write("a", i, 30*time.Second); o != acked {
			t.Fatalf("write %d after the leader was killed: %v", i, o)
		}
	}
	c.start(c.nodes[li])
	c.settle()
	c.waitHistory(70)

	// a member that lost its whole data directory (store included) refills
	// from a snapshot and builds its store from the log it received
	vi := (li + 1) % 3
	c.kill(c.nodes[vi])
	if err := os.RemoveAll(c.nodes[vi].dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.nodes[vi].dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c.start(c.nodes[vi])
	for i := 71; i <= 80; i++ {
		if o := c.write("a", i, 30*time.Second); o != acked {
			t.Fatalf("write %d: %v", i, o)
		}
	}
	c.settle()
	c.waitHistory(80)
}
