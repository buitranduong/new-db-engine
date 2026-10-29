// Package repl ships the log from a leader to replicas over TCP (see
// core/replica.go for what a replica is).
//
// A replica connects (over TLS and after proving a shared secret when
// configured, see security.go) and says where its log ends, or that it has nothing.
// The leader answers with its segment size, then, for an empty replica, a
// base backup (segments, then a checkpoint), and then streams durable log
// from where the replica's log ends. Every data frame carries the leader's
// oldest segment and durable LSN so that the replica can remove what the
// leader reclaimed; when idle the leader sends the same as a heartbeat. The
// replica acknowledges what it has made durable, which releases the
// leader's log up to there.
//
// Like a PostgreSQL replication slot, the leader keeps the log a replica
// still needs while it is disconnected (restarting, network trouble), for
// up to Server.KeepFor; a replica that comes back later than that finds
// its position reclaimed and needs a new base backup.
//
// Frames: [u8 type][u32 length][payload], integers little endian.
package repl

import (
	"bufio"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"stask/internal/core"
	"stask/internal/wal"
)

const (
	magic = "STASKREPL2" // 2: authentication before the hello

	fHello     = 'H' // replica: u8 fresh, u32 seg, i64 off, id
	fHeader    = 'h' // leader: i64 segMax
	fSegment   = 'S' // leader: u32 seg, bytes
	fCkpt      = 'C' // leader: u16 name length, name, bytes
	fBaseDone  = 'B' // leader: u32 seg, i64 off (where the replica's log ends)
	fData      = 'D' // leader: u32 seg, i64 off, u32 oldest, u64 durable, bytes
	fHeartbeat = 'K' // leader: u32 oldest, u64 durable
	fAck       = 'A' // replica: u32 seg, i64 off (durable on the replica)
	fError     = 'E' // leader: message
)

const maxFrame = 256 << 20

func writeFrame(w io.Writer, typ byte, parts ...[]byte) error {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	var h [5]byte
	h[0] = typ
	binary.LittleEndian.PutUint32(h[1:], uint32(n))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	for _, p := range parts {
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) { return readFrameMax(r, maxFrame) }

// readFrameMax reads a frame of at most max bytes (small before the peer
// is authenticated, so that it cannot make the reader allocate much).
func readFrameMax(r io.Reader, max uint32) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(h[1:])
	if n > max {
		return 0, nil, fmt.Errorf("frame of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	return h[0], b, nil
}

func posBytes(p wal.Pos) []byte {
	b := binary.LittleEndian.AppendUint32(nil, p.Seg)
	return binary.LittleEndian.AppendUint64(b, uint64(p.Off))
}

func parsePos(b []byte) (wal.Pos, error) {
	if len(b) < 12 {
		return wal.Pos{}, errors.New("short position")
	}
	return wal.Pos{Seg: binary.LittleEndian.Uint32(b), Off: int64(binary.LittleEndian.Uint64(b[4:]))}, nil
}

// ---- leader ----

// Server serves replicas of e.
type Server struct {
	// KeepFor is how long the log a disconnected replica needs is kept
	// (default 10 minutes).
	KeepFor time.Duration

	sec  *Security
	e    *core.Engine
	ln   net.Listener
	logf func(string, ...any)
	stop chan struct{}
	wg   sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]bool
	slots map[string]*slot
}

// slot keeps the log one replica needs.
type slot struct {
	sh        *core.Shipper
	connected bool
	lastSeen  time.Time
}

// Serve serves replicas of e on addr; sec (nil = none) secures the
// connections.
func Serve(e *core.Engine, addr string, sec *Security, logf func(string, ...any)) (*Server, error) {
	if err := sec.checkLeader(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{KeepFor: 10 * time.Minute, sec: sec, e: e, ln: ln, logf: logf, stop: make(chan struct{}),
		conns: map[net.Conn]bool{}, slots: map[string]*slot{}}
	s.wg.Add(2)
	go s.expire()
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns[c] = true
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				if err := s.serve(c); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					s.logf("repl: replica %s: %v", c.RemoteAddr(), err)
				}
				c.Close()
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
		}
	}()
	return s, nil
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

// expire releases the log kept for replicas disconnected for too long.
func (s *Server) expire() {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		for id, sl := range s.slots {
			if !sl.connected && time.Since(sl.lastSeen) > s.KeepFor {
				sl.sh.Close()
				delete(s.slots, id)
				s.logf("repl: replica %s gone for %v: its log is no longer kept", id, s.KeepFor)
			}
		}
		s.mu.Unlock()
	}
}

