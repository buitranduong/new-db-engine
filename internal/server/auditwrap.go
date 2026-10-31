package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"stask/internal/audit"
)

// AuditWrap records, in the audit trail (docs/22), the HTTP requests that
// matter for security: authentication failures (401), refused scopes (403)
// and every POST under /v1/admin/ (checkpoint, GC, scrub, cluster
// membership, fault injection), with the identity of the caller: the client
// certificate name under mTLS, or a short digest of the bearer token (never
// the token), and the address. Put it outside Auth.Wrap so that failures are
// seen; with no audit log configured it costs one atomic load per request.
func AuditWrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := audit.Get()
		if !l.Has(audit.ClassAdmin) {
			h.ServeHTTP(w, r)
			return
		}
		sr := &statusRecorder{ResponseWriter: w, code: 200}
		h.ServeHTTP(sr, r)
		admin := r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/admin/")
		ev := ""
		switch {
		case sr.code == http.StatusUnauthorized:
			ev = "http.auth_fail"
		case sr.code == http.StatusForbidden:
			ev = "http.denied"
		case admin:
			ev = "http.admin"
		default:
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		l.Log(audit.Record{Ev: ev, User: HTTPIdentity(r), Addr: host, Cmd: r.Method, Obj: r.URL.Path,
			Detail: http.StatusText(sr.code), OK: sr.code < 400}, true)
	})
}

// HTTPIdentity names the caller for the audit trail without revealing a secret.
func HTTPIdentity(r *http.Request) string {
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.PeerCertificates) > 0 {
		return "cert:" + r.TLS.PeerCertificates[0].Subject.CommonName
	}
	scheme, cred, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	cred = strings.TrimSpace(cred)
	switch {
	case strings.EqualFold(scheme, "Bearer") && cred != "":
		d := sha256.Sum256([]byte(cred))
		return "token:" + hex.EncodeToString(d[:4])
	case strings.EqualFold(scheme, "Basic"):
		return "basic"
	}
	return "anonymous"
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
