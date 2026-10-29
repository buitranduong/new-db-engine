package proj

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

func TestViewIntCodec(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	gens := map[string]func(i int) int64{
		"const":     func(i int) int64 { return 42 },
		"ascending": func(i int) int64 { return 1_000_000 + int64(i) },
		"step":      func(i int) int64 { return int64(i) * 1000 },
		"time":      func(i int) int64 { return 1_790_000_000_000 + int64(i)*37 + int64(rng.Intn(5)) },
		"random":    func(i int) int64 { return rng.Int63() - rng.Int63() },
		"small":     func(i int) int64 { return int64(rng.Intn(7)) },
		"extremes":  func(i int) int64 { return []int64{math.MinInt64, math.MaxInt64, 0, -1}[rng.Intn(4)] },
	}
	for name, g := range gens {
		for _, n := range []int{1, 2, 3, 5, 1000, viewBlock} {
			v := make([]int64, n)
			for i := range v {
				v[i] = g(i)
			}
			got, err := vDecInts(vEncInts(v))
			if err != nil || !slices.Equal(got, v) {
				t.Fatalf("%s n=%d: roundtrip failed (%v)", name, n, err)
			}
		}
	}
	fgens := map[string]func(i int) float64{
		"normal":   func(i int) float64 { return rng.NormFloat64() * 1e6 },
		"money":    func(i int) float64 { return math.Round(rng.Float64()*1e6) / 100 },
		"negative": func(i int) float64 { return float64(rng.Intn(2000)-1000) / 10 },
		"integers": func(i int) float64 { return float64(rng.Intn(50)) },
		"special": func(i int) float64 {
			return []float64{math.NaN(), math.Inf(1), math.Copysign(0, -1), 0, 1e300, 5e-324, 0.1}[rng.Intn(7)]
		},
		"big": func(i int) float64 { return float64(rng.Int63()) / 100 },
	}
	for name, g := range fgens {
		for _, n := range []int{1, 7, 5000, viewBlock} {
			f := make([]float64, n)
			for i := range f {
				f[i] = g(i)
			}
			got, err := vDecFloats(vEncFloats(f))
			if err != nil || len(got) != n {
				t.Fatalf("%s: %v", name, err)
			}
			for i := range f {
				if math.Float64bits(f[i]) != math.Float64bits(got[i]) {
					t.Fatalf("%s n=%d: float %d differs: %v vs %v", name, n, i, f[i], got[i])
				}
			}
		}
	}
}

func viewTables() []schema.Table {
	return []schema.Table{
		{Name: "sales", Key: "id", Columnar: true, Columns: []schema.Column{
			{Name: "id", Type: "int"}, {Name: "region", Type: "string"}, {Name: "qty", Type: "int"},
			{Name: "price", Type: "float"}, {Name: "ts", Type: "time"}, {Name: "ok", Type: "bool"}, {Name: "note", Type: "text"},
		}},
		{Name: "kv", Key: "k", Columnar: true, Columns: []schema.Column{
			{Name: "k", Type: "string"}, {Name: "g", Type: "int"}, {Name: "v", Type: "float"},
		}},
	}
}

type viewEnv struct {
	e *core.Engine
	c *Columnar
	f *core.Follower
}

func startView(t *testing.T, dir string, noSaveOnClose bool) *viewEnv {
	t.Helper()
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf,
		ViewSaveEvery: -1, NoViewSaveOnClose: noSaveOnClose})
	if err != nil {
		t.Fatal(err)
	}
	c := NewColumnar()
	return &viewEnv{e: e, c: c, f: e.Follow(c)}
}

