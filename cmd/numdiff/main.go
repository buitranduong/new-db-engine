// numdiff compares aggregates over numeric columns between PostgreSQL 16,
// a STASK columnar table and a STASK row table holding the same random
// data (NULLs, negatives, large values, several scales). Task colnumeric.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var rng *rand.Rand

func dec(intDigits, scale int) string {
	var sb strings.Builder
	if rng.Intn(3) == 0 {
		sb.WriteByte('-')
	}
	nd := 1 + rng.Intn(max(intDigits, 1))
	for i := 0; i < nd; i++ {
		d := rng.Intn(10)
		if i == 0 && nd > 1 && d == 0 {
			d = 1
		}
		sb.WriteByte(byte('0' + d))
	}
	if scale > 0 {
		sb.WriteByte('.')
		for i := 0; i < scale; i++ {
			sb.WriteByte(byte('0' + rng.Intn(10)))
		}
	}
	return sb.String()
}

type col struct {
	name       string
	intDigits  int
	scale      int
	nullable   bool
	constScale func() int
}

var cols = []col{
	{"p", 10, 2, true, nil}, {"q", 14, 4, true, nil}, {"r", 5, 0, true, nil}, {"big", 18, 0, true, nil}, {"c", 0, 6, true, nil},
}

func lit(c col) string {
	if c.name == "c" {
		return "0." + fmt.Sprintf("%06d", rng.Intn(1000000))
	}
	return dec(c.intDigits, c.scale)
}

func qconst(c col) string {
	switch rng.Intn(5) {
	case 0:
		return fmt.Sprint(rng.Intn(200) - 100)
	case 1: // more fractional digits than the scale
		return dec(max(c.intDigits-2, 1), c.scale+1+rng.Intn(3))
	case 2: // fewer
		return dec(max(c.intDigits-2, 1), rng.Intn(c.scale+1))
	case 3: // beyond the column's range
		return dec(25, rng.Intn(3))
	}
	return lit(c)
}

func genQuery(tbl string) string {
	groups := [][]string{nil, {"g"}, {"p"}, {"g", "r"}, {"r"}, {"g", "p"}, {"c"}}[rng.Intn(7)]
	var sel []string
	for _, g := range groups {
		if g == "g" {
			sel = append(sel, "g")
		} else {
			sel = append(sel, g+"::text AS "+g+"_t")
		}
	}
	sel = append(sel, "count(*)")
	for i := 0; i < 1+rng.Intn(4); i++ {
		c := cols[rng.Intn(len(cols))]
		fn := []string{"sum", "avg", "min", "max", "count"}[rng.Intn(5)]
		if fn == "count" {
			sel = append(sel, fmt.Sprintf("count(%s)", c.name))
		} else {
			sel = append(sel, fmt.Sprintf("%s(%s)::text", fn, c.name))
		}
	}
	var conds []string
	for i := 0; i < rng.Intn(3); i++ {
		c := cols[rng.Intn(len(cols))]
		switch rng.Intn(7) {
		case 0:
			conds = append(conds, fmt.Sprintf("%s IS NULL", c.name))
		case 1:
			conds = append(conds, fmt.Sprintf("%s IS NOT NULL", c.name))
		case 2:
			conds = append(conds, fmt.Sprintf("%s IN (%s, %s, %s)", c.name, qconst(c), qconst(c), lit(c)))
		case 3:
			conds = append(conds, fmt.Sprintf("%s BETWEEN %s AND %s", c.name, qconst(c), qconst(c)))
		case 4:
			conds = append(conds, fmt.Sprintf("(%s < %s OR %s >= %s)", c.name, qconst(c), c.name, qconst(c)))
		case 5:
			conds = append(conds, fmt.Sprintf("g <> 'g%d'", rng.Intn(6)))
		default:
			op := []string{"=", "<>", "<", "<=", ">", ">="}[rng.Intn(6)]
			if rng.Intn(2) == 0 {
				conds = append(conds, fmt.Sprintf("%s %s %s", c.name, op, qconst(c)))
			} else {
				conds = append(conds, fmt.Sprintf("%s %s %s", qconst(c), op, c.name))
			}
		}
	}
	q := "SELECT " + strings.Join(sel, ", ") + " FROM " + tbl
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	if len(groups) > 0 {
		q += " GROUP BY " + strings.Join(groups, ", ")
		q += " ORDER BY " + strings.Join(groups, ", ")
		if groups[0] != "g" && rng.Intn(3) == 0 {
			q += " NULLS FIRST"
		}
	}
	return q
}

func run(ctx context.Context, c *pgx.Conn, q string) string {
	rows, err := c.Query(ctx, q, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return errText(err)
	}
	var sb strings.Builder
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return errText(err)
		}
		for i, v := range vals {
			if i > 0 {
				sb.WriteString("|")
			}
			if v == nil {
				sb.WriteString("NULL")
			} else {
				fmt.Fprint(&sb, v)
			}
		}
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return errText(err)
	}
	return sb.String()
}

func errText(err error) string {
	if pe, ok := err.(*pgconn.PgError); ok {
		return "ERR " + pe.Code
	}
	return "ERR " + err.Error()
}

