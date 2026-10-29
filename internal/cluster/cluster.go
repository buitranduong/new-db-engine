// Package cluster makes a set of staskd processes one database by majority
// consensus (docs/06 section 5): internal/raft orders the log, this package
// moves the engine's log bytes through it.
//
// Every node runs the engine as a read-only follower that applies committed
// Raft entries with Engine.Apply (the same path as an asynchronous
// replica, so its log is byte-identical to the leader's). The node that
// wins the election applies every committed entry, then Promote()s its
// engine; from then on the engine's commit path writes and fdatasyncs a
// batch locally, calls Replicate (core.Quorum), which proposes the batch's
// bytes as a Raft entry and returns when a majority has fsynced it, and only
// then publishes and answers the clients.
//
// A deposed leader's log may hold batches the cluster did not commit.
// Rather than rewind a live engine, the node restarts itself (exec of its
// own binary) and the engine, opened with a cut at the last applied LSN,
// discards that tail (core.Options.Cut), then follows the new leader.
// Likewise a node that fell behind the compacted Raft log fetches a
// snapshot (the engine's base backup, tied to a Raft index) and restarts on
// it.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"stask/internal/core"
	"stask/internal/raft"
	"stask/internal/repl"
	"stask/internal/wal"
)

type Config struct {
	ID    uint64
	Peers map[uint64]string // raft address of every node, this one included
	// HTTP addresses of the nodes (informational: clients and the router use them)
	HTTP map[uint64]string
	// Listen is where this node accepts Raft connections (default Peers[ID]).
	Listen string
	// Dir is the engine's data directory; Raft state goes to Dir/raft.
	Dir      string
	SegMax   int64
	Settings repl.Settings
	Insecure bool // allow a non-loopback Listen without authentication

	TickInterval   time.Duration // default 50 ms
	ElectionTicks  int           // default 20 (election timeout 1 to 2 s)
	CompactEntries int           // compact when the Raft log holds more entries (default 20000)
	CompactKeep    int           // entries kept behind the applied index (default 2000)

	Logf func(string, ...any)
	// Restart is called when the node must restart itself (deposed leader,
	// installed snapshot). The default executes the same binary in place.
	Restart func(reason string)
}

type Cluster struct {
	cfg Config
	e   *core.Engine
	g   *raft.Group
	st  *raft.FileStorage
	app *appliedFile
	inc uint64 // identifies this process in the entries it proposes

	serverSec *repl.Security
	ln        net.Listener
	peers     map[uint64]*peerConn
	cutLSN    uint64
	startApp  applied

	stop chan struct{}
	wg   sync.WaitGroup

	// the applier
	qmu    sync.Mutex
	qcond  *sync.Cond
	queue  []raft.Entry
	cur    atomic.Pointer[applied] // last applied entry
	dirty  atomic.Bool
	boot   bool // entries the engine already has are skipped (after a restart)
	asyncE atomic.Pointer[error]

	// the leader side
	repMu      sync.Mutex
	sent       wal.Pos // where the next Replicate's bytes start
	leaderTerm atomic.Uint64
	writable   atomic.Bool

	baseMu   sync.Mutex
	base     applied // lsn and end position at the Raft log's base (snap.json)
	snapBusy atomic.Bool
	closed   atomic.Bool

	Stats struct {
		Proposed, Applied, Snapshots, Compactions atomic.Int64
		ProposeNs                                 atomic.Int64 // total time in Replicate
	}
}

