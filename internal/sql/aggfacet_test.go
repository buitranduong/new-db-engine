package sql

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/spill"
)

// ---- fixtures ----

const aggSchema = `(number TEXT PRIMARY KEY, carrier TEXT, status TEXT, price BIGINT, name TEXT)`

func aggRows(rng *rand.Rand, n int) []string {
	const alpha = "0123456789abcdefABCDEF"
	seen := map[string]bool{}
	var rows []string
	for len(rows) < n {
		b := make([]byte, 8)
		for i := range b {
			b[i] = alpha[rng.Intn(len(alpha))]
		}
		num := string(b)
		if seen[num] {
			continue
		}
		seen[num] = true
		carrier, status, price := fmt.Sprintf("'c%d'", rng.Intn(5)), fmt.Sprintf("'s%d'", rng.Intn(3)), fmt.Sprint(rng.Intn(1000))
		if rng.Intn(25) == 0 {
			carrier = "NULL"
		}
		if rng.Intn(25) == 0 {
			price = "NULL"
		}
		rows = append(rows, fmt.Sprintf("('%s', %s, %s, %s, 'n%s%d')", num, carrier, status, price, num[:3], rng.Intn(100)))
	}
	return rows
}

func waitView(t *testing.T, e *core.Engine, p *planner.Platform) {
	t.Helper()
	for i := 0; i < 5000 && (!p.ColF.Ready() || p.ColF.Applied() < e.DurableLSN()); i++ {
		time.Sleep(time.Millisecond)
	}
	if !p.ColF.Ready() || p.ColF.Applied() < e.DurableLSN() {
		t.Fatal("columnar view did not catch up")
	}
}

// ---- EXPLAIN and lane classification of aggregates ----

func TestAggFacetExplainViewNode(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	s := NewSession(e)
	s.Views = p
	run(t, s, `CREATE TABLE sims `+aggSchema+` WITH (columnar = true)`)
	run(t, s, `CREATE TABLE plain `+aggSchema)
	rows := aggRows(rand.New(rand.NewSource(1)), 500)
	run(t, s, `INSERT INTO sims VALUES `+strings.Join(rows, ","))
	run(t, s, `INSERT INTO plain VALUES `+strings.Join(rows, ","))
	waitView(t, e, p)

	out := explainText(t, s, `EXPLAIN SELECT carrier, count(*) FROM sims WHERE status = 's1' GROUP BY carrier`)
	for _, w := range []string{"Columnar View Aggregate on sims", "Consistency: strong", "View: 500 live rows", "lane:"} {
		if !strings.Contains(out, w) {
			t.Errorf("want %q in\n%s", w, out)
		}
	}
	run(t, s, `SET stask.read_staleness = '2s'`)
	if out = explainText(t, s, `EXPLAIN SELECT count(*) FROM sims`); !strings.Contains(out, "Consistency: bounded by stask.read_staleness=2000ms") {
		t.Errorf("bounded contract not shown:\n%s", out)
	}
	run(t, s, `SET stask.read_staleness = 0`)
	// the contract is never weakened silently: strong is the default
	if out = explainText(t, s, `EXPLAIN SELECT count(*) FROM sims`); strings.Contains(out, "bounded") {
		t.Errorf("default must stay strong:\n%s", out)
	}
	// reasons when the view is not used
	for _, c := range []struct{ q, want string }{
		{`EXPLAIN SELECT carrier, count(*) FROM plain GROUP BY carrier`, "HashAggregate"},
		{`EXPLAIN SELECT carrier, min(name) FROM sims GROUP BY carrier`, "Columnar view not used: grouping, aggregate or filter not expressible"},
	} {
		if out = explainText(t, s, c.q); !strings.Contains(out, c.want) || strings.Contains(out, "Columnar View Aggregate") {
			t.Errorf("%s: want %q and no view node:\n%s", c.q, c.want, out)
		}
	}
	run(t, s, `BEGIN`)
	out = explainText(t, s, `EXPLAIN SELECT carrier, count(*) FROM sims GROUP BY carrier`)
	run(t, s, `ROLLBACK`)
	if !strings.Contains(out, "Columnar view not used: inside BEGIN") {
		t.Errorf("in a transaction the view must not be used:\n%s", out)
	}
	// EXPLAIN ANALYZE agrees with what ran
	if out = explainText(t, s, `EXPLAIN ANALYZE SELECT carrier, count(*) FROM sims GROUP BY carrier`); !strings.Contains(out, "Columnar view: used for 1 aggregate(s)") {
		t.Errorf("EXPLAIN ANALYZE:\n%s", out)
	}
}

