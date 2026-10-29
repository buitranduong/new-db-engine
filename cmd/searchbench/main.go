// searchbench: pattern-search timings on SIM-like numbers, scan vs trigram
// view (in-process, no wire), plus how fast each view applies a bulk load.
//
//	searchbench -dir /tmp/sb -n 1000000 [-pg 'host=127.0.0.1 ...' to print the SQL]
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/sql"
)

var prefixes = []struct{ p, carrier string }{
	{"090", "mobifone"}, {"093", "mobifone"}, {"089", "mobifone"}, {"070", "mobifone"}, {"079", "mobifone"}, {"077", "mobifone"},
	{"091", "vinaphone"}, {"094", "vinaphone"}, {"088", "vinaphone"}, {"083", "vinaphone"}, {"084", "vinaphone"}, {"085", "vinaphone"},
	{"096", "viettel"}, {"097", "viettel"}, {"098", "viettel"}, {"086", "viettel"}, {"032", "viettel"}, {"033", "viettel"}, {"034", "viettel"}, {"035", "viettel"},
	{"092", "vietnamobile"}, {"056", "vietnamobile"}, {"058", "vietnamobile"}, {"099", "gmobile"}, {"059", "gmobile"},
}

func gen(rng *rand.Rand, seen map[string]struct{}) (num, carrier string) {
	for {
		pf := prefixes[rng.Intn(len(prefixes))]
		var b strings.Builder
		b.WriteString(pf.p)
		if rng.Intn(50) == 0 { // ~2% pretty tails
			for i := 0; i < 3; i++ {
				b.WriteByte(byte('0' + rng.Intn(10)))
			}
			d := byte('0' + rng.Intn(10))
			switch rng.Intn(3) {
			case 0:
				b.Write([]byte{d, d, d, d})
			case 1:
				x := byte('0' + rng.Intn(6))
				b.Write([]byte{x, x + 1, x + 2, x + 3})
			default:
				e := byte('0' + rng.Intn(10))
				b.Write([]byte{d, e, d, e})
			}
		} else {
			for i := 0; i < 7; i++ {
				b.WriteByte(byte('0' + rng.Intn(10)))
			}
		}
		n := b.String()
		if _, dup := seen[n]; !dup {
			seen[n] = struct{}{}
			return n, pf.carrier
		}
	}
}

func must(s *sql.Session, q string) *sql.Result {
	st, err := sql.Parse(q)
	if err != nil {
		panic(fmt.Sprint(q, err))
	}
	var res *sql.Result
	for _, x := range st {
		if res, err = s.Exec(x, nil); err != nil {
			panic(fmt.Sprint(q, ": ", err))
		}
	}
	return res
}

