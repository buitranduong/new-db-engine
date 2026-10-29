// cdcbench consumes a CDC slot over HTTP (GET /v1/cdc/{slot}/stream) for a
// while, confirming what it received every -ack interval, and reports the
// change rate and the lag from commit to receipt (wal2json timestamps; run
// it on the server's machine so that both clocks are the same). It checks
// that transactions arrive in commit order.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:7070", "staskd HTTP base URL")
	slot := flag.String("slot", "bench", "slot name (created with wal2json if missing)")
	dur := flag.Duration("dur", 30*time.Second, "how long to consume")
	ack := flag.Duration("ack", time.Second, "confirm interval")
	drop := flag.Bool("drop", false, "drop the slot at the end")
	flag.Parse()

	body, _ := json.Marshal(map[string]string{"name": *slot, "plugin": "wal2json"})
	if r, err := http.Post(*base+"/v1/cdc", "application/json", bytes.NewReader(body)); err != nil {
		log.Fatal(err)
	} else {
		r.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", *base+"/v1/cdc/"+*slot+"/stream?include-timestamp=1&include-xids=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("stream: %s", resp.Status)
	}

	var mu sync.Mutex
	lastCommit := ""
	acks := 0
	go func() { // confirm periodically, as a consumer that applied up to there
		t := time.NewTicker(*ack)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			lsn := lastCommit
			mu.Unlock()
			if lsn == "" {
				continue
			}
			if r, err := http.Post(*base+"/v1/cdc/"+*slot+"/ack?lsn="+lsn, "", nil); err == nil {
				r.Body.Close()
				mu.Lock()
				acks++
				mu.Unlock()
			}
		}
	}()

	type msg struct {
		LSN  string `json:"lsn"`
		XID  uint64 `json:"xid"`
		Data struct {
			Action    string `json:"action"`
			Timestamp string `json:"timestamp"`
		} `json:"data"`
		Commit bool   `json:"commit"`
		Error  string `json:"error"`
	}
	var lags []float64
	changes, txs := map[string]int{}, 0
	var lastXID uint64
	order := 0
	t0 := time.Now()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	// only commit lines are decoded (the consumer must not be the bottleneck)
	actionAt := []byte(`"action":"`)
	for sc.Scan() {
		b := sc.Bytes()
		i := bytes.Index(b, actionAt)
		if i < 0 {
			log.Fatalf("stream: %s", b) // an error line
		}
		switch a := b[i+len(actionAt)]; a {
		case 'I', 'U', 'D':
			changes[string(a)]++
		case 'C':
			var m msg
			if err := json.Unmarshal(b, &m); err != nil {
				log.Fatalf("bad line %q: %v", b, err)
			}
			txs++
			if m.XID <= lastXID {
				order++
			}
			lastXID = m.XID
			if ts, err := time.Parse("2006-01-02 15:04:05.999999-07", m.Data.Timestamp); err == nil {
				lags = append(lags, float64(time.Since(ts).Microseconds())/1000)
			}
			mu.Lock()
			lastCommit = m.LSN
			mu.Unlock()
		}
	}
	el := time.Since(t0).Seconds()
	sort.Float64s(lags)
	pct := func(p float64) float64 {
		if len(lags) == 0 {
			return math.NaN()
		}
		return lags[min(len(lags)-1, int(math.Ceil(p*float64(len(lags))))-1)]
	}
	n := changes["I"] + changes["U"] + changes["D"]
	mu.Lock()
	res := map[string]any{"seconds": el, "transactions": txs, "changes": n, "inserts": changes["I"], "updates": changes["U"],
		"deletes": changes["D"], "changes_per_s": float64(n) / el, "lag_p50_ms": pct(0.5), "lag_p99_ms": pct(0.99),
		"lag_max_ms": pct(1), "acks": acks, "out_of_order": order}
	mu.Unlock()
	json.NewEncoder(os.Stdout).Encode(res)
	fmt.Fprintf(os.Stderr, "cdc: %d tx, %d changes (%.0f/s; I %d U %d D %d), lag p50 %.1f p99 %.1f max %.1f ms, %d acks, %d out of order\n",
		txs, n, float64(n)/el, changes["I"], changes["U"], changes["D"], pct(0.5), pct(0.99), pct(1), acks, order)
	if *drop {
		r, _ := http.NewRequest("DELETE", *base+"/v1/cdc/"+*slot, nil)
		if resp, err := http.DefaultClient.Do(r); err == nil {
			resp.Body.Close()
		}
	}
}