func (c *Config) defaults() {
	if c.TickInterval == 0 {
		c.TickInterval = 50 * time.Millisecond
	}
	if c.ElectionTicks == 0 {
		c.ElectionTicks = 20
	}
	if c.CompactEntries == 0 {
		c.CompactEntries = 20000
	}
	if c.CompactKeep == 0 {
		c.CompactKeep = 2000
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Listen == "" {
		c.Listen = c.Peers[c.ID]
	}
}

func (c *Cluster) raftDir() string { return filepath.Join(c.cfg.Dir, "raft") }

// Prepare readies the data directory before the engine is opened: it
// finishes an installed snapshot, opens the Raft state and decides where
// the engine's log must be cut. Use CutLSN in core.Options (Cut: true) and
// Quorum() as core.Options.Quorum, then call Start with the opened engine.
func Prepare(cfg Config) (*Cluster, error) {
	cfg.defaults()
	if cfg.ID == 0 || cfg.Peers[cfg.ID] == "" {
		return nil, errors.New("cluster: this node's id must be in the peer list")
	}
	c := &Cluster{cfg: cfg, stop: make(chan struct{}), boot: true, peers: map[uint64]*peerConn{}}
	c.qcond = sync.NewCond(&c.qmu)
	var b [8]byte
	rand.Read(b[:])
	c.inc = binary.LittleEndian.Uint64(b[:]) | 1
	if err := os.MkdirAll(c.raftDir(), 0o755); err != nil {
		return nil, err
	}
	if err := c.finishSnapshot(); err != nil {
		return nil, fmt.Errorf("cluster: finishing the installed snapshot: %w", err)
	}
	var err error
	if c.st, err = raft.OpenFileStorage(filepath.Join(c.raftDir(), "raft.log")); err != nil {
		return nil, err
	}
	var a applied
	if c.app, a, err = openApplied(filepath.Join(c.raftDir(), "applied.bin")); err != nil {
		return nil, err
	}
	_, snap, _, _ := c.st.Load()
	if a.Index < snap.Index {
		return nil, fmt.Errorf("cluster: the applied record (entry %d) is behind the compacted Raft log (base %d): "+
			"this node's state is damaged; empty its data directory so that it fetches a snapshot", a.Index, snap.Index)
	}
	c.startApp, c.cutLSN = a, a.LSN
	c.cur.Store(&a)
	c.readBase()
	return c, nil
}

// CutLSN is the LSN after which the engine's log must be discarded on open.
func (c *Cluster) CutLSN() uint64 { return c.cutLSN }

// Quorum is the core.Quorum of the engine.
func (c *Cluster) Quorum() core.Quorum { return c }

// Start begins the Raft node, the transport and the apply loop on the
// opened engine (which must have been opened with Options.Cluster).
func (c *Cluster) Start(e *core.Engine) error {
	c.e = e
	if _, pos, _ := e.DurablePoint(); pos.Less(c.startApp.Pos) {
		return fmt.Errorf("cluster: the engine's log ends at %d:%d, before the applied entry %d (%d:%d)",
			pos.Seg, pos.Off, c.startApp.Index, c.startApp.Pos.Seg, c.startApp.Pos.Off)
	}
	var ids []uint64
	for id := range c.cfg.Peers {
		ids = append(ids, id)
	}
	sec, err := c.cfg.Settings.Leader()
	if err != nil {
		return err
	}
	if err := repl.CheckServer(c.cfg.Listen, sec, c.cfg.Insecure); err != nil {
		return err
	}
	c.serverSec = sec
	tr := &transport{c: c}
	g, err := raft.NewGroup(raft.Config{ID: c.cfg.ID, Peers: ids, ElectionTicks: c.cfg.ElectionTicks, HeartbeatTicks: 1,
		Seed: time.Now().UnixNano(), Storage: c.st, Applied: c.startApp.Index,
		NeedSnapshot: c.needSnapshot}, tr, c.onApply, c.cfg.TickInterval)
	if err != nil {
		return err
	}
	c.g = g
	if c.ln, err = net.Listen("tcp", c.cfg.Listen); err != nil {
		g.Stop()
		return err
	}
	for id, addr := range c.cfg.Peers {
		if id == c.cfg.ID {
			continue
		}
		psec, err := c.cfg.Settings.Replica(addr)
		if err != nil {
			return err
		}
		p := &peerConn{c: c, id: id, addr: addr, sec: psec, out: make(chan raft.Msg, 4096)}
		c.peers[id] = p
		c.wg.Add(1)
		go p.run()
	}
	c.wg.Add(4)
	go c.acceptLoop()
	go c.applier()
	go c.monitor()
	go c.flusher()
	c.cfg.Logf("cluster: node %d of %v; raft on %s; engine at lsn %d (applied entry %d, raft log %d..%d)",
		c.cfg.ID, ids, c.cfg.Listen, e.DurableLSN(), c.startApp.Index, g.Info().First, g.Info().Last)
	return nil
}

func (c *Cluster) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	close(c.stop)
	if c.ln != nil {
		c.ln.Close()
	}
	c.qcond.Broadcast()
	if c.g != nil {
		c.g.Stop()
	}
	c.wg.Wait()
	if a := c.cur.Load(); a != nil {
		c.app.write(*a, true)
	}
	c.app.Close()
	c.st.Close()
}

