package sql

import (
	"fmt"
	"strings"
	"testing"
)

// The differential test of the primary-key range scans (rangepath.go, docs/14 row "rangefix"): every statement
// of a corpus runs twice in a transaction that is rolled back, once with the range scans (the default) and
// once with rangeScanOff (the full scans of before), as plain statements and after the transaction has
// written to the table itself (the INSERT .. SELECT of an import batch reads what its UPDATE wrote). Results
// and affected row counts, and the table after the statement, must be equal.

type rangeTable struct {
	name   string
	cols   string   // "k, v": the columns the statements read
	key    string   // the column a statement orders by
	create []string // DDL and data
	preds  []string // WHERE conjunctions; '@' stands for the table qualifier ("" or "a.")
	own    []string // statements that make the transaction write to the table first
}

func rangeTables() []rangeTable {
	texts := []string{"", " ", "0", "09", "090", "0900", "0a", "A", "AB", "Z", "a", "a%", "a_", "ab", "abc", "abd", "abe", "abz",
		"b", "é", "éa", "ÿ", "~", "z", "zz", "a\\b", "ab\xc3\xa9"}
	var tv []string
	for i, k := range texts {
		tv = append(tv, fmt.Sprintf("('%s', %d, 'w%d')", strings.ReplaceAll(k, "'", "''"), i%7, i))
	}
	for i := 1; i <= 120; i++ {
		tv = append(tv, fmt.Sprintf("('n%d', %d, 'w')", i, i%9))
	}
	txtPreds := []string{}
	vals := []string{"''", "'a'", "'ab'", "'abc'", "'b'", "'é'", "'n5'", "'n50'", "'zz'", "'~'", "'nope'", "'09'", "'A'", "'n119'"}
	for _, v := range vals {
		for _, op := range []string{">=", ">", "<=", "<"} {
			txtPreds = append(txtPreds, fmt.Sprintf("@k %s %s", op, v))
		}
		txtPreds = append(txtPreds, fmt.Sprintf("%s <= @k", v), fmt.Sprintf("%s > @k", v))
	}
	pairs := [][2]string{{"'a'", "'b'"}, {"'ab'", "'abd'"}, {"'n1'", "'n2'"}, {"'n5'", "'n50'"}, {"'b'", "'a'"}, {"''", "'a'"}, {"'09'", "'0900'"},
		{"'é'", "'ÿ'"}, {"'z'", "'zz'"}, {"'abc'", "'abc'"}}
	for _, p := range pairs {
		txtPreds = append(txtPreds,
			fmt.Sprintf("@k BETWEEN %s AND %s", p[0], p[1]),
			fmt.Sprintf("@k NOT BETWEEN %s AND %s", p[0], p[1]),
			fmt.Sprintf("@k >= %s AND @k < %s", p[0], p[1]),
			fmt.Sprintf("@k > %s AND @k <= %s", p[0], p[1]),
			fmt.Sprintf("@k >= %s AND @k < %s AND @v > 2", p[0], p[1]),
			fmt.Sprintf("@k < %s OR @k > %s", p[0], p[1]))
	}
	for _, l := range []string{"'a%'", "'ab%'", "'abc%'", "'n1%'", "'n%5'", "'%'", "''", "'a_'", "'a%b'", "'é%'", "'n1_'", "'z%'", "'~%'", "'ab'", "'a\\%%'"} {
		txtPreds = append(txtPreds, fmt.Sprintf("@k LIKE %s", l), fmt.Sprintf("@k NOT LIKE %s", l), fmt.Sprintf("@k ILIKE %s", l))
	}
	txtPreds = append(txtPreds,
		"@k >= NULL", "@k < NULL", "@k BETWEEN NULL AND 'b'", "@k > $1", "@k >= $1 AND @k < $2", "@k LIKE $1 || '%'", "@k >= $1 AND @v = $3",
		"@k = 'ab'", "@k IN ('a', 'ab', 'zz')", "@k >= 'a' AND @k = 'ab'", "@k > 'a' AND @k IN ('ab','b','zzz')",
		"@k >= 'a' AND @k < 'n3' AND @k <> 'ab'", "@k >= 'n1' AND @k < 'n2' AND @v IN (SELECT 1 UNION SELECT 3)",
		"@k >= 'a' COLLATE \"C\" AND @k < 'b'", "@k::text >= 'a' AND @k < 'b'", "lower(@k) >= 'a' AND @k < 'n3'",
		"@k >= 'n1' AND @k < 'n2' AND 1/(@v - 3) > 0", "NOT (@k < 'n5')", "NOT (@k >= 'n5' AND @k < 'n7')",
		"true", "false", "@k >= 'a' AND false", "@k > 'zzzz'", "@k < ''", "@k <= ''")
	own := []string{
		"UPDATE %s SET v = v + 100 WHERE k < 'n3'", "DELETE FROM %s WHERE k = 'ab' OR k = 'n20'",
		"INSERT INTO %s VALUES ('abb', 5, 'own'), ('n200', 1, 'own'), ('', 9, 'own'), ('zzz', 2, 'own')",
		"UPDATE %s SET k = 'n61x' WHERE k = 'n61'",
	}

	var iv []string
	add := func(a, b int64) {
		for i := a; i <= b; i++ {
			iv = append(iv, fmt.Sprintf("(%d, %d)", i, i%5))
		}
	}
	add(-5, 120)
	add(995, 1005)
	add(9990, 10010)
	add(99990, 100010)
	iv = append(iv, "(9223372036854775807, 1)", "(-9223372036854775808, 2)", "(9223372036854775800, 3)")
	var intPreds []string
	ivals := []string{"0", "5", "9", "10", "99", "100", "995", "1000", "1005", "9990", "10000", "10010", "-3", "-1", "99990", "100000", "100010", "9223372036854775807", "-9223372036854775808", "7"}
	for _, a := range ivals {
		for _, op := range []string{">=", ">", "<=", "<"} {
			intPreds = append(intPreds, fmt.Sprintf("@id %s %s", op, a))
		}
	}
	for _, a := range []string{"0", "10", "100", "995", "1000", "9990", "10000", "99990", "-3"} {
		for _, b := range []string{"5", "9", "99", "999", "1005", "10010", "100010", "-1", "9223372036854775807"} {
			intPreds = append(intPreds, fmt.Sprintf("@id >= %s AND @id < %s", a, b), fmt.Sprintf("@id BETWEEN %s AND %s", a, b),
				fmt.Sprintf("@id > %s AND @id <= %s", a, b))
		}
	}
	intPreds = append(intPreds, "@id >= 1.5 AND @id < 99.5", "@id > $4 AND @id <= $5", "@id BETWEEN $4 AND $5", "@id >= NULL", "@id >= 10 AND @id < 5",
		"@id >= 10 AND @id < 99 AND @v = 2", "@id < 100 OR @id > 99990", "@id >= '1000' AND @id < '1010'", "@id >= 1000 AND @id < 1010 AND @id <> 1005")
	intOwn := []string{"UPDATE %s SET v = v + 100 WHERE id < 20", "DELETE FROM %s WHERE id = 1000 OR id = 10", "INSERT INTO %s VALUES (1001000, 5), (1003, 4), (12, 1)"}

	var c1 []string
	for a := 1; a <= 5; a++ {
		for _, b := range []string{"", "x", "xy", "y", "z"} {
			for c := 1; c <= 4; c++ {
				c1 = append(c1, fmt.Sprintf("(%d, '%s', %d, %d)", a, b, c, (a+c)%6))
			}
		}
	}
	c1p := []string{"@a = 2 AND @b >= 'x'", "@a = 2 AND @b > 'x' AND @b < 'z'", "@a >= 2 AND @a < 4", "@a BETWEEN 2 AND 3", "@a = 2 AND @b = 'y' AND @c > 2",
		"@a = 2 AND @b = 'y' AND @c BETWEEN 2 AND 3", "@a > 3", "@a <= 2", "@a < 1", "@a = 1 AND @b LIKE 'x%'", "@a = 3", "@a = 3 AND @b = 'xy'",
		"@a >= 2 AND @b = 'x'", "@a = 2 AND @b <= 'x'", "@a = 2 AND @b < ''", "@a = 2 AND @b > 'z'", "@a > 2 AND @a < 3", "@a = 2 AND @b >= $1 AND @b < $2",
		"@a = 5 AND @b = 'z' AND @c >= 4", "@a = 5 AND @b = 'z' AND @c > 4", "@a = 1 AND @b = '' AND @c <= 1", "@a >= 4", "@a = 2 AND @c = 3",
		"@a = 2 AND @b IN ('x', 'y') AND @c < 3", "@a BETWEEN 2 AND 3 AND @b = 'x'", "@a = 2 AND @b >= NULL", "@a = 2 OR @a = 4", "@a < 3 OR @b = 'z'",
		"@a = 1 AND @b >= 'xy' AND @b <= 'xy'", "@a >= 1 AND @a <= 5 AND @v = 3", "@a = 2 AND @b >= 'x' AND @b < 'xz'"}
	c1own := []string{"UPDATE %s SET v = v + 100 WHERE a = 2", "DELETE FROM %s WHERE a = 3 AND b = 'x'", "INSERT INTO %s VALUES (2, 'xa', 1, 9), (6, 'a', 1, 1), (0, 'a', 1, 1)"}

	var c2 []string
	for _, a := range []string{"", "p", "pq", "q", "qp", "é"} {
		for b := -2; b <= 5; b++ {
			c2 = append(c2, fmt.Sprintf("('%s', %d, %d)", a, b, (b+3)%4))
		}
	}
	c2p := []string{"@a >= 'p'", "@a > 'p' AND @a < 'q'", "@a LIKE 'p%'", "@a = 'p' AND @b > 0", "@a = 'p' AND @b BETWEEN -1 AND 2", "@a = 'pq'", "@a < 'q'",
		"@a >= '' AND @a <= ''", "@a = '' AND @b >= 4", "@a > 'q' AND @b = 1", "@a BETWEEN 'p' AND 'pq' AND @b < 0", "@a = 'p' AND @b >= -2 AND @b < -1",
		"@a LIKE 'p%' AND @b = 3", "@a >= 'é'", "@a = 'q' AND @b < -2", "@a = 'q' AND @b <= -2", "@a >= 'p' AND @a < 'pr' AND @b > 3"}
	c2own := []string{"UPDATE %s SET v = v + 100 WHERE a = 'p'", "DELETE FROM %s WHERE a = 'pq' AND b = 1", "INSERT INTO %s VALUES ('pa', 1, 1), ('', 9, 9), ('r', 0, 0)"}

	var xv []string
	for i := 1; i <= 90; i++ {
		xv = append(xv, fmt.Sprintf("('x%02d', %d, %d)", i, i%4, i))
	}
	xp := []string{"@k >= 'x10' AND @k < 'x20'", "@k >= 'x10' AND @k < 'x20' AND @c = 1", "@k >= 'x10' AND @c = 1", "@k < 'x30' AND @c = 2", "@c = 3 AND @k > 'x80'",
		"@k LIKE 'x1%' AND @c IN (1, 2)", "@c BETWEEN 1 AND 2 AND @k BETWEEN 'x40' AND 'x45'", "@k > 'x85' OR @c = 0", "@k >= 'x10' AND @c IS NULL", "@k <= 'x03'"}
	xown := []string{"UPDATE %s SET c = c + 1 WHERE k < 'x15'", "DELETE FROM %s WHERE k = 'x12'", "INSERT INTO %s VALUES ('x115', 1, 1), ('x11a', 2, 2)"}

	return []rangeTable{
		{name: "rt_txt", cols: "k, v", key: "k", preds: txtPreds, own: own, create: []string{
			"CREATE TABLE rt_txt (k TEXT PRIMARY KEY, v INT, w TEXT)", "INSERT INTO rt_txt VALUES " + strings.Join(tv, ", "),
			"CREATE TABLE rt_txt_c (k TEXT PRIMARY KEY, v INT, w TEXT)"}},
		{name: "rt_var", cols: "k, v", key: "k", preds: txtPreds[:60], own: own, create: []string{
			"CREATE TABLE rt_var (k VARCHAR(20) PRIMARY KEY, v INT, w TEXT)", "INSERT INTO rt_var SELECT * FROM rt_txt WHERE length(k) <= 20",
			"CREATE TABLE rt_var_c (k VARCHAR(20) PRIMARY KEY, v INT, w TEXT)"}},
		{name: "rt_int", cols: "id, v", key: "id", preds: intPreds, own: intOwn, create: []string{
			"CREATE TABLE rt_int (id BIGINT PRIMARY KEY, v INT)", "INSERT INTO rt_int VALUES " + strings.Join(iv, ", "),
			"CREATE TABLE rt_int_c (id BIGINT PRIMARY KEY, v INT)"}},
		{name: "rt_c1", cols: "a, b, c, v", key: "a, b, c", preds: c1p, own: c1own, create: []string{
			"CREATE TABLE rt_c1 (a INT, b TEXT, c INT, v INT, PRIMARY KEY (a, b, c))", "INSERT INTO rt_c1 VALUES " + strings.Join(c1, ", "),
			"CREATE TABLE rt_c1_c (a INT, b TEXT, c INT, v INT, PRIMARY KEY (a, b, c))"}},
		{name: "rt_c2", cols: "a, b, v", key: "a, b", preds: c2p, own: c2own, create: []string{
			"CREATE TABLE rt_c2 (a TEXT, b INT, v INT, PRIMARY KEY (a, b))", "INSERT INTO rt_c2 VALUES " + strings.Join(c2, ", "),
			"CREATE TABLE rt_c2_c (a TEXT, b INT, v INT, PRIMARY KEY (a, b))"}},
		{name: "rt_idx", cols: "k, c, v", key: "k", preds: xp, own: xown, create: []string{
			"CREATE TABLE rt_idx (k TEXT PRIMARY KEY, c INT, v INT)", "CREATE INDEX rt_idx_c ON rt_idx (c)", "INSERT INTO rt_idx VALUES " + strings.Join(xv, ", "),
			"CREATE TABLE rt_idx_c (k TEXT PRIMARY KEY, c INT, v INT)"}},
	}
}