func TestAggFacetLaneClassification(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	s := NewSession(e)
	s.Views = p
	run(t, s, `CREATE TABLE sims `+aggSchema+` WITH (columnar = true)`)
	run(t, s, `CREATE TABLE plain `+aggSchema)
	rows := aggRows(rand.New(rand.NewSource(2)), 300)
	run(t, s, `INSERT INTO sims VALUES `+strings.Join(rows, ","))
	run(t, s, `INSERT INTO plain VALUES `+strings.Join(rows, ","))
	waitView(t, e, p)
	defer func(n int64) { ViewNormalRows = n }(ViewNormalRows)

	heavy := func(q string) bool {
		t.Helper()
		st, err := ParseOne(q)
		if err != nil {
			t.Fatal(err)
		}
		return s.heavy(st, nil)
	}
	facet := `SELECT carrier, count(*) FROM sims GROUP BY carrier`
	ViewNormalRows = 1000
	if heavy(facet) || heavy(`SELECT count(*) FROM sims WHERE carrier = 'c1' AND status = 's2'`) ||
		heavy(`SELECT carrier, status, sum(price), min(price), max(price) FROM sims GROUP BY carrier, status HAVING count(*) > 2`) {
		t.Fatal("aggregates the fresh view answers cheaply must run on the normal lane")
	}
	ViewNormalRows = 100 // the view is now "big"
	if !heavy(facet) {
		t.Fatal("an aggregate over a big view must go to the heavy lane")
	}
	ViewNormalRows = 0
	if !heavy(facet) {
		t.Fatal("ViewNormalRows=0 sends every aggregate to the heavy lane")
	}
	ViewNormalRows = 1000
	if !heavy(`SELECT carrier, count(*) FROM plain GROUP BY carrier`) {
		t.Fatal("a table without a columnar view scans: heavy lane")
	}
	if !heavy(`SELECT carrier, min(name) FROM sims GROUP BY carrier`) {
		t.Fatal("an aggregate the view cannot express scans: heavy lane")
	}
	if !heavy(`SELECT number FROM sims WHERE carrier = 'c1'`) {
		t.Fatal("an unbounded scan stays heavy")
	}
	s.explicit = true
	if !heavy(facet) {
		t.Fatal("inside a transaction the view is not used: heavy lane")
	}
	s.explicit = false
	before := laneViewAggs.Load()
	heavy(facet)
	if laneViewAggs.Load() != before+1 {
		t.Fatal("view-lane aggregates must be counted")
	}
	// the platform tells the planner what the view holds
	rows64, lag, ok := p.ViewInfo("sims")
	if !ok || rows64 != 300 || lag > time.Second {
		t.Fatalf("ViewInfo = %d rows, lag %v, ok %v", rows64, lag, ok)
	}
	if _, _, ok := p.ViewInfo("plain"); ok {
		t.Fatal("a table without a view has no ViewInfo")
	}
}

func TestAutoHeavyWorkers(t *testing.T) {
	for cpus, want := range map[int]int{1: 0, 2: 0, 3: 0, 4: 1, 7: 1, 8: 2, 16: 4, 64: 4} {
		if got := AutoHeavyWorkers(cpus); got != want {
			t.Errorf("AutoHeavyWorkers(%d) = %d, want %d", cpus, got, want)
		}
	}
}

func TestLaneReportAndShapeMoves(t *testing.T) {
	st, _ := ParseOne(`SELECT number FROM sims WHERE number LIKE '%12' OR number LIKE '%34' LIMIT 50`)
	h0, n0 := shapeMovesHeavy.Load(), shapeMovesNormal.Load()
	for i := 0; i < 6; i++ {
		noteSelectTime(st, nil, 10*slowSelect)
	}
	if shapeMovesHeavy.Load() != h0+1 {
		t.Fatalf("moves to heavy = %d, want %d", shapeMovesHeavy.Load(), h0+1)
	}
	rep := LaneReport()
	shapes, _ := rep["heavy_shapes"].([]map[string]any)
	found := false
	for _, sh := range shapes {
		found = found || strings.Contains(fmt.Sprint(sh["shape"]), "sims")
	}
	if !found {
		t.Fatalf("the moved shape must be listed: %v", rep["heavy_shapes"])
	}
	for i := 0; i < 20; i++ {
		noteSelectTime(st, nil, time.Millisecond)
	}
	if shapeMovesNormal.Load() != n0+1 {
		t.Fatalf("moves to normal = %d, want %d", shapeMovesNormal.Load(), n0+1)
	}
	for _, k := range []string{"enabled", "statements", "shape_moves", "view_aggregate_lane", "view_aggregates"} {
		if _, ok := rep[k]; !ok {
			t.Errorf("LaneReport lacks %q", k)
		}
	}
}

