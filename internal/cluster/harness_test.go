package cluster

// Test harness for real processes: staskd binaries in a 3-node cluster, TCP
// fault-injecting proxies on every node-to-node link (iptables is not
// available), kill -9, and HTTP clients that record which writes were
// acknowledged.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func staskdBin(t *testing.T) string {
	if testing.Short() || os.Getenv("STASK_NO_PROC") != "" {
		t.Skip("process tests skipped")
	}
	if p := os.Getenv("STASK_BIN"); p != "" { // test another build, e.g. main's, as the single-node baseline
		return p
	}
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "staskd-bin")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "staskd")
		out, err := exec.Command("go", "build", "-o", binPath, "../../cmd/staskd").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

// ---- fault-injecting TCP proxy ----

type proxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	block  bool
	delay  time.Duration
	conns  map[net.Conn]bool
}

func newProxy(listen, target string) (*proxy, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	p := &proxy{ln: ln, target: target, conns: map[net.Conn]bool{}}
	go p.accept()
	return p, nil
}

func (p *proxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		blocked := p.block
		p.mu.Unlock()
		if blocked {
			c.Close()
			continue
		}
		go p.pipe(c)
	}
}

func (p *proxy) pipe(c net.Conn) {
	t, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	if p.block {
		p.mu.Unlock()
		c.Close()
		t.Close()
		return
	}
	p.conns[c], p.conns[t] = true, true
	p.mu.Unlock()
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				p.mu.Lock()
				d := p.delay
				p.mu.Unlock()
				if d > 0 {
					time.Sleep(d)
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					err = werr
				}
			}
			if err != nil {
				dst.Close()
				src.Close()
				return
			}
		}
	}
	go cp(t, c)
	cp(c, t)
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, t)
	p.mu.Unlock()
}

func (p *proxy) setBlocked(b bool) {
	p.mu.Lock()
	p.block = b
	var cs []net.Conn
	if b {
		for c := range p.conns {
			cs = append(cs, c)
		}
	}
	p.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

func (p *proxy) setDelay(d time.Duration) { p.mu.Lock(); p.delay = d; p.mu.Unlock() }
func (p *proxy) close()                   { p.ln.Close(); p.setBlocked(true) }

// ---- a cluster of staskd processes ----

type tnode struct {
	id       uint64
	dir      string
	log      string
	http     string
	raft     string
	args     []string
	cmd      *exec.Cmd
	done     chan struct{}
	restarts int
}

type tcluster struct {
	t       *testing.T
	nodes   []*tnode
	proxies map[[2]int]*proxy // [from][to]
	bin     string
	rng     *rand.Rand
	client  *http.Client
	lead    atomic.Int32 // index of the last known leader
}

// portLockDir is shared by every `go test` process on the machine (and by
// every user), so it is a fixed path and not os.TempDir (TMPDIR may differ).
const portLockDir = "/tmp/stask-test-ports"

// lockPort takes an exclusive, non-blocking flock on a per-port lock file. The
// lock lives as long as the returned file is open (the kernel drops it if the
// test process dies), so two concurrent test processes never get the same port.
func lockPort(port int) *os.File {
	_ = os.MkdirAll(portLockDir, 0o777)
	_ = os.Chmod(portLockDir, 0o777)
	f, err := os.OpenFile(filepath.Join(portLockDir, fmt.Sprintf("%d.lock", port)), os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil
	}
	_ = f.Chmod(0o666)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil
	}
	return f
}

