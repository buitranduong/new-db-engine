package sql

import (
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"stask/internal/spill"
)

func TestSpillCodecCoversDatum(t *testing.T) {
	if n := reflect.TypeOf(Datum{}).NumField(); n != 4 {
		t.Fatalf("Datum has %d fields: update encodeRow/decodeRow in spillexec.go", n)
	}
	rows := [][]Datum{{}, {Null, IntD(-5), FloatD(1.5), TextD("héllo"), BoolD(true), TimeD(1 << 50), {K: KArray, S: "{1,2}"}, FloatD(-0.0), TextD("")}}
	for _, r := range rows {
		got, err := decodeRow(encodeRow(nil, r))
		if err != nil || len(got) != len(r) {
			t.Fatalf("%v %v", got, err)
		}
		for i := range r {
			if got[i] != r[i] {
				t.Fatalf("col %d: %#v vs %#v", i, got[i], r[i])
			}
		}
	}
	if _, err := decodeRow([]byte{5, 1}); err == nil {
		t.Fatal("malformed row accepted")
	}
}

func spillFixture(t *testing.T) *Session {
	s := NewSession(newEngine(t))
	run(t, s, `CREATE TABLE a (id bigint PRIMARY KEY, k bigint, g text, f double precision, tag text)`)
	run(t, s, `CREATE TABLE b (id bigint PRIMARY KEY, k bigint, w bigint)`)
	rng := rand.New(rand.NewSource(7))
	ins := func(table string, n int, row func(i int) string) {
		for lo := 1; lo <= n; lo += 1000 {
			var vs []string
			for i := lo; i < lo+1000 && i <= n; i++ {
				vs = append(vs, row(i))
			}
			run(t, s, "INSERT INTO "+table+" VALUES "+strings.Join(vs, ","))
		}
	}
	null := func(p float64, v string) string {
		if rng.Float64() < p {
			return "NULL"
		}
		return v
	}
	ins("a", 12000, func(i int) string {
		return fmt.Sprintf("(%d,%s,%s,%s,%s)", i, null(0.05, fmt.Sprint(rng.Intn(3000))), null(0.1, fmt.Sprintf("'g%d'", rng.Intn(500))),
			null(0.1, fmt.Sprintf("%.2f", rng.Float64()*100)), fmt.Sprintf("'%s'", strings.Repeat("x", rng.Intn(40))))
	})
	ins("b", 9000, func(i int) string {
		return fmt.Sprintf("(%d,%s,%d)", i, null(0.05, fmt.Sprint(rng.Intn(4000))), rng.Intn(100))
	})
	return s
}

var spillQueries = []string{
	`SELECT a.id, b.id FROM a JOIN b ON a.k = b.k ORDER BY a.id, b.id`,
	`SELECT a.id, b.id FROM a LEFT JOIN b ON a.k = b.k AND b.w > 50 ORDER BY a.id, b.id`,
	`SELECT a.id, b.id FROM a RIGHT JOIN b ON a.k = b.k ORDER BY a.id, b.id`,
	`SELECT a.id, b.id FROM a FULL JOIN b ON a.k = b.k AND a.id < b.id ORDER BY a.id, b.id`,
	`SELECT count(*), sum(b.w), min(a.f), max(a.g), avg(a.f) FROM a JOIN b ON a.k = b.k`,
	`SELECT g, count(*), count(f), sum(f), min(k), max(k), avg(f) FROM a, (SELECT 1 AS one) o GROUP BY g ORDER BY g`,
	`SELECT g, k % 7 AS m, count(*) FROM a, (SELECT 1 AS one) o GROUP BY g, k % 7 HAVING count(*) > 2 ORDER BY g, m`,
	`SELECT b.w, count(*), sum(a.f) FROM a JOIN b ON a.k = b.k WHERE a.id % 3 = 0 GROUP BY b.w ORDER BY b.w`,
	`SELECT DISTINCT g FROM a, (SELECT 1 AS one) o ORDER BY g`,
	`SELECT DISTINCT k % 100, g FROM a, (SELECT 1 AS one) o ORDER BY 1, 2`,
	`SELECT id, f FROM a, (SELECT 1 AS one) o ORDER BY f DESC NULLS LAST, id`,
	`SELECT id, g, tag FROM a, (SELECT 1 AS one) o ORDER BY g, tag DESC, id LIMIT 700 OFFSET 100`,
	`SELECT id FROM a, (SELECT 1 AS one) o WHERE f > 20 ORDER BY id LIMIT 5`,
	`SELECT count(*), sum(f) FROM a, (SELECT 1 AS one) o WHERE id < 0`,
	`SELECT a.id, b.id FROM a, b WHERE a.id = b.id AND a.k < 100 ORDER BY 1, 2`,
	`SELECT a.id, b.w FROM (SELECT * FROM a WHERE id < 40) a JOIN (SELECT * FROM b WHERE id < 40) b ON a.k % 100 < b.w ORDER BY 1, 2`,
}

