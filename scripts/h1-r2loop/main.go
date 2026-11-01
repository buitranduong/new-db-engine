// h1-r2loop: R2 alone (docs/34 H1). One connection, simple protocol, the canonical R2 (cmd/stask-m2 R2Query) pages 1-3 in a
// loop; prints client-side p50/p99/mean per page and an md5 of each page (oracle equality between hybrid and non-hybrid runs).
package main

import (
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func q(page int) string {
	lo := func(q int) string { return fmt.Sprintf("t.rn BETWEEN %d AND %d", (page-1)*q+1, page*q) }
	return fmt.Sprintf(`SELECT t.number, c.code, st.name, t.price, t.store_rate FROM (
  SELECT number, carrier_id, store_id, price, store_rate,
         row_number() OVER (PARTITION BY carrier_id ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3)) t
JOIN carriers c ON c.id = t.carrier_id JOIN stores st ON st.id = t.store_id
WHERE (t.carrier_id = 1 AND %s) OR (t.carrier_id = 2 AND %s) OR (t.carrier_id = 3 AND %s)
ORDER BY t.store_rate DESC, md5(t.number || '42'), t.number`, lo(30), lo(50), lo(20))
}

func main() {
	dsn := flag.String("dsn", "postgres://postgres:wm@127.0.0.1:28852/postgres?sslmode=disable", "")
	n := flag.Int("n", 300, "rounds (3 pages each)")
	warm := flag.Int("warm", 30, "warm-up rounds")
	flag.Parse()
	ctx := context.Background()
	cc, err := pgx.ParseConfig(*dsn)
	if err != nil {
		panic(err)
	}
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		panic(err)
	}
	defer c.Close(ctx)
	lat := [3][]float64{}
	for i := -*warm; i < *n; i++ {
		for p := 1; p <= 3; p++ {
			t0 := time.Now()
			rows, err := c.Query(ctx, q(p))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			var sb strings.Builder
			nr := 0
			for rows.Next() {
				v, _ := rows.Values()
				fmt.Fprint(&sb, v...)
				sb.WriteString("\n")
				nr++
			}
			if rows.Err() != nil {
				fmt.Fprintln(os.Stderr, rows.Err())
				os.Exit(1)
			}
			d := time.Since(t0)
			if i == 0 {
				fmt.Printf("page %d rows=%d md5=%x\n", p, nr, md5.Sum([]byte(sb.String())))
			}
			if i >= 0 {
				lat[p-1] = append(lat[p-1], float64(d.Microseconds())/1000)
			}
		}
	}
	all := []float64{}
	for p := 0; p < 3; p++ {
		all = append(all, lat[p]...)
		report(fmt.Sprintf("page %d", p+1), lat[p])
	}
	report("all", all)
}

func report(name string, x []float64) {
	sort.Float64s(x)
	s := 0.0
	for _, v := range x {
		s += v
	}
	fmt.Printf("client %-7s n=%d p50=%.2f ms p99=%.2f ms mean=%.2f ms\n", name, len(x), x[len(x)/2], x[len(x)*99/100], s/float64(len(x)))
}
