// e2e is the client side of the end-to-end acceptance scripts (accept/pitr.sh,
// accept/kill9.sh). It drives a running staskd over the PostgreSQL protocol
// and keeps its own record of every transaction, so the server's state can be
// compared with what the client was told.
//
//	e2e write  -url U -log F [-workers 4] [-batch 20] [-size 1000] [-dur 0] [-gen 0]
//	    Concurrent transactions on table ev (insert a batch; now and then
//	    update and delete rows) until -dur, SIGINT, or the first error of a
//	    worker (a killed server). Each transaction is logged with its send and
//	    acknowledge times; one that failed is logged as outcome unknown.
//	e2e cdcmap -url U -slot S -out F
//	    Drains logical slot S (test_decoding) into "commitLSN commitTimeNs
//	    firstInsertedID" lines: the server's own record of every commit.
//	e2e verify -url U -log F -mode exact -cdcmap M (-lsn L | -ts RFC3339)
//	    The table must hold exactly the effect of the logged transactions
//	    committed up to the target: none lost, none extra, rows intact. Also
//	    checks the target against the client's clock (acked before the target:
//	    present; sent after it: absent).
//	e2e verify -url U -log F1,F2,.. -mode acked
//	    After a crash: every acknowledged transaction is present, and what
//	    else is there is, per worker, only the one transaction in flight.
//	e2e ping -url U [-q "select 1"]
//	    Connects with pgx (SCRAM, TLS as the URL says) and runs a query; exit 1 if refused.
//	e2e pick -cdcmap M -frac 0.6
//	    Prints "lsn ts(RFC3339Nano)" of the commit at that fraction of the map.
//
// Exit status 1 on any violation.
package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

type txn struct {
	W   int     `json:"w"`
	N   int     `json:"n"`
	Ins []int64 `json:"ins"`
	Upd []int64 `json:"upd,omitempty"` // each updated by one version
	Del []int64 `json:"del,omitempty"`
	S   int64   `json:"s"`
	A   int64   `json:"a,omitempty"`
	OK  bool    `json:"ok"`
	Err string  `json:"err,omitempty"`
}

func payload(id int64, ver, size int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", id, ver)))
	var sb strings.Builder
	for sb.Len() < size {
		sb.WriteString(hex.EncodeToString(h[:]))
		h = sha256.Sum256(h[:])
	}
	return sb.String()[:size]
}

func sum(p string) string { s := md5.Sum([]byte(p)); return hex.EncodeToString(s[:]) }

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, "FAIL "+f+"\n", a...); os.Exit(1) }

