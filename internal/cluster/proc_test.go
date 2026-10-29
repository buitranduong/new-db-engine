package cluster

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"stask/internal/router"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[min(len(ds)-1, int(float64(len(ds))*p))]
}

// TestProcChaos: three real processes, clients writing all the time, and
// rounds of kill -9 (leader, follower, two nodes), partitions through the
// fault proxies (leader cut off, follower cut off, only the leader's
// outgoing links cut). After every round the cluster is healed and
// converged and every write that was acknowledged must be on all nodes.
func TestProcChaos(t *testing.T) {
	runs, rounds := envInt("STASK_CHAOS_RUNS", 2), envInt("STASK_CHAOS_ROUNDS", 8)
	var totAcked, totUnknown, totUnknownPresent int
	for run := 0; run < runs; run++ {
		c := newCluster(t, 3, "-raft-compact-entries", "300", "-raft-compact-keep", "100")
		c.createTable()
		w := c.startWriters(6, fmt.Sprintf("r%d", run))
		actions := map[string]int{}
		for round := 0; round < rounds && !t.Failed(); round++ {
			li, _ := c.waitLeader(40 * time.Second)
			var fi []int
			for i := range c.nodes {
				if i != li {
					fi = append(fi, i)
				}
			}
			f := fi[c.rng.Intn(len(fi))]
			time.Sleep(time.Duration(300+c.rng.Intn(1200)) * time.Millisecond)
			var act string
			switch c.rng.Intn(7) {
			case 0:
				act = "kill -9 leader"
				c.kill(c.nodes[li])
				time.Sleep(time.Duration(500+c.rng.Intn(2500)) * time.Millisecond)
				c.start(c.nodes[li])
			case 1:
				act = "kill -9 follower"
				c.kill(c.nodes[f])
				time.Sleep(time.Duration(500+c.rng.Intn(2000)) * time.Millisecond)
				c.start(c.nodes[f])
			case 2:
				act = "partition leader"
				c.isolate(li, true)
				time.Sleep(time.Duration(3000+c.rng.Intn(3000)) * time.Millisecond)
				c.isolate(li, false)
			case 3:
				act = "partition follower"
				c.isolate(f, true)
				time.Sleep(time.Duration(1500+c.rng.Intn(2500)) * time.Millisecond)
				c.isolate(f, false)
			case 4:
				act = "kill -9 two nodes"
				c.kill(c.nodes[li])
				c.kill(c.nodes[f])
				time.Sleep(time.Duration(1000+c.rng.Intn(2000)) * time.Millisecond)
				c.start(c.nodes[f])
				time.Sleep(300 * time.Millisecond)
				c.start(c.nodes[li])
			case 5:
				act = "cut the leader's outgoing links"
				for j := range c.nodes {
					if j != li {
						c.proxies[[2]int{li, j}].setBlocked(true)
					}
				}
				time.Sleep(time.Duration(3000+c.rng.Intn(2000)) * time.Millisecond)
				c.heal()
			case 6:
				act = "cut the leader's incoming links"
				for j := range c.nodes {
					if j != li {
						c.proxies[[2]int{j, li}].setBlocked(true)
					}
				}
				time.Sleep(time.Duration(3000+c.rng.Intn(2000)) * time.Millisecond)
				c.heal()
			}
			actions[act]++
			c.waitLeader(40 * time.Second)
		}
		w.finish()
		c.settle()
		present, up := c.verify(w)
		totAcked += present
		totUnknown += len(w.unknown)
		totUnknownPresent += up
		t.Logf("run %d: %d rounds %v; %d acknowledged writes, all on all 3 nodes; %d with unknown outcome (%d of them present)",
			run, rounds, actions, present, len(w.unknown), up)
		for _, nd := range c.nodes {
			b, _ := os.ReadFile(nd.log)
			t.Logf("node %d: %d restarts-in-place, %d snapshots installed, %d compactions", nd.id,
				strings.Count(string(b), "restarts:"), strings.Count(string(b), "installed the snapshot"), strings.Count(string(b), "raft log compacted"))
		}
	}
	t.Logf("total: %d acknowledged writes, none lost; %d unknown outcomes (%d present)", totAcked, totUnknown, totUnknownPresent)
}

