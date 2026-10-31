package sql

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/proj"
)

// compare checks that the view and the scan agree on a battery of pattern
// statements, and that the view answered.
func trgCompare(t *testing.T, scan, idx *Session, pats []string, when string) {
	t.Helper()
	run(t, idx, "SET stask.consistency = strong")
	n := 0
	qs := []string{}
	for _, pat := range pats {
		for _, op := range []string{"LIKE", "ILIKE"} {
			qs = append(qs, fmt.Sprintf("SELECT id FROM sim WHERE num %s %s", op, sqlQuote(pat)))
		}
	}
	for _, re := range []string{"^888", "888$", "8+8", "(?i)AB", "[0-9]88"} {
		qs = append(qs, "SELECT id FROM sim WHERE num ~ "+sqlQuote(re))
	}
	for _, k := range []string{"quad", "triple", "asc", "repeat", "palin"} {
		qs = append(qs, fmt.Sprintf("SELECT id FROM sim WHERE sim_pretty(num, '%s')", k))
	}
	qs = append(qs, "SELECT id FROM sim WHERE num LIKE '%8%' AND num LIKE '%9%' AND carrier = 'c1' ORDER BY id",
		"SELECT id FROM sim WHERE num LIKE '%8%' ORDER BY id LIMIT 20",
		"SELECT count(*) FROM sim WHERE num ILIKE '%AB%' OR price = 3")
	for _, q := range qs {
		w, g := run(t, scan, q), run(t, idx, q)
		if strings.Contains(q, "LIKE '%888%'") && !strings.HasPrefix(lastPlan(idx), "Trigram Scan") {
			t.Fatalf("%s: view not used: %s", when, lastPlan(idx))
		}
		a, b := col0(w), col0(g)
		if len(w.Rows) > 0 && len(w.Rows[0]) > 0 && strings.Contains(q, "count(*)") {
			a, b = []string{fmt.Sprint(w.Rows)}, []string{fmt.Sprint(g.Rows)}
		}
		if strings.Contains(q, "LIMIT") {
			a, b = []string{fmt.Sprint(w.Rows)}, []string{fmt.Sprint(g.Rows)}
		}
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Fatalf("%s: %s: scan %d rows, view %d rows", when, q, len(a), len(b))
		}
		n++
	}
}

func waitTrg(t *testing.T, p *planner.Platform, e *core.Engine, want string) {
	t.Helper()
	tab, _ := e.UserTable("sim")
	for i := 0; i < 600; i++ {
		if p.Sub.StorageOf(tab.Schema().ID, "num") == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("trigram storage never became %s (now %q)", want, p.Sub.StorageOf(tab.Schema().ID, "num"))
}

func trgFixture(t *testing.T, dir string, n int) (*core.Engine, *planner.Platform, *Session, *Session, *rand.Rand, []string) {
	e := openDiskEngine(t, dir, true)
	p := planner.Start(e)
	scan, idx := NewSession(e), NewSession(e)
	idx.Views = p
	run(t, scan, `CREATE TABLE sim (id BIGINT PRIMARY KEY, num TEXT, carrier TEXT, price BIGINT)`)
	rng := rand.New(rand.NewSource(21))
	var all []string
	var vals []string
	for i := 0; i < n; i++ {
		num := "NULL"
		if rng.Intn(15) != 0 {
			s := randText(rng)
			all = append(all, s)
			num = sqlQuote(s)
		}
		vals = append(vals, fmt.Sprintf("(%d, %s, 'c%d', %d)", i, num, rng.Intn(4), rng.Intn(100)))
	}
	run(t, scan, "INSERT INTO sim VALUES "+strings.Join(vals, ","))
	run(t, idx, `CREATE INDEX sim_num ON sim USING gin (num gin_trgm_ops)`)
	return e, p, scan, idx, rng, all
}

func trgPatterns(rng *rand.Rand, all []string) []string {
	pats := []string{"%888%", "888%", "%888", "%8_8%", "%\\%%", "a%", "%é%", "%", "", "x8", "%x8", "___", "%a_b%", "%AB%", "0%8%", "%'%", "%1 %"}
	for i := 0; i < 60; i++ {
		r := []rune(all[rng.Intn(len(all))])
		if len(r) > 2 {
			a := rng.Intn(len(r))
			b := a + 1 + rng.Intn(len(r)-a)
			sub := strings.NewReplacer("%", "\\%", "_", "\\_", "\\", "\\\\").Replace(string(r[a:b]))
			pats = append(pats, "%"+sub+"%", sub+"%")
		}
	}
	return pats
}

// The trigram view answers identically from RAM and from a disk store, also
// while the store is built and while writes (inserts, updates, deletes) run;
// the move back to memory too; and the disk store is really read.
func TestSubstrDiskMatchesScan(t *testing.T) {
	dir := t.TempDir()
	e, p, scan, idx, rng, all := trgFixture(t, dir, 3000)
	defer e.Close()
	defer p.Sub.CloseDisk()
	pats := trgPatterns(rng, all)
	trgCompare(t, scan, idx, pats, "memory")

	// writes run while the posting lists move to disk
	var stop atomic.Bool
	var wg sync.WaitGroup
	var next atomic.Int64
	next.Store(100000)
	wsess := NewSession(e)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := rand.New(rand.NewSource(4))
		for !stop.Load() {
			switch r.Intn(3) {
			case 0:
				id := next.Add(1)
				tryRun(wsess, fmt.Sprintf("INSERT INTO sim VALUES (%d, %s, 'c1', %d)", id, sqlQuote(randText(r)), r.Intn(100)))
			case 1:
				tryRun(wsess, fmt.Sprintf("UPDATE sim SET num = %s WHERE id = %d", sqlQuote(randText(r)), r.Intn(3000)))
			default:
				tryRun(wsess, fmt.Sprintf("DELETE FROM sim WHERE id = %d", r.Intn(3000)))
			}
		}
	}()
	run(t, scan, `ALTER INDEX sim_num SET (index_storage = 'disk')`)
	waitTrg(t, p, e, "disk")
	stop.Store(true)
	wg.Wait()
	trgCompare(t, scan, idx, pats, "disk")
	if got, _ := e.IndexStorage("sim", "sim_num"); got != "disk" {
		t.Fatalf("catalog storage %q", got)
	}
	var st proj.SubstrStat
	for _, x := range p.Sub.Report() {
		st = x
	}
	if st.Storage != "disk" || st.DiskBytes == 0 || st.Postings == 0 {
		t.Fatalf("report: %+v", st)
	}
	// more writes on the disk store
	run(t, scan, "UPDATE sim SET num = num || '8x8' WHERE id % 5 = 0")
	run(t, scan, "DELETE FROM sim WHERE id % 17 = 0")
	run(t, scan, "INSERT INTO sim VALUES (7000001, '12345888', 'c1', 5), (7000002, '9999', 'c1', 5)")
	trgCompare(t, scan, idx, pats, "disk+writes")

	run(t, scan, `ALTER INDEX sim_num SET (index_storage = 'memory')`)
	waitTrg(t, p, e, "memory")
	trgCompare(t, scan, idx, pats, "back to memory")
	run(t, scan, `ALTER TABLE sim SET (index_storage = 'disk')`) // every index of the table, trigram included
	waitTrg(t, p, e, "disk")
	trgCompare(t, scan, idx, pats, "disk again")
}

