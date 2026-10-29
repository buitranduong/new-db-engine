package core

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"stask/internal/schema"
)

// A batch (Engine.Write, or any commit) may hold several operations on the
// same key; they apply in order, so the last one wins. Recovery, replicas
// and CDC must reach the same state and the same events as the live engine
// (docs/01 invariants I2/I3).

func skRow(id string, n int64) schema.Row { return schema.Row{{S: id}, {I: n}, {S: "x"}} }

type skOp struct {
	key string
	n   int64 // put when del is false
	del bool
}

func (o skOp) op() WriteOp {
	if o.del {
		return WriteOp{Table: "kv", Key: o.key}
	}
	return WriteOp{Table: "kv", Row: skRow(o.key, o.n)}
}

func skDump(t *testing.T, e *Engine) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := e.ScanSnapshot("kv", e.DurableLSN(), func(key string, r schema.Row, v *Version) bool {
		out[key] = r[1].I
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	// point reads must agree with the scan
	for k, n := range out {
		r, _, err := e.Get("kv", k)
		if err != nil || r == nil || r[1].I != n {
			t.Fatalf("get %s: %v %v, scan says %d", k, r, err, n)
		}
	}
	return out
}

func skStr(m map[string]int64) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	for _, k := range ks {
		fmt.Fprintf(&b, "%s=%d ", k, m[k])
	}
	return b.String()
}

func skSame(t *testing.T, what string, got, want map[string]int64, live int64) {
	t.Helper()
	if skStr(got) != skStr(want) {
		t.Fatalf("%s: state differs\n got:  %s\n want: %s", what, skStr(got), skStr(want))
	}
	if live >= 0 && live != int64(len(want)) {
		t.Fatalf("%s: LiveRows %d, want %d", what, live, len(want))
	}
}

func skApply(model map[string]int64, ops []skOp) {
	for _, o := range ops {
		if o.del {
			delete(model, o.key)
		} else {
			model[o.key] = o.n
		}
	}
}

func skOpen(t *testing.T, dir string) *Engine {
	t.Helper()
	return open(t, dir, 1<<20)
}

func skWrite(t *testing.T, e *Engine, ops []skOp) {
	t.Helper()
	w := make([]WriteOp, len(ops))
	for i, o := range ops {
		w[i] = o.op()
	}
	if _, err := e.Write(w); err != nil {
		t.Fatal(err)
	}
}

func skLive(e *Engine) int64 {
	ts, _ := e.Table("kv")
	return ts.LiveRows()
}

func TestSameKeyBatchRecovers(t *testing.T) {
	cases := map[string][]skOp{
		"put-del-new":      {{key: "a", n: 1}, {key: "a", del: true}},
		"put-put-new":      {{key: "a", n: 1}, {key: "a", n: 2}},
		"del-put-existing": {{key: "e", del: true}, {key: "e", n: 9}},
		"put-del-existing": {{key: "e", n: 5}, {key: "e", del: true}},
		"put-put-existing": {{key: "e", n: 5}, {key: "e", n: 6}},
		"long-chain":       {{key: "a", n: 1}, {key: "a", del: true}, {key: "a", n: 3}, {key: "b", n: 1}, {key: "a", del: true}, {key: "b", n: 2}},
	}
	for _, withCkpt := range []bool{false, true} {
		for name, ops := range cases {
			t.Run(fmt.Sprintf("%s/ckpt=%v", name, withCkpt), func(t *testing.T) {
				dir := t.TempDir()
				e := skOpen(t, dir)
				if _, err := e.CreateTable(kvTable()); err != nil {
					t.Fatal(err)
				}
				model := map[string]int64{}
				pre := []skOp{{key: "e", n: 1}, {key: "z", n: 1}}
				skWrite(t, e, pre)
				skApply(model, pre)
				if withCkpt {
					if err := e.Checkpoint(); err != nil {
						t.Fatal(err)
					}
				}
				skWrite(t, e, ops)
				skApply(model, ops)
				skSame(t, "live", skDump(t, e), model, skLive(e))
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e = skOpen(t, dir)
				defer e.Close()
				skSame(t, "reopened", skDump(t, e), model, skLive(e))
			})
		}
	}
}

// a checkpoint taken after the batch, then a restart from it
func TestSameKeyBatchCheckpointAfter(t *testing.T) {
	dir := t.TempDir()
	e := skOpen(t, dir)
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	model := map[string]int64{}
	ops := []skOp{{key: "a", n: 1}, {key: "a", del: true}, {key: "b", n: 1}, {key: "b", n: 2}}
	skWrite(t, e, ops)
	skApply(model, ops)
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = skOpen(t, dir)
	skSame(t, "from checkpoint", skDump(t, e), model, skLive(e))
	e.Close()
}

