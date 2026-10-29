package repl

// Securing the replication port. Without it anyone who reaches -repl-addr
// streams the whole database, and a replica follows whatever answers at
// -replica-of.
//
// Two independent layers, as PostgreSQL does for replication connections
// (hostssl + scram-sha-256 in pg_hba.conf, sslmode=verify-full in
// primary_conninfo) and Redis does for replicas (TLS + masterauth):
//
//   - TLS. The leader sniffs the first byte of a connection: 0x16 starts a
//     TLS handshake, anything else is the plaintext protocol (refused when
//     Require is set). A replica given a CA always dials TLS and checks the
//     leader's certificate against it and the leader's host name
//     (verify-full). With ClientAuth the leader also requires a client
//     certificate signed by the same CA (mTLS, like etcd's
//     --peer-client-cert-auth).
//   - A shared secret, proved by challenge-response so that it never
//     crosses the wire, even without TLS (as MongoDB's keyfile internal
//     authentication proves the key with SCRAM). Both sides prove it: the
//     replica so that the leader streams only to its replicas, the leader
//     so that a replica only applies log from its leader:
//
//       leader  -> replica  fChallenge: u8 flags (1 = secret required), nonce Ns
//       replica -> leader   fAuth: nonce Nr, HMAC(secret, "replica" Ns Nr B)
//       leader  -> replica  fAuthOK: HMAC(secret, "leader" Ns Nr B)
//
//     B is the TLS exporter of the connection (RFC 5705/9266 channel
//     binding), empty without TLS: a proof made on one TLS connection does
//     not verify on another, so a man in the middle cannot relay it.
//     Proofs are compared in constant time (hmac.Equal).
//
// A connection is authenticated when it proved the secret or presented a
// verified client certificate. A leader with either configured refuses the
// others.

import (
	"bufio"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const (
	fChallenge = 'N' // leader: u8 flags, nonce
	fAuth      = 'U' // replica: nonce, mac (empty without a secret)
	fAuthOK    = 'O' // leader: mac (empty without a secret)

	nonceLen       = 32
	exporterLabel  = "EXPORTER-stask-repl"
	handshakeLimit = 10 * time.Second // TLS, authentication and hello
	preAuthFrame   = 4 << 10          // largest frame read before authentication
)

// Security configures authentication and encryption of replication
// connections. The zero value (or nil) is the unsecured protocol.
type Security struct {
	// Secret shared by the leader and its replicas (empty = none).
	Secret []byte
	// TLS: on the leader, the server configuration (ClientAuth set to
	// tls.RequireAndVerifyClientCert for mTLS); on a replica, the client
	// configuration (RootCAs and ServerName to verify the leader, and a
	// client certificate for mTLS).
	TLS *tls.Config
	// Require refuses, on the leader, replicas that are not both encrypted
	// and authenticated; on a replica, a leader reached without TLS.
	Require bool
}

func (s *Security) secret() []byte {
	if s == nil {
		return nil
	}
	return s.Secret
}

func (s *Security) tls() *tls.Config {
	if s == nil {
		return nil
	}
	return s.TLS
}

// clientCerts reports whether the leader requires verified client
// certificates.
func (s *Security) clientCerts() bool {
	return s.tls() != nil && s.TLS.ClientAuth == tls.RequireAndVerifyClientCert
}

// Authenticates reports whether the leader authenticates replicas.
func (s *Security) Authenticates() bool { return len(s.secret()) > 0 || s.clientCerts() }

func (s *Security) checkLeader() error {
	if s == nil || !s.Require {
		return nil
	}
	if s.TLS == nil || len(s.TLS.Certificates) == 0 && s.TLS.GetCertificate == nil {
		return errors.New("replication: requiring secure replicas needs a TLS certificate")
	}
	if !s.Authenticates() {
		return errors.New("replication: requiring secure replicas needs a secret or client certificates")
	}
	return nil
}

func (s *Security) checkReplica() error {
	if s != nil && s.Require && s.TLS == nil {
		return errors.New("replication: requiring a secure leader needs a CA to verify it")
	}
	return nil
}

// Settings are the file-based settings behind a Security (staskd flags).
type Settings struct {
	Secret     string // the secret itself
	SecretFile string // or a file holding it (trailing white space ignored)
	CertFile   string // PEM certificate: the leader's server certificate, a replica's client certificate
	KeyFile    string
	CAFile     string // PEM CA: verifies the leader on a replica, client certificates on the leader
	ServerName string // name the leader's certificate must carry (default: host of the leader address)
	ClientAuth bool   // leader: require client certificates signed by CAFile
	Require    bool
}

func (st Settings) common() (*Security, error) {
	sec := &Security{Require: st.Require}
	switch {
	case st.Secret != "" && st.SecretFile != "":
		return nil, errors.New("replication: give the secret or a secret file, not both")
	case st.SecretFile != "":
		b, err := os.ReadFile(st.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("replication secret: %w", err)
		}
		sec.Secret = []byte(strings.TrimRight(string(b), " \t\r\n"))
		if len(sec.Secret) == 0 {
			return nil, fmt.Errorf("replication secret: %s is empty", st.SecretFile)
		}
	default:
		sec.Secret = []byte(st.Secret)
	}
	if (st.CertFile == "") != (st.KeyFile == "") {
		return nil, errors.New("replication TLS: certificate and key go together")
	}
	return sec, nil
}

func loadCA(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replication CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("replication CA: no certificate in %s", path)
	}
	return pool, nil
}