// acquire returns the slot of replica id, positioned at from, connected.
func (s *Server) acquire(id string, from wal.Pos) (*slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sl := s.slots[id]; sl != nil {
		if sl.connected {
			return nil, fmt.Errorf("replica %s is already connected", id)
		}
		if p, ok := sl.sh.Position(); ok && from.Less(p) {
			return nil, fmt.Errorf("replica %s asks for %d:%d, before what it acknowledged (%d:%d)", id, from.Seg, from.Off, p.Seg, p.Off)
		}
		sl.sh.SetPos(from)
		sl.connected = true
		return sl, nil
	}
	sh, err := s.e.NewShipper(from)
	if err != nil {
		return nil, err
	}
	sl := &slot{sh: sh, connected: true}
	s.slots[id] = sl
	return sl, nil
}

func (s *Server) release(sl *slot) {
	s.mu.Lock()
	sl.connected, sl.lastSeen = false, time.Now()
	s.mu.Unlock()
}

func (s *Server) Close() {
	close(s.stop)
	s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	for _, sl := range s.slots {
		sl.sh.Close()
	}
	s.slots = map[string]*slot{}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) serve(c net.Conn) error {
	c.SetDeadline(time.Now().Add(handshakeLimit))
	c, r, err := s.accept(c)
	if err != nil {
		return err
	}
	typ, b, err := readFrameMax(r, preAuthFrame)
	if err != nil || typ != fHello || len(b) < 13 {
		return errors.New("bad hello")
	}
	c.SetDeadline(time.Time{})
	fresh := b[0] == 1
	from, _ := parsePos(b[1:])
	id := string(b[13:])
	if id == "" {
		return errors.New("hello without replica id")
	}
	w := bufio.NewWriterSize(c, 1<<20)
	var wmu sync.Mutex // the heartbeat path and the stream share w
	send := func(typ byte, parts ...[]byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if err := writeFrame(w, typ, parts...); err != nil {
			return err
		}
		return w.Flush()
	}
	fail := func(err error) error {
		send(fError, []byte(err.Error()))
		return err
	}
	if fresh {
		from = wal.Pos{Seg: s.e.OldestSegment()}
	}
	sl, err := s.acquire(id, from)
	if err != nil {
		return fail(err)
	}
	defer s.release(sl)
	sh := sl.sh
	if err := send(fHeader, binary.LittleEndian.AppendUint64(nil, uint64(s.e.Log().SegMax()))); err != nil {
		return err
	}
	if fresh {
		t0 := time.Now()
		var bytes int64
		end, err := s.e.BaseBackup(sh, func(name string, data []byte) error {
			bytes += int64(len(data))
			nb := binary.LittleEndian.AppendUint16(nil, uint16(len(name)))
			return send(fCkpt, nb, []byte(name), data)
		}, func(seg uint32, data []byte) error {
			bytes += int64(len(data))
			return send(fSegment, binary.LittleEndian.AppendUint32(nil, seg), data)
		})
		if err != nil {
			return fail(err)
		}
		if err := send(fBaseDone, posBytes(end)); err != nil {
			return err
		}
		s.logf("repl: base backup to %s: %d MiB in %v", c.RemoteAddr(), bytes>>20, time.Since(t0).Round(time.Millisecond))
		// the replica opens its engine and follows on a new connection;
		// the slot keeps the log from end meanwhile
		sh.SetPos(end)
		return nil
	}
	s.logf("repl: streaming to %s from %d:%d", c.RemoteAddr(), from.Seg, from.Off)

	// acknowledgements release the leader's log
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			typ, b, err := readFrame(r)
			if err != nil {
				c.Close()
				return
			}
			if typ == fAck {
				if p, err := parsePos(b); err == nil {
					sh.SetPos(p)
				}
			}
		}
	}()
	for {
		// oldest is read before the durable point: everything durable when
		// that segment was reclaimed is at or below the durable LSN sent
		oldest := s.e.OldestSegment()
		idle := make(chan struct{})
		t := time.AfterFunc(time.Second, func() { close(idle) })
		data, next, durable, err := sh.Read(from, 4<<20, idle)
		t.Stop()
		switch {
		case errors.Is(err, core.ErrIdle):
			hb := binary.LittleEndian.AppendUint32(nil, oldest)
			hb = binary.LittleEndian.AppendUint64(hb, s.e.DurableLSN())
			if err := send(fHeartbeat, hb); err != nil {
				return err
			}
			continue
		case err != nil:
			return fail(err)
		}
		hdr := posBytes(from)
		hdr = binary.LittleEndian.AppendUint32(hdr, oldest)
		hdr = binary.LittleEndian.AppendUint64(hdr, durable)
		if err := send(fData, hdr, data); err != nil {
			return err
		}
		from = next
		select {
		case <-s.stop:
			return nil
		default:
		}
	}
}

