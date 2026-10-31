// memmap: how much live heap each structure of the engine costs, per key.
//
//	scripts/perf-run.sh bin-main/memmap -n 1000000,5000000,20000000 -dist prefix -updates 3
//
// For each N it loads N SIMs into an in-process engine (no wire protocol)
// and adds one structure at a time, reading the live heap after each step:
//
//	baseline        engine + views, no table
//	primary         table sims(number PK, ...) with N rows        -> primary index + log mapping
//	history         -updates K more versions of every row         -> what old versions cost in RAM
//	secondary       CREATE INDEX (carrier,status,price)           -> btree secondary index
//	trigram         CREATE INDEX .. gin_trgm_ops on number        -> trigram view
//	columnar        table sims_col (same rows, columnar=true)     -> columnar view (+ its own PK)
//
// and a pprof inuse_space attribution of the final state (attrib.go).
// docs/08 section 6 and docs/19 describe how to read the output. Every N
// runs in a child process so the Go heap starts clean; the parent only
// prints. It must be run through scripts/perf-run.sh to count as evidence
// (docs/AGENT_PROTOCOL section 6).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/planner"
	"stask/internal/sql"
)

type config struct {
	N       int64
	Dir     string
	Dist    string
	Updates int
	Batch   int
	Rate    int
	Skip    string // stages to skip: comma list of history,secondary,trigram,columnar
	Disk    bool   // after the other stages, move the secondary and trigram indexes to disk (stages spill-secondary, spill-trigram)
}

// StageResult is one step of the incremental measurement.
type StageResult struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
	Mem     Mem     `json:"mem"`
	Delta   int64   `json:"delta_heap_bytes"` // heap objects vs the previous stage
	Note    string  `json:"note,omitempty"`
	Grew    []Site  `json:"top_growth_sites,omitempty"` // pprof: where the stage's heap went
}

// Report is the result for one N.
type Report struct {
	N        int64            `json:"n"`
	Dist     string           `json:"dist"`
	Updates  int              `json:"updates"`
	Batch    int              `json:"batch"`
	Stages   []StageResult    `json:"stages"`
	Pprof    map[string]int64 `json:"pprof_inuse"`
	PprofTot int64            `json:"pprof_total"`
	Unknown  []Site           `json:"pprof_unattributed_top,omitempty"`
	Direct   map[string]int64 `json:"direct"` // numbers the structures report about themselves
	LogDisk  int64            `json:"log_disk_bytes"`
	GC       any              `json:"gc_stats"`
}

func main() {
	var (
		ns      = flag.String("n", "1000000", "comma list of SIM counts, e.g. 1000000,5000000,20000000")
		dir     = flag.String("dir", "/tmp/memmap", "work directory (removed per N; needs ~100 B * N * (updates+2) of disk)")
		dist    = flag.String("dist", "simbench", "number distribution: simbench (cmd/simbench numStr) | prefix (weighted 090/091/098.. prefixes, cmd/searchbench; use it for trigram)")
		updates = flag.Int("updates", 3, "extra versions written per SIM before the other structures are added (history cost)")
		batch   = flag.Int("batch", 1000, "rows per INSERT statement")
		rate    = flag.Int("memprofile-rate", 16384, "runtime.MemProfileRate (bytes between samples)")
		skip    = flag.String("skip", "", "comma list of stages to skip: history,secondary,trigram,columnar")
		disk    = flag.Bool("disk", false, "also measure moving the secondary and trigram indexes to disk (ALTER INDEX .. SET (index_storage = 'disk')): stages spill-secondary and spill-trigram show the heap that goes away")
		jsonOut = flag.String("json", "", "also write the reports as JSON to this file")
		child   = flag.Bool("child", false, "internal: run one N and print its report as JSON on stdout")
	)
	flag.Parse()
	runtime.MemProfileRate = *rate
	if *child {
		n, _ := strconv.ParseInt(strings.TrimSpace(*ns), 10, 64)
		rep, err := run(config{N: n, Dir: *dir, Dist: *dist, Updates: *updates, Batch: *batch, Rate: *rate, Skip: *skip, Disk: *disk}, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memmap:", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(rep)
		return
	}
	var all []*Report
	for _, s := range strings.Split(*ns, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "bad -n %q\n", s)
			os.Exit(2)
		}
		self, _ := os.Executable()
		cmd := exec.Command(self, "-child", "-n", s, "-dir", *dir, "-dist", *dist, "-updates", strconv.Itoa(*updates),
			"-batch", strconv.Itoa(*batch), "-memprofile-rate", strconv.Itoa(*rate), "-skip", *skip, "-disk="+strconv.FormatBool(*disk))
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "memmap: N=%d failed: %v\n", n, err)
			os.Exit(1)
		}
		var rep Report
		if err := json.Unmarshal(out, &rep); err != nil {
			fmt.Fprintf(os.Stderr, "memmap: bad child output: %v\n%s\n", err, out)
			os.Exit(1)
		}
		printReport(os.Stdout, &rep)
		all = append(all, &rep)
	}
	if len(all) > 1 {
		printLinearity(os.Stdout, all)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(all, "", " ")
		os.WriteFile(*jsonOut, b, 0o644)
	}
}

