// staskmix runs every class of work at once against one table over the
// PostgreSQL protocol: point reads, short range reads, single-row updates,
// inserts and deletes, multi-statement transactions, a bulk loader and an
// analytic aggregate. Each class is offered a fixed rate (open loop, latency
// measured from the intended start), so STASK and PostgreSQL get the same
// load and every class is reported on its own: a system that is fast at one
// class by starving the others shows it here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"stask/internal/benchcheck"
)

var statuses = []string{"new", "paid", "shipped", "cancelled"}

// row i is deterministic so both systems store the same data.
func rowArgs(i int64, args []any) []any {
	r := rand.New(rand.NewPCG(uint64(i), 42))
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
	note := fmt.Sprintf("Đơn hàng số %d của khách %d, giao tới quận %d", i, r.IntN(1_000_000), r.IntN(24)+1)
	return append(args, i, int64(r.IntN(1_000_000)), math.Round(r.Float64()*1e6)/100,
		statuses[r.IntN(len(statuses))], note, created)
}

func insertSQL(lo, hi int64) (string, []any) {
	var b strings.Builder
	b.WriteString("INSERT INTO orders (id, customer_id, amount, status, note, created) VALUES ")
	args := make([]any, 0, 6*(hi-lo))
	for i := lo; i < hi; i++ {
		if i > lo {
			b.WriteByte(',')
		}
		k := len(args)
		fmt.Fprintf(&b, "($%d,$%d,$%d,$%d,$%d,$%d)", k+1, k+2, k+3, k+4, k+5, k+6)
		args = rowArgs(i, args)
	}
	return b.String(), args
}

type class struct {
	name    string
	rate    float64 // operations/s offered
	workers int
	op      func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) // rows touched

	mu      sync.Mutex
	samples []sample
	errs    map[string]int
	maxQ    atomic.Int64 // deepest backlog seen by the dispatcher
	retries atomic.Int64
	rows    atomic.Int64
	dropped atomic.Int64
	// offered during the run but still queued at the end: the class could
	// not keep up with its rate
	unfinished atomic.Int64
	empty      atomic.Int64 // reads that found nothing (must stay 0 for stable rows)
	measured   atomic.Bool
}

// sample is one operation: total is from its intended start (open loop),
// wait the part spent before it began executing (backlog in this client and
// waiting for a pooled connection).
type sample struct {
	start       time.Time
	total, wait time.Duration
	acq         time.Duration // part of wait spent in pool.Acquire
}

// spans records when analytic aggregates were executing, so every class
// can be reported separately for operations that overlapped one.
var spans struct {
	mu sync.Mutex
	iv [][2]time.Time
}

func overlapsAgg(s sample) bool {
	end := s.start.Add(s.total)
	for _, iv := range spans.iv {
		if s.start.Before(iv[1]) && end.After(iv[0]) {
			return true
		}
	}
	return false
}

func pct(d []time.Duration) func(float64) float64 {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return func(p float64) float64 {
		if len(d) == 0 {
			return math.NaN()
		}
		return float64(d[min(int(math.Ceil(p*float64(len(d))))-1, len(d)-1)].Microseconds()) / 1000
	}
}

func (c *class) record(sm sample, err error) {
	if !c.measured.Load() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		k := err.Error()
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			k = pe.Code + " " + pe.Message
		}
		if c.errs == nil {
			c.errs = map[string]int{}
		}
		c.errs[k]++
		return
	}
	c.samples = append(c.samples, sm)
}

