package cluster

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"stask/internal/raft"
	"stask/internal/repl"
	"stask/internal/wal"
)

// Frames of the cluster protocol, after the connection setup of
// internal/repl (TLS, mTLS, shared secret):
//
//	peer -> node  'H' u64 id           a stream of Raft messages follows
//	peer -> node  'M' message          (raft.EncodeMsg)
//	peer -> node  'Q' u64 id, i64 segMax    snapshot request
//	node -> peer  'S' u32 seg, bytes   snapshot: a log segment
//	node -> peer  'C' u16 n, name, bytes     snapshot: the checkpoint
//	node -> peer  'm' JSON snapMeta    snapshot: its Raft index (last)
//	node -> peer  'B'                  snapshot complete
//	node -> peer  'E' text             refusal
const (
	fHello   = 'H'
	fMsg     = 'M'
	fSnapQ   = 'Q'
	fBackupQ = 'K' // a backup client (not a peer) asks for the same snapshot
	fSeg     = 'S'
	fCkpt    = 'C'
	fMeta    = 'm'
	fDone    = 'B'
	fErr     = 'E'
)

type transport struct{ c *Cluster }

func (t *transport) Send(m raft.Msg) {
	t.c.mmu.RLock()
	p := t.c.peers[m.To]
	t.c.mmu.RUnlock()
	if p != nil {
		select {
		case p.out <- m:
		default: // a peer that cannot keep up loses messages, as the network may
		}
	}
}

// peerConn sends Raft messages to one peer over one connection, dialing
// again (with a short back-off) when it breaks.
type peerConn struct {
	c    *Cluster
	id   uint64
	addr string
	sec  *repl.Security
	out  chan raft.Msg
	quit chan struct{} // closed when the node is removed from the cluster
}

func (p *peerConn) run() {
	defer p.c.wg.Done()
	backoff := 50 * time.Millisecond
	for {
		select {
		case <-p.c.stop:
			return
		case <-p.quit:
			return
		default:
		}
		err := p.session()
		if err != nil && !p.c.closed.Load() {
			p.c.cfg.Logf("cluster: peer %d (%s): %v", p.id, p.addr, err)
		}
		select {
		case <-p.c.stop:
			return
		case <-p.quit:
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Second)
		// anything queued while the peer was away is stale
		for drained := false; !drained; {
			select {
			case <-p.out:
			default:
				drained = true
			}
		}
	}
}

