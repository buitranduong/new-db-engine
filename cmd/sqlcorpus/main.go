// sqlcorpus measures SQL coverage: every corpus query runs on STASK and on
// the reference system of its dialect (PostgreSQL or ClickHouse), over the
// same generated data, and the canonical answers are compared.
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
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func main() {
	stURL := flag.String("stask", "", "STASK pgx URL")
	pgURL := flag.String("pg", "", "PostgreSQL pgx URL")
	chURL := flag.String("ch", "", "ClickHouse HTTP base URL")
	load := flag.Bool("load", true, "create and fill the tables")
	only := flag.String("only", "", "only queries whose text contains this")
	verbose := flag.Bool("v", false, "print every failure in full")
	flag.Parse()
	ctx := context.Background()
	st, err := pgx.Connect(ctx, *stURL)
	if err != nil {
		log.Fatal(err)
	}
	pg, err := pgx.Connect(ctx, *pgURL)
	if err != nil {
		log.Fatal(err)
	}
	ch := &chClient{base: *chURL}
	if *load {
		for _, s := range ddl("pg") {
			for _, c := range []*pgx.Conn{st, pg} {
				if _, err := c.Exec(ctx, s); err != nil {
					if c == st && strings.HasPrefix(s, "DROP") {
						continue // STASK starts on an empty data directory
					}
					log.Fatalf("%s: %v", s, err)
				}
			}
		}
		for _, s := range ddl("ch") {
			if _, err := ch.query(s); err != nil {
				log.Fatalf("%s: %v", s, err)
			}
		}
		for _, ins := range inserts() {
			for _, c := range []*pgx.Conn{st, pg} {
				if _, err := c.Exec(ctx, ins); err != nil {
					log.Fatalf("insert: %v", err)
				}
			}
			if _, err := ch.query(ins); err != nil {
				log.Fatalf("ch insert: %v", err)
			}
		}
	}
	type res struct {
		Dialect, Cat, SQL, Outcome, Detail string
	}
	var out []res
	stats := map[string][3]int{}
	for _, q := range corpus {
		if *only != "" && !strings.Contains(q.sql, *only) {
			continue
		}
		var want, got []string
		var werr, gerr error
		if q.dialect == "pg" {
			want, werr = pgRows(ctx, pg, q.sql)
		} else {
			want, werr = ch.rows(q.sql)
		}
		// STASK takes both dialects; where they conflict (name
		// resolution) the session's sql_dialect decides
		dialect := map[string]string{"pg": "postgres", "ch": "clickhouse"}[q.dialect]
		if _, err := st.Exec(ctx, "SET sql_dialect = '"+dialect+"'"); err != nil {
			log.Fatal(err)
		}
		got, gerr = pgRows(ctx, st, q.sql)
		r := res{Dialect: q.dialect, Cat: q.cat, SQL: q.sql}
		switch {
		case werr != nil:
			r.Outcome, r.Detail = "ref-error", werr.Error()
		case gerr != nil:
			r.Outcome, r.Detail = "error", gerr.Error()
		default:
			if !q.ord {
				sort.Strings(want)
				sort.Strings(got)
			}
			if strings.Join(want, "\n") == strings.Join(got, "\n") {
				r.Outcome = "pass"
			} else {
				r.Outcome = "wrong"
				r.Detail = fmt.Sprintf("want %.300q\n got %.300q", strings.Join(want, " | "), strings.Join(got, " | "))
			}
		}
		out = append(out, r)
		s := stats[q.dialect+"/"+q.cat]
		switch r.Outcome {
		case "pass":
			s[0]++
		case "ref-error":
		default:
			s[1]++
		}
		s[2]++
		stats[q.dialect+"/"+q.cat] = s
		if r.Outcome != "pass" && *verbose {
			log.Printf("%s %s: %s\n  %s", r.Outcome, q.cat, q.sql, r.Detail)
		}
	}
	var keys []string
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tp, tf := map[string]int{}, map[string]int{}
	fmt.Println("| dialect/category | pass | fail | total |\n|---|---|---|---|")
	for _, k := range keys {
		s := stats[k]
		fmt.Printf("| %s | %d | %d | %d |\n", k, s[0], s[1], s[2])
		d := strings.Split(k, "/")[0]
		tp[d] += s[0]
		tf[d] += s[1]
	}
	for _, d := range []string{"pg", "ch"} {
		if tp[d]+tf[d] > 0 {
			fmt.Printf("| **%s total** | **%d** | **%d** | %.0f%% |\n", d, tp[d], tf[d], 100*float64(tp[d])/float64(tp[d]+tf[d]))
		}
	}
	f, _ := os.Create("sqlcorpus.json")
	json.NewEncoder(f).Encode(out)
	f.Close()
	for _, r := range out {
		if r.Outcome != "pass" {
			d := r.Detail
			if len(d) > 160 && !*verbose {
				d = d[:160]
			}
			fmt.Fprintf(os.Stderr, "%-9s %-11s %s\n          %s\n", r.Outcome, r.Cat, r.SQL, strings.ReplaceAll(d, "\n", " "))
		}
	}
}

