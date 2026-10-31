package main

import (
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
)

func init() {
	register("g_failover", "3-node cluster: kill -9 the leader mid-stream; the consumer follows to the new leader, the oracle still holds", scenFailover)
}

func scenFailover(t *T) {
	c := t.Env.NewCluster("g_failover", 3)
	t.OnCleanup(func() {
		for _, n := range c.Nodes {
			n.Term()
		}
	})
	t.Require(c.Up() == nil, "cluster did not elect a leader")
	leader, err := c.WaitLeader(30 * time.Second)
	t.Require(err == nil, "no leader: %v", err)
	t.Logf("leader is node %d (pid %d)", leader.ID, leader.Pid())
	dsn := c.DSN()
	connect := func() (*pgx.Conn, error) {
		ctx, cancel := ctxTimeout(5 * time.Second)
		defer cancel()
		cn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return nil, err
		}
		if _, err := cn.Exec(bg(), "SET TimeZone TO 'UTC'"); err != nil {
			cn.Close(bg())
			return nil, err
		}
		return cn, nil
	}
	src, err := connect()
	t.Require(err == nil, "connect: %v", err)
	t.OnCleanup(func() { src.Close(bg()) })
	bankSetupOn(t, src)
	ex(t, src, `CREATE TABLE applied_lsn (id int PRIMARY KEY, lsn bigint NOT NULL, endl bigint NOT NULL)`, `INSERT INTO applied_lsn VALUES (1, 0, 0)`)

	// a replication connection to a follower is refused cleanly (slots live on the leader)
	for _, n := range c.Nodes {
		if n == leader {
			continue
		}
		fr, err := DialRepl(bg(), n.ReplURL())
		if err != nil {
			t.Logf("follower %d: replication connect refused: %v", n.ID, err)
			continue
		}
		_, err = fr.CreateSlot(bg(), "on_follower", false)
		t.Check(err != nil, "CREATE_REPLICATION_SLOT on follower %d succeeded", n.ID)
		t.Logf("follower %d: CREATE_REPLICATION_SLOT: %v", n.ID, err)
		fr.Close()
		break
	}

	newSlot(t, leader, "s")
	seedBank(t, src, 8)
	cn, err := connect()
	t.Require(err == nil, "connect: %v", err)
	ap := newApplier(cn, map[string]string{"acct": "acct_c", "ev": "ev_c"})
	ap.Connect = connect
	reconnect := func(start pglogrepl.LSN) (*Repl, error) {
		end := time.Now().Add(90 * time.Second)
		var last error
		for time.Now().Before(end) {
			for _, n := range c.Nodes {
				if n.Pid() == 0 {
					continue
				}
				ctx, cancel := ctxTimeout(4 * time.Second)
				r, err := DialRepl(ctx, n.ReplURL())
				cancel()
				if err != nil {
					last = err
					continue
				}
				if err = r.Start(bg(), "s", start, "pub", false, 2); err == nil {
					return r, nil
				}
				last = fmt.Errorf("node %d: %w", n.ID, err)
				r.Close()
			}
			time.Sleep(300 * time.Millisecond)
		}
		return nil, last
	}
	r, err := reconnect(0)
	t.Require(err == nil, "start streaming: %v", err)
	p := startPump(t, r, ap)
	p.Reconnect = reconnect

	wl := newWLog()
	wdone := make(chan struct{})
	go func() {
		defer close(wdone)
		runWriters(WCfg{URL: dsn, N: 90, Accts: 8, Vol: 20, Seed: t.Seed, Cluster: true, Think: 10 * time.Millisecond}, 3, wl)
	}()
	t.Require(waitFor(60*time.Second, func() bool { return p.Count() >= 40 }), "only %d transactions before the kill (err %v)", p.Count(), p.Failed())
	before := p.Count()
	t.Logf("kill -9 node %d after %d transactions streamed", leader.ID, before)
	leader.Kill9()
	nl, err := c.WaitLeader(60 * time.Second)
	t.Require(err == nil, "no new leader after the kill: %v", err)
	t.Check(nl != leader, "the dead node is still the leader")
	t.Logf("new leader is node %d", nl.ID)
	t.Require(waitFor(90*time.Second, func() bool { return p.Reconnects > 0 && p.Count() > before+10 }),
		"after the failover the consumer got %d more transactions, %d reconnects, err %v", p.Count()-before, p.Reconnects, p.Failed())
	<-wdone
	for _, e := range wl.Errs {
		t.Errorf("writer: %s", e)
	}
	t.Logf("writers: %d acked, %d unknown (connection died at COMMIT), %d rolled back, %d retries", len(wl.Acked), len(wl.Unknown), len(wl.Rolled), wl.Retries.Load())
	chk, err := connect()
	t.Require(err == nil, "connect after failover: %v", err)
	t.OnCleanup(func() { chk.Close(bg()) })
	ex(t, chk, "INSERT INTO ev VALUES (999999999000, 0, 0, 0, 'marker')")
	t.Require(p.WaitEv(markerID, 90*time.Second), "consumer did not reach the marker: %v", p.Failed())
	all := p.Stop()
	uniq, dups, mism := dedupe(all)
	t.Check(len(mism) == 0, "redelivered transactions differ: %v", mism)
	t.Logf("%d deliveries, %d distinct, %d redelivered, %d reconnects", len(all), len(uniq), dups, p.Reconnects)
	verifyBank(t, uniq, wl, true)
	// no phantom: every ev row the stream delivered is in the final table
	ids := dump(t, chk, "ev")
	missing := 0
	for _, tx := range uniq {
		for i := range tx.Chgs {
			ch := &tx.Chgs[i]
			if ch.Rel == "ev" && ch.Op == 'I' {
				if _, ok := ids[string(ch.New["id"].Data)]; !ok {
					missing++
				}
			}
		}
	}
	t.Check(missing == 0, "%d ev rows were streamed that the final table does not have (changes of the old leader the cluster did not keep)", missing)
	for _, tb := range [][2]string{{"acct", "acct_c"}, {"ev", "ev_c"}} {
		if d := diffMaps(dump(t, chk, tb[0]), dump(t, chk, tb[1]), tb[0], tb[1], 5); d != "" {
			t.Errorf("replay differs from the source in %s after the failover: %s", tb[0], d)
		}
	}
	t.Logf("slot on the new leader: %s", scalar(t, chk, "SELECT slot_name || ' ' || coalesce(confirmed_flush_lsn,'') FROM pg_replication_slots"))
	// the old leader comes back as a follower and the cluster is whole again
	t.Require(leader.Start() == nil, "restart of the old leader")
	t.Check(waitFor(60*time.Second, func() bool { return leader.WaitSQL(2*time.Second) == nil && !isLeader(leader) }), "old leader did not rejoin as a follower")
}

// bankSetupOn creates the bank tables, their copies and the publication on an open connection.
func bankSetupOn(t *T, c *pgx.Conn) {
	ex(t, c, bankDDL1, bankDDL2,
		`CREATE TABLE acct_c (id bigint PRIMARY KEY, bal bigint NOT NULL, ver int NOT NULL DEFAULT 0, note text)`,
		`CREATE TABLE ev_c (id bigint PRIMARY KEY, w int NOT NULL, seq int NOT NULL, k int NOT NULL, kind text NOT NULL)`,
		`CREATE PUBLICATION pub FOR TABLE acct, ev`)
}