func (p *peerConn) session() error {
	conn, _, err := repl.Dial(p.addr, p.sec)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { // closing on shutdown (or removal) ends the writes below
		select {
		case <-p.c.stop:
		case <-p.quit:
		}
		conn.Close()
	}()
	w := bufio.NewWriterSize(conn, 256<<10)
	if err := repl.WriteFrame(w, fHello, binary.LittleEndian.AppendUint64(nil, p.c.cfg.ID)); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for {
		var m raft.Msg
		select {
		case m = <-p.out:
		case <-p.c.stop:
			return nil
		case <-p.quit:
			return nil
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := repl.WriteFrame(w, fMsg, raft.EncodeMsg(m)); err != nil {
			return err
		}
		// send what is queued together
		for more := true; more; {
			select {
			case m = <-p.out:
				if err := repl.WriteFrame(w, fMsg, raft.EncodeMsg(m)); err != nil {
					return err
				}
			default:
				more = false
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
}

func (c *Cluster) acceptLoop() {
	defer c.wg.Done()
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		go c.serve(conn)
	}
}

func (c *Cluster) serve(raw net.Conn) {
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn, r, err := repl.Accept(raw, c.serverSec)
	if err != nil {
		raw.Close()
		return
	}
	defer conn.Close()
	typ, b, err := repl.ReadFrameMax(r, 4<<10)
	if err != nil || len(b) < 8 {
		return
	}
	conn.SetDeadline(time.Time{})
	from := binary.LittleEndian.Uint64(b)
	if typ == fBackupQ {
		// authenticated like a peer (secret or mTLS) but is not one
		err := c.serveSnapshot(conn, 0)
		if err != nil {
			c.cfg.Logf("cluster: backup: %v", err)
			repl.WriteFrame(conn, fErr, []byte(err.Error()))
		}
		return
	}
	if c.members0(from).Raft == "" || from == c.cfg.ID {
		return
	}
	switch typ {
	case fHello:
		go func() {
			<-c.stop
			conn.Close()
		}()
		for {
			typ, b, err := repl.ReadFrame(r)
			if err != nil {
				return
			}
			if typ != fMsg {
				return
			}
			m, err := raft.DecodeMsg(b)
			if err != nil || m.From != from {
				return
			}
			c.g.Step(m)
		}
	case fSnapQ:
		if len(b) < 16 {
			return
		}
		err := c.serveSnapshot(conn, int64(binary.LittleEndian.Uint64(b[8:])))
		if err != nil {
			c.cfg.Logf("cluster: snapshot for node %d: %v", from, err)
			repl.WriteFrame(conn, fErr, []byte(err.Error()))
		}
	}
}

// ---- snapshots ----

type snapMeta struct {
	Index, Term, LSN uint64
	Seg              uint32
	Off              int64
	SegMax           int64
	// the configuration at Index and the addresses of the nodes (a node
	// that installs the snapshot may not have seen the entries that added them)
	Voters  []uint64          `json:",omitempty"`
	Members map[uint64]member `json:",omitempty"`
}

// serveSnapshot sends the engine's base backup and the Raft entry it
// corresponds to. Only the leader's engine has a state that is known to
// be committed through a published point.
func (c *Cluster) serveSnapshot(conn net.Conn, segMax int64) error {
	if c.e.ReadOnly() || !c.writable.Load() {
		return errors.New("not the leader")
	}
	if segMax == 0 {
		segMax = c.e.Log().SegMax()
	}
	if segMax != c.e.Log().SegMax() {
		return fmt.Errorf("segment size %d, the cluster's is %d", segMax, c.e.Log().SegMax())
	}
	sh, err := c.e.NewShipper(wal.Pos{Seg: c.e.OldestSegment()})
	if err != nil {
		return err
	}
	defer sh.Close()
	w := bufio.NewWriterSize(conn, 1<<20)
	send := func(typ byte, parts ...[]byte) error {
		conn.SetWriteDeadline(time.Now().Add(time.Minute))
		return repl.WriteFrame(w, typ, parts...)
	}
	end, err := c.e.BaseBackup(sh, func(name string, data []byte) error {
		return send(fCkpt, binary.LittleEndian.AppendUint16(nil, uint16(len(name))), []byte(name), data)
	}, func(seg uint32, data []byte) error {
		return send(fSeg, binary.LittleEndian.AppendUint32(nil, seg), data)
	})
	if err != nil {
		return err
	}
	meta, err := c.metaAt(end)
	if err != nil {
		return err
	}
	meta.SegMax = segMax
	meta.Voters = c.g.VotersAt(meta.Index)
	meta.Members = c.memberList()
	mb, _ := json.Marshal(meta)
	if err := send(fMeta, mb); err != nil {
		return err
	}
	if err := send(fDone); err != nil {
		return err
	}
	c.cfg.Logf("cluster: sent a snapshot at entry %d (lsn %d)", meta.Index, meta.LSN)
	return w.Flush()
}

// metaAt finds the Raft entry whose batch ends exactly at pos.
func (c *Cluster) metaAt(pos wal.Pos) (snapMeta, error) {
	info := c.g.Info()
	if pos == (wal.Pos{}) && info.BaseIndex == 0 {
		return snapMeta{}, errors.New("the log is empty: nothing to snapshot yet")
	}
	for i := info.Applied; i > info.BaseIndex; i-- {
		e, ok := c.g.Entry(i)
		if !ok {
			break
		}
		if e.Noop || e.Conf || len(e.Data) == 0 {
			continue
		}
		b, err := decodeBatch(e.Data)
		if err != nil {
			return snapMeta{}, err
		}
		if b.end() == pos {
			return snapMeta{Index: i, Term: e.Term, LSN: b.lastLSN, Seg: pos.Seg, Off: pos.Off}, nil
		}
		if b.end().Less(pos) {
			break
		}
	}
	c.baseMu.Lock()
	base := c.base
	c.baseMu.Unlock()
	if base.Index == info.BaseIndex && base.Pos == pos {
		return snapMeta{Index: base.Index, Term: info.BaseTerm, LSN: base.LSN, Seg: pos.Seg, Off: pos.Off}, nil
	}
	return snapMeta{}, fmt.Errorf("no Raft entry ends at log position %d:%d", pos.Seg, pos.Off)
}

// needSnapshot runs inside the Raft node: start one fetch at a time.
func (c *Cluster) needSnapshot(leader uint64) {
	if !c.snapBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		err := c.fetchSnapshot(leader)
		if err != nil {
			c.cfg.Logf("cluster: snapshot from node %d: %v", leader, err)
			time.Sleep(time.Second)
			c.snapBusy.Store(false)
			return
		}
		c.restart("installed a snapshot")
	}()
}

func (c *Cluster) snapDir() string { return filepath.Join(c.cfg.Dir, "snap.tmp") }
func (c *Cluster) snapReady() string {
	return filepath.Join(c.cfg.Dir, "snap.ready")
}

func (c *Cluster) fetchSnapshot(leader uint64) error {
	addr := c.members0(leader).Raft
	sec, err := c.cfg.Settings.Replica(addr)
	if err != nil {
		return err
	}
	tmp := c.snapDir()
	if _, err := fetchInto(addr, sec, fSnapQ, c.cfg.ID, c.cfg.SegMax, tmp, c.cfg.Logf); err != nil {
		return err
	}
	// the marker: from here on the next start installs it
	if err := os.RemoveAll(c.snapReady()); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.snapReady()); err != nil {
		return err
	}
	c.Stats.Snapshots.Add(1)
	return nil
}

// SnapInfo describes a backup: the Raft entry and LSN it ends at.
type SnapInfo = snapMeta

// Backup takes a consistent snapshot of the cluster from the node at raftAddr
// (it must be the leader) into dir: wal/, ckpt/ and meta.json. Dropped into an
// empty node directory as snap.ready (RestoreNode) it makes the node start
// from that state. The settings are the -repl-* security settings.
func Backup(raftAddr string, st repl.Settings, dir string, logf func(string, ...any)) (SnapInfo, error) {
	sec, err := st.Replica(raftAddr)
	if err != nil {
		return SnapInfo{}, err
	}
	return fetchInto(raftAddr, sec, fBackupQ, 0, 0, dir, logf)
}

// RestoreNode installs a backup made by Backup into the (empty) data
// directory of a cluster node: the next start of the node adopts it.
func RestoreNode(backupDir, nodeDir string) error {
	if _, err := os.Stat(filepath.Join(backupDir, "meta.json")); err != nil {
		return fmt.Errorf("%s is not a cluster backup: %w", backupDir, err)
	}
	for _, e := range []string{"wal", "ckpt", "raft", "snap.ready"} {
		if _, err := os.Stat(filepath.Join(nodeDir, e)); err == nil {
			return fmt.Errorf("%s is not empty (%s exists): a node that was a cluster member must not be reset in place", nodeDir, e)
		}
	}
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(nodeDir, "snap.tmp")
	os.RemoveAll(tmp)
	if err := copyTree(backupDir, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(nodeDir, "snap.ready"))
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(t)
		if err != nil {
			return err
		}
		if _, err = io.Copy(out, in); err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	})
}

