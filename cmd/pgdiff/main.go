// pgdiff is a differential tester: it reads script files of SQL sessions,
// runs every statement the same way on PostgreSQL 16 and on STASK through
// pgx, and compares column count, names, type OIDs, values, SQLSTATE and the
// command tag. It prints a markdown report; -fail-on-diff exits 1 when any
// statement differs.
//
// Script format (see scripts/pgdiff/*.sql):
//
//	-- ### a/name       starts a session (a fresh connection on each side);
//	                    the part before "/" is the group
//	-- drop: t1, t2     tables dropped on both sides before and after the session
//	-- args: 1 | x | null   parameters of the next statement (text, "null" = NULL)
//	-- unordered        compare the next statement's rows as a set
//	-- ordered          compare rows in order (default when SQL has ORDER BY)
//	-- simple           send the next statement with the simple protocol
//	-- note: text       free text shown beside the next statement in the report
//	SQL ending with ;   one statement (quotes and $$ are respected)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type stmt struct {
	SQL       string
	Args      []*string
	Unordered bool
	Ordered   bool
	Simple    bool
	Note      string
	Line      int
}

type session struct {
	Name, Group, File string
	Drop              []string
	Stmts             []stmt
}

type col struct {
	Name string
	OID  uint32
}

type result struct {
	Err    *pgconn.PgError
	ErrStr string // non-PG error (timeout, closed connection)
	Cols   []col
	Rows   [][]string // NULL is "\x00NULL"
	Tag    string
	Rowsn  int64
}

const null = "\x00NULL"

func main() {
	pgURL := flag.String("pg", "postgres://postgres@127.0.0.1:28432/postgres?sslmode=disable", "PostgreSQL 16 url")
	stURL := flag.String("stask", "postgres://app@127.0.0.1:28433/stask?sslmode=disable", "STASK url")
	failOnDiff := flag.Bool("fail-on-diff", false, "exit 1 if any statement is DIFF or ERR-ONLY-ONE-SIDE")
	verbose := flag.Bool("v", false, "list PASS statements too")
	only := flag.String("only", "", "run only sessions whose name contains this")
	tol := flag.Float64("tol", 1e-9, "relative tolerance for float values")
	timeout := flag.Duration("timeout", 20*time.Second, "per-statement timeout")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: pgdiff [flags] script.sql|dir ...")
		os.Exit(2)
	}
	var sessions []session
	for _, p := range flag.Args() {
		fs := []string{p}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			fs, _ = filepath.Glob(filepath.Join(p, "*.sql"))
			sort.Strings(fs)
		}
		for _, f := range fs {
			ss, err := parseFile(f)
			if err != nil {
				fmt.Fprintln(os.Stderr, "parse:", err)
				os.Exit(2)
			}
			sessions = append(sessions, ss...)
		}
	}
	ctx := context.Background()
	var recs []record
	for _, s := range sessions {
		if *only != "" && !strings.Contains(s.Name, *only) {
			continue
		}
		recs = append(recs, runSession(ctx, s, *pgURL, *stURL, *tol, *timeout)...)
	}
	bad := report(recs, *verbose)
	if *failOnDiff && bad > 0 {
		os.Exit(1)
	}
}

// ---- parsing

func parseFile(path string) ([]session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []session
	var cur *session
	var pend stmt
	var buf strings.Builder
	startLine := 0
	inStr, inDollar := false, ""
	for ln, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if buf.Len() == 0 && !inStr && inDollar == "" && strings.HasPrefix(t, "--") {
			d := strings.TrimSpace(strings.TrimPrefix(t, "--"))
			switch {
			case strings.HasPrefix(d, "###"):
				name := strings.TrimSpace(strings.TrimPrefix(d, "###"))
				g := name
				if i := strings.Index(name, "/"); i >= 0 {
					g = name[:i]
				}
				out = append(out, session{Name: name, Group: g, File: filepath.Base(path)})
				cur = &out[len(out)-1]
				pend = stmt{}
			case strings.HasPrefix(d, "drop:"):
				for _, x := range strings.Split(strings.TrimPrefix(d, "drop:"), ",") {
					if x = strings.TrimSpace(x); x != "" && cur != nil {
						cur.Drop = append(cur.Drop, x)
					}
				}
			case strings.HasPrefix(d, "args:"):
				for _, x := range strings.Split(strings.TrimPrefix(d, "args:"), "|") {
					x = strings.TrimSpace(x)
					if x == "null" {
						pend.Args = append(pend.Args, nil)
					} else {
						x := x
						pend.Args = append(pend.Args, &x)
					}
				}
			case d == "unordered":
				pend.Unordered = true
			case d == "ordered":
				pend.Ordered = true
			case d == "simple":
				pend.Simple = true
			case strings.HasPrefix(d, "note:"):
				pend.Note = strings.TrimSpace(strings.TrimPrefix(d, "note:"))
			}
			continue
		}
		if buf.Len() == 0 && t == "" {
			continue
		}
		if buf.Len() == 0 {
			startLine = ln + 1
		}
		buf.WriteString(line + "\n")
		// scan for statement end
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case inDollar != "":
				if strings.HasPrefix(line[i:], inDollar) {
					i += len(inDollar) - 1
					inDollar = ""
				}
			case inStr:
				if c == '\'' {
					inStr = false
				}
			case c == '\'':
				inStr = true
			case c == '$' && dollarTag(line[i:]) != "":
				inDollar = dollarTag(line[i:])
				i += len(inDollar) - 1
			case c == '-' && strings.HasPrefix(line[i:], "--"):
				i = len(line)
			}
		}
		if !inStr && inDollar == "" && strings.HasSuffix(strings.TrimSpace(stripComment(line)), ";") {
			if cur == nil {
				return nil, fmt.Errorf("%s:%d: statement before any '-- ###' header", path, startLine)
			}
			pend.SQL = strings.TrimSuffix(strings.TrimSpace(buf.String()), ";")
			pend.Line = startLine
			cur.Stmts = append(cur.Stmts, pend)
			pend = stmt{}
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		return nil, fmt.Errorf("%s:%d: unterminated statement", path, startLine)
	}
	return out, nil
}