// ---- the user-space scheduler of the heavy lane ----

func TestHeavyGovernorShare(t *testing.T) {
	now := time.Unix(1000, 0)
	sat := true
	g := newHeavyGov(0.25, 8, func() bool { return sat }) // 2 cores of 8
	g.now = func() time.Time { return now }
	g.sleep = func(d time.Duration) { now = now.Add(d) }
	g.last = now
	g.tokens = g.burst

	t0 := now
	g.wait()
	if now != t0 {
		t.Fatal("a worker with tokens must not wait")
	}
	g.charge(2 * time.Second) // 1.8 s of debt at 2 cores/s = 0.9 s
	g.wait()
	if w := now.Sub(t0); w < 850*time.Millisecond || w > 960*time.Millisecond {
		t.Fatalf("waited %v in debt, want about 900ms (debt / share)", w)
	}
	if g.waits.Load() != 1 {
		t.Fatalf("waits = %d", g.waits.Load())
	}
	// not saturated: no wait however deep the debt
	sat = false
	g.charge(10 * time.Second)
	t1 := now
	g.wait()
	if now != t1 {
		t.Fatal("heavy work must not wait while the node has spare CPU")
	}
	// never starved: a huge debt is capped, so the wait is bounded
	sat = true
	g.charge(1000 * time.Second)
	t2 := now
	g.wait()
	if w := now.Sub(t2); w > 2100*time.Millisecond {
		t.Fatalf("wait %v exceeds maxDebt/rate = 2s: heavy work could starve", w)
	}
	// long-run share: serial 500 ms tasks under saturation get at least the share
	now = time.Unix(2000, 0)
	g = newHeavyGov(0.25, 8, func() bool { return true })
	g.now = func() time.Time { return now }
	g.sleep = func(d time.Duration) { now = now.Add(d) }
	g.last, g.tokens = now, g.burst
	start := now
	var work time.Duration
	for now.Sub(start) < 60*time.Second {
		g.wait()
		now = now.Add(500 * time.Millisecond) // the task runs
		work += 500 * time.Millisecond
		g.charge(500 * time.Millisecond)
	}
	// one worker can use at most one core; the share is 2 cores, so it is never throttled
	if share := float64(work) / float64(now.Sub(start)); share < 0.95 {
		t.Fatalf("a single worker under a 2-core share ran %.0f%% of the time, want it unthrottled", 100*share)
	}
	// eight workers' worth of work against a 2-core share: throttled to the share
	now = time.Unix(3000, 0)
	g = newHeavyGov(0.25, 8, func() bool { return true })
	g.now = func() time.Time { return now }
	g.sleep = func(d time.Duration) { now = now.Add(d) }
	g.last, g.tokens = now, g.burst
	start, work = now, 0
	for now.Sub(start) < 60*time.Second {
		g.wait()
		task := 4 * time.Second // eight threads busy for 500 ms
		now = now.Add(500 * time.Millisecond)
		work += task
		g.charge(task)
	}
	cores := float64(work) / float64(now.Sub(start))
	if cores < 1.8 || cores > 2.3 {
		t.Fatalf("saturated heavy lane used %.2f cores, want about its 2-core share", cores)
	}
}

// ---- search-page access paths ----

