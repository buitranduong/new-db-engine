// staskbench drives the Addendum-A workloads (WL-1..WL-5) against staskd
// over HTTP and reports latency percentiles, throughput, view lag and
// write amplification.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	baseURL   = flag.String("url", "http://127.0.0.1:17070", "staskd base url")
	token     = flag.String("token", os.Getenv("STASK_HTTP_TOKEN"), "bearer token for a staskd with -http-token (default $STASK_HTTP_TOKEN; empty = none)")
	workload  = flag.String("wl", "1", "workload: 1|2|3|4|5")
	dur       = flag.Duration("duration", 30*time.Second, "measurement duration")
	conc      = flag.Int("conc", 32, "concurrent clients")
	rate      = flag.Float64("rate", 0, "target total ops/s (0 = closed loop, as fast as possible)")
	nKeys     = flag.Int("n", 1_000_000, "rows to preload")
	valSize   = flag.Int("value-size", 1024, "WL-1 value size in bytes")
	skipLoad  = flag.Bool("skip-load", false, "assume data is already loaded")
	pid       = flag.Int("pid", 0, "staskd pid (for /proc/<pid>/io write accounting)")
	outFile   = flag.String("out", "", "append the JSON summary to this file")
	bgRate    = flag.Int("bg-writes", 2000, "background write rows/s during read-heavy workloads (WL-2, WL-3)")
	qconc     = flag.Int("qconc", 2, "query concurrency for WL-2/WL-3/WL-4")
	label     = flag.String("label", "", "free-form label stored with the result")
	embedded  = flag.String("embedded", "", "WL-1 only: open the engine in-process at this directory instead of using HTTP")
	pgURL     = flag.String("pg", "", "WL-1 only: run the same mix over the PostgreSQL protocol at this URL (STASK's PG port or PostgreSQL)")
	redisAddr = flag.String("redis", "", "WL-1 only: run the same mix against Redis at this host:port instead of staskd")
	diskDev   = flag.String("disk", envOr("STASK_DISK", "sda3"), "block device (as in /proc/diskstats) holding the data dir")
	warmup    = flag.Duration("warmup", 5*time.Second, "run this long before measuring (not recorded)")
)

// ---- http client ----

type client struct {
	hc *http.Client
}

func newClient(n int) *client {
	tr := &http.Transport{
		MaxIdleConns:        n * 2,
		MaxIdleConnsPerHost: n * 2,
		MaxConnsPerHost:     0,
		DisableCompression:  true,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	return &client{hc: &http.Client{Transport: tr, Timeout: 120 * time.Second}}
}

// do returns the HTTP status; out (if non-nil) receives the JSON body.
func (c *client) do(method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, *baseURL+path, rd)
	if err != nil {
		return 0, err
	}
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		d := json.NewDecoder(resp.Body)
		d.UseNumber()
		if err := d.Decode(out); err != nil && err != io.EOF {
			return resp.StatusCode, err
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, nil
}

func (c *client) post(path string, body, out any) (int, error) { return c.do("POST", path, body, out) }
func (c *client) get(path string, out any) (int, error)        { return c.do("GET", path, nil, out) }

func must(code int, err error, what string) {
	if err != nil || code/100 != 2 {
		log.Fatalf("%s: status=%d err=%v", what, code, err)
	}
}

func num(v any) float64 {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case float64:
		return x
	}
	return 0
}

// ---- latency recording ----

type recorder struct {
	lat   map[string][]time.Duration
	errs  map[string]int
	extra map[string][]float64
}

func newRecorder() *recorder {
	return &recorder{lat: map[string][]time.Duration{}, errs: map[string]int{}, extra: map[string][]float64{}}
}

func (r *recorder) add(op string, d time.Duration) { r.lat[op] = append(r.lat[op], d) }
func (r *recorder) err(op string)                  { r.errs[op]++ }
func (r *recorder) val(name string, v float64)     { r.extra[name] = append(r.extra[name], v) }

func (r *recorder) merge(o *recorder) {
	for k, v := range o.lat {
		r.lat[k] = append(r.lat[k], v...)
	}
	for k, v := range o.errs {
		r.errs[k] += v
	}
	for k, v := range o.extra {
		r.extra[k] = append(r.extra[k], v...)
	}
}

