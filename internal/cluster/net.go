package cluster

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
	fHello = 'H'
	fMsg   = 'M'
	fSnapQ = 'Q'
	fSeg   = 'S'
	fCkpt  = 'C'
	fMeta  = 'm'
	fDone  = 'B'
	fErr   = 'E'
)

type transport struct{ c *Cluster }

func (t *transport) Send(m raft.Msg) {
	if p := t.c.peers[m.To]; p != nil {
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
}

func (p *peerConn) run() {
	defer p.c.wg.Done()
	backoff := 50 * time.Millisecond
	for {
		select {
		case <-p.c.stop:
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
	go func() { // closing on shutdown ends the writes below
		<-p.c.stop
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
	if _, ok := c.cfg.Peers[from]; !ok || from == c.cfg.ID {
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
}

// serveSnapshot sends the engine's base backup and the Raft entry it
// corresponds to. Only the leader's engine has a state that is known to
// be committed through a published point.
func (c *Cluster) serveSnapshot(conn net.Conn, segMax int64) error {
	if c.e.ReadOnly() || !c.writable.Load() {
		return errors.New("not the leader")
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
		if e.Noop || len(e.Data) == 0 {
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
	addr := c.cfg.Peers[leader]
	sec, err := c.cfg.Settings.Replica(addr)
	if err != nil {
		return err
	}
	conn, r, err := repl.Dial(addr, sec)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := binary.LittleEndian.AppendUint64(nil, c.cfg.ID)
	req = binary.LittleEndian.AppendUint64(req, uint64(c.cfg.SegMax))
	if err := repl.WriteFrame(conn, fSnapQ, req); err != nil {
		return err
	}
	tmp := c.snapDir()
	os.RemoveAll(tmp)
	for _, d := range []string{"wal", "ckpt"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			return err
		}
	}
	write := func(path string, data []byte, size int64) error {
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
	var meta snapMeta
	haveMeta := false
	t0 := time.Now()
	var bytes int64
	for done := false; !done; {
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		typ, b, err := repl.ReadFrame(r)
		if err != nil {
			return err
		}
		switch typ {
		case fSeg:
			seg := binary.LittleEndian.Uint32(b)
			bytes += int64(len(b) - 4)
			if err := write(filepath.Join(tmp, "wal", fmt.Sprintf("%08d.wal", seg)), b[4:], c.cfg.SegMax); err != nil {
				return err
			}
		case fCkpt:
			n := int(binary.LittleEndian.Uint16(b))
			bytes += int64(len(b) - 2 - n)
			if err := write(filepath.Join(tmp, "ckpt", filepath.Base(string(b[2:2+n]))), b[2+n:], 0); err != nil {
				return err
			}
		case fMeta:
			if err := json.Unmarshal(b, &meta); err != nil {
				return err
			}
			haveMeta = true
		case fDone:
			done = true
		case fErr:
			return fmt.Errorf("node %d: %s", leader, b)
		default:
			return fmt.Errorf("unexpected frame %q", typ)
		}
	}
	if !haveMeta || meta.SegMax != c.cfg.SegMax {
		return errors.New("snapshot without a usable description")
	}
	mb, _ := json.Marshal(meta)
	if err := write(filepath.Join(tmp, "meta.json"), mb, 0); err != nil {
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
	c.cfg.Logf("cluster: fetched a snapshot at entry %d from node %d: %d MiB in %v", meta.Index, leader, bytes>>20, time.Since(t0).Round(time.Millisecond))
	return nil
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
	err = raft.InstallSnapshot(st, raft.Snapshot{Index: meta.Index, Term: meta.Term})
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
	c.cfg.Logf("cluster: installed the snapshot at entry %d (lsn %d)", meta.Index, meta.LSN)
	return os.RemoveAll(ready)
}