func skipped(c config, stage string) bool {
	for _, s := range strings.Split(c.Skip, ",") {
		if strings.TrimSpace(s) == stage {
			return true
		}
	}
	return false
}

type runner struct {
	e *core.Engine
	p *planner.Platform
	s *sql.Session
}

func (r *runner) exec(q string) error {
	st, err := sql.Parse(q)
	if err != nil {
		return fmt.Errorf("%s: %w", clip(q), err)
	}
	for _, x := range st {
		if _, err = r.s.Exec(x, nil); err != nil {
			return fmt.Errorf("%s: %w", clip(q), err)
		}
	}
	return nil
}

func clip(q string) string {
	if len(q) > 120 {
		return q[:120] + "..."
	}
	return q
}

// waitViews blocks until every derived view has applied the whole log.
func (r *runner) waitViews() {
	d := r.e.DurableLSN()
	for r.p.SubF.Applied() < d || r.p.ColF.Applied() < d || r.p.SF.Applied() < d || r.p.TSF.Applied() < d {
		time.Sleep(5 * time.Millisecond)
	}
}

// load writes rows [from, to) of table into t in batches. gen(i) is the
// number; version v>0 changes the price so that every version differs.
func (r *runner) load(table string, c config, num func(int64) string, from, to int64, version int, upsert bool) error {
	var b strings.Builder
	for i := from; i < to; {
		b.Reset()
		fmt.Fprintf(&b, "INSERT INTO %s (number,carrier,status,price,updated_at) VALUES ", table)
		end := min(to, i+int64(c.Batch))
		for j := i; j < end; j++ {
			if j > i {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "('%s','%s','%s',%d,%d)", num(j), carrierOf(j), statusOf(j), priceOf(j)+int64(version)*10, 1700000000+int64(version))
		}
		if upsert {
			b.WriteString(" ON CONFLICT (number) DO UPDATE SET price=excluded.price, updated_at=excluded.updated_at")
		}
		if err := r.exec(b.String()); err != nil {
			return err
		}
		i = end
	}
	return nil
}