// freeBase reserves need consecutive ports in 21000-21999: each port is flocked
// (cross-process, held until the test ends) and also probed with a real listen
// (guards against processes that are not stask tests). Ports are therefore
// collision free between concurrent go test runs.
func freeBase(t *testing.T, need int) int {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(os.Getpid())))
	var lastErr error
	for try := 0; try < 500; try++ {
		base := 21000 + rng.Intn(980-need)
		var ls []net.Listener
		var locks []*os.File
		ok := true
		for i := 0; i < need; i++ {
			lk := lockPort(base + i)
			if lk == nil {
				lastErr = fmt.Errorf("port %d locked by another test process", base+i)
				ok = false
				break
			}
			locks = append(locks, lk)
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				lastErr = err
				ok = false
				break
			}
			ls = append(ls, l)
		}
		for _, l := range ls {
			l.Close()
		}
		if ok {
			t.Cleanup(func() {
				for _, f := range locks {
					f.Close()
				}
			})
			return base
		}
		for _, f := range locks {
			f.Close()
		}
	}
	t.Fatalf("no free port block in 21000-21999 (last error: %v)", lastErr)
	return 0
}

// newCluster starts n nodes. extra are additional staskd flags; segment
// size is small and pre-zeroing off so that the disk is not filled.
func newCluster(t *testing.T, n int, extra ...string) *tcluster {
	c := &tcluster{t: t, bin: staskdBin(t), proxies: map[[2]int]*proxy{}, rng: rand.New(rand.NewSource(time.Now().UnixNano())),
		client: &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}}
	base := freeBase(t, 3*n+n*n)
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		c.nodes = append(c.nodes, &tnode{id: uint64(i + 1), dir: filepath.Join(dir, fmt.Sprintf("n%d", i+1)),
			log:  filepath.Join(dir, fmt.Sprintf("n%d.log", i+1)),
			http: fmt.Sprintf("127.0.0.1:%d", base+i), raft: fmt.Sprintf("127.0.0.1:%d", base+n+i)})
	}
	pp := base + 2*n
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			p, err := newProxy(fmt.Sprintf("127.0.0.1:%d", pp), c.nodes[j].raft)
			pp++
			if err != nil {
				t.Fatal(err)
			}
			c.proxies[[2]int{i, j}] = p
		}
	}
	for i, nd := range c.nodes {
		var peers []string
		for j, o := range c.nodes {
			addr := o.raft
			if j != i {
				addr = c.proxies[[2]int{i, j}].ln.Addr().String()
			}
			peers = append(peers, fmt.Sprintf("%d=%s@%s", o.id, addr, o.http))
		}
		nd.args = append([]string{"-dir", nd.dir, "-addr", nd.http, "-pg-addr", "", "-segment-mb", "8", "-prezero=false", "-gc=false",
			"-raft-id", fmt.Sprint(nd.id), "-raft-listen", nd.raft, "-raft-peers", strings.Join(peers, ",")}, extra...)
	}
	t.Cleanup(func() {
		for _, nd := range c.nodes {
			c.kill(nd)
		}
		for _, p := range c.proxies {
			p.close()
		}
		if t.Failed() {
			for _, nd := range c.nodes {
				b, _ := os.ReadFile(nd.log)
				if len(b) > 6000 {
					b = b[len(b)-6000:]
				}
				t.Logf("---- node %d log (tail) ----\n%s", nd.id, b)
			}
		}
	})
	for _, nd := range c.nodes {
		c.start(nd)
	}
	return c
}

