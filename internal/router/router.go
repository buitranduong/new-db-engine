// Package router is a small HTTP router for a leader with replicas
// (docs/06 section 2a): writes, transactions and everything it does not
// know go to the leader; point reads, history and queries go to a replica.
//
// Read-your-writes: the router remembers, per session (header
// X-Stask-Session, else the client address), the highest LSN the leader
// answered in X-Stask-Lsn. A read sent to a replica carries that LSN (or
// the client's own X-Stask-Min-Lsn if higher) as X-Stask-Min-Lsn, so the
// replica waits until it has applied it. A replica that cannot catch up in
// time (503), or does not answer, is skipped and the leader serves the read.
package router

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	headerLSN     = "X-Stask-Lsn"
	headerMinLSN  = "X-Stask-Min-Lsn"
	headerTimeout = "X-Stask-Min-Lsn-Timeout"
	headerSession = "X-Stask-Session"
	maxSessions   = 100000
	maxReadBody   = 1 << 20
)

type Router struct {
	leader   *url.URL
	replicas []*url.URL
	lp       *httputil.ReverseProxy
	client   *http.Client
	rr       atomic.Uint64

	// WaitTimeout is how long a replica may hold a read for the LSN
	// (default 2s) before the router falls back to the leader.
	WaitTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]uint64

	// Counters for tests and metrics.
	ToLeader, ToReplica, Fallbacks atomic.Int64
}

// New returns a router for the leader and replica base URLs.
func New(leader string, replicas ...string) (*Router, error) {
	lu, err := url.Parse(leader)
	if err != nil {
		return nil, err
	}
	r := &Router{leader: lu, WaitTimeout: 2 * time.Second, sessions: map[string]uint64{},
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, s := range replicas {
		u, err := url.Parse(s)
		if err != nil {
			return nil, err
		}
		r.replicas = append(r.replicas, u)
	}
	r.lp = httputil.NewSingleHostReverseProxy(lu)
	r.lp.ModifyResponse = func(resp *http.Response) error {
		if v, err := strconv.ParseUint(resp.Header.Get(headerLSN), 10, 64); err == nil && resp.Request != nil {
			r.note(sessionOf(resp.Request), v)
		}
		return nil
	}
	return r, nil
}

func sessionOf(req *http.Request) string {
	if s := req.Header.Get(headerSession); s != "" {
		return "s:" + s
	}
	h, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		h = req.RemoteAddr
	}
	return "a:" + h
}

func (r *Router) note(sess string, lsn uint64) {
	r.mu.Lock()
	if len(r.sessions) >= maxSessions && r.sessions[sess] == 0 {
		clear(r.sessions) // crude bound; a forgotten session only loses its token
	}
	if lsn > r.sessions[sess] {
		r.sessions[sess] = lsn
	}
	r.mu.Unlock()
}

func (r *Router) token(sess string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[sess]
}

// readOnly reports whether the request can be served by a replica.
func readOnly(req *http.Request) bool {
	p := req.URL.Path
	switch {
	case req.Method == http.MethodGet && (p == "/v1/get" || p == "/v1/history" || p == "/v1/tables"):
		return true
	case req.Method == http.MethodPost && p == "/v1/query":
		return true
	}
	return false
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if len(r.replicas) == 0 || !readOnly(req) {
		r.ToLeader.Add(1)
		r.lp.ServeHTTP(w, req)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxReadBody+1))
	if err != nil || len(body) > maxReadBody {
		http.Error(w, "request body too large for routing", http.StatusRequestEntityTooLarge)
		return
	}
	min := r.token(sessionOf(req))
	if v, err := strconv.ParseUint(req.Header.Get(headerMinLSN), 10, 64); err == nil && v > min {
		min = v
	}
	n := len(r.replicas)
	start := int(r.rr.Add(1))
	for i := 0; i < n; i++ {
		rep := r.replicas[(start+i)%n]
		resp, err := r.forward(rep, req, body, min)
		if err != nil || resp.StatusCode == http.StatusServiceUnavailable {
			if resp != nil {
				resp.Body.Close()
			}
			r.Fallbacks.Add(1)
			continue
		}
		r.ToReplica.Add(1)
		copyResp(w, resp)
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.ToLeader.Add(1)
	r.lp.ServeHTTP(w, req)
}

func (r *Router) forward(to *url.URL, req *http.Request, body []byte, min uint64) (*http.Response, error) {
	u := *to
	u.Path = strings.TrimRight(to.Path, "/") + req.URL.Path
	u.RawQuery = req.URL.RawQuery
	out, err := http.NewRequestWithContext(req.Context(), req.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range req.Header {
		out.Header[k] = v
	}
	if min > 0 {
		out.Header.Set(headerMinLSN, strconv.FormatUint(min, 10))
		out.Header.Set(headerTimeout, strconv.FormatInt(r.WaitTimeout.Milliseconds(), 10))
	}
	return r.client.Do(out)
}

func copyResp(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