func run(c config, log io.Writer) (*Report, error) {
	num, err := numFn(c.Dist)
	if err != nil {
		return nil, err
	}
	// the work directory is deleted before and after: never a directory that is not clearly ours
	if b := filepath.Base(filepath.Clean(c.Dir)); !strings.Contains(b, "memmap") && b != "mm" {
		return nil, fmt.Errorf("-dir %q: the last path element must contain \"memmap\" (the directory is removed)", c.Dir)
	}
	if err := os.RemoveAll(c.Dir); err != nil {
		return nil, err
	}
	defer os.RemoveAll(c.Dir)
	e, err := core.Open(core.Options{Dir: c.Dir, SegmentSize: 64 << 20, NoViewFile: true, NoCheckpoint: true, DiskIndexes: c.Disk, DiskIndexCacheMB: 16})
	if err != nil {
		return nil, err
	}
	defer e.Close()
	p := planner.Start(e)
	r := &runner{e: e, p: p, s: sql.NewSession(e)}
	rep := &Report{N: c.N, Dist: c.Dist, Updates: c.Updates, Batch: c.Batch, Direct: map[string]int64{}}

	prev := readMem()
	prevSites := profileAttribution().Sites
	rep.Stages = append(rep.Stages, StageResult{Name: "baseline", Mem: prev})
	stage := func(name, note string, t0 time.Time) {
		r.waitViews()
		m := readMem()
		sites := profileAttribution().Sites
		rep.Stages = append(rep.Stages, StageResult{Name: name, Seconds: time.Since(t0).Seconds(), Mem: m, Delta: m.HeapObjects - prev.HeapObjects, Note: note, Grew: growth(prevSites, sites, 6)})
		prevSites = sites
		fmt.Fprintf(log, "memmap: N=%d stage %-10s %7.1fs heap %6d MB (+%d MB)\n", c.N, name, time.Since(t0).Seconds(), m.HeapObjects>>20, (m.HeapObjects-prev.HeapObjects)>>20)
		prev = m
	}

	t0 := time.Now()
	if err := r.exec(`CREATE TABLE sims (number text PRIMARY KEY, carrier text, status text, price bigint, updated_at bigint)`); err != nil {
		return nil, err
	}
	if err := r.exec(`ALTER TABLE sims SET (history_retention = '60 days')`); err != nil {
		return nil, err
	}
	if err := r.load("sims", c, num, 0, c.N, 0, false); err != nil {
		return nil, err
	}
	stage("primary", "table with N rows, primary key only", t0)
	if ts, err := e.Table("sims"); err == nil {
		st := ts.IndexStats()
		rep.Direct["primary_index_bytes"] = st.Bytes
		rep.Direct["primary_key_arena_bytes"] = st.ArenaBytes
		rep.Direct["primary_leaves"] = int64(st.Leaves)
	}

	if !skipped(c, "history") && c.Updates > 0 {
		t0 = time.Now()
		for v := 1; v <= c.Updates; v++ {
			if err := r.load("sims", c, num, 0, c.N, v, true); err != nil {
				return nil, err
			}
		}
		stage("history", fmt.Sprintf("%d more versions of every row (old versions live only in the log)", c.Updates), t0)
		rep.Direct["log_versions_total"] = c.N * int64(c.Updates+1)
	}
	if !skipped(c, "secondary") {
		t0 = time.Now()
		if err := r.exec(`CREATE INDEX sims_cf ON sims (carrier, status, price)`); err != nil {
			return nil, err
		}
		stage("secondary", "btree (carrier,status,price)", t0)
		for _, st := range e.SecIndexStats() {
			rep.Direct["secondary_entries"] += int64(st.Entries)
		}
	}
	if !skipped(c, "trigram") {
		t0 = time.Now()
		if err := r.exec(`CREATE INDEX sims_trgm ON sims USING gin (number gin_trgm_ops)`); err != nil {
			return nil, err
		}
		stage("trigram", "trigram view on number (dist="+c.Dist+")", t0)
	}
	if !skipped(c, "columnar") {
		t0 = time.Now()
		if err := r.exec(`CREATE TABLE sims_col (number text PRIMARY KEY, carrier text, status text, price bigint, updated_at bigint) WITH (columnar = true)`); err != nil {
			return nil, err
		}
		if err := r.load("sims_col", c, num, 0, c.N, 0, false); err != nil {
			return nil, err
		}
		stage("columnar", "second table sims_col with the same N rows, columnar=true: includes its own primary index, subtract the primary stage", t0)
		if t, err := e.Table("sims_col"); err == nil {
			rep.Direct["columnar_mem_bytes"] = p.Col.MemBytes(t.Schema().ID)
		}
	}

	if c.Disk {
		if !skipped(c, "secondary") {
			t0 = time.Now()
			if err := r.exec(`ALTER INDEX sims_cf SET (index_storage = 'disk')`); err != nil {
				return nil, err
			}
			stage("spill-secondary", "the btree (carrier,status,price) moved to a disk file: the heap that goes away", t0)
		}
		if !skipped(c, "trigram") {
			t0 = time.Now()
			if err := r.exec(`ALTER INDEX sims_trgm SET (index_storage = 'disk')`); err != nil {
				return nil, err
			}
			ts, _ := e.Table("sims")
			for p.Sub.StorageOf(ts.Schema().ID, "number") != "disk" {
				time.Sleep(10 * time.Millisecond)
			}
			p.Sub.WaitConversions()
			stage("spill-trigram", "the trigram posting lists moved to a disk file (documents stay in RAM)", t0)
		}
	}
	if ts, err := e.Table("sims"); err == nil {
		rep.Direct["primary_key_arena_dead_bytes"] = ts.IndexStats().ArenaDead
	}
	ir := e.IndexMemoryReport()
	rep.Direct["index_mem_ram_bytes_estimate"], rep.Direct["index_mem_disk_bytes"] = ir.RAMBytes, ir.DiskBytes
	rep.LogDisk = e.Log().DiskBytes()
	rep.GC = e.GCStats()
	runtime.GC()
	runtime.GC()
	a := profileAttribution()
	rep.Pprof, rep.PprofTot, rep.Unknown = a.Bytes, a.Total, a.Unknown
	return rep, nil
}

// ---- output ----

func mb(b int64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<20)) }

func perKey(b, n int64) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(b)/float64(n))
}

func stage(rep *Report, name string) *StageResult {
	for i := range rep.Stages {
		if rep.Stages[i].Name == name {
			return &rep.Stages[i]
		}
	}
	return nil
}

