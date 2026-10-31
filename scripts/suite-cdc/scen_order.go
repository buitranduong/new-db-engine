package main

import (
	"fmt"
	"strings"
	"time"
)

func init() {
	register("h_order", "the same transactions through pgoutput, the HTTP stream and NOTIFY: same set, same order, same commit position", scenOrder)
}

func scenOrder(t *T) {
	n := startSingle(t, "h_order")
	bankSetup(t, n.URL(), false)
	src := mustConn(t, n.URL())
	newSlot(t, n, "s_pg")
	t.Require(httpCreateSlot(n, "s_json", "wal2json") == nil, "HTTP slot")
	r := startStream(t, n, "s_pg", 0, "pub", false)
	pp := startPump(t, r, nil)
	hs := openHTTPStream(n, "s_json", nil)
	defer hs.Close()
	lst := startListener(t, n.URL(), "ordch")
	defer lst.Stop()
	seedBank(t, src, 8)

	wl := newWLog()
	runWriters(WCfg{URL: n.URL(), N: 60, Accts: 8, Vol: 20, Seed: t.Seed, Notify: "ordch"}, 4, wl)
	for _, e := range wl.Errs {
		t.Errorf("writer: %s", e)
	}
	ex(t, src, "INSERT INTO ev VALUES (999999999000, 0, 0, 0, 'marker')")
	ex(t, src, "SELECT pg_notify('ordch', 'marker')")
	t.Require(pp.WaitEv(markerID, 30*time.Second), "pgoutput consumer: %v", pp.Failed())
	var jtx []*HTx
	t.Require(waitFor(30*time.Second, func() bool {
	drain:
		for {
			select {
			case x, ok := <-hs.Txs:
				if !ok {
					return true
				}
				jtx = append(jtx, x)
			default:
				break drain
			}
		}
		for _, x := range jtx {
			for _, c := range x.Changes {
				if c.Table == "ev" && c.col("kind") == "marker" {
					return true
				}
			}
		}
		return false
	}), "HTTP consumer did not reach the marker (%d tx, err %v)", len(jtx), hs.Err())
	t.Require(waitFor(30*time.Second, func() bool {
		for _, x := range lst.Snapshot() {
			if x.Payload == "marker" {
				return true
			}
		}
		return false
	}), "listener did not get the marker")
	pg := pp.Stop()

	// the sequence of "w:seq" per mechanism, in delivery order
	var seqPG, seqHTTP, seqNotify []string
	for _, tx := range pg {
		if k := firstMarker(tx); k != "" {
			seqPG = append(seqPG, k)
		}
	}
	for _, x := range jtx {
		for _, c := range x.Changes {
			if c.Table == "ev" && c.col("kind") != "marker" {
				seqHTTP = append(seqHTTP, fmt.Sprintf("%s:%s", c.col("w"), c.col("seq")))
				break
			}
		}
	}
	for _, x := range lst.Snapshot() {
		if x.Payload != "marker" {
			seqNotify = append(seqNotify, x.Payload)
		}
	}
	t.Logf("transactions: pgoutput %d, HTTP %d, NOTIFY %d; writers committed %d, rolled back %d", len(seqPG), len(seqHTTP), len(seqNotify), len(wl.Acked), len(wl.Rolled))
	// notifications of rolled back transactions and savepoints never arrive
	for _, p := range seqNotify {
		if strings.Contains(p, ":rb") || strings.Contains(p, ":sp") {
			t.Errorf("notification of a rolled back (sub)transaction delivered: %s", p)
		}
	}
	// every kind of writer transaction writes ev rows, so each committed one has a marker
	t.Check(len(seqPG) == len(wl.Acked), "pgoutput delivered %d marked transactions, %d were committed", len(seqPG), len(wl.Acked))
	cmp := func(name string, a, b []string) {
		if strings.Join(a, " ") != strings.Join(b, " ") {
			i := 0
			for i < len(a) && i < len(b) && a[i] == b[i] {
				i++
			}
			t.Errorf("%s differ at position %d of %d/%d: %v vs %v", name, i, len(a), len(b), a[max(0, i-2):min(len(a), i+3)], b[max(0, i-2):min(len(b), i+3)])
		}
	}
	cmp("pgoutput and HTTP order", seqPG, seqHTTP)
	cmp("pgoutput and NOTIFY order", seqPG, seqNotify)
	// the commit position of each transaction is the same in pgoutput and in the HTTP stream
	bad := 0
	if len(pg) != len(jtx) {
		t.Errorf("pgoutput has %d transactions, HTTP %d", len(pg), len(jtx))
	} else {
		for i := range pg {
			if pg[i].EndLSN.String() != jtx[i].CommitLSN {
				if bad < 3 {
					t.Errorf("transaction %d: pgoutput commit ends at %v, HTTP commit line says %s", i, pg[i].EndLSN, jtx[i].CommitLSN)
				}
				bad++
			}
		}
	}
	// a HTTP ack moves the slot
	if len(jtx) > 0 {
		last := jtx[len(jtx)-1].CommitLSN
		t.Check(httpAck(n, "s_json", last) == nil, "HTTP ack %s", last)
		t.Check(scalar(t, src, "SELECT confirmed_flush_lsn FROM pg_replication_slots WHERE slot_name = 's_json'") == last, "HTTP ack did not move the slot to %s", last)
	}
}

func firstMarker(tx *TxEvt) string {
	for i := range tx.Chgs {
		c := &tx.Chgs[i]
		if c.Rel == "ev" && c.Op == 'I' && string(c.New["kind"].Data) != "marker" {
			return fmt.Sprintf("%s:%s", c.New["w"].Data, c.New["seq"].Data)
		}
	}
	return ""
}
