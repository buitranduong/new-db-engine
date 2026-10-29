// simfresh measures freshness of STASK derived views (the trigram search view)
// while millions of rows are imported:
//
//	-mode lag   view lag samples (/v1/status: durable_lsn vs substr applied_lsn, lag_ms)
//	            during a bulk import, and read-after-write probes: a row is
//	            written (INSERT / UPDATE) and searched for immediately with
//	            stask.consistency = bounded / strong / eventual / session / (unset: server default).
//	-mode kill  kill -9 staskd in the middle of an import, restart on the same
//	            directory, compare acknowledged writes, core (primary index)
//	            and the view.
//
// It starts and owns its own staskd (ports given by flags). Latencies are
// relative only (shared machine).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	mode      = flag.String("mode", "lag", "lag or kill")
	staskdBin = flag.String("staskd", "/tmp/simrun-bin/staskd", "staskd binary")
	dir       = flag.String("dir", "/tmp/simfresh-data", "data directory (removed at the end)")
	httpAddr  = flag.String("http", "127.0.0.1:28451", "staskd HTTP address")
	pgAddr    = flag.String("pg", "127.0.0.1:28452", "staskd pgwire address")
	nBase     = flag.Int64("sims", 200000, "initial rows")
	nImport   = flag.Int64("rows", 100000, "rows written by the import")
	batch     = flag.Int("batch", 1000, "rows per INSERT..ON CONFLICT statement")
	writers   = flag.Int("writers", 2, "concurrent import writers")
	rounds    = flag.Int("rounds", 3, "kill rounds")
	noView    = flag.Bool("no-view", false, "do not create the trigram view (control)")
	probeSec  = flag.Int("idle-sec", 5, "seconds of idle probing before the import")
	keep      = flag.Bool("keep", false, "keep the data dir")
)

func numStr(i int64) string { return fmt.Sprintf("0%09d", (i*7919+12345)%1000000000) }

var logf *os.File