// structureRows maps stages to the structure each one measures. columnar is
// reported net of the primary stage because it carries its own primary index.
func structureRows(rep *Report) [][3]any {
	var out [][3]any
	add := func(name string, delta int64, note string) { out = append(out, [3]any{name, delta, note}) }
	if s := stage(rep, "primary"); s != nil {
		add("primary index + log mapping", s.Delta, "delta of the primary stage over baseline")
	}
	if s := stage(rep, "history"); s != nil {
		add(fmt.Sprintf("history: %d extra versions/key", rep.Updates), s.Delta, "RAM cost of old versions (expected ~0: only the head is indexed)")
	}
	if s := stage(rep, "secondary"); s != nil {
		add("secondary btree (carrier,status,price)", s.Delta, "")
	}
	if s := stage(rep, "trigram"); s != nil {
		add("trigram view (number)", s.Delta, "dist="+rep.Dist)
	}
	if s, p := stage(rep, "columnar"), stage(rep, "primary"); s != nil && p != nil {
		add("columnar view, net of one primary", s.Delta-p.Delta, fmt.Sprintf("raw column bytes (MemBytes) %s MB", mb(rep.Direct["columnar_mem_bytes"])))
	}
	return out
}

func printReport(w io.Writer, rep *Report) {
	n := rep.N
	fmt.Fprintf(w, "\n=== memmap N=%d dist=%s updates=%d ===\n", n, rep.Dist, rep.Updates)
	if n < 256*int64(rep.Batch) {
		fmt.Fprintf(w, "WARNING: N < 256 commits (%d rows): the committer's ring of the last 256 batches (~155 B/op each) is still filling, so the per-key numbers of the early stages include it and are not steady-state; use N >= %d\n", 256*rep.Batch, 4*256*rep.Batch)
	}
	fmt.Fprintf(w, "%-44s %10s %10s  %s\n", "structure (heap delta, runtime/metrics)", "MB", "B/key", "note")
	for _, r := range structureRows(rep) {
		d := r[1].(int64)
		fmt.Fprintf(w, "%-44s %10s %10s  %s\n", r[0], mb(d), perKey(d, n), r[2])
	}
	last := rep.Stages[len(rep.Stages)-1].Mem
	fmt.Fprintf(w, "%-44s %10s %10s  heap objects at the end (baseline %s MB)\n", "TOTAL live heap", mb(last.HeapObjects), perKey(last.HeapObjects, n), mb(rep.Stages[0].Mem.HeapObjects))
	fmt.Fprintf(w, "%-44s %10s %10s  go runtime total / process RSS %s MB\n", "Go memory (mapped)", mb(last.TotalGo), perKey(last.TotalGo, n), mb(last.RSS))
	fmt.Fprintf(w, "\npprof inuse_space of the final state (sampled; total %s MB, heap objects %s MB)\n", mb(rep.PprofTot), mb(last.HeapObjects))
	fmt.Fprintf(w, "%-44s %10s %10s\n", "structure (first matching frame)", "MB", "B/key")
	for _, s := range structureOrder {
		if b := rep.Pprof[s]; b > 0 {
			fmt.Fprintf(w, "%-44s %10s %10s\n", s, mb(b), perKey(b, n))
		}
	}
	for _, u := range rep.Unknown {
		fmt.Fprintf(w, "  unattributed site: %-60s %s MB\n", u.Func, mb(u.Bytes))
	}
	for _, s := range rep.Stages {
		if len(s.Grew) == 0 || s.Name == "baseline" {
			continue
		}
		fmt.Fprintf(w, "\nwhere stage %q allocated (pprof growth, innermost stask frame):\n", s.Name)
		for _, g := range s.Grew {
			fmt.Fprintf(w, "  %8s MB  %s\n", mb(g.Bytes), g.Func)
		}
	}
	fmt.Fprintf(w, "\ndirect: ")
	for k, v := range rep.Direct {
		fmt.Fprintf(w, "%s=%d ", k, v)
	}
	fmt.Fprintf(w, "\nlog on disk %s MB = %s B per version written\n", mb(rep.LogDisk), perKey(rep.LogDisk, n*int64(rep.Updates+1)))
	fmt.Fprintf(w, "stage times: ")
	for _, s := range rep.Stages {
		fmt.Fprintf(w, "%s=%.0fs ", s.Name, s.Seconds)
	}
	fmt.Fprintln(w)
}

// printLinearity compares bytes/key across the N values: a structure whose
// B/key grows with N is not linear (B-tree fill, map growth, posting lists).
func printLinearity(w io.Writer, reps []*Report) {
	fmt.Fprintf(w, "\n=== B/key across N (linear extrapolation is only honest if these are flat) ===\n%-44s", "structure")
	for _, r := range reps {
		fmt.Fprintf(w, " %12d", r.N)
	}
	fmt.Fprintln(w)
	rows := map[string][]string{}
	var order []string
	for _, r := range reps {
		for _, x := range structureRows(r) {
			name := x[0].(string)
			if _, ok := rows[name]; !ok {
				order = append(order, name)
			}
			rows[name] = append(rows[name], perKey(x[1].(int64), r.N))
		}
	}
	for _, name := range order {
		fmt.Fprintf(w, "%-44s", name)
		for _, v := range rows[name] {
			fmt.Fprintf(w, " %12s", v)
		}
		fmt.Fprintln(w)
	}
}
