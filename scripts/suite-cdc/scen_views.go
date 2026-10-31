package main

import (
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

func init() {
	register("i_views", "CDC with columnar + trigram + history views and long consistent reads (COPY, pg_dump): nothing blocks the stream", scenViews)
}

func scenViews(t *T) {
	n := startSingle(t, "i_views", "-hist-store", "-hist-store-tables", "acct")
	c := mustConn(t, n.URL())
	ex(t, c, `CREATE TABLE acct (id bigint PRIMARY KEY, bal bigint NOT NULL, ver int NOT NULL DEFAULT 0, note text) WITH (columnar = true, trigram = 'note')`, bankDDL2,
		`CREATE TABLE acct_c (id bigint PRIMARY KEY, bal bigint NOT NULL, ver int NOT NULL DEFAULT 0, note text)`,
		`CREATE TABLE ev_c (id bigint PRIMARY KEY, w int NOT NULL, seq int NOT NULL, k int NOT NULL, kind text NOT NULL)`,
		`CREATE PUBLICATION pub FOR TABLE acct, ev`)
	newSlot(t, n, "s")
	r := startStream(t, n, "s", 0, "pub", false)
	ap := newApplier(mustConn(t, n.URL()), map[string]string{"acct": "acct_c", "ev": "ev_c"})
	p := startPump(t, r, ap)
	seedBank(t, c, 8)

	wl := newWLog()
	var active atomic.Bool
	active.Store(true)
	wdone := make(chan struct{})
	go func() {
		defer close(wdone)
		runWriters(WCfg{URL: n.URL(), N: 120, Accts: 8, Vol: 20, Seed: t.Seed, Think: 8 * time.Millisecond}, 4, wl)
		active.Store(false)
	}()

	// pg_dump-style readers: one consistent snapshot, several reads over a while, while the stream runs
	rd := mustConn(t, n.URL())
	windows, stalled := 0, 0
	for active.Load() && windows < 12 {
		before := p.Count()
		ex(t, rd, "BEGIN ISOLATION LEVEL REPEATABLE READ")
		s1 := scalar(t, rd, "SELECT sum(bal)::text FROM acct WHERE id < 8")
		var copied int
		if tag, err := rd.PgConn().CopyTo(bg(), discard{}, "COPY (SELECT * FROM acct) TO STDOUT"); err != nil {
			t.Errorf("COPY TO inside the snapshot: %v", err)
		} else {
			copied = int(tag.RowsAffected())
		}
		time.Sleep(250 * time.Millisecond)
		s2 := scalar(t, rd, "SELECT sum(bal)::text FROM acct WHERE id < 8")
		cnt := scalar(t, rd, "SELECT count(*)::text FROM acct")
		ex(t, rd, "COMMIT")
		if s1 != "8000" || s2 != "8000" {
			t.Errorf("snapshot read: sum of hot balances %s then %s, want 8000 both (transfers keep it constant)", s1, s2)
		}
		if fmt.Sprint(copied) != cnt {
			t.Errorf("snapshot read: COPY returned %d rows, count(*) %s in the same snapshot", copied, cnt)
		}
		if active.Load() && p.Count() == before {
			stalled++
		}
		windows++
	}
	t.Logf("%d consistent read windows, %d without a single transaction delivered while writers were active", windows, stalled)
	t.Check(stalled <= windows/4, "the stream stalled during %d of %d long read windows", stalled, windows)

	// a real pg_dump (postgres:16 image) of the table while the stream is live; recorded either way
	if out, err := pgDump(n); err != nil {
		t.Gap("pg_dump (postgres:16) --data-only -t acct against STASK failed: %s", firstLines(out, 3))
	} else {
		t.Logf("pg_dump --data-only -t acct: ok, %d bytes", len(out))
	}

	<-wdone
	for _, e := range wl.Errs {
		t.Errorf("writer: %s", e)
	}
	ex(t, c, "INSERT INTO ev VALUES (999999999000, 0, 0, 0, 'marker')")
	t.Require(p.WaitEv(markerID, 30*time.Second), "consumer: %v", p.Failed())
	all := p.Stop()
	uniq, dups, _ := dedupe(all)
	t.Check(dups == 0, "%d duplicates", dups)
	verifyBank(t, uniq, wl, true)
	for _, tb := range [][2]string{{"acct", "acct_c"}, {"ev", "ev_c"}} {
		if d := diffMaps(dump(t, c, tb[0]), dump(t, c, tb[1]), tb[0], tb[1], 5); d != "" {
			t.Errorf("replay differs from the source in %s: %s", tb[0], d)
		}
	}
	// the derived views agree with the stream's replay
	agg := func(tb string) string {
		return scalar(t, c, fmt.Sprintf("SELECT count(*)::text || '/' || sum(bal)::text || '/' || sum(ver)::text FROM %s", tb))
	}
	t.Check(agg("acct") == agg("acct_c"), "columnar aggregate %s differs from the replay %s", agg("acct"), agg("acct_c"))
	like := func(tb string) string {
		return scalar(t, c, fmt.Sprintf("SELECT count(*)::text FROM %s WHERE note LIKE '%%w1 s%%'", tb))
	}
	t.Check(like("acct") == like("acct_c"), "trigram LIKE count %s differs from the replay %s", like("acct"), like("acct_c"))
	t.Logf("aggregate %s, LIKE matches %s", agg("acct"), like("acct"))
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

func firstLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, " | ")
}

// pgDump runs pg_dump from the postgres:16 image against the node in a
// container it names and removes itself.
func pgDump(n *Node) (string, error) {
	name := fmt.Sprintf("cdcsuite-pgdump-%d", n.Pid())
	host, port, _ := strings.Cut(n.PG, ":")
	cmd := exec.Command("docker", "run", "--rm", "--name", name, "--network", "host", "-e", "PGSSLMODE=disable", "postgres:16",
		"pg_dump", "-h", host, "-p", port, "-U", "app", "--data-only", "-t", "acct", "stask")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		exec.Command("docker", "rm", "-f", name).Run()
		<-done
		err = fmt.Errorf("timeout")
	}
	return string(out), err
}
