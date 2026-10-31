package pgwire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"stask/internal/sql"
)

func copyConn(t *testing.T) (context.Context, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, startServer(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	return ctx, conn
}

func copyIn(ctx context.Context, conn *pgx.Conn, q, data string) (string, error) {
	tag, err := conn.PgConn().CopyFrom(ctx, strings.NewReader(data), q)
	return tag.String(), err
}

func copyOut(t *testing.T, ctx context.Context, conn *pgx.Conn, q string) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := conn.PgConn().CopyTo(ctx, &b, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return b.String()
}

func count(t *testing.T, ctx context.Context, conn *pgx.Conn, table string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCopyTextRoundTrip(t *testing.T) {
	ctx, conn := copyConn(t)
	conn.Exec(ctx, `CREATE TABLE c1 (id BIGINT PRIMARY KEY, name TEXT, amt DOUBLE PRECISION, ok BOOLEAN, at TIMESTAMPTZ)`)
	in := "1\tplain\t1.5\tt\t2025-03-04 05:06:07+00\n" +
		"2\ttab\\there\\nnl\\\\bs\t\\N\tf\t\\N\n" +
		"3\t\\N\t2\t\\N\t\\N\n" +
		"4\t\\\\N\t3\tt\t\\N\n" + // the two characters \N as text
		"5\t\t4\tt\t\\N\n" // empty string, not NULL
	tag, err := copyIn(ctx, conn, "COPY c1 FROM STDIN", in)
	if err != nil || tag != "COPY 5" {
		t.Fatalf("tag=%q err=%v", tag, err)
	}
	rows, _ := conn.Query(ctx, `SELECT id, name, amt, ok FROM c1 ORDER BY id`)
	type r struct {
		id   int64
		name *string
		amt  *float64
		ok   *bool
	}
	var got []r
	for rows.Next() {
		var x r
		if err := rows.Scan(&x.id, &x.name, &x.amt, &x.ok); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	if len(got) != 5 || *got[1].name != "tab\there\nnl\\bs" || got[1].amt != nil || got[2].name != nil ||
		*got[3].name != `\N` || got[4].name == nil || *got[4].name != "" || got[2].ok != nil {
		t.Fatalf("got %+v", got)
	}
	out := copyOut(t, ctx, conn, "COPY c1 TO STDOUT")
	back := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(back) != 5 {
		t.Fatalf("out=%q", out)
	}
	// re-load the output into another table and compare the dumps
	conn.Exec(ctx, `CREATE TABLE c1b (id BIGINT PRIMARY KEY, name TEXT, amt DOUBLE PRECISION, ok BOOLEAN, at TIMESTAMPTZ)`)
	if _, err := copyIn(ctx, conn, "COPY c1b FROM STDIN", out); err != nil {
		t.Fatal(err)
	}
	a, b := copyOut(t, ctx, conn, "COPY (SELECT * FROM c1 ORDER BY id) TO STDOUT"), copyOut(t, ctx, conn, "COPY (SELECT * FROM c1b ORDER BY id) TO STDOUT")
	if a != b {
		t.Fatalf("round trip differs:\n%q\n%q", a, b)
	}
	if !strings.Contains(out, "tab\\there\\nnl\\\\bs") || !strings.Contains(out, "\\\\N") {
		t.Fatalf("escapes: %q", out)
	}
	// column list, defaults, custom delimiter and NULL, the end marker
	conn.Exec(ctx, `CREATE TABLE c1d (id BIGINT PRIMARY KEY, name TEXT DEFAULT 'dflt', n BIGINT)`)
	if tag, err = copyIn(ctx, conn, "COPY c1d (id, n) FROM STDIN WITH (DELIMITER '|', NULL 'NULL')", "1|10\n2|NULL\n\\.\n9|9\n"); err != nil || tag != "COPY 2" {
		t.Fatalf("tag=%q err=%v", tag, err)
	}
	if o := copyOut(t, ctx, conn, "COPY c1d (n, name) TO STDOUT (DELIMITER ',', NULL '-')"); o != "10,dflt\n-,dflt\n" {
		t.Fatalf("out=%q", o)
	}
}

func TestCopyCSV(t *testing.T) {
	ctx, conn := copyConn(t)
	conn.Exec(ctx, `CREATE TABLE c2 (id BIGINT PRIMARY KEY, name TEXT, note TEXT)`)
	in := "id,name,note\n" +
		"1,\"a,b\",\"say \"\"hi\"\"\"\n" +
		"2,\"multi\nline\",\n" + // note NULL
		"3,,\"\"\n" + // name NULL, note empty string
		"4,\"\\N\",\\N\r\n"
	tag, err := copyIn(ctx, conn, "COPY c2 FROM STDIN WITH (FORMAT csv, HEADER true)", in)
	if err != nil || tag != "COPY 4" {
		t.Fatalf("tag=%q err=%v", tag, err)
	}
	var name, note *string
	conn.QueryRow(ctx, `SELECT name, note FROM c2 WHERE id = 1`).Scan(&name, &note)
	if *name != "a,b" || *note != `say "hi"` {
		t.Fatalf("1: %v %v", *name, *note)
	}
	conn.QueryRow(ctx, `SELECT name, note FROM c2 WHERE id = 2`).Scan(&name, &note)
	if *name != "multi\nline" || note != nil {
		t.Fatalf("2: %v %v", name, note)
	}
	conn.QueryRow(ctx, `SELECT name, note FROM c2 WHERE id = 3`).Scan(&name, &note)
	if name != nil || note == nil || *note != "" {
		t.Fatalf("3: %v %v", name, note)
	}
	conn.QueryRow(ctx, `SELECT name, note FROM c2 WHERE id = 4`).Scan(&name, &note)
	if *name != `\N` || *note != `\N` { // csv: backslash is no escape, NULL is the empty string
		t.Fatalf("4: %v %v", name, note)
	}
	out := copyOut(t, ctx, conn, "COPY c2 TO STDOUT CSV HEADER")
	if !strings.HasPrefix(out, "id,name,note\n") || !strings.Contains(out, "1,\"a,b\",\"say \"\"hi\"\"\"\n") ||
		!strings.Contains(out, "2,\"multi\nline\",\n") || !strings.Contains(out, "3,,\"\"\n") {
		t.Fatalf("out=%q", out)
	}
	conn.Exec(ctx, `CREATE TABLE c2b (id BIGINT PRIMARY KEY, name TEXT, note TEXT)`)
	if _, err := copyIn(ctx, conn, "COPY c2b FROM STDIN CSV HEADER", out); err != nil {
		t.Fatal(err)
	}
	a, b := copyOut(t, ctx, conn, "COPY (SELECT * FROM c2 ORDER BY id) TO STDOUT CSV"), copyOut(t, ctx, conn, "COPY (SELECT * FROM c2b ORDER BY id) TO STDOUT CSV")
	if a != b {
		t.Fatalf("csv round trip differs:\n%q\n%q", a, b)
	}
	// query form with an expression, other quote character
	if o := copyOut(t, ctx, conn, `COPY (SELECT id, name FROM c2 WHERE id = 1) TO STDOUT WITH (FORMAT csv, QUOTE '''')`); o != "1,a,b\n" && o != "1,'a,b'\n" {
		t.Fatalf("o=%q", o)
	}
}

func TestCopyErrors(t *testing.T) {
	ctx, conn := copyConn(t)
	conn.Exec(ctx, `CREATE TABLE c3 (id BIGINT PRIMARY KEY, v BIGINT NOT NULL)`)
	conn.Exec(ctx, `INSERT INTO c3 VALUES (-1, 0)`)

	// bad value late in a large load: nothing of it remains
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&sb, "%d\t%d\n", i, i)
	}
	sb.WriteString("20000\tnotanumber\n")
	_, err := copyIn(ctx, conn, "COPY c3 FROM STDIN", sb.String())
	if sqlState(err) != "22P02" || !strings.Contains(err.Error(), "line 20001") {
		t.Fatalf("err=%v", err)
	}
	if n := count(t, ctx, conn, "c3"); n != 1 {
		t.Fatalf("rows after failed COPY: %d", n)
	}
	// the connection is usable, and the same load without the bad line works
	if _, err = copyIn(ctx, conn, "COPY c3 FROM STDIN", sb.String()[:strings.LastIndex(sb.String(), "20000\t")]); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, conn, "c3"); n != 20001 {
		t.Fatalf("rows=%d", n)
	}
	for _, c := range []struct{ data, state string }{
		{"1\t1\n", "23505"},                 // existing key
		{"100000\t1\n100000\t2\n", "23505"}, // duplicate within the load
		{"100001\t\\N\n", "23502"},          // NOT NULL
		{"100002\n", "22P04"},               // missing column
		{"100003\t1\t2\n", "22P04"},         // extra column
	} {
		_, err := copyIn(ctx, conn, "COPY c3 FROM STDIN", c.data)
		if sqlState(err) != c.state {
			t.Errorf("%q: err=%v want %s", c.data, err, c.state)
		}
	}
	if n := count(t, ctx, conn, "c3"); n != 20001 {
		t.Fatalf("rows=%d", n)
	}
	// client failure (CopyFail): reader error
	_, err = conn.PgConn().CopyFrom(ctx, io.MultiReader(strings.NewReader("200000\t1\n"), errReader{}), "COPY c3 FROM STDIN")
	if err == nil || count(t, ctx, conn, "c3") != 20001 {
		t.Fatalf("CopyFail: err=%v", err)
	}
	// in a transaction the error aborts the block; ROLLBACK drops earlier rows
	conn.Exec(ctx, `BEGIN`)
	if _, err := copyIn(ctx, conn, "COPY c3 FROM STDIN", "300000\t1\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := copyIn(ctx, conn, "COPY c3 FROM STDIN", "300001\tx\n"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := conn.Exec(ctx, `SELECT 1`); sqlState(err) != "25P02" {
		t.Fatalf("after error in block: %v", err)
	}
	conn.Exec(ctx, `ROLLBACK`)
	if n := count(t, ctx, conn, "c3"); n != 20001 {
		t.Fatalf("rows=%d", n)
	}
	conn.Exec(ctx, `BEGIN`)
	copyIn(ctx, conn, "COPY c3 FROM STDIN", "300000\t1\n")
	conn.Exec(ctx, `COMMIT`)
	if n := count(t, ctx, conn, "c3"); n != 20002 {
		t.Fatalf("rows=%d", n)
	}

	// refusals
	for q, st := range map[string]string{
		"COPY c3 TO STDOUT (FORMAT binary)":            "0A000",
		"COPY c3 FROM '/etc/passwd'":                   "0A000",
		"COPY c3 TO PROGRAM 'cat'":                     "0A000",
		"COPY nosuch FROM STDIN":                       "42P01",
		"COPY c3 (nope) FROM STDIN":                    "42703",
		"COPY c3 FROM STDIN (FORMAT xml)":              "22023",
		"COPY c3 FROM STDIN (QUOTE 'x')":               "22023",
		"COPY (SELECT 1) FROM STDIN":                   "0A000",
		"COPY (INSERT INTO c3 VALUES (1,1)) TO STDOUT": "0A000",
	} {
		_, e1 := conn.PgConn().CopyFrom(ctx, strings.NewReader(""), q)
		var b bytes.Buffer
		_, e2 := conn.PgConn().CopyTo(ctx, &b, q)
		e := e1
		if strings.Contains(q, " TO ") || strings.Contains(q, "TO STDOUT") {
			e = e2
		}
		if sqlState(e) != st {
			t.Errorf("%s: %v want %s", q, e, st)
		}
	}
	if n := count(t, ctx, conn, "c3"); n != 20002 {
		t.Fatalf("rows=%d", n)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("client gave up") }

func TestCopyColumnar(t *testing.T) {
	ctx, conn := copyConn(t)
	if _, err := conn.Exec(ctx, `CREATE TABLE c4 (id BIGINT PRIMARY KEY, g BIGINT, x DOUBLE PRECISION) WITH (columnar = true)`); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 1; i <= 50000; i++ {
		fmt.Fprintf(&sb, "%d\t%d\t%d.5\n", i, i%7, i)
	}
	if _, err := copyIn(ctx, conn, "COPY c4 FROM STDIN", sb.String()); err != nil {
		t.Fatal(err)
	}
	var n int64
	var s float64
	if err := conn.QueryRow(ctx, `SELECT count(*), sum(x) FROM c4 WHERE g = 3`).Scan(&n, &s); err != nil {
		t.Fatal(err)
	}
	wn, ws := int64(0), 0.0
	for i := 1; i <= 50000; i++ {
		if i%7 == 3 {
			wn++
			ws += float64(i) + 0.5
		}
	}
	if n != wn || s != ws {
		t.Fatalf("aggregate n=%d sum=%v want %d %v", n, s, wn, ws)
	}
	if out := copyOut(t, ctx, conn, "COPY c4 TO STDOUT"); strings.Count(out, "\n") != 50000 {
		t.Fatalf("lines=%d", strings.Count(out, "\n"))
	}
}

func TestCopyCancelTimeoutBudget(t *testing.T) {
	ctx, conn := copyConn(t)
	conn.Exec(ctx, `CREATE TABLE c5 (id BIGINT PRIMARY KEY, v TEXT)`)
	// statement_timeout covers the whole COPY, including the wait for data
	conn.Exec(ctx, `SET statement_timeout = '150ms'`)
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := fmt.Fprintf(pw, "%d\tx\n", i); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		pw.Close()
	}()
	_, err := conn.PgConn().CopyFrom(ctx, pr, "COPY c5 FROM STDIN")
	pr.CloseWithError(io.EOF)
	if sqlState(err) != "57014" {
		t.Fatalf("timeout: %v", err)
	}
	conn.Exec(ctx, `SET statement_timeout = 0`)
	if n := count(t, ctx, conn, "c5"); n != 0 {
		t.Fatalf("rows=%d", n)
	}

	// node/query memory budget: 53200 and a clean rollback
	old := sql.QueryMemLimit
	sql.QueryMemLimit = 2 << 20
	defer func() { sql.QueryMemLimit = old }()
	var sb strings.Builder
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&sb, "%d\tvalue-%d\n", i, i)
	}
	_, err = copyIn(ctx, conn, "COPY c5 FROM STDIN", sb.String())
	if sqlState(err) != "53200" {
		t.Fatalf("budget: %v", err)
	}
	if n := count(t, ctx, conn, "c5"); n != 0 || sql.SQLMemInUse() != 0 {
		t.Fatalf("rows=%d inuse=%d", n, sql.SQLMemInUse())
	}
}

