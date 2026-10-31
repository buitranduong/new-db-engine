package sql

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/proj"
)

// Hybrid path (docs/31 9): the answer of the segment path equals the answer
// of the row executor, row for row and column for column, on the base, after
// changes to the tail, after changes to the dimension; EXPLAIN keeps its
// contract; every fallback says why.

const hyGen = `INSERT INTO sims SELECT '09' || lpad(g::text, 9, '0'),
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + g %% 5],
  500000 + ((g::bigint*7919) %% 4000000)::int,
  CASE WHEN (((g::bigint/5)*13) %% 10) < 3 THEN 'A' WHEN (((g::bigint/5)*13) %% 10) < 8 THEN 'B' ELSE 'C' END,
  (5 + (g::bigint*31) %% 6)::int
FROM generate_series(%d, %d) g`

func hyQuery(page int, join bool) string {
	if join {
		return fmt.Sprintf(`SELECT number, name, price, store_rate FROM (
  SELECT s.number, c.name, s.tel, s.price, s.store_rate,
         row_number() OVER (PARTITION BY s.tel ORDER BY s.store_rate DESC, md5(s.number || '42'), s.number) AS rn
  FROM sims s JOIN carriers c ON c.tel = s.tel AND c.active
  WHERE s.category = 'TAM_HOA' AND s.price BETWEEN 1000000 AND 3000000
) t
WHERE (tel='A' AND rn BETWEEN (%d-1)*20+1 AND %d*20) OR (tel='B' AND rn BETWEEN (%d-1)*50+1 AND %d*50) OR (tel='C' AND rn BETWEEN (%d-1)*30+1 AND %d*30)
ORDER BY store_rate DESC, md5(number || '42'), number`, page, page, page, page, page, page)
	}
	return fmt.Sprintf(`SELECT number, tel, price, store_rate FROM (
  SELECT number, tel, price, store_rate,
         row_number() OVER (PARTITION BY tel ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims
  WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND tel IN ('A','B','C')
) t
WHERE (tel='A' AND rn BETWEEN (%d-1)*20+1 AND %d*20)
   OR (tel='B' AND rn BETWEEN (%d-1)*50+1 AND %d*50)
   OR (tel='C' AND rn BETWEEN (%d-1)*30+1 AND %d*30)
ORDER BY store_rate DESC, md5(number || '42'), number`, page, page, page, page, page, page)
}

type hyEnv struct {
	t       *testing.T
	hy, row *Session
	h       *Hybrid
}

func newHyEnv(t *testing.T, n int) *hyEnv {
	e := newEngine(t)
	hs, rs := NewSession(e), NewSession(e)
	mustq(t, rs, `CREATE TABLE sims (number TEXT PRIMARY KEY, category TEXT, price INT, tel TEXT, store_rate INT) WITH (columnar = on)`)
	mustq(t, rs, `CREATE TABLE carriers (tel TEXT PRIMARY KEY, name TEXT, active BOOLEAN)`)
	mustq(t, rs, `INSERT INTO carriers VALUES ('A','A',true),('B','B',true),('C','C',true)`)
	mustq(t, rs, fmt.Sprintf(hyGen, 0, n-1))
	// the segment state is built from the data already in the core (Bootstrap)
	h := NewHybridLogf(e, t.Logf)
	hs.Hybrid = h
	v := &hyEnv{t: t, hy: hs, row: rs, h: h}
	v.sync()
	return v
}

func (v *hyEnv) sync() {
	v.t.Helper()
	if !v.h.Seg.Follower().WaitApplied(v.h.E.DurableLSN(), 30*time.Second) {
		v.t.Fatal("segment follower did not catch up")
	}
}

