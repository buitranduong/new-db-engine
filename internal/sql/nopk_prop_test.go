package sql

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"stask/internal/core"
)

// Property tests: random writes to a table without PRIMARY KEY and to one
// with a composite key, compared with a reference model after every step,
// across checkpoints and restarts of the engine.

func reopen(t *testing.T, e *core.Engine, dir string) *core.Engine {
	t.Helper()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return e2
}

func dump(t *testing.T, s *Session, q string) string {
	t.Helper()
	r := run(t, s, q)
	var rows []string
	for _, row := range r.Rows {
		var vs []string
		for _, v := range row {
			vs = append(vs, v.String())
		}
		rows = append(rows, strings.Join(vs, ","))
	}
	sort.Strings(rows)
	return strings.Join(rows, ";")
}

func TestPropNoPrimaryKey(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { e.Close() }()
			s := NewSession(e)
			run(t, s, `CREATE TABLE np (g INT, v INT, s TEXT)`)
			type row struct{ g, v int }
			var model []row
			render := func() string {
				var rows []string
				for _, r := range model {
					rows = append(rows, fmt.Sprintf("%d,%d,x%d", r.g, r.v, r.g))
				}
				sort.Strings(rows)
				return strings.Join(rows, ";")
			}
			for step := 0; step < 300; step++ {
				switch op := rng.Intn(10); {
				case op < 5: // insert 1-5 rows, duplicates likely
					n := 1 + rng.Intn(5)
					var vals []string
					for i := 0; i < n; i++ {
						r := row{rng.Intn(6), rng.Intn(4)}
						model = append(model, r)
						vals = append(vals, fmt.Sprintf("(%d, %d, 'x%d')", r.g, r.v, r.g))
					}
					run(t, s, `INSERT INTO np VALUES `+strings.Join(vals, ","))
				case op < 7:
					g, v := rng.Intn(6), rng.Intn(4)
					for i := range model {
						if model[i].g == g {
							model[i].v = v
						}
					}
					run(t, s, fmt.Sprintf(`UPDATE np SET v = %d WHERE g = %d`, v, g))
				case op < 9:
					v := rng.Intn(4)
					g := rng.Intn(6)
					var keep []row
					for _, r := range model {
						if !(r.v == v && r.g <= g) {
							keep = append(keep, r)
						}
					}
					model = keep
					run(t, s, fmt.Sprintf(`DELETE FROM np WHERE v = %d AND g <= %d`, v, g))
				default:
					if rng.Intn(2) == 0 {
						if err := e.Checkpoint(); err != nil {
							t.Fatal(err)
						}
					} else {
						e = reopen(t, e, dir)
						s = NewSession(e)
					}
				}
				if got := dump(t, s, `SELECT * FROM np`); got != render() {
					t.Fatalf("seed %d step %d: table %q, model %q", seed, step, got, render())
				}
			}
			e = reopen(t, e, dir)
			s = NewSession(e)
			if got := dump(t, s, `SELECT * FROM np`); got != render() {
				t.Fatalf("after the final restart: %q, model %q", got, render())
			}
			t.Logf("%d rows at the end", len(model))
		})
	}
}

func TestPropCompositeKey(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { e.Close() }()
			s := NewSession(e)
			run(t, s, `CREATE TABLE ck (a INT, b TEXT, v INT, PRIMARY KEY (a, b))`)
			type key struct {
				a int
				b string
			}
			model := map[key]int{}
			render := func() string {
				var rows []string
				for k, v := range model {
					rows = append(rows, fmt.Sprintf("%d,%s,%d", k.a, k.b, v))
				}
				sort.Strings(rows)
				return strings.Join(rows, ";")
			}
			// strings with a zero byte and prefixes exercise the key encoding
			names := []string{"", "a", "ab", "b", "a\x01", "a\x01b"}
			lit := func(b string) string { return "'" + strings.ReplaceAll(b, "\x01", "' || chr(1) || '") + "'" }
			for step := 0; step < 300; step++ {
				k := key{rng.Intn(5) - 2, names[rng.Intn(len(names))]}
				v := rng.Intn(100)
				switch op := rng.Intn(10); {
				case op < 3:
					_, err := tryRun(s, fmt.Sprintf(`INSERT INTO ck VALUES (%d, %s, %d)`, k.a, lit(k.b), v))
					if _, dup := model[k]; dup {
						if code(err) != "23505" {
							t.Fatalf("step %d: duplicate insert %v: %v", step, k, err)
						}
					} else if err != nil {
						t.Fatal(err)
					} else {
						model[k] = v
					}
				case op < 5:
					run(t, s, fmt.Sprintf(`INSERT INTO ck VALUES (%d, %s, %d) ON CONFLICT (a, b) DO UPDATE SET v = excluded.v`, k.a, lit(k.b), v))
					model[k] = v
				case op < 6:
					run(t, s, fmt.Sprintf(`UPDATE ck SET v = %d WHERE a = %d`, v, k.a))
					for mk := range model {
						if mk.a == k.a {
							model[mk] = v
						}
					}
				case op < 7: // move one key
					nk := key{rng.Intn(5) - 2, names[rng.Intn(len(names))]}
					_, err := tryRun(s, fmt.Sprintf(`UPDATE ck SET a = %d, b = %s WHERE a = %d AND b = %s`, nk.a, lit(nk.b), k.a, lit(k.b)))
					old, have := model[k]
					_, taken := model[nk]
					switch {
					case !have:
						if err != nil {
							t.Fatal(err)
						}
					case nk != k && taken:
						if code(err) != "23505" {
							t.Fatalf("step %d: move %v to taken %v: %v", step, k, nk, err)
						}
					default:
						if err != nil {
							t.Fatal(err)
						}
						delete(model, k)
						model[nk] = old
					}
				case op < 9:
					run(t, s, fmt.Sprintf(`DELETE FROM ck WHERE a = %d AND b = %s`, k.a, lit(k.b)))
					delete(model, k)
				default:
					if rng.Intn(2) == 0 {
						if err := e.Checkpoint(); err != nil {
							t.Fatal(err)
						}
					} else {
						e = reopen(t, e, dir)
						s = NewSession(e)
					}
				}
				if got := dump(t, s, `SELECT a, b, v FROM ck`); got != render() {
					t.Fatalf("seed %d step %d: table %q, model %q", seed, step, got, render())
				}
			}
			e = reopen(t, e, dir)
			s = NewSession(e)
			if got := dump(t, s, `SELECT a, b, v FROM ck`); got != render() {
				t.Fatalf("after the final restart: %q, model %q", got, render())
			}
			t.Logf("%d rows at the end", len(model))
		})
	}
}