// ---- state exposed to the server ----

type Status struct {
	ID         uint64 `json:"id"`
	Role       string `json:"role"`
	Term       uint64 `json:"term"`
	Leader     uint64 `json:"leader"`
	Writable   bool   `json:"writable"` // the leader's engine accepts writes
	Lease      bool   `json:"lease"`
	Commit     uint64 `json:"commit"`
	Applied    uint64 `json:"applied"`
	First      uint64 `json:"log_first"`
	Last       uint64 `json:"log_last"`
	HTTP       string `json:"http,omitempty"`
	LeaderHTTP string `json:"leader_http,omitempty"`
}

func (c *Cluster) Status() Status {
	i := c.g.Info()
	s := Status{ID: c.cfg.ID, Role: i.Role.String(), Term: i.Term, Leader: i.Leader,
		Writable: c.writable.Load() && !c.e.ReadOnly(), Lease: i.Lease, Commit: i.Commit,
		Applied: c.cur.Load().Index, First: i.First, Last: i.Last, HTTP: c.cfg.HTTP[c.cfg.ID]}
	if i.Leader != 0 {
		s.LeaderHTTP = c.cfg.HTTP[i.Leader]
	}
	return s
}

// ---- the leader's commit path ----

// Replicate implements core.Quorum.
func (c *Cluster) Replicate(lastLSN uint64, end wal.Pos) error {
	t0 := time.Now()
	c.repMu.Lock()
	defer c.repMu.Unlock()
	chunks, err := c.readRange(c.sent, end)
	if err != nil {
		return err
	}
	b := &batch{inc: c.inc, lastLSN: lastLSN, chunks: chunks}
	if len(chunks) == 0 {
		c.sent = end
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.g.Propose(ctx, encodeBatch(b)); err != nil {
		// the engine stops writing; the loop in monitor restarts the node
		c.cfg.Logf("cluster: replicate lsn %d: %v", lastLSN, err)
		return err
	}
	c.sent = end
	c.Stats.Proposed.Add(1)
	c.Stats.ProposeNs.Add(int64(time.Since(t0)))
	return nil
}

// readRange returns the log bytes in [from, end), cut at segment ends.
func (c *Cluster) readRange(from, end wal.Pos) ([]chunk, error) {
	lg := c.e.Log()
	var out []chunk
	for seg := from.Seg; seg <= end.Seg; seg++ {
		start := int64(0)
		if seg == from.Seg {
			start = from.Off
		}
		stop := end.Off
		if seg != end.Seg {
			var err error
			if stop, _, err = lg.Scan(seg, start, -1, func(*wal.Record) error { return nil }); err != nil {
				return nil, err
			}
		}
		if stop <= start {
			continue
		}
		buf := make([]byte, stop-start)
		if n, err := lg.ReadAt(seg, start, buf); err != nil && !(errors.Is(err, io.EOF) && n == len(buf)) {
			return nil, fmt.Errorf("read log %d:%d: %w", seg, start, err)
		}
		out = append(out, chunk{wal.Pos{Seg: seg, Off: start}, buf})
	}
	return out, nil
}

// ---- applying committed entries ----

// onApply runs inside the Raft node (under its lock): only queue.
func (c *Cluster) onApply(e raft.Entry) {
	c.qmu.Lock()
	c.queue = append(c.queue, e)
	c.qmu.Unlock()
	c.qcond.Signal()
}

func (c *Cluster) applier() {
	defer c.wg.Done()
	for {
		c.qmu.Lock()
		for len(c.queue) == 0 && !c.closed.Load() {
			c.qcond.Wait()
		}
		if c.closed.Load() {
			c.qmu.Unlock()
			return
		}
		ents := c.queue
		c.queue = nil
		c.qmu.Unlock()
		for _, e := range ents {
			if err := c.applyOne(e); err != nil {
				c.cfg.Logf("cluster: FATAL applying entry %d: %v", e.Index, err)
				c.restart("apply error: " + err.Error())
				return
			}
		}
	}
}

func (c *Cluster) applyOne(e raft.Entry) error {
	prev := c.cur.Load()
	next := &applied{Index: e.Index, LSN: prev.LSN, Pos: prev.Pos}
	if !e.Noop && len(e.Data) > 0 {
		b, err := decodeBatch(e.Data)
		if err != nil {
			return err
		}
		end := b.end()
		switch {
		case b.inc == c.inc:
			// proposed by this process: the engine wrote it before proposing
		case c.boot && !c.e.WritePos().Less(end):
			// the engine already has it (restart with a stale applied record)
		default:
			if !c.e.ReadOnly() {
				return errors.New("an entry of another leader arrived while this node's engine accepts writes")
			}
			if c.e.WritePos() != b.chunks[0].pos && !(b.chunks[0].pos.Off == 0 && b.chunks[0].pos.Seg == c.e.WritePos().Seg+1) {
				return fmt.Errorf("the engine's log is at %v, the entry starts at %v", c.e.WritePos(), b.chunks[0].pos)
			}
			c.boot = false
			for _, ch := range b.chunks {
				if err := c.e.Apply(ch.pos, ch.data); err != nil {
					return err
				}
			}
		}
		next.LSN, next.Pos = b.lastLSN, end
	}
	c.cur.Store(next)
	c.dirty.Store(true)
	c.Stats.Applied.Add(1)
	return nil
}

// flusher writes the applied record (no fsync) when it changed.
func (c *Cluster) flusher() {
	defer c.wg.Done()
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		if c.dirty.Swap(false) {
			c.app.write(*c.cur.Load(), false)
		}
	}
}