func (v *hyEnv) cmp(what string, q string) *hyReport {
	v.t.Helper()
	v.sync()
	got, err := tryRun(v.hy, q)
	if err != nil {
		v.t.Fatalf("%s: hybrid session: %v", what, err)
	}
	rep := v.hy.hy.last
	want, err := tryRun(v.row, q)
	if err != nil {
		v.t.Fatalf("%s: row session: %v", what, err)
	}
	if rep == nil || !rep.used {
		v.t.Fatalf("%s: the hybrid path was not used: %+v", what, rep)
	}
	if len(got.Cols) != len(want.Cols) {
		v.t.Fatalf("%s: %d columns, want %d", what, len(got.Cols), len(want.Cols))
	}
	for i := range got.Cols {
		if got.Cols[i] != want.Cols[i] {
			v.t.Errorf("%s: column %d is %+v, row path says %+v", what, i, got.Cols[i], want.Cols[i])
		}
	}
	if g, w := render(got), render(want); g != w || got.Tag != want.Tag {
		v.t.Fatalf("%s: hybrid and row path differ (%d vs %d rows):\n hybrid %s\n row    %s", what, len(got.Rows), len(want.Rows), g, w)
	}
	for i := range got.Rows {
		for j := range got.Rows[i] {
			if got.Rows[i][j].K != want.Rows[i][j].K {
				v.t.Fatalf("%s: row %d column %d has datum kind %d, row path %d", what, i, j, got.Rows[i][j].K, want.Rows[i][j].K)
			}
		}
	}
	if len(got.Rows) == 0 {
		v.t.Fatalf("%s: empty result proves nothing", what)
	}
	return rep
}

func TestHybridR2MatchesRowPath(t *testing.T) {
	oldMin, oldFrac := proj.SegMergeMinTail, proj.SegMergeFrac
	proj.SegMergeMinTail, proj.SegMergeFrac = 300, 1000
	t.Cleanup(func() { proj.SegMergeMinTail, proj.SegMergeFrac = oldMin, oldFrac }) // runs after the engine is closed
	v := newHyEnv(t, 30000)
	steps := func(label string) {
		for _, join := range []bool{false, true} {
			for page := 1; page <= 3; page++ {
				v.cmp(fmt.Sprintf("%s page %d join=%v", label, page, join), hyQuery(page, join))
			}
		}
	}
	steps("base")
	// changes land in the tail: updates (one key twice), deletes, inserts
	mustq(t, v.row, `UPDATE sims SET store_rate = 10 WHERE number IN ('090000000000','090000000005','090000000010','090000000015','090000000020')`)
	mustq(t, v.row, `UPDATE sims SET store_rate = 5, price = 1500000 WHERE number = '090000000005'`)
	mustq(t, v.row, `UPDATE sims SET price = 2000000 WHERE number IN (SELECT number FROM sims WHERE category = 'TAM_HOA' AND price > 3000000 LIMIT 40)`)
	mustq(t, v.row, `DELETE FROM sims WHERE number IN (SELECT number FROM sims WHERE category = 'TAM_HOA' AND tel = 'B' LIMIT 25)`)
	mustq(t, v.row, fmt.Sprintf(hyGen, 30000, 30500))
	steps("after tail")
	// a merge in between (the knobs make the tail small enough)
	mustq(t, v.row, `UPDATE sims SET store_rate = 9 WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 1200000`)
	steps("after merge")
	if v.h.Seg.Merges.Load() == 0 {
		t.Fatal("no merge ran")
	}
	// the dimension: rename a carrier, switch one off
	mustq(t, v.row, `UPDATE carriers SET name = 'C2' WHERE tel = 'C'`)
	rep := v.cmp("dimension renamed", hyQuery(1, true))
	if rep.dimPass != 3 {
		t.Fatalf("dim rows passing: %d", rep.dimPass)
	}
	mustq(t, v.row, `UPDATE carriers SET active = false WHERE tel = 'B'`)
	rep = v.cmp("dimension B off", hyQuery(2, true))
	if rep.dimPass != 2 {
		t.Fatalf("dim rows passing after B off: %d", rep.dimPass)
	}
	res, _ := tryRun(v.hy, hyQuery(1, true))
	for _, r := range res.Rows {
		if r[1].S == "B" {
			t.Fatal("a row of the switched-off carrier B")
		}
	}
	// ack-then-read: a commit acknowledged to this session is visible at once
	for i := 0; i < 200; i++ {
		mustq(t, v.hy, fmt.Sprintf(`INSERT INTO sims VALUES ('08%09d','TAM_HOA',1500000,'A',11)`, i))
		r, err := tryRun(v.hy, strings.Replace(hyQuery(1, false), "AND tel IN ('A','B','C')", "AND tel IN ('A','B','C')", 1))
		if err != nil {
			t.Fatal(err)
		}
		if !v.hy.hy.last.used {
			t.Fatalf("round %d: path not used: %s", i, v.hy.hy.last.why)
		}
		if len(r.Rows) == 0 || r.Rows[0][3].I != 11 {
			t.Fatalf("round %d: the row just committed (store_rate 11) is not first: %s", i, render(r))
		}
		if i%20 == 0 {
			want, _ := tryRun(v.row, hyQuery(1, false))
			if render(r) != render(want) {
				t.Fatalf("round %d differs from the row path", i)
			}
		}
	}
}