// TestSpillMatchesMemory runs each query in memory and again with a budget
// that breaks it and operators limited to 256 KB, and compares the rows.
func TestSpillMatchesMemory(t *testing.T) {
	s := spillFixture(t)
	defer func(q, n, sm, so int64, sd string) {
		QueryMemLimit, SQLMemLimit, SpillMaxBytes, SpillOpMem, SpillDir = q, n, sm, so, sd
	}(QueryMemLimit, SQLMemLimit, SpillMaxBytes, SpillOpMem, SpillDir)
	norm := func(r *Result, ordered bool) string {
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
		if !ordered {
			sort.Strings(out)
		}
		return strings.Join(out, "\n")
	}
	for _, q := range spillQueries {
		QueryMemLimit, SQLMemLimit, SpillDir = 0, 0, ""
		want := run(t, s, q)
		QueryMemLimit, SpillOpMem, SpillDir = 3<<20, 256<<10, t.TempDir()
		before := spill.Snapshot()
		got, err := tryRun(s, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		ordered := strings.Contains(q, "ORDER BY")
		if norm(got, ordered) != norm(want, ordered) {
			t.Fatalf("%s: spilled result differs (%d rows vs %d)\n%.300s\n%.300s", q, len(got.Rows), len(want.Rows), norm(got, ordered), norm(want, ordered))
		}
		after := spill.Snapshot()
		t.Logf("%-90.90s rows=%-6d spill files=%d bytes=%d", q, len(want.Rows), after.Files-before.Files, after.BytesWritten-before.BytesWritten)
		if n := SQLMemInUse(); n != 0 {
			t.Fatalf("%d bytes still reserved", n)
		}
	}
}

func TestSpillRefusalsAndLimits(t *testing.T) {
	s := spillFixture(t)
	defer func(q, so int64, sd string, sm int64) {
		QueryMemLimit, SpillOpMem, SpillDir, SpillMaxBytes = q, so, sd, sm
	}(QueryMemLimit, SpillOpMem, SpillDir, SpillMaxBytes)
	QueryMemLimit, SpillOpMem, SpillDir = 3<<20, 256<<10, t.TempDir()
	// not a covered shape (array_agg grows with the group): still the budget error
	if _, err := tryRun(s, `SELECT g, array_agg(tag) FROM a JOIN b ON a.k = b.k GROUP BY g`); code(err) != "53200" {
		t.Fatalf("uncovered shape: %v", err)
	}
	// temp file quota
	SpillMaxBytes = 100 << 10
	if _, err := tryRun(s, `SELECT a.id, b.id FROM a JOIN b ON a.k = b.k ORDER BY 1, 2`); code(err) != "53100" {
		t.Fatalf("quota: %v", err)
	}
	SpillMaxBytes = 0
	// statement_timeout inside a spilling query
	run(t, s, `SET statement_timeout = '600ms'`)
	start, files := time.Now(), spill.Snapshot().Files
	_, err := tryRun(s, `SELECT count(*) FROM a x JOIN a y ON x.id % 50 = y.id % 50 JOIN a z ON z.id % 50 = y.id % 50`)
	if code(err) != "57014" || time.Since(start) > 5*time.Second || spill.Snapshot().Files == files {
		t.Fatalf("timeout in the spilling pass: %v after %v, %d temp files", err, time.Since(start), spill.Snapshot().Files-files)
	}
	run(t, s, `SET statement_timeout = 0`)
	if ents, _ := os.ReadDir(SpillDir); len(ents) != 0 {
		t.Fatalf("%d temp directories left after the cancelled and failed queries", len(ents))
	}
	if n := SQLMemInUse(); n != 0 {
		t.Fatalf("%d bytes still reserved", n)
	}
}