// TestCopyVsInsert prints the time to load N rows with COPY and with
// batched INSERT (STASK_COPY_ROWS, default 100000; the 1M figure of docs/05
// is STASK_COPY_ROWS=1000000).
func TestCopyVsInsert(t *testing.T) {
	n := 100000
	if v := os.Getenv("STASK_COPY_ROWS"); v != "" {
		fmt.Sscan(v, &n)
	} else if testing.Short() {
		t.Skip("short")
	}
	ctx, conn := copyConn(t)
	conn.Exec(ctx, `CREATE TABLE bc (id BIGINT PRIMARY KEY, a BIGINT, b TEXT, c DOUBLE PRECISION)`)
	conn.Exec(ctx, `CREATE TABLE bi (id BIGINT PRIMARY KEY, a BIGINT, b TEXT, c DOUBLE PRECISION)`)
	t0 := time.Now()
	pr, pw := io.Pipe()
	go func() {
		w := bytes.Buffer{}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&w, "%d\t%d\tname-%d\t%d.25\n", i, i%1000, i, i)
			if w.Len() > 64<<10 {
				pw.Write(w.Bytes())
				w.Reset()
			}
		}
		pw.Write(w.Bytes())
		pw.Close()
	}()
	if _, err := conn.PgConn().CopyFrom(ctx, pr, "COPY bc FROM STDIN"); err != nil {
		t.Fatal(err)
	}
	dc := time.Since(t0)
	t0 = time.Now()
	const batch = 1000
	conn.Exec(ctx, `BEGIN`)
	for i := 0; i < n; i += batch {
		var sb strings.Builder
		sb.WriteString("INSERT INTO bi VALUES ")
		for j := i; j < i+batch && j < n; j++ {
			if j > i {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "(%d,%d,'name-%d',%d.25)", j, j%1000, j, j)
		}
		if _, err := conn.Exec(ctx, sb.String()); err != nil {
			t.Fatal(err)
		}
	}
	conn.Exec(ctx, `COMMIT`)
	di := time.Since(t0)
	t.Logf("rows=%d COPY %v (%.0f rows/s)  INSERT batch=%d in one tx %v (%.0f rows/s)", n, dc, float64(n)/dc.Seconds(), batch, di, float64(n)/di.Seconds())
	if count(t, ctx, conn, "bc") != n || count(t, ctx, conn, "bi") != n {
		t.Fatal("row counts")
	}
}
