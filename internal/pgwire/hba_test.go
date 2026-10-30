package pgwire

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"stask/internal/sql"
)

func TestHBAMatch(t *testing.T) {
	h, err := ParseHBA(`
# comment
hostssl  all      app,etl   10.0.0.0/8     scram-sha-256
host     appdb    all       127.0.0.1      scram-sha-256
host     all      all       192.168.1.0/24 reject   # denied explicitly
host     all      ops       all            scram-sha-256
`)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP
	for _, c := range []struct {
		ip      string
		tls     bool
		db, usr string
		want    string
	}{
		{"10.1.2.3", true, "x", "app", "scram-sha-256"},
		{"10.1.2.3", false, "x", "app", ""}, // hostssl: plain refused
		{"10.1.2.3", true, "x", "bob", ""},  // user not listed
		{"127.0.0.1", false, "appdb", "zed", "scram-sha-256"},
		{"127.0.0.1", false, "other", "zed", ""},
		{"192.168.1.9", true, "x", "ops", "reject"}, // first match wins
		{"8.8.8.8", false, "x", "ops", "scram-sha-256"},
		{"8.8.8.8", false, "x", "app", ""},
	} {
		if got := h.Match(ip(c.ip), c.tls, c.db, c.usr); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
	if err := h.Check(AuthSCRAM); err != nil {
		t.Error(err)
	}
	if err := h.Check(AuthPassword); err == nil || !strings.Contains(err.Error(), "scram-sha-256") {
		t.Errorf("Check with password auth: %v", err)
	}
	for _, bad := range []string{"", "host all all 1.2.3.4 md5", "local all all x trust", "host all +grp 1.2.3.4/32 trust", "host all all example.com trust", "host all all"} {
		if _, err := ParseHBA(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestHBAEnforced(t *testing.T) {
	addr := startAuthServer(t, func(s *Server) {
		s.Auth = AuthSCRAM
		s.Password = "pw"
		s.TLS = selfSigned(t)
		h, err := ParseHBA("hostssl all app 127.0.0.1 scram-sha-256\nhost all all all reject\n")
		if err != nil {
			t.Fatal(err)
		}
		s.HBA = h
	})
	if _, err := selectOne(t, "postgres://app:pw@"+addr+"/stask?sslmode=require"); err != nil {
		t.Fatalf("allowed connection: %v", err)
	}
	if _, err := selectOne(t, "postgres://app:pw@"+addr+"/stask?sslmode=disable"); sqlState(err) != "28000" {
		t.Fatalf("plain connection of an allowed user: want 28000, got %v", err)
	}
	if _, err := selectOne(t, "postgres://other:pw@"+addr+"/stask?sslmode=require"); sqlState(err) != "28000" {
		t.Fatalf("user not on the list: want 28000, got %v", err)
	}
	// a refused attempt is not an authentication failure of the throttle, but is counted
	before := sql.AuthFailuresTotal()
	selectOne(t, "postgres://other:pw@"+addr+"/stask?sslmode=require")
	if sql.AuthFailuresTotal() <= before {
		t.Error("a refusal by the allow list is not counted in stask_pg_auth_failures_total")
	}
}

// Failed logins from one address are slowed down; a good login resets.
func TestAuthThrottle(t *testing.T) {
	var ft failTracker
	for i := 0; i < 2; i++ {
		if d := ft.delay("1.2.3.4"); d != 0 {
			t.Fatalf("delay %v after %d failures", d, i)
		}
		ft.record("1.2.3.4")
	}
	ft.record("1.2.3.4")
	d3 := ft.delay("1.2.3.4")
	ft.record("1.2.3.4")
	d4 := ft.delay("1.2.3.4")
	if d3 != 50*time.Millisecond || d4 != 100*time.Millisecond {
		t.Fatalf("delays %v %v", d3, d4)
	}
	for i := 0; i < 30; i++ {
		ft.record("1.2.3.4")
	}
	if d := ft.delay("1.2.3.4"); d != 2*time.Second {
		t.Fatalf("cap: %v", d)
	}
	if ft.delay("5.6.7.8") != 0 {
		t.Fatal("another address is delayed")
	}
	ft.ok("1.2.3.4")
	if ft.delay("1.2.3.4") != 0 {
		t.Fatal("success did not reset")
	}
}

// Role passwords are never in pg_stat_activity or the slow-statement log.
func TestRolePasswordNotLogged(t *testing.T) {
	var logged []string
	sql.SetSlowQueryLog(time.Nanosecond, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	defer sql.SetSlowQueryLog(0, nil)
	addr := startAuthServer(t, func(s *Server) {})
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, "postgres://postgres@"+addr+"/stask?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE ROLE leaky LOGIN PASSWORD 'sup3r-s3cret-pw'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `ALTER ROLE leaky PASSWORD 'an0ther-s3cret-pw'`); err != nil {
		t.Fatal(err)
	}
	for _, l := range logged {
		if strings.Contains(l, "s3cret") {
			t.Fatalf("a password reached the slow-statement log: %s", l)
		}
	}
	if len(logged) == 0 {
		t.Fatal("the slow log recorded nothing: the test checks nothing")
	}
	rows, err := conn.Query(ctx, `SELECT query FROM pg_stat_activity`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var q *string
		rows.Scan(&q)
		if q != nil && strings.Contains(*q, "s3cret") {
			t.Fatalf("a password is in pg_stat_activity: %s", *q)
		}
	}
	var v *string
	if err := conn.QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE rolname = 'leaky'`).Scan(&v); err == nil && v != nil && strings.Trim(*v, "*") != "" {
		t.Fatalf("pg_authid shows a password or verifier: %q", *v)
	}
}
