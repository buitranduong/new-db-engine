package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
)

func authServer(t *testing.T, c SecurityConfig) (*httptest.Server, *Auth) {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	a, tc, err := c.Build(t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	h := a.Wrap(New(planner.Start(e), nil).Handler())
	var srv *httptest.Server
	if tc != nil {
		srv = httptest.NewUnstartedServer(h)
		srv.TLS = tc
		srv.StartTLS()
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv, a
}

func do(t *testing.T, hc *http.Client, method, url, auth, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	r, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r
}

// TestHTTPAuthTokens: missing and wrong tokens get 401 with a challenge,
// good tokens 200; a read-only token reads but may not write (403); the
// health endpoint needs nothing; Basic with the PostgreSQL password works.
func TestHTTPAuthTokens(t *testing.T) {
	srv, _ := authServer(t, SecurityConfig{Tokens: "full1, full2", ROTokens: "reader", Basic: true, BasicPass: "pgpw"})
	hc := srv.Client()
	u := srv.URL
	table := `{"name":"t","columns":[{"name":"id","type":"string"},{"name":"v","type":"int"}],"key":"id"}`
	cases := []struct {
		method, path, auth, body string
		want                     int
	}{
		{"GET", "/v1/status", "", "", 401},
		{"GET", "/v1/status", "Bearer nope", "", 401},
		{"GET", "/v1/status", "Bearer full1x", "", 401},
		{"GET", "/v1/status", "Token full1", "", 401},
		{"POST", "/v1/admin/checkpoint", "", "", 401},
		{"GET", "/v1/health", "", "", 200},
		{"GET", "/v1/status", "Bearer full1", "", 200},
		{"GET", "/v1/status", "bearer full2", "", 200},
		{"GET", "/v1/status", "Bearer reader", "", 200},
		{"POST", "/v1/tables", "Bearer reader", table, 403},
		{"POST", "/v1/tables", "Bearer full2", table, 200},
		{"GET", "/v1/tables", "Bearer reader", "", 200},
		{"POST", "/v1/query", "Bearer reader", `{"kind":"scan","scan":{"table":"t"}}`, 200},
		{"POST", "/v1/write", "Bearer reader", `{"ops":[{"table":"t","row":{"id":"a","v":1}}]}`, 403},
		{"POST", "/v1/write", "Bearer full1", `{"ops":[{"table":"t","row":{"id":"a","v":1}}]}`, 200},
		{"POST", "/v1/admin/checkpoint", "Bearer reader", "", 403},
		{"GET", "/v1/status", "Basic " + basic("any", "pgpw"), "", 200},
		{"GET", "/v1/status", "Basic " + basic("any", "bad"), "", 401},
	}
	for _, c := range cases {
		r := do(t, hc, c.method, u+c.path, c.auth, c.body)
		if r.StatusCode != c.want {
			t.Errorf("%s %s auth=%q: %d, want %d", c.method, c.path, c.auth, r.StatusCode, c.want)
		}
		ch := r.Header.Get("WWW-Authenticate")
		switch r.StatusCode {
		case 401:
			if !strings.Contains(ch, `Bearer realm="stask"`) || !strings.Contains(ch, `Basic realm="stask"`) {
				t.Errorf("%s %s: challenge %q", c.method, c.path, ch)
			}
			if c.auth != "" && !strings.Contains(ch, "invalid_token") {
				t.Errorf("%s %s auth=%q: challenge %q lacks invalid_token", c.method, c.path, c.auth, ch)
			}
		case 403:
			if !strings.Contains(ch, "insufficient_scope") {
				t.Errorf("%s %s: challenge %q", c.method, c.path, ch)
			}
		}
	}
}

func basic(u, p string) string {
	r, _ := http.NewRequest("GET", "/", nil)
	r.SetBasicAuth(u, p)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

// TestHTTPAuthDefaultOpen: with nothing configured the API stays open, as
// the benchmarks and scripts expect.
func TestHTTPAuthDefaultOpen(t *testing.T) {
	srv, a := authServer(t, SecurityConfig{})
	if a.Enabled() {
		t.Fatal("auth enabled with nothing configured")
	}
	if r := do(t, srv.Client(), "GET", srv.URL+"/v1/status", "", ""); r.StatusCode != 200 {
		t.Fatalf("status: %d", r.StatusCode)
	}
}

// TestHTTPAuthTokenFile: tokens from a file; a rewritten file is picked up
// by Reload, and an unreadable one keeps the previous tokens.
func TestHTTPAuthTokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens")
	os.WriteFile(p, []byte("# tokens\nold\nro oldreader\n"), 0o600)
	srv, a := authServer(t, SecurityConfig{TokenFile: p})
	hc := srv.Client()
	get := func(tok string) int {
		return do(t, hc, "GET", srv.URL+"/v1/status", "Bearer "+tok, "").StatusCode
	}
	if get("old") != 200 || get("oldreader") != 200 || get("new") != 401 {
		t.Fatal("initial tokens")
	}
	os.WriteFile(p, []byte("new\n"), 0o600)
	future := time.Now().Add(time.Minute)
	os.Chtimes(p, future, future)
	if err := a.Reload(false); err != nil {
		t.Fatal(err)
	}
	if get("old") != 401 || get("oldreader") != 401 || get("new") != 200 {
		t.Fatal("rotated tokens")
	}
	os.WriteFile(p, []byte("bad line with spaces\n"), 0o600)
	if err := a.Reload(true); err == nil {
		t.Fatal("bad file accepted")
	}
	if get("new") != 200 {
		t.Fatal("bad file dropped the previous tokens")
	}
	// an empty token file still authenticates (denies everyone)
	os.WriteFile(p, nil, 0o600)
	if err := a.Reload(true); err != nil {
		t.Fatal(err)
	}
	if get("new") != 401 || do(t, hc, "GET", srv.URL+"/v1/status", "", "").StatusCode != 401 {
		t.Fatal("empty token file must fail closed")
	}
}

type pki struct {
	dir                        string
	caPEM                      []byte
	ca                         *x509.Certificate
	caKey                      *ecdsa.PrivateKey
	serverCert, serverKey      string
	clientCert, otherClientTLS tls.Certificate
}

func newCert(t *testing.T, tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func pemPair(t *testing.T, der []byte, key *ecdsa.PrivateKey) ([]byte, []byte) {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func makePKI(t *testing.T) *pki {
	p := &pki{dir: t.TempDir()}
	now := time.Now()
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, key := newCert(t, caT, nil, nil)
	p.ca, _ = x509.ParseCertificate(der)
	p.caKey = key
	p.caPEM, _ = pemPair(t, der, key)
	srvT := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, key = newCert(t, srvT, p.ca, p.caKey)
	c, k := pemPair(t, der, key)
	p.serverCert, p.serverKey = filepath.Join(p.dir, "srv.pem"), filepath.Join(p.dir, "srv.key")
	os.WriteFile(p.serverCert, c, 0o600)
	os.WriteFile(p.serverKey, k, 0o600)
	os.WriteFile(filepath.Join(p.dir, "ca.pem"), p.caPEM, 0o600)
	cliT := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "client"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, key = newCert(t, cliT, p.ca, p.caKey)
	c, k = pemPair(t, der, key)
	p.clientCert, _ = tls.X509KeyPair(c, k)
	// self-signed, not from the CA
	der, key = newCert(t, cliT, nil, nil)
	c, k = pemPair(t, der, key)
	p.otherClientTLS, _ = tls.X509KeyPair(c, k)
	return p
}

func tlsClient(p *pki, certs ...tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: certs}}}
}

// TestHTTPTLS: the API over TLS with a token; plain HTTP to the TLS port
// fails.
func TestHTTPTLS(t *testing.T) {
	p := makePKI(t)
	srv, _ := authServer(t, SecurityConfig{Tokens: "tok", TLSCert: p.serverCert, TLSKey: p.serverKey})
	if !strings.HasPrefix(srv.URL, "https://") {
		t.Fatalf("url %s", srv.URL)
	}
	hc := tlsClient(p)
	if r := do(t, hc, "GET", srv.URL+"/v1/status", "Bearer tok", ""); r.StatusCode != 200 || r.TLS == nil {
		t.Fatalf("tls status: %d", r.StatusCode)
	}
	if r := do(t, hc, "GET", srv.URL+"/v1/status", "", ""); r.StatusCode != 401 {
		t.Fatalf("tls without token: %d", r.StatusCode)
	}
	plain := strings.Replace(srv.URL, "https://", "http://", 1)
	if r := do(t, &http.Client{Timeout: 5 * time.Second}, "GET", plain+"/v1/status", "Bearer tok", ""); r.StatusCode == 200 {
		t.Fatal("plain HTTP served on the TLS port")
	}
}

// TestHTTPMTLS: with only a client CA, a certificate it signed is the
// credential; no certificate or a foreign one fails the handshake.
func TestHTTPMTLS(t *testing.T) {
	p := makePKI(t)
	srv, a := authServer(t, SecurityConfig{TLSCert: p.serverCert, TLSKey: p.serverKey, TLSClientCA: filepath.Join(p.dir, "ca.pem")})
	if !a.Enabled() {
		t.Fatal("mTLS must count as authentication")
	}
	if r := do(t, tlsClient(p, p.clientCert), "POST", srv.URL+"/v1/admin/checkpoint", "", ""); r.StatusCode != 200 {
		t.Fatalf("client cert: %d", r.StatusCode)
	}
	for name, hc := range map[string]*http.Client{"none": tlsClient(p), "foreign": tlsClient(p, p.otherClientTLS)} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/status", nil)
		if r, err := hc.Do(req); err == nil {
			r.Body.Close()
			t.Fatalf("%s client certificate accepted: %d", name, r.StatusCode)
		}
	}
}

