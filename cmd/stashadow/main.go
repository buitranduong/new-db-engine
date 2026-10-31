// stashadow is the shadow-mode tool: it keeps STASK in step with a live
// PostgreSQL (the system of record) through logical replication, without
// serving clients, and compares the two. See docs/16_tiered_engine_vision.md
// section 4 ("Chế độ bóng") for how to run it.
//
//	stashadow run     -source URL -stask URL -tables a,b,c   initial snapshot, then follow changes (resumable)
//	stashadow compare -source URL -stask URL -tables a,b,c   counts, checksums, samples, query set; JSON + table
//	stashadow status  -source URL -stask URL                  applied LSN, lag
//	stashadow drop    -source URL -stask URL                  drop slot, publication and control row
//	stashadow txstats FILE...                                 throughput and latency from -txlog files
//	stashadow verify  -a URL -b URL [-tables ..]              per-table row counts and order-independent checksums computed inside each database (either direction)
//	stashadow drain   -source URL                             wait until everything the (frozen) source committed is in STASK
//	stashadow sync-sequences -from URL -to URL                carry sequence positions across (logical replication does not)
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"stask/internal/shadow"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "compare":
		cmdCompare(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "drop":
		cmdDrop(os.Args[2:])
	case "txstats":
		cmdTxStats(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "drain":
		cmdDrain(os.Args[2:])
	case "sync-sequences":
		cmdSyncSeq(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: stashadow run|compare|status|drop|txstats [flags]  (stashadow <cmd> -h for flags)")
	os.Exit(2)
}

type common struct {
	source, stask, tables, slot, pub string
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.source, "source", env("SHADOW_SOURCE", "postgres://postgres@127.0.0.1:28510/postgres?sslmode=disable"), "source PostgreSQL URL (the system of record)")
	fs.StringVar(&c.stask, "stask", env("SHADOW_STASK", "postgres://app@127.0.0.1:28501/stask?sslmode=disable"), "STASK URL (PG protocol)")
	fs.StringVar(&c.tables, "tables", env("SHADOW_TABLES", ""), "comma-separated public tables to shadow")
	fs.StringVar(&c.slot, "slot", "stashadow", "logical replication slot name (also the key of the control row in STASK)")
	fs.StringVar(&c.pub, "publication", "stashadow_pub", "publication name")
}

func (c *common) tableList() []string {
	var out []string
	for _, t := range strings.Split(c.tables, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s stashadow: %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

func cmdRun(args []string) {
	var c common
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c.flags(fs)
	createTables := fs.Bool("create-tables", false, "initial snapshot: DROP and CREATE the tables in STASK from the source catalog (otherwise they must exist and are truncated)")
	setRI := fs.Bool("set-replica-identity", false, "run ALTER TABLE .. REPLICA IDENTITY FULL on source tables without a primary key")
	upsert := fs.Bool("upsert", false, "apply INSERT as upsert (repair mode; hides duplicate-key divergence)")
	strict := fs.Bool("strict", false, "stop when an UPDATE/DELETE finds no row in STASK")
	reset := fs.Bool("reset", false, "drop the slot and control row and snapshot again")
	rate := fs.Float64("rate", 0, "limit applied rows per second (0 = unlimited)")
	chunk := fs.Int("chunk", 500, "statements per pipeline flush")
	pause := fs.String("pause-file", "", "while this file exists no new transaction is applied (touch to pause, rm to resume)")
	txlog := fs.String("txlog", "", "append one line per applied transaction: commit_us,applied_us,rows,end_lsn")
	schemaChanges := fs.String("schema-changes", "apply", "ALTER TABLE on the source: apply (ADD/DROP/RENAME COLUMN, ALTER COLUMN TYPE are carried to STASK) or stop (stop with the DDL to run)")
	metricsAddr := fs.String("metrics-addr", "", "serve Prometheus metrics (lag bytes and seconds, throughput, reconnects) on this address, path /metrics")
	cpuprof := fs.String("cpuprofile", "", "write a CPU profile to this file (for tuning)")
	logEvery := fs.Duration("log-every", 5*time.Second, "progress line interval")
	fs.Parse(args)
	if len(c.tableList()) == 0 {
		fmt.Fprintln(os.Stderr, "stashadow: -tables is required")
		os.Exit(2)
	}
	if *cpuprof != "" {
		f, err := os.Create(*cpuprof)
		if err == nil {
			pprof.StartCPUProfile(f)
			defer pprof.StopCPUProfile()
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stats := shadow.NewStats()
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			stats.WritePrometheus(w)
		})
		go func() { logf("metrics: %v", http.ListenAndServe(*metricsAddr, mux)) }()
	}
	cfg := shadow.Config{Stats: stats, SchemaChanges: *schemaChanges, SourceURL: c.source, StaskURL: c.stask, Tables: c.tableList(), Slot: c.slot, Publication: c.pub,
		CreateTables: *createTables, SetReplicaIdentity: *setRI, Upsert: *upsert, Strict: *strict, Reset: *reset,
		RowsPerSec: *rate, Chunk: *chunk, PauseFile: *pause, TxLog: *txlog, LogEvery: *logEvery, Logf: logf}
	err := shadow.Run(ctx, cfg)
	if err != nil {
		logf("FATAL: %v", err)
		os.Exit(3)
	}
	logf("stopped (resume by running the same command again)")
}

func cmdCompare(args []string) {
	var c common
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	c.flags(fs)
	buckets := fs.Int("buckets", 256, "key blocks for the checksum")
	sample := fs.Int("sample", 200, "random rows per table checked by primary-key lookup")
	queries := fs.String("queries", "", "SQL file run on both systems and compared (scripts/shadow/sim_queries.sql)")
	retries := fs.Int("retries", 0, "re-check a differing table/query this many times (use >0 against a live, written source)")
	retryWait := fs.Duration("retry-wait", 2*time.Second, "pause between retries")
	wait := fs.Duration("wait", 30*time.Second, "wait up to this long for STASK to apply everything the source flushed (0 = do not wait)")
	out := fs.String("json", "", "write the JSON report to this file")
	seed := fs.Int64("seed", time.Now().UnixNano(), "random seed")
	every := fs.Duration("every", 0, "repeat the comparison at this interval (0 = once)")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exit := 0
	for {
		rep, err := shadow.Compare(ctx, shadow.CompareConfig{SourceURL: c.source, StaskURL: c.stask, Tables: c.tableList(), Slot: c.slot,
			Buckets: *buckets, Sample: *sample, QueriesFile: *queries, Retries: *retries, RetryWait: *retryWait, WaitCatchUp: *wait, Seed: *seed, Logf: logf})
		if err != nil {
			fmt.Fprintln(os.Stderr, "compare:", err)
			os.Exit(3)
		}
		rep.WriteTable(os.Stdout)
		if *out != "" {
			f, err := os.Create(*out)
			if err == nil {
				rep.WriteJSON(f)
				f.Close()
			}
		}
		if !rep.OK {
			exit = 1
		}
		if *every <= 0 || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(*every):
		}
		if ctx.Err() != nil {
			break
		}
	}
	os.Exit(exit)
}

func cmdStatus(args []string) {
	var c common
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	c.flags(fs)
	fs.Parse(args)
	l, err := shadow.Status(context.Background(), c.source, c.stask, c.slot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		os.Exit(3)
	}
	fmt.Printf("source flush LSN      %s\nslot confirmed flush  %s\nSTASK applied LSN     %s\nbehind (applied)      %d bytes\nbehind (acked)        %d bytes\napplied               %d txs, %d rows\nlast commit applied   %.0f ms ago\n",
		l.SourceFlushLSN, l.SlotConfirmed, l.AppliedLSN, l.LagBytesApplied, l.LagBytesAcked, l.AppliedTxs, l.AppliedRows, l.LastCommitAgeMs)
}

func cmdDrop(args []string) {
	var c common
	fs := flag.NewFlagSet("drop", flag.ExitOnError)
	c.flags(fs)
	fs.Parse(args)
	if err := shadow.Drop(context.Background(), c.source, c.stask, c.slot, c.pub); err != nil {
		fmt.Fprintln(os.Stderr, "drop:", err)
		os.Exit(3)
	}
	fmt.Println("dropped slot, publication and control row")
}

// cmdTxStats reads -txlog files (any number of process lifetimes) and prints
// throughput and commit-to-applied latency.
func cmdTxStats(files []string) {
	var lat []int64
	var rows, txs int64
	var first, last int64
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		for _, line := range strings.Split(string(b), "\n") {
			p := strings.Split(line, ",")
			if len(p) != 4 {
				continue
			}
			c, e1 := strconv.ParseInt(p[0], 10, 64)
			a, e2 := strconv.ParseInt(p[1], 10, 64)
			n, e3 := strconv.ParseInt(p[2], 10, 64)
			if e1 != nil || e2 != nil || e3 != nil {
				continue
			}
			lat = append(lat, a-c)
			rows += n
			txs++
			if first == 0 || a < first {
				first = a
			}
			if a > last {
				last = a
			}
		}
	}
	if txs == 0 {
		fmt.Println("no transactions")
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	span := float64(last-first) / 1e6
	fmt.Printf("txs=%d rows=%d span=%.1fs  rows/s(over span)=%.0f txs/s=%.0f\n", txs, rows, span, float64(rows)/span, float64(txs)/span)
	fmt.Printf("commit->applied latency ms: p50=%.1f p90=%.1f p99=%.1f max=%.1f\n",
		float64(shadow.Percentile(lat, 50))/1000, float64(shadow.Percentile(lat, 90))/1000, float64(shadow.Percentile(lat, 99))/1000, float64(lat[len(lat)-1])/1000)
}

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	a := fs.String("a", env("SHADOW_SOURCE", "postgres://postgres@127.0.0.1:28510/postgres?sslmode=disable"), "reference database URL (usually PostgreSQL, the system of record)")
	b := fs.String("b", env("SHADOW_STASK", "postgres://app@127.0.0.1:28501/stask?sslmode=disable"), "database URL to check")
	tables := fs.String("tables", "", "comma-separated tables (default: every base table of schema public in A)")
	retries := fs.Int("retries", 0, "re-check a differing table this many times (live systems)")
	wait := fs.Duration("retry-wait", 2*time.Second, "pause between retries")
	js := fs.String("json", "", "write the JSON report here")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var names []string
	for _, t := range strings.Split(*tables, ",") {
		if t = strings.TrimSpace(t); t != "" {
			names = append(names, t)
		}
	}
	rep, err := shadow.Verify(ctx, shadow.VerifyConfig{AURL: *a, BURL: *b, Tables: names, Retries: *retries, RetryWait: *wait})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(3)
	}
	rep.WriteTable(os.Stdout)
	if *js != "" {
		if f, err := os.Create(*js); err == nil {
			rep.WriteJSON(f)
			f.Close()
		}
	}
	if !rep.OK {
		os.Exit(1)
	}
}