func main() {
	if len(os.Args) < 2 {
		die("usage: e2e write|cdcmap|verify|pick")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	url := fs.String("url", "", "postgres URL")
	logf := fs.String("log", "", "client transaction log")
	workers := fs.Int("workers", 4, "concurrent connections")
	batch := fs.Int("batch", 20, "rows inserted per transaction")
	size := fs.Int("size", 1000, "payload bytes")
	gen := fs.Int("gen", 0, "generation: ids and worker numbers of this run start at gen*workers, so several runs can share a table")
	dur := fs.Duration("dur", 0, "write for this long (0 = until SIGINT)")
	slot := fs.String("slot", "", "slot")
	out := fs.String("out", "", "output file")
	mode := fs.String("mode", "exact", "verify mode: exact|acked")
	cdcmap := fs.String("cdcmap", "", "file from cdcmap")
	lsn := fs.Uint64("lsn", 0, "target commit LSN")
	ts := fs.String("ts", "", "target time (RFC 3339)")
	query := fs.String("q", "select 1", "ping: query; the first column of the first row is printed")
	frac := fs.Float64("frac", 0.5, "pick: fraction of commits")
	fs.Parse(os.Args[2:])
	switch cmd {
	case "write":
		write(*url, *logf, *workers, *batch, *size, *dur, *gen)
	case "cdcmap":
		runCDC(*url, *slot, *out)
	case "verify":
		verify(*url, *logf, *mode, *cdcmap, *lsn, *ts, *size)
	case "pick":
		pick(*cdcmap, *frac)
	case "ping":
		ping(*url, *query)
	default:
		die("unknown command %q", cmd)
	}
}

type stmt struct {
	sql  string
	args []any
}

func write(url, logf string, workers, batch, size int, dur time.Duration, gen int) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if dur > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, dur)
		defer c()
	}
	f, err := os.Create(logf)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	var mu sync.Mutex
	bw := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(bw)
	emit := func(t *txn) { mu.Lock(); enc.Encode(t); mu.Unlock() }
	var acked, failed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w0 int) {
			defer wg.Done()
			conn, err := pgx.Connect(context.Background(), url)
			if err != nil {
				die("worker %d connect: %v", w, err)
			}
			defer conn.Close(context.Background())
			w := gen*workers + w0 // global worker number
			rng := rand.New(rand.NewSource(int64(w) + 1))
			next := int64(w)*1_000_000_000 + 1
			live := map[int64]int{} // id -> version
			var order []int64
			for n := 1; ctx.Err() == nil; n++ {
				t := &txn{W: w, N: n}
				var stmts []stmt
				var vals []string
				var args []any
				for i := 0; i < batch; i++ {
					id := next
					next++
					t.Ins = append(t.Ins, id)
					k := len(args)
					vals = append(vals, fmt.Sprintf("($%d,$%d,0,$%d)", k+1, k+2, k+3))
					args = append(args, id, w, payload(id, 0, size))
				}
				stmts = append(stmts, stmt{"INSERT INTO ev(id,w,ver,payload) VALUES " + strings.Join(vals, ","), args})
				if n%3 == 0 && len(order) > 0 {
					id := order[rng.Intn(len(order))]
					v := live[id] + 1
					t.Upd = append(t.Upd, id)
					stmts = append(stmts, stmt{fmt.Sprintf("UPDATE ev SET ver=%d, payload=$1 WHERE id=%d", v, id), []any{payload(id, v, size)}})
				}
				if n%4 == 0 && len(order) > 200 {
					id := order[0]
					t.Del = append(t.Del, id)
					stmts = append(stmts, stmt{fmt.Sprintf("DELETE FROM ev WHERE id=%d", id), nil})
				}
				t.S = time.Now().UnixNano()
				err := func() error {
					bg := context.Background()
					tx, err := conn.Begin(bg)
					if err != nil {
						return err
					}
					for _, s := range stmts {
						if _, err := tx.Exec(bg, s.sql, s.args...); err != nil {
							tx.Rollback(bg)
							return err
						}
					}
					return tx.Commit(bg)
				}()
				t.A = time.Now().UnixNano()
				if err != nil {
					t.Err = err.Error()
					emit(t)
					failed.Add(1)
					return // outcome unknown: stop this worker
				}
				t.OK = true
				emit(t)
				acked.Add(1)
				for _, id := range t.Ins {
					live[id] = 0
					order = append(order, id)
				}
				for _, id := range t.Upd {
					live[id]++
				}
				for _, id := range t.Del {
					delete(live, id)
					order = order[1:]
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() {
		tk := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-tk.C:
				mu.Lock()
				bw.Flush()
				mu.Unlock()
				fmt.Fprintf(os.Stderr, "%s acked=%d failed=%d\n", time.Now().Format("15:04:05"), acked.Load(), failed.Load())
			case <-done:
				return
			}
		}
	}()
	wg.Wait()
	close(done)
	mu.Lock()
	bw.Flush()
	mu.Unlock()
	fmt.Printf("write: acked=%d unknown=%d\n", acked.Load(), failed.Load())
}

var (
	reCommit = regexp.MustCompile(`^COMMIT (\d+) \(at (.*)\)$`)
	reIns    = regexp.MustCompile(`^table public\.ev: INSERT: id\[bigint\]:(-?\d+) `)
)

