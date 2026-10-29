package server

// HTTP API security (docs/12_plan.md §P9 S2/S3): TLS with optional client
// certificates (mTLS), bearer tokens in two classes (full and read-only),
// optional HTTP Basic with the PostgreSQL password, and a startup check
// that refuses a non-loopback listener without authentication.

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Access is what a credential may do.
type Access int

const (
	AccessNone     Access = iota
	AccessReadOnly        // GET endpoints and POST /v1/query
	AccessFull            // everything, admin endpoints included
)

// SecurityConfig holds the HTTP security settings, one field per staskd flag.
type SecurityConfig struct {
	Tokens       string // -http-token: comma-separated full-access tokens
	ROTokens     string // -http-token-ro: comma-separated read-only tokens
	TokenFile    string // -http-token-file: one token per line, "ro <token>" for read-only; re-read on change
	Basic        bool   // -http-basic: accept HTTP Basic with BasicPass as the password (any user name)
	BasicPass    string // -pg-password
	TLSCert      string // -http-tls-cert
	TLSKey       string // -http-tls-key
	TLSClientCA  string // -http-tls-client-ca: require client certificates signed by this CA (mTLS)
	HTTPInsecure bool   // -http-insecure: allow a non-loopback -addr without authentication
}

// hashed tokens: comparing SHA-256 digests in constant time hides both the
// content and the length of the configured tokens.
type tokenSet struct {
	full, ro [][32]byte
}

func (ts *tokenSet) empty() bool { return ts == nil || len(ts.full)+len(ts.ro) == 0 }

// Auth checks the credentials of HTTP requests. A nil *Auth lets everything
// through (no authentication configured).
type Auth struct {
	static tokenSet
	file   string
	fromF  atomic.Pointer[tokenSet]
	basic  [32]byte
	useB   bool
	mtls   bool // client certificates are verified by the TLS layer
	logf   func(string, ...any)

	mu    sync.Mutex // serializes reloads
	mtime time.Time
	size  int64
}

func splitTokens(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// parseTokenFile reads "token" (full access) or "ro token" / "rw token"
// lines; blank lines and lines starting with # are ignored.
func parseTokenFile(path string) (*tokenSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ts := &tokenSet{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fs := strings.Fields(line)
		switch {
		case len(fs) == 1:
			ts.full = append(ts.full, sha256.Sum256([]byte(fs[0])))
		case len(fs) == 2 && fs[0] == "rw":
			ts.full = append(ts.full, sha256.Sum256([]byte(fs[1])))
		case len(fs) == 2 && fs[0] == "ro":
			ts.ro = append(ts.ro, sha256.Sum256([]byte(fs[1])))
		default:
			return nil, fmt.Errorf("%s:%d: want \"<token>\", \"rw <token>\" or \"ro <token>\"", path, n)
		}
	}
	return ts, sc.Err()
}

// Enabled reports whether requests must authenticate.
func (a *Auth) Enabled() bool { return a != nil }

// Reload re-reads the token file if it changed (or always, with force).
// On error the previous tokens stay in force.
func (a *Auth) Reload(force bool) error {
	if a == nil || a.file == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := os.Stat(a.file)
	if err != nil {
		return err
	}
	if !force && st.ModTime().Equal(a.mtime) && st.Size() == a.size {
		return nil
	}
	ts, err := parseTokenFile(a.file)
	if err != nil {
		return err
	}
	a.fromF.Store(ts)
	a.mtime, a.size = st.ModTime(), st.Size()
	if a.logf != nil {
		a.logf("http auth: loaded %d full and %d read-only tokens from %s", len(ts.full), len(ts.ro), a.file)
	}
	return nil
}

// Watch re-reads the token file every interval until stop is closed, so a
// rotated file takes effect without a restart (SIGHUP forces it sooner).
func (a *Auth) Watch(every time.Duration, stop <-chan struct{}) {
	if a == nil || a.file == "" {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := a.Reload(false); err != nil && a.logf != nil {
				a.logf("http auth: token file: %v (previous tokens kept)", err)
			}
		}
	}
}

// match compares d with every token of the set without stopping early.
func match(d [32]byte, set [][32]byte) bool {
	ok := 0
	for i := range set {
		ok |= subtle.ConstantTimeCompare(d[:], set[i][:])
	}
	return ok == 1
}

func (a *Auth) tokenAccess(tok string) Access {
	d := sha256.Sum256([]byte(tok))
	full, ro := match(d, a.static.full), match(d, a.static.ro)
	if fs := a.fromF.Load(); fs != nil {
		full = match(d, fs.full) || full
		ro = match(d, fs.ro) || ro
	}
	switch {
	case full:
		return AccessFull
	case ro:
		return AccessReadOnly
	}
	return AccessNone
}

// hasTokens reports whether any bearer token is configured; a token file
// that is configured but empty still counts (fail closed).
func (a *Auth) hasTokens() bool { return !a.static.empty() || a.file != "" }