func (c *class) summary(secs float64) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := map[string]any{"class": c.name, "target_per_s": c.rate, "done": len(c.samples), "per_s": float64(len(c.samples)) / secs,
		"rows_per_s": float64(c.rows.Load()) / secs, "errors": c.errs, "retries": c.retries.Load(),
		"dropped": c.dropped.Load(), "unfinished": c.unfinished.Load(), "empty": c.empty.Load(), "max_queue": c.maxQ.Load()}
	if len(c.samples) == 0 {
		return s
	}
	var total, wait, exec, acq, in, out []time.Duration
	for _, sm := range c.samples {
		total = append(total, sm.total)
		wait = append(wait, sm.wait)
		acq = append(acq, sm.acq)
		exec = append(exec, sm.total-sm.wait)
		if c.name != "agg" && overlapsAgg(sm) {
			in = append(in, sm.total)
		} else {
			out = append(out, sm.total)
		}
	}
	q := pct(total)
	s["p50_ms"], s["p99_ms"], s["p999_ms"], s["max_ms"] = q(0.5), q(0.99), q(0.999), q(1)
	qw, qe := pct(wait), pct(exec)
	s["wait_p50_ms"], s["wait_p99_ms"], s["exec_p50_ms"], s["exec_p99_ms"] = qw(0.5), qw(0.99), qe(0.5), qe(0.99)
	s["exec_p999_ms"], s["exec_max_ms"] = qe(0.999), qe(1)
	qa := pct(acq)
	s["acquire_p99_ms"], s["acquire_max_ms"] = qa(0.99), qa(1)
	// per-second worst execution time and worst total, by intended start
	// (unix seconds): finds a stall and lets it be lined up with server logs
	type sec struct{ exec, total, acq time.Duration }
	tl := map[int64]*sec{}
	for _, sm := range c.samples {
		k := sm.start.Unix()
		x := tl[k]
		if x == nil {
			x = &sec{}
			tl[k] = x
		}
		x.exec, x.total, x.acq = max(x.exec, sm.total-sm.wait), max(x.total, sm.total), max(x.acq, sm.acq)
	}
	var line, aline []string
	for _, k := range slices.Sorted(maps.Keys(tl)) {
		ts := time.Unix(k, 0).Format("15:04:05")
		line = append(line, fmt.Sprintf("%s %.0f/%.0f", ts, float64(tl[k].exec.Microseconds())/1000, float64(tl[k].total.Microseconds())/1000))
		aline = append(aline, fmt.Sprintf("%s %.1f", ts, float64(tl[k].acq.Microseconds())/1000))
	}
	s["timeline_exec_total_max_ms"] = strings.Join(line, ", ")
	s["timeline_acquire_max_ms"] = strings.Join(aline, ", ")
	if len(in) > 0 && len(out) > 0 {
		qi, qo := pct(in), pct(out)
		s["during_agg_n"], s["during_agg_p99_ms"] = len(in), qi(0.99)
		s["no_agg_n"], s["no_agg_p99_ms"] = len(out), qo(0.99)
	}
	return s
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}

