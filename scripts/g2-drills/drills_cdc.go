package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- D7: stalled and slow CDC consumers ----
//
// Three logical slots on one node that is rewritten continuously (so that old
// log segments hold only dead versions and would be reclaimed if no slot held
// them): "healthy" confirms every transaction at once, "slow" confirms one
// transaction every 25 ms (slower than the writers), "stalled" never reads.
// The engine is started with -slot-max-lag-mb 12. Expected (docs/26, core/cdc.go
// checkSlots): the lag of the stalled and the slow slot grows, the engine
// invalidates a slot that holds back more than the limit at its next cleaning
// round and reclaims the log, the healthy slot is untouched, writes never fail.

type cdcConsumer struct {
	n     *Node
	slot  string
	delay time.Duration
	stop  atomic.Bool
	wg    sync.WaitGroup
	acked atomic.Int64
	err   atomic.Value
	code  atomic.Int64
}

func (c *cdcConsumer) start() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			for !c.stop.Load() {
				time.Sleep(100 * time.Millisecond)
			}
			cancel()
		}()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+c.n.HTTP+"/v1/cdc/"+c.slot+"/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.err.Store(err.Error())
			return
		}
		defer resp.Body.Close()
		c.code.Store(int64(resp.StatusCode))
		if resp.StatusCode != 200 {
			b := make([]byte, 300)
			k, _ := resp.Body.Read(b)
			c.err.Store(string(b[:k]))
			return
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		hc := &http.Client{Timeout: 10 * time.Second}
		var lsn string
		var n int
		lastAck := time.Now()
		for sc.Scan() && !c.stop.Load() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if cm, _ := m["commit"].(bool); !cm {
				continue
			}
			lsn = fmt.Sprint(m["lsn"])
			if f, ok := m["lsn"].(float64); ok {
				lsn = fmt.Sprintf("%.0f", f)
			}
			if c.delay > 0 {
				time.Sleep(c.delay)
			} else if n++; n%200 != 0 && time.Since(lastAck) < 30*time.Millisecond {
				continue // confirm in batches: a confirmation covers everything before it
			}
			r, err := hc.Post("http://"+c.n.HTTP+"/v1/cdc/"+c.slot+"/ack?lsn="+lsn, "application/json", nil)
			if err != nil {
				c.err.Store(err.Error())
				return
			}
			r.Body.Close()
			lastAck = time.Now()
			c.acked.Add(1)
		}
	}()
}

func (c *cdcConsumer) halt() { c.stop.Store(true); c.wg.Wait() }

type slotSample struct {
	t       time.Duration
	lag     map[string]float64
	walDisk float64
}

