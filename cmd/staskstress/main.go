// staskstress opens N concurrent PostgreSQL-protocol connections to staskd
// and measures (1) connection setup, (2) a burst where every connection
// sends one request at the same instant, and (3) sustained closed-loop load.
//
// It speaks a minimal subset of the protocol (startup + simple query) so
// that one client connection costs only a few KB, which is what allows
// hundreds of thousands of connections from one machine.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hist is a lock-free log-scale latency histogram (microseconds).
type hist struct{ b [512]atomic.Int64 }

func bucket(us int64) int {
	if us < 1 {
		us = 1
	}
	return min(int(math.Log2(float64(us))*16), 511)
}

func (h *hist) add(d time.Duration) { h.b[bucket(d.Microseconds())].Add(1) }

func (h *hist) count() int64 {
	var n int64
	for i := range h.b {
		n += h.b[i].Load()
	}
	return n
}

// q returns the upper bound of the bucket holding quantile p, in ms.
func (h *hist) q(p float64) float64 {
	n := h.count()
	if n == 0 {
		return 0
	}
	target := int64(math.Ceil(p * float64(n)))
	var acc int64
	for i := range h.b {
		acc += h.b[i].Load()
		if acc >= target {
			return math.Pow(2, float64(i+1)/16) / 1000
		}
	}
	return math.Inf(1)
}

type client struct {
	c net.Conn
	r *bufio.Reader
	w []byte
}

func writeMsg(buf []byte, t byte, body ...[]byte) []byte {
	n := 4
	for _, b := range body {
		n += len(b)
	}
	if t != 0 {
		buf = append(buf, t)
	}
	buf = binary.BigEndian.AppendUint32(buf, uint32(n))
	for _, b := range body {
		buf = append(buf, b...)
	}
	return buf
}

func dial(addr string, local net.IP, timeout time.Duration, password string) (*client, error) {
	d := net.Dialer{Timeout: timeout}
	if local != nil {
		d.LocalAddr = &net.TCPAddr{IP: local}
	}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	cl := &client{c: c, r: bufio.NewReaderSize(c, 1024)}
	c.SetDeadline(time.Now().Add(timeout))
	body := binary.BigEndian.AppendUint32(nil, 196608)
	body = append(body, "user\x00stress\x00database\x00stask\x00\x00"...)
	if _, err := c.Write(writeMsg(nil, 0, body)); err != nil {
		c.Close()
		return nil, err
	}
	for {
		t, msg, err := cl.read()
		if err != nil {
			c.Close()
			return nil, err
		}
		switch t {
		case 'R':
			if len(msg) >= 4 && binary.BigEndian.Uint32(msg) == 3 {
				if _, err := c.Write(writeMsg(nil, 'p', []byte(password+"\x00"))); err != nil {
					c.Close()
					return nil, err
				}
			}
		case 'E':
			c.Close()
			return nil, fmt.Errorf("server error: %s", errText(msg))
		case 'Z':
			c.SetDeadline(time.Time{})
			return cl, nil
		}
	}
}

func (cl *client) read() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(cl.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
	if n < 0 || n > 1<<24 {
		return 0, nil, fmt.Errorf("bad length %d", n)
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(cl.r, buf)
	return hdr[0], buf, err
}

func errText(msg []byte) string {
	var code, text string
	for len(msg) > 1 {
		f := msg[0]
		i := 1
		for i < len(msg) && msg[i] != 0 {
			i++
		}
		v := string(msg[1:i])
		msg = msg[min(i+1, len(msg)):]
		switch f {
		case 'C':
			code = v
		case 'M':
			text = v
		}
	}
	return code + " " + text
}

// query runs one simple query and returns the SQLSTATE of an error, if any.
func (cl *client) query(sql string) (string, error) {
	cl.w = writeMsg(cl.w[:0], 'Q', []byte(sql), []byte{0})
	if _, err := cl.c.Write(cl.w); err != nil {
		return "", err
	}
	state := ""
	for {
		t, msg, err := cl.read()
		if err != nil {
			return "", err
		}
		switch t {
		case 'E':
			state = errText(msg)
		case 'Z':
			return state, nil
		}
	}
}

// localIPs expands "127.0.0.1-127.0.0.40" into addresses to dial from, so
// that more than ~64k connections can reach one server port.
func localIPs(spec string) []net.IP {
	if spec == "" {
		return []net.IP{nil}
	}
	lo, hi, _ := strings.Cut(spec, "-")
	a, b := net.ParseIP(lo).To4(), net.ParseIP(hi).To4()
	if b == nil {
		b = a
	}
	var ips []net.IP
	for x := binary.BigEndian.Uint32(a); x <= binary.BigEndian.Uint32(b); x++ {
		ips = append(ips, binary.BigEndian.AppendUint32(nil, x))
	}
	return ips
}

// memAvailableMB reads MemAvailable from /proc/meminfo.
func memAvailableMB() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return math.Inf(1)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "MemAvailable:") {
			kb, _ := strconv.ParseFloat(strings.Fields(l)[1], 64)
			return kb / 1024
		}
	}
	return math.Inf(1)
}