// Access returns what the request's credentials allow, and whether any
// credential was presented at all.
func (a *Auth) Access(r *http.Request) (Access, bool) {
	if a == nil {
		return AccessFull, false
	}
	// only mTLS configured: the TLS layer verified the client certificate
	if a.mtls && !a.hasTokens() && !a.useB {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			return AccessFull, true
		}
		return AccessNone, false
	}
	h := r.Header.Get("Authorization")
	scheme, cred, _ := strings.Cut(h, " ")
	cred = strings.TrimSpace(cred)
	switch {
	case strings.EqualFold(scheme, "Bearer") && cred != "":
		return a.tokenAccess(cred), true
	case strings.EqualFold(scheme, "Basic") && a.useB:
		if _, pw, ok := r.BasicAuth(); ok {
			d := sha256.Sum256([]byte(pw))
			if subtle.ConstantTimeCompare(d[:], a.basic[:]) == 1 {
				return AccessFull, true
			}
		}
		return AccessNone, true
	case h != "":
		return AccessNone, true
	}
	return AccessNone, false
}

// readOnlyRequest: endpoints a read-only token may call.
func readOnlyRequest(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	return r.Method == http.MethodPost && r.URL.Path == "/v1/query"
}

// publicPath: answered without credentials. /v1/health says only whether
// the engine accepts writes; /v1/status (table names, row counts, views)
// needs a credential.
func publicPath(r *http.Request) bool {
	return r.URL.Path == "/v1/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// Wrap puts authentication in front of h. A nil *Auth returns h unchanged.
func (a *Auth) Wrap(h http.Handler) http.Handler {
	if a == nil {
		return h
	}
	challenge := `Bearer realm="stask"`
	if a.useB {
		challenge += `, Basic realm="stask"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath(r) {
			h.ServeHTTP(w, r)
			return
		}
		acc, presented := a.Access(r)
		switch {
		case acc == AccessFull, acc == AccessReadOnly && readOnlyRequest(r):
			h.ServeHTTP(w, r)
			return
		case acc == AccessReadOnly:
			// RFC 6750 §3.1: valid token, not enough scope
			w.Header().Set("WWW-Authenticate", `Bearer realm="stask", error="insufficient_scope"`)
			reply(w, http.StatusForbidden, map[string]any{"error": "read-only token"})
			return
		}
		if presented {
			w.Header().Set("WWW-Authenticate", strings.Replace(challenge, `Bearer realm="stask"`, `Bearer realm="stask", error="invalid_token"`, 1))
		} else {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		reply(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
	})
}

// Build turns the settings into an *Auth (nil when nothing is configured)
// and a *tls.Config (nil without -http-tls-cert).
func (c SecurityConfig) Build(logf func(string, ...any)) (*Auth, *tls.Config, error) {
	var tc *tls.Config
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, nil, errors.New("-http-tls-cert and -http-tls-key go together")
	}
	if c.TLSClientCA != "" && c.TLSCert == "" {
		return nil, nil, errors.New("-http-tls-client-ca needs -http-tls-cert and -http-tls-key")
	}
	if c.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return nil, nil, err
		}
		tc = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		if c.TLSClientCA != "" {
			pem, err := os.ReadFile(c.TLSClientCA)
			if err != nil {
				return nil, nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, nil, fmt.Errorf("%s: no PEM certificates", c.TLSClientCA)
			}
			tc.ClientCAs, tc.ClientAuth = pool, tls.RequireAndVerifyClientCert
		}
	}
	a := &Auth{file: c.TokenFile, mtls: c.TLSClientCA != "", logf: logf}
	for _, t := range splitTokens(c.Tokens) {
		a.static.full = append(a.static.full, sha256.Sum256([]byte(t)))
	}
	for _, t := range splitTokens(c.ROTokens) {
		a.static.ro = append(a.static.ro, sha256.Sum256([]byte(t)))
	}
	if c.Basic {
		if c.BasicPass == "" {
			return nil, nil, errors.New("-http-basic needs -pg-password")
		}
		a.basic, a.useB = sha256.Sum256([]byte(c.BasicPass)), true
	}
	if a.file != "" {
		if err := a.Reload(true); err != nil {
			return nil, nil, fmt.Errorf("-http-token-file: %w", err)
		}
	}
	if !a.hasTokens() && !a.useB && !a.mtls {
		return nil, tc, nil
	}
	return a, tc, nil
}

// IsLoopback reports whether addr (host:port) listens only on loopback.
// An empty host means every interface; a host name counts as loopback only
// if all its addresses are.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// CheckBind refuses a listener that anyone on the network can reach with
// no authentication, unless insecure is set (flag named in the error).
// flagAddr and hint name the flags in the error.
func CheckBind(addr string, authenticated, insecure bool, flagAddr, hint string) error {
	if addr == "" || authenticated || insecure || IsLoopback(addr) {
		return nil
	}
	return fmt.Errorf("%s %s is not a loopback address and nothing authenticates its clients; %s", flagAddr, addr, hint)
}
