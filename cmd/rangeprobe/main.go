// rangeprobe times the statements of one stask-m2 import batch (docs/14 rangefix row) on a loaded server:
// EXPLAIN of each statement, then the real batch (UPDATE, INSERT..SELECT, seq, journal) inside a
// transaction that is rolled back (or committed with -commit). Usage:
//
//	rangeprobe -dsn postgres://... -rows 5000 -offset 100000 [-analyze] [-commit]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	dsn := flag.String("dsn", "postgres://postgres:wm@127.0.0.1:29810/postgres?sslmode=disable", "server")
	rows := flag.Int("rows", 5000, "rows per batch")
	off := flag.Int("offset", 100000, "rank of the first key of the range")
	analyze := flag.Bool("analyze", false, "EXPLAIN ANALYZE (executes the statement, inside the rolled back transaction)")
	commit := flag.Bool("commit", false, "commit instead of rollback")
	xs := flag.String("x", "", "run these statements (separated by ;;) in one transaction, timing each, then roll back")
	flag.Parse()
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(*dsn)
	must(err)
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c, err := pgx.ConnectConfig(ctx, cfg)
	must(err)
	defer c.Close(ctx)
	if *xs != "" {
		tx, err := c.Begin(ctx)
		must(err)
		for _, q := range strings.Split(*xs, ";;") {
			t0 := time.Now()
			ct, err := tx.Exec(ctx, q)
			if err != nil {
				fmt.Printf("%.0f ms ERR %v: %s\n", ms(t0), err, q)
				continue
			}
			fmt.Printf("%.0f ms (%d rows): %s\n", ms(t0), ct.RowsAffected(), q)
		}
		tx.Rollback(ctx)
		return
	}
	var lo, hi string
	must(c.QueryRow(ctx, fmt.Sprintf("SELECT number FROM sims ORDER BY number OFFSET %d LIMIT 1", *off)).Scan(&lo))
	must(c.QueryRow(ctx, fmt.Sprintf("SELECT number FROM sims ORDER BY number OFFSET %d LIMIT 1", *off+*rows)).Scan(&hi))
	fmt.Printf("range [%s, %s) = %d rows\n", lo, hi, *rows)
	upd := fmt.Sprintf("UPDATE sims SET price = 500000 + ((price * 7 + 3) %% 4000000), updated_at = now() WHERE number >= '%s' AND number < '%s'", lo, hi)
	ins := fmt.Sprintf("INSERT INTO history (id, sim_number, changed_at, field, old_value, new_value) SELECT 900000000000 + CAST(substr(number, 3) AS bigint), number, now(), 'price', NULL, CAST(price AS text) FROM sims WHERE number >= '%s' AND number < '%s'", lo, hi)
	explain := func(tx pgx.Tx, q string) {
		p := "EXPLAIN "
		if *analyze {
			p = "EXPLAIN ANALYZE "
		}
		t0 := time.Now()
		r, err := tx.Query(ctx, p+q)
		must(err)
		var lines []string
		for r.Next() {
			var s string
			must(r.Scan(&s))
			lines = append(lines, "    "+s)
		}
		must(r.Err())
		fmt.Printf("%s (%.0f ms)\n%s\n", p+q[:min(60, len(q))], ms(t0), strings.Join(lines, "\n"))
	}
	tx, err := c.Begin(ctx)
	must(err)
	explain(tx, upd)
	must(tx.Rollback(ctx))
	tx, err = c.Begin(ctx)
	must(err)
	explain(tx, ins)
	must(tx.Rollback(ctx))
	// the real batch
	tx, err = c.Begin(ctx)
	must(err)
	t0 := time.Now()
	ct, err := tx.Exec(ctx, upd)
	must(err)
	fmt.Printf("UPDATE: %d rows, %.0f ms\n", ct.RowsAffected(), ms(t0))
	explain(tx, ins) // plan after the transaction's own writes
	sel := ins[strings.Index(ins, "SELECT"):]
	ts := time.Now()
	rr, err := tx.Query(ctx, sel)
	must(err)
	nsel := 0
	for rr.Next() {
		nsel++
	}
	must(rr.Err())
	fmt.Printf("SELECT part alone: %d rows, %.0f ms\n", nsel, ms(ts))
	t1 := time.Now()
	ct, err = tx.Exec(ctx, ins)
	must(err)
	fmt.Printf("INSERT..SELECT: %d rows, %.0f ms\n", ct.RowsAffected(), ms(t1))
	t2 := time.Now()
	var seq int64
	must(tx.QueryRow(ctx, "UPDATE m2_seq SET v = v + 1 WHERE id = 1 RETURNING v").Scan(&seq))
	_, err = tx.Exec(ctx, fmt.Sprintf("INSERT INTO m2_journal (seq, txid, kind, payload) VALUES (%d, 1, 'probe', '{}')", seq+1000000000))
	must(err)
	fmt.Printf("seq+journal: %.0f ms\n", ms(t2))
	t3 := time.Now()
	if *commit {
		must(tx.Commit(ctx))
	} else {
		must(tx.Rollback(ctx))
	}
	fmt.Printf("end: %.0f ms; whole batch %.0f ms\n", ms(t3), ms(t0))
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
func must(err error) {
	if err != nil {
		fmt.Println("ERROR:", err)
		os.Exit(1)
	}
}