func main() {
	pgDSN := flag.String("pg", "", "PostgreSQL DSN")
	stDSN := flag.String("stask", "", "STASK DSN")
	n := flag.Int("n", 20000, "rows")
	queries := flag.Int("queries", 600, "random queries")
	seed := flag.Int64("seed", 1, "seed")
	nan := flag.Bool("nan", false, "put a NaN into p")
	flag.Parse()
	rng = rand.New(rand.NewSource(*seed))
	ctx := context.Background()
	pg, err := pgx.Connect(ctx, *pgDSN)
	if err != nil {
		panic(err)
	}
	st, err := pgx.Connect(ctx, *stDSN)
	if err != nil {
		panic(err)
	}
	ddl := func(c *pgx.Conn, name, with string) {
		c.Exec(ctx, "DROP TABLE IF EXISTS "+name)
		if _, err := c.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id bigint PRIMARY KEY, g text, p numeric(12,2), q numeric(18,4), r numeric(5,0),
			big numeric(18,0), c numeric(6,6), u numeric)%s`, name, with)); err != nil {
			panic(err)
		}
	}
	ddl(pg, "m", "")
	ddl(st, "m_col", " WITH (columnar = true)")
	ddl(st, "m_row", "")
	nullable := func(s string) string {
		if rng.Intn(20) == 0 {
			return "NULL"
		}
		return "'" + s + "'"
	}
	for b := 0; b < *n/500; b++ {
		var vals []string
		for i := 0; i < 500; i++ {
			id := b*500 + i
			p := lit(cols[0])
			if rng.Intn(4) == 0 {
				p = fmt.Sprintf("%d.%02d", rng.Intn(40), rng.Intn(100)) // few distinct values: group by them
			}
			vals = append(vals, fmt.Sprintf("(%d,'g%d',%s,%s,%s,%s,%s,%s)", id, rng.Intn(6), nullable(p), nullable(lit(cols[1])),
				nullable(lit(cols[2])), nullable(lit(cols[3])), nullable(lit(cols[4])), nullable(dec(20, rng.Intn(5)))))
		}
		ins := " VALUES " + strings.Join(vals, ",")
		for _, t := range []struct {
			c *pgx.Conn
			n string
		}{{pg, "m"}, {st, "m_col"}, {st, "m_row"}} {
			if _, err := t.c.Exec(ctx, "INSERT INTO "+t.n+ins); err != nil {
				fmt.Println("insert", t.n, err)
				os.Exit(1)
			}
		}
	}
	both := func(sql string) {
		for _, t := range []struct {
			c *pgx.Conn
			n string
		}{{pg, "m"}, {st, "m_col"}, {st, "m_row"}} {
			if _, err := t.c.Exec(ctx, strings.ReplaceAll(sql, "TBL", t.n)); err != nil {
				fmt.Println("exec", t.n, sql, err)
				os.Exit(1)
			}
		}
	}
	both("UPDATE TBL SET p = p + 1.5 WHERE id % 7 = 0")
	both("UPDATE TBL SET q = q * 2 WHERE id % 13 = 0 AND abs(q) < 1000000")
	both("DELETE FROM TBL WHERE id % 11 = 0")
	if *nan {
		both("UPDATE TBL SET p = 'NaN' WHERE id = 77")
	}
	// out-of-range values are rejected alike
	for _, bad := range []string{"INSERT INTO TBL (id, p) VALUES (-5, 99999999999.99)", "INSERT INTO TBL (id, r) VALUES (-5, 123456)",
		"INSERT INTO TBL (id, c) VALUES (-5, 1.5)", "INSERT INTO TBL (id, p) VALUES (-5, 'abc')"} {
		var out []string
		for _, t := range []struct {
			c *pgx.Conn
			n string
		}{{pg, "m"}, {st, "m_col"}, {st, "m_row"}} {
			_, err := t.c.Exec(ctx, strings.ReplaceAll(bad, "TBL", t.n))
			out = append(out, errText(err))
		}
		if out[0] != out[1] || out[0] != out[2] {
			fmt.Printf("DIFF %s: %v\n", bad, out)
		} else {
			fmt.Printf("same %s: %s\n", bad, out[0])
		}
	}
	diff, same, errs := 0, 0, 0
	for i := 0; i < *queries; i++ {
		q := genQuery("TBL")
		a := run(ctx, pg, strings.ReplaceAll(q, "TBL", "m"))
		b := run(ctx, st, strings.ReplaceAll(q, "TBL", "m_col"))
		c := run(ctx, st, strings.ReplaceAll(q, "TBL", "m_row"))
		if strings.HasPrefix(a, "ERR") {
			errs++
		}
		if a != b || a != c {
			diff++
			if diff <= 8 {
				fmt.Printf("DIFF %s\n  pg   %q\n  col  %q\n  row  %q\n", q, trunc(a), trunc(b), trunc(c))
			}
		} else {
			same++
		}
	}
	fmt.Printf("queries %d: identical %d, different %d (PG errors %d)\n", *queries, same, diff, errs)
	if diff > 0 {
		os.Exit(1)
	}
}

func trunc(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
