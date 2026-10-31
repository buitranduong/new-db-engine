package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A bank-style multi-table transaction workload with a correctness oracle.
//
// Each transaction (writer w, sequence seq) moves amt from account A to
// account B and records the two legs in ev:
//
//	UPDATE acct (A: bal-amt, ver+1, note), UPDATE acct (B: bal+amt, ver+1, note),
//	INSERT ev (w,seq,k=0 debit A), INSERT ev (w,seq,k=1 credit B)
//
// Some transactions write their ev rows and then ROLL BACK. The client keeps
// a ledger: acknowledged (COMMIT returned), outcome unknown (the connection
// broke while COMMIT was in flight), rolled back. The oracle then requires:
//
//	exactly once   every acknowledged (w,seq) has exactly its two ev rows
//	atomic         no (w,seq) has one row; an unknown one has 0 or 2
//	no phantom     no ev row of a rolled-back or never-issued (w,seq)
//	invariants     per account bal = 1000 + credits - debits and
//	               ver = number of legs; total balance is unchanged
//
// The columnar, trigram, secondary-index and history views are compared with
// the core by the reconcile tool (Node.WaitInSync).

const (
	nAccts  = 16
	initBal = 1000
	bankDDL = `CREATE TABLE acct (id bigint PRIMARY KEY, bal bigint NOT NULL, ver int NOT NULL DEFAULT 0, note text) WITH (columnar = true, trigram = 'note')`
	evDDL   = `CREATE TABLE ev (id bigint PRIMARY KEY, w int NOT NULL, seq int NOT NULL, k int NOT NULL, kind text NOT NULL, acct bigint NOT NULL, amt bigint NOT NULL) WITH (columnar = true)`
)

func evID(w, seq, k int) int64 { return int64(w)*10_000_000 + int64(seq)*10 + int64(k) }

type Leg struct {
	A, B int
	Amt  int64
}

type Ledger struct {
	mu     sync.Mutex
	Acked  map[[2]int]Leg
	Maybe  map[[2]int]Leg
	Rolled map[[2]int]bool
	Next   map[int]int // next sequence per writer
	Fails  []string    // problems seen by writers that are not outcomes (an UPDATE touching no row)
}

func NewLedger() *Ledger {
	return &Ledger{Acked: map[[2]int]Leg{}, Maybe: map[[2]int]Leg{}, Rolled: map[[2]int]bool{}, Next: map[int]int{}}
}

func (l *Ledger) Counts() (acked, maybe, rolled int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.Acked), len(l.Maybe), len(l.Rolled)
}

// BankSetup creates and seeds the tables (plus a secondary index on each).
func BankSetup(url string) error {
	if err := exec1(url, bankDDL, evDDL, "CREATE INDEX acct_ver_idx ON acct (ver)", "CREATE INDEX ev_acct_idx ON ev (acct)"); err != nil {
		return err
	}
	c, err := connect(url)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	for i := 0; i < nAccts; i++ {
		if _, err := c.Exec(context.Background(), "INSERT INTO acct (id, bal, ver, note) VALUES ($1, $2, 0, $3)", i, initBal, fmt.Sprintf("acct-%d", i)); err != nil {
			return err
		}
	}
	return nil
}

type outcome int

