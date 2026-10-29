// Package pgwire serves the STASK SQL dialect over the PostgreSQL
// frontend/backend protocol v3 (simple and extended query), so standard
// drivers (libpq/psql, pgx, JDBC, psycopg) can connect.
package pgwire

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/core"
	"stask/internal/sql"
)

// Type OIDs (pg_type).
const (
	oidBool        = 16
	oidInt8        = 20
	oidInt2        = 21
	oidInt4        = 23
	oidText        = 25
	oidFloat4      = 700
	oidFloat8      = 701
	oidUnknown     = 705
	oidVarchar     = 1043
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidNumeric     = 1700
	oidDate        = 1082
	oidInterval    = 1186
	oidTextArray   = 1009
	oidBytea       = 17
	oidJSON        = 114
	oidJSONB       = 3802
	oidUUID        = 2950
	oidName        = 19
	oidBpchar      = 1042
	oidTime        = 1083
)

// dataCols are the columns the rows of portal p are announced with.
func dataCols(p *portal) []sql.Column {
	if p.ps.desc != nil && len(p.ps.desc.Cols) == len(p.res.Cols) {
		return p.ps.desc.Cols
	}
	return p.res.Cols
}

func oidOf(k sql.Kind) uint32 {
	switch k {
	case sql.KInt:
		return oidInt8
	case sql.KFloat:
		return oidFloat8
	case sql.KBool:
		return oidBool
	case sql.KTime:
		return oidTimestamptz
	case sql.KTimestamp:
		return oidTimestamp
	case sql.KNumeric:
		return oidNumeric
	case sql.KUUID:
		return oidUUID
	case sql.KBytea:
		return oidBytea
	case sql.KJSON:
		return oidJSON
	case sql.KJSONB:
		return oidJSONB
	case sql.KDate:
		return oidDate
	case sql.KInterval:
		return oidInterval
	case sql.KArray:
		return oidTextArray
	}
	return oidText
}

func kindOfOID(oid uint32) (sql.Kind, bool) {
	switch oid {
	case oidInt8, oidInt2, oidInt4:
		return sql.KInt, true
	case oidFloat4, oidFloat8:
		return sql.KFloat, true
	case oidNumeric:
		return sql.KNumeric, true
	case oidBool:
		return sql.KBool, true
	case oidTimestamp:
		return sql.KTimestamp, true
	case oidTimestamptz:
		return sql.KTime, true
	case oidDate:
		return sql.KDate, true
	case oidUUID:
		return sql.KUUID, true
	case oidBytea:
		return sql.KBytea, true
	case oidJSON:
		return sql.KJSON, true
	case oidJSONB:
		return sql.KJSONB, true
	case oidText, oidVarchar, oidName, oidBpchar:
		return sql.KText, true
	}
	return 0, false
}

// Server accepts PostgreSQL connections.
type Server struct {
	E        *core.Engine
	Password string // empty = trust authentication
	// Auth: AuthTrust, AuthPassword or AuthSCRAM (auth.go); empty =
	// AuthPassword when Password is set, else AuthTrust.
	Auth string
	// TLS: answer SSLRequest with 'S' and accept direct SSL connections
	// (nil = 'N'). TLSRequire refuses startups that are not encrypted.
	TLS        *tls.Config
	TLSRequire bool
	MaxConns   int64 // 0 = unlimited; beyond it new sessions get SQLSTATE 53300
	// RolesOnly closes the shared-password login once a stored superuser
	// role that can log in exists: only roles (CREATE ROLE) then connect.
	RolesOnly bool
	Logf      func(string, ...any)
	Views     sql.Views // derived views SELECT aggregates may be answered from

	// Session defaults of statement_timeout,
	// idle_in_transaction_session_timeout and idle_session_timeout
	// (0 = off; sessions may SET their own).
	StatementTimeout   time.Duration
	IdleInTxTimeout    time.Duration
	IdleSessionTimeout time.Duration

	// TCP keepalive of accepted connections, as PostgreSQL's
	// tcp_keepalives_idle/_interval/_count (0 = the OS default) and
	// tcp_user_timeout (0 = the OS default; Linux only). NoKeepAlive
	// turns keepalive probes off.
	NoKeepAlive       bool
	KeepAliveIdle     time.Duration
	KeepAliveInterval time.Duration
	KeepAliveCount    int
	TCPUserTimeout    time.Duration

	ln      net.Listener
	nextPID atomic.Int32
	mu      sync.Mutex
	conns   map[int32]*conn
	Active  atomic.Int64

	scram  *core.SCRAM      // AuthSCRAM: derived from Password
	cbHash func() hash.Hash // tls-server-end-point hash (nil = no -PLUS)
}

