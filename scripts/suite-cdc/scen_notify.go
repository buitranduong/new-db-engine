package main

import (
	"fmt"
	"strings"
	"time"
)

func init() {
	register("f_notify", "LISTEN/NOTIFY: payload, channels, delivery at commit only, rollback, savepoints, many listeners", scenNotify)
}

func scenNotify(t *T) {
	n := startSingle(t, "f_notify")
	snd := mustConn(t, n.URL())
	l1 := startListener(t, n.URL(), "chan_a")
	defer l1.Stop()
	count := func(l *Listener) int { return len(l.Snapshot()) }
	settle := func() { time.Sleep(400 * time.Millisecond) }

	// 1. autocommit NOTIFY and pg_notify, payload kept exactly
	ex(t, snd, `NOTIFY chan_a, 'hello'`)
	if _, err := snd.Exec(bg(), "SELECT pg_notify('chan_a', $1)", "p\"q'r\né中"); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}
	ex(t, snd, `NOTIFY chan_a`)
	waitFor(5*time.Second, func() bool { return count(l1) >= 3 })
	g := l1.Snapshot()
	t.Require(len(g) == 3, "got %d notifications, want 3: %v", len(g), g)
	t.Check(g[0] == (Notif{Channel: "chan_a", Payload: "hello"}) || g[0].Channel == "chan_a" && g[0].Payload == "hello", "first notification %+v", g[0])
	t.Check(g[1].Payload == "p\"q'r\né中", "payload not preserved: %q", g[1].Payload)
	t.Check(g[2].Payload == "", "NOTIFY without payload: %q", g[2].Payload)

	// 2. only at commit: nothing while the transaction is open
	base := count(l1)
	ex(t, snd, `BEGIN`, `NOTIFY chan_a, 'in-tx'`)
	settle()
	t.Check(count(l1) == base, "notification delivered before COMMIT")
	ex(t, snd, `COMMIT`)
	t.Check(waitFor(5*time.Second, func() bool { return count(l1) == base+1 }), "notification not delivered after COMMIT")

	// 3. rollback drops it; savepoint rollback drops only the rolled back part
	base = count(l1)
	ex(t, snd, `BEGIN`, `NOTIFY chan_a, 'rolled-back'`, `ROLLBACK`)
	ex(t, snd, `BEGIN`, `NOTIFY chan_a, 'kept1'`, `SAVEPOINT s`, `NOTIFY chan_a, 'dropped'`, `ROLLBACK TO s`, `NOTIFY chan_a, 'kept2'`, `COMMIT`)
	t.Check(waitFor(5*time.Second, func() bool { return count(l1) >= base+2 }), "savepoint commit delivered %d, want 2", count(l1)-base)
	settle()
	g = l1.Snapshot()[base:]
	var ps []string
	for _, x := range g {
		ps = append(ps, x.Payload)
	}
	t.Check(strings.Join(ps, ",") == "kept1,kept2", "after rollback and savepoint rollback got %v, want [kept1 kept2]", ps)

	// 4. identical channel+payload within one transaction collapse; different ones keep their order
	base = count(l1)
	ex(t, snd, `BEGIN`, `NOTIFY chan_a, 'x'`, `NOTIFY chan_a, 'y'`, `NOTIFY chan_a, 'x'`, `NOTIFY chan_a, 'y'`, `COMMIT`)
	waitFor(5*time.Second, func() bool { return count(l1) >= base+2 })
	settle()
	ps = nil
	for _, x := range l1.Snapshot()[base:] {
		ps = append(ps, x.Payload)
	}
	t.Check(strings.Join(ps, ",") == "x,y", "duplicates in one transaction: got %v, want [x y]", ps)

	// 5. channels: names fold to lower case unless quoted; other channels are not delivered
	l2 := startListener(t, n.URL(), "other", `"MixedCase"`)
	defer l2.Stop()
	ex(t, snd, `NOTIFY OTHER, 'folded'`, `NOTIFY "MixedCase", 'quoted'`, `NOTIFY mixedcase, 'wrong'`, `NOTIFY chan_a, 'a-only'`)
	waitFor(5*time.Second, func() bool { return count(l2) >= 2 })
	settle()
	var got2 []string
	for _, x := range l2.Snapshot() {
		got2 = append(got2, x.Channel+"="+x.Payload)
	}
	t.Check(strings.Join(got2, ",") == "other=folded,MixedCase=quoted", "listener of other and \"MixedCase\" got %v", got2)

	// 6. payload limit (PostgreSQL: 8000 bytes, error 22023)
	err := exErr(snd, `SELECT pg_notify('chan_a', $1)`, strings.Repeat("z", 8000))
	t.Check(err != nil && sqlState(err) == "22023", "payload of 8000 bytes: %v (want SQLSTATE 22023)", err)
	t.Check(exErr(snd, `SELECT pg_notify('chan_a', $1)`, strings.Repeat("z", 7999)) == nil, "payload of 7999 bytes refused")

	// 7. UNLISTEN, LISTEN inside a transaction, pg_listening_channels
	pa := l2.Pause()
	ex(t, l2.c, `UNLISTEN other`)
	chans := scalar(t, l2.c, `SELECT string_agg(pg_listening_channels, ',') FROM pg_listening_channels()`)
	t.Check(chans == "MixedCase", "pg_listening_channels after UNLISTEN: %q", chans)
	ex(t, l2.c, `BEGIN`, `LISTEN late`)
	pa()
	base = count(l2)
	ex(t, snd, `NOTIFY late, 'before-commit'`)
	settle()
	t.Check(count(l2) == base, "LISTEN inside an open transaction took effect before COMMIT")
	pa = l2.Pause()
	ex(t, l2.c, `COMMIT`)
	pa()
	ex(t, snd, `NOTIFY late, 'after-commit'`)
	t.Check(waitFor(5*time.Second, func() bool { return count(l2) == base+1 }), "LISTEN did not take effect at COMMIT")
	pa = l2.Pause()
	ex(t, l2.c, `UNLISTEN *`)
	pa()
	base = count(l2)
	ex(t, snd, `NOTIFY late, 'x'`, `NOTIFY "MixedCase", 'x'`)
	settle()
	t.Check(count(l2) == base, "UNLISTEN * left a channel listening")

	// 8. a session hears its own notifications (after its commit)
	pa = l1.Pause()
	ex(t, l1.c, `NOTIFY chan_a, 'self'`)
	pa()
	base = count(l1)
	t.Check(waitFor(5*time.Second, func() bool { return count(l1) == base+1 }) || l1.Snapshot()[len(l1.Snapshot())-1].Payload == "self",
		"the notifying session did not hear itself")

	// 9. many listeners, many senders: everyone gets every notification, in commit order
	const nl, ns, per = 30, 3, 20
	var ls []*Listener
	for i := 0; i < nl; i++ {
		ls = append(ls, startListener(t, n.URL(), "fan"))
	}
	defer func() {
		for _, l := range ls {
			l.Stop()
		}
	}()
	done := make(chan struct{}, ns)
	for s := 0; s < ns; s++ {
		c := mustConn(t, n.URL())
		go func() {
			for k := 0; k < per; k++ {
				c.Exec(bg(), "SELECT pg_notify('fan', $1)", fmt.Sprintf("%d:%d", s, k))
			}
			done <- struct{}{}
		}()
	}
	for s := 0; s < ns; s++ {
		<-done
	}
	waitFor(15*time.Second, func() bool {
		for _, l := range ls {
			if count(l) < ns*per {
				return false
			}
		}
		return true
	})
	var ref []string
	for i, l := range ls {
		var ps []string
		for _, x := range l.Snapshot() {
			ps = append(ps, x.Payload)
		}
		if len(ps) != ns*per {
			t.Errorf("listener %d got %d of %d", i, len(ps), ns*per)
			continue
		}
		last := map[string]int{}
		for _, p := range ps {
			var s, k int
			fmt.Sscanf(p, "%d:%d", &s, &k)
			if prev, ok := last[fmt.Sprint(s)]; ok && k != prev+1 {
				t.Errorf("listener %d: sender %d notification %d after %d", i, s, k, prev)
			}
			last[fmt.Sprint(s)] = k
		}
		if i == 0 {
			ref = ps
		} else if strings.Join(ps, " ") != strings.Join(ref, " ") {
			t.Errorf("listener %d saw a different global order than listener 0", i)
		}
	}

	// 10. NOTIFY from a trigger (the usual outbox wake-up)
	ex(t, snd, `CREATE TABLE orders (id bigint PRIMARY KEY, v text)`)
	if err := exErr(snd, `CREATE TRIGGER orders_notify AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION pg_notify('trig', 'x')`); err != nil {
		if err2 := exErr(snd, `CREATE TRIGGER orders_notify AFTER INSERT ON orders FOR EACH ROW BEGIN SELECT pg_notify('trig', NEW.id); END`); err2 != nil {
			t.Logf("trigger forms tried: %v / %v", err, err2)
			t.Gap("NOTIFY from a trigger: CREATE TRIGGER with a pg_notify body is not accepted (%v)", err2)
		}
	}
	lt := startListener(t, n.URL(), "trig")
	defer lt.Stop()
	ex(t, snd, `INSERT INTO orders VALUES (1, 'a')`)
	time.Sleep(500 * time.Millisecond)
	t.Logf("trigger NOTIFY delivered %d notification(s)", count(lt))
}