func runCDC(url, slot, out string) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		die("%v", err)
	}
	defer conn.Close(ctx)
	f, err := os.Create(out)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	defer bw.Flush()
	first := int64(-1)
	n := 0
	for {
		rows, err := conn.Query(ctx, "SELECT data FROM pg_logical_slot_get_changes($1, NULL, 20000, 'include-timestamp', '1')", slot)
		if err != nil {
			die("get_changes: %v", err)
		}
		got := 0
		for rows.Next() {
			var d string
			rows.Scan(&d)
			got++
			if m := reIns.FindStringSubmatch(d); m != nil {
				if first < 0 {
					first, _ = strconv.ParseInt(m[1], 10, 64)
				}
			} else if m := reCommit.FindStringSubmatch(d); m != nil {
				t, err := time.Parse("2006-01-02 15:04:05.999999-07", strings.TrimSuffix(m[2], "+00")+"+00")
				if err != nil {
					die("commit time %q: %v", m[2], err)
				}
				fmt.Fprintf(bw, "%s %d %d\n", m[1], t.UnixNano(), first)
				first = -1
				n++
			}
		}
		if err := rows.Err(); err != nil {
			die("get_changes: %v", err)
		}
		if got == 0 {
			break
		}
	}
	fmt.Printf("cdcmap: %d commits\n", n)
}

type commit struct {
	lsn uint64
	ts  int64
}

func readMap(path string) (map[int64]commit, []commit) {
	f, err := os.Open(path)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	cm := map[int64]commit{}
	var all []commit
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l uint64
		var t, first int64
		fmt.Sscanf(sc.Text(), "%d %d %d", &l, &t, &first)
		cm[first] = commit{l, t}
		all = append(all, commit{l, t})
	}
	return cm, all
}

func pick(path string, frac float64) {
	_, all := readMap(path)
	if len(all) == 0 {
		die("empty map")
	}
	c := all[int(float64(len(all)-1)*frac)]
	// a time just after the commit: the CDC text has microsecond precision
	fmt.Printf("%d %s\n", c.lsn, time.Unix(0, c.ts+2000).UTC().Format(time.RFC3339Nano))
}

func readLog(path string) []*txn {
	f, err := os.Open(path)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	var ts []*txn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var t txn
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			die("log: %v", err)
		}
		ts = append(ts, &t)
	}
	return ts
}

type state map[int64]int // id -> version

func apply(s state, t *txn) {
	for _, id := range t.Ins {
		s[id] = 0
	}
	for _, id := range t.Upd {
		s[id]++
	}
	for _, id := range t.Del {
		delete(s, id)
	}
}

func dbState(url string) (map[int64][2]string, error) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT id, ver, md5(payload) FROM ev")
	if err != nil {
		return nil, err
	}
	out := map[int64][2]string{}
	for rows.Next() {
		var id int64
		var ver int64
		var h string
		if err := rows.Scan(&id, &ver, &h); err != nil {
			return nil, err
		}
		out[id] = [2]string{strconv.FormatInt(ver, 10), h}
	}
	return out, rows.Err()
}

// compare prints (up to 10) differences between want and the database.
var quiet bool // compare counts but does not print

func compare(want state, db map[int64][2]string, size int, label string) int {
	bad := 0
	rep := func(f string, a ...any) {
		if bad < 10 && !quiet {
			fmt.Printf("  %s: "+f+"\n", append([]any{label}, a...)...)
		}
		bad++
	}
	var ids []int64
	for id := range want {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		r, ok := db[id]
		if !ok {
			rep("LOST id %d", id)
			continue
		}
		if r[0] != strconv.Itoa(want[id]) || r[1] != sum(payload(id, want[id], size)) {
			rep("CORRUPT/STALE id %d: db ver=%s md5=%s, want ver %d", id, r[0], r[1], want[id])
		}
	}
	for id := range db {
		if _, ok := want[id]; !ok {
			rep("EXTRA id %d", id)
		}
	}
	return bad
}

