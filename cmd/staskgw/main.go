// staskgw: the single PostgreSQL-protocol endpoint over PostgreSQL and
// ClickHouse (docs/35 sections 5 to 7). It runs chsync for the tables
// itself: the first start (or -reset) snapshots them into ClickHouse.
//
//	staskgw -listen 127.0.0.1:6432 -pg URL -ch URL -tables a,b,c [-http 127.0.0.1:6480]
//	curl http://127.0.0.1:6480/shapes   # every query shape, where it runs and why
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"stask/internal/gateway"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	var cfg gateway.Config
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:6432", "address clients connect to (PostgreSQL protocol)")
	flag.StringVar(&cfg.PGURL, "pg", env("CHSYNC_SOURCE", "postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable"), "PostgreSQL URL; client sessions run as this user")
	flag.StringVar(&cfg.CHURL, "ch", env("CHSYNC_CH", "http://127.0.0.1:8123"), "ClickHouse HTTP URL")
	tables := flag.String("tables", env("CHSYNC_TABLES", ""), "comma-separated tables kept in ClickHouse")
	flag.StringVar(&cfg.Slot, "slot", "chsync", "chsync slot (key of the watermark)")
	flag.Float64Var(&cfg.CostThreshold, "cost", 20000, "PostgreSQL estimated cost from which a read is a ClickHouse candidate")
	flag.IntVar(&cfg.VerifyRuns, "verify-runs", 3, "identical comparisons with PostgreSQL before a query shape is answered by ClickHouse")
	flag.IntVar(&cfg.VerifyConns, "verify-conns", 4, "PostgreSQL connections used to compare results (comparisons running at once)")
	flag.DurationVar(&cfg.VerifyTimeout, "verify-timeout", 10*time.Minute, "longest comparison (the PostgreSQL run of an expensive query included)")
	flag.Float64Var(&cfg.Resample, "resample", 0.01, "fraction of ClickHouse answers compared with PostgreSQL again")
	flag.DurationVar(&cfg.MaxStale, "max-stale", 3*time.Second, "watermark older than this: answer from PostgreSQL")
	flag.DurationVar(&cfg.MaxWait, "max-wait", 30*time.Second, "the longest a shape slow on PostgreSQL waits for ClickHouse to be fresh (bounded by its PostgreSQL time)")
	flag.DurationVar(&cfg.WaitOwnWrite, "wait-own-write", 500*time.Millisecond, "how long a read waits for ClickHouse to include the session's own writes before it goes to PostgreSQL")
	flag.DurationVar(&cfg.PollEvery, "poll", 2*time.Millisecond, "how often a read waiting for its own writes looks at the watermark again")
	flag.BoolVar(&cfg.Reset, "reset", false, "drop the replication slot and the ClickHouse tables, snapshot again")
	translate := flag.Bool("translate", false, "print the ClickHouse form of the SQL read from stdin and exit")
	layout := flag.String("layout", "", "base sort columns per table before the key, e.g. sims=category,status,carrier_id,store_rate;orders=status")
	flag.IntVar(&cfg.Partitions, "partitions", 16, "hash partitions of tables of a million rows or more (set at snapshot)")
	flag.IntVar(&cfg.CopyStreams, "copy-streams", 4, "connections that copy the initial snapshot")
	flag.IntVar(&cfg.DeltaRows, "delta-rows", 20000, "delta versions from which a table is consolidated into its base")
	httpAddr := flag.String("http", "127.0.0.1:6480", "status endpoint (/shapes); empty = off")
	flag.Parse()
	for _, t := range strings.Split(*tables, ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.Tables = append(cfg.Tables, t)
		}
	}
	cfg.Layout = map[string][]string{}
	for _, spec := range strings.Split(*layout, ";") {
		if name, cols, ok := strings.Cut(strings.TrimSpace(spec), "="); ok {
			cfg.Layout[strings.TrimSpace(name)] = strings.Split(strings.ReplaceAll(cols, " ", ""), ",")
		}
	}
	if *translate {
		in, _ := io.ReadAll(os.Stdin)
		q, err := gateway.Translate(string(in))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(q)
		return
	}
	cfg.Logf = func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000 ")+"staskgw: "+f+"\n", a...)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := gateway.New(ctx, cfg)
	if err != nil {
		cfg.Logf("start: %v", err)
		os.Exit(2)
	}
	if *httpAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/shapes", func(w http.ResponseWriter, _ *http.Request) { r.Report(w) })
		go http.ListenAndServe(*httpAddr, mux)
	}
	if err := r.Serve(ctx); err != nil {
		cfg.Logf("%v", err)
		os.Exit(1)
	}
}