func (s *Server) Serve(ln net.Listener) error {
	s.ln = ln
	s.conns = map[int32]*conn{}
	if s.Logf == nil {
		s.Logf = log.Printf
	}
	if err := s.prepareAuth(); err != nil {
		ln.Close()
		return err
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// EMFILE and friends: back off instead of spinning
			s.Logf("pgwire: accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) Close() error {
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

type prepared struct {
	query  string
	stmt   sql.Stmt
	params []uint32 // parameter OIDs
	desc   *sql.Description
}

type portal struct {
	ps      *prepared
	params  []sql.Datum
	formats []int16 // result formats
	res     *sql.Result
	sent    int
	started bool
}

type conn struct {
	s       *Server
	nc      net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	sess    *sql.Session
	pid     int32
	secret  int32
	stmts   map[string]*prepared
	portals map[string]*portal
	skip    bool // after an error in extended mode, discard until Sync
	over    bool // accepted beyond MaxConns: refuse at startup
	tls     bool // the connection was upgraded to TLS
	rbuf    []byte
	// a read deadline enforces an idle timeout: the SQLSTATE and message
	// sent when it expires (code empty: no deadline set)
	idleCode, idleMsg string

	// Notifications (LISTEN): a goroutine delivers them while the session
	// is idle; wmu serializes it with the message loop, which holds wmu
	// while it handles a message. inBatch: an extended-protocol sequence
	// is open (until Sync).
	wmu       sync.Mutex
	inBatch   bool
	notifying bool
	done      chan struct{}

	// replication=database: a logical walsender (walsender.go)
	replication bool
	dbname      string
	user, app   string // startup parameters
	role        string // the stored role of the session; "" = bootstrap superuser
	tempSlots   []string
}

func (s *Server) handle(nc net.Conn) {
	n := s.Active.Add(1)
	defer s.Active.Add(-1)
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		s.setKeepAlive(tc)
	}
	c := &conn{s: s, nc: nc, r: bufio.NewReaderSize(nc, 8192), w: bufio.NewWriterSize(nc, 8192),
		stmts: map[string]*prepared{}, portals: map[string]*portal{}, over: s.MaxConns > 0 && n > s.MaxConns}
	defer func() { c.nc.Close() }() // the TLS conn after an upgrade
	if err := c.startup(); err != nil {
		if !errors.Is(err, io.EOF) {
			s.Logf("pgwire: startup from %v: %v", nc.RemoteAddr(), err)
		}
		return
	}
	c.sess = sql.NewSession(s.E)
	c.sess.Views = s.Views
	c.sess.PID = c.pid
	c.sess.SetRole(c.role)
	host, port, _ := net.SplitHostPort(nc.RemoteAddr().String())
	pn, _ := strconv.Atoi(port)
	c.sess.Track(c.user, c.app, host, int64(pn))
	c.sess.SetTimeoutDefaults(s.StatementTimeout, s.IdleInTxTimeout, s.IdleSessionTimeout)
	c.done = make(chan struct{})
	defer close(c.done)
	defer func() {
		c.wmu.Lock()
		c.sess.Close()
		c.dropTempSlots()
		c.wmu.Unlock()
	}()
	s.mu.Lock()
	s.conns[c.pid] = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c.pid)
		s.mu.Unlock()
	}()
	if err := c.loop(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.Logf("pgwire: connection %d: %v", c.pid, err)
	}
}

// ---- framing ----

func (c *conn) readMsg() (byte, []byte, error) {
	t, err := c.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if c.idleCode != "" { // the client is no longer idle
		c.nc.SetReadDeadline(time.Time{})
		c.idleCode = ""
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:])) - 4
	if n < 0 || n > 1<<30 {
		return 0, nil, fmt.Errorf("bad message length %d", n)
	}
	// Message bodies are only used until the next read (everything kept is
	// copied out), so one buffer per connection serves all ordinary messages.
	buf := c.rbuf
	if n <= 64<<10 {
		if cap(buf) < n {
			c.rbuf = make([]byte, max(n, 512))
		}
		buf = c.rbuf[:n]
	} else {
		buf = make([]byte, n)
	}
	_, err = io.ReadFull(c.r, buf)
	return t, buf, err
}

