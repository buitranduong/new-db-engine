package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Process management. The suite only ever signals the processes it started
// itself (it keeps their *exec.Cmd), never by name or pattern
// (docs/AGENT_PROTOCOL section 9).

// Node is one staskd process.
type Node struct {
	ID   int
	Dir  string
	Log  string
	HTTP string
	PG   string
	Raft string
	args []string
	env  *Env

	mu  sync.Mutex
	cmd *exec.Cmd
}

// Env owns the work directory, the binary and the ports.
type Env struct {
	Bin      string
	Work     string
	nextPort int
	mu       sync.Mutex
	all      []*Node
}

func newEnv(bin, work string, basePort int) *Env {
	return &Env{Bin: bin, Work: work, nextPort: basePort}
}

// port returns a free TCP port at or after the next counter value.
func (e *Env) port() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		p := e.nextPort
		e.nextPort++
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			return p
		}
	}
}

// NewNode prepares (does not start) a single node in a fresh directory.
func (e *Env) NewNode(name string, extra ...string) *Node {
	dir := filepath.Join(e.Work, name)
	os.RemoveAll(dir)
	os.MkdirAll(dir, 0o755)
	n := &Node{ID: 1, Dir: filepath.Join(dir, "data"), Log: filepath.Join(dir, "staskd.log"), env: e,
		HTTP: fmt.Sprintf("127.0.0.1:%d", e.port()), PG: fmt.Sprintf("127.0.0.1:%d", e.port())}
	n.args = append([]string{"-dir", n.Dir, "-addr", n.HTTP, "-pg-addr", n.PG, "-segment-mb", "8", "-prezero=false"}, extra...)
	e.all = append(e.all, n)
	return n
}

// Start launches the process and waits until it accepts a SQL connection.
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
	cmd := exec.Command(n.env.Bin, n.args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	n.cmd = cmd
	go func() { cmd.Wait(); lf.Close() }()
	return nil
}

// WaitSQL waits for the pg port to accept connections.
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
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("node %d: no SQL connection within %v (log %s)", n.ID, d, n.Log)
}

// Up is Start plus WaitSQL.
func (n *Node) Up() error {
	if err := n.Start(); err != nil {
		return err
	}
	return n.WaitSQL(60 * time.Second)
}

// Pid is the process id (0 when not running).
func (n *Node) Pid() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil {
		return 0
	}
	return n.cmd.Process.Pid
}

func (n *Node) signal(sig syscall.Signal) {
	n.mu.Lock()
	cmd := n.cmd
	n.cmd = nil
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(sig)
	done := make(chan struct{})
	go func() { cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
	}
}

// Kill9 is kill -9.
func (n *Node) Kill9() { n.signal(syscall.SIGKILL) }

// Term is a graceful stop.
func (n *Node) Term() { n.signal(syscall.SIGTERM) }

// URL is a connection URL of this node.
func (n *Node) URL() string {
	return fmt.Sprintf("postgres://app@%s/stask?sslmode=disable", n.PG)
}

// ReplURL is the URL of a replication (walsender) connection.
func (n *Node) ReplURL() string { return n.URL() + "&replication=database" }

// StopAll stops everything the suite started.
func (e *Env) StopAll() {
	for _, n := range e.all {
		n.Term()
	}
}

// Cluster is a 3-node Raft cluster of real staskd processes.
type Cluster struct {
	Nodes []*Node
}

// NewCluster prepares n nodes (not started).
func (e *Env) NewCluster(name string, n int, extra ...string) *Cluster {
	base := filepath.Join(e.Work, name)
	os.RemoveAll(base)
	c := &Cluster{}
	for i := 0; i < n; i++ {
		nd := &Node{ID: i + 1, env: e, Dir: filepath.Join(base, fmt.Sprintf("n%d", i+1)), Log: filepath.Join(base, fmt.Sprintf("n%d.log", i+1)),
			HTTP: fmt.Sprintf("127.0.0.1:%d", e.port()), PG: fmt.Sprintf("127.0.0.1:%d", e.port()), Raft: fmt.Sprintf("127.0.0.1:%d", e.port())}
		os.MkdirAll(base, 0o755)
		c.Nodes = append(c.Nodes, nd)
		e.all = append(e.all, nd)
	}
	var peers []string
	for _, nd := range c.Nodes {
		peers = append(peers, fmt.Sprintf("%d=%s@%s", nd.ID, nd.Raft, nd.HTTP))
	}
	for _, nd := range c.Nodes {
		nd.args = append([]string{"-dir", nd.Dir, "-addr", nd.HTTP, "-pg-addr", nd.PG, "-segment-mb", "8", "-prezero=false", "-gc=false",
			"-raft-id", fmt.Sprint(nd.ID), "-raft-listen", nd.Raft, "-raft-peers", strings.Join(peers, ",")}, extra...)
	}
	return c
}

// Up starts every node and waits for a leader.
func (c *Cluster) Up() error {
	for _, n := range c.Nodes {
		if err := n.Start(); err != nil {
			return err
		}
	}
	_, err := c.WaitLeader(60 * time.Second)
	return err
}

// leaderOf asks one node whether it takes writes (pg_is_in_recovery false).
func isLeader(n *Node) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, n.URL())
	if err != nil {
		return false
	}
	defer c.Close(context.Background())
	var rec bool
	if err := c.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&rec); err != nil {
		return false
	}
	return !rec
}

// WaitLeader returns the node that takes writes.
func (c *Cluster) WaitLeader(d time.Duration) (*Node, error) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		for _, n := range c.Nodes {
			if n.Pid() != 0 && isLeader(n) {
				return n, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, errors.New("no leader")
}

// DSN lists every node: libpq/pgx pick the one that takes writes.
func (c *Cluster) DSN() string {
	var hosts []string
	for _, n := range c.Nodes {
		hosts = append(hosts, n.PG)
	}
	return fmt.Sprintf("postgres://app@%s/stask?sslmode=disable&target_session_attrs=read-write&connect_timeout=3", strings.Join(hosts, ","))
}