// TestProcFailoverTime: kill -9 the leader and measure, on the wall clock,
// the time until a write is acknowledged by the new leader.
func TestProcFailoverTime(t *testing.T) {
	n := envInt("STASK_FAILOVER_RUNS", 8)
	c := newCluster(t, 3)
	c.createTable()
	var ds, elect []time.Duration
	for run := 0; run < n; run++ {
		li, _ := c.waitLeader(30 * time.Second)
		for k := 0; k < 20; k++ {
			c.write(fmt.Sprintf("pre-%d-%d", run, k), k, 5*time.Second)
		}
		c.lead.Store(int32(li))
		t0 := time.Now()
		c.kill(c.nodes[li])
		// a poller notes when a survivor first reports itself writable leader
		var tElect time.Time
		stopPoll := make(chan struct{})
		polled := make(chan struct{})
		go func() {
			defer close(polled)
			for {
				select {
				case <-stopPoll:
					return
				default:
				}
				if ni, _ := c.leader(); ni >= 0 && ni != li {
					tElect = time.Now()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		// the client side: first acknowledged write on a survivor
		for {
			if c.write(fmt.Sprintf("post-%d", run), run, 200*time.Millisecond) == acked {
				break
			}
			if time.Since(t0) > 30*time.Second {
				t.Fatal("no write acknowledged within 30 s of killing the leader")
			}
		}
		close(stopPoll)
		<-polled
		ds = append(ds, time.Since(t0))
		if !tElect.IsZero() {
			elect = append(elect, tElect.Sub(t0))
		}
		c.start(c.nodes[li])
		c.settle()
	}
	t.Logf("kill -9 leader -> first acknowledged write on the new leader, %d runs (3 nodes, tick 50 ms, election 20 ticks = 1.0-2.0 s timeout): min %v  median %v  p90 %v  max %v",
		n, pct(ds, 0), pct(ds, 0.5), pct(ds, 0.9), pct(ds, 1))
	t.Logf("  of which until the new leader reports writable: median %v max %v", pct(elect, 0.5), pct(elect, 1))
}

// TestProcSnapshot: a follower is down while the leader writes past the
// compaction threshold, so the entries it needs are gone: it must fetch a
// snapshot and end with the same data.
func TestProcSnapshot(t *testing.T) {
	c := newCluster(t, 3, "-raft-compact-entries", "200", "-raft-compact-keep", "50")
	c.createTable()
	li, _ := c.waitLeader(20 * time.Second)
	f := (li + 1) % 3
	for k := 0; k < 30; k++ {
		c.write(fmt.Sprintf("a-%d", k), k, 5*time.Second)
	}
	c.kill(c.nodes[f])
	w := c.startWriters(4, "s")
	for w.count() < 1500 {
		time.Sleep(100 * time.Millisecond)
	}
	w.finish()
	var first uint64
	for i, nd := range c.nodes {
		if i != f {
			if s, ok := c.status(nd); ok && s.First > first {
				first = s.First
			}
		}
	}
	if first < 100 {
		t.Fatalf("the leader's Raft log was not compacted (first index %d)", first)
	}
	c.start(c.nodes[f])
	c.settle()
	b, _ := os.ReadFile(c.nodes[f].log)
	if !strings.Contains(string(b), "installed the snapshot") {
		t.Fatalf("node %d caught up without a snapshot", c.nodes[f].id)
	}
	present, _ := c.verify(w)
	t.Logf("follower down for %d writes, Raft log compacted to entry %d, it installed a snapshot and holds all %d acknowledged writes", w.count(), first, present)
}

// startSingle runs one staskd without Raft, with the same engine settings.
func startSingle(t *testing.T) (*tcluster, *tnode) {
	bin := staskdBin(t)
	base := freeBase(t, 2)
	dir := t.TempDir()
	nd := &tnode{id: 1, dir: filepath.Join(dir, "n1"), log: filepath.Join(dir, "n1.log"), http: fmt.Sprintf("127.0.0.1:%d", base)}
	nd.args = []string{"-dir", nd.dir, "-addr", nd.http, "-pg-addr", "", "-segment-mb", "8", "-prezero=false", "-gc=false"}
	c := newCluster0(t, bin, nd)
	c.start(nd)
	t.Cleanup(func() { c.kill(nd) })
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if _, err := c.client.Get("http://" + nd.http + "/v1/health"); err == nil {
			break
		}
	}
	c.createTable()
	return c, nd
}

// TestProcLatency: commit latency of one node against a 3-node cluster,
// one client and eight clients, on this shared machine (see the load
// average in the log line).
func TestProcLatency(t *testing.T) {
	n := envInt("STASK_LAT_WRITES", 1500)
	type res struct {
		p50, p99 time.Duration
		rate     float64
	}
	measure := func(c *tcluster, clients int, tag string) res {
		var mu sync.Mutex
		var lat []time.Duration
		var wg sync.WaitGroup
		t0 := time.Now()
		for g := 0; g < clients; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				var mine []time.Duration
				for k := 0; k < n/clients; k++ {
					s := time.Now()
					if c.write(fmt.Sprintf("%s-%d-%d", tag, g, k), k, 5*time.Second) == acked {
						mine = append(mine, time.Since(s))
					}
				}
				mu.Lock()
				lat = append(lat, mine...)
				mu.Unlock()
			}(g)
		}
		wg.Wait()
		el := time.Since(t0)
		return res{pct(lat, 0.5), pct(lat, 0.99), float64(len(lat)) / el.Seconds()}
	}
	load, _ := os.ReadFile("/proc/loadavg")
	one, _ := startSingle(t)
	cl := newCluster(t, 3)
	cl.createTable()
	cl.waitLeader(20 * time.Second)
	for i := 0; i < 50; i++ { // warm up
		one.write(fmt.Sprintf("w%d", i), i, 5*time.Second)
		cl.write(fmt.Sprintf("w%d", i), i, 5*time.Second)
	}
	for _, clients := range []int{1, 8} {
		a := measure(one, clients, fmt.Sprintf("a%d", clients))
		b := measure(cl, clients, fmt.Sprintf("b%d", clients))
		t.Logf("%d client(s), %d writes: single node p50 %v p99 %v %.0f/s | 3-node cluster p50 %v p99 %v %.0f/s | p50 ratio %.1fx",
			clients, n, a.p50.Round(10*time.Microsecond), a.p99.Round(10*time.Microsecond), a.rate,
			b.p50.Round(10*time.Microsecond), b.p99.Round(10*time.Microsecond), b.rate, float64(b.p50)/float64(a.p50))
	}
	t.Logf("machine load average at start: %s", strings.TrimSpace(string(load)))
	_ = exec.Command
}