func ddl(dialect string) []string {
	if dialect == "pg" {
		return []string{
			`DROP TABLE IF EXISTS customers`, `DROP TABLE IF EXISTS orders`, `DROP TABLE IF EXISTS items`, `DROP TABLE IF EXISTS tmp`,
			`CREATE TABLE customers (id BIGINT PRIMARY KEY, name TEXT, city TEXT, segment TEXT, created TIMESTAMPTZ)`,
			`CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT, amount DOUBLE PRECISION, status TEXT, qty BIGINT, created TIMESTAMPTZ)`,
			`CREATE TABLE items (id BIGINT PRIMARY KEY, order_id BIGINT, product TEXT, price DOUBLE PRECISION)`,
			`CREATE TABLE tmp (id BIGINT PRIMARY KEY, v BIGINT)`,
		}
	}
	return []string{
		`CREATE DATABASE IF NOT EXISTS corpus`,
		`DROP TABLE IF EXISTS customers`, `DROP TABLE IF EXISTS orders`, `DROP TABLE IF EXISTS items`,
		`CREATE TABLE customers (id Int64, name String, city String, segment String, created DateTime64(3, 'UTC')) ENGINE = MergeTree ORDER BY id`,
		`CREATE TABLE orders (id Int64, customer_id Int64, amount Nullable(Float64), status String, qty Int64, created DateTime64(3, 'UTC')) ENGINE = MergeTree ORDER BY id`,
		`CREATE TABLE items (id Int64, order_id Int64, product String, price Float64) ENGINE = MergeTree ORDER BY id`,
	}
}

func inserts() []string {
	ts := func(ms int64) string {
		return "'" + time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05") + "'"
	}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	cities := []string{"hanoi", "hcm", "danang", "hue", "cantho"}
	segs := []string{"smb", "ent", "gov"}
	statuses := []string{"new", "paid", "shipped", "refunded"}
	var out []string
	var b strings.Builder
	b.WriteString("INSERT INTO customers (id, name, city, segment, created) VALUES ")
	for i := int64(1); i <= 200; i++ {
		if i > 1 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d, 'cust %d', '%s', '%s', %s)", i, i, cities[i%5], segs[(i*7)%3], ts(base+i*3600_000))
	}
	out = append(out, b.String())
	for lo := int64(1); lo <= 5000; lo += 1000 {
		b.Reset()
		b.WriteString("INSERT INTO orders (id, customer_id, amount, status, qty, created) VALUES ")
		for i := lo; i < lo+1000; i++ {
			if i > lo {
				b.WriteString(", ")
			}
			amount := "NULL"
			if i%23 != 0 {
				amount = strconv.FormatFloat(float64((i*7919)%100000)/100, 'f', 2, 64)
			}
			fmt.Fprintf(&b, "(%d, %d, %s, '%s', %d, %s)", i, (i*37)%200+1, amount, statuses[(i*13)%4], (i*31)%5+1, ts(base+i*137_000))
		}
		out = append(out, b.String())
	}
	for lo := int64(1); lo <= 12000; lo += 2000 {
		b.Reset()
		b.WriteString("INSERT INTO items (id, order_id, product, price) VALUES ")
		for i := lo; i < lo+2000; i++ {
			if i > lo {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "(%d, %d, 'p%d', %s)", i, (i*7)%5000+1, i%50, strconv.FormatFloat(float64(i%997)/10, 'f', 1, 64))
		}
		out = append(out, b.String())
	}
	return out
}

func pgRows(ctx context.Context, c *pgx.Conn, q string) ([]string, error) {
	rows, err := c.Query(ctx, q, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, canonRow(vals))
	}
	return out, rows.Err()
}

type chClient struct{ base string }

func (c *chClient) query(q string) ([]byte, error) {
	v := url.Values{"database": {"corpus"}, "join_use_nulls": {"1"}}
	if strings.HasPrefix(q, "CREATE DATABASE") {
		v.Del("database")
	}
	resp, err := http.Post(c.base+"/?"+v.Encode(), "text/plain", strings.NewReader(q))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("clickhouse: %.300s", bytes.TrimSpace(b))
	}
	return b, nil
}

func (c *chClient) rows(q string) ([]string, error) {
	b, err := c.query(q + " FORMAT JSONCompact")
	if err != nil {
		return nil, err
	}
	var r struct{ Data [][]any }
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&r); err != nil {
		return nil, err
	}
	var out []string
	for _, row := range r.Data {
		out = append(out, canonRow(row))
	}
	return out, nil
}

func canonRow(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = canon(v)
	}
	return strings.Join(parts, ",")
}

var timeLayouts = []string{"2006-01-02 15:04:05.999999999", "2006-01-02"}

// canon prints a value so that equal answers print equally on every
// system: numbers rounded to 4 decimals, times as epoch milliseconds,
// booleans as 1/0.
func canon(v any) string {
	num := func(f float64) string {
		if math.IsNaN(f) {
			return "NaN"
		}
		f = math.Round(f*1e4) / 1e4
		if f == math.Trunc(f) && math.Abs(f) < 1e15 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', 4, 64)
	}
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int64:
		return num(float64(x))
	case int32:
		return num(float64(x))
	case int16:
		return num(float64(x))
	case int8:
		return num(float64(x))
	case uint64:
		return num(float64(x))
	case uint32:
		return num(float64(x))
	case uint8:
		return num(float64(x))
	case float64:
		return num(x)
	case float32:
		return num(float64(x))
	case json.Number:
		f, _ := x.Float64()
		return num(f)
	case time.Time:
		return num(float64(x.UnixMilli()))
	case pgtype.Numeric:
		f, err := x.Float64Value()
		if err != nil || !f.Valid {
			return "NULL"
		}
		return num(f.Float64)
	case pgtype.Interval:
		return fmt.Sprintf("interval(%dmo %dd %dus)", x.Months, x.Days, x.Microseconds)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = canon(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		for _, l := range timeLayouts {
			if t, err := time.Parse(l, x); err == nil {
				return num(float64(t.UnixMilli()))
			}
		}
		if f, err := strconv.ParseFloat(x, 64); err == nil && strings.TrimSpace(x) == x && x != "" {
			return num(f)
		}
		return strconv.Quote(x)
	}
	return fmt.Sprintf("%T:%v", v, v)
}
