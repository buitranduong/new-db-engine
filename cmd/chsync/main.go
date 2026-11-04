// chsync: keep ClickHouse in step with PostgreSQL, one source transaction at
// a time, readable at a watermark (docs/35).
//
//	chsync run    -source URL -ch URL -tables a,b,c   # snapshot once, then follow
//	chsync verify -source URL -ch URL -tables a,b,c   # quiet source: compare every row at W
//	chsync status -source URL -ch URL                 # W, source position, lag
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"

	"stask/internal/chsync"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func logf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000 ")+"chsync: "+f+"\n", a...)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: chsync run|verify|status [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	var cfg chsync.Config
	fs.StringVar(&cfg.SourceURL, "source", env("CHSYNC_SOURCE", "postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable"), "source PostgreSQL URL (the system of record)")
	fs.StringVar(&cfg.CHURL, "ch", env("CHSYNC_CH", "http://127.0.0.1:8123"), "ClickHouse HTTP URL")
	tables := fs.String("tables", env("CHSYNC_TABLES", ""), "comma-separated public tables")
	fs.StringVar(&cfg.Slot, "slot", "chsync", "logical replication slot (also the key of the watermark in ClickHouse)")
	fs.StringVar(&cfg.Publication, "publication", "chsync_pub", "publication")
	fs.IntVar(&cfg.BatchRows, "batch-rows", 50000, "flush once this many row versions are pending")
	fs.DurationVar(&cfg.BatchWait, "batch-wait", 200*time.Millisecond, "flush once the oldest pending transaction is this old")
	fs.BoolVar(&cfg.Reset, "reset", false, "run: drop the slot and the ClickHouse tables and snapshot again")
	wait := fs.Duration("wait", 2*time.Minute, "verify: how long to wait for W to reach the source position")
	fs.Parse(os.Args[2:])
	for _, t := range strings.Split(*tables, ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.Tables = append(cfg.Tables, t)
		}
	}
	cfg.Logf = logf
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "run":
		if len(cfg.Tables) == 0 {
			logf("-tables is required")
			os.Exit(2)
		}
		backoff := time.Second
		for {
			t0 := time.Now()
			err := chsync.Run(ctx, cfg)
			if ctx.Err() != nil {
				logf("stopped")
				return
			}
			if chsync.IsFatal(err) {
				logf("FATAL: %v", err)
				os.Exit(1)
			}
			if time.Since(t0) > time.Minute {
				backoff = time.Second
			}
			logf("error: %v; reconnecting in %s", err, backoff)
			cfg.Reset = false
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 10*time.Second)
		}
	case "verify":
		rep, err := chsync.Verify(ctx, cfg, *wait)
		if err != nil {
			logf("verify: %v", err)
			os.Exit(2)
		}
		rep.Write(os.Stdout)
		if !rep.OK {
			os.Exit(1)
		}
	case "status":
		ch := chsync.NewCH(cfg.CHURL)
		w, ok, err := chsync.ReadW(ctx, ch, cfg.Slot)
		if err != nil {
			logf("status: %v", err)
			os.Exit(2)
		}
		pc, err := pgconn.Connect(ctx, cfg.SourceURL)
		if err != nil {
			logf("source: %v", err)
			os.Exit(2)
		}
		r := pc.ExecParams(ctx, "SELECT pg_current_wal_lsn()::text", nil, nil, nil, nil).Read()
		pc.Close(ctx)
		if r.Err != nil {
			logf("source: %v", r.Err)
			os.Exit(2)
		}
		cur, _ := pglogrepl.ParseLSN(string(r.Rows[0][0]))
		lag := int64(cur) - int64(w)
		fmt.Printf("snapshot_done=%v W=%s source=%s lag_bytes=%d\n", ok, pglogrepl.LSN(w), cur, max(lag, 0))
	default:
		fmt.Fprintln(os.Stderr, "usage: chsync run|verify|status [flags]")
		os.Exit(2)
	}
}
