// staskbulk measures bulk INSERT and UPDATE of N rows over the PostgreSQL
// protocol while concurrent readers run point SELECTs. The same binary runs
// against staskd and against PostgreSQL, for a like-for-like comparison.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
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

type lat struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *lat) add(d time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
}

func (l *lat) summary() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return map[string]any{"n": 0}
	}
	sort.Slice(l.d, func(i, j int) bool { return l.d[i] < l.d[j] })
	q := func(p float64) float64 {
		return float64(l.d[min(int(math.Ceil(p*float64(len(l.d))))-1, len(l.d)-1)].Microseconds()) / 1000
	}
	return map[string]any{"n": len(l.d), "p50_ms": q(0.5), "p99_ms": q(0.99), "p999_ms": q(0.999), "max_ms": q(1)}
}

type errs struct {
	mu sync.Mutex
	m  map[string]int
}

func (e *errs) add(err error) {
	k := err.Error()
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		k = pe.Code + " " + pe.Message
	}
	e.mu.Lock()
	if e.m == nil {
		e.m = map[string]int{}
	}
	e.m[k]++
	e.mu.Unlock()
}

func state(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

var statuses = []string{"new", "paid", "shipped", "cancelled"}

// row i is deterministic so runs against both systems store the same data.
func rowArgs(i int, args []any) []any {
	r := rand.New(rand.NewPCG(uint64(i), 42))
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
	note := fmt.Sprintf("Đơn hàng số %d của khách %d, giao tới quận %d", i, r.IntN(1_000_000), r.IntN(24)+1)
	return append(args, int64(i), int64(r.IntN(1_000_000)), math.Round(r.Float64()*1e6)/100,
		statuses[r.IntN(len(statuses))], note, created)
}

func main() {
	url := flag.String("url", "postgres://app@127.0.0.1:17432/stask?sslmode=disable", "connection URL")
	n := flag.Int("n", 1_000_000, "rows")
	workers := flag.Int("workers", 32, "writer connections")
	batch := flag.Int("batch", 100, "rows per INSERT/UPDATE statement (1 = one row per statement)")
	readers := flag.Int("readers", 16, "concurrent SELECT connections during writes (0 = none)")
	readRate := flag.Int("read-rate", 0, "total SELECTs/s across readers (0 = as fast as possible)")
	phases := flag.String("phases", "insert,update", "insert, update, update-all, verify, read")
	readFor := flag.Duration("read-for", 15*time.Second, "read phase: how long the readers run over the existing rows [0, n)")
	label := flag.String("label", "", "label")
	flag.Parse()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, fmt.Sprintf("%s&pool_max_conns=%d", *url, *workers+*readers+2))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	report := map[string]any{"label": *label, "rows": *n, "workers": *workers, "batch": *batch, "readers": *readers, "read_rate": *readRate}

	var maxID atomic.Int64 // rows [0, maxID) are known to exist
	stopReaders := func() func() map[string]any {
		if *readers == 0 {
			return func() map[string]any { return nil }
		}
		var l lat
		var e errs
		var notFound atomic.Int64
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < *readers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(w), 9))
				var interval time.Duration
				next := time.Now()
				if *readRate > 0 {
					interval = time.Duration(float64(time.Second) * float64(*readers) / float64(*readRate))
					next = next.Add(time.Duration(r.Int64N(int64(interval))))
				}
				for {
					if interval > 0 {
						// open loop: fixed schedule, latency measured from the intended start
						if d := time.Until(next); d > 0 {
							time.Sleep(d)
						}
					}
					select {
					case <-stop:
						return
					default:
					}
					hi := maxID.Load()
					if hi == 0 {
						time.Sleep(time.Millisecond)
						continue
					}
					id := r.Int64N(hi)
					var amount float64
					var status string
					s := time.Now()
					if interval > 0 {
						s = next
						next = next.Add(interval)
					}
					err := pool.QueryRow(ctx, `SELECT amount, status FROM orders WHERE id = $1`, id).Scan(&amount, &status)
					switch {
					case errors.Is(err, pgx.ErrNoRows):
						notFound.Add(1)
					case err != nil:
						e.add(err)
					default:
						l.add(time.Since(s))
					}
				}
			}(w)
		}
		return func() map[string]any {
			close(stop)
			wg.Wait()
			s := l.summary()
			s["errors"] = e.m
			s["not_found"] = notFound.Load() // must be 0: committed rows are always visible
			return s
		}
	}

	exec := func(name string, stmt func(lo, hi int) (string, []any)) map[string]any {
		var done atomic.Int64
		var e errs
		var retries atomic.Int64
		var l lat
		next := atomic.Int64{}
		var wmu sync.Mutex
		finished := map[int]int{}
		stopR := stopReaders()
		t0 := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < *workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					lo := int(next.Add(int64(*batch))) - *batch
					if lo >= *n {
						return
					}
					hi := min(lo+*batch, *n)
					q, args := stmt(lo, hi)
					for {
						s := time.Now()
						_, err := pool.Exec(ctx, q, args...)
						if st := state(err); st == "40001" || st == "40P01" {
							retries.Add(1)
							e.add(err)
							continue
						}
						if err != nil {
							e.add(err)
							break
						}
						l.add(time.Since(s))
						done.Add(int64(hi - lo))
						break
					}
					if name == "insert" {
						// readers may target any id below the lowest unfinished batch
						wmu.Lock()
						finished[lo] = hi
						for {
							h, ok := finished[int(maxID.Load())]
							if !ok {
								break
							}
							delete(finished, int(maxID.Load()))
							maxID.Store(int64(h))
						}
						wmu.Unlock()
					}
				}
			}()
		}
		var prog sync.WaitGroup
		quit := make(chan struct{})
		prog.Add(1)
		go func() {
			defer prog.Done()
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-quit:
					return
				case <-t.C:
					log.Printf("%s: %d/%d rows, %.0f rows/s", name, done.Load(), *n, float64(done.Load())/time.Since(t0).Seconds())
				}
			}
		}()
		wg.Wait()
		wall := time.Since(t0)
		close(quit)
		prog.Wait()
		if name == "insert" {
			maxID.Store(int64(*n))
		}
		res := map[string]any{"rows_done": done.Load(), "wall_s": wall.Seconds(), "rows_per_s": float64(done.Load()) / wall.Seconds(),
			"stmt_latency": l.summary(), "errors": e.m, "retries": retries.Load(), "readers": stopR()}
		log.Printf("%s: %d rows in %v (%.0f rows/s), retries %d, errors %v", name, done.Load(), wall.Round(time.Millisecond),
			float64(done.Load())/wall.Seconds(), retries.Load(), e.m)
		return res
	}

	for _, ph := range strings.Split(*phases, ",") {
		switch ph {
		case "insert":
			pool.Exec(ctx, `DROP TABLE IF EXISTS orders`) // PostgreSQL only; STASK starts from an empty directory
			if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS orders (id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL,
				amount DOUBLE PRECISION, status TEXT, note TEXT, created TIMESTAMPTZ)`); err != nil {
				log.Fatal(err)
			}
			report["insert"] = exec("insert", func(lo, hi int) (string, []any) {
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
			})
		case "update":
			report["update"] = exec("update", func(lo, hi int) (string, []any) {
				if hi-lo == 1 {
					return `UPDATE orders SET amount = amount + 1, status = 'paid' WHERE id = $1`, []any{int64(lo)}
				}
				ids := make([]string, 0, hi-lo)
				for i := lo; i < hi; i++ {
					ids = append(ids, fmt.Sprint(i))
				}
				return `UPDATE orders SET amount = amount + 1, status = 'paid' WHERE id IN (` + strings.Join(ids, ",") + `)`, nil
			})
		case "update-all":
			// one statement, one transaction, every row
			stopR := stopReaders()
			t0 := time.Now()
			tag, err := pool.Exec(ctx, `UPDATE orders SET amount = amount + 1`)
			r := map[string]any{"wall_s": time.Since(t0).Seconds(), "tag": tag.String()}
			if err != nil {
				r["error"] = err.Error()
			}
			r["readers"] = stopR()
			log.Printf("update-all: %v in %v (err %v)", tag, time.Since(t0).Round(time.Millisecond), err)
			report["update_all"] = r
		case "read":
			// readers only, in their own process, over rows loaded earlier
			maxID.Store(int64(*n))
			stopR := stopReaders()
			time.Sleep(*readFor)
			r := stopR()
			log.Printf("read: %v", r)
			report["read"] = r
		case "verify":
			// count and checksum, to compare before and after a restart
			var cnt int64
			var sum float64
			err := pool.QueryRow(ctx, `SELECT count(*), sum(amount) FROM orders`).Scan(&cnt, &sum)
			log.Printf("verify: count=%d sum(amount)=%.2f err=%v", cnt, sum, err)
			report["verify"] = map[string]any{"count": cnt, "sum_amount": sum}
		}
	}
	var cnt int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&cnt); err == nil {
		report["final_count"] = cnt
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
	os.Exit(0)
}