func TestSearchPagePlans(t *testing.T) {
	e := newEngine(t)
	p := planner.Start(e)
	s := NewSession(e)
	s.Views = p
	run(t, s, `CREATE TABLE sims `+aggSchema+` WITH (columnar = true)`)
	run(t, s, `CREATE INDEX sims_csp ON sims (carrier, status, price)`)
	run(t, s, `CREATE INDEX sims_price ON sims (price, number)`)
	run(t, s, `CREATE INDEX sims_name ON sims (name)`)
	run(t, s, `CREATE INDEX sims_trgm ON sims USING gin (number gin_trgm_ops)`)
	run(t, s, `INSERT INTO sims VALUES `+strings.Join(aggRows(rand.New(rand.NewSource(3)), 2000), ","))
	waitView(t, e, p)

	cases := []struct {
		name, q string
		want    []string
		not     []string
	}{
		{"prefix LIKE on the primary key is a key range", `SELECT * FROM sims WHERE number LIKE 'ab%' LIMIT 50`,
			[]string{"Index Scan using sims_pkey", "number >= 'ab' AND number < 'ac'"}, []string{"Trigram", "Seq Scan"}},
		{"prefix LIKE on an indexed column is an index range", `SELECT * FROM sims WHERE name LIKE 'nab%' LIMIT 50`,
			[]string{"Index Scan using sims_name", "name >= 'nab' AND name < 'nac'"}, []string{"Seq Scan"}},
		{"suffix LIKE uses trigrams", `SELECT * FROM sims WHERE number LIKE '%ab12' LIMIT 50`, []string{"Trigram Scan on sims using sims_trgm"}, nil},
		{"contains LIKE uses trigrams", `SELECT * FROM sims WHERE number LIKE '%ab1%' LIMIT 50`, []string{"Trigram Scan on sims using sims_trgm"}, nil},
		{"ILIKE contains uses trigrams", `SELECT * FROM sims WHERE number ILIKE '%AB1%' LIMIT 50`, []string{"Trigram Scan on sims using sims_trgm"}, nil},
		{"ILIKE prefix uses trigrams", `SELECT * FROM sims WHERE number ILIKE 'AB%' LIMIT 50`, []string{"Trigram Scan on sims using sims_trgm"}, nil},
		{"contains without a trigram index says why", `SELECT * FROM sims WHERE name LIKE '%12%' LIMIT 50`,
			[]string{"Seq Scan", "no trigram index"}, nil},
		{"combined equality filters with ORDER BY LIMIT walk the index in order", `SELECT * FROM sims WHERE carrier = 'c1' AND status = 's1' ORDER BY price LIMIT 20`,
			[]string{"Index Scan using sims_csp", "no sort"}, []string{"Sort"}},
		{"combined filters with a range and DESC", `SELECT * FROM sims WHERE carrier = 'c1' AND status = 's1' AND price > 100 ORDER BY price DESC LIMIT 20`,
			[]string{"Index Scan Backward using sims_csp", "price > 100"}, []string{"Sort"}},
		{"count with filters comes from the columnar view", `SELECT count(*) FROM sims WHERE carrier = 'c1' AND status = 's1'`, []string{"Columnar View Aggregate on sims"}, nil},
		{"count with a contains filter uses the trigram view", `SELECT count(*) FROM sims WHERE number LIKE '%ab1%'`, []string{"Trigram Scan on sims using sims_trgm"}, nil},
		{"keyset on the primary key", `SELECT * FROM sims WHERE number > 'm' ORDER BY number LIMIT 50`,
			[]string{"Index Scan using sims_pkey", "number > 'm'", "no sort"}, []string{"Seq Scan"}},
		{"keyset on a secondary index pair", `SELECT * FROM sims WHERE (price, number) > (500, 'm') ORDER BY price, number LIMIT 50`,
			[]string{"Index Scan using sims_price", "price >= 500", "no sort"}, []string{"all entries"}},
		{"keyset spelled as OR", `SELECT * FROM sims WHERE price > 500 OR (price = 500 AND number > 'm') ORDER BY price, number LIMIT 50`,
			[]string{"Index Scan using sims_price", "price >= 500"}, []string{"all entries"}},
		{"DISTINCT ON reports its reason", `SELECT DISTINCT ON (carrier) carrier, price FROM sims ORDER BY carrier, price`,
			[]string{"Unique", "no loose index scan"}, nil},
		{"GROUP BY HAVING comes from the columnar view", `SELECT carrier, status, count(*), sum(price) FROM sims GROUP BY carrier, status HAVING count(*) > 3`,
			[]string{"Columnar View Aggregate on sims", "Group Key: carrier, status"}, []string{"Seq Scan"}},
		{"GROUP BY over a contains filter uses the trigram view", `SELECT carrier, count(*) FROM sims WHERE number LIKE '%ab1%' GROUP BY carrier`,
			[]string{"HashAggregate", "Trigram Scan"}, nil},
	}
	for _, c := range cases {
		out := explainText(t, s, "EXPLAIN "+c.q)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: want %q in\n%s", c.name, w, out)
			}
		}
		for _, w := range c.not {
			if strings.Contains(out, w) {
				t.Errorf("%s: did not want %q in\n%s", c.name, w, out)
			}
		}
	}
}

