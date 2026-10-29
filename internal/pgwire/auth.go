package pgwire

// Transport security and authentication: TLS (SSLRequest and PostgreSQL
// 17's direct SSL negotiation) and the password methods trust, cleartext
// password and SCRAM-SHA-256[-PLUS], as PostgreSQL implements them:
//
//	https://www.postgresql.org/docs/16/protocol-flow.html
//	https://www.postgresql.org/docs/16/sasl-authentication.html
//	RFC 5802 (SCRAM), RFC 7677 (SCRAM-SHA-256), RFC 5929 (tls-server-end-point)

import (
	"bufio"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"net"
	"strconv"
	"strings"
	"time"
)

// Authentication methods (Server.Auth).
const (
	AuthTrust    = "trust"
	AuthPassword = "password" // cleartext password message
	AuthSCRAM    = "scram"    // SCRAM-SHA-256, -PLUS over TLS
)

const (
	scramIterations     = 4096 // PostgreSQL's default scram_iterations
	scramSaltLen        = 16
	scramNonceLen       = 18
	tlsHandshakeTimeout = 10 * time.Second
	alpnPostgres        = "postgresql"
)

// scramVerifier is what PostgreSQL stores in pg_authid for a SCRAM
// password: the password itself is not needed to check a proof.
type scramVerifier struct {
	salt      []byte
	iters     int
	storedKey []byte
	serverKey []byte
}

func newSCRAMVerifier(password string) (*scramVerifier, error) {
	salt := make([]byte, scramSaltLen)
	rand.Read(salt)
	// PostgreSQL applies SASLprep and falls back to the raw bytes when that
	// fails; for ASCII passwords without control characters SASLprep is the
	// identity, so the raw bytes are what PostgreSQL would hash as well.
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return nil, err
	}
	ck := hmacSHA256(salted, "Client Key")
	sk := sha256.Sum256(ck)
	return &scramVerifier{salt: salt, iters: scramIterations, storedKey: sk[:], serverKey: hmacSHA256(salted, "Server Key")}, nil
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// prepareAuth checks the authentication settings and derives what the
// handshakes need; Serve calls it before accepting.
func (s *Server) prepareAuth() error {
	switch s.Auth {
	case "":
		s.Auth = AuthTrust
		if s.Password != "" {
			s.Auth = AuthPassword
		}
	case AuthTrust:
	case AuthPassword, AuthSCRAM:
		if s.Password == "" {
			return fmt.Errorf("pgwire: auth %q needs a password", s.Auth)
		}
	default:
		return fmt.Errorf("pgwire: unknown auth method %q (trust, password, scram)", s.Auth)
	}
	if s.Auth == AuthSCRAM {
		v, err := newSCRAMVerifier(s.Password)
		if err != nil {
			return err
		}
		s.scram = v
	}
	if s.TLSRequire && s.TLS == nil {
		return errors.New("pgwire: TLS required but no certificate configured")
	}
	if s.TLS != nil {
		cfg := s.TLS.Clone()
		if cfg.MinVersion < tls.VersionTLS12 {
			cfg.MinVersion = tls.VersionTLS12
		}
		// A client that offers ALPN must offer "postgresql"; direct SSL
		// connections must offer it (checked after the handshake).
		cfg.NextProtos = []string{alpnPostgres}
		s.TLS = cfg
		s.cbHash = endPointHash(cfg)
	}
	return nil
}

// endPointHash returns the hash function of tls-server-end-point channel
// binding (RFC 5929 §4.1) for the server certificate, or nil when it cannot
// be known: several certificates or GetCertificate, or a signature
// algorithm without a hash (Ed25519). SCRAM-SHA-256-PLUS is then not
// offered.
func endPointHash(cfg *tls.Config) func() hash.Hash {
	if len(cfg.Certificates) != 1 || cfg.GetCertificate != nil || len(cfg.Certificates[0].Certificate) == 0 {
		return nil
	}
	leaf := cfg.Certificates[0].Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(cfg.Certificates[0].Certificate[0]); err != nil {
			return nil
		}
	}
	switch leaf.SignatureAlgorithm {
	case x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1, x509.DSAWithSHA1,
		x509.SHA256WithRSA, x509.SHA256WithRSAPSS, x509.ECDSAWithSHA256, x509.DSAWithSHA256:
		return sha256.New // MD5 and SHA-1 are replaced by SHA-256
	case x509.SHA384WithRSA, x509.SHA384WithRSAPSS, x509.ECDSAWithSHA384:
		return sha512.New384
	case x509.SHA512WithRSA, x509.SHA512WithRSAPSS, x509.ECDSAWithSHA512:
		return sha512.New
	}
	return nil
}