// dollarTag returns the opening dollar quote ($$ or $tag$) at the start of s, or "".
func dollarTag(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '$' {
			return s[:i+1]
		}
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' && i > 1) {
			return ""
		}
	}
	return ""
}

func stripComment(l string) string {
	inS := false
	for i := 0; i < len(l); i++ {
		if l[i] == '\'' {
			inS = !inS
		} else if !inS && strings.HasPrefix(l[i:], "--") {
			return l[:i]
		}
	}
	return l
}

// ---- execution

type record struct {
	Sess    *session
	St      stmt
	Pg, Sk  result
	Verdict string // PASS, PASS-MSG, DIFF, ERR-ONLY-ONE-SIDE
	Class   string
	Detail  string
}

func runSession(ctx context.Context, s session, pgURL, stURL string, tol float64, to time.Duration) []record {
	conns := [2]*pgconn.PgConn{}
	var out []record
	for i, u := range []string{pgURL, stURL} {
		c, err := pgconn.Connect(ctx, u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connect side %d: %v\n", i, err)
			os.Exit(2)
		}
		conns[i] = c
		defer c.Close(ctx)
	}
	cleanup := func() {
		for _, c := range conns {
			if c.IsClosed() {
				continue
			}
			exec(ctx, c, "ROLLBACK", nil, true, to)
			for _, t := range s.Drop {
				exec(ctx, c, "DROP TABLE IF EXISTS "+t+" CASCADE", nil, true, to)
			}
		}
	}
	cleanup()
	defer cleanup()
	ss := s
	for _, st := range s.Stmts {
		r := record{Sess: &ss, St: st}
		r.Pg = exec(ctx, conns[0], st.SQL, st.Args, st.Simple, to)
		r.Sk = exec(ctx, conns[1], st.SQL, st.Args, st.Simple, to)
		compare(&r, tol)
		out = append(out, r)
		for i, c := range conns { // reconnect if a side died
			if c.IsClosed() {
				u := []string{pgURL, stURL}[i]
				if nc, err := pgconn.Connect(ctx, u); err == nil {
					conns[i] = nc
				}
			}
		}
	}
	return out
}

func exec(ctx context.Context, c *pgconn.PgConn, sql string, args []*string, simple bool, to time.Duration) result {
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	var rr *pgconn.Result
	var err error
	if simple && len(args) == 0 {
		var rs []*pgconn.Result
		rs, err = c.Exec(cctx, sql).ReadAll()
		if len(rs) > 0 {
			rr = rs[len(rs)-1]
		}
	} else {
		params := make([][]byte, len(args))
		for i, a := range args {
			if a != nil {
				params[i] = []byte(*a)
			}
		}
		rr = c.ExecParams(cctx, sql, params, nil, nil, nil).Read()
		err = rr.Err
	}
	var res result
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			res.Err = pe
		} else {
			res.ErrStr = err.Error()
		}
		return res
	}
	if rr == nil {
		return res
	}
	for _, f := range rr.FieldDescriptions {
		res.Cols = append(res.Cols, col{f.Name, f.DataTypeOID})
	}
	for _, row := range rr.Rows {
		r := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				r[i] = null
			} else {
				r[i] = string(v)
			}
		}
		res.Rows = append(res.Rows, r)
	}
	res.Tag = rr.CommandTag.String()
	res.Rowsn = rr.CommandTag.RowsAffected()
	return res
}

// ---- comparison

var floatOIDs = map[uint32]bool{700: true, 701: true}