func startStaskd() (*exec.Cmd, time.Duration) {
	cmd := exec.Command(*staskdBin, "-dir", *dir, "-addr", *httpAddr, "-pg-addr", *pgAddr, "-segment-mb", "64", "-prezero=false")
	logf, _ = os.OpenFile(*dir+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd.Stdout, cmd.Stderr = logf, logf
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		fatal(err)
	}
	for i := 0; i < 600; i++ {
		if _, err := status(); err == nil {
			return cmd, time.Since(t0)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fatal(fmt.Errorf("staskd did not come up"))
	return nil, 0
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(2)
}

type viewSt struct {
	Name       string  `json:"name"`
	Ready      bool    `json:"ready"`
	AppliedLSN int64   `json:"applied_lsn"`
	LagMS      float64 `json:"lag_ms"`
}
type statusT struct {
	DurableLSN int64    `json:"durable_lsn"`
	Views      []viewSt `json:"views"`
}

func status() (statusT, error) {
	var s statusT
	cl := http.Client{Timeout: 2 * time.Second}
	r, err := cl.Get("http://" + *httpAddr + "/v1/status")
	if err != nil {
		return s, err
	}
	defer r.Body.Close()
	return s, json.NewDecoder(r.Body).Decode(&s)
}

func substrView(s statusT) viewSt {
	for _, v := range s.Views {
		if v.Name == "substr" {
			return v
		}
	}
	return viewSt{}
}

func dsn() string { return "postgres://stask@" + *pgAddr + "/stask?sslmode=disable" }

func connect(ctx context.Context) *pgx.Conn {
	cc, err := pgx.ParseConfig(dsn())
	if err != nil {
		fatal(err)
	}
	cc.DefaultQueryExecMode = pgx.QueryExecModeExec
	for i := 0; i < 100; i++ {
		c, err := pgx.ConnectConfig(ctx, cc)
		if err == nil {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	fatal(fmt.Errorf("cannot connect"))
	return nil
}

func exec1(ctx context.Context, c *pgx.Conn, q string, a ...any) error {
	_, err := c.Exec(ctx, q, a...)
	return err
}

func setup(ctx context.Context) {
	c := connect(ctx)
	defer c.Close(ctx)
	must(exec1(ctx, c, `CREATE TABLE sims (number text PRIMARY KEY, carrier text, status text, price bigint, updated_at bigint)`))
	t0 := time.Now()
	pr, pw := pipeCSV(*nBase)
	tag, err := c.PgConn().CopyFrom(ctx, pr, "COPY sims(number,carrier,status,price) FROM STDIN WITH (FORMAT csv)")
	must(err)
	fmt.Printf("setup: loaded %d rows in %v\n", tag.RowsAffected(), time.Since(t0).Round(time.Millisecond))
	_ = pw
	if !*noView {
		t0 = time.Now()
		must(exec1(ctx, c, `CREATE INDEX sims_trgm ON sims USING gin (number gin_trgm_ops)`))
		fmt.Printf("setup: trigram view created in %v\n", time.Since(t0).Round(time.Millisecond))
		waitCaughtUp(30 * time.Second)
	}
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

type pipeR struct {
	r *os.File
}

func pipeCSV(n int64) (*os.File, *os.File) {
	r, w, _ := os.Pipe()
	go func() {
		bw := bufio.NewWriterSize(w, 1<<16)
		for i := int64(0); i < n; i++ {
			fmt.Fprintf(bw, "%s,viettel,available,%d\n", numStr(i), 100000+(i%500)*10000)
		}
		bw.Flush()
		w.Close()
	}()
	return r, w
}

func waitCaughtUp(max time.Duration) (time.Duration, bool) {
	t0 := time.Now()
	for time.Since(t0) < max {
		s, err := status()
		if err == nil {
			if v := substrView(s); v.Ready && v.AppliedLSN >= s.DurableLSN {
				return time.Since(t0), true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return max, false
}

// ---------------------------------------------------------------- stats

func quant(d []float64, q float64) float64 {
	if len(d) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), d...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

type sampler struct {
	mu       sync.Mutex
	gap      []float64 // durable_lsn - applied_lsn
	lagMS    []float64
	phaseTag string
	byPhase  map[string][2][]float64
}

func (s *sampler) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		st, err := status()
		if err != nil {
			continue
		}
		v := substrView(st)
		s.mu.Lock()
		p := s.byPhase[s.phaseTag]
		p[0] = append(p[0], float64(st.DurableLSN-v.AppliedLSN))
		p[1] = append(p[1], v.LagMS)
		s.byPhase[s.phaseTag] = p
		s.mu.Unlock()
	}
}

func (s *sampler) phase(n string) { s.mu.Lock(); s.phaseTag = n; s.mu.Unlock() }

// ---------------------------------------------------------------- probes

type probeRes struct {
	n, found, stale int
	lat             []float64
	engines         map[string]int
	visMS           []float64 // eventual: time until visible
	notVisible      int
}

func lastPlanEngine(ctx context.Context, c *pgx.Conn) string {
	var s string
	if err := c.QueryRow(ctx, `SHOW stask.last_plan`).Scan(&s); err != nil {
		return "?"
	}
	if i := strings.Index(s, "engine="); i >= 0 {
		e := s[i+7:]
		if j := strings.IndexAny(e, " \n"); j >= 0 {
			e = e[:j]
		}
		return e
	}
	return "none"
}

var probeSeq atomic.Int64

// probe: write a new row (autocommit, acknowledged), then search it by its
// full number through the trigram pattern. mode = bounded|strong|eventual.
func probeInsert(ctx context.Context, c *pgx.Conn, consistency string, r *probeRes) {
	n := probeSeq.Add(1)
	num := fmt.Sprintf("7%09d", n)
	price := int64(1000 + n)
	if err := exec1(ctx, c, `INSERT INTO sims (number,carrier,status,price,updated_at) VALUES ($1,'probe','available',$2,0)`, num, price); err != nil {
		return
	}
	t0 := time.Now()
	r.n++
	var got int64
	pat := "%" + num + "%"
	err := c.QueryRow(ctx, `SELECT price FROM sims WHERE number LIKE $1`, pat).Scan(&got)
	r.lat = append(r.lat, float64(time.Since(t0).Microseconds())/1000)
	r.engines[lastPlanEngine(ctx, c)]++
	if err == nil {
		r.found++
		return
	}
	if consistency != "eventual" {
		return
	}
	for time.Since(t0) < 3*time.Second {
		time.Sleep(time.Millisecond)
		if c.QueryRow(ctx, `SELECT price FROM sims WHERE number LIKE $1`, pat).Scan(&got) == nil {
			r.visMS = append(r.visMS, float64(time.Since(t0).Microseconds())/1000)
			return
		}
	}
	r.notVisible++
}

// probeUpdate: change the price of an existing indexed row and search for it
// with a pattern: the answer must carry the new price (candidates are re-read
// from the core).
func probeUpdate(ctx context.Context, c *pgx.Conn, rng *rand.Rand, r *probeRes) {
	i := rng.Int64N(*nBase)
	num := numStr(i)
	price := 5_000_000 + rng.Int64N(1_000_000)
	if err := exec1(ctx, c, `UPDATE sims SET price=$1 WHERE number=$2`, price, num); err != nil {
		return
	}
	t0 := time.Now()
	var got int64
	err := c.QueryRow(ctx, `SELECT price FROM sims WHERE number LIKE $1`, "%"+num+"%").Scan(&got)
	r.lat = append(r.lat, float64(time.Since(t0).Microseconds())/1000)
	r.engines[lastPlanEngine(ctx, c)]++
	r.n++
	if err == nil && got == price {
		r.found++
	} else {
		r.stale++
	}
}

func probeLoop(ctx context.Context, consistency string, kind string, out *probeRes, stopAt <-chan struct{}, seed uint64) {
	c := connect(ctx)
	defer c.Close(context.Background())
	if consistency != "default" {
		_ = exec1(ctx, c, "SET stask.consistency = '"+consistency+"'")
	}
	rng := rand.New(rand.NewPCG(seed, 1))
	for {
		select {
		case <-stopAt:
			return
		case <-ctx.Done():
			return
		default:
		}
		if kind == "insert" {
			probeInsert(ctx, c, consistency, out)
		} else {
			probeUpdate(ctx, c, rng, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- import

var batchSeq atomic.Int64

// importer: INSERT..ON CONFLICT batches. Numbers of batch b of writer w are
// deterministic: 5xxxxxxxxx range (new rows) so that acked batches can be verified.
func batchNums(w, b int64) (lo int64, nums []string) {
	lo = 500_000_000 + w*50_000_000 + b*int64(*batch)
	for i := int64(0); i < int64(*batch); i++ {
		nums = append(nums, fmt.Sprintf("0%09d", lo+i))
	}
	return
}

func runImporter(ctx context.Context, w int64, perWriter int64, acked *[]int64, mu *sync.Mutex, stopNow <-chan struct{}) (rows int64, errs int) {
	c := connect(ctx)
	defer c.Close(context.Background())
	var b int64
	for rows < perWriter {
		select {
		case <-stopNow:
			return
		default:
		}
		_, nums := batchNums(w, b)
		var sb strings.Builder
		sb.WriteString(`INSERT INTO sims (number,carrier,status,price,updated_at) VALUES `)
		args := make([]any, 0, len(nums)*3)
		for i, n := range nums {
			if i > 0 {
				sb.WriteByte(',')
			}
			k := i * 3
			fmt.Fprintf(&sb, "($%d,$%d,'available',$%d,1)", k+1, k+2, k+3)
			args = append(args, n, "mobifone", int64(777000)+b)
		}
		sb.WriteString(` ON CONFLICT (number) DO UPDATE SET price=excluded.price, carrier=excluded.carrier, updated_at=1`)
		if err := exec1(ctx, c, sb.String(), args...); err != nil {
			errs++
			if ctx.Err() != nil {
				return
			}
			if errs > 50 {
				return
			}
			continue
		}
		mu.Lock()
		*acked = append(*acked, w*1_000_000+b)
		mu.Unlock()
		rows += int64(len(nums))
		b++
	}
	return
}

// ---------------------------------------------------------------- modes

func modeLag() {
	ctx := context.Background()
	os.RemoveAll(*dir)
	cmd, _ := startStaskd()
	defer func() {
		_ = cmd.Process.Kill()
		cmd.Wait()
		logf.Close()
		if !*keep {
			os.RemoveAll(*dir)
			os.Remove(*dir + ".log")
		}
	}()
	setup(ctx)

	sp := &sampler{byPhase: map[string][2][]float64{}}
	sctx, scancel := context.WithCancel(ctx)
	go sp.run(sctx)

	kinds := []struct{ name, cons, kind string }{
		{"insert/bounded", "bounded", "insert"},
		{"insert/strong", "strong", "insert"},
		{"insert/eventual", "eventual", "insert"},
		{"insert/session", "session", "insert"},
		{"insert/default", "default", "insert"}, // no SET: the server default (D26: session)
		{"update/bounded", "bounded", "update"},
		{"update/strong", "strong", "update"},
		{"update/default", "default", "update"},
	}
	runProbes := func(phase string, fn func()) map[string]*probeRes {
		res := map[string]*probeRes{}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i, k := range kinds {
			r := &probeRes{engines: map[string]int{}}
			res[k.name] = r
			wg.Add(1)
			go func(i int, k struct{ name, cons, kind string }) {
				defer wg.Done()
				probeLoop(ctx, k.cons, k.kind, r, stop, uint64(i)+1)
			}(i, k)
		}
		sp.phase(phase)
		fn()
		close(stop)
		wg.Wait()
		return res
	}
	idle := runProbes("idle", func() { time.Sleep(time.Duration(*probeSec) * time.Second) })

	// import
	var acked []int64
	var mu sync.Mutex
	var importDur time.Duration
	var totalRows int64
	imp := runProbes("import", func() {
		t0 := time.Now()
		var wg sync.WaitGroup
		var rows atomic.Int64
		for w := int64(0); w < int64(*writers); w++ {
			wg.Add(1)
			go func(w int64) {
				defer wg.Done()
				n, _ := runImporter(ctx, w, *nImport/int64(*writers), &acked, &mu, nil)
				rows.Add(n)
			}(w)
		}
		wg.Wait()
		importDur = time.Since(t0)
		totalRows = rows.Load()
	})
	catch, ok := waitCaughtUp(60 * time.Second)
	sp.phase("after")
	time.Sleep(time.Second)
	scancel()

	fmt.Printf("\n== mode lag: %d base rows, import %d rows by %d writers (batch %d) in %.2fs = %.0f rows/s; view caught up %.3fs after the import returned (ok=%v)\n",
		*nBase, totalRows, *writers, *batch, importDur.Seconds(), float64(totalRows)/importDur.Seconds(), catch.Seconds(), ok)
	fmt.Println("\n-- view lag samples every 100 ms (/v1/status: durable_lsn - substr applied_lsn, lag_ms)")
	fmt.Printf("%-8s %6s %12s %12s %12s %12s %12s\n", "phase", "n", "gap p50", "gap p99", "gap max", "lag_ms p99", "lag_ms max")
	for _, ph := range []string{"idle", "import", "after"} {
		v := sp.byPhase[ph]
		fmt.Printf("%-8s %6d %12.0f %12.0f %12.0f %12.1f %12.1f\n", ph, len(v[0]), quant(v[0], .5), quant(v[0], .99), quant(v[0], 1), quant(v[1], .99), quant(v[1], 1))
	}
	fmt.Println("\n-- read-after-write probes (write acknowledged, then search at once by trigram LIKE '%<full number>%')")
	fmt.Printf("%-8s %-17s %6s %9s %7s %9s %9s  %s\n", "phase", "probe", "n", "seen-now", "stale", "lat p50", "lat p99", "plans")
	for _, set := range []struct {
		ph string
		m  map[string]*probeRes
	}{{"idle", idle}, {"import", imp}} {
		for _, k := range kinds {
			r := set.m[k.name]
			e := []string{}
			for en, n := range r.engines {
				e = append(e, fmt.Sprintf("%s=%d", en, n))
			}
			sort.Strings(e)
			extra := ""
			if k.cons == "eventual" && k.kind == "insert" {
				extra = fmt.Sprintf("  visible after p50 %.1f / p99 %.1f / max %.1f ms, never within 3s: %d", quant(r.visMS, .5), quant(r.visMS, .99), quant(r.visMS, 1), r.notVisible)
			}
			fmt.Printf("%-8s %-17s %6d %9d %7d %8.2fms %8.2fms  %s%s\n", set.ph, k.name, r.n, r.found, r.stale, quant(r.lat, .5), quant(r.lat, .99), strings.Join(e, " "), extra)
		}
	}
	// final consistency
	c := connect(ctx)
	defer c.Close(ctx)
	fmt.Println("\n-- after the import")
	compare(ctx, c, acked)
}

// compare: acked batches present in core, core count, view count under strong.
func compare(ctx context.Context, c *pgx.Conn, acked []int64) (missing int) {
	var core int64
	must(c.QueryRow(ctx, `SELECT count(*) FROM sims`).Scan(&core))
	planCore := lastPlanEngine(ctx, c)
	_ = exec1(ctx, c, `SET stask.consistency = 'strong'`)
	var viewN int64
	err := c.QueryRow(ctx, `SELECT count(*) FROM sims WHERE number LIKE '0%'`).Scan(&viewN)
	planView := lastPlanEngine(ctx, c)
	_ = exec1(ctx, c, `SET stask.consistency = 'bounded'`)
	// numbers not starting with 0 (probe rows '7...') are not in the view query
	var nonZero int64
	must(c.QueryRow(ctx, `SELECT count(*) FROM sims WHERE number >= '1'`).Scan(&nonZero))
	// every row written by an acknowledged batch carries updated_at = 1 (new numbers only)
	var marked int64
	must(c.QueryRow(ctx, `SELECT count(*) FROM sims WHERE updated_at = 1`).Scan(&marked))
	if want := int64(len(acked)) * int64(*batch); marked < want {
		missing = int(want - marked)
	}
	fmt.Printf("core count(*) = %d (plan engine=%s); view count (LIKE '0%%', strong) = %d (plan engine=%s, err=%v); core rows not starting with 0 = %d\n", core, planCore, viewN, planView, err, nonZero)
	fmt.Printf("acknowledged batches: %d (%d rows); rows written by the import found in core: %d (extra over acked = batches in flight at the kill); acked rows missing from core: %d; core-view difference (core - nonzero - view) = %d\n", len(acked), int64(len(acked))*int64(*batch), marked, missing, core-nonZero-viewN)
	return
}

func modeKill() {
	ctx := context.Background()
	os.RemoveAll(*dir)
	cmd, _ := startStaskd()
	setup(ctx)
	var acked []int64
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 5))
	fmt.Printf("== mode kill: %d base rows, %d rounds, %d writers, batch %d, view=%v\n", *nBase, *rounds, *writers, *batch, !*noView)
	bad := 0
	var nextBatch int64
	_ = nextBatch
	for round := 1; round <= *rounds; round++ {
		wctx, wcancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for w := int64(0); w < int64(*writers); w++ {
			wg.Add(1)
			go func(w int64) {
				defer wg.Done()
				// each round continues numbering: use round as part of writer id
				runImporter(wctx, w+int64(round-1)*int64(*writers), 1<<40, &acked, &mu, nil)
			}(w)
		}
		d := time.Duration(1500+rng.IntN(2500)) * time.Millisecond
		time.Sleep(d)
		pre, _ := status()
		t0 := time.Now()
		_ = cmd.Process.Kill() // SIGKILL
		cmd.Wait()
		wcancel()
		wg.Wait()
		mu.Lock()
		nAck := len(acked)
		mu.Unlock()
		fmt.Printf("\nround %d: kill -9 after %.1fs of import, %d batches acknowledged (%d rows), durable_lsn %d / substr applied_lsn %d just before the kill\n",
			round, d.Seconds(), nAck, nAck**batch, pre.DurableLSN, substrView(pre).AppliedLSN)
		var up time.Duration
		cmd, up = startStaskd()
		_ = t0
		st0, _ := status()
		fmt.Printf("  restart to ready: %.2fs; right after: durable_lsn %d, substr ready=%v applied_lsn %d\n", up.Seconds(), st0.DurableLSN, substrView(st0).Ready, substrView(st0).AppliedLSN)
		cu, ok := waitCaughtUp(120 * time.Second)
		fmt.Printf("  view caught up to durable_lsn %.3fs after ready (ok=%v)\n", cu.Seconds(), ok)
		c := connect(ctx)
		missing := compare(ctx, c, acked)
		c.Close(ctx)
		if missing > 0 {
			bad++
		}
	}
	_ = cmd.Process.Kill()
	cmd.Wait()
	logf.Close()
	if !*keep {
		os.RemoveAll(*dir)
		os.Remove(*dir + ".log")
	}
	if bad > 0 {
		fmt.Printf("\nRESULT INVALID: %d round(s) lost acknowledged rows\n", bad)
		os.Exit(1)
	}
	fmt.Println("\nRESULT VALID: no acknowledged row lost in any round")
}

func main() {
	flag.Parse()
	var _ = pgxpool.Config{}
	switch *mode {
	case "lag":
		modeLag()
	case "kill":
		modeKill()
	default:
		fatal(fmt.Errorf("unknown mode"))
	}
}