const (
	outAcked outcome = iota
	outRolled
	outRetry   // definitely not committed: try the same sequence again
	outUnknown // COMMIT was in flight when the connection broke
	outFailed  // never reached COMMIT: not committed, reconnect
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Workload runs writers until Stop.
type Workload struct {
	URL     string
	L       *Ledger
	Writers int
	// Dial, if set, opens a connection to the node that takes writes (a
	// multi-host DSN gives up on the whole list when its first host hangs).
	Dial    func() (*pgx.Conn, error)
	Timeout time.Duration // per transaction attempt (default 20 s)
	stop    atomic.Bool
	wg      sync.WaitGroup
	Errs    atomic.Int64
	LastErr atomic.Value
}

func NewWorkload(url string, l *Ledger, writers int) *Workload {
	return &Workload{URL: url, L: l, Writers: writers}
}

func (wl *Workload) Start(seed int64) {
	for w := 0; w < wl.Writers; w++ {
		wl.wg.Add(1)
		go wl.writer(w, rand.New(rand.NewSource(seed+int64(w)*7919)))
	}
}

func (wl *Workload) Stop() {
	wl.stop.Store(true)
	wl.wg.Wait()
}

func (wl *Workload) noteErr(err error) {
	wl.Errs.Add(1)
	wl.LastErr.Store(err.Error())
}

func (wl *Workload) writer(w int, rng *rand.Rand) {
	defer wl.wg.Done()
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			conn.Close(context.Background())
		}
	}()
	for !wl.stop.Load() {
		if conn == nil || conn.IsClosed() {
			var c *pgx.Conn
			var err error
			if wl.Dial != nil {
				c, err = wl.Dial()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				c, err = pgx.Connect(ctx, wl.URL)
				cancel()
			}
			if err != nil {
				wl.noteErr(err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			conn = c
		}
		wl.L.mu.Lock()
		seq := wl.L.Next[w]
		wl.L.mu.Unlock()
		a := rng.Intn(nAccts)
		b := (a + 1 + rng.Intn(nAccts-1)) % nAccts
		leg := Leg{A: a, B: b, Amt: int64(1 + rng.Intn(50))}
		rolled := rng.Intn(8) == 0
		out, err := wl.attempt(conn, w, seq, leg, rolled)
		wl.L.mu.Lock()
		switch out {
		case outAcked:
			wl.L.Acked[[2]int{w, seq}] = leg
			wl.L.Next[w] = seq + 1
		case outRolled:
			wl.L.Rolled[[2]int{w, seq}] = true
			wl.L.Next[w] = seq + 1
		case outUnknown:
			wl.L.Maybe[[2]int{w, seq}] = leg
			wl.L.Next[w] = seq + 1
		case outRetry, outFailed:
			// same sequence again: nothing of it can be in the database
		}
		wl.L.mu.Unlock()
		if err != nil {
			wl.noteErr(err)
		}
		switch out {
		case outUnknown, outFailed:
			if conn != nil {
				conn.Close(context.Background())
				conn = nil
			}
			time.Sleep(50 * time.Millisecond)
		case outRetry:
			time.Sleep(time.Duration(1+rng.Intn(5)) * time.Millisecond)
		}
	}
}

func (wl *Workload) attempt(conn *pgx.Conn, w, seq int, leg Leg, rolled bool) (outcome, error) {
	to := wl.Timeout
	if to == 0 {
		to = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return outFailed, err
	}
	fail := func(err error) (outcome, error) {
		rb, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		tx.Rollback(rb)
		rcancel()
		switch pgCode(err) {
		case "40001", "40P01", "25006", "55000", "57014":
			return outRetry, err
		}
		if pgCode(err) != "" && conn.IsClosed() == false {
			return outRetry, err // the server answered with an error: the transaction did not commit
		}
		return outFailed, err
	}
	note := fmt.Sprintf("w%d-s%d", w, seq)
	for _, u := range []struct {
		id    int
		delta int64
	}{{leg.A, -leg.Amt}, {leg.B, leg.Amt}} {
		ct, err := tx.Exec(ctx, "UPDATE acct SET bal = bal + $1, ver = ver + 1, note = $2 WHERE id = $3", u.delta, note, u.id)
		if err != nil {
			return fail(err)
		}
		if ct.RowsAffected() != 1 {
			wl.L.mu.Lock()
			wl.L.Fails = append(wl.L.Fails, fmt.Sprintf("w%d seq %d: UPDATE of account %d touched %d rows", w, seq, u.id, ct.RowsAffected()))
			wl.L.mu.Unlock()
			return fail(fmt.Errorf("update touched %d rows", ct.RowsAffected()))
		}
	}
	kind0, kind1 := "debit", "credit"
	if rolled {
		kind0, kind1 = "rolled", "rolled"
	}
	if _, err := tx.Exec(ctx, "INSERT INTO ev (id, w, seq, k, kind, acct, amt) VALUES ($1,$2,$3,0,$4,$5,$6), ($7,$2,$3,1,$8,$9,$6)",
		evID(w, seq, 0), w, seq, kind0, leg.A, leg.Amt, evID(w, seq, 1), kind1, leg.B); err != nil {
		return fail(err)
	}
	if rolled {
		rb, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		if err := tx.Rollback(rb); err != nil {
			return outFailed, err
		}
		return outRolled, nil
	}
	if err := tx.Commit(ctx); err != nil {
		switch pgCode(err) {
		case "40001", "40P01", "25006", "55000", "23505":
			return outRetry, err
		}
		if pgCode(err) != "" {
			return outRetry, err
		}
		return outUnknown, err
	}
	return outAcked, nil
}

type evRow struct {
	ID, Acct, Amt int64
	W, Seq, K     int
	Kind          string
}

// BankOracle checks the database behind url against the ledger. It returns
// the violations (empty: correct) and a one-line summary.
func BankOracle(url string, l *Ledger) ([]string, string) {
	var bad []string
	add := func(f string, a ...any) {
		if len(bad) < 40 {
			bad = append(bad, fmt.Sprintf(f, a...))
		}
	}
	c, err := connect(url)
	if err != nil {
		return []string{"oracle: " + err.Error()}, ""
	}
	defer c.Close(context.Background())
	ctx := context.Background()
	rows, err := c.Query(ctx, "SELECT id, w, seq, k, kind, acct, amt FROM ev")
	if err != nil {
		return []string{"oracle: " + err.Error()}, ""
	}
	groups := map[[2]int][]evRow{}
	n := 0
	for rows.Next() {
		var r evRow
		if err := rows.Scan(&r.ID, &r.W, &r.Seq, &r.K, &r.Kind, &r.Acct, &r.Amt); err != nil {
			add("scan ev: %v", err)
			break
		}
		if r.ID != evID(r.W, r.Seq, r.K) {
			add("ev id %d does not match (w=%d seq=%d k=%d)", r.ID, r.W, r.Seq, r.K)
		}
		groups[[2]int{r.W, r.Seq}] = append(groups[[2]int{r.W, r.Seq}], r)
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		add("read ev: %v", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.Fails {
		add("writer: %s", f)
	}
	bal := make([]int64, nAccts)
	ver := make([]int64, nAccts)
	for i := range bal {
		bal[i] = initBal
	}
	committed := 0
	for key, g := range groups {
		_, acked := l.Acked[key]
		leg, maybe := l.Maybe[key]
		if l.Rolled[key] {
			add("rolled-back transaction (w=%d seq=%d) has %d ev rows", key[0], key[1], len(g))
			continue
		}
		if !acked && !maybe {
			add("ev rows of (w=%d seq=%d) that no transaction of the client committed or left unknown (phantom)", key[0], key[1])
			continue
		}
		if acked {
			leg = l.Acked[key]
		}
		if len(g) != 2 {
			add("partial transaction (w=%d seq=%d): %d of 2 ev rows (acked=%v)", key[0], key[1], len(g), acked)
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].K < g[j].K })
		if g[0].K != 0 || g[1].K != 1 || g[0].Kind != "debit" || g[1].Kind != "credit" ||
			int(g[0].Acct) != leg.A || int(g[1].Acct) != leg.B || g[0].Amt != leg.Amt || g[1].Amt != leg.Amt {
			add("ev rows of (w=%d seq=%d) differ from what was sent: %+v want %+v", key[0], key[1], g, leg)
			continue
		}
		committed++
		bal[leg.A] -= leg.Amt
		bal[leg.B] += leg.Amt
		ver[leg.A]++
		ver[leg.B]++
	}
	lost := 0
	for key := range l.Acked {
		if _, ok := groups[key]; !ok {
			lost++
			if lost <= 10 {
				add("LOST acknowledged transaction (w=%d seq=%d)", key[0], key[1])
			}
		}
	}
	if lost > 10 {
		add("... %d acknowledged transactions lost in all", lost)
	}
	arows, err := c.Query(ctx, "SELECT id, bal, ver FROM acct ORDER BY id")
	if err != nil {
		add("read acct: %v", err)
		return bad, ""
	}
	var total int64
	seen := 0
	for arows.Next() {
		var id, b, v int64
		if err := arows.Scan(&id, &b, &v); err != nil {
			add("scan acct: %v", err)
			break
		}
		seen++
		total += b
		if id < 0 || id >= nAccts {
			add("unknown account %d", id)
			continue
		}
		if b != bal[id] || v != ver[id] {
			add("account %d: bal=%d ver=%d, the committed ev rows imply bal=%d ver=%d", id, b, v, bal[id], ver[id])
		}
	}
	arows.Close()
	if seen != nAccts {
		add("%d accounts, want %d", seen, nAccts)
	}
	if total != nAccts*initBal {
		add("total balance %d, want %d", total, nAccts*initBal)
	}
	return bad, fmt.Sprintf("ev rows=%d, committed tx=%d (acked %d, unknown %d of which present %d, rolled back %d), total balance=%d",
		n, committed, len(l.Acked), len(l.Maybe), committed-len(l.Acked)+lost, len(l.Rolled), total)
}

// ColumnarVsCore compares an aggregate the columnar view answers
// (autocommit) with the same aggregate read through the row path (inside a
// transaction the view is not used).
func ColumnarVsCore(url string) (string, error) {
	c, err := connect(url)
	if err != nil {
		return "", err
	}
	defer c.Close(context.Background())
	ctx := context.Background()
	const q = "SELECT count(*), sum(bal), sum(ver), min(bal), max(bal) FROM acct"
	var a, b [5]int64
	if err := c.QueryRow(ctx, q).Scan(&a[0], &a[1], &a[2], &a[3], &a[4]); err != nil {
		return "", err
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, q).Scan(&b[0], &b[1], &b[2], &b[3], &b[4]); err != nil {
		return "", err
	}
	if a != b {
		return "", fmt.Errorf("aggregate from the view %v differs from the row path %v", a, b)
	}
	return fmt.Sprintf("acct aggregate view==rows %v", a), nil
}

func joinBad(bad []string) string {
	s := strings.Join(bad, "; ")
	if len(s) > 700 {
		s = s[:700] + fmt.Sprintf("... (%d violations in all)", len(bad))
	}
	return s
}

// LeaderDial returns a dial function that tries every running node with a
// short timeout and keeps the first connection that takes writes.
func LeaderDial(nodes []*Node) func() (*pgx.Conn, error) {
	var rot atomic.Int64
	return func() (*pgx.Conn, error) {
		start := int(rot.Add(1))
		for i := range nodes {
			n := nodes[(start+i)%len(nodes)]
			if !n.Running() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
			c, err := pgx.Connect(ctx, n.URL())
			if err != nil {
				cancel()
				continue
			}
			var ro string
			err = c.QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro)
			cancel()
			if err == nil && ro == "off" {
				return c, nil
			}
			c.Close(context.Background())
		}
		return nil, errors.New("no node that takes writes is reachable")
	}
}