func compare(r *record, tol float64) {
	pg, sk := &r.Pg, &r.Sk
	r.Verdict = "PASS"
	if pg.ErrStr != "" || sk.ErrStr != "" {
		r.Verdict, r.Class = "DIFF", "transport"
		if (pg.ErrStr != "") != (sk.ErrStr != "") {
			r.Verdict = "ERR-ONLY-ONE-SIDE"
		}
		r.Detail = "non-PG error (timeout/connection): pg=" + pg.ErrStr + " stask=" + sk.ErrStr
		return
	}
	switch {
	case pg.Err != nil && sk.Err != nil:
		if pg.Err.Code != sk.Err.Code {
			r.Verdict, r.Class = "DIFF", "sqlstate"
			r.Detail = fmt.Sprintf("SQLSTATE pg=%s stask=%s", pg.Err.Code, sk.Err.Code)
		} else if pg.Err.Message != sk.Err.Message {
			r.Verdict, r.Class = "PASS-MSG", "message"
			r.Detail = "same SQLSTATE " + pg.Err.Code + ", message differs"
		}
		return
	case pg.Err != nil || sk.Err != nil:
		r.Verdict = "ERR-ONLY-ONE-SIDE"
		if sk.Err != nil {
			r.Class = "feature"
			switch sk.Err.Code {
			case "0A000", "42601", "42883", "42704", "42809":
			default:
				r.Class = "semantics"
			}
			r.Detail = "STASK rejects: " + sk.Err.Code
		} else {
			r.Class = "semantics"
			r.Detail = "STASK accepts, PostgreSQL rejects: " + pg.Err.Code
		}
		return
	}
	var d []string
	class := ""
	valDiff := false
	set := func(c string) {
		if class == "" {
			class = c
		}
	}
	if len(pg.Cols) != len(sk.Cols) {
		d = append(d, fmt.Sprintf("column count pg=%d stask=%d", len(pg.Cols), len(sk.Cols)))
		set("columns")
	} else {
		for i := range pg.Cols {
			if pg.Cols[i].Name != sk.Cols[i].Name {
				d = append(d, fmt.Sprintf("col %d name pg=%q stask=%q", i+1, pg.Cols[i].Name, sk.Cols[i].Name))
				set("columns")
			}
			if pg.Cols[i].OID != sk.Cols[i].OID {
				d = append(d, fmt.Sprintf("col %d (%s) type OID pg=%d stask=%d", i+1, pg.Cols[i].Name, pg.Cols[i].OID, sk.Cols[i].OID))
				set("type")
			}
		}
	}
	if pg.Tag != sk.Tag {
		d = append(d, fmt.Sprintf("command tag pg=%q stask=%q", pg.Tag, sk.Tag))
		set("tag")
	}
	if len(pg.Cols) == len(sk.Cols) {
		a, b := pg.Rows, sk.Rows
		if !r.St.Ordered && (r.St.Unordered || !hasOrderBy(r.St.SQL)) {
			a, b = sortedRows(a), sortedRows(b)
		}
		if len(a) != len(b) {
			d = append(d, fmt.Sprintf("row count pg=%d stask=%d", len(a), len(b)))
			set("value")
		} else {
		rows:
			for i := range a {
				for j := range a[i] {
					if !valEq(a[i][j], b[i][j], pg.Cols[j].OID, tol) {
						d = append(d, fmt.Sprintf("row %d col %s: pg=%s stask=%s", i+1, pg.Cols[j].Name, show(a[i][j]), show(b[i][j])))
						valDiff = true
						if looksLikeFormat(a[i][j], b[i][j]) {
							set("format")
						} else {
							set("value")
						}
						break rows
					}
				}
			}
		}
	}
	if len(d) > 0 {
		r.Verdict, r.Class, r.Detail = "DIFF", class, strings.Join(d, "; ")
		// a type difference explains value text differences
		if class != "type" && containsStr(d, "type OID") {
			r.Class = "type"
		}
		if r.Class == "type" && valDiff {
			r.Class = "type+value" // the other type changes the text too (numeric 1.50 vs float 1.5)
		}
	}
}

func containsStr(d []string, sub string) bool {
	for _, x := range d {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

// looksLikeFormat: the two texts hold the same characters apart from
// punctuation/case/whitespace, or the same number.
func looksLikeFormat(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(s)
		return strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' {
				return r
			}
			return -1
		}, s)
	}
	if norm(a) == norm(b) {
		return true
	}
	fa, e1 := strconv.ParseFloat(a, 64)
	fb, e2 := strconv.ParseFloat(b, 64)
	return e1 == nil && e2 == nil && fa == fb
}

func hasOrderBy(sql string) bool {
	u := strings.Join(strings.Fields(strings.ToUpper(sql)), " ")
	return strings.Contains(u, "ORDER BY")
}