func (v *viewEnv) ready(t *testing.T) {
	t.Helper()
	for i := 0; i < 2000 && !v.f.Ready(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !v.f.Ready() {
		t.Fatal("view not ready")
	}
}

func (v *viewEnv) write(t *testing.T, rng *rand.Rand, batches int) uint64 {
	t.Helper()
	var lsn uint64
	for i := 0; i < batches; i++ {
		var ops []core.WriteOp
		seen := map[string]bool{} // one operation per key in a batch
		for j := 0; j < 20; j++ {
			id := int64(rng.Intn(30000))
			if tag := fmt.Sprint(id%500, "/", id); seen[tag] {
				continue
			} else {
				seen[tag] = true
			}
			switch rng.Intn(10) {
			case 0:
				ops = append(ops, core.WriteOp{Table: "sales", Key: fmt.Sprint(id)})
			case 1:
				ops = append(ops, core.WriteOp{Table: "kv", Key: fmt.Sprint("k", id%500)})
			case 2, 3:
				ops = append(ops, core.WriteOp{Table: "kv", Row: schema.Row{{S: fmt.Sprint("k", id%500)}, {I: id % 9}, {F: rng.Float64()}}})
			default:
				r := schema.Row{{I: id}, {S: fmt.Sprint("r", rng.Intn(6))}, {I: int64(rng.Intn(100))}, {F: rng.Float64() * 50},
					{I: 1_790_000_000_000 + rng.Int63n(1e9)}, {I: int64(rng.Intn(2))}, {S: fmt.Sprint("n", rng.Intn(10))}}
				if rng.Intn(15) == 0 {
					r[3].Null = true
				}
				if rng.Intn(15) == 0 {
					r[2].Null = true
				}
				ops = append(ops, core.WriteOp{Table: "sales", Row: r})
			}
		}
		var err error
		if lsn, err = v.e.Write(ops); err != nil {
			t.Fatal(err)
		}
	}
	if !v.f.WaitApplied(lsn, 20*time.Second) {
		t.Fatal("view did not catch up")
	}
	return lsn
}

var viewQueries = []AggQuery{
	{Table: "sales", Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "qty"}, {Fn: "avg", Col: "price"}, {Fn: "max", Col: "price"}, {Fn: "min", Col: "ts"}}},
	{Table: "sales", GroupBy: []string{"region"}, Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "qty"}, {Fn: "min", Col: "price"}}},
	{Table: "sales", GroupBy: []string{"region", "ok"}, Filters: []Filter{{Col: "qty", Op: ">=", Value: 50}}, Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "price"}}},
	{Table: "sales", GroupBy: []string{"note"}, Aggs: []Agg{{Fn: "count"}}},
	{Table: "kv", GroupBy: []string{"g"}, Aggs: []Agg{{Fn: "count"}, {Fn: "sum", Col: "v"}}},
	{Table: "kv", Aggs: []Agg{{Fn: "count"}, {Fn: "max", Col: "v"}}},
}

func (v *viewEnv) answers(t *testing.T) []string {
	t.Helper()
	var out []string
	for i := range viewQueries {
		q := viewQueries[i]
		ts, err := v.e.Table(q.Table)
		if err != nil { // dropped
			out = append(out, "-")
			continue
		}
		res, err := v.c.Aggregate(ts.Schema().ID, &q)
		if err != nil {
			t.Fatal(err)
		}
		// sorted, floats rounded: group order is not defined
		var rows []string
		for _, r := range res.Rows {
			s := ""
			for _, x := range r {
				if f, ok := x.(float64); ok {
					x = math.Round(f*1e6) / 1e6
				}
				s += fmt.Sprintf("%v|", x)
			}
			rows = append(rows, s)
		}
		slices.Sort(rows)
		out = append(out, fmt.Sprint(rows))
	}
	return out
}

func (v *viewEnv) mustSave(t *testing.T) {
	t.Helper()
	before := v.f.SavedLSN()
	v.f.RequestSave()
	for i := 0; i < 2000 && v.f.SavedLSN() == before; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if v.f.SavedLSN() == before {
		t.Fatal("no save happened")
	}
}

func setupView(t *testing.T) (dir string, want []string, lsnB uint64) {
	dir = t.TempDir()
	v := startView(t, dir, true)
	for _, tb := range viewTables() {
		if _, err := v.e.CreateTable(tb); err != nil {
			t.Fatal(err)
		}
	}
	v.ready(t)
	rng := rand.New(rand.NewSource(7))
	v.write(t, rng, 400)
	v.mustSave(t) // the view as of here is on disk ...
	lsnB = v.write(t, rng, 300)
	want = v.answers(t) // ... and this much more is only in the log
	v.e.Close()         // NoViewSaveOnClose: as after a crash
	return
}

// A restart loads the saved view and replays only the log after it: the
// answers are those of a view that never stopped.
func TestViewRestartReplaysTail(t *testing.T) {
	dir, want, lsnB := setupView(t)
	v := startView(t, dir, true)
	defer v.e.Close()
	v.ready(t)
	if !v.f.Restored() {
		t.Fatal("view was rebuilt instead of loaded")
	}
	if !v.f.WaitApplied(lsnB, 10*time.Second) {
		t.Fatal("tail not replayed")
	}
	if got := v.answers(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("answers differ after restart\n got %v\nwant %v", got, want)
	}
	// and a view rebuilt from the data agrees
	dir2 := t.TempDir()
	copyDir(t, dir, dir2)
	os.Remove(filepath.Join(dir2, viewFile))
	b := startView(t, dir2, true)
	defer b.e.Close()
	b.ready(t)
	if b.f.Restored() {
		t.Fatal("rebuilt view claims to be restored")
	}
	b.f.WaitApplied(lsnB, 10*time.Second)
	if got := b.answers(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt answers differ\n got %v\nwant %v", got, want)
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	ents, _ := os.ReadDir(from)
	for _, en := range ents {
		if en.IsDir() {
			os.MkdirAll(filepath.Join(to, en.Name()), 0o755)
			copyDir(t, filepath.Join(from, en.Name()), filepath.Join(to, en.Name()))
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, en.Name()))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(to, en.Name()), b, 0o644)
	}
}

