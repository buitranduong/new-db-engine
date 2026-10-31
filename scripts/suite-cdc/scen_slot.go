package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func init() {
	register("e_slot", "slot lifecycle: consumer restart, staskd restart and kill -9, drop, two slots, unacked retention", scenSlot)
	register("e_slot_lost", "slot that falls behind -slot-max-lag-mb is invalidated cleanly", scenSlotLost)
}

// reconnector returns a Pump reconnect function for a single node.
func reconnector(t *T, n *Node, slot string, deadline time.Duration) func(pglogrepl.LSN) (*Repl, error) {
	return func(start pglogrepl.LSN) (*Repl, error) {
		end := time.Now().Add(deadline)
		var last error
		for time.Now().Before(end) {
			ctx, cancel := ctxTimeout(5 * time.Second)
			r, err := DialRepl(ctx, n.ReplURL())
			cancel()
			if err == nil {
				if err = r.Start(bg(), slot, start, "pub", false, 2); err == nil {
					return r, nil
				}
				r.Close()
			}
			last = err
			time.Sleep(300 * time.Millisecond)
		}
		return nil, last
	}
}

// startStreamRetry starts a stream, retrying while the slot is still held by
// a connection that just died (PostgreSQL answers 55006 until the walsender
// notices; clients such as Debezium retry).
func startStreamRetry(t *T, n *Node, slot string, start pglogrepl.LSN, pubs string) *Repl {
	t0 := time.Now()
	for {
		r := mustRepl(t, n)
		err := r.Start(bg(), slot, start, pubs, false, 2)
		if err == nil {
			if d := time.Since(t0); d > 200*time.Millisecond {
				t.Logf("slot %s was released %v after the previous consumer died", slot, d.Truncate(time.Millisecond))
			}
			return r
		}
		r.Close()
		if pgCode(err) != "55006" || time.Since(t0) > 15*time.Second {
			t.Fatalf("START_REPLICATION %s: %v", slot, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func scenSlot(t *T) {
	n := startSingle(t, "e_slot")
	bankSetup(t, n.URL(), true)
	src := mustConn(t, n.URL())
	newSlot(t, n, "s1")
	ex(t, src, `CREATE TABLE applied_lsn (id int PRIMARY KEY, lsn bigint NOT NULL, endl bigint NOT NULL)`, `INSERT INTO applied_lsn VALUES (1, 0, 0)`)
	apc := mustConn(t, n.URL())
	ap := newApplier(apc, map[string]string{"acct": "acct_c", "ev": "ev_c"})
	ap.Connect = func() (*pgx.Conn, error) { return pgxConnect(bg(), n.URL()) }
	const accts = 8
	seedBank(t, src, accts)
	wl := newWLog()
	stop := make(chan struct{})
	wdone := make(chan struct{})
	go func() {
		defer close(wdone)
		runWriters(WCfg{URL: n.URL(), N: 220, Accts: accts, Vol: 20, Seed: t.Seed, Cluster: true, Stop: stop, Think: 3 * time.Millisecond}, 3, wl)
	}()

	// ---- phase 1: the consumer crashes (connection dropped, no goodbye), restarts from the slot's position
	r1 := startStream(t, n, "s1", 0, "pub", false)
	p1 := startPump(t, r1, ap)
	t.Require(waitFor(30*time.Second, func() bool { return p1.Count() >= 60 }), "consumer 1 got %d transactions (err %v)", p1.Count(), p1.Failed())
	rx1 := p1.Stop()
	acked1 := p1.Acked
	r1.Close()
	cf := ""
	waitFor(2*time.Second, func() bool { return false })
	cf = scalar(t, src, "SELECT confirmed_flush_lsn FROM pg_replication_slots WHERE slot_name = 's1'")
	t.Logf("phase 1: consumer 1 received and applied %d transactions, last acknowledged %v, server confirmed_flush_lsn %s", len(rx1), acked1, cf)
	cfl, _ := pglogrepl.ParseLSN(cf)
	t.Check(cfl <= acked1, "server confirmed %s, ahead of what the consumer acknowledged (%v)", cf, acked1)
	// restart with start 0: the server resumes after the confirmed position
	r2 := startStreamRetry(t, n, "s1", 0, "pub")
	p2 := startPump(t, r2, ap)
	t.Require(waitFor(30*time.Second, func() bool { return p2.Count() >= 40 }), "consumer 2 got %d transactions (err %v)", p2.Count(), p2.Failed())
	rx2 := p2.Stop()
	t.Check(rx2[0].EndLSN > cfl, "consumer 2 was sent a transaction ending at %v, not above the confirmed position %v", rx2[0].EndLSN, cfl)
	t.Logf("phase 1: consumer 2 resumed with %d transactions, the first ends at %v (confirmed was %v)", len(rx2), rx2[0].EndLSN, cfl)
	acked2 := p2.Acked
	r2.Close()
	// restart with an explicit start position taken from the consumer's own store (the applier): exactly once
	r3 := startStreamRetry(t, n, "s1", ap.LastEnd, "pub")
	p3 := startPump(t, r3, ap)
	t.Require(waitFor(30*time.Second, func() bool { return p3.Count() >= 20 }), "consumer 3 got %d transactions (err %v)", p3.Count(), p3.Failed())
	t.Check(p3.All[0].EndLSN > acked2, "consumer 3 started with a transaction ending at %v, not above its start %v", p3.All[0].EndLSN, acked2)
	// ---- phase 2: staskd restarts gracefully while writers and consumer run; the consumer reconnects by itself
	p3.Reconnect = reconnector(t, n, "s1", 60*time.Second)
	n.Term()
	t.Require(n.Up() == nil, "restart after SIGTERM")
	t.Require(waitFor(20*time.Second, func() bool { return slotCount(t, n, "s1") == "1" }), "slot s1 not there after restart")
	before := p3.Count()
	t.Require(waitFor(60*time.Second, func() bool { return p3.Count() > before+10 && p3.Reconnects > 0 }), "after SIGTERM restart: consumer got %d more transactions, %d reconnects, err %v", p3.Count()-before, p3.Reconnects, p3.Failed())
	// ---- phase 3: kill -9
	n.Kill9()
	t.Require(n.Up() == nil, "restart after kill -9")
	before = p3.Count()
	rc := p3.Reconnects
	t.Require(waitFor(60*time.Second, func() bool { return p3.Count() > before+5 && p3.Reconnects > rc }), "after kill -9: consumer got %d more transactions, %d reconnects, err %v", p3.Count()-before, p3.Reconnects-rc, p3.Failed())
	<-wdone
	for _, e := range wl.Errs {
		t.Errorf("writer: %s", e)
	}
	src = mustConn(t, n.URL())
	ex(t, src, "INSERT INTO ev VALUES (999999999000, 0, 0, 0, 'marker')")
	t.Require(p3.WaitEv(markerID, 60*time.Second), "consumer did not reach the marker: %v", p3.Failed())
	rx3 := p3.Stop()
	var all []*TxEvt
	all = append(all, rx1...)
	all = append(all, rx2...)
	all = append(all, rx3...)
	uniq, dups, mism := dedupe(all)
	t.Check(len(mism) == 0, "redelivered transactions differ: %v", mism)
	t.Logf("all phases: %d deliveries, %d distinct transactions, %d redelivered (at-least-once); writers: %d acked %d unknown %d rolled back", len(all), len(uniq), dups, len(wl.Acked), len(wl.Unknown), len(wl.Rolled))
	verifyBank(t, uniq, wl, true)
	for _, tb := range [][2]string{{"acct", "acct_c"}, {"ev", "ev_c"}} {
		if d := diffMaps(dump(t, src, tb[0]), dump(t, src, tb[1]), tb[0], tb[1], 5); d != "" {
			t.Errorf("replay differs from the source in %s after the restarts: %s", tb[0], d)
		}
	}
	r3.Close()

	// ---- phase 4: slot management
	rm := mustRepl(t, n)
	rs := startStreamRetry(t, n, "s1", 0, "pub")
	// a second consumer on a busy slot is refused
	rb := mustRepl(t, n)
	err := rb.Start(bg(), "s1", 0, "pub", false, 2)
	t.Check(pgCode(err) == "55006", "second consumer of an active slot: %v (want 55006)", err)
	rb.Drain(bg())
	// dropping an active slot is refused
	err = pglogrepl.DropReplicationSlot(bg(), rm.Conn(), "s1", pglogrepl.DropReplicationSlotOptions{})
	t.Check(pgCode(err) == "55006", "DROP of an active slot: %v (want 55006)", err)
	rs.Stop(bg())
	rs.Close()
	t.Check(pglogrepl.DropReplicationSlot(bg(), rm.Conn(), "s1", pglogrepl.DropReplicationSlotOptions{Wait: true}) == nil, "DROP of an idle slot failed")
	t.Check(scalar(t, src, "SELECT count(*) FROM pg_replication_slots") == "0", "slot still listed after DROP")
	err = pglogrepl.DropReplicationSlot(bg(), rm.Conn(), "s1", pglogrepl.DropReplicationSlotOptions{})
	t.Check(pgCode(err) == "42704", "DROP of a missing slot: %v (want 42704)", err)
	rd := mustRepl(t, n)
	err = rd.Start(bg(), "s1", 0, "pub", false, 2)
	t.Check(pgCode(err) == "42704", "START on a dropped slot: %v (want 42704)", err)
	rd.Drain(bg())
	// create again: starts from now, old data is not replayed
	ex(t, src, "INSERT INTO ev VALUES (999999999001, 0, 0, 0, 'before-recreate')")
	newSlot(t, n, "s1")
	_, err = rm.CreateSlot(bg(), "s1", false)
	t.Check(pgCode(err) == "42710", "CREATE of an existing slot: %v (want 42710)", err)
	ex(t, src, "INSERT INTO ev VALUES (999999999002, 0, 0, 0, 'after-recreate')")
	rn := startStream(t, n, "s1", 0, "pub", false)
	tx, err := rn.Next(bg(), 10*time.Second)
	t.Require(err == nil, "recreated slot: %v", err)
	t.Check(len(tx.Chgs) == 1 && string(tx.Chgs[0].New["kind"].Data) == "after-recreate", "recreated slot delivered %v first, want only the change after its creation", txStrings([]*TxEvt{tx}))
	rn.Stop(bg())
	rn.Close()

	// two consumers on two slots see the same stream; the one that never acknowledges holds the log (lag grows), the other does not
	newSlot(t, n, "s_fast")
	newSlot(t, n, "s_idle")
	lagOf := func(slot string) int64 {
		var v int64
		fmt.Sscan(scalar(t, src, "SELECT lag_bytes FROM pg_replication_slots WHERE slot_name = $1", slot), &v)
		return v
	}
	l0 := lagOf("s_idle")
	rf := startStream(t, n, "s_fast", 0, "pub", false)
	pf := startPump(t, rf, nil)
	for i := 0; i < 200; i++ {
		ex(t, src, fmt.Sprintf("INSERT INTO ev VALUES (%d, 0, 0, 0, 'retention')", 999999990000+i))
	}
	t.Require(waitFor(20*time.Second, func() bool { return pf.Count() >= 200 }), "fast consumer got %d of 200", pf.Count())
	l1 := lagOf("s_idle")
	t.Check(l1 > l0, "lag_bytes of the unacknowledged slot did not grow: %d then %d", l0, l1)
	waitFor(5*time.Second, func() bool { return lagOf("s_fast") <= 1000 })
	t.Logf("retention: unacknowledged slot holds %d bytes of log after 200 transactions (was %d); acknowledging slot holds %d", l1, l0, lagOf("s_fast"))
	t.Check(lagOf("s_fast") < l1, "acknowledging slot holds as much as the idle one: %d vs %d", lagOf("s_fast"), l1)
	ri := startStream(t, n, "s_idle", 0, "pub", false)
	pi := startPump(t, ri, nil)
	t.Require(waitFor(20*time.Second, func() bool { return pi.Count() >= 200 }), "idle consumer attached late got %d of 200", pi.Count())
	t.Check(waitFor(5*time.Second, func() bool { return lagOf("s_idle") <= 1000 }), "after acknowledging, the slot still holds %d bytes", lagOf("s_idle"))
	a, b := pf.Stop(), pi.Stop()
	same := len(a) >= 200 && len(b) >= 200
	for i := 0; same && i < 200; i++ {
		same = a[len(a)-200+i].CommitLSN == b[len(b)-200+i].CommitLSN
	}
	t.Check(same, "the two slots did not deliver the same transactions")
	rf.Close()
	ri.Close()

	// a TEMPORARY slot goes away with its connection
	rt := mustRepl(t, n)
	_, err = rt.CreateSlot(bg(), "s_temp", true)
	t.Require(err == nil, "temporary slot: %v", err)
	t.Check(scalar(t, src, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 's_temp'") == "1", "temporary slot not listed")
	rt.Close()
	t.Check(waitFor(5*time.Second, func() bool {
		return scalar(t, src, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 's_temp'") == "0"
	}), "temporary slot survived its connection")
	// the HTTP view of the same slots
	code, body, herr := httpDo("GET", "http://"+n.HTTP+"/v1/cdc", nil)
	t.Check(herr == nil && code == 200 && len(body) > 10, "GET /v1/cdc: %d %v %s", code, herr, body)
}

func scenSlotLost(t *T) {
	n := startSingle(t, "e_slot_lost", "-segment-mb", "1", "-slot-max-lag-mb", "1", "-gc-interval", "1s")
	c := mustConn(t, n.URL())
	ex(t, c, `CREATE TABLE big (id bigint PRIMARY KEY, v text)`, `CREATE PUBLICATION pub FOR TABLE big`)
	newSlot(t, n, "s_lost")
	newSlot(t, n, "s_ok")
	rok := startStream(t, n, "s_ok", 0, "pub", false)
	pok := startPump(t, rok, nil)
	for i := 0; i < 40; i++ {
		ex(t, c, fmt.Sprintf(`INSERT INTO big SELECT g + %d, repeat('x', 1000) FROM generate_series(1, 400) g`, i*400))
	}
	t.Logf("wrote ~16 MB with a 1 MiB limit")
	ok := waitFor(30*time.Second, func() bool {
		return scalar(t, c, "SELECT coalesce(wal_status, 'NULL') FROM pg_replication_slots WHERE slot_name = 's_lost'") == "lost"
	})
	t.Check(ok, "the idle slot was not invalidated (wal_status %s)", scalar(t, c, "SELECT wal_status FROM pg_replication_slots WHERE slot_name = 's_lost'"))
	r := mustRepl(t, n)
	err := r.Start(bg(), "s_lost", 0, "pub", false, 2)
	t.Check(ok && pgCode(err) == "55000", "START on an invalidated slot: %v (want 55000)", err)
	r.Drain(bg())
	t.Check(waitFor(20*time.Second, func() bool { return pok.Count() >= 40 }), "the healthy slot got %d transactions, err %v", pok.Count(), pok.Failed())
	pok.Stop()
	_, err = r.CreateSlot(bg(), "s_lost", false)
	if err != nil {
		t.Check(pgCode(err) == "42710", "re-create over a lost slot: %v", err)
	}
	t.Check(pglogrepl.DropReplicationSlot(bg(), r.Conn(), "s_lost", pglogrepl.DropReplicationSlotOptions{Wait: true}) == nil, "DROP of the invalidated slot failed")
}

// slotCount counts the slots of that name over a fresh connection (the node may have just restarted).
func slotCount(t *T, n *Node, name string) string {
	ctx, cancel := ctxTimeout(3 * time.Second)
	defer cancel()
	c, err := pgxConnect(ctx, n.URL())
	if err != nil {
		return "unreachable"
	}
	defer c.Close(bg())
	var k string
	if err := c.QueryRow(ctx, "SELECT count(*)::text FROM pg_replication_slots WHERE slot_name = $1", name).Scan(&k); err != nil {
		return "error"
	}
	return k
}