func TestHybridFallbacksSayWhy(t *testing.T) {
	v := newHyEnv(t, 3000)
	mustq(t, v.row, `CREATE TABLE plain (number TEXT PRIMARY KEY, category TEXT, price INT, tel TEXT, store_rate INT)`)
	q := strings.ReplaceAll(hyQuery(1, false), "FROM sims", "FROM plain")
	if _, err := tryRun(v.hy, q); err != nil {
		t.Fatal(err)
	}
	if r := v.hy.hy.last; r == nil || r.used || !strings.Contains(r.why, "not columnar") {
		t.Fatalf("a table without columnar must say so: %+v", r)
	}
	// not an R2 candidate: no report
	mustq(t, v.hy, `SELECT count(*) FROM sims`)
	if v.hy.hy.last != nil {
		t.Fatal("a plain statement left a hybrid report")
	}
	// inside an explicit transaction: allowed while it has written nothing
	mustq(t, v.hy, `BEGIN`)
	mustq(t, v.hy, hyQuery(1, false))
	if r := v.hy.hy.last; r == nil || !r.used {
		t.Fatalf("a read-only explicit transaction should use the path: %+v", r)
	}
	mustq(t, v.hy, `INSERT INTO sims VALUES ('07000000001','TAM_HOA',1500000,'A',10)`)
	mustq(t, v.hy, hyQuery(1, false))
	if r := v.hy.hy.last; r == nil || r.used || !strings.Contains(r.why, "uncommitted writes") {
		t.Fatalf("explicit transaction with writes: %+v", r)
	}
	mustq(t, v.hy, `ROLLBACK`)
	mustq(t, v.hy, `BEGIN ISOLATION LEVEL SERIALIZABLE`)
	mustq(t, v.hy, hyQuery(1, false))
	if r := v.hy.hy.last; r == nil || r.used || !strings.Contains(r.why, "SERIALIZABLE") {
		t.Fatalf("serializable: %+v", r)
	}
	mustq(t, v.hy, `ROLLBACK`)
	// an unsupported predicate
	q = strings.Replace(hyQuery(1, false), "price BETWEEN 1000000 AND 3000000", "price % 7 = 0", 1)
	mustq(t, v.hy, q)
	if r := v.hy.hy.last; r == nil || r.used || r.why == "" {
		t.Fatalf("unsupported predicate: %+v", r)
	}
	// with the path off nothing is recorded
	mustq(t, v.row, hyQuery(1, false))
	if v.row.hy.last != nil {
		t.Fatal("a session without -hybrid has a report")
	}
	// bounded and eventual use the newest state
	for _, mode := range []string{"bounded", "eventual", "strong"} {
		mustq(t, v.hy, "SET stask.consistency = '"+mode+"'")
		v.cmp("mode "+mode, hyQuery(1, false))
	}
	mustq(t, v.hy, "SET stask.consistency = 'session'")
	// a budget violated: rank range beyond the budget falls back with the reason
	big := strings.ReplaceAll(hyQuery(1, false), "*20", "*20000000")
	mustq(t, v.hy, big)
	if r := v.hy.hy.last; r == nil || r.used || !strings.Contains(r.why, "budget") {
		t.Fatalf("rank beyond the budget: %+v", r)
	}
}

