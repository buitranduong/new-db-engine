package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The bank workload: transfers between hot accounts (conflicts), upserts and
// deletes of volatile accounts, rolled-back transactions, savepoint
// rollbacks and COPY, each transaction leaving "ev" marker rows that name
// the writer and the sequence number, so a consumer can check every
// committed transaction arrived once, whole, and in order, and no
// rolled-back one arrived.

const bankDDL1 = `CREATE TABLE acct (id bigint PRIMARY KEY, bal bigint NOT NULL, ver int NOT NULL DEFAULT 0, note text)`
const bankDDL2 = `CREATE TABLE ev (id bigint PRIMARY KEY, w int NOT NULL, seq int NOT NULL, k int NOT NULL, kind text NOT NULL)`

func evID(w, seq, k int) int64 { return int64(w)*10_000_000 + int64(seq)*10 + int64(k) }

// Rec describes the committed transaction of one writer sequence number.
type Rec struct {
	Kind    string
	ExpEv   int
	ExpAcct int // exact number of acct changes, -1: 0 or 1 (delete of a maybe absent row)
}

type seqKey struct{ W, Seq int }

// WLog is what the writers know about their own transactions.
type WLog struct {
	mu      sync.Mutex
	Acked   map[seqKey]Rec
	Unknown map[seqKey]Rec // the connection broke at COMMIT: outcome not known
	Rolled  map[seqKey]string
	Errs    []string
	Retries atomic.Int64
	Done    atomic.Int64
}

func newWLog() *WLog {
	return &WLog{Acked: map[seqKey]Rec{}, Unknown: map[seqKey]Rec{}, Rolled: map[seqKey]string{}}
}

// WCfg configures one writer.
type WCfg struct {
	URL      string
	W        int
	N        int
	Accts    int
	Vol      int
	Seed     int64
	Notify   string // channel for pg_notify markers ("" none)
	Kinds    []string
	Think    time.Duration
	Cluster  bool // connections may break (failover): reconnect, unknown commits
	Stop     <-chan struct{}
	Progress func(done int)
}

var allKinds = []string{"transfer", "transfer", "transfer", "transfer", "upsert", "upsert", "delete", "rollback", "savepoint", "copy"}

func retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return strings.HasPrefix(pe.Code, "40") || pe.Code == "55P03" || pe.Code == "25006" || pe.Code == "57P01"
	}
	return false
}

func connBroken(err error) bool {
	if err == nil {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "25006" || pe.Code == "57P01" || pe.Code == "08006"
	}
	return true // not an SQL error: network
}

// runWriter runs cfg.N transactions; it returns when done or stopped.
func runWriter(cfg WCfg, wl *WLog) {
	rng := rand.New(rand.NewSource(cfg.Seed*1000 + int64(cfg.W)))
	kinds := cfg.Kinds
	if len(kinds) == 0 {
		kinds = allKinds
	}
	var c *pgx.Conn
	connect := func() bool {
		for i := 0; i < 400; i++ {
			select {
			case <-cfg.Stop:
				return false
			default:
			}
			ctx, cancel := context.WithTimeout(bg(), 5*time.Second)
			cc, err := pgx.Connect(ctx, cfg.URL)
			cancel()
			if err == nil {
				if _, err = cc.Exec(bg(), "SET TimeZone TO 'UTC'"); err == nil {
					c = cc
					return true
				}
				cc.Close(bg())
			}
			time.Sleep(250 * time.Millisecond)
		}
		return false
	}
	if !connect() {
		wl.fail("writer %d: cannot connect", cfg.W)
		return
	}
	defer func() {
		if c != nil {
			c.Close(bg())
		}
	}()
	for seq := 0; seq < cfg.N; seq++ {
		select {
		case <-cfg.Stop:
			return
		default:
		}
		kind := kinds[rng.Intn(len(kinds))]
		// the random choices of one transaction are fixed before the first attempt
		a := rng.Intn(cfg.Accts)
		b := (a + 1 + rng.Intn(cfg.Accts-1)) % cfg.Accts
		amt := 1 + rng.Intn(50)
		vid := 1000 + rng.Intn(cfg.Vol)
		bal := rng.Intn(100000)
		for attempt := 0; ; attempt++ {
			if attempt > 200 {
				wl.fail("writer %d seq %d (%s): gave up after %d attempts", cfg.W, seq, kind, attempt)
				return
			}
			if c == nil || c.IsClosed() {
				if !connect() {
					wl.fail("writer %d seq %d: reconnect failed", cfg.W, seq)
					return
				}
			}
			rec, rolled, err, commitSent := doTx(c, cfg, kind, seq, a, b, amt, vid, bal)
			key := seqKey{cfg.W, seq}
			if err == nil {
				wl.mu.Lock()
				if rolled {
					wl.Rolled[key] = kind
				} else {
					wl.Acked[key] = rec
				}
				wl.mu.Unlock()
				break
			}
			if commitSent && connBroken(err) {
				wl.mu.Lock()
				wl.Unknown[key] = rec
				wl.mu.Unlock()
				c.Close(bg())
				c = nil
				break
			}
			if retryable(err) || (cfg.Cluster && connBroken(err)) {
				wl.Retries.Add(1)
				if connBroken(err) {
					c.Close(bg())
					c = nil
				}
				time.Sleep(time.Duration(5+rng.Intn(20)) * time.Millisecond)
				continue
			}
			wl.fail("writer %d seq %d (%s): %v", cfg.W, seq, kind, err)
			return
		}
		wl.Done.Add(1)
		if cfg.Progress != nil {
			cfg.Progress(seq + 1)
		}
		if cfg.Think > 0 {
			time.Sleep(cfg.Think)
		}
	}
}