func med(f func() *sql.Result, reps int) (time.Duration, int, string) {
	var ds []time.Duration
	var rows int
	var first string
	for i := 0; i < reps; i++ {
		t0 := time.Now()
		r := f()
		ds = append(ds, time.Since(t0))
		rows = len(r.Rows)
		if len(r.Rows) > 0 && i == 0 {
			first = fmt.Sprint(r.Rows[0][len(r.Rows[0])-1].String())
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2], rows, first
}

func waitAll(p *planner.Platform, e *core.Engine, which string) time.Duration {
	t0 := time.Now()
	d := e.DurableLSN()
	for {
		ok := true
		if strings.Contains(which, "s") && p.SubF.Applied() < d {
			ok = false
		}
		if strings.Contains(which, "c") && p.ColF.Applied() < d {
			ok = false
		}
		if strings.Contains(which, "b") && p.SF.Applied() < d {
			ok = false
		}
		if ok {
			return time.Since(t0)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func main() {
	dir := flag.String("dir", "/tmp/sb", "data dir (removed at the end)")
	n := flag.Int("n", 1000000, "rows")
	mode := flag.String("mode", "query", "query | apply")
	pgout := flag.String("csv", "", "write the numbers as a CSV file for loading into PostgreSQL")
	flag.Parse()
	os.RemoveAll(*dir)
	defer os.RemoveAll(*dir)
	e, err := core.Open(core.Options{Dir: *dir, SegmentSize: 64 << 20})
	if err != nil {
		panic(err)
	}
	defer e.Close()
	p := planner.Start(e)
	w := sql.NewSession(e)
	rng := rand.New(rand.NewSource(42))
	seen := map[string]struct{}{}

	if *mode == "catchup" {
		// stop the trigram view, load, release: its own apply rate, apart from the writes
		must(w, "CREATE TABLE cu (num TEXT PRIMARY KEY, carrier TEXT, status TEXT, price BIGINT) WITH (trigram='num')")
		waitAll(p, e, "s")
		release := p.Sub.Hold()
		rg := rand.New(rand.NewSource(1))
		sn := map[string]struct{}{}
		t0 := time.Now()
		for done := 0; done < *n; {
			var vals []string
			for i := 0; i < 1000 && done < *n; i++ {
				num, c := gen(rg, sn)
				vals = append(vals, fmt.Sprintf("('%s','%s','free',%d)", num, c, rg.Intn(500)*10000))
				done++
			}
			must(w, "INSERT INTO cu VALUES "+strings.Join(vals, ","))
		}
		load := time.Since(t0)
		fmt.Printf("core loaded %d rows in %.1fs (%.0f rows/s); view applied lsn %d of %d\n", *n, load.Seconds(), float64(*n)/load.Seconds(), p.SubF.Applied(), e.DurableLSN())
		t1 := time.Now()
		release()
		waitAll(p, e, "s")
		d := time.Since(t1)
		fmt.Printf("trigram view caught up in %.2fs after release => %.0f rows/s apply rate\n", d.Seconds(), float64(*n)/d.Seconds())
		return
	}
	if *mode == "apply" {
		// bulk load into three tables with one view each: how fast does each view catch up?
		for _, v := range []struct{ name, opt, wait string }{
			{"none", "", ""}, {"trigram", "trigram='num'", "s"}, {"columnar", "columnar=true", "c"}, {"bm25", "search='num'", "b"},
		} {
			opt := ""
			if v.opt != "" {
				opt = " WITH (" + v.opt + ")"
			}
			must(w, "CREATE TABLE ap_"+v.name+" (num TEXT PRIMARY KEY, carrier TEXT, status TEXT, price BIGINT)"+opt)
			waitAll(p, e, "scb")
			rg := rand.New(rand.NewSource(1))
			sn := map[string]struct{}{}
			t0 := time.Now()
			applied0 := p.SubF.Applied()
			_ = applied0
			for done := 0; done < *n; {
				var vals []string
				for i := 0; i < 1000 && done < *n; i++ {
					num, c := gen(rg, sn)
					vals = append(vals, fmt.Sprintf("('%s','%s','free',%d)", num, c, rg.Intn(500)*10000))
					done++
				}
				must(w, "INSERT INTO ap_"+v.name+" VALUES "+strings.Join(vals, ","))
			}
			load := time.Since(t0)
			lag := waitAll(p, e, "scb")
			var ms runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&ms)
			fmt.Printf("  heap in use after this table: %d MB\n", ms.HeapInuse>>20)
			fmt.Printf("view=%-9s rows=%d core_load=%.1fs (%.0f rows/s)  view_caught_up_after_load_end=%.2fs  => view applies >= %.0f rows/s overall\n",
				v.name, *n, load.Seconds(), float64(*n)/load.Seconds(), lag.Seconds(), float64(*n)/(load+lag).Seconds())
		}
		return
	}

	must(w, "CREATE TABLE sim (num TEXT PRIMARY KEY, carrier TEXT, status TEXT, price BIGINT)")
	var csv *os.File
	if *pgout != "" {
		csv, _ = os.Create(*pgout)
		defer csv.Close()
	}
	t0 := time.Now()
	for done := 0; done < *n; {
		var vals []string
		for i := 0; i < 1000 && done < *n; i++ {
			num, c := gen(rng, seen)
			st := "free"
			if rng.Intn(4) == 0 {
				st = "sold"
			}
			pr := rng.Intn(500) * 10000
			vals = append(vals, fmt.Sprintf("('%s','%s','%s',%d)", num, c, st, pr))
			if csv != nil {
				fmt.Fprintf(csv, "%s,%s,%s,%d\n", num, c, st, pr)
			}
			done++
		}
		must(w, "INSERT INTO sim VALUES "+strings.Join(vals, ","))
	}
	fmt.Printf("loaded %d rows in %.1fs\n", *n, time.Since(t0).Seconds())
	seen = nil
	scan, idx := sql.NewSession(e), sql.NewSession(e)
	idx.Views = p
	t1 := time.Now()
	must(w, "CREATE INDEX sim_num ON sim USING gin (num gin_trgm_ops)")
	waitAll(p, e, "s")
	fmt.Printf("CREATE INDEX (backfill of %d existing rows) done in %.2fs\n", *n, time.Since(t1).Seconds())
	must(idx, "SET stask.consistency = strong")
	live, total := p.Sub.Stats(1)
	_ = live
	_ = total

	qs := []struct{ name, q string }{
		{"prefix 7 digits LIMIT 50", "SELECT num, carrier, price FROM sim WHERE num LIKE '0901234%' LIMIT 50"},
		{"prefix 3 digits LIMIT 50", "SELECT num, carrier, price FROM sim WHERE num LIKE '090%' LIMIT 50"},
		{"prefix 4 digits count", "SELECT count(*) FROM sim WHERE num LIKE '0901%'"},
		{"suffix 8888 LIMIT 50", "SELECT num, carrier, price FROM sim WHERE num LIKE '%8888' LIMIT 50"},
		{"suffix 8888 count", "SELECT count(*) FROM sim WHERE num LIKE '%8888'"},
		{"suffix 88 count (2 chars, shorter than a trigram)", "SELECT count(*) FROM sim WHERE num LIKE '%88'"},
		{"contains 888 count", "SELECT count(*) FROM sim WHERE num LIKE '%888%'"},
		{"contains 888 LIMIT 50", "SELECT num FROM sim WHERE num LIKE '%888%' LIMIT 50"},
		{"contains 123456 count", "SELECT count(*) FROM sim WHERE num LIKE '%123456%'"},
		{"regex ^09[0-3].*8888$ count", "SELECT count(*) FROM sim WHERE num ~ '^09[0-3].*8888$'"},
		{"tu quy (quad) LIMIT 50", "SELECT num, carrier FROM sim WHERE sim_pretty(num,'quad') LIMIT 50"},
		{"tu quy (quad) count", "SELECT count(*) FROM sim WHERE sim_pretty(num,'quad')"},
		{"tien (asc) count", "SELECT count(*) FROM sim WHERE sim_pretty(num,'asc')"},
		{"lap (repeat) count", "SELECT count(*) FROM sim WHERE sim_pretty(num,'repeat')"},
		{"filters: suffix 8888 + carrier + status + price", "SELECT num, price FROM sim WHERE num LIKE '%8888' AND carrier='viettel' AND status='free' AND price BETWEEN 1000000 AND 3000000 LIMIT 50"},
		{"facet: carrier counts, contains 888", "SELECT carrier, count(*) FROM sim WHERE num LIKE '%888%' GROUP BY carrier ORDER BY carrier"},
		{"facet: carrier counts, suffix 8888 + status", "SELECT carrier, count(*) FROM sim WHERE num LIKE '%8888' AND status='free' GROUP BY carrier ORDER BY carrier"},
		{"facet: carrier counts, quad", "SELECT carrier, count(*) FROM sim WHERE sim_pretty(num,'quad') GROUP BY carrier ORDER BY carrier"},
	}
	fmt.Printf("%-52s %12s %12s %8s  %s\n", "query", "scan (ms)", "view (ms)", "speedup", "rows / plan")
	for _, c := range qs {
		qq := c.q
		reps := 5
		ds, rows, _ := med(func() *sql.Result { return must(scan, qq) }, 3)
		dv, rows2, _ := med(func() *sql.Result { return must(idx, qq) }, reps)
		plan := "scan"
		if strings.HasPrefix(idx.PlanNote(), "Trigram") {
			plan = "trigram"
		}
		chk := ""
		if !strings.Contains(c.q, "LIMIT") && rows != rows2 {
			chk = " MISMATCH"
		}
		// compare values of count(*) and facets
		a, b := must(scan, qq), must(idx, qq)
		if !strings.Contains(c.q, "LIMIT") && fmt.Sprint(a.Rows) != fmt.Sprint(b.Rows) {
			chk = " MISMATCH"
		}
		val := ""
		if len(a.Rows) == 1 && len(a.Rows[0]) == 1 {
			val = a.Rows[0][0].String()
		}
		fmt.Printf("%-52s %12.2f %12.2f %7.1fx  rows=%d %s %s%s\n", c.name, ms(ds), ms(dv), ms(ds)/ms(dv), rows2, plan, val, chk)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