// fetchInto asks the node at addr for a snapshot (typ fSnapQ from a peer, or
// fBackupQ from a backup client) and writes it into tmp. segMax 0 accepts the
// node's segment size.
func fetchInto(addr string, sec *repl.Security, typ byte, from uint64, segMax int64, tmp string, logf func(string, ...any)) (snapMeta, error) {
	conn, r, err := repl.Dial(addr, sec)
	if err != nil {
		return snapMeta{}, err
	}
	defer conn.Close()
	req := binary.LittleEndian.AppendUint64(nil, from)
	req = binary.LittleEndian.AppendUint64(req, uint64(segMax))
	if err := repl.WriteFrame(conn, typ, req); err != nil {
		return snapMeta{}, err
	}
	os.RemoveAll(tmp)
	for _, d := range []string{"wal", "ckpt"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			return snapMeta{}, err
		}
	}
	var meta snapMeta
	haveMeta := false
	t0 := time.Now()
	var bytes int64
	for done := false; !done; {
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		typ, b, err := repl.ReadFrame(r)
		if err != nil {
			return snapMeta{}, err
		}
		switch typ {
		case fSeg:
			seg := binary.LittleEndian.Uint32(b)
			bytes += int64(len(b) - 4)
			if err := writeSnapFile(filepath.Join(tmp, "wal", fmt.Sprintf("%08d.wal", seg)), b[4:], segMax); err != nil {
				return snapMeta{}, err
			}
		case fCkpt:
			n := int(binary.LittleEndian.Uint16(b))
			bytes += int64(len(b) - 2 - n)
			if err := writeSnapFile(filepath.Join(tmp, "ckpt", filepath.Base(string(b[2:2+n]))), b[2+n:], 0); err != nil {
				return snapMeta{}, err
			}
		case fMeta:
			if err := json.Unmarshal(b, &meta); err != nil {
				return snapMeta{}, err
			}
			haveMeta = true
		case fDone:
			done = true
		case fErr:
			return snapMeta{}, fmt.Errorf("%s: %s", addr, b)
		default:
			return snapMeta{}, fmt.Errorf("unexpected frame %q", typ)
		}
	}
	if !haveMeta || meta.SegMax == 0 || (segMax != 0 && meta.SegMax != segMax) {
		return snapMeta{}, errors.New("snapshot without a usable description")
	}
	if segMax == 0 { // the segment size was not known while the segments arrived
		ents, _ := os.ReadDir(filepath.Join(tmp, "wal"))
		for _, e := range ents {
			if err := os.Truncate(filepath.Join(tmp, "wal", e.Name()), meta.SegMax); err != nil {
				return snapMeta{}, err
			}
		}
	}
	mb, _ := json.Marshal(meta)
	if err := writeSnapFile(filepath.Join(tmp, "meta.json"), mb, 0); err != nil {
		return snapMeta{}, err
	}
	logf("cluster: fetched a snapshot at entry %d: %d MiB in %v", meta.Index, bytes>>20, time.Since(t0).Round(time.Millisecond))
	return meta, nil
}

