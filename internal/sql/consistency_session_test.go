package sql

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"stask/internal/planner"
)

// D26: the default consistency is "session": a session always finds the
// rows it has just written through the trigram view.
func TestDefaultSessionConsistency(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	w := NewSession(e)
	w.Views = p
	run(t, w, `CREATE TABLE sim (id BIGINT PRIMARY KEY, num TEXT) WITH (trigram = 'num')`)
	if got := w.vars[SearchConsistencyVar]; got != "" && got != "session" {
		t.Fatalf("default consistency %q", got)
	}
	if r := run(t, w, `SHOW stask.consistency`); r.Rows[0][0].String() != "session" {
		t.Fatalf("SHOW stask.consistency = %v", r.Rows[0][0])
	}
	// every write is visible to the next search of the same session
	for i := 0; i < 300; i++ {
		num := fmt.Sprintf("0912%06d", i)
		run(t, w, fmt.Sprintf(`INSERT INTO sim VALUES (%d, '%s')`, i, num))
		r := run(t, w, fmt.Sprintf(`SELECT id FROM sim WHERE num LIKE '%%%s'`, num[4:]))
		if len(r.Rows) != 1 {
			t.Fatalf("iteration %d: own write not found (%d rows) plan=%s", i, len(r.Rows), lastPlan(w))
		}
		if i == 0 && !strings.Contains(lastPlan(w), "engine=substr") {
			t.Fatalf("view not used: %s", lastPlan(w))
		}
	}
	// with the view held back the session waits for it (up to 500 ms) rather than answering without its own row
	release := p.Sub.Hold()
	run(t, w, `INSERT INTO sim VALUES (1000, '0999000000')`)
	go func() { time.Sleep(120 * time.Millisecond); release() }()
	t0 := time.Now()
	r := run(t, w, `SELECT id FROM sim WHERE num LIKE '%9990000%'`)
	waited := time.Since(t0)
	if len(r.Rows) != 1 || waited < 100*time.Millisecond || !strings.Contains(lastPlan(w), "engine=substr") {
		t.Fatalf("session read: rows=%d waited=%v plan=%s", len(r.Rows), waited, lastPlan(w))
	}
	// a view that never catches up: the core answers (still the session's own row)
	release2 := p.Sub.Hold()
	run(t, w, `INSERT INTO sim VALUES (1001, '0999000001')`)
	r = run(t, w, `SELECT id FROM sim WHERE num LIKE '%9990000%' ORDER BY id`)
	if len(r.Rows) != 2 || !strings.Contains(lastPlan(w), "engine=core") {
		t.Fatalf("fallback: rows=%d plan=%s", len(r.Rows), lastPlan(w))
	}
	// bounded, strong and eventual remain selectable
	run(t, w, `SET stask.consistency = bounded`)
	run(t, w, `SET stask.max_lag_ms = 5000`)
	r = run(t, w, `SELECT id FROM sim WHERE num LIKE '%9990000%'`)
	if !strings.Contains(lastPlan(w), "mode=bounded") {
		t.Fatalf("bounded not honoured: %s", lastPlan(w))
	}
	t.Logf("bounded with a held view found %d of 2 own rows (allowed by its contract): %s", len(r.Rows), lastPlan(w))
	run(t, w, `SET stask.consistency = eventual`)
	run(t, w, `SELECT id FROM sim WHERE num LIKE '%9990000%'`)
	if !strings.Contains(lastPlan(w), "mode=eventual") {
		t.Fatalf("eventual not honoured: %s", lastPlan(w))
	}
	release2()
	run(t, w, `SET stask.consistency = strong`)
	run(t, w, `SELECT id FROM sim WHERE num LIKE '%9990000%'`)
	if !strings.Contains(lastPlan(w), "mode=strong") {
		t.Fatalf("strong not honoured: %s", lastPlan(w))
	}
	// a fresh session that wrote nothing reads the view as it is
	r2 := NewSession(e)
	r2.Views = p
	run(t, r2, `SELECT id FROM sim WHERE num LIKE '%9990000%'`)
	if !strings.Contains(lastPlan(r2), "engine=substr") {
		t.Fatalf("fresh session: %s", lastPlan(r2))
	}
}
