package main

import (
	"context"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"stask/internal/reconcile"
)

// Process management. A drill only ever signals the processes it started
// itself (it keeps their *exec.Cmd), never by name or pattern
// (docs/AGENT_PROTOCOL section 9). Ports come from 21000-21999, taken with
// the cross-process flock of /tmp/stask-test-ports that `go test` uses.

const portLockDir = "/tmp/stask-test-ports"

type Env struct {
	Bin, Ctl, Work string
	Rng            *rand.Rand
	mu             sync.Mutex
	locks          []*os.File
	nodes          []*Node
}

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

// Ports reserves n ports (locked until the program exits).
func (e *Env) Ports(n int) []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []int
	for try := 0; try < 5000 && len(out) < n; try++ {
		p := 21000 + e.Rng.Intn(990)
		lk := lockPort(p)
		if lk == nil {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			lk.Close()
			continue
		}
		l.Close()
		e.locks = append(e.locks, lk)
		out = append(out, p)
	}
	if len(out) < n {
		panic("no free ports in 21000-21999")
	}
	return out
}

// Node is one staskd process.
type Node struct {
	ID             int
	Dir, Log       string
	HTTP, PG, Raft string
	Repl           string
	args           []string
	env            *Env
	mu             sync.Mutex
	cmd            *exec.Cmd
	done           chan struct{}
	stopped        bool // SIGSTOP sent
	Starts         int
	Runner         func(n *Node, args []string) *exec.Cmd // e.g. docker
}

func (e *Env) Stop() {
	for _, n := range e.nodes {
		n.Term()
	}
	for _, l := range e.locks {
		l.Close()
	}
}

// NewNode prepares a single node (not started) with its own directory.
func (e *Env) NewNode(name string, extra ...string) *Node {
	ps := e.Ports(3)
	base := filepath.Join(e.Work, name)
	os.RemoveAll(base)
	os.MkdirAll(base, 0o755)
	n := &Node{ID: 1, env: e, Dir: filepath.Join(base, "data"), Log: filepath.Join(base, "staskd.log"),
		HTTP: fmt.Sprintf("127.0.0.1:%d", ps[0]), PG: fmt.Sprintf("127.0.0.1:%d", ps[1]), Repl: fmt.Sprintf("127.0.0.1:%d", ps[2])}
	n.args = append([]string{"-dir", n.Dir, "-addr", n.HTTP, "-pg-addr", n.PG, "-segment-mb", "8", "-prezero=false"}, extra...)
	e.nodes = append(e.nodes, n)
	return n
}

// NewCluster prepares n Raft nodes.
func (e *Env) NewCluster(name string, n int, extra ...string) []*Node {
	base := filepath.Join(e.Work, name)
	os.RemoveAll(base)
	os.MkdirAll(base, 0o755)
	var out []*Node
	var peers []string
	for i := 0; i < n; i++ {
		ps := e.Ports(3)
		nd := &Node{ID: i + 1, env: e, Dir: filepath.Join(base, fmt.Sprintf("n%d", i+1)), Log: filepath.Join(base, fmt.Sprintf("n%d.log", i+1)),
			HTTP: fmt.Sprintf("127.0.0.1:%d", ps[0]), PG: fmt.Sprintf("127.0.0.1:%d", ps[1]), Raft: fmt.Sprintf("127.0.0.1:%d", ps[2])}
		out = append(out, nd)
		e.nodes = append(e.nodes, nd)
		peers = append(peers, fmt.Sprintf("%d=%s@%s", nd.ID, nd.Raft, nd.HTTP))
	}
	for _, nd := range out {
		nd.args = append([]string{"-dir", nd.Dir, "-addr", nd.HTTP, "-pg-addr", nd.PG, "-segment-mb", "8", "-prezero=false", "-gc=false",
			"-raft-id", fmt.Sprint(nd.ID), "-raft-listen", nd.Raft, "-raft-peers", strings.Join(peers, ",")}, extra...)
	}
	return out
}

