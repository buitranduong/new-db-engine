// aggbench times analytical and point queries, one at a time, against
// PostgreSQL, STASK (SQL row path or the columnar view over HTTP) and
// ClickHouse (HTTP), on the orders table of staskmix. Every system gets
// the same queries; results are reduced to a checksum so that answers can
// be compared across systems.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type query struct {
	name string
	sql  string // PostgreSQL / STASK SQL; ClickHouse unless chSQL is set
	ch   string
	col  map[string]any // STASK columnar /v1/query aggregate
	runs int
}

var queries = []query{
	{name: "q1-groupby-status", runs: 20,
		sql: `SELECT status, count(*), sum(amount) FROM orders GROUP BY status`,
		col: map[string]any{"table": "orders", "group_by": []string{"status"},
			"aggs": []map[string]string{{"fn": "count"}, {"fn": "sum", "col": "amount"}}}},
	{name: "q2-filter-agg", runs: 20,
		sql: `SELECT count(*), sum(amount) FROM orders WHERE amount > 9000`,
		col: map[string]any{"table": "orders", "filters": []map[string]any{{"col": "amount", "op": ">", "value": 9000}},
			"aggs": []map[string]string{{"fn": "count"}, {"fn": "sum", "col": "amount"}}}},
	{name: "q3-top-customers", runs: 10,
		sql: `SELECT customer_id, sum(amount) AS s FROM orders GROUP BY customer_id ORDER BY s DESC LIMIT 10`,
		col: map[string]any{"table": "orders", "group_by": []string{"customer_id"},
			"aggs": []map[string]string{{"fn": "sum", "col": "amount", "as": "s"}}, "order_by": "s", "desc": true, "limit": 10}},
	{name: "q5-daily", runs: 20,
		sql: `SELECT date_trunc('day', created) AS d, count(*), sum(amount) FROM orders GROUP BY date_trunc('day', created) ORDER BY d`,
		ch:  `SELECT toStartOfDay(created) AS d, count(), sum(amount) FROM orders GROUP BY d ORDER BY d`,
		col: map[string]any{"table": "orders", "group_by": []string{"created"}, "group_trunc": []string{"day"},
			"aggs": []map[string]string{{"fn": "count"}, {"fn": "sum", "col": "amount"}}, "order_by": "created"}},
	{name: "q6-distinct-customers", runs: 10,
		sql: `SELECT status, count(DISTINCT customer_id) FROM orders GROUP BY status`,
		ch:  `SELECT status, uniqExact(customer_id) FROM orders GROUP BY status`,
		col: map[string]any{"table": "orders", "group_by": []string{"status"},
			"aggs": []map[string]any{{"fn": "count", "col": "customer_id", "distinct": true}}}},
	{name: "q4-point-by-id", runs: 2000,
		sql: `SELECT id, customer_id, amount, status FROM orders WHERE id = %d`},
}