// ---- views and plans give the base table's answers ----

// aggDiffQueries are the search-page statements of the planner tests; T is
// the table. Each is deterministic (a unique tie-break in every ORDER BY).
func aggDiffQueries(rng *rand.Rand) []string {
	const alpha = "0123456789abcdefABCDEF"
	pre := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[rng.Intn(len(alpha))]
		}
		return string(b)
	}
	var qs []string
	for i := 0; i < 12; i++ {
		qs = append(qs,
			fmt.Sprintf(`SELECT number FROM T WHERE number LIKE '%s%%' ORDER BY number LIMIT 30`, pre(1+rng.Intn(3))),
			fmt.Sprintf(`SELECT number FROM T WHERE number LIKE '%%%s' ORDER BY number`, pre(2)),
			fmt.Sprintf(`SELECT number FROM T WHERE number LIKE '%%%s%%' ORDER BY number LIMIT 40`, pre(2)),
			fmt.Sprintf(`SELECT number FROM T WHERE number ILIKE '%%%s%%' AND carrier = 'c%d' ORDER BY price, number LIMIT 25`, pre(2), rng.Intn(5)),
			fmt.Sprintf(`SELECT number FROM T WHERE number ILIKE '%s%%' ORDER BY number`, pre(2)),
			fmt.Sprintf(`SELECT name, number FROM T WHERE name LIKE 'n%s%%' ORDER BY name, number`, pre(2)),
			fmt.Sprintf(`SELECT count(*) FROM T WHERE number LIKE '%%%s%%'`, pre(2)),
			fmt.Sprintf(`SELECT number, price FROM T WHERE number > '%s' ORDER BY number LIMIT 30`, pre(2)),
			fmt.Sprintf(`SELECT number FROM T WHERE number >= '%s' AND number < '%s' ORDER BY number`, pre(1), pre(2)),
			fmt.Sprintf(`SELECT number, price FROM T WHERE (price, number) > (%d, '%s') ORDER BY price, number LIMIT 30`, rng.Intn(1000), pre(2)),
			fmt.Sprintf(`SELECT number, price FROM T WHERE price > %d OR (price = %d AND number > '%s') ORDER BY price, number LIMIT 30`, rng.Intn(1000), rng.Intn(1000), pre(2)),
			fmt.Sprintf(`SELECT number, price FROM T WHERE carrier = 'c%d' AND status = 's%d' ORDER BY price, number LIMIT 25`, rng.Intn(5), rng.Intn(3)),
			fmt.Sprintf(`SELECT number, price FROM T WHERE carrier = 'c%d' AND price > %d ORDER BY price DESC, number DESC LIMIT 25`, rng.Intn(5), rng.Intn(1000)),
			fmt.Sprintf(`SELECT count(*), sum(price), min(price), max(price) FROM T WHERE carrier = 'c%d' AND status = 's%d' AND price BETWEEN %d AND %d`, rng.Intn(5), rng.Intn(3), rng.Intn(300), 300+rng.Intn(700)),
			fmt.Sprintf(`SELECT carrier, count(*) FROM T WHERE status = 's%d' GROUP BY carrier ORDER BY carrier`, rng.Intn(3)),
		)
	}
	qs = append(qs,
		`SELECT DISTINCT ON (carrier) carrier, price, number FROM T ORDER BY carrier, price, number`,
		`SELECT carrier, status, count(*), sum(price), min(price), max(price) FROM T GROUP BY carrier, status HAVING count(*) > 20 ORDER BY carrier, status`,
		`SELECT carrier, count(*) FROM T GROUP BY carrier ORDER BY carrier`,
		`SELECT count(*) FROM T`,
		`SELECT min(price), max(price), sum(price) FROM T`,
		`SELECT status, count(DISTINCT carrier) FROM T GROUP BY status ORDER BY status`,
		`SELECT carrier, count(*) FROM T WHERE number LIKE '%a%' GROUP BY carrier HAVING count(*) > 1 ORDER BY carrier`,
	)
	return qs
}

