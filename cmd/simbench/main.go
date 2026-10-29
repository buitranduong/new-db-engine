// simbench simulates the target workload: warehouse (Kho), CRM and web shop
// share one database holding tens of millions of SIM numbers; every day a
// spreadsheet of millions of rows is imported while orders keep coming.
// The same client runs against staskd and PostgreSQL (-system stask|pg).
//
// Phases: baseline (no import) -> one phase per import method, with the
// background load still running -> after. Every operation is tagged with the
// phase in which it completed, so latency under import vs. baseline can be
// compared per operation. Latencies are relative only (shared machine).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------- stats

type opStat struct {
	lat     []time.Duration
	fail    int
	retries int
	lost    int
	errs    map[string]int
}

type phase struct {
	name       string
	start, end time.Time
	mu         sync.Mutex
	ops        map[string]*opStat
	lockN      int // lock-wait samples taken
	lockSum    int
	lockMax    int
	importDur  time.Duration
	importErr  string
	importRows int
}

var cur atomic.Pointer[phase]

func (p *phase) op(name string) *opStat {
	s := p.ops[name]
	if s == nil {
		s = &opStat{errs: map[string]int{}}
		p.ops[name] = s
	}
	return s
}

func rec(name string, d time.Duration, retries int, lost bool, err error) {
	p := cur.Load()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.op(name)
	s.retries += retries
	if err != nil {
		s.fail++
		s.errs[errKey(err)]++
		return
	}
	if lost {
		s.lost++
	}
	s.lat = append(s.lat, d)
}

func errKey(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		m := pe.Message
		if len(m) > 60 {
			m = m[:60]
		}
		return pe.Code + " " + m
	}
	m := err.Error()
	if len(m) > 70 {
		m = m[:70]
	}
	return m
}

// retryable: serialization failure, deadlock, lock timeout, or STASK's
// commit-time conflict/abort messages.
func retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "40001", "40P01", "55P03":
			return true
		}
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "conflict") || strings.Contains(m, "serializ") || strings.Contains(m, "deadlock") || strings.Contains(m, "aborted")
}

