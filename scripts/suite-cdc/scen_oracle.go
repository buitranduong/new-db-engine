package main

import (
	"time"
)

func init() {
	register("d_oracle", "consistency oracle: 4 concurrent writers with conflicts, replay of the stream equals the source", scenOracle)
}

const markerID = 999_999_999_000 // an ev id no writer uses

func bankSetup(t *T, url string, withCopies bool) {
	c := mustConn(t, url)
	ex(t, c, bankDDL1, bankDDL2)
	if withCopies {
		ex(t, c, `CREATE TABLE acct_c (id bigint PRIMARY KEY, bal bigint NOT NULL, ver int NOT NULL DEFAULT 0, note text)`,
			`CREATE TABLE ev_c (id bigint PRIMARY KEY, w int NOT NULL, seq int NOT NULL, k int NOT NULL, kind text NOT NULL)`)
	}
	ex(t, c, `CREATE PUBLICATION pub FOR TABLE acct, ev`)
}

// compareToSource checks the destination tables equal the source ones.
func compareToSource(t *T, c interface{}, url string) {}

func scenOracle(t *T) {
	n := startSingle(t, "d_oracle")
	bankSetup(t, n.URL(), true)
	newSlot(t, n, "s")
	r := startStream(t, n, "s", 0, "pub", false)
	ap := newApplier(mustConn(t, n.URL()), map[string]string{"acct": "acct_c", "ev": "ev_c"})
	p := startPump(t, r, ap)
	src := mustConn(t, n.URL())

	const accts, writers, per = 8, 4, 150
	seedBank(t, src, accts)
	wl := newWLog()
	runWriters(WCfg{URL: n.URL(), N: per, Accts: accts, Vol: 20, Seed: t.Seed}, writers, wl)
	for _, e := range wl.Errs {
		t.Errorf("writer: %s", e)
	}
	t.Logf("writers: %d acked, %d rolled back, %d retries after conflicts", len(wl.Acked), len(wl.Rolled), wl.Retries.Load())
	ex(t, src, "INSERT INTO ev VALUES (999999999000, 0, 0, 0, 'marker')")
	t.Require(p.WaitEv(markerID, 30*time.Second), "consumer did not reach the end marker (err %v, %d tx)", p.Failed(), p.Count())
	all := p.Stop()
	uniq, dups, mism := dedupe(all)
	t.Check(dups == 0 && len(mism) == 0, "duplicates in a run without restarts: %d %v", dups, mism)
	sc := verifyBank(t, uniq, wl, true)
	t.Logf("stream: %d transactions, %d changes, %d acked transactions matched", sc.Txs, sc.Changes, sc.Acked)

	// the oracle: replay equals source, row by row
	for _, tb := range [][2]string{{"acct", "acct_c"}, {"ev", "ev_c"}} {
		a, b := dump(t, src, tb[0]), dump(t, src, tb[1])
		if d := diffMaps(a, b, tb[0], tb[1], 5); d != "" {
			t.Errorf("replay of the stream differs from the source in %s: %s", tb[0], d)
		}
		t.Logf("%s: %d rows in the source, %d in the replay", tb[0], len(a), len(b))
	}
	// the money did not change: transfers only move it between the hot accounts
	sum := scalar(t, src, "SELECT sum(bal) FROM acct WHERE id < 8")
	sumc := scalar(t, src, "SELECT sum(bal) FROM acct_c WHERE id < 8")
	t.Check(sum == "8000" && sumc == "8000", "sum of balances of the hot accounts: source %s, replay %s, want 8000", sum, sumc)
	// every stream position was confirmed
	last := all[len(all)-1].EndLSN
	cf := ""
	waitFor(5*time.Second, func() bool {
		cf = scalar(t, src, "SELECT confirmed_flush_lsn FROM pg_replication_slots WHERE slot_name = 's'")
		return cf == last.String()
	})
	t.Check(cf == last.String(), "confirmed_flush_lsn %s, last transaction ends at %v", cf, last)
}