// ---- replica ----

// replicaInfo is kept in the replica's data directory.
type replicaInfo struct {
	ID     string `json:"id"` // names the replica's slot on the leader
	Leader string `json:"leader"`
	SegMax int64  `json:"segment_size"`
}

func readInfo(dir string) (replicaInfo, error) {
	var ri replicaInfo
	b, err := os.ReadFile(infoPath(dir))
	if err == nil {
		err = json.Unmarshal(b, &ri)
	}
	return ri, err
}

func infoPath(dir string) string { return filepath.Join(dir, "replica.json") }

// Prepare makes dir ready to open as a replica of leader: if it holds no
// replica yet, it fetches a base backup. It returns the segment size the
// engine must be opened with.
func Prepare(leader, dir string, sec *Security, logf func(string, ...any)) (int64, error) {
	if ri, err := readInfo(dir); err == nil {
		return ri.SegMax, nil
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	if _, err := os.Stat(filepath.Join(dir, "wal")); err == nil {
		return 0, fmt.Errorf("%s holds data that is not a replica", dir)
	}
	return baseBackup(leader, dir, sec, logf, "replica.json")
}

// BackupInfo is backup.json of a base backup.
type BackupInfo struct {
	Leader string    `json:"leader"`
	SegMax int64     `json:"segment_size"`
	Time   time.Time `json:"time"`
}

// BackupPath is the path of backup.json in a base backup directory.
func BackupPath(dir string) string { return filepath.Join(dir, "backup.json") }

// BaseBackup copies into dir (which must hold no database) the leader's
// newest checkpoint and the log after it, as pg_basebackup does. Opened
// as is, it is the leader as of the backup; with the archive the leader
// writes since (-archive-dir), it restores to any later point
// (core/archive.go). The leader keeps a replication slot for the backup
// until its slot expiry.
func BaseBackup(leader, dir string, sec *Security, logf func(string, ...any)) (int64, error) {
	if _, err := os.Stat(filepath.Join(dir, "wal")); err == nil {
		return 0, fmt.Errorf("%s already holds a database", dir)
	}
	return baseBackup(leader, dir, sec, logf, "backup.json")
}

// baseBackup fetches a base backup into dir; infoName (replica.json or
// backup.json) is written last.
func baseBackup(leader, dir string, sec *Security, logf func(string, ...any), infoName string) (int64, error) {
	t0 := time.Now()
	c, r, err := dial(leader, sec)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	var rnd [8]byte
	crand.Read(rnd[:])
	id := hex.EncodeToString(rnd[:])
	if err := hello(c, true, wal.Pos{}, id); err != nil {
		return 0, err
	}
	segMax, err := readHeader(r)
	if err != nil {
		return 0, err
	}
	tmp := dir + ".base"
	os.RemoveAll(tmp)
	for _, d := range []string{"wal", "ckpt"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			return 0, err
		}
	}
	write := func(path string, data []byte, size int64) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err == nil && size > 0 {
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
	var bytes int64
	for done := false; !done; {
		typ, b, err := readFrame(r)
		if err != nil {
			return 0, err
		}
		switch typ {
		case fSegment:
			seg := binary.LittleEndian.Uint32(b)
			bytes += int64(len(b) - 4)
			if err := write(filepath.Join(tmp, "wal", fmt.Sprintf("%08d.wal", seg)), b[4:], segMax); err != nil {
				return 0, err
			}
		case fCkpt:
			n := int(binary.LittleEndian.Uint16(b))
			name := filepath.Base(string(b[2 : 2+n]))
			bytes += int64(len(b) - 2 - n)
			if err := write(filepath.Join(tmp, "ckpt", name), b[2+n:], 0); err != nil {
				return 0, err
			}
		case fBaseDone:
			done = true
		case fError:
			return 0, fmt.Errorf("leader: %s", b)
		default:
			return 0, fmt.Errorf("unexpected frame %q in base backup", typ)
		}
	}
	ib, _ := json.Marshal(replicaInfo{ID: id, Leader: leader, SegMax: segMax})
	if infoName == "backup.json" {
		ib, _ = json.Marshal(BackupInfo{Leader: leader, SegMax: segMax, Time: time.Now().UTC()})
	}
	if err := write(filepath.Join(tmp, infoName), ib, 0); err != nil {
		return 0, err
	}
	// the directory becomes a replica in one rename
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	// replica.json last: until it exists the directory is not a replica
	for _, d := range []string{"wal", "ckpt", infoName} {
		if err := os.Rename(filepath.Join(tmp, d), filepath.Join(dir, d)); err != nil {
			return 0, err
		}
	}
	os.RemoveAll(tmp)
	logf("repl: base backup from %s into %s: %d MiB in %v", leader, dir, bytes>>20, time.Since(t0).Round(time.Millisecond))
	return segMax, nil
}

func hello(w io.Writer, fresh bool, p wal.Pos, id string) error {
	f := byte(0)
	if fresh {
		f = 1
	}
	return writeFrame(w, fHello, []byte{f}, posBytes(p), []byte(id))
}

func readHeader(r io.Reader) (int64, error) {
	typ, b, err := readFrame(r)
	if err != nil {
		return 0, err
	}
	if typ == fError {
		return 0, fmt.Errorf("leader: %s", b)
	}
	if typ != fHeader || len(b) < 8 {
		return 0, fmt.Errorf("unexpected frame %q", typ)
	}
	return int64(binary.LittleEndian.Uint64(b)), nil
}

// Follow applies the leader's log to e (opened as a replica) until stop,
// reconnecting after errors.
func Follow(e *core.Engine, leader string, sec *Security, stop <-chan struct{}, logf func(string, ...any)) {
	for {
		err := followOnce(e, leader, sec, stop, logf)
		select {
		case <-stop:
			return
		default:
		}
		logf("repl: following %s: %v (retrying)", leader, err)
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
		}
	}
}

