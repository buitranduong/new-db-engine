package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"stask/internal/audit"
	"stask/internal/core"
	"stask/internal/planner"
)

// TestClusterMembersSecurity: POST /v1/admin/cluster/members changes the
// voters of the cluster, so it is the most dangerous endpoint. With mTLS and
// tokens it must refuse: no client certificate (handshake), a foreign
// certificate, a good certificate with no token, a wrong token, a read-only
// token; and the change must not reach the cluster in any refused case. The
// accepted and the refused attempts are in the audit trail, with the caller
// named by certificate or token digest and never by token.
func TestClusterMembersSecurity(t *testing.T) {
	p := makePKI(t)
	dir := t.TempDir()
	l, err := audit.Open(audit.Config{Dir: dir, Key: []byte("0123456789abcdef-audit-key"), Classes: audit.DefaultClasses})
	if err != nil {
		t.Fatal(err)
	}
	audit.SetDefault(l)
	t.Cleanup(func() { audit.SetDefault(nil) })

	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 4 << 20, NoCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	var changes []string
	s := New(planner.Start(e), nil)
	s.ClusterChange = func(op string, id uint64, raft, http string) error {
		changes = append(changes, op)
		return nil
	}
	a, tc, err := SecurityConfig{Tokens: "admin-tok", ROTokens: "ro-tok", TLSCert: p.serverCert, TLSKey: p.serverKey,
		TLSClientCA: filepath.Join(p.dir, "ca.pem")}.Build(t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(AuditWrap(a.Wrap(s.Handler())))
	srv.TLS = tc
	srv.StartTLS()
	t.Cleanup(srv.Close)
	body := `{"op":"add","id":4,"raft":"10.0.0.4:7000"}`
	url := srv.URL + "/v1/admin/cluster/members"

	for name, hc := range map[string]*http.Client{"no certificate": tlsClient(p), "foreign certificate": tlsClient(p, p.otherClientTLS)} {
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		if r, err := hc.Do(req); err == nil {
			r.Body.Close()
			t.Fatalf("%s accepted: %d", name, r.StatusCode)
		}
	}
	good := tlsClient(p, p.clientCert)
	for _, c := range []struct {
		name, auth string
		want       int
	}{{"certificate, no token", "", 401}, {"certificate, wrong token", "Bearer nope", 401}, {"certificate, read-only token", "Bearer ro-tok", 403}} {
		if r := do(t, good, "POST", url, c.auth, body); r.StatusCode != c.want {
			t.Fatalf("%s: %d, want %d", c.name, r.StatusCode, c.want)
		}
	}
	if len(changes) != 0 {
		t.Fatalf("a refused request changed the cluster: %v", changes)
	}
	if r := do(t, good, "POST", url, "Bearer admin-tok", body); r.StatusCode != 200 {
		t.Fatalf("certificate + admin token: %d", r.StatusCode)
	}
	if len(changes) != 1 || changes[0] != "add" {
		t.Fatalf("changes %v", changes)
	}
	l.Close()
	audit.SetDefault(nil)
	if _, err := audit.Verify(dir, []byte("0123456789abcdef-audit-key")); err != nil {
		t.Fatal(err)
	}
	rs, _ := audit.Query(dir, audit.Filter{Ev: "cluster.member"})
	if len(rs) != 1 || !rs[0].OK || rs[0].User != "cert:client" || !strings.Contains(rs[0].Detail, "node=4") {
		t.Fatalf("cluster.member records: %+v", rs)
	}
	fails, _ := audit.Query(dir, audit.Filter{Ev: "http."})
	var f401, f403 int
	for _, r := range fails {
		if strings.Contains(r.User, "admin-tok") || strings.Contains(r.User, "ro-tok") {
			t.Fatalf("a token in the audit trail: %+v", r)
		}
		switch r.Ev {
		case "http.auth_fail":
			f401++
		case "http.denied":
			f403++
		}
	}
	if f401 != 2 || f403 != 1 {
		t.Fatalf("auth failures in the trail: 401=%d 403=%d: %+v", f401, f403, fails)
	}
}
