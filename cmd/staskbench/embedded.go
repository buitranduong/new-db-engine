package main

import (
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// wl1Embedded runs the WL-1 mix against the engine in-process (no HTTP,
// no JSON) to separate engine cost from transport cost.
func wl1Embedded(dir string) {
	e, err := core.Open(core.Options{Dir: dir, Logf: func(string, ...any) {}})
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Table("kv"); err != nil {
		if _, err := e.CreateTable(schema.Table{Name: "kv", Key: "id", Columns: []schema.Column{
			{Name: "id", Type: "string"}, {Name: "n", Type: "int"}, {Name: "val", Type: "string"}}}); err != nil {
			log.Fatal(err)
		}
	}
	pool := make([]string, 256)
	prng := rand.New(rand.NewSource(42))
	for i := range pool {
		pool[i] = randString(prng, *valSize)
	}
	key := func(i int) string { return fmt.Sprintf("k%09d", i) }
	res := map[string]any{"mode": "embedded"}
	if !*skipLoad {
		t0 := time.Now()
		var wg sync.WaitGroup
		for l := 0; l < 8; l++ {
			wg.Add(1)
			go func(l int) {
				defer wg.Done()
				for s := l * 200; s < *nKeys; s += 8 * 200 {
					var ops []core.WriteOp
					for i := s; i < min(s+200, *nKeys); i++ {
						ops = append(ops, core.WriteOp{Table: "kv", Row: schema.Row{{S: key(i)}, {I: int64(i)}, {S: pool[i%len(pool)]}}})
					}
					if _, err := e.Write(ops); err != nil {
						log.Fatal(err)
					}
				}
			}(l)
		}
		wg.Wait()
		res["load_rows_per_s"] = float64(*nKeys) / time.Since(t0).Seconds()
	}
	var insSeq atomic.Int64
	insSeq.Store(time.Now().UnixNano() / 1000)
	s0, w0, p0 := e.Stats.Syncs.Load(), e.Log().BytesWritten(), e.Stats.PayloadBytes.Load()
	recs, el := run(*conc, *dur, *rate, func(w int, rng *rand.Rand, rec *recorder) (string, bool) {
		p := rng.Intn(100)
		switch {
		case p < 70:
			r, _, err := e.Get("kv", key(rng.Intn(*nKeys)))
			return "read", err == nil && r != nil
		case p < 90:
			_, err := e.Write([]core.WriteOp{{Table: "kv", Row: schema.Row{{S: key(rng.Intn(*nKeys))}, {I: rng.Int63()}, {S: pool[rng.Intn(len(pool))]}}}})
			return "update", err == nil
		default:
			_, err := e.Write([]core.WriteOp{{Table: "kv", IfAbsent: true, Row: schema.Row{{S: fmt.Sprintf("i%02d-%012d", w, insSeq.Add(1))}, {I: 0}, {S: pool[rng.Intn(len(pool))]}}}})
			return "insert", err == nil
		}
	})
	res["ops"] = recs.summary(el)
	res["timeline_max_ms"] = timeline(recs)
	syncs := float64(e.Stats.Syncs.Load() - s0)
	res["amplification"] = map[string]any{"waf_logical": float64(e.Log().BytesWritten()-w0) / float64(max(e.Stats.PayloadBytes.Load()-p0, 1)),
		"commits_per_sync":       float64(recs.summary(el)["update"].Count+recs.summary(el)["insert"].Count) / max(syncs, 1),
		"waf_device_upper_bound": 0.0, "syncs_over_50ms": e.Stats.SyncsOver50.Load()}
	report("WL-1 point operations (embedded)", res)
}
