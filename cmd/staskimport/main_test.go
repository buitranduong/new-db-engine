package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xuri/excelize/v2"

	"stask/internal/core"
	"stask/internal/pgwire"
)

const specJSON = `{
 "table": "sims", "key": "number",
 "columns": [
  {"name": "number", "source": "SIM", "pattern": "^0[0-9]{9}$"},
  {"name": "carrier", "source": "Carrier", "enum": ["viettel", "mobifone", "vinaphone"]},
  {"name": "status", "source": "Status", "enum": ["new", "paid", "reserved", "sold"], "keep_old_when": ["reserved", "sold"]},
  {"name": "price", "source": "Price", "type": "int", "min": 0}
 ],
 "touch": {"updated_at": "now"},
 "history": {"table": "sim_history", "key_column": "number", "at_column": "changed_at", "extra": {"source": "import"}}
}`

type rig struct {
	t    *testing.T
	e    *core.Engine
	url  string
	dir  string
	spec string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	e, err := core.Open(core.Options{Dir: t.TempDir(), SegmentSize: 8 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &pgwire.Server{E: e, Logf: func(string, ...any) {}}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close(); e.Close() })
	r := &rig{t: t, e: e, url: "postgres://app@" + ln.Addr().String() + "/stask?sslmode=disable", dir: t.TempDir()}
	for _, q := range []string{
		"CREATE TABLE sims (number text PRIMARY KEY, carrier text NOT NULL, status text, price bigint, updated_at timestamptz)",
		"CREATE TABLE sim_history (id bigserial PRIMARY KEY, number text, changed_at timestamptz, source text, old_carrier text, new_carrier text, old_status text, new_status text, old_price bigint, new_price bigint)",
	} {
		r.exec(q)
	}
	r.spec = filepath.Join(r.dir, "spec.json")
	os.WriteFile(r.spec, []byte(specJSON), 0o644)
	return r
}

