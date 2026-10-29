// staskchaos is the WL-6 harness: it runs staskd as a child process, drives
// concurrent transactional writes, injects failures (kill -9, disk full,
// fsync EIO), restarts, and verifies that every acknowledged transaction
// survived intact and that no transaction is visible partially.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	bin      = flag.String("bin", "./bin/staskd", "staskd binary")
	dir      = flag.String("dir", "/mnt/sda3/stask-data/chaos", "data directory (wiped at start unless -keep)")
	keep     = flag.Bool("keep", false, "keep existing data directory")
	addr     = flag.String("addr", "127.0.0.1:7171", "listen address for the child")
	cycles   = flag.Int("cycles", 20, "fault/restart cycles")
	writers  = flag.Int("writers", 16, "concurrent writers")
	mode     = flag.String("mode", "kill", "kill | write-enospc | create-enospc | sync-eio | mixed")
	segMB    = flag.Int("segment-mb", 16, "segment size passed to staskd")
	minRun   = flag.Duration("min-run", time.Second, "min write time before a fault")
	maxRun   = flag.Duration("max-run", 4*time.Second, "max write time before a fault")
	valBytes = flag.Int("value-size", 512, "value size")
	outFile  = flag.String("out", "", "append JSON summary to this file")
	hot      = flag.Int("hot", 0, "hot rows per writer overwritten by every transaction (creates garbage for log reclamation)")
	extra    = flag.String("server-args", "", "extra staskd flags, space separated (e.g. \"-gc-interval 1s\")")
)

var hc = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}

func call(method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+*addr+path, rd)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		d := json.NewDecoder(resp.Body)
		d.UseNumber()
		d.Decode(out)
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, nil
}

// ---- model of what the client was told ----

type txRec struct {
	seq   int64
	value int64
}

type model struct {
	mu       sync.Mutex
	acked    map[string]txRec // "pair id" -> acknowledged value
	maybe    map[string]int64 // unacknowledged attempts (outcome unknown)
	ackedSeq map[int]int64    // writer -> highest acknowledged counter
	ackedHot map[string]int64 // hot row -> highest acknowledged counter written to it
	attempts atomic.Int64
	acks     atomic.Int64
	failures atomic.Int64
}

type child struct {
	cmd     *exec.Cmd
	started time.Time
}