// LoadTLS builds a server TLS configuration from PEM certificate and key
// files.
func LoadTLS(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// prefixConn reads through the startup reader, so bytes it has already
// buffered (the ClientHello of a direct SSL connection) reach TLS.
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *prefixConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// startTLS runs the server side of the TLS handshake on the connection and
// replaces its reader and writer. direct: the client began with a
// ClientHello instead of an SSLRequest.
func (c *conn) startTLS(direct bool) error {
	var raw net.Conn = c.nc
	if direct {
		raw = &prefixConn{Conn: c.nc, r: c.r}
	}
	tc := tls.Server(raw, c.s.TLS)
	c.nc.SetDeadline(time.Now().Add(tlsHandshakeTimeout))
	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	c.nc.SetDeadline(time.Time{})
	c.nc = tc
	c.r = bufio.NewReaderSize(tc, 8192)
	c.w.Reset(tc)
	c.tls = true
	if direct && tc.ConnectionState().NegotiatedProtocol != alpnPostgres {
		c.sendFatal("08P01", "received direct SSL connection request without ALPN protocol negotiation extension")
		c.w.Flush()
		return errors.New("direct SSL without ALPN")
	}
	return nil
}

// sendFatal sends an error that ends the session, as PostgreSQL does for
// startup and authentication failures.
func (c *conn) sendFatal(code, text string) {
	c.send(newMsg('E').byte1('S').str("FATAL").byte1('V').str("FATAL").byte1('C').str(code).byte1('M').str(text).byte1(0))
}

// authenticate runs the configured authentication exchange; on failure the
// error has been sent to the client.
func (c *conn) authenticate(user string) error {
	switch c.s.Auth {
	case AuthPassword:
		if err := c.send(newMsg('R').int32(3)); err != nil { // cleartext password
			return err
		}
		c.w.Flush()
		t, body, err := c.readMsg()
		if err != nil {
			return err
		}
		pr := &reader{b: body}
		if t != 'p' || pr.str() != c.s.Password {
			c.sendFatal("28P01", fmt.Sprintf("password authentication failed for user %q", user))
			c.w.Flush()
			return errors.New("authentication failed")
		}
	case AuthSCRAM:
		if err := c.scramAuth(user); err != nil {
			c.w.Flush()
			return err
		}
	}
	return nil
}

const (
	scramSHA256     = "SCRAM-SHA-256"
	scramSHA256Plus = "SCRAM-SHA-256-PLUS"
	cbindType       = "tls-server-end-point"
)

// scramAuth is the server side of SCRAM-SHA-256 in PostgreSQL's SASL
// messages: AuthenticationSASL, SASLInitialResponse,
// AuthenticationSASLContinue, SASLResponse, AuthenticationSASLFinal.
func (c *conn) scramAuth(user string) error {
	v := c.s.scram
	plus := c.tls && c.s.cbHash != nil
	m := newMsg('R').int32(10)
	if plus {
		m.str(scramSHA256Plus)
	}
	c.send(m.str(scramSHA256).byte1(0))
	if err := c.w.Flush(); err != nil {
		return err
	}
	violation := func(text string) error {
		c.sendFatal("08P01", text)
		return errors.New(text)
	}

	// SASLInitialResponse: mechanism, then the client-first-message
	t, body, err := c.readMsg()
	if err != nil {
		return err
	}
	if t != 'p' {
		return violation(fmt.Sprintf("expected SASL response, got message type %d", t))
	}
	r := &reader{b: body}
	mech := r.str()
	n := r.int32()
	first := string(r.bytes(n))
	if r.err != nil {
		return violation("malformed SASLInitialResponse message")
	}
	if mech != scramSHA256 && !(plus && mech == scramSHA256Plus) {
		return violation("client selected an invalid SASL authentication mechanism")
	}
	// gs2-header: cbind-flag "," [authzid] ","
	flag, rest, ok1 := strings.Cut(first, ",")
	authzid, bare, ok2 := strings.Cut(rest, ",")
	if !ok1 || !ok2 {
		return violation(fmt.Sprintf("malformed SCRAM message: %q", first))
	}
	switch {
	case flag == "n":
	case flag == "y":
		// the client could bind but thinks the server cannot: a
		// downgrade when SCRAM-SHA-256-PLUS was offered
		if plus {
			return violation("SCRAM channel binding negotiation error")
		}
	case flag == "p="+cbindType:
		if mech != scramSHA256Plus {
			return violation("channel binding requested with mechanism " + mech)
		}
	case strings.HasPrefix(flag, "p="):
		return violation(fmt.Sprintf("unsupported SCRAM channel-binding type %q", flag[2:]))
	default:
		return violation(fmt.Sprintf("malformed SCRAM message: unexpected channel-binding flag %q", flag))
	}
	if mech == scramSHA256Plus && flag[0] != 'p' {
		return violation("channel binding is required with " + scramSHA256Plus)
	}
	if authzid != "" {
		return violation("client uses authorization identity, but it is not supported")
	}
	// client-first-message-bare: n=<user>,r=<nonce>[,ext]; PostgreSQL
	// ignores the user name here and uses the startup packet's
	attrs := strings.Split(bare, ",")
	if len(attrs) < 2 || strings.HasPrefix(attrs[0], "m=") {
		return violation("client requires an unsupported SCRAM extension")
	}
	if !strings.HasPrefix(attrs[0], "n=") || !strings.HasPrefix(attrs[1], "r=") || len(attrs[1]) < 3 {
		return violation(fmt.Sprintf("malformed SCRAM message: %q", first))
	}
	clientNonce := attrs[1][2:]
	if !printable(clientNonce) {
		return violation("malformed SCRAM message: invalid nonce")
	}

	// server-first-message
	sn := make([]byte, scramNonceLen)
	rand.Read(sn)
	nonce := clientNonce + base64.StdEncoding.EncodeToString(sn)
	serverFirst := "r=" + nonce + ",s=" + base64.StdEncoding.EncodeToString(v.salt) + ",i=" + strconv.Itoa(v.iters)
	c.send(newMsg('R').int32(11).bytes([]byte(serverFirst)))
	if err := c.w.Flush(); err != nil {
		return err
	}

	// SASLResponse: client-final-message c=<cbind>,r=<nonce>[,ext],p=<proof>
	t, body, err = c.readMsg()
	if err != nil {
		return err
	}
	if t != 'p' {
		return violation(fmt.Sprintf("expected SASL response, got message type %d", t))
	}
	final := string(body)
	i := strings.LastIndex(final, ",p=")
	if i < 0 {
		return violation(fmt.Sprintf("malformed SCRAM message: %q", final))
	}
	withoutProof, proofB64 := final[:i], final[i+3:]
	fattrs := strings.Split(withoutProof, ",")
	if len(fattrs) < 2 || !strings.HasPrefix(fattrs[0], "c=") || !strings.HasPrefix(fattrs[1], "r=") {
		return violation(fmt.Sprintf("malformed SCRAM message: %q", final))
	}
	cbind := []byte(flag + "," + authzid + ",")
	if flag[0] == 'p' {
		h := c.s.cbHash()
		h.Write(c.s.TLS.Certificates[0].Certificate[0])
		cbind = h.Sum(cbind)
	}
	if got, err := base64.StdEncoding.DecodeString(fattrs[0][2:]); err != nil || !hmac.Equal(got, cbind) {
		return violation("SCRAM channel binding check failed")
	}
	if fattrs[1][2:] != nonce {
		return violation("unexpected SCRAM nonce")
	}
	proof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil || len(proof) != sha256.Size {
		return violation("malformed SCRAM message: invalid proof")
	}
	authMsg := bare + "," + serverFirst + "," + withoutProof
	sig := hmacSHA256(v.storedKey, authMsg)
	ck := make([]byte, sha256.Size)
	subtle.XORBytes(ck, proof, sig)
	sk := sha256.Sum256(ck)
	if subtle.ConstantTimeCompare(sk[:], v.storedKey) != 1 {
		c.sendFatal("28P01", fmt.Sprintf("password authentication failed for user %q", user))
		return errors.New("authentication failed")
	}
	serverFinal := "v=" + base64.StdEncoding.EncodeToString(hmacSHA256(v.serverKey, authMsg))
	return c.send(newMsg('R').int32(12).bytes([]byte(serverFinal)))
}

// printable: RFC 5802 nonce characters (printable ASCII except ',').
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e || s[i] == ',' {
			return false
		}
	}
	return true
}