func rssMB(pid int) float64 {
	if pid <= 0 {
		pid = os.Getpid()
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			kb, _ := strconv.ParseFloat(strings.Fields(l)[1], 64)
			return kb / 1024
		}
	}
	return -1
}

type phase struct {
	Requests int64          `json:"requests"`
	Errors   map[string]int `json:"errors,omitempty"`
	WallMs   float64        `json:"wall_ms"`
	TPS      float64        `json:"tps"`
	P50      float64        `json:"p50_ms"`
	P99      float64        `json:"p99_ms"`
	P999     float64        `json:"p999_ms"`
	Max      float64        `json:"max_ms"`
}

type errCount struct {
	mu sync.Mutex
	m  map[string]int
}

func (e *errCount) add(s string) {
	if i := strings.IndexAny(s, "0123456789"); i > 0 && strings.Contains(s, ":") {
		// collapse per-address details, e.g. "dial tcp 127.0.0.3:0->...: ..."
		s = s[strings.LastIndex(s, ": ")+2:]
	}
	e.mu.Lock()
	if e.m == nil {
		e.m = map[string]int{}
	}
	e.m[s]++
	e.mu.Unlock()
}

func main() {
	addr := flag.String("addr", "127.0.0.1:17432", "server address")
	conns := flag.Int("conns", 1000, "concurrent connections")
	src := flag.String("src", "", "local source IP range, e.g. 127.0.0.1-127.0.0.40")
	dialPar := flag.Int("dial-par", 2000, "connection attempts in flight")
	dialTimeout := flag.Duration("dial-timeout", 30*time.Second, "connect + startup timeout")
	reqTimeout := flag.Duration("req-timeout", 60*time.Second, "per-request timeout")
	keys := flag.Int("keys", 100000, "rows in the stress table")
	writePct := flag.Int("write-pct", 20, "share of requests that are UPDATEs")
	duration := flag.Duration("duration", 30*time.Second, "sustained phase length (0 = skip)")
	think := flag.Duration("think", 0, "pause between requests per connection in the sustained phase")
	setup := flag.Bool("setup", true, "create and fill the stress table")
	password := flag.String("password", "", "server password")
	serverPID := flag.Int("server-pid", 0, "staskd pid, for memory reporting")
	label := flag.String("label", "", "label in the JSON report")
	minFree := flag.Float64("min-free-mb", 2048, "stop opening connections when the machine has less memory available")
	flag.Parse()

	if *setup {
		cl, err := dial(*addr, nil, *dialTimeout, *password)
		if err != nil {
			log.Fatalf("setup connect: %v", err)
		}
		if st, err := cl.query(`CREATE TABLE IF NOT EXISTS stress (k BIGINT PRIMARY KEY, v BIGINT NOT NULL)`); err != nil || st != "" {
			log.Fatalf("create: %v %s", err, st)
		}
		for i := 0; i < *keys; i += 1000 {
			var b strings.Builder
			b.WriteString("INSERT INTO stress VALUES ")
			for j := i; j < min(i+1000, *keys); j++ {
				if j > i {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "(%d,0)", j)
			}
			b.WriteString(" ON CONFLICT DO NOTHING")
			if st, err := cl.query(b.String()); err != nil || st != "" {
				log.Fatalf("fill: %v %s", err, st)
			}
		}
		cl.c.Close()
	}

	report := map[string]any{"label": *label, "conns_target": *conns, "write_pct": *writePct, "keys": *keys}
	baseRSS := rssMB(*serverPID)

	// ---- phase 1: connect ----
	ips := localIPs(*src)
	clients := make([]*client, *conns)
	var connHist hist
	var connErr errCount
	var ok atomic.Int64
	sem := make(chan struct{}, *dialPar)
	var wg sync.WaitGroup
	t0 := time.Now()
	stoppedAt := -1
	for i := 0; i < *conns; i++ {
		if i%1000 == 0 && memAvailableMB() < *minFree {
			stoppedAt = i
			log.Printf("connect: stopping at %d attempts, MemAvailable below %.0f MB", i, *minFree)
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			s := time.Now()
			cl, err := dial(*addr, ips[i%len(ips)], *dialTimeout, *password)
			if err != nil {
				connErr.add(err.Error())
				return
			}
			connHist.add(time.Since(s))
			clients[i] = cl
			ok.Add(1)
		}(i)
		if i > 0 && i%100000 == 0 {
			log.Printf("connect: %d attempted, %d ok, %.1fs", i, ok.Load(), time.Since(t0).Seconds())
		}
	}
	wg.Wait()
	connWall := time.Since(t0)
	live := clients[:0]
	for _, c := range clients {
		if c != nil {
			live = append(live, c)
		}
	}
	clients = live
	report["connect"] = map[string]any{
		"ok": len(clients), "failed": *conns - len(clients), "errors": connErr.m, "stopped_low_memory_at": stoppedAt,
		"wall_s": connWall.Seconds(), "rate_per_s": float64(len(clients)) / connWall.Seconds(),
		"p50_ms": connHist.q(0.5), "p99_ms": connHist.q(0.99), "max_ms": connHist.q(1),
	}
	report["server_rss_mb_idle"] = rssMB(*serverPID)
	report["server_rss_mb_before"] = baseRSS
	if len(clients) > 0 && *serverPID > 0 {
		report["server_kb_per_conn"] = (rssMB(*serverPID) - baseRSS) * 1024 / float64(len(clients))
	}
	report["client_rss_mb"] = rssMB(0)
	log.Printf("connected %d/%d in %v", len(clients), *conns, connWall.Round(time.Millisecond))

	request := func(cl *client, r *rand.Rand) (string, error) {
		k := r.IntN(*keys)
		var q string
		if r.IntN(100) < *writePct {
			q = "UPDATE stress SET v = v + 1 WHERE k = " + strconv.Itoa(k)
		} else {
			q = "SELECT v FROM stress WHERE k = " + strconv.Itoa(k)
		}
		cl.c.SetDeadline(time.Now().Add(*reqTimeout))
		return cl.query(q)
	}
	runPhase := func(name string, body func(i int, cl *client, h *hist, ec *errCount, n *atomic.Int64)) phase {
		var h hist
		var ec errCount
		var n atomic.Int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, cl := range clients {
			wg.Add(1)
			go func(i int, cl *client) {
				defer wg.Done()
				<-start
				body(i, cl, &h, &ec, &n)
			}(i, cl)
		}
		time.Sleep(200 * time.Millisecond) // let goroutines park on the barrier
		t := time.Now()
		close(start)
		wg.Wait()
		wall := time.Since(t)
		p := phase{Requests: n.Load(), Errors: ec.m, WallMs: float64(wall.Microseconds()) / 1000,
			TPS: float64(h.count()) / wall.Seconds(), P50: h.q(0.5), P99: h.q(0.99), P999: h.q(0.999), Max: h.q(1)}
		log.Printf("%s: %d requests, %d ok, wall %v, p50 %.2fms p99 %.2fms max %.2fms, errors %v",
			name, p.Requests, h.count(), wall.Round(time.Millisecond), p.P50, p.P99, p.Max, ec.m)
		return p
	}
	dead := make([]atomic.Bool, len(clients))
	one := func(i int, cl *client, h *hist, ec *errCount, n *atomic.Int64) bool {
		if dead[i].Load() {
			return false
		}
		r := rand.New(rand.NewPCG(uint64(i), uint64(time.Now().UnixNano())))
		s := time.Now()
		st, err := request(cl, r)
		n.Add(1)
		switch {
		case err != nil:
			ec.add(err.Error())
			dead[i].Store(true)
			return false
		case st != "":
			ec.add(st)
		default:
			h.add(time.Since(s))
		}
		return true
	}

	// ---- phase 2: burst: every connection sends one request at once ----
	if len(clients) > 0 {
		report["burst"] = runPhase("burst", func(i int, cl *client, h *hist, ec *errCount, n *atomic.Int64) {
			one(i, cl, h, ec, n)
		})
		report["server_rss_mb_burst"] = rssMB(*serverPID)
	}

	// ---- phase 3: sustained closed loop ----
	if len(clients) > 0 && *duration > 0 {
		deadline := time.Now().Add(*duration + 200*time.Millisecond)
		report["sustained"] = runPhase("sustained", func(i int, cl *client, h *hist, ec *errCount, n *atomic.Int64) {
			r := rand.New(rand.NewPCG(uint64(i), 7))
			if *think > 0 {
				// spread start times so think-time load is not synchronized
				time.Sleep(time.Duration(r.Int64N(int64(*think))))
			}
			for time.Now().Before(deadline) {
				if !one(i, cl, h, ec, n) {
					return
				}
				if *think > 0 {
					time.Sleep(*think)
				}
			}
		})
		report["server_rss_mb_sustained"] = rssMB(*serverPID)
	}
	for _, c := range clients {
		c.c.Close()
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
}