// Leader returns the security of a leader's replication port.
func (st Settings) Leader() (*Security, error) {
	sec, err := st.common()
	if err != nil {
		return nil, err
	}
	if st.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(st.CertFile, st.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("replication TLS: %w", err)
		}
		sec.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	if st.ClientAuth {
		if sec.TLS == nil || st.CAFile == "" {
			return nil, errors.New("replication TLS: client certificates need a certificate, a key and a CA")
		}
		if sec.TLS.ClientCAs, err = loadCA(st.CAFile); err != nil {
			return nil, err
		}
		sec.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return sec, sec.checkLeader()
}

// Replica returns the security of a replica of the leader at leaderAddr.
func (st Settings) Replica(leaderAddr string) (*Security, error) {
	sec, err := st.common()
	if err != nil {
		return nil, err
	}
	if st.CAFile != "" {
		pool, err := loadCA(st.CAFile)
		if err != nil {
			return nil, err
		}
		name := st.ServerName
		if name == "" {
			if name, _, err = net.SplitHostPort(leaderAddr); err != nil {
				return nil, fmt.Errorf("replication TLS: leader address: %w", err)
			}
		}
		sec.TLS = &tls.Config{RootCAs: pool, ServerName: name, MinVersion: tls.VersionTLS12}
		if st.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(st.CertFile, st.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("replication TLS: %w", err)
			}
			sec.TLS.Certificates = []tls.Certificate{cert}
		}
	} else if st.CertFile != "" {
		return nil, errors.New("replication TLS: a replica's client certificate needs a CA to verify the leader")
	}
	return sec, sec.checkReplica()
}

// CheckListen refuses to serve replicas on an address other hosts can
// reach while they are not authenticated, unless insecure is set.
func CheckListen(addr string, sec *Security, insecure bool) error {
	if insecure || sec.Authenticates() || isLoopback(addr) {
		return nil
	}
	return fmt.Errorf("replication port %s is reachable from other hosts but replicas are not authenticated: "+
		"set -repl-secret (or -repl-secret-file) or -repl-tls-client-auth, bind a loopback address, or pass -repl-insecure", addr)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bufConn reads through r, which may already hold bytes of the connection.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func mac(secret []byte, role string, ns, nr, binding []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("stask-repl " + role))
	h.Write(ns)
	h.Write(nr)
	h.Write(binding)
	return h.Sum(nil)
}

// binding is the TLS exporter of c, nil for a plaintext connection.
func binding(c net.Conn) ([]byte, error) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return nil, nil
	}
	cs := tc.ConnectionState()
	return cs.ExportKeyingMaterial(exporterLabel, nil, 32)
}