func TestBuildErrors(t *testing.T) {
	for _, c := range []SecurityConfig{
		{TLSCert: "x"},
		{TLSClientCA: "x"},
		{Basic: true},
		{TokenFile: "/nonexistent/tokens"},
	} {
		if _, _, err := c.Build(nil); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

// TestCheckBind: a listener reachable from the network needs
// authentication or the explicit insecure flag.
func TestCheckBind(t *testing.T) {
	cases := []struct {
		addr          string
		auth, insecur bool
		ok            bool
	}{
		{"127.0.0.1:17070", false, false, true},
		{"127.0.0.5:17070", false, false, true},
		{"[::1]:17070", false, false, true},
		{"localhost:17070", false, false, true},
		{"", false, false, true}, // listener off
		{":17070", false, false, false},
		{"0.0.0.0:17070", false, false, false},
		{"[::]:17070", false, false, false},
		{"192.168.0.220:17070", false, false, false},
		{"0.0.0.0:17070", true, false, true},
		{"0.0.0.0:17070", false, true, true},
		{"no-such-host.invalid:17070", false, false, false},
	}
	for _, c := range cases {
		err := CheckBind(c.addr, c.auth, c.insecur, "-addr", "hint")
		if (err == nil) != c.ok {
			t.Errorf("CheckBind(%q, auth=%v, insecure=%v) = %v", c.addr, c.auth, c.insecur, err)
		}
	}
}