// TestProcRouter: clients talk only to the router (staskroute -nodes); the
// leader is killed three times; the router finds the new leader and the
// clients keep writing. Every acknowledged write must be on all nodes.
func TestProcRouter(t *testing.T) {
	c := newCluster(t, 3)
	c.createTable()
	c.waitLeader(20 * time.Second)
	var urls []string
	for _, nd := range c.nodes {
		urls = append(urls, "http://"+nd.http)
	}
	rt, err := router.NewCluster(urls...)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(rt)
	defer front.Close()
	var mu sync.Mutex
	ackedKeys := map[string]int{}
	var lastAck time.Time
	var maxGap time.Duration
	errs := 0
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			cl := &http.Client{Timeout: 25 * time.Second}
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("rt-%d-%d", g, n)
				body := fmt.Sprintf(`{"ops":[{"op":"put","table":"t","row":{"id":%q,"v":%d}}]}`, key, n)
				resp, err := cl.Post(front.URL+"/v1/write", "application/json", strings.NewReader(body))
				mu.Lock()
				if err == nil && resp.StatusCode == 200 {
					now := time.Now()
					if !lastAck.IsZero() && now.Sub(lastAck) > maxGap {
						maxGap = now.Sub(lastAck)
					}
					lastAck = now
					ackedKeys[key] = n
				} else {
					errs++
				}
				mu.Unlock()
				if resp != nil {
					resp.Body.Close()
				}
			}
		}(g)
	}
	for cycle := 0; cycle < 3; cycle++ {
		time.Sleep(1500 * time.Millisecond)
		li, _ := c.waitLeader(30 * time.Second)
		mu.Lock()
		before := len(ackedKeys)
		mu.Unlock()
		c.kill(c.nodes[li])
		for end := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			mu.Lock()
			n := len(ackedKeys)
			mu.Unlock()
			if n > before+50 {
				break
			}
			if time.Now().After(end) {
				t.Fatal("clients did not resume through the router after the leader was killed")
			}
		}
		c.start(c.nodes[li])
	}
	close(stop)
	wg.Wait()
	c.settle()
	w := &writers{acked: ackedKeys, unknown: map[string]int{}}
	c.verify(w)
	t.Logf("3 leader kills behind the router: %d acknowledged writes, all on all nodes; %d client-visible errors (writes in flight when the leader died); longest pause between two acknowledgements %v; router found a new leader %d times",
		len(ackedKeys), errs, maxGap.Round(10*time.Millisecond), rt.Rediscovered.Load())
}