func dumpRows(t *testing.T, s *Session, q string) string {
	t.Helper()
	r := run(t, s, q)
	var out []string
	for _, row := range r.Rows {
		var cells []string
		for _, d := range row {
			if d.K == KFloat { // the sum order differs between the plans
				cells = append(cells, fmt.Sprintf("%.9g", d.F))
			} else {
				cells = append(cells, fmt.Sprint(d))
			}
		}
		out = append(out, strings.Join(cells, "|"))
	}
	if !strings.Contains(q, "ORDER BY") {
		sort.Strings(out)
	}
	return strings.Join(out, "\n")
}

func TestAggFacetDifferential(t *testing.T) {
	dir := t.TempDir()
	open := func() (*core.Engine, *planner.Platform) {
		e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return e, planner.Start(e)
	}
	e, p := open()
	// base has no index and no view: every answer is a scan of the rows. Its key is an int so
	// that no primary-key range applies to number either.
	w := NewSession(e)
	run(t, w, `CREATE TABLE base (id BIGINT PRIMARY KEY, number TEXT, carrier TEXT, status TEXT, price BIGINT, name TEXT)`)
	run(t, w, `CREATE TABLE fast `+aggSchema+` WITH (columnar = true)`)
	run(t, w, `CREATE INDEX fast_csp ON fast (carrier, status, price)`)
	run(t, w, `CREATE INDEX fast_price ON fast (price, number)`)
	run(t, w, `CREATE INDEX fast_name ON fast (name)`)
	run(t, w, `CREATE INDEX fast_trgm ON fast USING gin (number gin_trgm_ops)`)
	rng := rand.New(rand.NewSource(42))
	rows := aggRows(rng, 3000)
	var brows []string
	for i, r := range rows {
		brows = append(brows, fmt.Sprintf("(%d, %s", i, r[1:]))
	}
	for lo := 0; lo < len(rows); lo += 500 {
		hi := min(lo+500, len(rows))
		run(t, w, `INSERT INTO fast VALUES `+strings.Join(rows[lo:hi], ","))
		run(t, w, `INSERT INTO base VALUES `+strings.Join(brows[lo:hi], ","))
	}

	check := func(phase string, e *core.Engine, p *planner.Platform) {
		t.Helper()
		waitView(t, e, p)
		fast, base := NewSession(e), NewSession(e)
		fast.Views = p
		views0 := ViewAggregates.Load()
		for _, q := range aggDiffQueries(rand.New(rand.NewSource(7))) {
			want := dumpRows(t, base, strings.ReplaceAll(q, " T", " base"))
			got := dumpRows(t, fast, strings.ReplaceAll(q, " T", " fast"))
			if got != want {
				t.Fatalf("%s: %s\nfast:\n%.400s\nbase:\n%.400s", phase, q, got, want)
			}
		}
		if ViewAggregates.Load() == views0 {
			t.Fatalf("%s: no aggregate was answered from the columnar view", phase)
		}
	}
	check("loaded", e, p)

	// updates (indexed columns, NULLs, key-neutral), deletes and re-inserts
	for _, tb := range []string{"base", "fast"} {
		run(t, w, fmt.Sprintf(`UPDATE %s SET price = price + 17 WHERE carrier = 'c1'`, tb))
		run(t, w, fmt.Sprintf(`UPDATE %s SET status = 's2', name = 'zz' || name WHERE price %% 7 = 0`, tb))
		run(t, w, fmt.Sprintf(`UPDATE %s SET carrier = NULL WHERE price %% 31 = 3`, tb))
		run(t, w, fmt.Sprintf(`DELETE FROM %s WHERE price %% 11 = 5`, tb))
	}
	extra := aggRows(rand.New(rand.NewSource(99)), 400)
	exist := map[string]bool{}
	for _, r := range rows {
		exist[r[2:10]] = true
	}
	var fe, be []string
	for i, r := range extra {
		if exist[r[2:10]] {
			continue
		}
		fe = append(fe, r)
		be = append(be, fmt.Sprintf("(%d, %s", 100000+i, r[1:]))
	}
	run(t, w, `INSERT INTO fast VALUES `+strings.Join(fe, ","))
	run(t, w, `INSERT INTO base VALUES `+strings.Join(be, ","))
	check("after update/delete/insert", e, p)

	// restart: the log alone rebuilds the table, the indexes and the views
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, p = open()
	t.Cleanup(func() { e.Close() })
	check("after restart", e, p)
}