// A restart builds the view straight from the catalog's storage; compaction
// (renumbering the documents) works on a disk column and a conversion that
// races it starts over.
func TestSubstrDiskRestartAndCompaction(t *testing.T) {
	old := proj.CompactMinDead
	proj.CompactMinDead = 50
	defer func() { proj.CompactMinDead = old }()
	dir := t.TempDir()
	e, p, scan, idx, rng, all := trgFixture(t, dir, 1500)
	pats := trgPatterns(rng, all)
	run(t, scan, `ALTER INDEX sim_num SET (index_storage = 'disk')`)
	waitTrg(t, p, e, "disk")
	// enough updates to compact more than once
	for r := 0; r < 4; r++ {
		run(t, scan, fmt.Sprintf("UPDATE sim SET num = num || 'z%d' WHERE id %% 2 = 0", r))
	}
	trgCompare(t, scan, idx, pats, "disk after compaction")
	p.Sub.CloseDisk()
	e.Close()

	e2 := openDiskEngine(t, dir, true)
	p2 := planner.Start(e2)
	scan2, idx2 := NewSession(e2), NewSession(e2)
	idx2.Views = p2
	waitTrg(t, p2, e2, "disk") // built on disk from the catalog
	trgCompare(t, scan2, idx2, pats, "restart")

	// without the option the catalog's 'disk' is served from memory
	p2.Sub.CloseDisk()
	e2.Close()
	e3 := openDiskEngine(t, dir, false)
	defer e3.Close()
	p3 := planner.Start(e3)
	scan3, idx3 := NewSession(e3), NewSession(e3)
	idx3.Views = p3
	waitTrg(t, p3, e3, "memory")
	trgCompare(t, scan3, idx3, pats, "option off")
}

// DROP INDEX while a conversion is between its steps: the conversion stops
// without error, the file is gone, and an index of the same name works.
func TestSubstrDiskDropRacesConversion(t *testing.T) {
	for _, step := range []string{"built", "caught-up"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			e, p, scan, idx, rng, all := trgFixture(t, dir, 1500)
			defer e.Close()
			defer p.Sub.CloseDisk()
			reached, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			p.Sub.SetHook(func(s string) {
				if s == step {
					once.Do(func() { reached <- struct{}{}; <-release })
				}
			})
			run(t, scan, `ALTER INDEX sim_num SET (index_storage = 'disk')`)
			select {
			case <-reached:
			case <-time.After(20 * time.Second):
				t.Fatal("conversion never reached the step")
			}
			run(t, idx, `DROP INDEX sim_num`)
			time.Sleep(50 * time.Millisecond)
			close(release)
			p.Sub.WaitConversions()
			tab, _ := e.UserTable("sim")
			if s := p.Sub.StorageOf(tab.Schema().ID, "num"); s != "" {
				t.Fatalf("dropped index still reports %q", s)
			}
			run(t, idx, `CREATE INDEX sim_num ON sim USING gin (num gin_trgm_ops)`)
			run(t, scan, `ALTER INDEX sim_num SET (index_storage = 'disk')`)
			waitTrg(t, p, e, "disk")
			trgCompare(t, scan, idx, trgPatterns(rng, all), "recreated")
		})
	}
}