type opStats struct {
	Count  int     `json:"count"`
	Errors int     `json:"errors"`
	OpsSec float64 `json:"ops_per_sec"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	P999Ms float64 `json:"p999_ms"`
	MaxMs  float64 `json:"max_ms"`
}

func pct(s []time.Duration, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	i := int(float64(len(s)-1) * p)
	return float64(s[i].Microseconds()) / 1000
}

func (r *recorder) summary(elapsed time.Duration) map[string]opStats {
	out := map[string]opStats{}
	all := []time.Duration{}
	allErr := 0
	for op, s := range r.lat {
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		var sum time.Duration
		for _, d := range s {
			sum += d
		}
		st := opStats{Count: len(s), Errors: r.errs[op], OpsSec: float64(len(s)) / elapsed.Seconds(),
			P50Ms: pct(s, .5), P95Ms: pct(s, .95), P99Ms: pct(s, .99), P999Ms: pct(s, .999)}
		if len(s) > 0 {
			st.MeanMs = float64(sum.Microseconds()) / 1000 / float64(len(s))
			st.MaxMs = float64(s[len(s)-1].Microseconds()) / 1000
		}
		out[op] = st
		all = append(all, s...)
		allErr += r.errs[op]
	}
	for op, n := range r.errs {
		if _, ok := out[op]; !ok {
			out[op] = opStats{Errors: n}
			allErr += n
		}
	}
	if len(r.lat) > 1 {
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
		var sum time.Duration
		for _, d := range all {
			sum += d
		}
		out["ALL"] = opStats{Count: len(all), Errors: allErr, OpsSec: float64(len(all)) / elapsed.Seconds(),
			MeanMs: float64(sum.Microseconds()) / 1000 / float64(max(len(all), 1)), MaxMs: pct(all, 1),
			P50Ms: pct(all, .5), P95Ms: pct(all, .95), P99Ms: pct(all, .99), P999Ms: pct(all, .999)}
	}
	return out
}

// timeline: max latency per second of the run (to correlate stalls)
func timeline(r *recorder) []float64 {
	secs, lats := r.extra["tl_sec"], r.extra["tl_lat"]
	var out []float64
	for i, s := range secs {
		for int(s) >= len(out) {
			out = append(out, 0)
		}
		out[int(s)] = max(out[int(s)], lats[i])
	}
	for i := range out {
		out[i] = float64(int(out[i]))
	}
	return out
}

func valSummary(v []float64) map[string]float64 {
	if len(v) == 0 {
		return nil
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	q := func(p float64) float64 { return s[int(float64(len(s)-1)*p)] }
	return map[string]float64{"n": float64(len(s)), "mean": sum / float64(len(s)), "p50": q(.5), "p95": q(.95), "p99": q(.99), "max": s[len(s)-1]}
}

// run executes fn from n workers for d. With a target rate, each worker
// follows a fixed schedule and latency is measured from the scheduled
// start (no coordinated omission).
func run(n int, d time.Duration, targetRate float64, fn func(w int, rng *rand.Rand, rec *recorder) (string, bool)) (*recorder, time.Duration) {
	recs := make([]*recorder, n)
	var wg sync.WaitGroup
	start := time.Now()
	measureFrom := start.Add(*warmup)
	deadline := measureFrom.Add(d)
	cpu0 := make(chan time.Duration, 1)
	go func() {
		time.Sleep(time.Until(measureFrom))
		cpu0 <- processCPU()
	}()
	for w := 0; w < n; w++ {
		recs[w] = newRecorder()
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)*7919 + time.Now().UnixNano()))
			var interval time.Duration
			next := start
			if targetRate > 0 {
				interval = time.Duration(float64(n) / targetRate * float64(time.Second))
				next = start.Add(time.Duration(rng.Int63n(int64(interval) + 1)))
			}
			for {
				t0 := time.Now()
				if targetRate > 0 {
					if next.After(deadline) {
						return
					}
					if sl := time.Until(next); sl > 0 {
						time.Sleep(sl)
					}
					t0 = next
					next = next.Add(interval)
				} else if t0.After(deadline) {
					return
				}
				op, ok := fn(w, rng, recs[w])
				if op == "" || t0.Before(measureFrom) {
					continue
				}
				if ok {
					lat := time.Since(t0)
					recs[w].add(op, lat)
					sec := float64(int(t0.Sub(measureFrom).Seconds()))
					recs[w].val("tl_sec", sec)
					recs[w].val("tl_lat", float64(lat.Microseconds())/1000)
				} else {
					recs[w].err(op)
				}
			}
		}(w)
	}
	wg.Wait()
	el := time.Since(measureFrom)
	clientCPU = (processCPU() - <-cpu0).Seconds() / el.Seconds()
	all := newRecorder()
	for _, r := range recs {
		all.merge(r)
	}
	return all, el
}

// ---- server-side accounting ----

type snapshot struct {
	status    map[string]any
	procWrite int64
	devWrite  int64
	at        time.Time
}

func (c *client) snap() snapshot {
	s := snapshot{at: time.Now()}
	c.get("/v1/status", &s.status)
	if *pid > 0 {
		s.procWrite = procIOWrite(*pid)
	}
	s.devWrite = deviceWriteBytes()
	return s
}

func procIOWrite(pid int) int64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "write_bytes: "); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

// deviceWriteBytes: sectors written on the data disk (includes every other
// process on the machine; reported as context only).
func deviceWriteBytes() int64 {
	b, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 9 && f[2] == *diskDev {
			n, _ := strconv.ParseInt(f[9], 10, 64)
			return n * 512
		}
	}
	return 0
}

func amplification(a, b snapshot) map[string]any {
	d := func(k string) float64 { return num(b.status[k]) - num(a.status[k]) }
	payload := d("payload_bytes")
	out := map[string]any{
		"payload_bytes":    payload,
		"wal_bytes":        d("wal_bytes"),
		"checkpoint_bytes": d("checkpoint_bytes"),
		"commits":          d("commits"),
		"syncs":            d("syncs"),
		"conflicts":        d("conflicts"),
	}
	if payload > 0 {
		out["wal_prezero_bytes"] = d("wal_prezero_bytes")
		out["waf_logical"] = (d("wal_bytes") + d("checkpoint_bytes") + d("wal_prezero_bytes")) / payload
		if b.procWrite > 0 {
			out["proc_write_bytes"] = b.procWrite - a.procWrite
			out["waf_process"] = float64(b.procWrite-a.procWrite) / payload
		}
		out["device_write_bytes_all_processes"] = b.devWrite - a.devWrite
		out["waf_device_upper_bound"] = float64(b.devWrite-a.devWrite) / payload
	}
	out["sync_max_ms_since_start"] = num(b.status["sync_max_ms"])
	out["syncs_over_50ms"] = d("syncs_over_50ms")
	if sy := d("syncs"); sy > 0 {
		out["sync_mean_ms"] = d("sync_ns_total") / sy / 1e6
	}
	if c := d("commits"); c > 0 {
		out["commits_per_sync"] = c / max(d("syncs"), 1)
	}
	return out
}

func report(name string, res map[string]any) {
	res["workload"] = name
	res["label"] = *label
	if tl, ok := res["timeline_max_ms"]; ok && tl == nil {
		delete(res, "timeline_max_ms")
	}
	res["time"] = time.Now().Format(time.RFC3339)
	if clientCPU > 0 {
		// the client's own CPU during the measured window: above 70% of
		// the cores it may use, latency includes client-side queueing
		// (docs/11 §5)
		n := runtime.NumCPU()
		res["client_cpu_cores"] = clientCPU
		res["client_cpus"] = n
		if clientCPU > 0.7*float64(n) {
			res["client_saturated"] = true
			log.Printf("WARNING: client used %.2f of %d cores: results may be client-bound", clientCPU, n)
		}
	}
	res["params"] = map[string]any{"duration_s": dur.Seconds(), "conc": *conc, "rate": *rate, "n": *nKeys,
		"value_size": *valSize, "bg_writes": *bgRate, "qconc": *qconc, "warmup_s": warmup.Seconds()}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *outFile != "" {
		f, err := os.OpenFile(*outFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			c, _ := json.Marshal(res)
			f.Write(append(c, '\n'))
			f.Close()
		}
	}
}

// waitViews blocks until every derived view has applied the durable LSN.
func (c *client) waitViews() time.Duration {
	t0 := time.Now()
	for {
		var st map[string]any
		c.get("/v1/status", &st)
		durable := num(st["durable_lsn"])
		ok := true
		views, _ := st["views"].([]any)
		for _, v := range views {
			m := v.(map[string]any)
			if m["ready"] != true || num(m["applied_lsn"]) < durable {
				ok = false
			}
		}
		if ok {
			return time.Since(t0)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func createTable(c *client, t map[string]any) {
	var out map[string]any
	code, err := c.post("/v1/tables", t, &out)
	if code == 400 && out != nil && strings.Contains(fmt.Sprint(out["error"]), "already exists") {
		return
	}
	must(code, err, "create table")
}

// load inserts rows produced by gen in batches from several loaders.
func load(c *client, table string, n, batch, loaders int, gen func(rng *rand.Rand, i int) map[string]any) time.Duration {
	t0 := time.Now()
	var wg sync.WaitGroup
	next := make(chan int, 64)
	go func() {
		for i := 0; i < n; i += batch {
			next <- i
		}
		close(next)
	}()
	var mu sync.Mutex
	done := 0
	for l := 0; l < loaders; l++ {
		wg.Add(1)
		go func(l int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(l)))
			for start := range next {
				ops := make([]map[string]any, 0, batch)
				for i := start; i < min(start+batch, n); i++ {
					ops = append(ops, map[string]any{"op": "put", "table": table, "row": gen(rng, i)})
				}
				var out map[string]any
				code, err := c.post("/v1/write", map[string]any{"ops": ops}, &out)
				if err != nil || code != 200 {
					log.Fatalf("load %s: %d %v %v", table, code, err, out)
				}
				mu.Lock()
				done += len(ops)
				if done%(batch*200) < len(ops) {
					log.Printf("loaded %d/%d %s rows (%.0f rows/s)", done, n, table, float64(done)/time.Since(t0).Seconds())
				}
				mu.Unlock()
			}
		}(l)
	}
	wg.Wait()
	return time.Since(t0)
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime)
	c := newClient(*conc + 16)
	switch *workload {
	case "1":
		if *embedded != "" {
			wl1Embedded(*embedded)
			return
		}
		if *pgURL != "" {
			wl1PG(*pgURL)
			return
		}
		if *redisAddr != "" {
			wl1Redis(*redisAddr)
			return
		}
		wl1(c)
	case "2":
		wl2(c)
	case "3":
		wl3(c)
	case "4":
		wl4(c)
	case "5":
		wl5(c)
	default:
		log.Fatalf("unknown workload %q", *workload)
	}
}

// clientCPU is the client's CPU (cores) during the last run's measured window.
var clientCPU float64

func processCPU() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func check(code int, err error) { must(code, err, "request") }