func (r *rig) conn() *pgx.Conn {
	c, err := pgx.Connect(context.Background(), r.url)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func (r *rig) exec(q string, args ...any) {
	r.t.Helper()
	c := r.conn()
	if _, err := c.Exec(context.Background(), q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
}

func (r *rig) count(q string, args ...any) int64 {
	r.t.Helper()
	var n int64
	if err := r.conn().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	return n
}

type simRow struct {
	num             string
	carrier, status string
	price           int64
}

var carriers = []string{"viettel", "mobifone", "vinaphone"}

func gen(n int, f func(i int, r *simRow)) []simRow {
	rows := make([]simRow, n)
	for i := range rows {
		rows[i] = simRow{num: fmt.Sprintf("09%08d", i), carrier: carriers[i%3], status: "new", price: int64(100 + i%900)}
		if f != nil {
			f(i, &rows[i])
		}
	}
	return rows
}

var header = []string{"SIM", "Carrier", "Status", "Price"}

func writeCSV(t *testing.T, path string, rows []simRow, extra ...[]string) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(strings.Join(header, ",") + "\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s,%s,%s,%d\n", r.num, r.carrier, r.status, r.price)
	}
	for _, x := range extra {
		b.WriteString(strings.Join(x, ",") + "\n")
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeXLSX(t *testing.T, path string, rows []simRow) {
	t.Helper()
	f := excelize.NewFile()
	sw, err := f.NewStreamWriter("Sheet1")
	if err != nil {
		t.Fatal(err)
	}
	hs := make([]any, len(header))
	for i, h := range header {
		hs[i] = h
	}
	sw.SetRow("A1", hs)
	for i, r := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		sw.SetRow(cell, []any{r.num, r.carrier, r.status, r.price})
	}
	if err := sw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.SaveAs(path); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) run(file string, mod func(*options)) *Summary {
	r.t.Helper()
	o := options{url: r.url, specPath: r.spec, batch: 2000}
	if mod != nil {
		mod(&o)
	}
	var out, errb bytes.Buffer
	s, err := run(context.Background(), o, file, &out, &errb)
	if err != nil {
		r.t.Fatalf("run: %v\n%s%s", err, out.String(), errb.String())
	}
	return s
}

func TestImportCSVIdempotentAndDiff(t *testing.T) {
	r := newRig(t)
	f1 := filepath.Join(r.dir, "day1.csv")
	writeCSV(t, f1, gen(50000, nil))

	dry := r.run(f1, func(o *options) { o.dry = true })
	if dry.Counts.Inserted != 50000 || dry.Counts.Updated != 0 || r.count("SELECT count(*) FROM sims") != 0 {
		t.Fatalf("dry run: %+v", dry.Counts)
	}
	s := r.run(f1, nil)
	if s.Counts.Inserted != 50000 || s.Counts.Unchanged != 0 || !s.Complete {
		t.Fatalf("day1: %+v", s.Counts)
	}
	if n := r.count("SELECT count(*) FROM sims"); n != 50000 {
		t.Fatalf("rows %d", n)
	}
	if n := r.count("SELECT count(*) FROM sim_history WHERE old_price IS NULL AND source = 'import'"); n != 50000 {
		t.Fatalf("history of inserts: %d", n)
	}

	// Same file again: nothing is written (the log does not move).
	lsn := r.e.DurableLSN()
	s = r.run(f1, func(o *options) { o.restart = true })
	if s.Counts.Unchanged != 50000 || s.Counts.Inserted+s.Counts.Updated != 0 || s.Counts.History != 0 {
		t.Fatalf("rerun: %+v", s.Counts)
	}
	if r.e.DurableLSN() != lsn {
		t.Fatalf("unchanged rows wrote to the log: lsn %d -> %d", lsn, r.e.DurableLSN())
	}
	// A finished checkpoint: the plain rerun does nothing at all.
	if s2 := r.run(f1, nil); !s2.Complete {
		t.Fatalf("complete checkpoint: %+v", s2)
	}

	// Day 2: 5000 prices change, 500 rows are 'sold' in the table and the file says 'new' (kept),
	// 20 rows are invalid, 10 repeat a key.
	r.exec("UPDATE sims SET status = 'sold' WHERE number >= '0900040000' AND number < '0900040500'")
	rows := gen(50000, func(i int, x *simRow) {
		if i%10 == 0 {
			x.price += 7
		}
	})
	var bad [][]string
	for i := 0; i < 20; i++ {
		bad = append(bad, []string{fmt.Sprintf("12%08d", i), "viettel", "new", "5"}) // key pattern
	}
	for i := 0; i < 10; i++ {
		bad = append(bad, []string{fmt.Sprintf("09%08d", i), "viettel", "new", "999"}) // repeats, later batch: not a dup but an update
	}
	f2 := filepath.Join(r.dir, "day2.csv")
	writeCSV(t, f2, rows, bad...)
	lsn = r.e.DurableLSN()
	dry = r.run(f2, func(o *options) { o.dry = true })
	if r.e.DurableLSN() != lsn {
		t.Fatal("dry run wrote")
	}
	s = r.run(f2, nil)
	if s.Counts != withoutRetries(dry.Counts, s.Counts) && (s.Counts.Inserted != dry.Counts.Inserted || s.Counts.Updated != dry.Counts.Updated || s.Counts.Unchanged != dry.Counts.Unchanged) {
		t.Fatalf("dry %+v != real %+v", dry.Counts, s.Counts)
	}
	if s.Counts.Invalid != 20 || s.Counts.Inserted != 0 {
		t.Fatalf("day2: %+v", s.Counts)
	}
	// 5000 price changes (rows i%10==0) ; the 10 trailing duplicates of rows 0..9 change row 0..9 again: row 0 changed twice
	// (batch order: first +7 within main rows, then 999 in the tail), rows 1..9 change once.
	wantUpdated := int64(5000 + 9 + 1) // 5000 main (incl. row 0) + tail rows 0..9 (row 0 counts again)
	if s.Counts.Updated != wantUpdated {
		t.Fatalf("updated %d, want %d (%+v)", s.Counts.Updated, wantUpdated, s.Counts)
	}
	if n := r.count("SELECT count(*) FROM sim_history WHERE old_price IS NOT NULL"); n != wantUpdated {
		t.Fatalf("history rows for changes: %d, want %d", n, wantUpdated)
	}
	var op, np int64
	if err := r.conn().QueryRow(context.Background(), "SELECT old_price, new_price FROM sim_history WHERE number = '0900000020' AND old_price IS NOT NULL").Scan(&op, &np); err != nil || np != op+7 {
		t.Fatalf("history old->new: %d -> %d (%v)", op, np, err)
	}
	if n := r.count("SELECT count(*) FROM sims WHERE status = 'sold'"); n != 500 {
		t.Fatalf("sold rows were overwritten: %d", n)
	}
	eb, _ := os.ReadFile(f2 + ".errors.csv")
	if strings.Count(string(eb), "\n") != 21 || !strings.Contains(string(eb), "does not match") {
		t.Fatalf("error report:\n%s", eb)
	}
}

func withoutRetries(a, _ Counts) Counts { a.Retries = 0; return a }

func TestImportXLSX(t *testing.T) {
	r := newRig(t)
	f := filepath.Join(r.dir, "day1.xlsx")
	writeXLSX(t, f, gen(50000, nil))
	s := r.run(f, nil)
	if s.Counts.Inserted != 50000 || s.Counts.Invalid != 0 {
		t.Fatalf("xlsx: %+v", s.Counts)
	}
	if n := r.count("SELECT sum(price) FROM sims"); n == 0 {
		t.Fatal("no prices")
	}
	f2 := filepath.Join(r.dir, "day2.xlsx")
	writeXLSX(t, f2, gen(50000, func(i int, x *simRow) {
		if i%100 == 0 {
			x.status = "paid"
		}
	}))
	s = r.run(f2, nil)
	if s.Counts.Updated != 500 || s.Counts.Unchanged != 49500 {
		t.Fatalf("xlsx day2: %+v", s.Counts)
	}
}

func TestBadRowDoesNotStopTheFile(t *testing.T) {
	r := newRig(t)
	// carrier is NOT NULL in the table but optional in this spec: the database refuses that row
	sp := strings.Replace(specJSON, `"enum": ["viettel", "mobifone", "vinaphone"]}`, `"enum": ["viettel", "mobifone", "vinaphone"]}`, 1)
	os.WriteFile(r.spec, []byte(sp), 0o644)
	rows := gen(3000, nil)
	rows[1234].carrier = ""
	f := filepath.Join(r.dir, "bad.csv")
	writeCSV(t, f, rows)
	s := r.run(f, nil)
	if s.Counts.Failed != 1 || s.Counts.Inserted != 2999 || !s.Complete {
		t.Fatalf("counts %+v", s.Counts)
	}
	if n := r.count("SELECT count(*) FROM sims"); n != 2999 {
		t.Fatalf("rows %d", n)
	}
	eb, _ := os.ReadFile(f + ".errors.csv")
	if !strings.Contains(string(eb), "0900001234") {
		t.Fatalf("error report lacks the key:\n%s", eb)
	}
}

func buildBinary(t *testing.T) string {
	bin := filepath.Join(t.TempDir(), "staskimport")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func TestResumeAfterKill(t *testing.T) {
	bin := buildBinary(t)
	for _, beforeCkpt := range []bool{false, true} {
		t.Run(fmt.Sprintf("kill_before_checkpoint=%v", beforeCkpt), func(t *testing.T) {
			r := newRig(t)
			f := filepath.Join(r.dir, "day.csv")
			writeCSV(t, f, gen(50000, nil))
			args := []string{"-url", r.url, "-spec", r.spec, "-batch", "2000", "-progress", "0"}
			cmd := exec.Command(bin, append(append([]string{}, args...), "-test-crash-after", "7", fmt.Sprintf("-test-crash-before-ckpt=%v", beforeCkpt), f)...)
			err := cmd.Run()
			if err == nil {
				t.Fatal("the importer was not killed")
			}
			ck, _ := loadCheckpoint(f + ".import.json")
			wantDone := int64(7 * 2000)
			if beforeCkpt {
				wantDone = 6 * 2000
			}
			if ck == nil || ck.RowsDone != wantDone || ck.Complete {
				t.Fatalf("checkpoint after kill: %+v", ck)
			}
			committed := r.count("SELECT count(*) FROM sims")
			if committed != int64(7*2000) {
				t.Fatalf("rows committed at the kill: %d", committed)
			}
			out, err := exec.Command(bin, append(append([]string{}, args...), "-json", f)...).Output()
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			var s Summary
			if err := json.Unmarshal(out, &s); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if !s.Complete || s.Resumed != wantDone {
				t.Fatalf("resume summary %+v", s)
			}
			if n := r.count("SELECT count(*) FROM sims"); n != 50000 {
				t.Fatalf("rows after resume: %d", n)
			}
			// no row twice in the history, whichever window the kill fell in
			if n := r.count("SELECT count(*) FROM sim_history"); n != 50000 {
				t.Fatalf("history rows %d, want 50000", n)
			}
			if n := r.count("SELECT count(DISTINCT number) FROM sim_history"); n != 50000 {
				t.Fatalf("distinct history keys %d", n)
			}
			if beforeCkpt && s.Counts.Unchanged < 2000 {
				// the batch applied but not recorded is seen again as unchanged
				t.Fatalf("counts %+v", s.Counts)
			}
		})
	}
}

// Readers that run during an import see each batch whole or not at all.
func TestConcurrentReadersSeeBatchSnapshots(t *testing.T) {
	r := newRig(t)
	const n, batch = 20000, 500
	f1 := filepath.Join(r.dir, "v1.csv")
	writeCSV(t, f1, gen(n, func(i int, x *simRow) { x.price = 1 }))
	r.run(f1, func(o *options) { o.batch = batch })
	f2 := filepath.Join(r.dir, "v2.csv")
	writeCSV(t, f2, gen(n, func(i int, x *simRow) { x.price = 2 }))

	var stop atomic.Bool
	var wg sync.WaitGroup
	var reads, torn, errs atomic.Int64
	var other atomic.Int64 // slowest write of an unrelated row, microseconds
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			c := r.conn()
			for k := g; !stop.Load(); k += 7 {
				b := k % (n / batch)
				lo, hi := fmt.Sprintf("09%08d", b*batch), fmt.Sprintf("09%08d", (b+1)*batch-1)
				var mn, mx int64
				if err := c.QueryRow(context.Background(), "SELECT min(price), max(price) FROM sims WHERE number >= $1 AND number <= $2", lo, hi).Scan(&mn, &mx); err != nil {
					errs.Add(1)
					continue
				}
				reads.Add(1)
				if mn != mx {
					torn.Add(1)
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() { // an application write to a key the import also touches: must not be blocked for long
		defer wg.Done()
		c := r.conn()
		for i := 0; !stop.Load(); i++ {
			t0 := time.Now()
			c.Exec(context.Background(), "INSERT INTO sims (number, carrier, status, price) VALUES ($1,'viettel','new',5) ON CONFLICT (number) DO UPDATE SET status = excluded.status", fmt.Sprintf("08%08d", i%50))
			if d := time.Since(t0).Microseconds(); d > other.Load() {
				other.Store(d)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	s := r.run(f2, func(o *options) { o.batch = batch; o.pause = 2 * time.Millisecond })
	stop.Store(true)
	wg.Wait()
	if s.Counts.Updated != n {
		t.Fatalf("counts %+v", s.Counts)
	}
	if reads.Load() < 50 {
		t.Fatalf("only %d reads ran during the import", reads.Load())
	}
	if torn.Load() != 0 || errs.Load() != 0 {
		t.Fatalf("%d of %d reads saw a half-applied batch, %d errors", torn.Load(), reads.Load(), errs.Load())
	}
	t.Logf("%d snapshot reads during the import, none torn; slowest unrelated write %d us", reads.Load(), other.Load())
}

func TestSpecErrors(t *testing.T) {
	r := newRig(t)
	f := filepath.Join(r.dir, "x.csv")
	os.WriteFile(f, []byte("SIM,Carrier\n0900000001,viettel\n"), 0o644)
	_, err := run(context.Background(), options{url: r.url, specPath: r.spec, batch: 10}, f, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "missing in the file header") {
		t.Fatalf("err %v", err)
	}
	for _, bad := range []string{`{"table":"t","key":"zz","columns":[{"name":"a"}]}`, `{"table":"t; drop","key":"a","columns":[{"name":"a"}]}`, `{"table":"t","key":"a","columns":[{"name":"a","type":"blob"}]}`} {
		p := filepath.Join(r.dir, "bad.json")
		os.WriteFile(p, []byte(bad), 0o644)
		if _, _, err := LoadSpec(p); err == nil {
			t.Errorf("spec accepted: %s", bad)
		}
	}
}