func cmdDrain(args []string) {
	fs := flag.NewFlagSet("drain", flag.ExitOnError)
	src := fs.String("source", env("SHADOW_SOURCE", "postgres://postgres@127.0.0.1:28510/postgres?sslmode=disable"), "source PostgreSQL URL (frozen for writes)")
	slot := fs.String("slot", "stashadow", "slot name of the running stashadow")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up after this long")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := shadow.Drain(ctx, *src, *slot, *timeout, logf); err != nil {
		fmt.Fprintln(os.Stderr, "drain:", err)
		os.Exit(1)
	}
	fmt.Println("drained")
}

func cmdSyncSeq(args []string) {
	fs := flag.NewFlagSet("sync-sequences", flag.ExitOnError)
	from := fs.String("from", "", "URL of the system the writes were on")
	to := fs.String("to", "", "URL of the system the writes go to next")
	margin := fs.Int64("margin", 0, "add this to every value (room for ids handed out but not committed)")
	fs.Parse(args)
	if *from == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "sync-sequences: -from and -to are required")
		os.Exit(2)
	}
	res, err := shadow.SyncSequences(context.Background(), *from, *to, *margin, logf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync-sequences:", err)
		os.Exit(1)
	}
	set := 0
	for _, r := range res {
		if r.Set {
			set++
		} else {
			logf("sequence %s not changed: %s", r.Name, r.Reason)
		}
	}
	fmt.Printf("%d sequences read, %d set\n", len(res), set)
}