// ---- roles ----

func (c *Cluster) monitor() {
	defer c.wg.Done()
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	lastCompact := time.Now()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		info := c.g.Info()
		if c.writable.Load() {
			if info.Role != raft.Leader || info.Term != c.leaderTerm.Load() {
				c.restart(fmt.Sprintf("deposed (now %s in term %d)", info.Role, info.Term))
				return
			}
			if err := c.e.Failed(); err != nil {
				c.restart("engine failed: " + err.Error())
				return
			}
		} else if info.Role == raft.Leader && info.Commit >= info.TermStart && c.cur.Load().Index >= info.Commit {
			// every committed entry is in the engine, and the no-op of this
			// term is committed, so no earlier entry can change
			_, pos, _ := c.e.DurablePoint()
			c.repMu.Lock()
			c.sent = pos
			c.repMu.Unlock()
			if err := c.e.Promote(); err != nil {
				c.cfg.Logf("cluster: promote: %v", err)
				c.restart("promote failed")
				return
			}
			c.leaderTerm.Store(info.Term)
			c.writable.Store(true)
			c.cfg.Logf("cluster: node %d is the leader of term %d (engine lsn %d, log %d:%d)", c.cfg.ID, info.Term, c.e.DurableLSN(), pos.Seg, pos.Off)
		}
		if time.Since(lastCompact) > time.Second {
			lastCompact = time.Now()
			c.maybeCompact(info)
		}
	}
}