func writeSnapFile(path string, data []byte, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil && size > 0 {
		err = f.Truncate(size)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// finishSnapshot installs a fetched snapshot (Prepare, before the engine
// is opened). Each step can be repeated after a crash.
func (c *Cluster) finishSnapshot() error {
	ready := c.snapReady()
	mb, err := os.ReadFile(filepath.Join(ready, "meta.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var meta snapMeta
	if err := json.Unmarshal(mb, &meta); err != nil {
		return err
	}
	dir := c.cfg.Dir
	for _, d := range []string{"wal", "ckpt"} {
		if _, err := os.Stat(filepath.Join(ready, d)); err == nil {
			if err := os.RemoveAll(filepath.Join(dir, d)); err != nil {
				return err
			}
			if err := os.Rename(filepath.Join(ready, d), filepath.Join(dir, d)); err != nil {
				return err
			}
		}
	}
	// CDC files describe the old state; the engine rebuilds them from the replicated table
	os.RemoveAll(filepath.Join(dir, "slots"))
	os.Remove(filepath.Join(dir, "cdc.json"))
	st, err := raft.OpenFileStorage(filepath.Join(c.raftDir(), "raft.log"))
	if err != nil {
		return err
	}
	err = raft.InstallSnapshot(st, raft.Snapshot{Index: meta.Index, Term: meta.Term, Voters: meta.Voters})
	st.Close()
	if err != nil {
		return err
	}
	af, _, err := openApplied(filepath.Join(c.raftDir(), "applied.bin"))
	if err != nil {
		return err
	}
	err = af.write(applied{meta.Index, meta.LSN, wal.Pos{Seg: meta.Seg, Off: meta.Off}}, true)
	af.Close()
	if err != nil {
		return err
	}
	bb, _ := json.Marshal(baseFile{meta.Index, meta.Term, meta.LSN, meta.Seg, meta.Off})
	if err := writeFileSync(c.basePath(), bb); err != nil {
		return err
	}
	if len(meta.Members) > 0 {
		c.mmu.Lock()
		for id, m := range meta.Members {
			if c.members[id].Raft == "" {
				c.members[id] = m
			}
		}
		c.mmu.Unlock()
		c.saveMembers()
	}
	c.cfg.Logf("cluster: installed the snapshot at entry %d (lsn %d)", meta.Index, meta.LSN)
	return os.RemoveAll(ready)
}