// ---- memory budget per statement (work_mem) ----

func TestWorkMemSpillsAggregatesAndSorts(t *testing.T) {
	s := spillFixture(t)
	defer func(q, n, sm, so int64, sd string) {
		QueryMemLimit, SQLMemLimit, SpillMaxBytes, SpillOpMem, SpillDir = q, n, sm, so, sd
	}(QueryMemLimit, SQLMemLimit, SpillMaxBytes, SpillOpMem, SpillDir)
	QueryMemLimit, SQLMemLimit, SpillOpMem, SpillDir = 0, 0, 0, ""
	queries := []struct {
		q     string
		spill bool // must write temp files under 300 kB
	}{
		{`SELECT k, count(*), sum(f), min(id) FROM a GROUP BY k ORDER BY k`, true},
		{`SELECT k, g, count(*) FROM a GROUP BY k, g HAVING count(*) >= 1 ORDER BY k, g LIMIT 500`, true},
		{`SELECT DISTINCT k, g FROM a ORDER BY k, g LIMIT 800`, true},
		{`SELECT id, tag FROM a ORDER BY tag, id LIMIT 1000`, true},
		{`SELECT id FROM a ORDER BY f DESC NULLS LAST, id LIMIT 300`, true},
		{`SELECT b.w, count(*), sum(a.f) FROM a JOIN b ON a.k = b.k GROUP BY b.w ORDER BY b.w`, true},
		{`SELECT b.w, a.k, count(*) FROM a JOIN b ON a.k = b.k GROUP BY b.w, a.k ORDER BY 3 DESC, 1, 2 LIMIT 100`, true},
		{`SELECT g, count(*) FROM a GROUP BY g ORDER BY g`, false}, // 500 small groups fit
	}
	want := make([]string, len(queries))
	for i, c := range queries {
		want[i] = dumpRows(t, s, c.q)
	}
	run(t, s, `SET work_mem = '300kB'`)

	// spilling off: the budget is enforced by failing, also on the single-table path
	if _, err := tryRun(s, queries[0].q); code(err) != "53200" {
		t.Fatalf("without spill space a big GROUP BY must fail with 53200, got %v", err)
	}
	if _, err := tryRun(s, queries[3].q); code(err) != "53200" {
		t.Fatalf("without spill space a big sort must fail with 53200, got %v", err)
	}

	SpillDir = t.TempDir()
	for i, c := range queries {
		before := spill.Snapshot()
		got := dumpRows(t, s, c.q)
		if got != want[i] {
			gl, wl := strings.Split(got, "\n"), strings.Split(want[i], "\n")
			for j := 0; j < min(len(gl), len(wl)); j++ {
				if gl[j] != wl[j] {
					t.Fatalf("%s: result under work_mem differs at row %d (%d vs %d rows): %q vs %q", c.q, j, len(gl), len(wl), gl[j], wl[j])
				}
			}
			t.Fatalf("%s: result under work_mem has %d rows, want %d", c.q, len(gl), len(wl))
		}
		files := spill.Snapshot().Files - before.Files
		t.Logf("%-100.100s spill files=%d", c.q, files)
		if c.spill && files == 0 {
			t.Errorf("%s: expected temp files under work_mem = 300kB", c.q)
		}
		if n := SQLMemInUse(); n != 0 {
			t.Fatalf("%d bytes still reserved", n)
		}
	}
	// the setting is per session and RESET lifts it
	other := NewSession(s.E)
	before := spill.Snapshot()
	if got := dumpRows(t, other, queries[0].q); got != want[0] || spill.Snapshot().Files != before.Files {
		t.Fatal("another session must not be limited by this session's work_mem")
	}
	run(t, s, `RESET work_mem`)
	before = spill.Snapshot()
	if got := dumpRows(t, s, queries[0].q); got != want[0] || spill.Snapshot().Files != before.Files {
		t.Fatal("RESET work_mem must lift the budget")
	}
	for in, w := range map[string]int64{"4MB": 4 << 20, "512": 512 << 10, "64 kB": 64 << 10, "1GB": 1 << 30, "": 0, "junk": 0, "-3": 0} {
		s.vars[WorkMemVar] = in
		if got := s.workMemBytes(); got != w {
			t.Errorf("work_mem %q = %d, want %d", in, got, w)
		}
	}
}