// The EXPLAIN contract of docs/30 4.5 / docs/31 7: the template, not the numbers.
func TestHybridExplainTemplate(t *testing.T) {
	v := newHyEnv(t, 3000)
	out := func(q string) string {
		r := mustq(t, v.hy, q)
		var ls []string
		for _, row := range r.Rows {
			ls = append(ls, row[0].S)
		}
		return strings.Join(ls, "\n")
	}
	must := func(text string, parts ...string) {
		t.Helper()
		for _, p := range parts {
			if !strings.Contains(text, p) {
				t.Errorf("EXPLAIN lacks %q:\n%s", p, text)
			}
		}
	}
	ex := out("EXPLAIN " + hyQuery(1, true))
	must(ex, "Hybrid Plan  (contract: strong", "snapshot lsn=", "state lsn=", "applied lsn=", "view lag=", "delta size:",
		"Top-N per Partition on sims", "ranks kept per partition: A: 1-20 / B: 1-50 / C: 1-30", "Partition: tel",
		"Order: store_rate DESC, md5(number || '42'), number", "Path: SEGMENT(base)+TAIL+DIM-BROADCAST", "Why:", "Rejected: ROW-EXECUTOR",
		"Read base:", "Read tail:", "Read gap:", "DENSE-BROADCAST build=carriers", "Read core:", "Memory:", "lane: normal", "Planning: shape=R2")
	if strings.Contains(ex, "Seq Scan") {
		t.Errorf("Seq Scan in the plan of the R2 query is a failure:\n%s", ex)
	}
	t.Logf("EXPLAIN:\n%s", ex)
	an := out("EXPLAIN ANALYZE " + hyQuery(1, true))
	t.Logf("EXPLAIN ANALYZE:\n%s", an)
	must(an, "Hybrid Plan  (contract: strong", "chunks (", "passed the filter", "hidden by the changed-rows bitmap", "md5 evaluated",
		"rows fetched by key at snapshot", "Execution:", "100 rows returned")
	// not used: says why, and the ordinary plan follows
	mustq(t, v.row, `CREATE TABLE plain (number TEXT PRIMARY KEY, category TEXT, price INT, tel TEXT, store_rate INT)`)
	nu := out("EXPLAIN " + strings.ReplaceAll(hyQuery(1, false), "FROM sims", "FROM plain"))
	must(nu, "Hybrid not used:", "not columnar")
	// a plain statement is not annotated
	if p := out("EXPLAIN SELECT count(*) FROM sims"); strings.Contains(p, "Hybrid") {
		t.Errorf("a non-R2 statement mentions the hybrid path:\n%s", p)
	}
	// off: EXPLAIN is the old one
	r := mustq(t, v.row, "EXPLAIN "+hyQuery(1, false))
	for _, row := range r.Rows {
		if strings.Contains(row[0].S, "Hybrid") {
			t.Errorf("hybrid off but EXPLAIN says %q", row[0].S)
		}
	}
}

// R-prio (docs/29): the hybrid path keeps the R2 query on the normal lane.
func TestHybridR2StaysOnNormalLane(t *testing.T) {
	v := newHyEnv(t, 3000)
	st, err := ParseOne(hyQuery(1, true))
	if err != nil {
		t.Fatal(err)
	}
	if v.hy.heavy(st, nil) {
		t.Fatal("the R2 quota query must not start on the heavy lane")
	}
	for i := 0; i < 6; i++ {
		noteSelectTime(st, nil, 20*slowSelect)
	}
	if v.hy.heavy(st, nil) {
		t.Fatal("slow runs must not move the R2 query to the heavy lane")
	}
	if rep := v.cmp("lane", hyQuery(1, true)); rep == nil {
		t.Fatal("no report")
	}
	if v.hy.heavy(st, nil) {
		t.Fatal("running through the hybrid path moved the query to the heavy lane")
	}
}