func (n *Node) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd != nil {
		return errors.New("already running")
	}
	lf, err := os.OpenFile(n.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(lf, "---- start %d at %s ----\n", n.Starts+1, time.Now().Format(time.RFC3339Nano))
	cmd := exec.Command(n.env.Bin, n.args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return err
	}
	n.cmd, n.done, n.stopped = cmd, make(chan struct{}), false
	n.Starts++
	done := n.done
	go func() { cmd.Wait(); lf.Close(); close(done) }()
	return nil
}

func (n *Node) Running() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cmd != nil
}

func (n *Node) signal(sig syscall.Signal, wait bool) {
	n.mu.Lock()
	cmd, done := n.cmd, n.done
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM {
		n.cmd = nil
	}
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(sig)
	if sig == syscall.SIGTERM && n.stopped {
		cmd.Process.Signal(syscall.SIGCONT)
	}
	if !wait {
		return
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		<-done
	}
}

func (n *Node) Kill9()  { n.signal(syscall.SIGKILL, true) }
func (n *Node) Term()   { n.signal(syscall.SIGTERM, true) }
func (n *Node) Pause()  { n.signal(syscall.SIGSTOP, false); n.stopped = true }
func (n *Node) Resume() { n.signal(syscall.SIGCONT, false); n.stopped = false }

func (n *Node) Pid() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil {
		return 0
	}
	return n.cmd.Process.Pid
}

func (n *Node) URL() string { return fmt.Sprintf("postgres://app@%s/stask?sslmode=disable", n.PG) }

func ClusterDSN(ns []*Node) string {
	var hosts []string
	for _, n := range ns {
		hosts = append(hosts, n.PG)
	}
	return fmt.Sprintf("postgres://app@%s/stask?sslmode=disable&target_session_attrs=read-write&connect_timeout=3", strings.Join(hosts, ","))
}

