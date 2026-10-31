package cluster

// Evidence for the choice of the HA path (docs/21 section 1): with an
// asynchronous replica, a leader that dies loses writes it had acknowledged.

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAsyncReplicaLosesAckedWrites: leader + asynchronous replica
// (-repl-addr / -replica-of); writers acknowledge rows over HTTP; the leader
// is killed with SIGKILL; the replica is promoted (restarted without
// -replica-of); the acknowledged rows missing there are counted. The test
// reports the numbers and only requires the harness to have worked: the loss
// is a property of asynchronous replication, the number varies from run to run.
func TestAsyncReplicaLosesAckedWrites(t *testing.T) {
	bin := staskdBin(t)
	trials := envInt("STASK_ASYNC_TRIALS", 4)
	totalAcked, totalLost := 0, 0
	for trial := 0; trial < trials; trial++ {
		base := freeBase(t, 4)
		dir := t.TempDir()
		lead := &tnode{dir: filepath.Join(dir, "l"), log: filepath.Join(dir, "l.log"), http: fmt.Sprintf("127.0.0.1:%d", base)}
		repA := fmt.Sprintf("127.0.0.1:%d", base+1)
		rep := &tnode{dir: filepath.Join(dir, "r"), log: filepath.Join(dir, "r.log"), http: fmt.Sprintf("127.0.0.1:%d", base+2)}
		c := newCluster0(t, bin, lead, rep)
		lead.args = []string{"-dir", lead.dir, "-addr", lead.http, "-pg-addr", "", "-segment-mb", "8", "-prezero=false", "-gc=false", "-repl-addr", repA}
		rep.args = []string{"-dir", rep.dir, "-addr", rep.http, "-pg-addr", "", "-segment-mb", "8", "-prezero=false", "-gc=false", "-replica-of", repA}
		c.start(lead)
		waitHTTP(t, c, lead)
		c.start(rep)
		// the table
		body := `{"name":"t","columns":[{"name":"id","type":"string"},{"name":"v","type":"int"}],"key":"id"}`
		resp, err := c.client.Post("http://"+lead.http+"/v1/tables", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		w := &writers{acked: map[string]int{}, unknown: map[string]int{}, stop: make(chan struct{})}
		var wg sync.WaitGroup
		for k := 0; k < 8; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				for n := 0; ; n++ {
					select {
					case <-w.stop:
						return
					default:
					}
					key := fmt.Sprintf("t%d-%d-%d", trial, k, n)
					if c.writeTo(0, key, n) == acked {
						w.mu.Lock()
						w.acked[key] = n
						w.mu.Unlock()
					}
				}
			}(k)
		}
		for w.count() < 400 {
			time.Sleep(10 * time.Millisecond)
		}
		c.kill(lead)
		close(w.stop)
		wg.Wait()
		c.kill(rep)
		// promote: the replica's directory opened as a leader
		rep.args = []string{"-dir", rep.dir, "-addr", rep.http, "-pg-addr", "", "-segment-mb", "8", "-prezero=false", "-gc=false"}
		c.start(rep)
		waitHTTP(t, c, rep)
		lost := 0
		for k, v := range w.acked {
			if got, found, err := c.get(1, k); err != nil || !found || got != v {
				lost++
			}
		}
		t.Logf("trial %d: %d acknowledged writes, %d missing on the promoted replica", trial, len(w.acked), lost)
		totalAcked += len(w.acked)
		totalLost += lost
		c.kill(rep)
	}
	t.Logf("asynchronous replica, %d trials: %d of %d acknowledged writes lost at failover (the Raft cluster: 0 of 88.981 in TestProcChaos)", trials, totalLost, totalAcked)
}

func waitHTTP(t *testing.T, c *tcluster, nd *tnode) {
	t.Helper()
	end := time.Now().Add(30 * time.Second)
	for time.Now().Before(end) {
		resp, err := c.client.Get("http://" + nd.http + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not come up", nd.http)
}
