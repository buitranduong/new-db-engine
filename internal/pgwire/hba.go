package pgwire

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// HBA is an allow list in the style of pg_hba.conf (docs/21 section 7).
// Lines, first match wins, no match means refused:
//
//	# TYPE      DATABASE  USER       ADDRESS          METHOD
//	hostssl     all       app,etl    10.0.0.0/8       scram-sha-256
//	host        all       all        127.0.0.1/32     scram-sha-256
//	host        all       all        0.0.0.0/0        reject
//
// TYPE: host (any connection), hostssl (TLS only), hostnossl (plain only).
// DATABASE and USER: all, or names separated by commas (a name starting with
// + is not supported). ADDRESS: CIDR, a single IP, or all. METHOD: reject,
// trust, password or scram-sha-256. The method is also a requirement on the
// server's own authentication (-pg-auth): scram-sha-256 needs scram, password
// needs password or scram; the server refuses to start if a rule asks for
// more than it can do (the file never silently weakens authentication).
type HBA struct {
	rules []hbaRule
}

type hbaRule struct {
	ssl     int // 0 any, 1 only TLS, 2 only plain
	dbs     map[string]bool
	users   map[string]bool
	nets    []*net.IPNet // nil with any=true: all addresses
	any     bool
	method  string
	lineNum int
}

// ParseHBA parses the text of a file.
func ParseHBA(text string) (*HBA, error) {
	h := &HBA{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 5 {
			return nil, fmt.Errorf("line %d: want TYPE DATABASE USER ADDRESS METHOD", n)
		}
		r := hbaRule{lineNum: n, dbs: map[string]bool{}, users: map[string]bool{}}
		switch f[0] {
		case "host":
		case "hostssl":
			r.ssl = 1
		case "hostnossl":
			r.ssl = 2
		default:
			return nil, fmt.Errorf("line %d: type %q (want host, hostssl or hostnossl)", n, f[0])
		}
		for _, d := range strings.Split(f[1], ",") {
			r.dbs[d] = true
		}
		for _, u := range strings.Split(f[2], ",") {
			if strings.HasPrefix(u, "+") || strings.HasPrefix(u, "@") {
				return nil, fmt.Errorf("line %d: group and file user lists (%q) are not supported", n, u)
			}
			r.users[u] = true
		}
		switch {
		case f[3] == "all":
			r.any = true
		case strings.Contains(f[3], "/"):
			_, ipn, err := net.ParseCIDR(f[3])
			if err != nil {
				return nil, fmt.Errorf("line %d: address %q: %v", n, f[3], err)
			}
			r.nets = []*net.IPNet{ipn}
		default:
			ip := net.ParseIP(f[3])
			if ip == nil {
				return nil, fmt.Errorf("line %d: address %q is not an IP, a CIDR or all (host names are not supported)", n, f[3])
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			r.nets = []*net.IPNet{{IP: ip, Mask: net.CIDRMask(bits, bits)}}
		}
		switch f[4] {
		case "reject", "trust", "password", "scram-sha-256":
			r.method = f[4]
		default:
			return nil, fmt.Errorf("line %d: method %q (want reject, trust, password or scram-sha-256)", n, f[4])
		}
		h.rules = append(h.rules, r)
	}
	if len(h.rules) == 0 {
		return nil, fmt.Errorf("no rules: every connection would be refused")
	}
	return h, sc.Err()
}

// Match returns the method of the first rule that fits, or "" if none.
func (h *HBA) Match(ip net.IP, tls bool, db, user string) string {
	for _, r := range h.rules {
		if r.ssl == 1 && !tls || r.ssl == 2 && tls {
			continue
		}
		if !r.dbs["all"] && !r.dbs[db] {
			continue
		}
		if !r.users["all"] && !r.users[user] {
			continue
		}
		if !r.any {
			in := false
			for _, n := range r.nets {
				if n.Contains(ip) {
					in = true
				}
			}
			if !in {
				continue
			}
		}
		return r.method
	}
	return ""
}

// Check verifies that the server's authentication meets every rule.
func (h *HBA) Check(auth string) error {
	for _, r := range h.rules {
		switch {
		case r.method == "scram-sha-256" && auth != AuthSCRAM:
			return fmt.Errorf("pg_hba line %d asks for scram-sha-256 but -pg-auth is %q", r.lineNum, auth)
		case r.method == "password" && auth != AuthSCRAM && auth != AuthPassword:
			return fmt.Errorf("pg_hba line %d asks for password authentication but -pg-auth is %q", r.lineNum, auth)
		}
	}
	return nil
}

// ---- throttle of failed logins ----

// failTracker slows down guessing: after failures from one address the next
// failure answer is delayed (50 ms doubling, at most 2 s), and a success
// forgets the address. The delay holds only that connection's goroutine.
type failTracker struct {
	mu sync.Mutex
	m  map[string]*failRec
}

type failRec struct {
	n    int
	last time.Time
}

// delay is how long a new attempt from host waits before it is examined.
func (t *failTracker) delay(host string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.m[host]
	if r == nil || time.Since(r.last) > 5*time.Minute || r.n <= 2 {
		return 0
	}
	d := 50 * time.Millisecond << (r.n - 3)
	if d > 2*time.Second || d <= 0 {
		d = 2 * time.Second
	}
	return d
}

// record notes a failed login from host.
func (t *failTracker) record(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*failRec{}
	}
	r := t.m[host]
	if r == nil || time.Since(r.last) > 5*time.Minute {
		r = &failRec{}
		t.m[host] = r
	}
	r.n++
	r.last = time.Now()
	if len(t.m) > 10000 { // bound the table: forget the old ones
		for k, v := range t.m {
			if time.Since(v.last) > time.Minute {
				delete(t.m, k)
			}
		}
	}
}

func (t *failTracker) ok(host string) {
	t.mu.Lock()
	delete(t.m, host)
	t.mu.Unlock()
}