// rangeCase runs prefix statements, then q, then the table checks, in one transaction that is rolled back.
func rangeCase(s *Session, tb rangeTable, prefix []string, q string, ps []Datum) string {
	var out []string
	if _, err := tryRun(s, "BEGIN"); err != nil {
		return "BEGIN " + err.Error()
	}
	defer tryRun(s, "ROLLBACK")
	for _, p := range prefix {
		if _, err := tryRun(s, fmt.Sprintf(p, tb.name)); err != nil {
			return "PREFIX ERR " + err.Error()
		}
	}
	n := 0
	if strings.Contains(q, "$") {
		n = len(ps)
	}
	res, err := tryRun(s, q, ps[:n]...)
	ordered := strings.Contains(q, "ORDER BY")
	out = append(out, resultText(res, err, ordered))
	if res != nil {
		out = append(out, "tag "+res.Tag)
	}
	if err != nil {
		// a failed statement aborts the transaction: nothing more to compare
		return strings.Join(out, "\n")
	}
	for _, t := range []string{tb.name, tb.name + "_c"} {
		r2, e2 := tryRun(s, fmt.Sprintf("SELECT * FROM %s ORDER BY %s", t, tb.key), nil...)
		out = append(out, t+": "+resultText(r2, e2, true))
	}
	return strings.Join(out, "\n")
}