// WaitSQL waits until the node accepts a SQL connection.
func (n *Node) WaitSQL(d time.Duration) error {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := pgx.Connect(ctx, n.URL())
		cancel()
		if err == nil {
			c.Close(context.Background())
			return nil
		}
		if n.done != nil {
			select {
			case <-n.done:
				return fmt.Errorf("node %d exited during start (log %s)", n.ID, n.Log)
			default:
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("node %d: no SQL connection within %v (log %s)", n.ID, d, n.Log)
}

func (n *Node) Up() error {
	if err := n.Start(); err != nil {
		return err
	}
	return n.WaitSQL(120 * time.Second)
}

// IsLeader: the node takes writes.
func (n *Node) IsLeader() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+n.HTTP+"/v1/ready?write=1", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode == 200
}

func WaitLeader(ns []*Node, d time.Duration, not ...*Node) (*Node, error) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		for _, n := range ns {
			skip := false
			for _, x := range not {
				if x == n {
					skip = true
				}
			}
			if !skip && n.Running() && n.IsLeader() {
				return n, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("no leader")
}

func (n *Node) Get(path string) (string, int, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	r, err := hc.Get("http://" + n.HTTP + path)
	if err != nil {
		return "", 0, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b), r.StatusCode, nil
}

func (n *Node) Post(path string, timeout time.Duration) (string, int, error) {
	hc := &http.Client{Timeout: timeout}
	r, err := hc.Post("http://"+n.HTTP+path, "application/json", nil)
	if err != nil {
		return "", 0, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b), r.StatusCode, nil
}

// WaitReady waits for /v1/ready (the derived views have caught up).
func (n *Node) WaitReady(d time.Duration) error {
	end := time.Now().Add(d)
	var last string
	for time.Now().Before(end) {
		b, code, err := n.Get("/v1/ready")
		if err == nil && code == 200 {
			return nil
		}
		last = fmt.Sprintf("%v %d %s", err, code, strings.TrimSpace(b))
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("node %d not ready after %v: %s", n.ID, d, last)
}

// Metrics reads /metrics: "name" or "name{labels}" -> value.
func (n *Node) Metrics() (map[string]float64, error) {
	b, code, err := n.Get("/metrics")
	if err != nil || code != 200 {
		return nil, fmt.Errorf("metrics: %v %d", err, code)
	}
	m := map[string]float64{}
	for _, ln := range strings.Split(b, "\n") {
		if ln == "" || ln[0] == 0x23 {
			continue
		}
		i := strings.LastIndexByte(ln, 0x20)
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(ln[i+1:], 64); err == nil {
			m[ln[:i]] = v
		}
	}
	return m, nil
}

func (n *Node) StatusJSON() (map[string]any, error) {
	b, code, err := n.Get("/v1/status")
	if err != nil || code != 200 {
		return nil, fmt.Errorf("status: %v %d", err, code)
	}
	var m map[string]any
	return m, json.Unmarshal([]byte(b), &m)
}

func (n *Node) ClusterStatus() (map[string]any, error) {
	b, code, err := n.Get("/v1/cluster")
	if err != nil || code != 200 {
		return nil, fmt.Errorf("cluster: %v %d", err, code)
	}
	var m map[string]any
	return m, json.Unmarshal([]byte(b), &m)
}

func (n *Node) DurableLSN() uint64 {
	m, err := n.StatusJSON()
	if err != nil {
		return 0
	}
	f, _ := m["durable_lsn"].(float64)
	return uint64(f)
}

// Reconcile runs `staskctl reconcile` against the node and parses the answer.
func (n *Node) Reconcile(sample int, extra ...string) (*reconcile.Report, string, error) {
	args := []string{"reconcile", "-url", "http://" + n.HTTP, "-sample", strconv.Itoa(sample), "-json"}
	args = append(args, extra...)
	cmd := exec.Command(n.env.Ctl, args...)
	out, err := cmd.Output()
	var rep reconcile.Report
	if len(out) > 0 {
		if jerr := json.Unmarshal(out, &rep); jerr != nil {
			return nil, string(out), jerr
		}
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("staskctl reconcile: %v", err)
	}
	return &rep, string(out), nil // exit codes 3/4 are verdicts, not failures
}

// ReconcileSummary is a one-line description of a report.
func ReconcileSummary(r *reconcile.Report) string {
	var parts []string
	for _, t := range r.Tables {
		for _, s := range t.Structures {
			p := fmt.Sprintf("%s/%s=%s", t.Table, s.Name, s.Status)
			if s.BehindRecords > 0 {
				p += fmt.Sprintf("(behind %d)", s.BehindRecords)
			}
			if s.Diff != nil && s.Diff.Count > 0 {
				p += fmt.Sprintf("(%d keys %q..%q)", s.Diff.Count, s.Diff.FirstKey, s.Diff.LastKey)
			}
			parts = append(parts, p)
		}
	}
	return fmt.Sprintf("verdict=%s %s", r.Verdict, strings.Join(parts, " "))
}

// WaitInSync polls a cheap reconcile until the views caught up, then runs the
// full content comparison. It returns the final report.
func (n *Node) WaitInSync(d time.Duration, extra ...string) (*reconcile.Report, error) {
	end := time.Now().Add(d)
	for {
		rep, _, err := n.Reconcile(0, extra...)
		if err == nil && rep.Verdict == reconcile.StInSync {
			break
		}
		if time.Now().After(end) {
			if err != nil {
				return nil, err
			}
			return rep, fmt.Errorf("views not caught up after %v: %s", d, ReconcileSummary(rep))
		}
		time.Sleep(300 * time.Millisecond)
	}
	rep, _, err := n.Reconcile(1, extra...)
	if err != nil {
		return nil, err
	}
	return rep, nil
}

func connect(url string) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pgx.Connect(ctx, url)
}

func exec1(url string, sqls ...string) error {
	c, err := connect(url)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	for _, s := range sqls {
		if _, err := c.Exec(context.Background(), s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

func scalarInt(url, q string) (int64, error) {
	c, err := connect(url)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	var v int64
	err = c.QueryRow(context.Background(), q).Scan(&v)
	return v, err
}

type pgxConn = pgx.Conn

func pgxConnect(ctx context.Context, url string) (*pgx.Conn, error) { return pgx.Connect(ctx, url) }

// SetDir moves the data directory of a node that is not running.
func (n *Node) SetDir(d string) {
	for i := range n.args {
		if n.args[i] == "-dir" && i+1 < len(n.args) {
			n.args[i+1] = d
		}
	}
	n.Dir = d
}