func quant(d []time.Duration, q float64) float64 {
	if len(d) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(q*float64(len(d)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(d) {
		i = len(d) - 1
	}
	return float64(d[i].Microseconds()) / 1000
}

// ---------------------------------------------------------------- data model

var carriers = []string{"viettel", "mobifone", "vinaphone", "vietnamobile"}

func numStr(i int64) string      { return fmt.Sprintf("0%09d", (i*7919+12345)%1000000000) }
func baseCarrier(i int64) string { return carriers[i%4] }
func basePrice(i int64) int64    { return 100000 + (i*37%500)*10000 }
func baseStatus(i int64) string {
	switch h := i % 20; {
	case h < 17:
		return "available"
	case h < 19:
		return "blocked"
	}
	return "inactive"
}

type row struct {
	num, carrier, status string
	price                int64
}

func (r row) csv(b *bytes.Buffer) {
	fmt.Fprintf(b, "%s,%s,%s,%d\n", r.num, r.carrier, r.status, r.price)
}

func initialRows(n int64, emit func(row) error) error {
	for i := int64(0); i < n; i++ {
		if err := emit(row{numStr(i), baseCarrier(i), baseStatus(i), basePrice(i)}); err != nil {
			return err
		}
	}
	return nil
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// importRows yields the rows of one "Excel file": newFrac new numbers (index
// >= firstNew), the rest distinct existing numbers split priceFrac:rest into
// price changes and status/carrier changes.
func importRows(n, n0, firstNew int64, newFrac, priceFrac float64, seed uint64, emit func(row) error) (newCount int64, err error) {
	rng := rand.New(rand.NewPCG(seed, 99))
	stride := int64(float64(n0) * 0.6180339)
	for gcd(stride, n0) != 1 {
		stride++
	}
	off := rng.Int64N(n0)
	var j, nn int64
	for k := int64(0); k < n; k++ {
		var r row
		x := rng.Float64()
		if x < newFrac {
			i := firstNew + nn
			nn++
			r = row{numStr(i), carriers[rng.IntN(4)], "available", 100000 + rng.Int64N(500)*10000}
		} else {
			i := (j*stride + off) % n0
			j++
			r = row{numStr(i), baseCarrier(i), baseStatus(i), basePrice(i)}
			if (x-newFrac)/(1-newFrac) < priceFrac {
				r.price = basePrice(i) + (1+rng.Int64N(50))*1000
			} else {
				r.carrier = carriers[(i+1+int64(rng.IntN(3)))%4]
				if rng.IntN(2) == 0 {
					r.status = "available"
				} else {
					r.status = "blocked"
				}
			}
		}
		if err = emit(r); err != nil {
			return
		}
	}
	return nn, nil
}

// ---------------------------------------------------------------- config

type cfg struct {
	system, dsn                 string
	n0, importN                 int64
	batch                       int
	web, orders, proc, inv, crm int
	think                       time.Duration
	baseSec, afterSec           int
	methods                     string
	hotN                        int
	hotP                        float64
	newFrac, priceFrac          float64
	impTimeout                  time.Duration
	noIndex                     bool
	iso                         string
	out                         string
	copySQL                     string
	minFreeGB                   float64
	history, es                 bool
}

var c cfg
var pool *pgxpool.Pool
var seqOrder atomic.Int64
var okOrders atomic.Int64
var histSeq atomic.Int64

func main() {
	flag.StringVar(&c.system, "system", "pg", "stask or pg")
	flag.StringVar(&c.dsn, "dsn", "", "connection string (default per system)")
	flag.Int64Var(&c.n0, "sims", 1_000_000, "initial SIM rows")
	flag.Int64Var(&c.importN, "import", 200_000, "rows per import file")
	flag.IntVar(&c.batch, "batch", 2000, "rows per batch for the INSERT..ON CONFLICT method (1000-5000)")
	flag.IntVar(&c.web, "web", 8, "website workers")
	flag.IntVar(&c.orders, "orders", 8, "order (customer) workers")
	flag.IntVar(&c.proc, "proc", 2, "order-processing workers")
	flag.IntVar(&c.inv, "inv", 1, "warehouse inventory-sync workers")
	flag.IntVar(&c.crm, "crm", 1, "CRM aggregate workers")
	flag.DurationVar(&c.think, "think", 5*time.Millisecond, "think time between operations of a worker")
	flag.IntVar(&c.baseSec, "base-sec", 20, "baseline phase seconds")
	flag.IntVar(&c.afterSec, "after-sec", 20, "after phase seconds")
	flag.StringVar(&c.methods, "methods", "copy_merge,batch_upsert,update_from", "import methods to run, in order")
	flag.IntVar(&c.hotN, "hot", 50, "size of the hot (premium) number set")
	flag.Float64Var(&c.hotP, "hot-p", 0.3, "probability an order targets a hot number")
	flag.Float64Var(&c.newFrac, "new-frac", 0.10, "share of new numbers in an import file")
	flag.Float64Var(&c.priceFrac, "price-frac", 0.667, "share of price changes among existing rows (0.667 of 90% = ~60% of file)")
	flag.DurationVar(&c.impTimeout, "imp-timeout", 10*time.Minute, "timeout of one import")
	flag.BoolVar(&c.noIndex, "no-index", false, "pg: do not create secondary indexes")
	flag.StringVar(&c.iso, "iso", "", "transaction isolation for order/process txns: '', serializable, repeatable read")
	flag.StringVar(&c.out, "out", "", "write JSON report here")
	flag.StringVar(&c.copySQL, "copy-sql", "COPY %s FROM STDIN WITH (FORMAT csv)", "COPY statement template")
	flag.Float64Var(&c.minFreeGB, "min-free-gb", 3, "refuse to start if the filesystem of -free-path has less free space (checked by caller script too)")
	flag.BoolVar(&c.history, "history", true, "write a sim_history row for every SIM change (import, orders) and run the history benchmarks")
	flag.BoolVar(&c.es, "es", true, "add Elasticsearch-style website queries: suffix, contains, pretty numbers, facet, combined filter")
	flag.Parse()
	if c.dsn == "" {
		if c.system == "stask" {
			c.dsn = "postgres://stask@127.0.0.1:17432/stask?sslmode=disable"
		} else {
			c.dsn = "postgres://postgres:sb@127.0.0.1:28433/postgres?sslmode=disable"
		}
	}
	ctx := context.Background()
	pc, err := pgxpool.ParseConfig(c.dsn)
	must(err)
	pc.MaxConns = int32(c.web + c.orders + c.proc + c.inv + c.crm + 6)
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	if c.system == "stask" {
		pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec // extended protocol, no statement cache
	}
	pool, err = pgxpool.NewWithConfig(ctx, pc)
	must(err)
	defer pool.Close()

	cur.Store(&phase{name: "setup", ops: map[string]*opStat{}})
	setup(ctx)
	explainEvidence(ctx)

	// workloads
	wctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	hot := newHot()
	queue := make(chan procItem, 200000)
	spawn := func(n int, f func(context.Context, int)) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) { defer wg.Done(); f(wctx, i) }(i)
		}
	}
	spawn(c.web, webWorker)
	spawn(c.orders, func(x context.Context, i int) { orderWorker(x, i, hot, queue) })
	spawn(c.proc, func(x context.Context, i int) { procWorker(x, i, hot, queue) })
	spawn(c.inv, invWorker)
	spawn(c.crm, crmWorker)
	wg.Add(1)
	go func() { defer wg.Done(); lockSampler(wctx) }()

	var phases []*phase
	begin := func(name string) *phase {
		p := &phase{name: name, start: time.Now(), ops: map[string]*opStat{}}
		if o := cur.Swap(p); o != nil && o.name != "setup" {
			o.end = p.start
		}
		phases = append(phases, p)
		return p
	}
	begin("baseline")
	time.Sleep(time.Duration(c.baseSec) * time.Second)
	expectedNew := int64(0)
	nextNew := c.n0
	for mi, m := range strings.Split(c.methods, ",") {
		p := begin("import_" + m)
		nn, d, err := runImport(ctx, m, mi, nextNew)
		p.importDur = d
		p.importRows = int(c.importN)
		if err != nil {
			p.importErr = errKey(err)
			fmt.Fprintf(os.Stderr, "import %s failed: %v\n", m, err)
			// a failed statement may have inserted nothing; recount below
		} else {
			expectedNew += nn
		}
		nextNew += nn
	}
	begin("after")
	time.Sleep(time.Duration(c.afterSec) * time.Second)
	stop()
	wg.Wait()
	cur.Load().end = time.Now()

	inv := invariants(ctx, expectedNew, phases)
	historyBench(ctx)
	report(phases, inv)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(2)
	}
}