func rangeStatements(tb rangeTable, pred string) []string {
	p := strings.ReplaceAll(pred, "@", "")
	pa := strings.ReplaceAll(pred, "@", "a.")
	first := strings.Split(tb.key, ", ")[0]
	vcol := "v"
	return []string{
		fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s", tb.cols, tb.name, p, tb.key),
		fmt.Sprintf("SELECT count(*), sum(%s), min(%s), max(%s) FROM %s WHERE %s", vcol, first, first, tb.name, p),
		fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT 3 OFFSET 1", tb.cols, tb.name, p, tb.key),
		fmt.Sprintf("SELECT count(*) FROM (SELECT %s FROM %s WHERE %s LIMIT 2) q", first, tb.name, p),
		fmt.Sprintf("UPDATE %s SET %s = %s + 1 WHERE %s", tb.name, vcol, vcol, p),
		fmt.Sprintf("DELETE FROM %s WHERE %s RETURNING %s", tb.name, p, first),
		fmt.Sprintf("INSERT INTO %s_c SELECT * FROM %s WHERE %s", tb.name, tb.name, p),
		fmt.Sprintf("UPDATE %s SET %s = -1 WHERE %s AND %s IN (SELECT 1 UNION SELECT 2 UNION SELECT 3)", tb.name, vcol, p, vcol),
		fmt.Sprintf("SELECT a.%s, b.%s FROM %s a JOIN %s b ON a.%s = b.%s WHERE %s ORDER BY 1, 2", first, vcol, tb.name, tb.name, first, first, pa),
		fmt.Sprintf("SELECT a.%s, b.%s FROM %s a LEFT JOIN %s b ON a.%s = b.%s AND %s ORDER BY 1, 2", first, vcol, tb.name, tb.name, first, first, strings.ReplaceAll(pred, "@", "b.")),
	}
}