func main() {
	system := flag.String("system", "pg", "pg | stask | stask-col | ch")
	target := flag.String("url", "", "pgx URL (pg, stask) or HTTP base URL (stask-col, ch)")
	only := flag.String("only", "", "comma-separated query names (default all)")
	exec := flag.String("exec", "", "run this statement and exit (setup)")
	n := flag.Int64("n", 2_000_000, "rows in the table (point lookups pick ids below)")
	label := flag.String("label", "", "label")
	show := flag.Bool("show", false, "print each query's canonical answer to stderr")
	flag.Parse()
	ctx := context.Background()

	var conn *pgx.Conn
	if *system == "pg" || *system == "stask" {
		var err error
		if conn, err = pgx.Connect(ctx, *target); err != nil {
			log.Fatal(err)
		}
		defer conn.Close(ctx)
	}
	if *exec != "" {
		var err error
		if conn != nil {
			_, err = conn.Exec(ctx, *exec)
		} else {
			_, err = chQuery(*target, *exec)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	// run returns a canonical text of the answer
	run := func(q query, arg int64) (string, error) {
		sql := q.sql
		if strings.Contains(sql, "%d") {
			sql = fmt.Sprintf(sql, arg)
		}
		switch *system {
		case "pg", "stask":
			rows, err := conn.Query(ctx, sql, pgx.QueryExecModeSimpleProtocol)
			if err != nil {
				return "", err
			}
			var out []string
			for rows.Next() {
				vals, err := rows.Values()
				if err != nil {
					return "", err
				}
				out = append(out, canon(vals))
			}
			return strings.Join(out, ";"), rows.Err()
		case "ch":
			if q.ch != "" {
				sql = q.ch
			}
			b, err := chQuery(*target, sql+" FORMAT JSONCompact")
			if err != nil {
				return "", err
			}
			var r struct{ Data [][]any }
			if err := json.Unmarshal(b, &r); err != nil {
				return "", err
			}
			var out []string
			for _, row := range r.Data {
				out = append(out, canon(row))
			}
			return strings.Join(out, ";"), nil
		case "stask-col":
			if q.col == nil {
				return "", errSkip
			}
			body, _ := json.Marshal(map[string]any{"kind": "aggregate", "aggregate": q.col,
				"consistency": map[string]any{"mode": "strong"}})
			resp, err := http.Post(*target+"/v1/query", "application/json", bytes.NewReader(body))
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				return "", fmt.Errorf("%s", b)
			}
			var r struct {
				Result struct{ Rows [][]any }  `json:"result"`
				Plan   struct{ Engine string } `json:"plan"`
			}
			if err := json.Unmarshal(b, &r); err != nil {
				return "", err
			}
			if r.Plan.Engine != "columnar" {
				return "", fmt.Errorf("answered by %q, not the columnar view", r.Plan.Engine)
			}
			var out []string
			for _, row := range r.Result.Rows {
				out = append(out, canon(row))
			}
			return strings.Join(out, ";"), nil
		}
		return "", fmt.Errorf("unknown system %s", *system)
	}

	var report []map[string]any
	rng := rand.New(rand.NewPCG(1, 2))
	for _, q := range queries {
		if *only != "" && !strings.Contains(","+*only+",", ","+q.name+",") {
			continue
		}
		arg := func() int64 { return rng.Int64N(*n) }
		// warm-up: one run (point lookups: 200)
		warm := 1
		if q.runs > 100 {
			warm = 200
		}
		var answer string
		var err error
		for i := 0; i < warm; i++ {
			if answer, err = run(q, arg()); err != nil {
				break
			}
		}
		if err == errSkip {
			continue
		}
		if err != nil {
			report = append(report, map[string]any{"query": q.name, "error": err.Error()})
			log.Printf("%s %s: %v", *system, q.name, err)
			continue
		}
		var lat []float64
		t0 := time.Now()
		for i := 0; i < q.runs; i++ {
			s := time.Now()
			if _, err = run(q, arg()); err != nil {
				break
			}
			lat = append(lat, float64(time.Since(s).Microseconds())/1000)
		}
		wall := time.Since(t0)
		if err != nil {
			report = append(report, map[string]any{"query": q.name, "error": err.Error()})
			continue
		}
		sort.Float64s(lat)
		pct := func(p float64) float64 { return lat[min(len(lat)-1, int(math.Ceil(p*float64(len(lat))))-1)] }
		if *show {
			fmt.Fprintf(os.Stderr, "== %s\n%s\n", q.name, answer)
		}
		sum := fmt.Sprintf("%x", fnv(answer))
		if q.runs > 100 {
			sum = "-" // random ids: no fixed answer
		}
		r := map[string]any{"query": q.name, "runs": len(lat), "min_ms": lat[0], "p50_ms": pct(0.5),
			"p99_ms": pct(0.99), "max_ms": lat[len(lat)-1], "per_s": float64(len(lat)) / wall.Seconds(), "answer": sum}
		if len(answer) < 400 && q.runs <= 100 {
			r["answer_text"] = answer
		}
		report = append(report, r)
		log.Printf("%-10s %-18s p50 %8.2f p99 %8.2f min %8.2f ms  answer %s", *system, q.name, pct(0.5), pct(0.99), lat[0], sum)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"system": *system, "label": *label, "queries": report})
}

var errSkip = fmt.Errorf("skip")

// canon prints values so that the same answer prints the same on every
// system: numbers rounded to cents, everything else as text.
func canon(vals []any) string {
	var parts []string
	for _, v := range vals {
		var f float64
		isNum := true
		switch x := v.(type) {
		case time.Time: // timestamps as epoch milliseconds on every system
			f = float64(x.UnixMilli())
		case int64:
			f = float64(x)
		case int32:
			f = float64(x)
		case uint64:
			f = float64(x)
		case float64:
			f = x
		case json.Number:
			f, _ = x.Float64()
		case string: // ClickHouse quotes 64-bit integers (and dates) in JSON
			if t, err := time.Parse("2006-01-02 15:04:05", x); err == nil {
				f = float64(t.UnixMilli())
			} else if _, err := fmt.Sscan(x, &f); err != nil {
				isNum = false
			}
		default:
			if s := fmt.Sprint(x); s != "" {
				if _, err := fmt.Sscan(s, &f); err != nil {
					isNum = false
				}
			}
		}
		if isNum {
			parts = append(parts, fmt.Sprintf("%.2f", math.Round(f*100)/100))
		} else {
			parts = append(parts, fmt.Sprint(v))
		}
	}
	// group order is not fixed without ORDER BY: callers sort rows
	return strings.Join(parts, ",")
}

func fnv(s string) uint64 {
	rows := strings.Split(s, ";")
	sort.Strings(rows)
	h := uint64(14695981039346656037)
	for _, c := range []byte(strings.Join(rows, ";")) {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

func chQuery(base, q string) ([]byte, error) {
	resp, err := http.Post(base+"/?"+url.Values{"database": {"default"}}.Encode(), "text/plain", strings.NewReader(q))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("clickhouse: %s", bytes.TrimSpace(b))
	}
	return b, nil
}
