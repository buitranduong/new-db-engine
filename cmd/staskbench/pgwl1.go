package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// wl1PG runs the WL-1 mix over the PostgreSQL protocol (-pg url): the same
// keys, values, mix and recorder as over HTTP, one connection per client,
// prepared statements (pgx's default statement cache). The same client
// drives STASK's PG port and PostgreSQL itself, so the two are compared
// with an identical client (docs/12 P0.2).
func wl1PG(url string) {
	ctx := context.Background()
	pool := make([]string, 256)
	prng := rand.New(rand.NewSource(42))
	for i := range pool {
		pool[i] = randString(prng, *valSize)
	}
	key := func(i int) string { return fmt.Sprintf("k%09d", i) }
	dial := func() *pgx.Conn {
		c, err := pgx.Connect(ctx, url)
		if err != nil {
			log.Fatal(err)
		}
		return c
	}
	res := map[string]any{"mode": "pg"}
	if !*skipLoad {
		c := dial()
		// fails harmlessly on a fresh server
		c.Exec(ctx, `DROP TABLE IF EXISTS kv`)
		if _, err := c.Exec(ctx, `CREATE TABLE kv (id text PRIMARY KEY, n bigint, val text)`); err != nil {
			log.Fatal(err)
		}
		t0 := time.Now()
		var wg sync.WaitGroup
		var next atomic.Int64
		for l := 0; l < 8; l++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := dial()
				defer c.Close(ctx)
				var sb strings.Builder
				var args []any
				for {
					lo := int(next.Add(200)) - 200
					if lo >= *nKeys {
						return
					}
					hi := min(lo+200, *nKeys)
					sb.Reset()
					args = args[:0]
					sb.WriteString("INSERT INTO kv (id, n, val) VALUES ")
					for i := lo; i < hi; i++ {
						if i > lo {
							sb.WriteByte(',')
						}
						p := len(args)
						fmt.Fprintf(&sb, "($%d,$%d,$%d)", p+1, p+2, p+3)
						args = append(args, key(i), int64(i), pool[i%len(pool)])
					}
					if _, err := c.Exec(ctx, sb.String(), args...); err != nil {
						log.Fatalf("load: %v", err)
					}
				}
			}()
		}
		wg.Wait()
		d := time.Since(t0)
		res["load_s"] = d.Seconds()
		res["load_rows_per_s"] = float64(*nKeys) / d.Seconds()
		// PostgreSQL: settle like the pgbench baseline (bench/pg/wl1_schema.sql);
		// STASK rejects these, which is fine
		for _, q := range []string{`VACUUM ANALYZE kv`, `CHECKPOINT`} {
			c.Exec(ctx, q)
		}
		c.Close(ctx)
	}
	conns := make([]*pgx.Conn, *conc)
	for i := range conns {
		conns[i] = dial()
	}
	var insSeq atomic.Int64
	insSeq.Store(time.Now().UnixNano() / 1000)
	recs, el := run(*conc, *dur, *rate, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
		c := conns[w]
		p := rng.Intn(100)
		switch {
		case p < 70:
			var v string
			err := c.QueryRow(ctx, `SELECT val FROM kv WHERE id = $1`, key(rng.Intn(*nKeys))).Scan(&v)
			return "read", err == nil
		case p < 90:
			tag, err := c.Exec(ctx, `UPDATE kv SET n = $2, val = $3 WHERE id = $1`,
				key(rng.Intn(*nKeys)), rng.Int63(), pool[rng.Intn(len(pool))])
			return "update", err == nil && tag.RowsAffected() == 1
		default:
			tag, err := c.Exec(ctx, `INSERT INTO kv (id, n, val) VALUES ($1, 0, $2)`,
				fmt.Sprintf("i%03d-%012d", w, insSeq.Add(1)), pool[rng.Intn(len(pool))])
			return "insert", err == nil && tag.RowsAffected() == 1
		}
	})
	res["ops"] = recs.summary(el)
	res["timeline_max_ms"] = timeline(recs)
	report("WL-1 point operations", res)
}