func followOnce(e *core.Engine, leader string, sec *Security, stop <-chan struct{}, logf func(string, ...any)) error {
	c, r, err := dial(leader, sec)
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			c.Close()
		case <-done:
		}
	}()
	ri, err := readInfo(filepath.Dir(e.Log().Dir()))
	if err != nil {
		return err
	}
	if err := hello(c, false, e.WritePos(), ri.ID); err != nil {
		return err
	}
	segMax, err := readHeader(r)
	if err != nil {
		return err
	}
	if segMax != e.Log().SegMax() {
		return fmt.Errorf("leader segment size %d, replica %d", segMax, e.Log().SegMax())
	}
	logf("repl: following %s from %d:%d (lsn %d)", leader, e.WritePos().Seg, e.WritePos().Off, e.DurableLSN())
	trimTo, trimAfter := uint32(0), uint64(0) // remove segments < trimTo once durable >= trimAfter
	var lastTrim time.Time
	for {
		typ, b, err := readFrame(r)
		if err != nil {
			return err
		}
		var oldest uint32
		var durable uint64
		switch typ {
		case fData:
			if len(b) < 24 {
				return errors.New("short data frame")
			}
			at, _ := parsePos(b)
			oldest = binary.LittleEndian.Uint32(b[12:])
			durable = binary.LittleEndian.Uint64(b[16:])
			if err := e.Apply(at, b[24:]); err != nil {
				return err
			}
			if err := writeFrame(c, fAck, posBytes(e.WritePos())); err != nil {
				return err
			}
		case fHeartbeat:
			if len(b) < 12 {
				return errors.New("short heartbeat")
			}
			oldest = binary.LittleEndian.Uint32(b)
			durable = binary.LittleEndian.Uint64(b[4:])
		case fError:
			return fmt.Errorf("leader: %s", b)
		default:
			return fmt.Errorf("unexpected frame %q", typ)
		}
		if oldest > trimTo {
			trimTo, trimAfter = oldest, durable
		}
		if trimTo > e.OldestSegment() && e.DurableLSN() >= trimAfter && time.Since(lastTrim) > time.Second {
			lastTrim = time.Now()
			if _, err := e.Trim(trimTo); err != nil {
				logf("repl: %v", err)
			}
		}
	}
}