func (c *tcluster) start(nd *tnode) {
	lf, err := os.OpenFile(nd.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		c.t.Fatal(err)
	}
	cmd := exec.Command(c.bin, nd.args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	nd.cmd, nd.done = cmd, make(chan struct{})
	go func(cmd *exec.Cmd, done chan struct{}) {
		cmd.Wait()
		lf.Close()
		close(done)
	}(cmd, nd.done)
}

func (c *tcluster) running(nd *tnode) bool {
	if nd.cmd == nil {
		return false
	}
	select {
	case <-nd.done:
		return false
	default:
		return true
	}
}

// kill9 kills the process with SIGKILL and waits for it to be gone.
func (c *tcluster) kill(nd *tnode) {
	if nd.cmd == nil {
		return
	}
	nd.cmd.Process.Signal(syscall.SIGKILL)
	<-nd.done
	nd.cmd = nil
}

// isolate cuts every link of node i (both directions) or restores them.
func (c *tcluster) isolate(i int, cut bool) {
	for k, p := range c.proxies {
		if k[0] == i || k[1] == i {
			p.setBlocked(cut)
		}
	}
}

func (c *tcluster) heal() {
	for _, p := range c.proxies {
		p.setBlocked(false)
		p.setDelay(0)
	}
}

// ---- client side ----

type nodeStatus struct {
	Status
	ok bool
}

func (c *tcluster) status(nd *tnode) (Status, bool) {
	resp, err := c.client.Get("http://" + nd.http + "/v1/cluster")
	if err != nil {
		return Status{}, false
	}
	defer resp.Body.Close()
	var s Status
	if json.NewDecoder(resp.Body).Decode(&s) != nil {
		return Status{}, false
	}
	return s, true
}

// leader returns the index of a node that is the leader and accepts
// writes (the highest term if two claim it), or -1.
func (c *tcluster) leader() (int, Status) {
	best, bs := -1, Status{}
	for i, nd := range c.nodes {
		if !c.running(nd) {
			continue
		}
		if s, ok := c.status(nd); ok && s.Role == "leader" && s.Writable && (best < 0 || s.Term > bs.Term) {
			best, bs = i, s
		}
	}
	return best, bs
}

func (c *tcluster) waitLeader(d time.Duration) (int, Status) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if i, s := c.leader(); i >= 0 {
			return i, s
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("no leader within %v", d)
	return -1, Status{}
}

type outcome int

const (
	acked   outcome = iota
	notRun          // refused before anything ran: safe to retry
	unknown         // may or may not have been applied
	rejected
)

// write puts one row on node i.
func (c *tcluster) writeTo(i int, key string, v int) outcome {
	body := fmt.Sprintf(`{"ops":[{"op":"put","table":"t","row":{"id":%q,"v":%d}}]}`, key, v)
	resp, err := c.client.Post("http://"+c.nodes[i].http+"/v1/write", "application/json", strings.NewReader(body))
	if err != nil {
		var ne *net.OpError
		if errors.As(err, &ne) && ne.Op == "dial" {
			return notRun
		}
		return unknown
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == 200:
		return acked
	case resp.Header.Get("X-Stask-Not-Leader") != "":
		return notRun
	case resp.StatusCode == 503:
		return unknown
	}
	return rejected
}

// write tries the nodes (starting with the last known leader) until the
// write is acknowledged, or its outcome is unknown, or the time is up.
func (c *tcluster) write(key string, v int, within time.Duration) outcome {
	end := time.Now().Add(within)
	for {
		start := int(c.lead.Load())
		for k := 0; k < len(c.nodes); k++ {
			i := (start + k) % len(c.nodes)
			switch o := c.writeTo(i, key, v); o {
			case acked:
				c.lead.Store(int32(i))
				return acked
			case unknown, rejected:
				return o
			}
		}
		if time.Now().After(end) {
			return notRun
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (c *tcluster) createTable() {
	body := `{"name":"t","columns":[{"name":"id","type":"string"},{"name":"v","type":"int"}],"key":"id"}`
	end := time.Now().Add(30 * time.Second)
	for time.Now().Before(end) {
		for i, nd := range c.nodes {
			resp, err := c.client.Post("http://"+nd.http+"/v1/tables", "application/json", strings.NewReader(body))
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				c.lead.Store(int32(i))
				return
			}
			if resp.StatusCode != 503 {
				c.t.Fatalf("create table: %d %s", resp.StatusCode, b)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatal("could not create the table")
}

// get returns the value of key on node i (found=false if absent).
func (c *tcluster) get(i int, key string) (v int, found bool, err error) {
	resp, err := c.client.Get("http://" + c.nodes[i].http + "/v1/get?table=t&key=" + key)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	var r struct {
		Found bool
		Row   struct{ V int }
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, false, err
	}
	return r.Row.V, r.Found, nil
}

func (c *tcluster) durableLSN(i int) uint64 {
	resp, err := c.client.Get("http://" + c.nodes[i].http + "/v1/status")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var r struct {
		DurableLSN uint64 `json:"durable_lsn"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	return r.DurableLSN
}

// settle brings every node up, heals the network and waits until all nodes
// have applied everything the leader committed.
func (c *tcluster) settle() {
	c.heal()
	for _, nd := range c.nodes {
		if !c.running(nd) {
			c.start(nd)
		}
	}
	c.waitLeader(40 * time.Second)
	end := time.Now().Add(60 * time.Second)
	for time.Now().Before(end) {
		li, ls := c.leader()
		if li < 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		same := true
		for i := range c.nodes {
			s, ok := c.status(c.nodes[i])
			if !ok || s.Applied != ls.Commit || s.Commit != ls.Commit {
				same = false
			}
		}
		if same {
			// a write through the leader proves it still commits
			if c.write(fmt.Sprintf("settle-%d", time.Now().UnixNano()), 0, 10*time.Second) == acked {
				time.Sleep(300 * time.Millisecond)
				continue2 := true
				_, ls2 := c.leader()
				for i := range c.nodes {
					s, ok := c.status(c.nodes[i])
					if !ok || s.Applied != ls2.Commit {
						continue2 = false
					}
				}
				if continue2 {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatal("the cluster did not converge")
}

// writers runs g clients that write distinct keys until stop is closed.
type writers struct {
	mu      sync.Mutex
	acked   map[string]int
	unknown map[string]int
	stop    chan struct{}
	wg      sync.WaitGroup
	lat     []time.Duration
}

func (c *tcluster) startWriters(g int, prefix string) *writers {
	w := &writers{acked: map[string]int{}, unknown: map[string]int{}, stop: make(chan struct{})}
	for k := 0; k < g; k++ {
		w.wg.Add(1)
		go func(k int) {
			defer w.wg.Done()
			for n := 0; ; n++ {
				select {
				case <-w.stop:
					return
				default:
				}
				key := fmt.Sprintf("%s-%d-%d", prefix, k, n)
				t0 := time.Now()
				o := c.write(key, n, 8*time.Second)
				w.mu.Lock()
				switch o {
				case acked:
					w.acked[key] = n
					w.lat = append(w.lat, time.Since(t0))
				case unknown:
					w.unknown[key] = n
				}
				w.mu.Unlock()
				if o == notRun {
					time.Sleep(100 * time.Millisecond)
				}
			}
		}(k)
	}
	return w
}

func (w *writers) finish() {
	close(w.stop)
	w.wg.Wait()
}

func (w *writers) count() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.acked) }

// verify checks that every acknowledged write is on every node, with its
// value, and that the nodes hold identical logs (same durable LSN).
func (c *tcluster) verify(w *writers) (present, unknownPresent int) {
	for i := range c.nodes {
		for k, v := range w.acked {
			got, found, err := c.get(i, k)
			if err != nil || !found || got != v {
				c.t.Errorf("node %d: acknowledged write %s=%d lost (found=%v value=%d err=%v)", c.nodes[i].id, k, v, found, got, err)
				if c.t.Failed() && len(c.t.Name()) > 0 {
					return
				}
			}
		}
	}
	for k := range w.unknown {
		if _, found, _ := c.get(0, k); found {
			unknownPresent++
		}
	}
	l0 := c.durableLSN(0)
	for i := range c.nodes {
		if l := c.durableLSN(i); l != l0 {
			c.t.Errorf("node %d durable lsn %d, node 1 %d", c.nodes[i].id, l, l0)
		}
	}
	return len(w.acked), unknownPresent
}

var _ = bytes.NewReader

// newCluster0 is a harness around already described processes (single node).
func newCluster0(t *testing.T, bin string, nodes ...*tnode) *tcluster {
	return &tcluster{t: t, bin: bin, nodes: nodes, proxies: map[[2]int]*proxy{}, rng: rand.New(rand.NewSource(1)),
		client: &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}}
}
