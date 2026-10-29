// numbench measures the CRM aggregate over a money column (numeric) on a
// STASK columnar table, a STASK row table and PostgreSQL 16 (docs/14,
// task colnumeric). It loads n SIMs by COPY, then times the aggregates.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dsn := flag.String("dsn", "", "connection string")
	n := flag.Int("n", 1_000_000, "SIM rows")
	mode := flag.String("mode", "columnar", "columnar | row (table kind; PG ignores)")
	reps := flag.Int("reps", 15, "repetitions of each query")
	sys := flag.String("system", "stask", "stask | pg")
	ptype := flag.String("price", "numeric(12,2)", "price column type")
	flag.Parse()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	with := ""
	if *sys == "stask" && *mode == "columnar" {
		with = " WITH (columnar = true)"
	}
	pool.Exec(ctx, "DROP TABLE IF EXISTS sims")
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE sims (number text PRIMARY KEY, carrier text, status text, price %s, updated_at bigint)%s`, *ptype, with))
	must(err)

	carriers := []string{"viettel", "mobifone", "vinaphone", "vietnamobile", "gmobile"}
	statuses := []string{"available", "reserved", "sold", "blocked"}
	rng := rand.New(rand.NewSource(42))
	pr, pw := io.Pipe()
	go func() {
		var b bytes.Buffer
		for i := 0; i < *n; i++ {
			price := int64(rng.Intn(5_000_000))*100 + int64(rng.Intn(100)) // up to 5,000,000.99
			if rng.Intn(50) == 0 {
				price = -price / 10
			}
			priceS := fmt.Sprintf("%d.%02d", price/100, abs(price%100))
			if price < 0 && price/100 == 0 {
				priceS = "-" + priceS
			}
			if rng.Intn(100) == 0 {
				priceS = "" // NULL (csv empty)
			}
			fmt.Fprintf(&b, "09%08d,%s,%s,%s,%d\n", i, carriers[rng.Intn(len(carriers))], statuses[rng.Intn(len(statuses))], priceS, 1_790_000_000+i)
			if b.Len() > 1<<20 {
				pw.Write(b.Bytes())
				b.Reset()
			}
		}
		pw.Write(b.Bytes())
		pw.Close()
	}()
	conn, err := pool.Acquire(ctx)
	must(err)
	t0 := time.Now()
	tag, err := conn.Conn().PgConn().CopyFrom(ctx, pr, "COPY sims FROM STDIN WITH (FORMAT csv)")
	must(err)
	conn.Release()
	load := time.Since(t0)
	fmt.Printf("load %d rows: %.2fs (%.0f rows/s)\n", tag.RowsAffected(), load.Seconds(), float64(*n)/load.Seconds())

	queries := []struct{ name, sql string }{
		{"crm_sum", `SELECT carrier, status, count(*), sum(price) FROM sims GROUP BY carrier, status ORDER BY 1, 2`},
		{"crm_all", `SELECT carrier, status, count(*), sum(price), avg(price), min(price), max(price) FROM sims GROUP BY carrier, status ORDER BY 1, 2`},
		{"crm_filter", `SELECT carrier, status, count(*), sum(price) FROM sims WHERE price > 1000000.50 AND price < 4000000 GROUP BY carrier, status ORDER BY 1, 2`},
		{"crm_count", `SELECT carrier, status, count(*) FROM sims GROUP BY carrier, status ORDER BY 1, 2`},
		{"total", `SELECT count(*), sum(price), avg(price) FROM sims`},
	}
	for _, q := range queries {
		var ds []time.Duration
		var first string
		for r := 0; r < *reps; r++ {
			t := time.Now()
			rows, err := pool.Query(ctx, q.sql)
			must(err)
			var sb strings.Builder
			for rows.Next() {
				vals, err := rows.Values()
				must(err)
				fmt.Fprintln(&sb, vals...)
			}
			must(rows.Err())
			d := time.Since(t)
			if r == 0 {
				first = sb.String()
			} else {
				if sb.String() != first {
					fmt.Println("RESULT CHANGED")
				}
				ds = append(ds, d)
			}
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		fmt.Printf("%-11s p50 %7.1f ms  min %7.1f ms  max %7.1f ms\n", q.name, ms(ds[len(ds)/2]), ms(ds[0]), ms(ds[len(ds)-1]))
		if q.name == "crm_all" || q.name == "total" {
			fmt.Print(indent(first))
		}
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ") + "\n"
}