// Edge cases of the prefix/range paths: wildcards and escapes inside keys and
// patterns, multi-byte text, empty strings, bounds at the ends of the key space.
func TestPrefixRangeEdgeCases(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE pk (k TEXT PRIMARY KEY, v BIGINT)`)
	run(t, s, `CREATE TABLE sc (id BIGINT PRIMARY KEY, k TEXT, v BIGINT)`)
	run(t, s, `CREATE INDEX sc_k ON sc (k)`)
	run(t, s, `CREATE TABLE base (id BIGINT PRIMARY KEY, k TEXT, v BIGINT)`)
	keys := []string{"", "a", "a%", "a_", "ab", "a\\", "a\\%", "b", "é", "éa", "éé", "z", "zz", "zzz", "~", "A", "ABC", "a b", "a-b", "0", "09"}
	for i, k := range keys {
		q := "'" + strings.ReplaceAll(k, "'", "''") + "'"
		run(t, s, fmt.Sprintf(`INSERT INTO pk VALUES (%s, %d)`, q, i))
		run(t, s, fmt.Sprintf(`INSERT INTO sc VALUES (%d, %s, %d)`, i, q, i))
		run(t, s, fmt.Sprintf(`INSERT INTO base VALUES (%d, %s, %d)`, i, q, i))
	}
	pats := []string{"a%", "a\\%", "a\\\\%", "%", "%%", "_", "a_", "a%%", "é%", "z%", "zz%", "", "a", "0%", "~%", "A%", "a b%", "a\\_", "ab%", "é_"}
	for _, pat := range pats {
		lit := "'" + strings.ReplaceAll(pat, "'", "''") + "'"
		for _, c := range []struct{ tbl, key string }{{"pk", "k"}, {"sc", "k"}} {
			for _, tail := range []string{"", " ORDER BY k LIMIT 3", " ORDER BY k LIMIT 100"} {
				q := fmt.Sprintf(`SELECT k, v FROM %s WHERE k LIKE %s`, c.tbl, lit)
				b := fmt.Sprintf(`SELECT k, v FROM base WHERE k LIKE %s`, lit)
				if tail != "" {
					q += tail
					b += tail
				} else {
					q += " ORDER BY k"
					b += " ORDER BY k"
				}
				if got, want := dumpRows(t, s, q), dumpRows(t, s, b); got != want {
					t.Fatalf("%s:\n%s\nvs base:\n%s", q, got, want)
				}
			}
		}
	}
	for _, w := range []string{`k > 'a'`, `k >= 'a'`, `k < 'a'`, `k <= 'a'`, `k > ''`, `k >= ''`, `k < ''`, `k <= ''`, `k BETWEEN 'a' AND 'b'`, `k > 'z' AND k < 'zzz'`,
		`k > 'zzzz'`, `k >= 'é'`, `k > 'a' OR k < 'A'`, `(k > 'a' AND k < 'b') OR k = 'z'`, `k > 'zzz' OR (k = 'zzz' AND v > 0)`} {
		for _, tbl := range []string{"pk", "sc"} {
			for _, tail := range []string{" ORDER BY k", " ORDER BY k LIMIT 4", " ORDER BY k DESC LIMIT 4"} {
				q := fmt.Sprintf(`SELECT k, v FROM %s WHERE %s%s`, tbl, w, tail)
				b := fmt.Sprintf(`SELECT k, v FROM base WHERE %s%s`, w, tail)
				if got, want := dumpRows(t, s, q), dumpRows(t, s, b); got != want {
					t.Fatalf("%s:\n%s\nvs base:\n%s", q, got, want)
				}
			}
		}
	}
	// a transaction that wrote the table reads its own writes: no range path, same answers
	run(t, s, `BEGIN`)
	run(t, s, `INSERT INTO pk VALUES ('azz', 99)`)
	if got := dumpRows(t, s, `SELECT k FROM pk WHERE k LIKE 'az%' ORDER BY k`); got != "azz" {
		t.Fatalf("own write invisible: %q", got)
	}
	run(t, s, `ROLLBACK`)
}