type msg struct{ b []byte }

func newMsg(t byte) *msg { return &msg{b: []byte{t, 0, 0, 0, 0}} }
func (m *msg) byte1(b byte) *msg {
	m.b = append(m.b, b)
	return m
}
func (m *msg) int16(v int) *msg {
	m.b = binary.BigEndian.AppendUint16(m.b, uint16(v))
	return m
}
func (m *msg) int32(v int) *msg {
	m.b = binary.BigEndian.AppendUint32(m.b, uint32(v))
	return m
}
func (m *msg) str(s string) *msg {
	m.b = append(append(m.b, s...), 0)
	return m
}
func (m *msg) bytes(b []byte) *msg {
	m.b = append(m.b, b...)
	return m
}

func (c *conn) send(m *msg) error {
	binary.BigEndian.PutUint32(m.b[1:], uint32(len(m.b)-1))
	_, err := c.w.Write(m.b)
	return err
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) int16() int {
	if len(r.b) < 2 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	v := int(int16(binary.BigEndian.Uint16(r.b)))
	r.b = r.b[2:]
	return v
}
func (r *reader) int32() int {
	if len(r.b) < 4 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	v := int(int32(binary.BigEndian.Uint32(r.b)))
	r.b = r.b[4:]
	return v
}
func (r *reader) str() string {
	i := 0
	for i < len(r.b) && r.b[i] != 0 {
		i++
	}
	if i == len(r.b) {
		r.err = io.ErrUnexpectedEOF
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return s
}
func (r *reader) bytes(n int) []byte {
	if n < 0 || len(r.b) < n {
		r.err = io.ErrUnexpectedEOF
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// ---- startup ----

func (c *conn) startup() error {
	if c.s.TLS != nil {
		// PostgreSQL 17 direct SSL: a TLS handshake record (0x16) instead
		// of a length; a startup packet's first byte is 0 (length < 16MB)
		if b, err := c.r.Peek(1); err != nil {
			return err
		} else if b[0] == 0x16 {
			if err := c.startTLS(true); err != nil {
				return err
			}
		}
	}
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(hdr[:])) - 4
		if n < 4 || n > 10000 {
			return fmt.Errorf("bad startup length %d", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return err
		}
		code := binary.BigEndian.Uint32(buf)
		switch code {
		case 80877103, 80877104: // SSLRequest, GSSENCRequest
			if c.tls {
				c.sendFatal("08P01", "encryption requested on an encrypted connection")
				c.w.Flush()
				return errors.New("encryption request after TLS")
			}
			if code == 80877104 || c.s.TLS == nil { // no GSSAPI; no certificate
				if _, err := c.nc.Write([]byte{'N'}); err != nil {
					return err
				}
				continue
			}
			// bytes sent before the handshake would be read as if they
			// had been encrypted (buffer stuffing, CVE-2021-23214)
			if c.r.Buffered() > 0 {
				c.sendFatal("08P01", "received unencrypted data after SSL request")
				c.w.Flush()
				return errors.New("data after SSLRequest")
			}
			if _, err := c.nc.Write([]byte{'S'}); err != nil {
				return err
			}
			if err := c.startTLS(false); err != nil {
				return err
			}
			continue
		case 80877102: // CancelRequest
			// no reply either way, so that the request cannot probe
			// for valid keys
			if len(buf) >= 12 {
				pid := int32(binary.BigEndian.Uint32(buf[4:]))
				c.s.mu.Lock()
				target := c.s.conns[pid]
				c.s.mu.Unlock()
				if target != nil {
					var want [4]byte
					binary.BigEndian.PutUint32(want[:], uint32(target.secret))
					if subtle.ConstantTimeCompare(want[:], buf[8:12]) == 1 {
						target.sess.Cancel()
					}
				}
			}
			return io.EOF
		case 196608: // protocol 3.0
		default:
			c.sendError(&sql.Error{Code: "08P01", Msg: fmt.Sprintf("unsupported frontend protocol %d.%d", code>>16, code&0xffff)})
			c.w.Flush()
			return fmt.Errorf("unsupported protocol %d", code)
		}
		if c.over {
			c.sendError(&sql.Error{Code: "53300", Msg: "sorry, too many clients already"})
			c.w.Flush()
			return io.EOF
		}
		params := map[string]string{}
		r := &reader{b: buf[4:]}
		for r.err == nil && len(r.b) > 1 {
			k := r.str()
			v := r.str()
			params[k] = v
		}
		switch strings.ToLower(params["replication"]) {
		case "", "false", "off", "no", "0":
		case "database":
			c.replication = true
		default:
			c.sendError(&sql.Error{Code: "0A000", Msg: "physical replication is not supported (use replication=database)"})
			c.w.Flush()
			return io.EOF
		}
		c.dbname = params["database"]
		if c.s.TLSRequire && !c.tls {
			// what pg_hba.conf with only hostssl lines answers
			host, _, _ := net.SplitHostPort(c.nc.RemoteAddr().String())
			c.sendFatal("28000", fmt.Sprintf("no pg_hba.conf entry for host %q, user %q, database %q, no encryption", host, params["user"], c.dbname))
			c.w.Flush()
			return errors.New("TLS required")
		}
		role, err := c.authenticate(params["user"])
		if err != nil {
			return err
		}
		c.user, c.app, c.role = params["user"], params["application_name"], role
		isSuper := "on"
		if role != "" {
			r, ok := c.s.E.Role(role)
			if !ok {
				c.sendFatal("28000", fmt.Sprintf("role %q does not exist", role))
				c.w.Flush()
				return errors.New("role dropped during login")
			}
			if !r.Super {
				isSuper = "off"
			}
			// as pg_hba.conf's "replication" entries and the REPLICATION
			// attribute: a walsender is for superusers and replication roles
			if c.replication && !r.Super && !r.Replication {
				c.sendFatal("42501", "permission denied to start WAL sender: must be superuser or replication role")
				c.w.Flush()
				return errors.New("permission denied to start WAL sender")
			}
		}
		c.pid = c.s.nextPID.Add(1)
		// the cancel key: unpredictable, so that only the client that
		// received it can cancel the session's statements
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		c.secret = int32(binary.BigEndian.Uint32(key[:]))
		c.send(newMsg('R').int32(0))
		// A replication connection reports 15: a PostgreSQL 16 subscriber
		// then reads the published tables from pg_publication_tables
		// rather than with pg_get_publication_tables() joined to pg_class,
		// which STASK does not have; the protocol is the same.
		version := "16.0"
		if c.replication {
			version = "15.0"
		}
		for _, kv := range [][2]string{
			{"server_version", version}, {"server_encoding", "UTF8"}, {"client_encoding", "UTF8"},
			{"DateStyle", "ISO, MDY"}, {"TimeZone", "UTC"}, {"integer_datetimes", "on"},
			{"standard_conforming_strings", "on"}, {"application_name", params["application_name"]},
			{"is_superuser", isSuper}, {"session_authorization", params["user"]}, {"IntervalStyle", "postgres"},
		} {
			c.send(newMsg('S').str(kv[0]).str(kv[1]))
		}
		c.send(newMsg('K').int32(int(c.pid)).int32(int(c.secret)))
		c.send(newMsg('Z').byte1('I'))
		return c.w.Flush()
	}
}

func (c *conn) ready() {
	if c.sess.TxStatus() == 'I' {
		c.sendNotifications()
	}
	c.send(newMsg('Z').byte1(c.sess.TxStatus()))
}

// sendNotifications writes the waiting notifications (NotificationResponse).
func (c *conn) sendNotifications() {
	l := c.sess.Listener()
	if l == nil {
		return
	}
	for _, n := range l.Take() {
		c.send(newMsg('A').int32(int(n.PID)).str(n.Channel).str(n.Payload))
	}
}

// startNotifier starts delivering notifications while the session is idle
// between commands (once the session has listened).
func (c *conn) startNotifier() {
	l := c.sess.Listener()
	if l == nil || c.notifying {
		return
	}
	c.notifying = true
	go func() {
		for {
			select {
			case <-l.Signal():
			case <-c.done:
				return
			}
			c.wmu.Lock()
			if !c.inBatch && c.sess.TxStatus() == 'I' {
				c.sendNotifications()
				c.w.Flush()
			}
			c.wmu.Unlock()
		}
	}()
}

// ---- main loop ----

func (c *conn) loop() error {
	for {
		c.armIdle()
		t, body, err := c.readMsg()
		if err != nil {
			if c.idleCode != "" && errors.Is(err, os.ErrDeadlineExceeded) {
				// as PostgreSQL: FATAL, then the session ends, which
				// aborts its transaction and releases its snapshot
				c.s.Logf("pgwire: connection %d: %s", c.pid, c.idleMsg)
				c.wmu.Lock()
				c.nc.SetWriteDeadline(time.Now().Add(time.Second))
				c.sendFatal(c.idleCode, c.idleMsg)
				c.w.Flush()
				c.wmu.Unlock()
				return nil
			}
			return err
		}
		c.wmu.Lock()
		stop, err := c.handleMsg(t, body)
		c.startNotifier()
		c.wmu.Unlock()
		if stop || err != nil {
			return err
		}
	}
}

// armIdle sets a read deadline for the session's idle timeout while it
// waits for the client after ReadyForQuery: idle_in_transaction_session_timeout
// inside a transaction block, idle_session_timeout outside one.
func (c *conn) armIdle() {
	var d time.Duration
	var code, msg string
	if !c.inBatch && c.r.Buffered() == 0 {
		if c.sess.TxStatus() != 'I' {
			d, code, msg = c.sess.IdleInTransactionTimeout(), "25P03", "terminating connection due to idle-in-transaction timeout"
		} else {
			d, code, msg = c.sess.IdleSessionTimeout(), "57P05", "terminating connection due to idle-session timeout"
		}
	}
	if d <= 0 {
		if c.idleCode != "" {
			c.nc.SetReadDeadline(time.Time{})
			c.idleCode = ""
		}
		return
	}
	c.nc.SetReadDeadline(time.Now().Add(d))
	c.idleCode, c.idleMsg = code, msg
}

// setKeepAlive applies the TCP keepalive settings to an accepted connection.
func (s *Server) setKeepAlive(tc *net.TCPConn) {
	osDefault := func(d time.Duration) time.Duration {
		if d <= 0 {
			return -1 // leave the system setting
		}
		return d
	}
	cfg := net.KeepAliveConfig{Enable: !s.NoKeepAlive, Idle: osDefault(s.KeepAliveIdle),
		Interval: osDefault(s.KeepAliveInterval), Count: s.KeepAliveCount}
	if cfg.Count <= 0 {
		cfg.Count = -1
	}
	if err := tc.SetKeepAliveConfig(cfg); err != nil {
		s.Logf("pgwire: keepalive: %v", err)
	}
	if s.TCPUserTimeout > 0 {
		if err := setUserTimeout(tc, s.TCPUserTimeout); err != nil {
			s.Logf("pgwire: tcp user timeout: %v", err)
		}
	}
}

// handleMsg handles one message (with wmu held); stop ends the connection.
func (c *conn) handleMsg(t byte, body []byte) (stop bool, err error) {
	if c.skip && t != 'S' && t != 'X' {
		return false, nil
	}
	switch t {
	case 'P', 'B', 'D', 'E', 'C', 'H':
		c.inBatch = true
	case 'S', 'Q':
		c.inBatch = false
	}
	switch t {
	case 'Q':
		r := &reader{b: body}
		if c.simpleQuery(r.str()) {
			return true, nil
		}
		c.ready()
		if err := c.w.Flush(); err != nil {
			return false, err
		}
	case 'P':
		c.parse(body)
	case 'B':
		c.bind(body)
	case 'D':
		c.describe(body)
	case 'E':
		if core.LatDebug {
			t := time.Now()
			c.execute(body)
			core.LatServer.Add(time.Since(t))
		} else {
			c.execute(body)
		}
	case 'C':
		r := &reader{b: body}
		kind, name := r.byte1(), r.str()
		if kind == 'S' {
			delete(c.stmts, name)
		} else {
			delete(c.portals, name)
		}
		c.send(newMsg('3'))
	case 'S':
		c.skip = false
		delete(c.portals, "")
		c.ready()
		if err := c.w.Flush(); err != nil {
			return false, err
		}
	case 'H':
		if err := c.w.Flush(); err != nil {
			return false, err
		}
	case 'X':
		return true, nil
	case 'F':
		c.fail(&sql.Error{Code: "0A000", Msg: "function call protocol is not supported"})
	case 'd', 'c', 'f':
		// COPY data outside COPY: ignore
	default:
		c.fail(&sql.Error{Code: "08P01", Msg: fmt.Sprintf("unexpected message %q", t)})
	}
	return false, nil
}

func (r *reader) byte1() byte {
	if len(r.b) < 1 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

// fail reports an error in the extended protocol and skips to Sync.
func (c *conn) fail(err error) {
	c.sendError(sql.AsError(err))
	c.skip = true
}

func (c *conn) sendError(e *sql.Error) {
	m := newMsg('E').byte1('S').str("ERROR").byte1('V').str("ERROR").byte1('C').str(e.Code).byte1('M').str(e.Msg)
	if e.Pos > 0 {
		m.byte1('P').str(strconv.Itoa(e.Pos))
	}
	c.send(m.byte1(0))
}

func (c *conn) notice(text string) {
	c.send(newMsg('N').byte1('S').str("NOTICE").byte1('V').str("NOTICE").byte1('C').str("00000").byte1('M').str(text).byte1(0))
}

func (c *conn) simpleQuery(q string) (stop bool) {
	if c.replication {
		if handled, stop := c.replCommand(q); handled {
			return stop
		}
	}
	if cs, ok, err := sql.ParseCopy(q); ok {
		return c.copyCommand(cs, err)
	}
	stmts, err := sql.Parse(q)
	if err != nil {
		c.sendError(sql.AsError(err))
		return false
	}
	if len(stmts) == 0 {
		c.send(newMsg('I'))
		return false
	}
	for _, st := range stmts {
		c.sess.SetQuery(q)
		res, err := c.sess.Exec(st, nil)
		if err != nil {
			c.sendError(sql.AsError(err))
			return false
		}
		if res.Notice != "" {
			c.notice(res.Notice)
		}
		if res.HasRows {
			c.rowDescription(res.Cols, nil)
		}
		for _, row := range res.Rows {
			if err := c.dataRow(row, nil, res.Cols); err != nil {
				c.sendError(sql.AsError(err))
				return false
			}
		}
		c.send(newMsg('C').str(res.Tag))
	}
	return false
}

func (c *conn) parse(body []byte) {
	r := &reader{b: body}
	name, q := r.str(), r.str()
	n := r.int16()
	oids := make([]uint32, max(n, 0))
	for i := range oids {
		oids[i] = uint32(r.int32())
	}
	if r.err != nil {
		c.fail(&sql.Error{Code: "08P01", Msg: "malformed Parse message"})
		return
	}
	st, err := sql.ParseOne(q)
	if err != nil {
		c.fail(err)
		return
	}
	ps := &prepared{query: q, stmt: st, params: oids}
	if st != nil {
		desc, err := c.sess.Describe(st)
		if err != nil {
			c.fail(err)
			return
		}
		ps.desc = desc
		// client-declared types win; unspecified ones come from inference
		for len(ps.params) < len(desc.Params) {
			ps.params = append(ps.params, 0)
		}
		for i := range ps.params {
			if ps.params[i] == 0 || ps.params[i] == oidUnknown {
				if i < len(desc.Params) {
					ps.params[i] = oidOf(desc.Params[i])
				} else {
					ps.params[i] = oidText
				}
			}
		}
	}
	c.stmts[name] = ps
	c.send(newMsg('1'))
}

func (c *conn) bind(body []byte) {
	r := &reader{b: body}
	pname, sname := r.str(), r.str()
	ps := c.stmts[sname]
	if ps == nil {
		c.fail(&sql.Error{Code: "26000", Msg: fmt.Sprintf("prepared statement %q does not exist", sname)})
		return
	}
	nf := r.int16()
	pformats := make([]int16, max(nf, 0))
	for i := range pformats {
		pformats[i] = int16(r.int16())
	}
	np := r.int16()
	if np != len(ps.params) {
		c.fail(&sql.Error{Code: "08P01", Msg: fmt.Sprintf("bind message supplies %d parameters, but prepared statement %q requires %d", np, sname, len(ps.params))})
		return
	}
	params := make([]sql.Datum, np)
	for i := range params {
		l := r.int32()
		if l == -1 {
			continue
		}
		raw := r.bytes(l)
		if r.err != nil {
			break
		}
		f := int16(0)
		switch {
		case len(pformats) == 1:
			f = pformats[0]
		case i < len(pformats):
			f = pformats[i]
		}
		d, err := decodeParam(ps.params[i], f, raw)
		if err != nil {
			c.fail(err)
			return
		}
		params[i] = d
	}
	nr := r.int16()
	rformats := make([]int16, max(nr, 0))
	for i := range rformats {
		rformats[i] = int16(r.int16())
	}
	if r.err != nil {
		c.fail(&sql.Error{Code: "08P01", Msg: "malformed Bind message"})
		return
	}
	c.portals[pname] = &portal{ps: ps, params: params, formats: rformats}
	c.send(newMsg('2'))
}

func (c *conn) describe(body []byte) {
	r := &reader{b: body}
	kind, name := r.byte1(), r.str()
	if kind == 'S' {
		ps := c.stmts[name]
		if ps == nil {
			c.fail(&sql.Error{Code: "26000", Msg: fmt.Sprintf("prepared statement %q does not exist", name)})
			return
		}
		m := newMsg('t').int16(len(ps.params))
		for _, o := range ps.params {
			m.int32(int(o))
		}
		c.send(m)
		if ps.desc != nil && ps.desc.HasRows {
			c.rowDescription(ps.desc.Cols, nil)
		} else {
			c.send(newMsg('n'))
		}
		return
	}
	p := c.portals[name]
	if p == nil {
		c.fail(&sql.Error{Code: "34000", Msg: fmt.Sprintf("portal %q does not exist", name)})
		return
	}
	if p.ps.desc != nil && p.ps.desc.HasRows {
		c.rowDescription(p.ps.desc.Cols, p.formats)
	} else {
		c.send(newMsg('n'))
	}
}

func (c *conn) execute(body []byte) {
	r := &reader{b: body}
	name := r.str()
	maxRows := r.int32()
	p := c.portals[name]
	if p == nil {
		c.fail(&sql.Error{Code: "34000", Msg: fmt.Sprintf("portal %q does not exist", name)})
		return
	}
	if p.ps.stmt == nil {
		c.send(newMsg('I'))
		return
	}
	if !p.started {
		var t0 time.Time
		if core.LatDebug {
			t0 = time.Now()
		}
		c.sess.SetQuery(p.ps.query)
		res, err := c.sess.Exec(p.ps.stmt, p.params)
		if core.LatDebug {
			if _, ok := p.ps.stmt.(*sql.Select); ok {
				core.LatRead.Add(time.Since(t0))
			}
		}
		if err != nil {
			c.fail(err)
			return
		}
		if res.Notice != "" {
			c.notice(res.Notice)
		}
		p.res, p.started = res, true
		// the described columns are authoritative for the client
		if p.ps.desc != nil && len(p.ps.desc.Cols) == len(res.Cols) {
			for _, row := range res.Rows {
				conformTo(row, p.ps.desc.Cols)
			}
		}
	}
	rows := p.res.Rows[p.sent:]
	if maxRows > 0 && len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	for _, row := range rows {
		if err := c.dataRow(row, p.formats, dataCols(p)); err != nil {
			c.fail(err)
			return
		}
	}
	p.sent += len(rows)
	if p.sent < len(p.res.Rows) {
		c.send(newMsg('s')) // PortalSuspended
		return
	}
	c.send(newMsg('C').str(p.res.Tag))
}

func conformTo(row []sql.Datum, cols []sql.Column) {
	for i := range row {
		if !row[i].IsNull() && row[i].K != cols[i].Kind {
			if v, err := sql.Coerce(row[i], cols[i].Kind); err == nil {
				row[i] = v
			}
		}
	}
}

// ---- values ----

func format(formats []int16, i int) int16 {
	switch {
	case len(formats) == 1:
		return formats[0]
	case i < len(formats):
		return formats[i]
	}
	return 0
}

// colOID is the type OID RowDescription announces for a result column.
func colOID(col sql.Column) uint32 {
	if col.OID != 0 {
		return col.OID
	}
	return oidOf(col.Kind)
}

func (c *conn) rowDescription(cols []sql.Column, formats []int16) {
	m := newMsg('T').int16(len(cols))
	for i, col := range cols {
		size := -1
		switch col.OID {
		case oidInt4, oidFloat4:
			size = 4
		case oidInt2:
			size = 2
		case oidTime:
			size = 8
		case oidName:
			size = 64
		}
		if size < 0 {
			switch col.Kind {
			case sql.KInt, sql.KFloat, sql.KTime, sql.KTimestamp:
				size = 8
			case sql.KBool:
				size = 1
			case sql.KUUID:
				size = 16
			case sql.KDate:
				size = 4
			case sql.KInterval:
				size = 16
			}
		}
		mod := int(col.Mod)
		if col.OID == 0 {
			mod = -1
		}
		m.str(col.Name).int32(0).int16(0).int32(int(colOID(col))).int16(size).int32(mod).int16(int(format(formats, i)))
	}
	c.send(m)
}

func (c *conn) dataRow(row []sql.Datum, formats []int16, cols []sql.Column) error {
	m := newMsg('D').int16(len(row))
	for i, d := range row {
		if d.IsNull() {
			m.int32(-1)
			continue
		}
		if format(formats, i) == 1 {
			var b []byte
			switch {
			case d.K == sql.KArray:
				b = textArrayBinary(d.S)
			default:
				oid := uint32(0)
				if i < len(cols) {
					oid = cols[i].OID
				}
				if v, ok := sql.BinaryValueFor(oid, d); ok {
					b = v
				} else {
					b = []byte(d.S)
				}
			}
			m.int32(len(b)).bytes(b)
			continue
		}
		s := d.String()
		if i < len(cols) && cols[i].OID == oidFloat4 && d.K == sql.KFloat {
			s = sql.FormatFloat4(d.F)
		}
		m.int32(len(s)).bytes([]byte(s))
	}
	return c.send(m)
}

func decodeParam(oid uint32, f int16, raw []byte) (sql.Datum, error) {
	k, known := kindOfOID(oid)
	if f == 0 {
		d := sql.TextD(string(raw))
		if !known || k == sql.KText {
			return d, nil
		}
		return sql.Coerce(d, k)
	}
	switch oid {
	case oidInt8:
		if len(raw) == 8 {
			return sql.IntD(int64(binary.BigEndian.Uint64(raw))), nil
		}
	case oidInt4:
		if len(raw) == 4 {
			return sql.IntD(int64(int32(binary.BigEndian.Uint32(raw)))), nil
		}
	case oidInt2:
		if len(raw) == 2 {
			return sql.IntD(int64(int16(binary.BigEndian.Uint16(raw)))), nil
		}
	case oidFloat8:
		if len(raw) == 8 {
			return sql.FloatD(math.Float64frombits(binary.BigEndian.Uint64(raw))), nil
		}
	case oidFloat4:
		if len(raw) == 4 {
			return sql.FloatD(float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))), nil
		}
	case oidBool:
		if len(raw) == 1 {
			return sql.BoolD(raw[0] != 0), nil
		}
	case oidTimestamp, oidTimestamptz, oidDate, oidNumeric, oidUUID, oidBytea, oidJSON, oidJSONB:
		d, err := sql.ValueFromBinary(k, raw)
		if err != nil {
			return sql.Null, &sql.Error{Code: "22P03", Msg: fmt.Sprintf("incorrect binary data format for type oid %d", oid)}
		}
		return d, nil
	case oidText, oidVarchar, oidUnknown, oidName, oidBpchar:
		return sql.TextD(string(raw)), nil
	default:
		return sql.Null, &sql.Error{Code: "0A000", Msg: fmt.Sprintf("binary format for type oid %d is not supported", oid)}
	}
	return sql.Null, &sql.Error{Code: "22P03", Msg: fmt.Sprintf("incorrect binary data format for type oid %d", oid)}
}

// textArrayBinary encodes a one-dimensional array given in PostgreSQL text
// form ({a,"b c",NULL}) as a binary text[].
func textArrayBinary(s string) []byte {
	var elems []*string
	if len(s) >= 2 {
		body := s[1 : len(s)-1]
		for i := 0; i < len(body); {
			var e []byte
			quoted := false
			if body[i] == '"' {
				quoted = true
				i++
				for i < len(body) && body[i] != '"' {
					if body[i] == '\\' && i+1 < len(body) {
						i++
					}
					e = append(e, body[i])
					i++
				}
				i++ // closing quote
			} else {
				for i < len(body) && body[i] != ',' {
					e = append(e, body[i])
					i++
				}
			}
			if !quoted && string(e) == "NULL" {
				elems = append(elems, nil)
			} else {
				v := string(e)
				elems = append(elems, &v)
			}
			i++ // comma
		}
	}
	b := binary.BigEndian.AppendUint32(nil, 1) // dimensions
	hasNull := uint32(0)
	for _, e := range elems {
		if e == nil {
			hasNull = 1
		}
	}
	if len(elems) == 0 {
		b = binary.BigEndian.AppendUint32(nil, 0)
		b = binary.BigEndian.AppendUint32(b, 0)
		return binary.BigEndian.AppendUint32(b, oidText)
	}
	b = binary.BigEndian.AppendUint32(b, hasNull)
	b = binary.BigEndian.AppendUint32(b, oidText)
	b = binary.BigEndian.AppendUint32(b, uint32(len(elems)))
	b = binary.BigEndian.AppendUint32(b, 1) // lower bound
	for _, e := range elems {
		if e == nil {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(*e)))
		b = append(b, *e...)
	}
	return b
}
