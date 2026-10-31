package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func sleepRand(e *Env, lo, hi time.Duration) time.Duration {
	d := lo + time.Duration(e.Rng.Int63n(int64(hi-lo)+1))
	time.Sleep(d)
	return d
}

func bankStart(e *Env, r *Result, t *Topo) *Ledger {
	if !r.Check(t.UpAll() == nil, "start") {
		panic("cannot start the system under test")
	}
	if err := BankSetup(t.DSN()); err != nil {
		r.Check(false, "setup: %v", err)
		panic("setup failed")
	}
	if !t.Cluster {
		if err := exec1(t.DSN(), "ALTER INDEX ev_acct_idx SET (index_storage = 'disk')"); err != nil {
			r.Logf("disk index not set: %v", err)
		}
	}
	return NewLedger()
}

// ---- D1 ----

func drillD1(e *Env, o *Opts, r *Result, mode string) {
	t := newTopo(e, "d1-"+mode, mode)
	l := bankStart(e, r, t)
	dsn := t.DSN()
	var kills []string
	for c := 0; c < o.Cycles; c++ {
		wl := t.Workload(l, o.Writers)
		ackedBefore, _, _ := l.Counts()
		wl.Start(o.Seed + int64(c)*101)
		d := sleepRand(e, 400*time.Millisecond, 6*time.Second)
		ackedMid, _, _ := l.Counts()
		// kill -9 (the whole system: in a cluster every node at once)
		t.KillAll()
		wl.Stop()
		kills = append(kills, d.Round(10*time.Millisecond).String())
		double := c%4 == 3 // every fourth cycle: crash again while the node recovers
		if err := startAll(t); !r.Check(err == nil, "cycle %d: restart: %v", c, err) {
			return
		}
		if double {
			sleepRand(e, 50*time.Millisecond, 800*time.Millisecond)
			t.KillAll()
			if err := startAll(t); !r.Check(err == nil, "cycle %d: restart after a crash during recovery: %v", c, err) {
				return
			}
		}
		bad, sum := BankOracle(dsn, l)
		r.Logf("cycle %d: killed after %v (%d transactions acknowledged in this run); recovered; %s", c, d.Round(time.Millisecond), ackedMid-ackedBefore, sum)
		if !r.Check(len(bad) == 0, "cycle %d: oracle: %s", c, joinBad(bad)) {
			return
		}
	}
	// views and structures after catch-up
	if err := t.Settle(180 * time.Second); !r.Check(err == nil, "settle: %v", err) {
		return
	}
	bad, sum := BankOracle(dsn, l)
	r.Check(len(bad) == 0, "final oracle: %s", joinBad(bad))
	a, m, rb := l.Counts()
	r.OracleOK("%d kill -9 cycles (%s) : every acknowledged transaction present exactly once (%d acked, %d unknown, %d rolled back), no partial transaction, invariants hold; %s", o.Cycles, mode, a, m, rb, sum)
	t.CheckViews(r)
	if msg, err := ColumnarVsCore(dsn); r.Check(err == nil, "columnar vs core: %v", err) {
		r.Logf("%s", msg)
	}
	for _, n := range t.Nodes {
		scrubCheck(r, n)
	}
	r.OracleOK("columnar, trigram, secondary-index and history structures equal the core after catch-up (reconcile -full), scrub clean")
}

func startAll(t *Topo) error {
	if err := t.UpAll(); err != nil {
		return err
	}
	return nil
}

func scrubCheck(r *Result, n *Node) {
	b, code, err := n.Post("/v1/admin/scrub?wait=1&rate_mb=0", 10*time.Minute)
	r.Check(err == nil && code == 200, "node %d scrub: %v %d %s", n.ID, err, code, b)
}

// ---- D2 ----