func TestRangeScanDifferential(t *testing.T) {
	s := NewSession(newEngine(t))
	defer rangeScanOff.Store(false)
	tbs := rangeTables()
	for _, tb := range tbs {
		for _, q := range tb.create {
			if _, err := tryRun(s, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	params := []Datum{TextD("ab"), TextD("b"), IntD(3), IntD(990), IntD(10005)}
	scans0, pkr0 := rangeStats.scans.Load(), joinStats.pkrange.Load()
	cases := 0
	for _, tb := range tbs {
		for _, pred := range tb.preds {
			for _, q := range rangeStatements(tb, pred) {
				for _, prefix := range [][]string{nil, tb.own} {
					var got [2]string
					for i, off := range []bool{true, false} {
						rangeScanOff.Store(off)
						got[i] = rangeCase(s, tb, prefix, q, params)
					}
					cases++
					if got[0] != got[1] {
						t.Errorf("results differ for %q (own writes first: %v)\n--- full scan\n%.400s\n--- range scan\n%.400s", q, prefix != nil, got[0], got[1])
					}
				}
			}
		}
	}
	rangeScanOff.Store(false)
	t.Logf("%d statement runs compared; range scans %d, pushed pkrange %d", cases, rangeStats.scans.Load()-scans0, joinStats.pkrange.Load()-pkr0)
	if rangeStats.scans.Load() == scans0 || joinStats.pkrange.Load() == pkr0 {
		t.Errorf("a range path never ran: the corpus does not cover it")
	}
}

func TestRangeScanExplain(t *testing.T) {
	s := NewSession(newEngine(t))
	for _, q := range []string{
		"CREATE TABLE r1 (k TEXT PRIMARY KEY, v INT)", "CREATE TABLE r2 (a INT, b TEXT, v INT, PRIMARY KEY (a, b))", "CREATE TABLE r3 (id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO r1 SELECT 'n' || i, i FROM generate_series(1, 50) i", "CREATE TABLE r1h (k TEXT PRIMARY KEY, v INT)"} {
		run(t, s, q)
	}
	for _, c := range []struct{ q, want string }{
		{"UPDATE r1 SET v = 1 WHERE k >= 'n1' AND k < 'n2'", "Index Scan using r1_pkey"},
		{"DELETE FROM r1 WHERE k LIKE 'n1%'", "Index Scan using r1_pkey"},
		{"INSERT INTO r1h SELECT * FROM r1 WHERE k >= 'n1' AND k < 'n2'", "Index Scan using r1_pkey"},
		{"SELECT count(*) FROM r1 WHERE k BETWEEN 'n1' AND 'n3'", "Index Scan using r1_pkey"},
		{"UPDATE r2 SET v = 1 WHERE a = 1 AND b >= 'x'", "Index Scan using r2_pkey"},
		{"UPDATE r2 SET v = 1 WHERE a >= 1 AND a < 3", "Index Scan using r2_pkey"},
		{"UPDATE r3 SET v = 1 WHERE id >= 1000 AND id < 2000", "Index Scan using r3_pkey"},
		{"UPDATE r3 SET v = 1 WHERE id >= 9 AND id < 20000", "Seq Scan on r3"},
		{"UPDATE r1 SET v = 1 WHERE v > 3", "Seq Scan on r1"},
	} {
		res := run(t, s, "EXPLAIN "+c.q)
		if text := render(res); !strings.Contains(text, c.want) {
			t.Errorf("%s\nwant %q in\n%s", c.q, c.want, text)
		}
	}
}

// the rows a range scan returns are the rows of the statement's snapshot plus its own writes: a concurrent
// committer is not seen, the transaction's own insert, update and delete are
func TestRangeScanSeesOwnWrites(t *testing.T) {
	s := NewSession(newEngine(t))
	run(t, s, "CREATE TABLE o (k TEXT PRIMARY KEY, v INT)")
	run(t, s, "INSERT INTO o SELECT 'k' || lpad(i::text, 3, '0'), i FROM generate_series(1, 100) i")
	run(t, s, "BEGIN")
	run(t, s, "UPDATE o SET v = v + 1000 WHERE k >= 'k010' AND k < 'k020'")
	run(t, s, "DELETE FROM o WHERE k = 'k015'")
	run(t, s, "INSERT INTO o VALUES ('k0155', 5), ('k011a', 6)")
	res := run(t, s, "SELECT k, v FROM o WHERE k >= 'k010' AND k < 'k020' ORDER BY k")
	want := "k010,1010;k011,1011;k011a,6;k012,1012;k013,1013;k014,1014;k0155,5;k016,1016;k017,1017;k018,1018;k019,1019"
	if got := render(res); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	run(t, s, "INSERT INTO o SELECT k || 'h', v FROM o WHERE k >= 'k010' AND k < 'k012'")
	res = run(t, s, "SELECT count(*) FROM o WHERE k >= 'k010' AND k < 'k012x'")
	if got := render(res); got != "7" {
		t.Errorf("count after INSERT..SELECT = %s, want 7 (k010 k010h k011 k011a k011ah k011h k012)", got)
	}
	run(t, s, "ROLLBACK")
}
