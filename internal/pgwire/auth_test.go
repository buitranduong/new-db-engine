package pgwire

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"stask/internal/core"
)

// selfSigned returns a TLS configuration with a fresh ECDSA P-256
// certificate for 127.0.0.1 (signed with SHA-256).
func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "stask-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// startAuthServer starts a server configured by setup and returns its
// address.
func startAuthServer(t *testing.T, setup func(*Server)) string {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{E: e, Logf: t.Logf}
	setup(s)
	if err := s.prepareAuth(); err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close(); e.Close() })
	return ln.Addr().String()
}

func selectOne(t *testing.T, url string) (*tls.ConnectionState, error) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var n int64
	if err := conn.QueryRow(ctx, `SELECT 1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("select 1: %d %v", n, err)
	}
	if tc, ok := conn.PgConn().Conn().(*tls.Conn); ok {
		cs := tc.ConnectionState()
		return &cs, nil
	}
	return nil, nil
}

func TestTLSRequire(t *testing.T) {
	addr := startAuthServer(t, func(s *Server) { s.TLS = selfSigned(t); s.TLSRequire = true })
	cs, err := selectOne(t, "postgres://app@"+addr+"/stask?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	if cs == nil || cs.Version < tls.VersionTLS12 {
		t.Fatalf("not TLS 1.2+: %+v", cs)
	}
	if _, err := selectOne(t, "postgres://app@"+addr+"/stask?sslmode=disable"); sqlState(err) != "28000" {
		t.Fatalf("want 28000 without TLS, got %v", err)
	}
}

func TestTLSOptional(t *testing.T) {
	addr := startAuthServer(t, func(s *Server) { s.TLS = selfSigned(t) })
	if cs, err := selectOne(t, "postgres://app@"+addr+"/stask?sslmode=prefer"); err != nil || cs == nil {
		t.Fatalf("prefer: %v %v", cs, err)
	}
	if cs, err := selectOne(t, "postgres://app@"+addr+"/stask?sslmode=disable"); err != nil || cs != nil {
		t.Fatalf("disable: %v %v", cs, err)
	}
	// without a certificate SSLRequest is refused and sslmode=require fails
	plain := startAuthServer(t, func(s *Server) {})
	if _, err := selectOne(t, "postgres://app@"+plain+"/stask?sslmode=require"); err == nil {
		t.Fatal("sslmode=require succeeded without a certificate")
	}
}

func TestSCRAM(t *testing.T) {
	addr := startAuthServer(t, func(s *Server) { s.Auth = AuthSCRAM; s.Password = "bí-mật s3cret"; s.TLS = selfSigned(t) })
	for _, mode := range []string{"disable", "require"} {
		if _, err := selectOne(t, "postgres://app:"+"b%C3%AD-m%E1%BA%ADt%20s3cret"+"@"+addr+"/stask?sslmode="+mode); err != nil {
			t.Fatalf("sslmode=%s: %v", mode, err)
		}
		_, err := selectOne(t, "postgres://app:wrong@"+addr+"/stask?sslmode="+mode)
		if sqlState(err) != "28P01" {
			t.Fatalf("sslmode=%s: want 28P01, got %v", mode, err)
		}
	}
}

func TestSCRAMReplication(t *testing.T) {
	addr := startAuthServer(t, func(s *Server) { s.Auth = AuthSCRAM; s.Password = "repl" })
	ctx := context.Background()
	for _, pw := range []string{"repl", "nope"} {
		cfg, err := pgconn.ParseConfig("postgres://app:" + pw + "@" + addr + "/stask?sslmode=disable&replication=database")
		if err != nil {
			t.Fatal(err)
		}
		pc, err := pgconn.ConnectConfig(ctx, cfg)
		if pw == "nope" {
			if sqlState(err) != "28P01" {
				t.Fatalf("want 28P01, got %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		res, err := pc.Exec(ctx, "IDENTIFY_SYSTEM").ReadAll()
		pc.Close(ctx)
		if err != nil || len(res) != 1 || len(res[0].Rows) != 1 {
			t.Fatalf("IDENTIFY_SYSTEM: %v %v", res, err)
		}
	}
}

// rawClient speaks just enough of the protocol to test what pgx v5.7
// cannot: SCRAM-SHA-256-PLUS and direct SSL.
type rawClient struct {
	t  *testing.T
	nc net.Conn
	r  *bufio.Reader
}

func (c *rawClient) write(b []byte) {
	if _, err := c.nc.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *rawClient) startup(user string) {
	b := []byte{0, 0, 0, 0, 0, 3, 0, 0}
	b = append(append(append(b, "user\x00"...), user...), 0)
	b = append(b, "database\x00stask\x00\x00"...)
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	c.write(b)
}

func (c *rawClient) msg(t byte, body []byte) {
	b := binary.BigEndian.AppendUint32([]byte{t}, uint32(len(body)+4))
	c.write(append(b, body...))
}

// read returns the next message; an ErrorResponse yields its SQLSTATE.
func (c *rawClient) read() (byte, []byte, string) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		c.t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint32(hdr[1:])-4)
	if _, err := io.ReadFull(c.r, body); err != nil {
		c.t.Fatal(err)
	}
	if hdr[0] == 'E' {
		for _, f := range strings.Split(string(body), "\x00") {
			if strings.HasPrefix(f, "C") {
				return 'E', body, f[1:]
			}
		}
	}
	return hdr[0], body, ""
}

// auth reads an Authentication message and returns its code and data.
func (c *rawClient) auth() (int, []byte, string) {
	t, body, code := c.read()
	if t != 'R' {
		return -1, body, code
	}
	return int(binary.BigEndian.Uint32(body)), body[4:], ""
}

func dialTLS(t *testing.T, addr string) *rawClient {
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	nc.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}) // SSLRequest
	var b [1]byte
	if _, err := io.ReadFull(nc, b[:]); err != nil || b[0] != 'S' {
		t.Fatalf("SSLRequest: %q %v", b, err)
	}
	tc := tls.Client(nc, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	return &rawClient{t: t, nc: tc, r: bufio.NewReader(tc)}
}

func hmacSum(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// scramPlus runs SCRAM-SHA-256-PLUS with tls-server-end-point binding and
// returns the SQLSTATE of the failure, or "" with AuthenticationOk.
func scramPlus(t *testing.T, addr, password string) string {
	c := dialTLS(t, addr)
	c.startup("app")
	code, data, state := c.auth()
	if code != 10 {
		t.Fatalf("want AuthenticationSASL, got %d %s", code, state)
	}
	if mechs := strings.Split(string(data), "\x00"); mechs[0] != scramSHA256Plus || mechs[1] != scramSHA256 {
		t.Fatalf("mechanisms %q", mechs)
	}
	gs2 := "p=tls-server-end-point,,"
	bare := "n=,r=clientnonce123"
	first := gs2 + bare
	body := append([]byte(scramSHA256Plus+"\x00"), binary.BigEndian.AppendUint32(nil, uint32(len(first)))...)
	c.msg('p', append(body, first...))
	code, data, state = c.auth()
	if code != 11 {
		t.Fatalf("want AuthenticationSASLContinue, got %d %s", code, state)
	}
	serverFirst := string(data)
	var nonce, salt string
	iters := 0
	for _, a := range strings.Split(serverFirst, ",") {
		switch a[:2] {
		case "r=":
			nonce = a[2:]
		case "s=":
			salt = a[2:]
		case "i=":
			for _, d := range a[2:] {
				iters = iters*10 + int(d-'0')
			}
		}
	}
	if !strings.HasPrefix(nonce, "clientnonce123") || iters != 4096 {
		t.Fatalf("server-first %q", serverFirst)
	}
	cert := c.nc.(*tls.Conn).ConnectionState().PeerCertificates[0].Raw
	h := sha256.Sum256(cert)
	cbind := base64.StdEncoding.EncodeToString(append([]byte(gs2), h[:]...))
	withoutProof := "c=" + cbind + ",r=" + nonce
	saltB, _ := base64.StdEncoding.DecodeString(salt)
	salted, err := pbkdf2.Key(sha256.New, password, saltB, iters, 32)
	if err != nil {
		t.Fatal(err)
	}
	ck := hmacSum(salted, "Client Key")
	sk := sha256.Sum256(ck)
	authMsg := bare + "," + serverFirst + "," + withoutProof
	sig := hmacSum(sk[:], authMsg)
	proof := make([]byte, len(ck))
	for i := range ck {
		proof[i] = ck[i] ^ sig[i]
	}
	c.msg('p', []byte(withoutProof+",p="+base64.StdEncoding.EncodeToString(proof)))
	code, data, state = c.auth()
	if code != 12 {
		return state
	}
	want := "v=" + base64.StdEncoding.EncodeToString(hmacSum(hmacSum(salted, "Server Key"), authMsg))
	if string(data) != want {
		t.Fatalf("server signature %q, want %q", data, want)
	}
	if code, _, state := c.auth(); code != 0 {
		t.Fatalf("want AuthenticationOk, got %d %s", code, state)
	}
	return ""
}

func TestSCRAMPlus(t *testing.T) {
	addr := startAuthServer(t, func(s *Server) { s.Auth = AuthSCRAM; s.Password = "plus"; s.TLS = selfSigned(t) })
	if state := scramPlus(t, addr, "plus"); state != "" {
		t.Fatalf("SCRAM-SHA-256-PLUS failed: %s", state)
	}
	if state := scramPlus(t, addr, "wrong"); state != "28P01" {
		t.Fatalf("want 28P01, got %q", state)
	}
	// "y": the client claims the server cannot bind, but -PLUS was offered
	c := dialTLS(t, addr)
	c.startup("app")
	c.auth()
	first := "y,,n=,r=abc"
	body := append([]byte(scramSHA256+"\x00"), binary.BigEndian.AppendUint32(nil, uint32(len(first)))...)
	c.msg('p', append(body, first...))
	if _, _, state := c.read(); state != "08P01" {
		t.Fatalf("downgrade: want 08P01, got %q", state)
	}
}

func TestDirectSSL(t *testing.T) {
	addr := startAuthServer(t, func(s *Server) { s.TLS = selfSigned(t); s.TLSRequire = true })
	for _, alpn := range [][]string{{"postgresql"}, nil} {
		tc, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: alpn})
		if err != nil {
			t.Fatal(err)
		}
		c := &rawClient{t: t, nc: tc, r: bufio.NewReader(tc)}
		if alpn == nil {
			// the server refuses after the handshake, before the startup
			if _, _, state := c.read(); state != "08P01" {
				t.Fatalf("without ALPN: want 08P01, got %q", state)
			}
			tc.Close()
			continue
		}
		c.startup("app")
		if code, _, state := c.auth(); code != 0 {
			t.Fatalf("direct SSL: want AuthenticationOk, got %d %s", code, state)
		}
		tc.Close()
	}
	// a client offering another ALPN protocol fails the handshake
	if tc, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}); err == nil {
		if _, err := tc.Read(make([]byte, 1)); err == nil {
			t.Fatal("handshake with ALPN http/1.1 succeeded")
		}
		tc.Close()
	}
}