func startChild() (*child, time.Duration, map[string]any) {
	args := []string{"-dir", *dir, "-addr", *addr, "-enable-faults", "-segment-mb", strconv.Itoa(*segMB),
		"-checkpoint-min-mb", "32"}
	cmd := exec.Command(*bin, append(args, strings.Fields(*extra)...)...)
	logf, _ := os.OpenFile(*dir+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	cmd.Stdout, cmd.Stderr = logf, logf
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	for {
		var st map[string]any
		code, err := call("GET", "/v1/status", nil, &st)
		if err == nil && code == 200 {
			return &child{cmd, t0}, time.Since(t0), st
		}
		if time.Since(t0) > 120*time.Second {
			log.Fatal("child did not become ready")
		}
		if cmd.ProcessState != nil {
			log.Fatal("child exited")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *child) kill9() {
	c.cmd.Process.Signal(syscall.SIGKILL)
	c.cmd.Wait()
}

func writer(w int, m *model, stop *atomic.Bool, wg *sync.WaitGroup, seq *atomic.Int64) {
	defer wg.Done()
	rng := rand.New(rand.NewSource(int64(w) + time.Now().UnixNano()))
	pad := make([]byte, *valBytes)
	for i := range pad {
		pad[i] = 'a' + byte(rng.Intn(26))
	}
	for !stop.Load() {
		n := seq.Add(1)
		v := rng.Int63n(1 << 40)
		id := fmt.Sprintf("p%012d", n)
		m.mu.Lock()
		counter := m.ackedSeq[w] + 1 + rng.Int63n(3)
		m.mu.Unlock()
		// one transaction = two rows of a pair + this writer's counter row
		ops := []map[string]any{
			{"op": "insert", "table": "chaos", "row": map[string]any{"id": id + "a", "v": v, "pad": string(pad)}},
			{"op": "insert", "table": "chaos", "row": map[string]any{"id": id + "b", "v": v, "pad": ""}},
			{"op": "put", "table": "chaos", "row": map[string]any{"id": fmt.Sprintf("w%03d", w), "v": counter, "pad": ""}},
		}
		hotKey := ""
		if *hot > 0 {
			hotKey = fmt.Sprintf("h%03d-%04d", w, rng.Intn(*hot))
			ops = append(ops, map[string]any{"op": "put", "table": "chaos", "row": map[string]any{"id": hotKey, "v": counter, "pad": string(pad)}})
		}
		m.attempts.Add(1)
		var out map[string]any
		code, err := call("POST", "/v1/write", map[string]any{"ops": ops}, &out)
		m.mu.Lock()
		if err == nil && code == 200 {
			m.acked[id] = txRec{n, v}
			if counter > m.ackedSeq[w] {
				m.ackedSeq[w] = counter
			}
			if hotKey != "" && counter > m.ackedHot[hotKey] {
				m.ackedHot[hotKey] = counter
			}
			m.acks.Add(1)
		} else {
			m.maybe[id] = v
			m.failures.Add(1)
		}
		m.mu.Unlock()
		if err != nil || code != 200 {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

type verifyResult struct {
	Rows            int   `json:"rows"`
	AckedChecked    int   `json:"acked_checked"`
	LostAcked       int   `json:"lost_acked"`
	WrongValue      int   `json:"wrong_value"`
	Partial         int   `json:"partial_transactions"`
	UnackedSurvived int   `json:"unacked_survived"`
	CounterRegress  int   `json:"counter_regressions"`
	ScanMs          int64 `json:"scan_ms"`
}

// verify scans the whole table through the strong range-scan API.
func verify(m *model) verifyResult {
	t0 := time.Now()
	rows := map[string]int64{}
	start := ""
	for {
		var out struct {
			Rows []map[string]any `json:"rows"`
		}
		code, err := call("POST", "/v1/query", map[string]any{"kind": "scan", "scan": map[string]any{"table": "chaos", "start": start, "limit": 5000}}, &out)
		if err != nil || code != 200 {
			log.Fatalf("verify scan: %d %v", code, err)
		}
		for _, r := range out.Rows {
			v, _ := r["v"].(json.Number).Int64()
			rows[r["id"].(string)] = v
		}
		if len(out.Rows) < 5000 {
			break
		}
		start = out.Rows[len(out.Rows)-1]["id"].(string) + "\x00"
	}
	var vr verifyResult
	vr.Rows = len(rows)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.acked {
		vr.AckedChecked++
		a, okA := rows[id+"a"]
		b, okB := rows[id+"b"]
		if !okA || !okB {
			vr.LostAcked++
			if vr.LostAcked < 5 {
				log.Printf("LOST acked tx %s (seq %d): a=%v b=%v", id, rec.seq, okA, okB)
			}
			continue
		}
		if a != rec.value || b != rec.value {
			vr.WrongValue++
		}
	}
	for id, v := range m.maybe {
		_, okA := rows[id+"a"]
		_, okB := rows[id+"b"]
		if okA != okB {
			vr.Partial++
		}
		if okA && okB {
			vr.UnackedSurvived++
			if rows[id+"a"] == v {
				// resolved: it committed; treat as acked from now on
				m.acked[id] = txRec{0, v}
				delete(m.maybe, id)
			}
		}
	}
	for id := range rows {
		if len(id) == 14 && id[0] == 'p' {
			pid := id[:13]
			_, a := rows[pid+"a"]
			_, b := rows[pid+"b"]
			if a != b {
				vr.Partial++
			}
		}
	}
	for w, s := range m.ackedSeq {
		if v, ok := rows[fmt.Sprintf("w%03d", w)]; !ok || v < s {
			vr.CounterRegress++
		}
	}
	for k, s := range m.ackedHot {
		if v, ok := rows[k]; !ok || v < s {
			vr.CounterRegress++
			if vr.CounterRegress < 5 {
				log.Printf("hot row %s regressed: have %v (present=%v), acked %d", k, v, ok, s)
			}
		}
	}
	vr.ScanMs = time.Since(t0).Milliseconds()
	return vr
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime)
	if !*keep {
		os.RemoveAll(*dir)
		os.Remove(*dir + ".log")
	}
	m := &model{acked: map[string]txRec{}, maybe: map[string]int64{}, ackedSeq: map[int]int64{}, ackedHot: map[string]int64{}}
	ch, rto, _ := startChild()
	code, err := call("POST", "/v1/tables", map[string]any{"name": "chaos", "key": "id", "columns": []map[string]string{
		{"name": "id", "type": "string"}, {"name": "v", "type": "int"}, {"name": "pad", "type": "string"}}}, nil)
	if err != nil || (code != 200 && code != 400) {
		log.Fatalf("create table: %d %v", code, err)
	}
	var seq atomic.Int64
	seq.Store(time.Now().Unix() * 1000)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	type cycleOut struct {
		Cycle    int            `json:"cycle"`
		Fault    string         `json:"fault"`
		Acked    int64          `json:"acked_total"`
		RTOms    float64        `json:"restart_to_ready_ms"`
		Recovery map[string]any `json:"recovery"`
		Verify   verifyResult   `json:"verify"`
		Note     string         `json:"note,omitempty"`
	}
	var results []cycleOut
	var rtos []float64
	totalLost, totalPartial, totalWrong, totalRegress := 0, 0, 0, 0
	for cyc := 1; cyc <= *cycles; cyc++ {
		fault := *mode
		if fault == "mixed" {
			fault = []string{"kill", "kill", "write-enospc", "create-enospc", "sync-eio"}[rng.Intn(5)]
		}
		stop := &atomic.Bool{}
		var wg sync.WaitGroup
		for w := 0; w < *writers; w++ {
			wg.Add(1)
			go writer(w, m, stop, &wg, &seq)
		}
		time.Sleep(*minRun + time.Duration(rng.Int63n(int64(*maxRun-*minRun)+1)))
		note := ""
		switch fault {
		case "kill":
		case "write-enospc", "sync-eio":
			key := map[string]string{"write-enospc": "write_enospc", "sync-eio": "sync_eio"}[fault]
			call("POST", "/v1/admin/fault", map[string]any{key: true}, nil)
			time.Sleep(500 * time.Millisecond)
			var st map[string]any
			call("GET", "/v1/status", nil, &st)
			code, _ := call("GET", "/v1/get?table=chaos&key=w000", nil, nil)
			note = fmt.Sprintf("write_failed=%v reads_still_served=%v", st["write_failed"] != "", code == 200 || code == 404)
		case "create-enospc":
			// segment allocation fails: writes are rejected cleanly, the
			// engine stays up; clearing the fault resumes writes w/o restart
			call("POST", "/v1/admin/fault", map[string]any{"create_enospc": true}, nil)
			acks0 := m.acks.Load()
			time.Sleep(3 * time.Second)
			var st map[string]any
			call("GET", "/v1/status", nil, &st)
			failsDuring := m.failures.Load()
			call("POST", "/v1/admin/fault", map[string]any{"create_enospc": false}, nil)
			acks1 := m.acks.Load()
			time.Sleep(time.Second)
			note = fmt.Sprintf("engine_failed=%v acks_during_fault=%d failures_so_far=%d acks_after_clear=%d",
				st["write_failed"] != "", acks1-acks0, failsDuring, m.acks.Load()-acks1)
		}
		ch.kill9() // power-cut equivalent for the process (page cache survives; see docs)
		stop.Store(true)
		wg.Wait()
		ch2, rt, st := startChild()
		ch = ch2
		vr := verify(m)
		rec, _ := st["recovery"].(map[string]any)
		results = append(results, cycleOut{cyc, fault, m.acks.Load(), float64(rt.Microseconds()) / 1000, rec, vr, note})
		rtos = append(rtos, float64(rt.Microseconds())/1000)
		totalLost += vr.LostAcked
		totalPartial += vr.Partial
		totalWrong += vr.WrongValue
		totalRegress += vr.CounterRegress
		log.Printf("cycle %d fault=%s acked=%d rows=%d lost=%d wrong=%d partial=%d regress=%d unacked_survived=%d restart=%v %s",
			cyc, fault, m.acks.Load(), vr.Rows, vr.LostAcked, vr.WrongValue, vr.Partial, vr.CounterRegress, vr.UnackedSurvived, rt, note)
	}
	ch.kill9()
	sort.Float64s(rtos)
	sum := map[string]any{
		"workload": "WL-6 chaos", "mode": *mode, "cycles": *cycles, "writers": *writers,
		"attempted_tx": m.attempts.Load(), "acked_tx": m.acks.Load(), "failed_or_unknown_tx": m.failures.Load(),
		"lost_acked_total": totalLost, "wrong_value_total": totalWrong, "partial_tx_total": totalPartial,
		"counter_regressions_total": totalRegress, "rpo0_holds": totalLost == 0 && totalWrong == 0 && totalPartial == 0 && totalRegress == 0,
		"restart_ms_p50": rtos[len(rtos)/2], "restart_ms_max": rtos[len(rtos)-1], "first_start_ms": float64(rto.Microseconds()) / 1000,
		"cycles_detail": results,
	}
	b, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(b))
	if *outFile != "" {
		f, _ := os.OpenFile(*outFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		c, _ := json.Marshal(sum)
		f.Write(append(c, '\n'))
		f.Close()
	}
}