func (wl *WLog) fail(f string, a ...any) {
	wl.mu.Lock()
	wl.Errs = append(wl.Errs, fmt.Sprintf(f, a...))
	wl.mu.Unlock()
}

func doTx(c *pgx.Conn, cfg WCfg, kind string, seq, a, b, amt, vid, bal int) (rec Rec, rolled bool, err error, commitSent bool) {
	ctx := bg()
	tx, err := c.Begin(ctx)
	if err != nil {
		return rec, false, err, false
	}
	abort := func(e error) (Rec, bool, error, bool) {
		tx.Rollback(ctx)
		return rec, false, e, false
	}
	w := cfg.W
	ev := func(k int, kd string) error {
		_, e := tx.Exec(ctx, "INSERT INTO ev (id, w, seq, k, kind) VALUES ($1,$2,$3,$4,$5)", evID(w, seq, k), w, seq, k, kd)
		return e
	}
	xfer := func() error {
		if _, e := tx.Exec(ctx, "UPDATE acct SET bal = bal - $1, ver = ver + 1 WHERE id = $2", amt, a); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, "UPDATE acct SET bal = bal + $1, ver = ver + 1 WHERE id = $2", amt, b)
		return e
	}
	notify := func(tag string) error {
		if cfg.Notify == "" {
			return nil
		}
		_, e := tx.Exec(ctx, "SELECT pg_notify($1, $2)", cfg.Notify, fmt.Sprintf("%d:%d%s", w, seq, tag))
		return e
	}
	switch kind {
	case "transfer":
		rec = Rec{Kind: kind, ExpEv: 3, ExpAcct: 2}
		for k := 0; k < 3; k++ {
			if e := ev(k, "m"); e != nil {
				return abort(e)
			}
		}
		if e := xfer(); e != nil {
			return abort(e)
		}
	case "upsert":
		rec = Rec{Kind: kind, ExpEv: 2, ExpAcct: 1}
		for k := 0; k < 2; k++ {
			if e := ev(k, "m"); e != nil {
				return abort(e)
			}
		}
		if _, e := tx.Exec(ctx, "INSERT INTO acct (id, bal, ver, note) VALUES ($1,$2,1,$3) ON CONFLICT (id) DO UPDATE SET bal = EXCLUDED.bal, ver = acct.ver + 1, note = EXCLUDED.note",
			vid, bal, fmt.Sprintf("w%d s%d", w, seq)); e != nil {
			return abort(e)
		}
	case "delete":
		rec = Rec{Kind: kind, ExpEv: 1, ExpAcct: -1}
		if e := ev(0, "m"); e != nil {
			return abort(e)
		}
		if _, e := tx.Exec(ctx, "DELETE FROM acct WHERE id = $1", vid); e != nil {
			return abort(e)
		}
	case "rollback":
		if e := ev(0, "rb"); e != nil {
			return abort(e)
		}
		if e := xfer(); e != nil {
			return abort(e)
		}
		if e := notify(":rb"); e != nil {
			return abort(e)
		}
		if e := tx.Rollback(ctx); e != nil {
			return rec, false, e, false
		}
		return rec, true, nil, false
	case "savepoint":
		rec = Rec{Kind: kind, ExpEv: 2, ExpAcct: 0}
		if e := ev(0, "m"); e != nil {
			return abort(e)
		}
		if _, e := tx.Exec(ctx, "SAVEPOINT s1"); e != nil {
			return abort(e)
		}
		if e := ev(1, "sp"); e != nil {
			return abort(e)
		}
		if _, e := tx.Exec(ctx, "INSERT INTO acct (id, bal, ver) VALUES ($1,$2,1) ON CONFLICT (id) DO UPDATE SET bal = EXCLUDED.bal", vid, bal); e != nil {
			return abort(e)
		}
		if e := notify(":sp"); e != nil {
			return abort(e)
		}
		if _, e := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s1"); e != nil {
			return abort(e)
		}
		if e := ev(2, "m"); e != nil {
			return abort(e)
		}
	case "copy":
		rec = Rec{Kind: kind, ExpEv: 3, ExpAcct: 2}
		rows := [][]any{}
		for k := 0; k < 3; k++ {
			rows = append(rows, []any{evID(w, seq, k), int32(w), int32(seq), int32(k), "m"})
		}
		if _, e := tx.Conn().CopyFrom(ctx, pgx.Identifier{"ev"}, []string{"id", "w", "seq", "k", "kind"}, pgx.CopyFromRows(rows)); e != nil {
			return abort(e)
		}
		if e := xfer(); e != nil {
			return abort(e)
		}
	}
	if e := notify(""); e != nil {
		return abort(e)
	}
	if e := tx.Commit(ctx); e != nil {
		return rec, false, e, true
	}
	return rec, false, nil, true
}