// ---------------------------------------------------------------- setup

func exec(ctx context.Context, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
}

func copyIn(ctx context.Context, table string, gen func(w io.Writer) error) (int64, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(gen(pw)) }()
	tag, err := conn.Conn().PgConn().CopyFrom(ctx, pr, fmt.Sprintf(c.copySQL, table))
	pr.CloseWithError(err)
	return tag.RowsAffected(), err
}

func csvWriter(w io.Writer) (func(row) error, func() error) {
	var b bytes.Buffer
	emit := func(r row) error {
		r.csv(&b)
		if b.Len() > 1<<16 {
			_, err := w.Write(b.Bytes())
			b.Reset()
			return err
		}
		return nil
	}
	flush := func() error { _, err := w.Write(b.Bytes()); b.Reset(); return err }
	return emit, flush
}

func setup(ctx context.Context) {
	for _, t := range []string{"order_items", "orders", "sims", "staging", "sim_history"} {
		_ = exec(ctx, "DROP TABLE IF EXISTS "+t)
	}
	must(exec(ctx, `CREATE TABLE sims (number text PRIMARY KEY, carrier text, status text, price bigint, updated_at bigint)`))
	must(exec(ctx, `CREATE TABLE orders (id bigint PRIMARY KEY, customer bigint, status text, total bigint, day bigint)`))
	must(exec(ctx, `CREATE TABLE order_items (id bigint PRIMARY KEY, order_id bigint, number text, price bigint, active bigint)`))
	must(exec(ctx, `CREATE TABLE sim_history (id bigint PRIMARY KEY, number text, changed_at bigint, carrier text, status text, price bigint, source text)`))
	histSeq.Store(1_000_000_000)
	t0 := time.Now()
	n, err := copyIn(ctx, "sims(number,carrier,status,price)", func(w io.Writer) error {
		emit, flush := csvWriter(w)
		if err := initialRows(c.n0, emit); err != nil {
			return err
		}
		return flush()
	})
	must(err)
	fmt.Printf("setup: loaded %d sims in %v\n", n, time.Since(t0).Round(time.Millisecond))
	if c.system == "pg" && !c.noIndex {
		t0 = time.Now()
		must(exec(ctx, `CREATE INDEX sims_num_pat ON sims (number text_pattern_ops)`))
		must(exec(ctx, `CREATE INDEX sims_cf ON sims (carrier, status, price)`))
		fmt.Printf("setup: pg secondary indexes in %v\n", time.Since(t0).Round(time.Millisecond))
	}
	if c.history {
		// seed old history: 3 rows for each of the first n0/10 SIMs, aged 0..89 days
		now := time.Now().Unix()
		_, err := copyIn(ctx, "sim_history", func(w io.Writer) error {
			var b bytes.Buffer
			for i := int64(0); i < c.n0/10; i++ {
				for k := int64(0); k < 3; k++ {
					age := (i*7 + k*31) % 90
					fmt.Fprintf(&b, "%d,%s,%d,%s,%s,%d,seed\n", histSeq.Add(1), numStr(i), now-age*86400, baseCarrier(i), baseStatus(i), basePrice(i))
				}
				if b.Len() > 1<<16 {
					if _, e := w.Write(b.Bytes()); e != nil {
						return e
					}
					b.Reset()
				}
			}
			_, e := w.Write(b.Bytes())
			return e
		})
		must(err)
		if c.system == "pg" && !c.noIndex {
			must(exec(ctx, `CREATE INDEX hist_num ON sim_history (number, changed_at)`))
			must(exec(ctx, `CREATE INDEX hist_at ON sim_history (changed_at)`))
		}
	}
	if c.system == "pg" && !c.noIndex && c.es {
		t0 = time.Now()
		must(exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm`))
		must(exec(ctx, `CREATE INDEX sims_trgm ON sims USING gin (number gin_trgm_ops)`))
		fmt.Printf("setup: pg_trgm GIN in %v\n", time.Since(t0).Round(time.Millisecond))
	}
	if c.system == "pg" {
		_ = exec(ctx, `ANALYZE`)
	}
}

// explainEvidence records the access path and a worst-case timing that must
// touch every row unless an index/range scan is used.
func explainEvidence(ctx context.Context) {
	qs := []struct {
		name, sql string
		args      []any
	}{
		{"prefix_hit", `SELECT number,carrier,status,price FROM sims WHERE number LIKE $1 LIMIT 50`, []any{"090%"}},
		{"prefix_miss", `SELECT number FROM sims WHERE number LIKE $1 LIMIT 50`, []any{"x%"}},
		{"filter_hit", `SELECT number,price FROM sims WHERE carrier=$1 AND status='available' AND price BETWEEN $2 AND $3 LIMIT 50`, []any{"viettel", 200000, 700000}},
		{"filter_miss", `SELECT number,price FROM sims WHERE carrier=$1 AND status='available' AND price BETWEEN $2 AND $3 LIMIT 50`, []any{"viettel", -2, -1}},
		{"lookup", `SELECT number,carrier,status,price FROM sims WHERE number=$1`, []any{numStr(12345)}},
	}
	fmt.Println("== access-path evidence (EXPLAIN + 5x timing) ==")
	for _, q := range qs {
		var plan []string
		rows, err := pool.Query(ctx, "EXPLAIN "+q.sql, q.args...)
		if err != nil {
			plan = []string{"EXPLAIN error: " + errKey(err)}
		} else {
			for rows.Next() {
				var s string
				if rows.Scan(&s) == nil {
					plan = append(plan, s)
				}
			}
			rows.Close()
			if len(plan) == 0 {
				plan = []string{"(no plan rows)"}
			}
		}
		var ts []float64
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			r, err := pool.Query(ctx, q.sql, q.args...)
			if err != nil {
				ts = append(ts, -1)
				continue
			}
			for r.Next() {
			}
			r.Close()
			ts = append(ts, float64(time.Since(t0).Microseconds())/1000)
		}
		fmt.Printf("%-12s ms=%v\n    plan: %s\n", q.name, ts, strings.Join(plan, " | "))
	}
}

// ---------------------------------------------------------------- imports

func runImport(ctx context.Context, m string, mi int, firstNew int64) (newCount int64, d time.Duration, err error) {
	ictx, cancel := context.WithTimeout(ctx, c.impTimeout)
	defer cancel()
	seed := uint64(1000 + mi)
	t0 := time.Now()
	defer func() { d = time.Since(t0) }()
	stamp := time.Now().Unix()
	switch m {
	case "copy_merge", "update_from":
		_ = exec(ictx, "DROP TABLE IF EXISTS staging")
		if err = exec(ictx, `CREATE TABLE staging (number text PRIMARY KEY, carrier text, status text, price bigint)`); err != nil {
			return
		}
		defer func() { _ = exec(context.Background(), "DROP TABLE IF EXISTS staging") }()
		var nn int64
		_, err = copyIn(ictx, "staging", func(w io.Writer) error {
			emit, flush := csvWriter(w)
			var e error
			nn, e = importRows(c.importN, c.n0, firstNew, c.newFrac, c.priceFrac, seed, emit)
			if e != nil {
				return e
			}
			return flush()
		})
		if err != nil {
			return
		}
		newCount = nn
		if c.history {
			th := time.Now()
			herr := exec(ictx, fmt.Sprintf(`INSERT INTO sim_history (id,number,changed_at,carrier,status,price,source)
SELECT %d + row_number() OVER (ORDER BY number), number, %d, carrier, status, price, 'import' FROM staging`, histSeq.Add(c.importN+1)-c.importN-1, stamp))
			rec("import_history_write", time.Since(th), 0, false, herr)
		}
		keep := `CASE WHEN sims.status IN ('reserved','sold') THEN sims.status ELSE %s.status END`
		if m == "copy_merge" {
			err = exec(ictx, fmt.Sprintf(`INSERT INTO sims (number,carrier,status,price,updated_at)
SELECT number,carrier,status,price,%d FROM staging
ON CONFLICT (number) DO UPDATE SET carrier=excluded.carrier, price=excluded.price, updated_at=excluded.updated_at, status=`+keep, stamp, "excluded"))
		} else {
			if err = exec(ictx, fmt.Sprintf(`UPDATE sims SET carrier=t.carrier, price=t.price, updated_at=%d, status=`+keep+`
FROM staging t WHERE sims.number=t.number`, stamp, "t")); err != nil {
				return
			}
			err = exec(ictx, fmt.Sprintf(`INSERT INTO sims (number,carrier,status,price,updated_at)
SELECT t.number,t.carrier,t.status,t.price,%d FROM staging t WHERE NOT EXISTS (SELECT 1 FROM sims s WHERE s.number=t.number)`, stamp))
		}
	case "batch_upsert":
		var buf []row
		var nn int64
		flush := func() error {
			if len(buf) == 0 {
				return nil
			}
			sort.Slice(buf, func(i, j int) bool { return buf[i].num < buf[j].num }) // same lock order everywhere
			var sb strings.Builder
			sb.WriteString(`INSERT INTO sims (number,carrier,status,price,updated_at) VALUES `)
			args := make([]any, 0, len(buf)*4)
			for i, r := range buf {
				if i > 0 {
					sb.WriteByte(',')
				}
				k := i * 4
				fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,%d)", k+1, k+2, k+3, k+4, stamp)
				args = append(args, r.num, r.carrier, r.status, r.price)
			}
			sb.WriteString(` ON CONFLICT (number) DO UPDATE SET carrier=excluded.carrier, price=excluded.price, updated_at=excluded.updated_at,
status=CASE WHEN sims.status IN ('reserved','sold') THEN sims.status ELSE excluded.status END`)
			var e error
			if c.history {
				th := time.Now()
				var hb strings.Builder
				hb.WriteString(`INSERT INTO sim_history (id,number,changed_at,carrier,status,price,source) VALUES `)
				ha := make([]any, 0, len(buf)*5)
				base := histSeq.Add(int64(len(buf))) - int64(len(buf))
				for i, r := range buf {
					if i > 0 {
						hb.WriteByte(',')
					}
					k := i * 5
					fmt.Fprintf(&hb, "($%d,$%d,%d,$%d,$%d,$%d,'import')", k+1, k+2, stamp, k+3, k+4, k+5)
					ha = append(ha, base+int64(i)+1, r.num, r.carrier, r.status, r.price)
				}
				rec("import_history_write", time.Since(th), 0, false, exec(ictx, hb.String(), ha...))
			}
			for try := 0; try < 10; try++ {
				if e = exec(ictx, sb.String(), args...); e == nil || !retryable(e) {
					break
				}
				rec("import_batch_retry", 0, 1, false, nil)
				time.Sleep(time.Duration(try+1) * 20 * time.Millisecond)
			}
			buf = buf[:0]
			return e
		}
		nn, err = importRows(c.importN, c.n0, firstNew, c.newFrac, c.priceFrac, seed, func(r row) error {
			buf = append(buf, r)
			if len(buf) >= c.batch {
				t := time.Now()
				e := flush()
				rec("import_batch", time.Since(t), 0, false, e)
				return e
			}
			return nil
		})
		if err == nil {
			err = flush()
		}
		newCount = nn
	default:
		err = fmt.Errorf("unknown method %q", m)
	}
	if err != nil {
		newCount = 0
	}
	return
}

// ---------------------------------------------------------------- workers

func pace(ctx context.Context, rng *rand.Rand) bool {
	t := time.Duration(rng.Int64N(int64(c.think)*2 + 1))
	select {
	case <-ctx.Done():
		return false
	case <-time.After(t):
		return true
	}
}

func timed(op string, f func() (lost bool, retries int, err error)) {
	t0 := time.Now()
	lost, r, err := f()
	rec(op, time.Since(t0), r, lost, err)
}

func drain(rows pgx.Rows) (n int, err error) {
	for rows.Next() {
		n++
	}
	rows.Close()
	return n, rows.Err()
}

func webWorker(ctx context.Context, id int) {
	rng := rand.New(rand.NewPCG(uint64(id), 1))
	for pace(ctx, rng) {
		pick := rng.IntN(3)
		if c.es && rng.IntN(2) == 0 {
			pick = 3 + rng.IntN(6)
		}
		switch pick {
		case 3, 4, 5, 6, 7, 8:
			esQuery(ctx, rng, pick)
		case 0:
			timed("web_prefix", func() (bool, int, error) {
				p := fmt.Sprintf("0%02d%%", rng.IntN(100))
				r, err := pool.Query(ctx, `SELECT number,carrier,status,price FROM sims WHERE number LIKE $1 LIMIT 50`, p)
				if err != nil {
					return false, 0, err
				}
				_, err = drain(r)
				return false, 0, err
			})
		case 1:
			timed("web_filter", func() (bool, int, error) {
				lo := 100000 + rng.Int64N(400)*10000
				r, err := pool.Query(ctx, `SELECT number,price FROM sims WHERE carrier=$1 AND status='available' AND price BETWEEN $2 AND $3 LIMIT 50`, carriers[rng.IntN(4)], lo, lo+500000)
				if err != nil {
					return false, 0, err
				}
				_, err = drain(r)
				return false, 0, err
			})
		default:
			timed("web_lookup", func() (bool, int, error) {
				r, err := pool.Query(ctx, `SELECT number,carrier,status,price FROM sims WHERE number=$1`, numStr(rng.Int64N(c.n0)))
				if err != nil {
					return false, 0, err
				}
				_, err = drain(r)
				return false, 0, err
			})
		}
	}
}

type hotSet struct {
	mu sync.Mutex
	n  []string
}

func newHot() *hotSet {
	h := &hotSet{}
	rng := rand.New(rand.NewPCG(7, 7))
	for len(h.n) < c.hotN {
		i := rng.Int64N(c.n0)
		if baseStatus(i) == "available" {
			h.n = append(h.n, numStr(i))
		}
	}
	return h
}
func (h *hotSet) pick(rng *rand.Rand) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[rng.IntN(len(h.n))]
}
func (h *hotSet) replace(old string, rng *rand.Rand) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, s := range h.n {
		if s == old {
			for {
				k := rng.Int64N(c.n0)
				if baseStatus(k) == "available" {
					h.n[i] = numStr(k)
					return
				}
			}
		}
	}
}

type procItem struct {
	id   int64
	nums []string
}

func txOpts() pgx.TxOptions {
	switch c.iso {
	case "serializable":
		return pgx.TxOptions{IsoLevel: pgx.Serializable}
	case "repeatable read":
		return pgx.TxOptions{IsoLevel: pgx.RepeatableRead}
	}
	return pgx.TxOptions{}
}

// retry runs f in a transaction, retrying retryable failures.
func retry(ctx context.Context, rng *rand.Rand, f func(tx pgx.Tx) (lost bool, err error)) (lost bool, retries int, err error) {
	for try := 0; ; try++ {
		var tx pgx.Tx
		tx, err = pool.BeginTx(ctx, txOpts())
		if err == nil {
			lost, err = f(tx)
			if err == nil && !lost {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(context.Background())
			}
		}
		if err == nil || !retryable(err) || try >= 8 || ctx.Err() != nil {
			return
		}
		retries++
		time.Sleep(time.Duration(rng.Int64N(int64(10*(try+1)))+1) * time.Millisecond)
	}
}

func orderWorker(ctx context.Context, id int, hot *hotSet, q chan procItem) {
	rng := rand.New(rand.NewPCG(uint64(id), 2))
	for pace(ctx, rng) {
		k := 1 + rng.IntN(2)
		var nums []string
		isHot := false
		for len(nums) < k {
			var s string
			if rng.Float64() < c.hotP {
				s, isHot = hot.pick(rng), true
			} else {
				s = numStr(rng.Int64N(c.n0))
			}
			dup := false
			for _, x := range nums {
				dup = dup || x == s
			}
			if !dup {
				nums = append(nums, s)
			}
		}
		sort.Strings(nums) // consistent lock order
		op := "order_reserve"
		if isHot {
			op = "order_reserve_hot"
		}
		oid := seqOrder.Add(1)
		var committed bool
		timed(op, func() (bool, int, error) {
			lost, r, err := retry(ctx, rng, func(tx pgx.Tx) (bool, error) {
				var total int64
				for _, n := range nums {
					var price int64
					e := tx.QueryRow(ctx, `UPDATE sims SET status='reserved', updated_at=$2 WHERE number=$1 AND status='available' RETURNING price`, n, oid).Scan(&price)
					if errors.Is(e, pgx.ErrNoRows) {
						return true, nil // someone else got it
					}
					if e != nil {
						return false, e
					}
					total += price
					if c.history {
						if _, e = tx.Exec(ctx, `INSERT INTO sim_history (id,number,changed_at,carrier,status,price,source) SELECT $1,number,$2,carrier,status,price,'order' FROM sims WHERE number=$3`, histSeq.Add(1), oid, n); e != nil {
							return false, e
						}
					}
				}
				if _, e := tx.Exec(ctx, `INSERT INTO orders (id,customer,status,total,day) VALUES ($1,$2,'pending',$3,$4)`, oid, int64(id), total, oid%30+1); e != nil {
					return false, e
				}
				for i, n := range nums {
					if _, e := tx.Exec(ctx, `INSERT INTO order_items (id,order_id,number,price,active) SELECT $1,$2,$3,price,1 FROM sims WHERE number=$3`, oid*10+int64(i), oid, n); e != nil {
						return false, e
					}
				}
				return false, nil
			})
			committed = err == nil && !lost
			return lost, r, err
		})
		if committed {
			okOrders.Add(1)
			select {
			case q <- procItem{oid, nums}:
			default:
			}
		}
	}
}

func procWorker(ctx context.Context, id int, hot *hotSet, q chan procItem) {
	rng := rand.New(rand.NewPCG(uint64(id), 3))
	for {
		var it procItem
		select {
		case <-ctx.Done():
			return
		case it = <-q:
		}
		if !pace(ctx, rng) {
			return
		}
		paid := rng.Float64() < 0.8
		op := "order_process_pay"
		if !paid {
			op = "order_process_cancel"
		}
		timed(op, func() (bool, int, error) {
			return retry(ctx, rng, func(tx pgx.Tx) (bool, error) {
				if paid {
					if _, e := tx.Exec(ctx, `UPDATE orders SET status='paid' WHERE id=$1`, it.id); e != nil {
						return false, e
					}
					for _, n := range it.nums {
						if _, e := tx.Exec(ctx, `UPDATE sims SET status='sold' WHERE number=$1`, n); e != nil {
							return false, e
						}
					}
					return false, nil
				}
				if _, e := tx.Exec(ctx, `UPDATE orders SET status='cancelled' WHERE id=$1`, it.id); e != nil {
					return false, e
				}
				for i, n := range it.nums {
					if _, e := tx.Exec(ctx, `UPDATE order_items SET active=0 WHERE id=$1`, it.id*10+int64(i)); e != nil {
						return false, e
					}
					if _, e := tx.Exec(ctx, `UPDATE sims SET status='available' WHERE number=$1 AND status='reserved'`, n); e != nil {
						return false, e
					}
				}
				return false, nil
			})
		})
		if paid {
			for _, n := range it.nums {
				hot.replace(n, rng)
			}
		}
	}
}

// Warehouse sync: a small batch of price updates in one transaction, in
// number order, never touching rows that the order flow owns.
func invWorker(ctx context.Context, id int) {
	rng := rand.New(rand.NewPCG(uint64(id), 4))
	for pace(ctx, rng) {
		nums := make([]string, 20)
		for i := range nums {
			nums[i] = numStr(rng.Int64N(c.n0))
		}
		sort.Strings(nums)
		timed("inv_sync", func() (bool, int, error) {
			return retry(ctx, rng, func(tx pgx.Tx) (bool, error) {
				for _, n := range nums {
					if _, e := tx.Exec(ctx, `UPDATE sims SET price=price+1000 WHERE number=$1 AND status IN ('available','blocked','inactive')`, n); e != nil {
						return false, e
					}
				}
				return false, nil
			})
		})
	}
}

func crmWorker(ctx context.Context, id int) {
	rng := rand.New(rand.NewPCG(uint64(id), 5))
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		i++
		if i%2 == 0 {
			timed("crm_count_by_carrier_status", func() (bool, int, error) {
				r, err := pool.Query(ctx, `SELECT carrier, status, count(*) FROM sims GROUP BY carrier, status`)
				if err != nil {
					return false, 0, err
				}
				_, err = drain(r)
				return false, 0, err
			})
		} else {
			timed("crm_revenue_by_day", func() (bool, int, error) {
				r, err := pool.Query(ctx, `SELECT day, sum(total) FROM orders WHERE status <> 'cancelled' GROUP BY day`)
				if err != nil {
					return false, 0, err
				}
				_, err = drain(r)
				return false, 0, err
			})
		}
		_ = rng
	}
}

// PG: sample sessions waiting on a heavyweight lock. STASK has no lock
// manager; its signal is the conflict/retry counters instead.
func lockSampler(ctx context.Context) {
	if c.system != "pg" {
		return
	}
	conn, err := pgx.Connect(ctx, c.dsn)
	if err != nil {
		return
	}
	defer conn.Close(context.Background())
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		var n int
		if conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock'`).Scan(&n) != nil {
			return
		}
		p := cur.Load()
		p.mu.Lock()
		p.lockN++
		p.lockSum += n
		if n > p.lockMax {
			p.lockMax = n
		}
		p.mu.Unlock()
	}
}

