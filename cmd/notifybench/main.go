// notifybench measures LISTEN/NOTIFY delivery: -listeners connections
// LISTEN on one channel, one sender issues -rate autocommit NOTIFYs per
// second whose payload is the send time, and every listener records the
// latency from send to receipt. Reports per-notification latency
// percentiles and lost or reordered notifications.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	url := flag.String("url", "postgres://app@127.0.0.1:5432/stask?sslmode=disable", "server URL")
	listeners := flag.Int("listeners", 8, "listening connections")
	rate := flag.Int("rate", 1000, "notifications per second")
	dur := flag.Duration("dur", 30*time.Second, "how long to send")
	flag.Parse()
	ctx := context.Background()

	var mu sync.Mutex
	var lats []float64
	reordered := 0
	var wg, ready sync.WaitGroup
	sent := 0
	done := make(chan struct{})
	for i := 0; i < *listeners; i++ {
		c, err := pgx.Connect(ctx, *url)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := c.Exec(ctx, "LISTEN bench"); err != nil {
			log.Fatal(err)
		}
		wg.Add(1)
		ready.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close(ctx)
			ready.Done()
			var mine []float64
			last := int64(-1)
			back := 0
			for {
				wctx, cancel := context.WithTimeout(ctx, time.Second)
				n, err := c.WaitForNotification(wctx)
				cancel()
				if err != nil {
					select {
					case <-done:
						mu.Lock()
						lats = append(lats, mine...)
						reordered += back
						mu.Unlock()
						return
					default:
						continue
					}
				}
				var seq, at int64
				fmt.Sscanf(n.Payload, "%d %d", &seq, &at)
				mine = append(mine, float64(time.Now().UnixNano()-at)/1e6)
				if seq < last {
					back++
				}
				last = max(last, seq)
			}
		}()
	}
	ready.Wait()
	sender, err := pgx.Connect(ctx, *url)
	if err != nil {
		log.Fatal(err)
	}
	t0 := time.Now()
	tick := time.Second / time.Duration(*rate)
	for next := t0; time.Since(t0) < *dur; next = next.Add(tick) {
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
		p := strconv.Itoa(sent) + " " + strconv.FormatInt(time.Now().UnixNano(), 10)
		if _, err := sender.Exec(ctx, "SELECT pg_notify('bench', $1)", p); err != nil {
			log.Fatal(err)
		}
		sent++
	}
	el := time.Since(t0).Seconds()
	time.Sleep(2 * time.Second) // let the tail arrive
	close(done)
	wg.Wait()
	lost := sent**listeners - len(lats)
	sort.Float64s(lats)
	pct := func(p float64) float64 {
		if len(lats) == 0 {
			return math.NaN()
		}
		return lats[min(len(lats)-1, max(0, int(math.Ceil(p*float64(len(lats))))-1))]
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"sent": sent, "sent_per_s": float64(sent) / el, "listeners": *listeners,
		"received": len(lats), "lost": lost, "reordered": reordered,
		"p50_ms": pct(0.5), "p99_ms": pct(0.99), "p999_ms": pct(0.999), "max_ms": pct(1)})
	fmt.Fprintf(os.Stderr, "notify: %d sent (%.0f/s) x %d listeners, %d received, %d lost, %d reordered, latency p50 %.2f p99 %.2f p999 %.2f max %.2f ms\n",
		sent, float64(sent)/el, *listeners, len(lats), lost, reordered, pct(0.5), pct(0.99), pct(0.999), pct(1))
}