func main() {
	url := flag.String("url", "postgres://app@127.0.0.1:17432/stask?sslmode=disable", "connection URL")
	n := flag.Int64("n", 2_000_000, "rows loaded before the mix")
	load := flag.Bool("load", true, "create and load the table first")
	dur := flag.Duration("dur", 30*time.Second, "measured duration")
	warm := flag.Duration("warm", 3*time.Second, "unmeasured warm-up")
	scale := flag.Float64("scale", 1, "multiplies every rate")
	only := flag.String("only", "", "comma-separated classes to run (default all)")
	rates := flag.String("rates", "", "per-class rate overrides, e.g. read=40000,agg=1")
	label := flag.String("label", "", "label")
	aggURL := flag.String("agg-url", "", "connection URL for the agg class (a replica); default -url")
	with := flag.String("with", "", "table options for CREATE TABLE, e.g. \"columnar = true\" (STASK)")
	serverPid := flag.Int("server-pid", 0, "also sample this process's CPU use per second (Linux), next to this client's")
	keybase := flag.Int64("keybase", 0, "probe mode: inserts use ids from keybase up and deletes remove those rows again, so a second process does not touch the main run's insert/delete keys")
	bo := benchcheck.Register(flag.CommandLine)
	flag.Parse()
	hc := benchcheck.New("staskmix", bo, flag.CommandLine)
	hc.CheckBuild()
	hc.Abort(map[string]any{"label": *label})
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, fmt.Sprintf("%s&pool_max_conns=%d", *url, int(300*max(*scale, 1))+20))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	aggPool := pool
	if *aggURL != "" {
		if aggPool, err = pgxpool.New(ctx, *aggURL+"&pool_max_conns=20"); err != nil {
			log.Fatal(err)
		}
		defer aggPool.Close()
	}

	if *load {
		// a leftover table is dropped; an empty one was created by the
		// calling script with its own options (WITH (columnar = true))
		var rows int64
		if pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&rows) == nil && rows > 0 {
			pool.Exec(ctx, `DROP TABLE orders`)
		}
		if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL,
			amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ)`+withClause(*with)); err != nil {
			log.Fatal(err)
		}
		t0 := time.Now()
		var next atomic.Int64
		var wg sync.WaitGroup
		for w := 0; w < 16; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					lo := next.Add(1000) - 1000
					if lo >= *n {
						return
					}
					q, args := insertSQL(lo, min(lo+1000, *n))
					if _, err := pool.Exec(ctx, q, args...); err != nil {
						log.Fatal(err)
					}
				}
			}()
		}
		wg.Wait()
		pool.Exec(ctx, `ANALYZE orders`) // PostgreSQL; STASK ignores or rejects it
		log.Printf("loaded %d rows in %v", *n, time.Since(t0).Round(time.Millisecond))
	}

	// self-check (docs/14): the table must still be what the run declares
	// (e.g. columnar, so the agg class measures the columnar engine). The
	// default expects the aggregate to be served by the columnar view when
	// the agg class runs; a row-path run says -expect orders,row.
	{
		spec := fmt.Sprintf("orders,min=%d,max=%d", *n*8/10, *n*3)
		if (*only == "" || strings.Contains(","+*only+",", ",agg,")) && !strings.Contains(*rates, "agg=0") {
			spec = fmt.Sprintf("orders,agg,min=%d,max=%d", *n*8/10, *n*3)
		}
		hc.DefaultExpect(spec)
		hc.Preconditions("before")
		hc.Abort(map[string]any{"label": *label, "rows": *n})
	}

	// key space: [0, stable) is never deleted (reads, ranges, updates,
	// transactions); [stable, n) is consumed by deletes from the top; new
	// rows go above n (single inserts) and above 1e12 (bulk loader).
	stable := *n * 8 / 10
	var delNext atomic.Int64
	delNext.Store(*n)
	var insNext atomic.Int64
	insNext.Store(*n)
	var probeDel atomic.Int64 // probe mode: next own row to delete
	if *keybase > 0 {
		insNext.Store(*keybase)
		probeDel.Store(*keybase)
	}
	var bulkNext atomic.Int64
	bulkNext.Store(1_000_000_000_000)
	if !*load && *keybase == 0 {
		// a later run on the same table continues where the earlier ones
		// stopped: inserts above the highest id, deletes below the lowest
		// row left in the delete range (else inserts hit existing keys and
		// deletes find nothing to delete)
		var top, del, bulk *int64
		if err := pool.QueryRow(ctx, `SELECT max(id) FROM orders WHERE id < $1`, int64(1_000_000_000_000)).Scan(&top); err != nil {
			log.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT max(id) FROM orders WHERE id >= $1 AND id < $2`, stable, *n).Scan(&del); err != nil {
			log.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT max(id) FROM orders WHERE id >= $1`, int64(1_000_000_000_000)).Scan(&bulk); err != nil {
			log.Fatal(err)
		}
		if top != nil && *top >= *n {
			insNext.Store(*top + 1)
		}
		delNext.Store(stable)
		if del != nil {
			delNext.Store(*del + 1)
		}
		if bulk != nil {
			bulkNext.Store(*bulk + 1)
		}
		log.Printf("resume: inserts from %d, deletes below %d, bulk from %d", insNext.Load(), delNext.Load(), bulkNext.Load())
	}

	classes := []*class{
		{name: "read", rate: 20000, workers: 48, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			var amount float64
			var status string
			err := c.QueryRow(ctx, `SELECT amount, status FROM orders WHERE id = $1`, r.Int64N(stable)).Scan(&amount, &status)
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, pgx.ErrNoRows
			}
			return 1, err
		}},
		{name: "range", rate: 1000, workers: 16, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			lo := r.Int64N(stable - 100)
			rows, err := c.Query(ctx, `SELECT id, amount FROM orders WHERE id >= $1 AND id < $2 ORDER BY id`, lo, lo+50)
			if err != nil {
				return 0, err
			}
			k := 0
			for rows.Next() {
				k++
			}
			return k, rows.Err()
		}},
		{name: "update", rate: 8000, workers: 96, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			tag, err := c.Exec(ctx, `UPDATE orders SET amount = amount + 1, status = 'paid' WHERE id = $1`, r.Int64N(stable))
			return int(tag.RowsAffected()), err
		}},
		{name: "insert", rate: 4000, workers: 48, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			i := insNext.Add(1) - 1
			q, args := insertSQL(i, i+1)
			tag, err := c.Exec(ctx, q, args...)
			return int(tag.RowsAffected()), err
		}},
		{name: "delete", rate: 2000, workers: 24, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			if *keybase > 0 {
				i := probeDel.Add(1) - 1
				if i >= insNext.Load() { // nothing of ours left to delete yet
					probeDel.Add(-1)
					return 0, nil
				}
				tag, err := c.Exec(ctx, `DELETE FROM orders WHERE id = $1`, i)
				return int(tag.RowsAffected()), err
			}
			i := delNext.Add(-1)
			if i < stable {
				return 0, nil
			}
			tag, err := c.Exec(ctx, `DELETE FROM orders WHERE id = $1`, i)
			return int(tag.RowsAffected()), err
		}},
		{name: "tx", rate: 1000, workers: 32, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			// transfer: read both, move an amount, all or nothing
			a, b := r.Int64N(stable), r.Int64N(stable)
			if a == b {
				b = (a + 1) % stable
			}
			tx, err := c.Begin(ctx)
			if err != nil {
				return 0, err
			}
			defer tx.Rollback(ctx)
			var va, vb float64
			if err := tx.QueryRow(ctx, `SELECT amount FROM orders WHERE id = $1`, a).Scan(&va); err != nil {
				return 0, err
			}
			if err := tx.QueryRow(ctx, `SELECT amount FROM orders WHERE id = $1`, b).Scan(&vb); err != nil {
				return 0, err
			}
			if _, err := tx.Exec(ctx, `UPDATE orders SET amount = $1 WHERE id = $2`, va-1, a); err != nil {
				return 0, err
			}
			if _, err := tx.Exec(ctx, `UPDATE orders SET amount = $1 WHERE id = $2`, vb+1, b); err != nil {
				return 0, err
			}
			return 2, tx.Commit(ctx)
		}},
		{name: "bulk", rate: 20, workers: 4, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			lo := bulkNext.Add(1000) - 1000
			q, args := insertSQL(lo, lo+1000)
			tag, err := c.Exec(ctx, q, args...)
			return int(tag.RowsAffected()), err
		}},
		{name: "page", rate: 0, workers: 32, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			// one web page: a 100-product listing, 25 product details and
			// 4 related-product blocks, 30 queries one after another
			lo := r.Int64N(stable - 200)
			rows, err := c.Query(ctx, `SELECT id, customer_id, amount, status FROM orders WHERE id >= $1 AND id < $2 ORDER BY id`, lo, lo+100)
			if err != nil {
				return 0, err
			}
			k := 0
			for rows.Next() {
				k++
			}
			if err := rows.Err(); err != nil {
				return k, err
			}
			for i := 0; i < 25; i++ {
				var amount float64
				var status, note string
				if err := c.QueryRow(ctx, `SELECT amount, status, note FROM orders WHERE id = $1`, lo+r.Int64N(100)).Scan(&amount, &status, &note); err != nil {
					return k, err
				}
				k++
			}
			for i := 0; i < 4; i++ {
				x := r.Int64N(stable - 20)
				rows, err := c.Query(ctx, `SELECT id, amount FROM orders WHERE id >= $1 AND id < $2 ORDER BY id`, x, x+20)
				if err != nil {
					return k, err
				}
				for rows.Next() {
					k++
				}
				if err := rows.Err(); err != nil {
					return k, err
				}
			}
			return k, nil
		}},
		{name: "agg", rate: 0.5, workers: 2, op: func(ctx context.Context, c *pgxpool.Conn, r *rand.Rand) (int, error) {
			rows, err := c.Query(ctx, `SELECT status, count(*), sum(amount) FROM orders GROUP BY status`)
			if err != nil {
				return 0, err
			}
			k := 0
			for rows.Next() {
				k++
			}
			return k, rows.Err()
		}},
	}
	want := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s != "" {
			want[s] = true
		}
	}
	over := map[string]float64{}
	for _, kv := range strings.Split(*rates, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				log.Fatal(err)
			}
			over[k] = f
		}
	}
	var active []*class
	for _, c := range classes {
		if r, ok := over[c.name]; ok {
			c.rate = r
		}
		c.rate *= *scale
		// closed workers cap throughput at workers/latency: grow them with
		// the load so overload shows up as server latency, not as a
		// client that cannot send; the probe's low rates need few
		c.workers = int(math.Ceil(float64(c.workers) * max(*scale, 1)))
		if *keybase > 0 {
			c.workers = min(c.workers, 16)
		}
		if (len(want) == 0 || want[c.name]) && c.rate > 0 {
			active = append(active, c)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for ci, c := range active {
		queue := make(chan time.Time, 1<<16)
		// dispatcher: one intended start per 1/rate, regardless of progress
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(queue)
			interval := time.Duration(float64(time.Second) / c.rate)
			next := time.Now()
			for {
				if d := time.Until(next); d > 0 {
					select {
					case <-stop:
						return
					case <-time.After(d):
					}
				}
				select {
				case <-stop:
					return
				default:
				}
				c.maxQ.Store(max(c.maxQ.Load(), int64(len(queue))))
				for ; !next.After(time.Now()); next = next.Add(interval) {
					select {
					case queue <- next:
					default:
						c.dropped.Add(1)
					}
				}
			}
		}()
		for w := 0; w < c.workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(ci*1000+w), 7))
				for start := range queue {
					select {
					case <-stop: // backlog left at the end is not run
						c.unfinished.Add(1)
						continue
					default:
					}
					p := pool
					if c.name == "agg" {
						p = aggPool
					}
					acqStart := time.Now()
					conn, err := p.Acquire(ctx)
					if err != nil {
						c.record(sample{}, err)
						continue
					}
					began := time.Now()
					var k int
					for {
						k, err = c.op(ctx, conn, r)
						if retryable(err) {
							c.retries.Add(1)
							continue
						}
						break
					}
					conn.Release()
					if c.name == "agg" {
						spans.mu.Lock()
						spans.iv = append(spans.iv, [2]time.Time{began, time.Now()})
						spans.mu.Unlock()
					}
					if errors.Is(err, pgx.ErrNoRows) {
						c.empty.Add(1)
						err = nil
					}
					c.record(sample{start, time.Since(start), began.Sub(start), began.Sub(acqStart)}, err)
					if err == nil && c.measured.Load() {
						c.rows.Add(int64(k))
					}
				}
			}(w)
		}
	}
	time.Sleep(*warm)
	for _, c := range active {
		c.measured.Store(true)
	}
	t0 := time.Now()
	cpu := sampleCPU(*serverPid, stop)
	time.Sleep(*dur)
	for _, c := range active {
		c.measured.Store(false)
	}
	secs := time.Since(t0).Seconds()
	close(stop)
	wg.Wait()

	var out []map[string]any
	for _, c := range active {
		s := c.summary(secs)
		out = append(out, s)
		log.Printf("%-7s target %8.1f/s done %8.1f/s p50 %7v p99 %7v p999 %7v max %7v err %v drop %d empty %d retry %d",
			c.name, c.rate, s["per_s"], s["p50_ms"], s["p99_ms"], s["p999_ms"], s["max_ms"], c.errs, c.dropped.Load(), c.empty.Load(), c.retries.Load())
		log.Printf("%-7s   wait p50 %v p99 %v | exec p50 %v p99 %v | during agg p99 %v (n=%v) | no agg p99 %v (n=%v) | max queue %d",
			c.name, s["wait_p50_ms"], s["wait_p99_ms"], s["exec_p50_ms"], s["exec_p99_ms"], s["during_agg_p99_ms"], s["during_agg_n"],
			s["no_agg_p99_ms"], s["no_agg_n"], c.maxQ.Load())
	}
	var cnt int64
	pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&cnt)
	cpuLine := strings.Join(<-cpu, ", ")
	log.Printf("cpu per second (client/server cores busy): %s", cpuLine)
	hc.Preconditions("after")
	res := map[string]any{"label": *label, "rows": *n, "secs": secs, "classes": out, "final_count": cnt,
		"timeline_cpu_client_server": cpuLine}
	code := hc.Finish(res)
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	os.Exit(code)
}

func withClause(opts string) string {
	if opts == "" {
		return ""
	}
	return " WITH (" + opts + ")"
}

// sampleCPU records, every second until stop, how many cores this process
// and the server process (if pid > 0) kept busy, from /proc (Linux): a
// latency spike with the client's cores full is the load generator's, not
// the server's (docs/11 §5).
func sampleCPU(pid int, stop <-chan struct{}) <-chan []string {
	out := make(chan []string, 1)
	ticks := func(p string) float64 {
		b, err := os.ReadFile(p)
		if err != nil {
			return 0
		}
		// fields after the parenthesized command: utime and stime are 14 and 15
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(f) < 13 {
			return 0
		}
		u, _ := strconv.ParseFloat(f[11], 64)
		st, _ := strconv.ParseFloat(f[12], 64)
		return (u + st) / 100 // USER_HZ
	}
	srv := ""
	if pid > 0 {
		srv = fmt.Sprintf("/proc/%d/stat", pid)
	}
	go func() {
		var line []string
		pc, ps, pt := ticks("/proc/self/stat"), ticks(srv), time.Now()
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				out <- line
				return
			case now := <-tk.C:
				c, s := ticks("/proc/self/stat"), ticks(srv)
				d := now.Sub(pt).Seconds()
				line = append(line, fmt.Sprintf("%s %.1f/%.1f", now.Format("15:04:05"), (c-pc)/d, (s-ps)/d))
				pc, ps, pt = c, s, now
			}
		}
	}()
	return out
}