// accept runs the leader's side of the connection setup up to the hello:
// TLS if the replica starts it, then authentication. It returns the
// connection to use (TLS or not) and its reader.
func (s *Server) accept(c net.Conn) (net.Conn, *bufio.Reader, error) {
	sec := s.sec
	r := bufio.NewReaderSize(c, 64<<10)
	first, err := r.Peek(1)
	if err != nil {
		return nil, nil, err
	}
	encrypted := first[0] == 0x16 // TLS handshake record
	if encrypted {
		if sec.tls() == nil {
			return nil, nil, errors.New("TLS requested, but the leader has no certificate (-repl-tls-cert)")
		}
		tc := tls.Server(&bufConn{c, r}, sec.TLS)
		if err := tc.Handshake(); err != nil {
			return nil, nil, fmt.Errorf("TLS: %w", err)
		}
		c, r = tc, bufio.NewReaderSize(tc, 64<<10)
	}
	m := make([]byte, len(magic))
	if _, err := io.ReadFull(r, m); err != nil || string(m) != magic {
		return nil, nil, errors.New("not a replica")
	}
	refuse := func(msg string) error {
		writeFrame(c, fError, []byte(msg))
		return errors.New(msg)
	}
	if !encrypted && (sec != nil && sec.Require || sec.clientCerts() && len(sec.secret()) == 0) {
		return nil, nil, refuse("the leader requires TLS (give the replica -repl-tls-ca)")
	}
	ns := make([]byte, nonceLen)
	crand.Read(ns)
	flags := byte(0)
	if len(sec.secret()) > 0 {
		flags = 1
	}
	if err := writeFrame(c, fChallenge, []byte{flags}, ns); err != nil {
		return nil, nil, err
	}
	typ, b, err := readFrameMax(r, preAuthFrame)
	if err != nil {
		return nil, nil, err
	}
	if typ != fAuth {
		return nil, nil, refuse("expected authentication")
	}
	var proof []byte
	if flags == 1 {
		if len(b) != nonceLen+sha256.Size {
			return nil, nil, refuse("authentication required (the replica has no -repl-secret)")
		}
		nr, got := b[:nonceLen], b[nonceLen:]
		cb, err := binding(c)
		if err != nil {
			return nil, nil, err
		}
		if !hmac.Equal(got, mac(sec.Secret, "replica", ns, nr, cb)) {
			return nil, nil, refuse("authentication failed")
		}
		proof = mac(sec.Secret, "leader", ns, nr, cb)
	}
	if err := writeFrame(c, fAuthOK, proof); err != nil {
		return nil, nil, err
	}
	return c, r, nil
}

// dial connects to the leader and runs the replica's side of the
// connection setup up to the hello.
func dial(leader string, sec *Security) (net.Conn, *bufio.Reader, error) {
	c, err := net.DialTimeout("tcp", leader, 5*time.Second)
	if err != nil {
		return nil, nil, err
	}
	c.SetDeadline(time.Now().Add(handshakeLimit))
	conn, r, err := dialSetup(c, leader, sec)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	c.SetDeadline(time.Time{})
	return conn, r, nil
}

func dialSetup(c net.Conn, leader string, sec *Security) (net.Conn, *bufio.Reader, error) {
	if err := sec.checkReplica(); err != nil {
		return nil, nil, err
	}
	if cfg := sec.tls(); cfg != nil {
		tc := tls.Client(c, cfg)
		if err := tc.Handshake(); err != nil {
			return nil, nil, fmt.Errorf("TLS to leader %s: %w", leader, err)
		}
		c = tc
	}
	r := bufio.NewReaderSize(c, 1<<20)
	if _, err := c.Write([]byte(magic)); err != nil {
		return nil, nil, err
	}
	typ, b, err := readFrameMax(r, preAuthFrame)
	if err != nil {
		return nil, nil, err
	}
	if typ == fError {
		return nil, nil, fmt.Errorf("leader: %s", b)
	}
	if typ != fChallenge || len(b) != 1+nonceLen {
		return nil, nil, fmt.Errorf("unexpected frame %q (is %s a STASK leader of this version?)", typ, leader)
	}
	required, ns := b[0]&1 == 1, b[1:]
	secret := sec.secret()
	if len(secret) > 0 && !required {
		// it could not prove the secret: anyone may be answering
		return nil, nil, fmt.Errorf("leader %s does not authenticate (no -repl-secret on the leader)", leader)
	}
	var nr, cb []byte
	var auth []byte
	if len(secret) > 0 {
		nr = make([]byte, nonceLen)
		crand.Read(nr)
		if cb, err = binding(c); err != nil {
			return nil, nil, err
		}
		auth = append(nr, mac(secret, "replica", ns, nr, cb)...)
	}
	if err := writeFrame(c, fAuth, auth); err != nil {
		return nil, nil, err
	}
	typ, b, err = readFrameMax(r, preAuthFrame)
	if err != nil {
		return nil, nil, err
	}
	if typ == fError {
		return nil, nil, fmt.Errorf("leader: %s", b)
	}
	if typ != fAuthOK {
		return nil, nil, fmt.Errorf("unexpected frame %q", typ)
	}
	if len(secret) > 0 && !hmac.Equal(b, mac(secret, "leader", ns, nr, cb)) {
		return nil, nil, fmt.Errorf("leader %s failed to prove the secret", leader)
	}
	return c, r, nil
}