// runWriters runs n writers to completion.
func runWriters(cfg WCfg, n int, wl *WLog) {
	var wg sync.WaitGroup
	for w := 1; w <= n; w++ {
		c := cfg
		c.W = w
		wg.Add(1)
		go func() { defer wg.Done(); runWriter(c, wl) }()
	}
	wg.Wait()
}

func seedBank(t *T, c *pgx.Conn, accts int) {
	t.Helper()
	ex(t, c, "BEGIN")
	for i := 0; i < accts; i++ {
		ex(t, c, fmt.Sprintf("INSERT INTO acct (id, bal) VALUES (%d, 1000)", i))
	}
	ex(t, c, "COMMIT")
}

// ---- verification of a stream against the writers' record ----

// dedupe removes transactions delivered again (same commit LSN) and counts them;
// it also reports the ones whose duplicate differs from the first delivery.
func dedupe(txs []*TxEvt) (out []*TxEvt, dups int, mismatches []string) {
	seen := map[uint64]*TxEvt{}
	for _, t := range txs {
		if f, ok := seen[uint64(t.CommitLSN)]; ok {
			dups++
			if chgLines(f) != chgLines(t) { // relation messages are per session: not compared
				mismatches = append(mismatches, fmt.Sprintf("tx at %v redelivered differently", t.CommitLSN))
			}
			continue
		}
		seen[uint64(t.CommitLSN)] = t
		out = append(out, t)
	}
	return
}

type streamCheck struct {
	Acked, Unknown, Rolled, Txs, Changes int
}