// maybeCompact trims the Raft log once it is long: it needs only the part
// a follower that is a little behind still wants; a node further behind
// gets a snapshot.
func (c *Cluster) maybeCompact(info raft.Info) {
	if int(info.Last-info.BaseIndex) <= c.cfg.CompactEntries {
		return
	}
	a := *c.cur.Load()
	if a.Index <= uint64(c.cfg.CompactKeep) {
		return
	}
	target := a.Index - uint64(c.cfg.CompactKeep)
	if target <= info.BaseIndex {
		return
	}
	// the base needs the position its data ends at: the last data entry at or before target
	var bi applied
	var found bool
	for i := target; i > info.BaseIndex; i-- {
		e, ok := c.g.Entry(i)
		if !ok {
			return
		}
		if e.Noop || len(e.Data) == 0 {
			continue
		}
		b, err := decodeBatch(e.Data)
		if err != nil {
			return
		}
		bi, found = applied{Index: i, LSN: b.lastLSN, Pos: b.end()}, true
		break
	}
	if !found {
		return
	}
	// Order matters: the applied record is synced first, so that a crash
	// after the compaction never finds an applied record behind the base.
	if err := c.app.write(a, true); err != nil {
		c.cfg.Logf("cluster: sync applied: %v", err)
		return
	}
	if err := c.writeBase(bi, c.termOf(bi.Index)); err != nil {
		c.cfg.Logf("cluster: %v", err)
		return
	}
	if err := c.g.Compact(bi.Index); err != nil {
		c.cfg.Logf("cluster: compact: %v", err)
		return
	}
	c.Stats.Compactions.Add(1)
	c.cfg.Logf("cluster: raft log compacted up to entry %d (%d entries kept)", bi.Index, info.Last-bi.Index)
}

func (c *Cluster) termOf(i uint64) uint64 {
	if e, ok := c.g.Entry(i); ok {
		return e.Term
	}
	return 0
}

// restart ends the process image in place (or calls Config.Restart).
func (c *Cluster) restart(reason string) {
	c.cfg.Logf("cluster: node %d restarts: %s", c.cfg.ID, reason)
	if c.cfg.Restart != nil {
		c.cfg.Restart(reason)
		return
	}
	if a := c.cur.Load(); a != nil {
		c.app.write(*a, true)
	}
	exe, err := os.Executable()
	if err == nil {
		err = syscall.Exec(exe, os.Args, os.Environ())
	}
	c.cfg.Logf("cluster: exec failed (%v): exiting for a supervisor to restart the node", err)
	os.Exit(75)
}

// ---- base of the Raft log (snap.json) ----

type baseFile struct {
	Index, Term, LSN uint64
	Seg              uint32
	Off              int64
}

func (c *Cluster) basePath() string { return filepath.Join(c.raftDir(), "snap.json") }

func (c *Cluster) readBase() {
	_, snap, _, _ := c.st.Load()
	b, err := os.ReadFile(c.basePath())
	if err != nil {
		return
	}
	var f baseFile
	if json.Unmarshal(b, &f) == nil && f.Index == snap.Index {
		c.base = applied{f.Index, f.LSN, wal.Pos{Seg: f.Seg, Off: f.Off}}
	}
}

func (c *Cluster) writeBase(a applied, term uint64) error {
	b, _ := json.Marshal(baseFile{a.Index, term, a.LSN, a.Pos.Seg, a.Pos.Off})
	if err := writeFileSync(c.basePath(), b); err != nil {
		return err
	}
	c.baseMu.Lock()
	c.base = a
	c.baseMu.Unlock()
	return nil
}

func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		if d, derr := os.Open(filepath.Dir(path)); derr == nil {
			d.Sync()
			d.Close()
		}
	}
	return err
}