func verify(url, logf, mode, cdcmap string, lsn uint64, tsText string, size int) {
	var txns []*txn
	for _, f := range strings.Split(logf, ",") {
		txns = append(txns, readLog(f)...)
	}
	db, err := dbState(url)
	if err != nil {
		die("read db: %v", err)
	}
	byW := map[int][]*txn{}
	for _, t := range txns {
		byW[t.W] = append(byW[t.W], t)
	}
	for _, ts := range byW {
		sort.Slice(ts, func(i, j int) bool { return ts[i].N < ts[j].N })
	}
	switch mode {
	case "acked":
		bad := 0
		okN, unk := 0, 0
		for w, ts := range byW {
			s := state{}
			var pend *txn
			for _, t := range ts {
				if t.OK {
					apply(s, t)
					okN++
				} else {
					pend = t
					unk++
				}
			}
			dbw := map[int64][2]string{}
			for id, r := range db {
				if int(id/1_000_000_000) == w {
					dbw[id] = r
				}
			}
			quiet = true
			n1 := compare(s, dbw, size, fmt.Sprintf("worker %d", w))
			quiet = false
			if n1 == 0 {
				continue
			}
			if pend != nil {
				s2 := state{}
				for k, v := range s {
					s2[k] = v
				}
				apply(s2, pend)
				quiet = true
				n2 := compare(s2, dbw, size, fmt.Sprintf("worker %d (+in flight)", w))
				quiet = false
				if n2 == 0 {
					fmt.Printf("  worker %d: the in-flight transaction %d did commit\n", w, pend.N)
					continue
				}
			}
			compare(s, dbw, size, fmt.Sprintf("worker %d", w)) // print
			bad += n1
		}
		fmt.Printf("verify acked: %d acknowledged txns, %d in flight, %d rows in db, %d violations\n", okN, unk, len(db), bad)
		if bad > 0 {
			os.Exit(1)
		}
	case "exact":
		cm, all := readMap(cdcmap)
		var lastLSN uint64
		for _, c := range all {
			if c.lsn > lastLSN {
				lastLSN = c.lsn
			}
		}
		var T int64
		if tsText != "" {
			tt, err := time.Parse(time.RFC3339Nano, tsText)
			if err != nil {
				die("-ts: %v", err)
			}
			T = tt.UnixNano()
		}
		in := func(c commit) bool {
			if lsn != 0 {
				return c.lsn <= lsn
			}
			return c.ts <= T
		}
		want := state{}
		inc, exc, notInMap, brk := 0, 0, 0, 0
		for _, ts := range byW {
			stop := false
			for _, t := range ts {
				c, ok := cm[t.Ins[0]]
				if !ok {
					if t.OK {
						notInMap++
					}
					continue
				}
				if in(c) {
					if stop {
						die("worker %d: txn %d is inside the target but an earlier one is not", t.W, t.N)
					}
					apply(want, t)
					inc++
				} else {
					stop = true
					exc++
				}
				if T != 0 {
					if t.OK && t.A < T && !in(c) {
						brk++
						fmt.Printf("  client clock: txn %d/%d acked at %d, before the target %d, yet commit ts %d is after\n", t.W, t.N, t.A, T, c.ts)
					}
					if t.S > T && in(c) {
						brk++
						fmt.Printf("  client clock: txn %d/%d sent after the target but commit ts %d is before\n", t.W, t.N, c.ts)
					}
				}
			}
		}
		bad := compare(want, db, size, "db") + brk
		tgt := fmt.Sprintf("lsn<=%d", lsn)
		if lsn == 0 {
			tgt = "ts<=" + tsText
		}
		fmt.Printf("verify exact %s: %d txns inside the target, %d after it, %d rows expected, %d in db, %d violations (last commit in the map: lsn %d; acked txns missing from the map: %d)\n",
			tgt, inc, exc, len(want), len(db), bad, lastLSN, notInMap)
		if bad > 0 || inc == 0 {
			os.Exit(1)
		}
	}
}

func ping(url, q string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		die("connect: %v", err)
	}
	defer conn.Close(ctx)
	var v any
	if err := conn.QueryRow(ctx, q).Scan(&v); err != nil {
		die("query: %v", err)
	}
	fmt.Println(v)
}