// verifyBank checks a deduplicated, ordered stream of the bank tables:
// every acknowledged transaction arrived once, whole; no rolled-back or
// savepoint-rolled-back change arrived; commit LSNs strictly increase;
// each writer's transactions arrive in its own order.
func verifyBank(t *T, txs []*TxEvt, wl *WLog, requireAll bool) streamCheck {
	t.Helper()
	var sc streamCheck
	seen := map[seqKey]int{} // tx index
	evCount := map[seqKey]int{}
	acctCount := map[seqKey]int{}
	lastSeq := map[int]int{}
	var last uint64
	ids := map[int64]bool{}
	bad := 0
	report := func(f string, a ...any) {
		if bad < 8 {
			t.Errorf(f, a...)
		}
		bad++
	}
	for i, tx := range txs {
		if uint64(tx.CommitLSN) <= last {
			report("commit order: tx %d has LSN %v not above the previous %x", i, tx.CommitLSN, last)
		}
		last = uint64(tx.CommitLSN)
		var mine []seqKey
		acct := 0
		for ci := range tx.Chgs {
			c := &tx.Chgs[ci]
			sc.Changes++
			switch c.Rel {
			case "acct":
				acct++
			case "ev":
				if c.Op != 'I' {
					report("ev change %s (only inserts expected)", c.String())
					continue
				}
				var id int64
				fmt.Sscan(string(c.New["id"].Data), &id)
				kind := string(c.New["kind"].Data)
				if ids[id] {
					report("ev id %d delivered twice (tx %v)", id, tx.CommitLSN)
				}
				ids[id] = true
				w := int(id / 10_000_000)
				seq := int(id % 10_000_000 / 10)
				if id == markerID {
					continue // the end-of-run marker is not a writer's transaction
				}
				if kind == "rb" || kind == "sp" {
					report("change of a rolled back (%s) transaction arrived: %s", kind, c.String())
				}
				k := seqKey{w, seq}
				if len(mine) == 0 || mine[len(mine)-1] != k {
					mine = append(mine, k)
				}
				evCount[k]++
			}
		}
		if len(mine) > 1 {
			// two writers' markers in one transaction cannot happen
			uniq := map[seqKey]bool{}
			for _, k := range mine {
				uniq[k] = true
			}
			if len(uniq) > 1 {
				report("tx %v mixes markers of %v", tx.CommitLSN, mine)
			}
		}
		for _, k := range mine {
			if j, ok := seen[k]; ok && j != i {
				report("transaction %v split over two commits (%d and %d)", k, j, i)
			}
			seen[k] = i
			acctCount[k] = acct
			if k.Seq <= lastSeq[k.W] && lastSeq[k.W] != 0 {
				report("writer %d: seq %d committed after seq %d (order)", k.W, k.Seq, lastSeq[k.W])
			}
			lastSeq[k.W] = max(lastSeq[k.W], k.Seq)
		}
		sc.Txs++
	}
	wl.mu.Lock()
	defer wl.mu.Unlock()
	for k, r := range wl.Acked {
		if _, ok := seen[k]; !ok {
			if requireAll {
				report("acknowledged transaction %v (%s) never arrived", k, r.Kind)
			}
			continue
		}
		sc.Acked++
		if evCount[k] != r.ExpEv {
			report("transaction %v (%s): %d ev rows, want %d", k, r.Kind, evCount[k], r.ExpEv)
		}
		if r.ExpAcct >= 0 && acctCount[k] != r.ExpAcct {
			report("transaction %v (%s): %d acct changes, want %d", k, r.Kind, acctCount[k], r.ExpAcct)
		}
		if r.ExpAcct < 0 && acctCount[k] > 1 {
			report("transaction %v (%s): %d acct changes, want 0 or 1", k, r.Kind, acctCount[k])
		}
	}
	for k, r := range wl.Unknown {
		if _, ok := seen[k]; ok {
			sc.Unknown++
			if evCount[k] != r.ExpEv {
				report("transaction %v (%s, outcome unknown): %d ev rows, want %d (torn)", k, r.Kind, evCount[k], r.ExpEv)
			}
		}
	}
	for k := range seen {
		_, a := wl.Acked[k]
		_, u := wl.Unknown[k]
		if r, ok := wl.Rolled[k]; ok {
			sc.Rolled++
			report("rolled back transaction %v (%s) arrived", k, r)
		} else if !a && !u {
			report("transaction %v arrived but no writer committed it", k)
		}
	}
	if bad > 8 {
		t.Errorf("... %d problems in total", bad)
	}
	return sc
}

func chgLines(t *TxEvt) string {
	var l []string
	for i := range t.Chgs {
		l = append(l, t.Chgs[i].String())
	}
	return strings.Join(l, "\n")
}