func catchUp(r *Result, n *Node, lead *Node, d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		ls, err1 := lead.ClusterStatus()
		ns, err2 := n.ClusterStatus()
		if err1 == nil && err2 == nil && fnum(ns, "applied")+2 >= fnum(ls, "commit") {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func drillD2(e *Env, o *Opts, r *Result, mode string) {
	t := newTopo(e, "d2", "cluster")
	l := bankStart(e, r, t)
	dsn := t.DSN()
	wl := t.Workload(l, o.Writers)
	wl.Start(o.Seed)
	cycles := min(o.Cycles, 6)
	var elect []time.Duration
	for c := 0; c < cycles; c++ {
		time.Sleep(2 * time.Second)
		lead, err := WaitLeader(t.Nodes, 30*time.Second)
		if !r.Check(err == nil, "cycle %d: no leader", c) {
			break
		}
		a0, _, _ := l.Counts()
		t0 := time.Now()
		lead.Kill9()
		nl, err := WaitLeader(t.Nodes, 60*time.Second, lead)
		if !r.Check(err == nil, "cycle %d: no new leader after killing node %d", c, lead.ID) {
			break
		}
		el := time.Since(t0)
		elect = append(elect, el)
		time.Sleep(2 * time.Second)
		a1, _, _ := l.Counts()
		r.Check(a1 > a0, "cycle %d: no transaction was acknowledged in the 2 s after the new leader (node %d) appeared", c, nl.ID)
		if !r.Check(lead.Start() == nil, "restart node %d", lead.ID) {
			break
		}
		r.Check(lead.WaitSQL(120*time.Second) == nil, "cycle %d: node %d did not come back", c, lead.ID)
		nl2, _ := WaitLeader(t.Nodes, 30*time.Second)
		r.Check(nl2 != nil && catchUp(r, lead, nl2, 120*time.Second), "cycle %d: node %d did not catch up after rejoining", c, lead.ID)
		// a follower
		var fol *Node
		for _, n := range t.Nodes {
			if n != nl2 && n.Running() && n != lead {
				fol = n
				break
			}
		}
		if fol == nil {
			fol = lead
		}
		fol.Kill9()
		time.Sleep(2 * time.Second)
		r.Check(fol.Start() == nil, "restart node %d", fol.ID)
		nl3, _ := WaitLeader(t.Nodes, 30*time.Second)
		r.Check(nl3 != nil && fol.WaitSQL(120*time.Second) == nil && catchUp(r, fol, nl3, 120*time.Second), "cycle %d: follower %d did not catch up", c, fol.ID)
		r.Logf("cycle %d: killed leader %d (new leader %d after %v), restarted; killed follower %d, restarted; %d acked so far", c, lead.ID, nl.ID, el.Round(10*time.Millisecond), fol.ID, a1)
		if len(r.Fails) > 0 {
			break
		}
	}
	wl.Stop()
	if len(r.Fails) > 0 {
		return
	}
	if err := t.Settle(180 * time.Second); !r.Check(err == nil, "settle: %v", err) {
		return
	}
	bad, sum := BankOracle(dsn, l)
	r.Check(len(bad) == 0, "oracle on the leader: %s", joinBad(bad))
	for _, n := range t.Nodes {
		bad, s := BankOracle(n.URL(), l)
		r.Check(len(bad) == 0, "oracle on node %d: %s", n.ID, joinBad(bad))
		r.Logf("node %d: %s", n.ID, s)
	}
	a, m, rb := l.Counts()
	r.OracleOK("%d leader kills and %d follower kills with rejoin under load: no acknowledged transaction lost (%d acked, %d unknown, %d rolled back), identical and correct on all 3 nodes; %s", cycles, cycles, a, m, rb, sum)
	t.CheckViews(r)
	for _, n := range t.Nodes {
		scrubCheck(r, n)
	}
	r.OracleOK("every node's views equal its core (reconcile -full), scrub clean on all nodes; election times after leader kill: %v", elect)
}

// ---- D6 ----

func drillD6(e *Env, o *Opts, r *Result, mode string) {
	t := newTopo(e, "d6", "cluster")
	l := bankStart(e, r, t)
	if err := exec1(t.DSN(), "CREATE TABLE canary (id bigint PRIMARY KEY, node int NOT NULL, phase text NOT NULL)"); err != nil {
		r.Check(false, "canary table: %v", err)
		return
	}
	dsn := t.DSN()
	wl := t.Workload(l, o.Writers)
	wl.Start(o.Seed)
	time.Sleep(3 * time.Second)
	old, err := WaitLeader(t.Nodes, 30*time.Second)
	if !r.Check(err == nil, "no leader") {
		wl.Stop()
		return
	}
	// sample who claims to take writes, every 50 ms, in parallel
	var t0Freeze time.Time
	var two atomic.Int64
	var twoMu sync.Mutex
	var twoAt []time.Time
	var samples atomic.Int64
	var sw sync.WaitGroup
	stopS := make(chan struct{})
	sw.Add(1)
	go func() {
		defer sw.Done()
		for {
			select {
			case <-stopS:
				return
			case <-time.After(50 * time.Millisecond):
			}
			var cnt atomic.Int32
			var w sync.WaitGroup
			var who sync.Map
			for _, n := range t.Nodes {
				w.Add(1)
				go func(n *Node) {
					defer w.Done()
					if n.Running() && n.IsLeader() {
						cnt.Add(1)
						st, _ := n.ClusterStatus()
						who.Store(n.ID, fmt.Sprintf("node %d: %v", n.ID, st))
					}
				}(n)
			}
			w.Wait()
			samples.Add(1)
			if cnt.Load() > 1 {
				two.Add(1)
				twoMu.Lock()
				twoAt = append(twoAt, time.Now())
				twoMu.Unlock()
				if two.Load() == 1 {
					var parts []string
					who.Range(func(_, v any) bool { parts = append(parts, v.(string)); return true })
					r.Logf("two nodes claimed to take writes %v after the freeze began: %s", time.Since(t0Freeze).Round(10*time.Millisecond), strings.Join(parts, " / "))
				}
			}
		}
	}()
	a0, _, _ := l.Counts()
	old.Pause()
	t0 := time.Now()
	t0Freeze = t0
	nl, err := WaitLeader(t.Nodes, 60*time.Second, old)
	if !r.Check(err == nil, "no new leader within 60 s of freezing node %d", old.ID) {
		old.Resume()
		close(stopS)
		sw.Wait()
		wl.Stop()
		return
	}
	el := time.Since(t0)
	const hold = 10 * time.Second // election timeout is 1 to 2 s with the default ticks
	time.Sleep(hold - time.Since(t0))
	a1, _, _ := l.Counts()
	r.Check(a1 > a0, "no transaction was acknowledged while node %d was frozen (new leader %d)", old.ID, nl.ID)
	r.Logf("node %d frozen for %v (election timeout 1-2 s); node %d became leader after %v; %d transactions acknowledged during the freeze", old.ID, hold, nl.ID, el.Round(10*time.Millisecond), a1-a0)
	old.Resume()
	tResume := time.Now()
	claimUntil := time.Duration(0)
	for time.Since(tResume) < 3*time.Second {
		if old.IsLeader() {
			claimUntil = time.Since(tResume)
		} else if claimUntil > 0 || time.Since(tResume) > 500*time.Millisecond {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Logf("the resumed node %d answered /v1/ready?write=1 with 200 (\"I take writes\") for %v after SIGCONT; the new leader was %d", old.ID, claimUntil.Round(5*time.Millisecond), nl.ID)
	// the resumed old leader gets writes sent straight to it: it must not acknowledge any it cannot keep
	canaryAcked := map[int64]bool{}
	var canaryErr, canaryOK int
	end := time.Now().Add(4 * time.Second)
	for id := int64(1); time.Now().Before(end); id++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := connectCtx(ctx, old.URL())
		if err != nil {
			cancel()
			canaryErr++
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_, err = c.Exec(ctx, "INSERT INTO canary (id, node, phase) VALUES ($1, $2, 'resumed')", id, old.ID)
		c.Close(context.Background())
		cancel()
		if err == nil {
			canaryAcked[id] = true
			canaryOK++
		} else {
			canaryErr++
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.Logf("writes sent straight to the resumed node %d: %d acknowledged, %d refused", old.ID, canaryOK, canaryErr)
	time.Sleep(2 * time.Second)
	close(stopS)
	sw.Wait()
	wl.Stop()
	if err := t.Settle(180 * time.Second); !r.Check(err == nil, "settle: %v", err) {
		return
	}
	leaders := 0
	for _, n := range t.Nodes {
		if n.IsLeader() {
			leaders++
		}
	}
	r.Check(leaders == 1, "%d nodes claim to take writes after the node resumed", leaders)
	twoMu.Lock()
	late := 0
	for _, at := range twoAt {
		if at.Before(tResume) || at.After(tResume.Add(1500*time.Millisecond)) {
			late++
		}
	}
	twoMu.Unlock()
	r.Check(late == 0, "two nodes claimed to take writes at the same time %d times outside the first 1.5 s after SIGCONT (split brain)", late)
	if two.Load() > 0 {
		r.Logf("FINDING F4: %d of %d samples saw two nodes answer ready?write=1 with 200, all within the first moments after SIGCONT: the resumed old leader says it takes writes until it processes a message of the new term; the writes sent to it in that window were refused (%d acknowledged)", two.Load(), samples.Load(), canaryOK)
	}
	bad, sum := BankOracle(dsn, l)
	r.Check(len(bad) == 0, "oracle: %s", joinBad(bad))
	for _, n := range t.Nodes {
		b2, _ := BankOracle(n.URL(), l)
		r.Check(len(b2) == 0, "oracle on node %d: %s", n.ID, joinBad(b2))
		cnt, err := scalarInt(n.URL(), "SELECT count(*) FROM canary")
		r.Check(err == nil && cnt == int64(len(canaryAcked)), "node %d holds %d canary rows (err %v), the client was acknowledged %d", n.ID, cnt, err, len(canaryAcked))
	}
	a, m, rb := l.Counts()
	r.OracleOK("leader frozen %v (> election timeout), new leader after %v, then resumed: no acknowledged write lost (%d bank tx acked, %d unknown; %d writes sent straight to the resumed node were acknowledged, all refused otherwise); the resumed node still answered ready?write=1 with 200 for %v after SIGCONT (%d of %d samples of 50 ms saw two such nodes, finding F4); exactly one writable node afterwards; %s", hold, el.Round(10*time.Millisecond), a, m, canaryOK, claimUntil.Round(5*time.Millisecond), two.Load(), samples.Load(), sum)
	_ = rb
	t.CheckViews(r)
	r.OracleOK("views equal the core on all nodes")
}

func connectCtx(ctx context.Context, url string) (*pgxConn, error) { return pgxConnect(ctx, url) }

var _ = fmt.Sprintf