func drillD7(e *Env, o *Opts, r *Result, mode string) {
	const limitMB = 12
	t := newTopoOpt(e, "d7", "single", TopoOpts{Plain: true, Extra: []string{"-segment-mb", "4", "-gc-interval", "1s", "-checkpoint-min-mb", "4",
		"-slot-max-lag-mb", fmt.Sprint(limitMB), "-gc-max-live", "0.5"}})
	n := t.Nodes[0]
	if !r.Check(t.UpAll() == nil, "start") {
		return
	}
	if err := exec1(n.URL(), "CREATE TABLE hot (id bigint PRIMARY KEY, n int NOT NULL, payload text NOT NULL)"); !r.Check(err == nil, "table: %v", err) {
		return
	}
	for i := 0; i < 300; i++ {
		exec1(n.URL(), fmt.Sprintf("INSERT INTO hot VALUES (%d, 0, 'x')", i))
	}
	for _, s := range []string{"healthy", "slow", "stalled"} {
		if err := exec1(n.URL(), fmt.Sprintf("SELECT pg_create_logical_replication_slot('%s', 'wal2json')", s)); !r.Check(err == nil, "slot %s: %v", s, err) {
			return
		}
	}
	healthy := &cdcConsumer{n: n, slot: "healthy"}
	slow := &cdcConsumer{n: n, slot: "slow", delay: 25 * time.Millisecond}
	healthy.start()
	slow.start()
	// writers rewrite the hot rows with 1 KB payloads
	var werr, wok atomic.Int64
	var stopW atomic.Bool
	var ww sync.WaitGroup
	payload := strings.Repeat("p", 1000)
	for w := 0; w < 4; w++ {
		ww.Add(1)
		go func(w int) {
			defer ww.Done()
			c, err := connect(n.URL())
			if err != nil {
				werr.Add(1)
				return
			}
			defer c.Close(context.Background())
			for i := 0; !stopW.Load(); i++ {
				id := (w*75 + i) % 300
				if _, err := c.Exec(context.Background(), "UPDATE hot SET n = n + 1, payload = $1 WHERE id = $2", payload, id); err != nil {
					werr.Add(1)
					time.Sleep(10 * time.Millisecond)
				} else {
					wok.Add(1)
				}
			}
		}(w)
	}
	// sample the metrics
	var samples []slotSample
	t0 := time.Now()
	invalidAt := map[string]time.Duration{}
	deadline := 150 * time.Second
	var peak = map[string]float64{}
	var peakAll = map[string]float64{}
	for time.Since(t0) < deadline {
		time.Sleep(500 * time.Millisecond)
		m, err := n.Metrics()
		if err != nil {
			continue
		}
		s := slotSample{t: time.Since(t0), lag: map[string]float64{}, walDisk: m["stask_wal_disk_bytes"]}
		for _, sl := range []string{"healthy", "slow", "stalled"} {
			s.lag[sl] = m[fmt.Sprintf("stask_cdc_slot_lag_bytes{slot=%q}", sl)]
			if s.lag[sl] > peakAll[sl] {
				peakAll[sl] = s.lag[sl]
			}
			if _, inv := invalidAt[sl]; !inv && s.lag[sl] > peak[sl] {
				peak[sl] = s.lag[sl] // held back while the slot was still valid
			}
		}
		samples = append(samples, s)
		b, _, _ := n.Get("/v1/cdc")
		var list []struct {
			Name    string `json:"name"`
			Invalid string `json:"invalid"`
		}
		json.Unmarshal([]byte(b), &list)
		for _, x := range list {
			if x.Invalid != "" {
				if _, ok := invalidAt[x.Name]; !ok {
					invalidAt[x.Name] = s.t
					r.Logf("slot %s invalidated after %v: %s (lag at the last sample %.1f MB)", x.Name, s.t.Round(time.Second), x.Invalid, s.lag[x.Name]/1048576)
				}
			}
		}
		if len(invalidAt) >= 2 && time.Since(t0) > invalidAt["stalled"]+15*time.Second {
			break
		}
	}
	// settle: keep writing a little more so the cleaner runs
	time.Sleep(5 * time.Second)
	stopW.Store(true)
	ww.Wait()
	healthy.halt()
	slow.halt()
	if !r.Check(len(samples) > 0, "no samples") {
		return
	}
	last := samples[len(samples)-1]
	// 1. the lag of the stalled slot grew until it was invalidated
	inv, invOK := invalidAt["stalled"]
	r.Check(invOK, "the stalled slot was never invalidated in %v (peak lag %.1f MB, limit %d MB)", deadline, peak["stalled"]/1048576, limitMB)
	grew := 0
	var prev float64
	for _, s := range samples {
		if invOK && s.t >= inv {
			break
		}
		if s.lag["stalled"] >= prev {
			grew++
		}
		prev = s.lag["stalled"]
	}
	pre := 0
	for _, s := range samples {
		if !invOK || s.t < inv {
			pre++
		}
	}
	// the invalidation happens at the first cleaning round after the limit is crossed, so the lag is sampled
	// at most one round (1 s of writes) past it
	r.Check(peak["stalled"] > float64(limitMB)*1048576/2, "the lag of the stalled slot only reached %.1f MB", peak["stalled"]/1048576)
	r.Check(grew >= pre-1, "the lag of the stalled slot did not grow monotonically before the invalidation (%d of %d samples did not decrease)", grew, pre)
	// 2. retention is bounded: peak lag within limit + slack, wal bytes bounded after
	slack := float64(40 << 20)
	r.Check(peak["stalled"] <= float64(limitMB<<20)+slack, "the stalled slot held back %.1f MB, more than the limit of %d MB + %.0f MB slack", peak["stalled"]/1048576, limitMB, slack/1048576)
	var maxWal float64
	for _, s := range samples {
		if s.walDisk > maxWal {
			maxWal = s.walDisk
		}
	}
	r.Check(last.walDisk <= float64(limitMB<<20)+slack, "the log still holds %.1f MB at the end (limit %d MB)", last.walDisk/1048576, limitMB)
	r.Logf("log on disk: peak %.1f MB, at the end %.1f MB; lag held back while valid: stalled slot %.1f MB, slow slot %.1f MB, healthy slot %.2f MB (healthy at the end %.2f MB); the metric of an invalidated slot goes on growing (stalled: %.1f MB at its peak) because it is the distance to a frozen position, it no longer holds the log back",
		maxWal/1048576, last.walDisk/1048576, peak["stalled"]/1048576, peak["slow"]/1048576, peak["healthy"]/1048576, last.lag["healthy"]/1048576, peakAll["stalled"]/1048576)
	// 3. the healthy slot is untouched, the slow one fell behind
	_, healthyInvalid := invalidAt["healthy"]
	r.Check(!healthyInvalid, "the healthy slot was invalidated")
	r.Check(healthy.acked.Load() > 100, "the healthy consumer confirmed only %d transactions", healthy.acked.Load())
	_, slowInvalid := invalidAt["slow"]
	r.Logf("slow consumer confirmed %d transactions and was %s; healthy consumer confirmed %d", slow.acked.Load(), map[bool]string{true: "invalidated", false: "kept"}[slowInvalid], healthy.acked.Load())
	// 4. writes never failed
	r.Check(werr.Load() == 0, "%d writes failed during the drill (%d ok)", werr.Load(), wok.Load())
	// 5. a consumer of an invalid slot is told
	b, code, _ := n.Get("/v1/cdc/stalled/stream")
	r.Check(code == 410, "reading the invalidated slot answered %d %s, want 410 Gone", code, trunc(strings.TrimSpace(b), 100))
	// 6. reconcile --cdc-slot says so
	for _, sl := range []string{"healthy", "stalled"} {
		rep, _, err := n.Reconcile(0, "-cdc-slot", sl, "-table", "hot")
		if !r.Check(err == nil && rep != nil && rep.Slot != nil, "reconcile -cdc-slot %s: %v", sl, err) {
			continue
		}
		r.Logf("reconcile -cdc-slot %s: %s confirmed_lsn=%d durable_lsn=%d behind %d records, %d bytes; %s", sl, rep.Slot.Status, rep.Slot.ConfirmedLSN, rep.Slot.DurableLSN, rep.Slot.BehindRecs, rep.Slot.LagBytes, rep.Slot.Detail)
		if sl == "stalled" {
			r.Check(rep.Slot.Status == "INVALID", "reconcile reports the invalidated slot as %s", rep.Slot.Status)
		}
	}
	// 7. the data are fine
	rep, err := n.WaitInSync(60 * time.Second)
	r.Check(err == nil && rep.Verdict == "IN_SYNC", "views after the drill: %v", err)
	scrubCheck(r, n)
	r.OracleOK("stalled slot: the lag it held back grew to %.1f MB (limit %d MB) and the engine invalidated it after %v, the log never exceeded %.1f MB; the slow slot was %s; the healthy slot was never touched (lag %.2f MB at the end); %d writes ok, 0 failed; the invalidated slot answers 410 and reconcile -cdc-slot reports INVALID", peak["stalled"]/1048576, limitMB, inv.Round(time.Second), maxWal/1048576, map[bool]string{true: "invalidated too", false: "kept"}[slowInvalid], last.lag["healthy"]/1048576, wok.Load())
}