// ---------------------------------------------------------------- invariants

type check struct {
	Name string
	OK   bool
	Info string
}

func invariants(ctx context.Context, expectedNew int64, phases []*phase) []check {
	var out []check
	add := func(name string, ok bool, f string, a ...any) {
		out = append(out, check{name, ok, fmt.Sprintf(f, a...)})
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sims`).Scan(&n); err != nil {
		add("sims row count", false, "%v", err)
	} else {
		add("sims row count", n == c.n0+expectedNew, "got %d want %d (initial %d + new %d)", n, c.n0+expectedNew, c.n0, expectedNew)
	}
	var no int64
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&no)
	add("orders count == committed orders", no == okOrders.Load(), "db %d client %d", no, okOrders.Load())
	var s1, s2 int64
	e1 := pool.QueryRow(ctx, `SELECT coalesce(sum(total),0) FROM orders WHERE status <> 'cancelled'`).Scan(&s1)
	e2 := pool.QueryRow(ctx, `SELECT coalesce(sum(price),0) FROM order_items WHERE active=1`).Scan(&s2)
	add("sum(total of live orders) == sum(active item prices)", e1 == nil && e2 == nil && s1 == s2, "%d vs %d (%v %v)", s1, s2, e1, e2)
	// no number held by two live items; reserved/sold sims == numbers with a live item
	active := map[string]int{}
	r, err := pool.Query(ctx, `SELECT number FROM order_items WHERE active=1`)
	if err == nil {
		for r.Next() {
			var s string
			_ = r.Scan(&s)
			active[s]++
		}
		r.Close()
	}
	dup := 0
	for _, v := range active {
		if v > 1 {
			dup++
		}
	}
	add("no number sold/held twice (live items per number <= 1)", err == nil && dup == 0, "%d live numbers, %d duplicated", len(active), dup)
	held := 0
	bad := 0
	r, err = pool.Query(ctx, `SELECT number FROM sims WHERE status IN ('reserved','sold')`)
	if err == nil {
		for r.Next() {
			var s string
			_ = r.Scan(&s)
			held++
			if active[s] != 1 {
				bad++
			}
		}
		r.Close()
	}
	add("reserved/sold sims == live items", err == nil && bad == 0 && held == len(active), "sims held %d, live items %d, mismatched %d", held, len(active), bad)
	for _, p := range phases {
		if p.importErr != "" {
			add("import "+p.name, true, "FAILED (not an invariant breach): %s", p.importErr)
		}
	}
	return out
}

// ---------------------------------------------------------------- report

func report(phases []*phase, inv []check) {
	type jo struct {
		N, Fail, Retries, Lost int
		PerSec, P50, P99, Max  float64
		Errs                   map[string]int
	}
	js := map[string]any{"system": c.system, "config": c}
	ops := map[string]bool{}
	for _, p := range phases {
		for k := range p.ops {
			ops[k] = true
		}
	}
	var names []string
	for k := range ops {
		names = append(names, k)
	}
	sort.Strings(names)
	stats := map[string]map[string]jo{}
	for _, p := range phases {
		dur := p.end.Sub(p.start).Seconds()
		fmt.Printf("\n== phase %s (%.1fs)", p.name, dur)
		if p.importRows > 0 {
			fmt.Printf("  import %d rows: %.2fs (%.0f rows/s) err=%q", p.importRows, p.importDur.Seconds(), float64(p.importRows)/p.importDur.Seconds(), p.importErr)
		}
		if p.lockN > 0 {
			fmt.Printf("  lock-wait sessions avg %.2f max %d", float64(p.lockSum)/float64(p.lockN), p.lockMax)
		}
		fmt.Printf("\n%-30s %8s %9s %9s %9s %9s %6s %7s %5s\n", "op", "n", "ops/s", "p50ms", "p99ms", "maxms", "fail", "retries", "lost")
		stats[p.name] = map[string]jo{}
		for _, k := range names {
			s := p.ops[k]
			if s == nil {
				continue
			}
			sort.Slice(s.lat, func(i, j int) bool { return s.lat[i] < s.lat[j] })
			o := jo{N: len(s.lat), Fail: s.fail, Retries: s.retries, Lost: s.lost, PerSec: float64(len(s.lat)) / dur,
				P50: quant(s.lat, .5), P99: quant(s.lat, .99), Max: quant(s.lat, 1), Errs: s.errs}
			stats[p.name][k] = o
			fmt.Printf("%-30s %8d %9.1f %9.2f %9.2f %9.2f %6d %7d %5d\n", k, o.N, o.PerSec, o.P50, o.P99, o.Max, o.Fail, o.Retries, o.Lost)
			for e, n := range s.errs {
				fmt.Printf("    error x%d: %s\n", n, e)
			}
		}
	}
	fmt.Println("\n== latency under import vs baseline (p50 / p99 ratio; >1 = slower)")
	base := stats["baseline"]
	for _, p := range phases {
		if !strings.HasPrefix(p.name, "import_") || strings.HasPrefix(p.name, "import_batch_") {
			continue
		}
		fmt.Printf("%s:\n", p.name)
		for _, k := range []string{"web_prefix", "web_filter", "web_lookup", "order_reserve", "order_reserve_hot", "order_process_pay", "order_process_cancel", "inv_sync"} {
			b, i := base[k], stats[p.name][k]
			if b.N > 0 && i.N > 0 {
				fmt.Printf("  %-22s p50 %.2f -> %.2f ms (x%.1f)   p99 %.2f -> %.2f ms (x%.1f)   ops/s %.0f -> %.0f\n", k, b.P50, i.P50, i.P50/b.P50, b.P99, i.P99, i.P99/b.P99, b.PerSec, i.PerSec)
			} else if b.N > 0 {
				fmt.Printf("  %-22s baseline p50 %.2f ms; NO completed ops during import (fail=%d)\n", k, b.P50, i.Fail)
			}
		}
	}
	verdict := "VALID"
	fmt.Println("\n== invariants")
	for _, ck := range inv {
		st := "ok  "
		if !ck.OK {
			st, verdict = "FAIL", "INVALID"
		}
		fmt.Printf("%s %s: %s\n", st, ck.Name, ck.Info)
	}
	fmt.Printf("\nRESULT %s (%s)\n", verdict, c.system)
	js["phases"], js["invariants"], js["result"] = stats, inv, verdict
	imp := map[string]any{}
	for _, p := range phases {
		if p.importRows > 0 {
			imp[p.name] = map[string]any{"rows": p.importRows, "seconds": p.importDur.Seconds(), "error": p.importErr, "lock_wait_avg": avg(p.lockSum, p.lockN), "lock_wait_max": p.lockMax}
		}
	}
	js["imports"] = imp
	if c.out != "" {
		b, _ := json.MarshalIndent(js, "", " ")
		_ = os.WriteFile(c.out, b, 0o644)
	}
	if verdict != "VALID" {
		os.Exit(1)
	}
}

func avg(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// Elasticsearch-style website / warehouse lookups.
func esQuery(ctx context.Context, rng *rand.Rand, pick int) {
	q := func(op, sql string, args ...any) {
		timed(op, func() (bool, int, error) {
			r, err := pool.Query(ctx, sql, args...)
			if err != nil {
				return false, 0, err
			}
			_, err = drain(r)
			return false, 0, err
		})
	}
	d := rng.IntN(10)
	switch pick {
	case 3:
		q("es_suffix", `SELECT number,carrier,status,price FROM sims WHERE number LIKE $1 LIMIT 50`, fmt.Sprintf("%%%d%d%d%d", d, d, d, d))
	case 4:
		q("es_contains", `SELECT number,carrier,status,price FROM sims WHERE number LIKE $1 LIMIT 50`, fmt.Sprintf("%%%d%d%d%%", d, d, d))
	case 5: // pretty numbers: quad / triple-pair endings written as LIKE patterns (no regex needed)
		q("es_pretty", `SELECT number,price FROM sims WHERE number LIKE $1 OR number LIKE $2 LIMIT 50`, fmt.Sprintf("%%%d%d%d%d", d, d, d, d), fmt.Sprintf("%%%d%d%d%d%d%d", d, d, (d+1)%10, (d+1)%10, (d+2)%10, (d+2)%10))
	case 6:
		q("es_facet_carrier", `SELECT carrier, count(*) FROM sims WHERE number LIKE $1 AND status='available' GROUP BY carrier`, fmt.Sprintf("%%%d%d%d%%", d, d, d))
	case 7:
		lo := 100000 + rng.Int64N(400)*10000
		q("es_combined", `SELECT number,price FROM sims WHERE number LIKE $1 AND carrier=$2 AND status='available' AND price BETWEEN $3 AND $4 LIMIT 50`, fmt.Sprintf("%%%d%d%d", d, d, d), carriers[rng.IntN(4)], lo, lo+500000)
	default: // warehouse lookup by prefix with count
		q("es_prefix_count", `SELECT count(*) FROM sims WHERE number LIKE $1`, fmt.Sprintf("0%02d%%", rng.IntN(100)))
	}
}

// historyBench runs on the quiesced database after the invariants.
func historyBench(ctx context.Context) {
	if !c.history {
		return
	}
	fmt.Println("\n== history (quiesced) ==")
	t := func(name string, f func() error) {
		t0 := time.Now()
		err := f()
		fmt.Printf("%-34s %9.2f ms err=%v\n", name, float64(time.Since(t0).Microseconds())/1000, err)
	}
	var n int64
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sim_history`).Scan(&n)
	fmt.Printf("sim_history rows: %d\n", n)
	rng := rand.New(rand.NewPCG(9, 9))
	for i := 0; i < 5; i++ {
		num := numStr(rng.Int64N(c.n0 / 10))
		t("history_of_one_sim_last200", func() error {
			r, err := pool.Query(ctx, `SELECT changed_at,carrier,status,price,source FROM sim_history WHERE number=$1 ORDER BY changed_at DESC LIMIT 200`, num)
			if err != nil {
				return err
			}
			_, err = drain(r)
			return err
		})
	}
	cut := time.Now().Unix() - 60*86400
	t("history_last_60_days_count", func() error {
		var k int64
		return pool.QueryRow(ctx, `SELECT count(*) FROM sim_history WHERE changed_at >= $1`, cut).Scan(&k)
	})
	t("history_delete_older_than_60d", func() error {
		_, err := pool.Exec(ctx, `DELETE FROM sim_history WHERE changed_at < $1`, cut)
		return err
	})
}