// A damaged or half-written view file is never used: the view is rebuilt
// and answers as before.
func TestViewDamagedFileRebuilds(t *testing.T) {
	dir, want, lsnB := setupView(t)
	path := filepath.Join(dir, viewFile)
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":     {},
		"truncated": orig[:len(orig)/2],
		"no-end":    orig[:len(orig)-9],
		"garbage":   []byte("not a view file at all"),
	}
	flip := slices.Clone(orig)
	flip[len(flip)/3] ^= 0x40
	cases["bitflip"] = flip
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			copyDir(t, dir, d)
			os.WriteFile(filepath.Join(d, viewFile), data, 0o644)
			v := startView(t, d, true)
			defer v.e.Close()
			v.ready(t)
			if v.f.Restored() {
				t.Fatal("damaged file was used")
			}
			v.f.WaitApplied(lsnB, 10*time.Second)
			if got := v.answers(t); !reflect.DeepEqual(got, want) {
				t.Fatalf("answers differ\n got %v\nwant %v", got, want)
			}
		})
	}
}

// A crash while a new file is being written (before the rename) leaves
// the previous file in place and loadable; the temporary file is ignored
// and replaced by the next save.
func TestViewCrashDuringSave(t *testing.T) {
	dir := t.TempDir()
	v := startView(t, dir, true)
	for _, tb := range viewTables() {
		if _, err := v.e.CreateTable(tb); err != nil {
			t.Fatal(err)
		}
	}
	v.ready(t)
	rng := rand.New(rand.NewSource(9))
	v.write(t, rng, 200)
	v.mustSave(t)
	good := v.answers(t)
	goodLSN := v.f.SavedLSN()
	v.write(t, rng, 100)
	crash := errors.New("crash")
	hk := func(stage string) error { return crash } // dies after the fsync, before the rename
	saveHook.Store(&hk)
	before := v.f.SavedLSN()
	v.f.RequestSave()
	time.Sleep(500 * time.Millisecond)
	saveHook.Store(nil)
	if v.f.SavedLSN() != before {
		t.Fatal("a crashed save was counted")
	}
	if _, err := os.Stat(filepath.Join(dir, viewFile+".tmp")); err == nil {
		t.Fatal("temporary file left behind by a failed save")
	}
	v.e.Close()
	// leave a torn temporary file as a real crash would
	os.WriteFile(filepath.Join(dir, viewFile+".tmp"), []byte("STCV torn"), 0o644)
	r := startView(t, dir, true)
	defer r.e.Close()
	r.ready(t)
	if !r.f.Restored() || r.f.SavedLSN() != goodLSN {
		t.Fatalf("previous view not loaded (restored=%v lsn=%d want %d)", r.f.Restored(), r.f.SavedLSN(), goodLSN)
	}
	_ = good
	r.mustSave(t) // overwrites the torn temporary file
	if _, err := os.Stat(filepath.Join(dir, viewFile+".tmp")); err == nil {
		t.Fatal("temporary file not replaced")
	}
}

// A view saved at Close is loaded by the next start with nothing to replay.
func TestViewSavedAtClose(t *testing.T) {
	dir := t.TempDir()
	v := startView(t, dir, false)
	for _, tb := range viewTables() {
		if _, err := v.e.CreateTable(tb); err != nil {
			t.Fatal(err)
		}
	}
	v.ready(t)
	v.write(t, rand.New(rand.NewSource(3)), 300)
	want := v.answers(t)
	v.e.Close()
	r := startView(t, dir, false)
	defer r.e.Close()
	r.ready(t)
	if !r.f.Restored() {
		t.Fatal("not restored after a clean close")
	}
	if got := r.answers(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("answers differ\n got %v\nwant %v", got, want)
	}
}

// The schema changed after the save (ALTER or DROP TABLE, found in the log
// tail): the file no longer describes the tables, so the view is rebuilt.
func TestViewSchemaChangeRebuilds(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(e *core.Engine) error
	}{
		{"add-column", func(e *core.Engine) error {
			return e.AlterTable("kv", func(d *schema.Table) error {
				d.Columns = append(d.Columns, schema.Column{Name: "extra", Type: "int"})
				return nil
			})
		}},
		{"drop-table", func(e *core.Engine) error { return e.DropTable("sales") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			v := startView(t, dir, true)
			for _, tb := range viewTables() {
				if _, err := v.e.CreateTable(tb); err != nil {
					t.Fatal(err)
				}
			}
			v.ready(t)
			v.write(t, rand.New(rand.NewSource(3)), 100)
			v.mustSave(t)
			want := v.answers(t)
			if err := tc.do(v.e); err != nil {
				t.Fatal(err)
			}
			v.e.Close()
			r := startView(t, dir, true)
			defer r.e.Close()
			r.ready(t)
			if r.f.Restored() {
				t.Fatal("a view of other tables was loaded")
			}
			r.f.WaitApplied(r.e.DurableLSN(), 10*time.Second)
			if got := r.answers(t)[4:]; !reflect.DeepEqual(got, want[4:]) {
				t.Fatalf("kv answers differ\n got %v\nwant %v", got, want[4:])
			}
		})
	}
}