func TestSameKeyInsertIfAbsentTwice(t *testing.T) {
	dir := t.TempDir()
	e := skOpen(t, dir)
	defer e.Close()
	if _, err := e.CreateTable(kvTable()); err != nil {
		t.Fatal(err)
	}
	ins := func(k string, n int64) WriteOp { return WriteOp{Table: "kv", Row: skRow(k, n), IfAbsent: true} }
	if _, err := e.Write([]WriteOp{ins("a", 1), ins("a", 2)}); !errors.Is(err, ErrExists) {
		t.Fatalf("second insert of the same key in one batch: %v, want ErrExists", err)
	}
	if r, _, _ := e.Get("kv", "a"); r != nil {
		t.Fatalf("failed batch left a row: %v", r)
	}
	// insert, delete, insert is legal
	if _, err := e.Write([]WriteOp{ins("a", 1), {Table: "kv", Key: "a"}, ins("a", 3)}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := e.Get("kv", "a"); r == nil || r[1].I != 3 {
		t.Fatalf("a = %v", r)
	}
	// existing row, delete, insert in one batch
	if _, err := e.Write([]WriteOp{{Table: "kv", Key: "a"}, ins("a", 4)}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := e.Get("kv", "a"); r == nil || r[1].I != 4 {
		t.Fatalf("a = %v", r)
	}
}

// skReplica ships the leader's log to a fresh replica.
func skReplica(t *testing.T, leader *Engine) *Engine {
	t.Helper()
	_, pos, _ := leader.DurablePoint()
	segs := leader.Log().Segments()
	if len(segs) != 1 {
		t.Skipf("log spans %d segments", len(segs))
	}
	buf := make([]byte, pos.Off)
	if _, err := leader.Log().ReadAt(segs[0], 0, buf); err != nil {
		t.Fatal(err)
	}
	r, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, Replica: true, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(r.Log().WritePos(), buf); err != nil {
		t.Fatal(err)
	}
	return r
}

// skBatches draws random batches over a small key space so that keys repeat
// within a batch.
func skBatches(rng *rand.Rand, nb int) [][]skOp {
	var out [][]skOp
	for i := 0; i < nb; i++ {
		n := 1 + rng.Intn(8)
		if rng.Intn(10) == 0 {
			n = 20 + rng.Intn(40)
		}
		var b []skOp
		for j := 0; j < n; j++ {
			o := skOp{key: fmt.Sprintf("k%02d", rng.Intn(10)), n: rng.Int63n(1000)}
			o.del = rng.Intn(3) == 0
			b = append(b, o)
		}
		out = append(out, b)
	}
	return out
}

func TestSameKeyProperty(t *testing.T) {
	seeds := 25
	if testing.Short() {
		seeds = 5
	}
	for seed := 1; seed <= seeds; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(seed)))
			dir := t.TempDir()
			e := skOpen(t, dir)
			if _, err := e.CreateTable(kvTable()); err != nil {
				t.Fatal(err)
			}
			if _, err := e.CreateSlot("s", "wal2json"); err != nil {
				t.Fatal(err)
			}
			model := map[string]int64{}
			var wantEvents [][]string
			batches := skBatches(rng, 60)
			for i, b := range batches {
				// events a sequential execution would emit
				var ev []string
				m2 := map[string]int64{}
				for k, v := range model {
					m2[k] = v
				}
				for _, o := range b {
					_, had := m2[o.key]
					switch {
					case o.del && had:
						ev = append(ev, "D "+o.key)
					case !o.del && had:
						ev = append(ev, fmt.Sprintf("U %s %d", o.key, o.n))
					case !o.del:
						ev = append(ev, fmt.Sprintf("I %s %d", o.key, o.n))
					}
					skApply(m2, []skOp{o})
				}
				skWrite(t, e, b)
				skApply(model, b)
				wantEvents = append(wantEvents, ev)
				if i == len(batches)/2 && seed%2 == 0 {
					if err := e.Checkpoint(); err != nil {
						t.Fatal(err)
					}
				}
				if i%15 == 0 {
					skSame(t, "live", skDump(t, e), model, skLive(e))
				}
			}
			skSame(t, "live", skDump(t, e), model, skLive(e))

			// CDC: same events, same order
			got := collect(t, e, "s", ChangeOpts{})
			if len(got) != len(wantEvents) {
				t.Fatalf("cdc: %d transactions, want %d", len(got), len(wantEvents))
			}
			for i := range got {
				if fmt.Sprint(got[i].ops) != fmt.Sprint(wantEvents[i]) && !(len(got[i].ops) == 0 && len(wantEvents[i]) == 0) {
					t.Fatalf("cdc tx %d: %v, want %v (batch %v)", i, got[i].ops, wantEvents[i], batches[i])
				}
			}

			rep := skReplica(t, e)
			skSame(t, "replica", skDump(t, rep), model, skLive(rep))
			rep.Close()

			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e = skOpen(t, dir)
			skSame(t, "reopened", skDump(t, e), model, skLive(e))
			// and again after more writes on the reopened engine
			more := skBatches(rng, 20)
			for _, b := range more {
				skWrite(t, e, b)
				skApply(model, b)
			}
			skSame(t, "reopened+writes", skDump(t, e), model, skLive(e))
			e.Close()
			e = skOpen(t, dir)
			skSame(t, "reopened twice", skDump(t, e), model, skLive(e))
			e.Close()
		})
	}
}