// A writer commits while a reader re-reads at fixed snapshots: at every
// snapshot the segment answer equals the row executor's answer at that same
// snapshot, with merges happening in between.
func TestHybridConcurrentWriterFixedSnapshot(t *testing.T) {
	oldMin, oldFrac := proj.SegMergeMinTail, proj.SegMergeFrac
	proj.SegMergeMinTail, proj.SegMergeFrac = 200, 1000
	t.Cleanup(func() { proj.SegMergeMinTail, proj.SegMergeFrac = oldMin, oldFrac })
	v := newHyEnv(t, 20000)
	w := NewSession(v.h.E)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		rng := uint32(7)
		next := func(n uint32) uint32 { rng = rng*1664525 + 1013904223; return (rng >> 8) % n }
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			k := fmt.Sprintf("09%09d", next(20000))
			var q string
			switch next(5) {
			case 0:
				q = fmt.Sprintf("DELETE FROM sims WHERE number = '%s'", k)
			case 1:
				q = fmt.Sprintf("INSERT INTO sims VALUES ('08%09d','TAM_HOA',%d,'%s',%d) ON CONFLICT (number) DO UPDATE SET store_rate = excluded.store_rate",
					next(3000), 1000000+next(2000000), []string{"A", "B", "C"}[next(3)], 5+next(6))
			default:
				q = fmt.Sprintf("UPDATE sims SET store_rate = %d, price = %d WHERE number = '%s'", 5+next(6), 1000000+next(2500000), k)
			}
			if _, err := tryRun(w, q); err != nil {
				done <- fmt.Errorf("%s: %w", q, err)
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()
	st, err := ParseOne(hyQuery(1, true))
	if err != nil {
		t.Fatal(err)
	}
	sel := st.(*Select)
	reads := 0
	for i := 0; i < 150; i++ {
		page := 1 + i%3
		sel = hyMustSel(t, hyQuery(page, i%2 == 0))
		tx := v.h.E.Begin(core.SnapshotIsolation)
		got, ok, err := v.hy.tryHybrid(tx, sel, nil)
		if err != nil || !ok {
			tx.Abort()
			t.Fatalf("read %d: hybrid not used: %v %+v", i, err, v.hy.hy.last)
		}
		time.Sleep(time.Duration(i%4) * time.Millisecond) // let the writer move on: the snapshot stays fixed
		want, err := v.hy.runQuery(tx, sel, nil)
		tx.Abort()
		if err != nil {
			t.Fatal(err)
		}
		if render(got) != render(want) {
			t.Fatalf("read %d at snapshot %d: hybrid and row path differ:\n hybrid %s\n row    %s", i, v.hy.hy.last.snap, render(got), render(want))
		}
		reads++
	}
	if v.h.Seg.Merges.Load() == 0 {
		t.Fatal("no merge happened under the writer")
	}
	t.Logf("%d reads equal at their fixed snapshots, %d merges, %d publishes", reads, v.h.Seg.Merges.Load(), v.h.Seg.Publishes.Load())
}

func hyMustSel(t *testing.T, q string) *Select {
	t.Helper()
	st, err := ParseOne(q)
	if err != nil {
		t.Fatal(err)
	}
	return st.(*Select)
}

// The state is derived: after the engine is closed and opened again the new
// segment state is rebuilt from the core and answers like the row executor
// (the in-process half of the kill -9 check; scripts/slice1/oracle.sh does it
// with a real kill -9).
func TestHybridRebuildFromCore(t *testing.T) {
	dir := t.TempDir()
	open := func() *core.Engine {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := open()
	s := NewSession(e)
	mustq(t, s, `CREATE TABLE sims (number TEXT PRIMARY KEY, category TEXT, price INT, tel TEXT, store_rate INT) WITH (columnar = on)`)
	mustq(t, s, `CREATE TABLE carriers (tel TEXT PRIMARY KEY, name TEXT, active BOOLEAN)`)
	mustq(t, s, `INSERT INTO carriers VALUES ('A','A',true),('B','B',true),('C','C',true)`)
	mustq(t, s, fmt.Sprintf(hyGen, 0, 9999))
	mustq(t, s, `UPDATE sims SET store_rate = 10 WHERE category = 'TAM_HOA' AND price < 1500000`)
	mustq(t, s, `DELETE FROM sims WHERE number LIKE '0900000%'`)
	mustq(t, s, `UPDATE carriers SET name = 'B2' WHERE tel = 'B'`)
	want := render(mustq(t, s, hyQuery(2, true)))
	e.Close()
	e = open()
	defer e.Close()
	h := NewHybridLogf(e, t.Logf)
	hs := NewSession(e)
	hs.Hybrid = h
	if !h.Seg.Follower().WaitApplied(e.DurableLSN(), 30*time.Second) {
		t.Fatal("segment state not rebuilt")
	}
	got := render(mustq(t, hs, hyQuery(2, true)))
	if hs.hy.last == nil || !hs.hy.last.used {
		t.Fatalf("path not used after the restart: %+v", hs.hy.last)
	}
	if got != want {
		t.Fatalf("after restart the segment answer differs:\n got  %s\n want %s", got, want)
	}
}

// A REPEATABLE READ transaction re-reads the same snapshot after the ring of
// retained states has moved past it: the transaction pinned its state.
func TestHybridExplicitTxPinsItsState(t *testing.T) {
	oldAge := proj.SegRingAge
	proj.SegRingAge = 50 * time.Millisecond
	t.Cleanup(func() { proj.SegRingAge = oldAge })
	v := newHyEnv(t, 5000)
	mustq(t, v.hy, `BEGIN ISOLATION LEVEL REPEATABLE READ`)
	first := render(mustq(t, v.hy, hyQuery(1, true)))
	if !v.hy.hy.last.used {
		t.Fatalf("first read: %s", v.hy.hy.last.why)
	}
	for i := 0; i < 80; i++ { // far more states than SegRingMin, and old enough
		mustq(t, v.row, fmt.Sprintf(`INSERT INTO sims VALUES ('07%09d','TAM_HOA',1500000,'A',12)`, i))
	}
	time.Sleep(120 * time.Millisecond)
	mustq(t, v.row, `INSERT INTO sims VALUES ('07999999999','TAM_HOA',1500000,'A',12)`)
	v.sync()
	again := render(mustq(t, v.hy, hyQuery(1, true)))
	if !v.hy.hy.last.used {
		t.Fatalf("second read in the transaction did not use the path: %s", v.hy.hy.last.why)
	}
	if first != again {
		t.Fatalf("a REPEATABLE READ transaction saw a change:\n %s\n %s", first, again)
	}
	mustq(t, v.hy, `COMMIT`)
	after := render(mustq(t, v.hy, hyQuery(1, true)))
	if after == first {
		t.Fatal("after COMMIT the new rows must be visible")
	}
}

// The harness shape (scripts/m2-harness): integer partition column, dimension
// tables joined OUTSIDE the derived table by integer keys. Row for row equal
// to the row executor, after tail changes, after a dimension change, and the
// INNER JOIN drops page rows whose dimension row is missing.
func hyOuterQuery(page int) string {
	lo := func(q int) string { return fmt.Sprintf("t.rn BETWEEN %d AND %d", (page-1)*q+1, page*q) }
	return fmt.Sprintf(`SELECT t.number, c.code, st.name, t.price, t.store_rate FROM (
  SELECT number, carrier_id, store_id, price, store_rate,
         row_number() OVER (PARTITION BY carrier_id ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3)) t
JOIN carriers c ON c.id = t.carrier_id JOIN stores st ON st.id = t.store_id
WHERE (t.carrier_id = 1 AND %s) OR (t.carrier_id = 2 AND %s) OR (t.carrier_id = 3 AND %s)
ORDER BY t.store_rate DESC, md5(t.number || '42'), t.number`, lo(30), lo(50), lo(20))
}

func TestHybridIntGroupOuterJoins(t *testing.T) {
	e := newEngine(t)
	hs, rs := NewSession(e), NewSession(e)
	mustq(t, rs, `CREATE TABLE sims (number TEXT PRIMARY KEY, carrier_id INT NOT NULL, store_id INT NOT NULL, category TEXT NOT NULL, price INT NOT NULL, store_rate INT NOT NULL, status TEXT NOT NULL) WITH (columnar = on)`)
	mustq(t, rs, `CREATE TABLE carriers (id INT PRIMARY KEY, code TEXT NOT NULL, name TEXT NOT NULL)`)
	mustq(t, rs, `CREATE TABLE stores (id INT PRIMARY KEY, name TEXT NOT NULL)`)
	mustq(t, rs, `INSERT INTO carriers VALUES (1,'VT','Viettel'),(2,'MB','Mobi'),(3,'VN','Vina')`)
	mustq(t, rs, `INSERT INTO stores SELECT g, 'store' || g::text FROM generate_series(1, 40) g`)
	mustq(t, rs, `INSERT INTO sims SELECT '09' || lpad(g::text, 9, '0'), 1 + g % 3, 1 + g % 40,
  (ARRAY['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'])[1 + g % 5],
  500000 + ((g::bigint*7919) % 4000000)::int, (5 + (g::bigint*31) % 6)::int,
  CASE WHEN g % 7 = 0 THEN 'sold' ELSE 'available' END FROM generate_series(0, 29999) g`)
	h := NewHybridLogf(e, t.Logf)
	hs.Hybrid = h
	v := &hyEnv{t: t, hy: hs, row: rs, h: h}
	v.sync()
	check := func(label string) {
		for page := 1; page <= 3; page++ {
			rep := v.cmp(fmt.Sprintf("%s page %d", label, page), hyOuterQuery(page))
			if rep.plan == nil || rep.plan.post == nil || !rep.plan.groupInt {
				t.Fatalf("%s: not the int-group/outer-join plan", label)
			}
		}
	}
	check("base")
	mustq(t, rs, `UPDATE sims SET store_rate = 10 WHERE number IN ('09000000001','090000000002','090000000003','090000000010')`)
	mustq(t, rs, `UPDATE sims SET status = 'sold' WHERE category = 'TAM_HOA' AND carrier_id = 2 AND price < 1500000`)
	mustq(t, rs, `UPDATE sims SET carrier_id = 3 WHERE number IN (SELECT number FROM sims WHERE carrier_id = 1 AND category = 'TAM_HOA' LIMIT 30)`)
	check("after tail")
	// a store disappears: its page rows leave the output (INNER JOIN), the ranks do not move
	mustq(t, rs, `DELETE FROM stores WHERE id IN (1, 2, 3)`)
	check("after dimension delete")
	mustq(t, rs, `UPDATE carriers SET code = 'VTX' WHERE id = 1`)
	check("after dimension rename")
	// a primary key range (the frozen sub-population of the harness), also over tail changes
	for _, kr := range []string{"AND number >= '090000015000'", "AND number > '090000010000' AND number <= '090000012000'", "AND number BETWEEN '090000005000' AND '090000007000'"} {
		q := strings.Replace(hyOuterQuery(1), "AND carrier_id IN (1,2,3))", "AND carrier_id IN (1,2,3) "+kr+")", 1)
		if q == hyOuterQuery(1) {
			t.Fatal("test query not rewritten")
		}
		v.cmp("key range "+kr, q)
		mustq(t, rs, `UPDATE sims SET store_rate = 10 WHERE number IN ('090000015001','090000015002','090000011000','090000006000')`)
		v.cmp("key range after tail "+kr, q)
	}
}

// The follower is a goroutine: if it does not move for a while (CPU saturated by readers) a strong read must still
// wait for it up to the deadline, and a wait that ran out makes the next ones fall back at once.
func TestHybridWaitAppliedPatient(t *testing.T) {
	v := newHyEnv(t, 2000)
	h, f := v.h, v.h.Seg.Follower()
	lsn := h.E.DurableLSN() + 1
	go func() {
		time.Sleep(450 * time.Millisecond) // longer than the old 200 ms "no progress" cut-off
		mustq(t, NewSession(h.E), `UPDATE sims SET store_rate = 9 WHERE number = '09000000001'`)
	}()
	t0 := time.Now()
	if !h.waitApplied(f, lsn, 5*time.Second) {
		t.Fatalf("waitApplied gave up after %v although the commit arrives after 450 ms", time.Since(t0))
	}
	if time.Since(t0) < 400*time.Millisecond {
		t.Fatalf("returned after %v, before the commit existed", time.Since(t0))
	}
	// a snapshot that never arrives: waits for the deadline, then the memo answers at once
	t0 = time.Now()
	if h.waitApplied(f, lsn+1000, 500*time.Millisecond) {
		t.Fatal("reached an LSN that does not exist")
	}
	if d := time.Since(t0); d < 450*time.Millisecond || d > 3*time.Second {
		t.Fatalf("first wait took %v, want about 500 ms", d)
	}
	t0 = time.Now()
	if h.waitApplied(f, lsn+1000, 500*time.Millisecond) || time.Since(t0) > 50*time.Millisecond {
		t.Fatalf("second wait did not fall back at once (%v)", time.Since(t0))
	}
}