func sortedRows(rows [][]string) [][]string {
	c := append([][]string(nil), rows...)
	sort.Slice(c, func(i, j int) bool { return strings.Join(c[i], "\x01") < strings.Join(c[j], "\x01") })
	return c
}

func valEq(a, b string, oid uint32, tol float64) bool {
	if a == b {
		return true
	}
	if floatOIDs[oid] {
		fa, e1 := strconv.ParseFloat(a, 64)
		fb, e2 := strconv.ParseFloat(b, 64)
		if e1 == nil && e2 == nil {
			if math.IsNaN(fa) && math.IsNaN(fb) {
				return true
			}
			return math.Abs(fa-fb) <= tol*math.Max(1, math.Max(math.Abs(fa), math.Abs(fb)))
		}
	}
	return false
}

func show(s string) string {
	if s == null {
		return "NULL"
	}
	return strconv.Quote(trunc(s, 80))
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- report

func fmtRes(r result) string {
	switch {
	case r.ErrStr != "":
		return "ERROR(transport) " + r.ErrStr
	case r.Err != nil:
		return fmt.Sprintf("ERROR %s: %s", r.Err.Code, r.Err.Message)
	}
	var sb strings.Builder
	if len(r.Cols) > 0 {
		names := make([]string, len(r.Cols))
		for i, c := range r.Cols {
			names[i] = fmt.Sprintf("%s:%d", c.Name, c.OID)
		}
		sb.WriteString("cols(" + strings.Join(names, ", ") + ") ")
		n := len(r.Rows)
		sb.WriteString(fmt.Sprintf("%d rows", n))
		for i, row := range r.Rows {
			if i == 3 {
				sb.WriteString(" ...")
				break
			}
			v := make([]string, len(row))
			for j, x := range row {
				v[j] = show(x)
			}
			sb.WriteString(" [" + strings.Join(v, ", ") + "]")
		}
		sb.WriteString(" ")
	}
	sb.WriteString("tag=" + r.Tag)
	return sb.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func report(recs []record, verbose bool) int {
	count := map[string]int{}
	byGroup := map[string]map[string]int{}
	byClass := map[string]int{}
	var groups []string
	for _, r := range recs {
		count[r.Verdict]++
		g := r.Sess.Group
		if byGroup[g] == nil {
			byGroup[g] = map[string]int{}
			groups = append(groups, g)
		}
		byGroup[g][r.Verdict]++
		if r.Verdict != "PASS" {
			byClass[r.Verdict+"/"+r.Class]++
		}
	}
	fmt.Println("# pgdiff report")
	fmt.Printf("\n%d statements in total: PASS %d, PASS-MSG %d (same SQLSTATE, different message), DIFF %d, ERR-ONLY-ONE-SIDE %d\n\n",
		len(recs), count["PASS"], count["PASS-MSG"], count["DIFF"], count["ERR-ONLY-ONE-SIDE"])
	fmt.Println("| group | statements | PASS | PASS-MSG | DIFF | ERR-ONLY-ONE-SIDE |")
	fmt.Println("|---|---|---|---|---|---|")
	for _, g := range groups {
		m := byGroup[g]
		fmt.Printf("| %s | %d | %d | %d | %d | %d |\n", g, m["PASS"]+m["PASS-MSG"]+m["DIFF"]+m["ERR-ONLY-ONE-SIDE"], m["PASS"], m["PASS-MSG"], m["DIFF"], m["ERR-ONLY-ONE-SIDE"])
	}
	var keys []string
	for k := range byClass {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("\n| verdict/class | count |\n|---|---|")
	for _, k := range keys {
		fmt.Printf("| %s | %d |\n", k, byClass[k])
	}
	fmt.Println("\n## Statements")
	for _, r := range recs {
		if r.Verdict == "PASS" && !verbose {
			continue
		}
		fmt.Printf("\n### %s — %s:%d (%s)\n\n", r.Verdict, r.Sess.Name, r.St.Line, r.Class)
		fmt.Printf("```sql\n%s\n```\n", r.St.SQL)
		if r.St.Note != "" {
			fmt.Printf("note: %s\n\n", r.St.Note)
		}
		if len(r.St.Args) > 0 {
			var a []string
			for _, x := range r.St.Args {
				if x == nil {
					a = append(a, "NULL")
				} else {
					a = append(a, strconv.Quote(*x))
				}
			}
			fmt.Printf("args: %s\n\n", strings.Join(a, ", "))
		}
		if r.Verdict == "PASS" {
			continue
		}
		fmt.Printf("- why: %s\n- pg: `%s`\n- stask: `%s`\n", r.Detail, oneLine(fmtRes(r.Pg)), oneLine(fmtRes(r.Sk)))
	}
	return count["DIFF"] + count["ERR-ONLY-ONE-SIDE"]
}
