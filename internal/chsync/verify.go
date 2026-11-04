package chsync

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"

	"stask/internal/shadow"
)

// TableCheck is the comparison of one table, PostgreSQL versus ClickHouse
// at watermark W.
type TableCheck struct {
	Table            string
	PGRows, CHRows   int64
	Missing, Extra   int64 // keys only in PostgreSQL / only in ClickHouse
	Differ           int64 // same key, some column differs
	Examples         []string
	PGBytes, CHBytes int64 // on disk: table + indexes + TOAST / active parts
}

type VerifyReport struct {
	W      uint64
	Tables []TableCheck
	OK     bool
}

// Verify compares every table row by row, PostgreSQL against the snapshot
// view of ClickHouse at W. The source must be quiet: Verify first waits up
// to wait for W to reach the current WAL position of the source.
func Verify(ctx context.Context, cfg Config, wait time.Duration) (*VerifyReport, error) {
	cfg.defaults()
	src, err := connect(ctx, cfg.SourceURL, false)
	if err != nil {
		return nil, err
	}
	defer src.Close(context.Background())
	tables, err := shadow.LoadSourceTables(ctx, src, cfg.Tables)
	if err != nil {
		return nil, err
	}
	ch := NewCH(cfg.CHURL)
	cur, _, err := pgOne(ctx, src, "SELECT pg_current_wal_lsn()::text")
	if err != nil {
		return nil, err
	}
	target, _ := pglogrepl.ParseLSN(cur)
	var w uint64
	for end := time.Now().Add(wait); ; time.Sleep(500 * time.Millisecond) {
		w, _, err = ReadW(ctx, ch, cfg.Slot)
		if err != nil {
			return nil, err
		}
		if w >= uint64(target) || time.Now().After(end) {
			break
		}
	}
	if w < uint64(target) {
		return nil, fmt.Errorf("W %s has not reached the source position %s after %s (is chsync running, is the source quiet?)", pglogrepl.LSN(w), target, wait)
	}
	rep := &VerifyReport{W: w, OK: true}
	for _, t := range tables {
		tc, err := verifyTable(ctx, cfg.SourceURL, ch, t, w)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name, err)
		}
		if s, ok, _ := pgOne(ctx, src, "SELECT pg_total_relation_size($1::regclass)::text", `"`+t.Name+`"`); ok {
			fmt.Sscan(s, &tc.PGBytes)
		}
		if s, err := ch.Query(ctx, "SELECT sum(bytes_on_disk) FROM system.parts WHERE active AND database = currentDatabase() AND table = "+sqlString(t.Name)); err == nil {
			fmt.Sscan(strings.TrimSpace(s), &tc.CHBytes)
		}
		if tc.Missing+tc.Extra+tc.Differ > 0 {
			rep.OK = false
		}
		rep.Tables = append(rep.Tables, tc)
	}
	return rep, nil
}

func isTextKind(k shadow.Kind) bool { return k == shadow.KText || k == shadow.KBpchar }

func verifyTable(ctx context.Context, srcURL string, ch *CH, t *shadow.Table, w uint64) (TableCheck, error) {
	tc := TableCheck{Table: t.Name}
	var pgCols, chCols, pgOrder, chOrder []string
	for _, c := range t.Cols {
		q := `"` + strings.ReplaceAll(c.Name, `"`, `""`) + `"`
		pgCols = append(pgCols, q)
		cc := qc(c.Name)
		if c.OID == 1184 { // same text as PostgreSQL in UTC, so Normalize sees one format
			cc = "formatDateTime(" + cc + ", '%Y-%m-%d %H:%i:%S.%f') || '+00'"
		}
		chCols = append(chCols, cc)
	}
	for _, p := range t.PK {
		c := t.Cols[p]
		q := `"` + strings.ReplaceAll(c.Name, `"`, `""`) + `"`
		if isTextKind(c.Kind) {
			q += ` COLLATE "C"` // byte order, as ClickHouse sorts String
		}
		pgOrder = append(pgOrder, q)
		chOrder = append(chOrder, qc(c.Name))
	}
	pgc, err := connect(ctx, srcURL, false)
	if err != nil {
		return tc, err
	}
	defer pgc.Close(context.Background())
	pr, pw := io.Pipe()
	go func() {
		_, err := pgc.CopyTo(ctx, pw, fmt.Sprintf(`COPY (SELECT %s FROM "%s" ORDER BY %s) TO STDOUT`, strings.Join(pgCols, ", "), t.Name, strings.Join(pgOrder, ", ")))
		pw.CloseWithError(err)
	}()
	defer pr.Close()
	cr, err := ch.Stream(ctx, fmt.Sprintf("SELECT %s FROM %s(w = %d) ORDER BY %s FORMAT TabSeparated", strings.Join(chCols, ", "), qc(ViewName(t.Name)), w, strings.Join(chOrder, ", ")))
	if err != nil {
		return tc, err
	}
	defer cr.Close()
	a, b := newRowReader(pr, t), newRowReader(cr, t)
	ra, rb := a.next(), b.next()
	for ra != nil || rb != nil {
		switch {
		case rb == nil || (ra != nil && keyCmp(t, ra, rb) < 0):
			tc.Missing++
			tc.PGRows++
			tc.example("only in PostgreSQL: " + ra.show())
			ra = a.next()
		case ra == nil || keyCmp(t, rb, ra) < 0:
			tc.Extra++
			tc.CHRows++
			tc.example("only in ClickHouse: " + rb.show())
			rb = b.next()
		default:
			tc.PGRows++
			tc.CHRows++
			for i := range t.Cols {
				if ra.norm[i] != rb.norm[i] {
					tc.Differ++
					tc.example(fmt.Sprintf("key %s column %s: PostgreSQL %q, ClickHouse %q", ra.key, t.Cols[i].Name, ra.raw[i], rb.raw[i]))
					break
				}
			}
			ra, rb = a.next(), b.next()
		}
	}
	if a.err != nil {
		return tc, fmt.Errorf("PostgreSQL: %w", a.err)
	}
	if b.err != nil {
		return tc, fmt.Errorf("ClickHouse: %w", b.err)
	}
	return tc, nil
}

func (tc *TableCheck) example(s string) {
	if len(tc.Examples) < 5 {
		tc.Examples = append(tc.Examples, s)
	}
}

type row struct {
	key       string
	raw, norm []string
}

// keyCmp orders two rows as both sides sort them: integers by value, the
// rest bytewise (PostgreSQL sorts text keys COLLATE "C" here).
func keyCmp(t *shadow.Table, a, b *row) int {
	for _, p := range t.PK {
		x, y := a.raw[p], b.raw[p]
		if t.Cols[p].Kind == shadow.KInt {
			var i, j int64
			fmt.Sscan(x, &i)
			fmt.Sscan(y, &j)
			if c := cmp.Compare(i, j); c != 0 {
				return c
			}
			continue
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func (r *row) show() string { return strings.Join(r.raw, " | ") }

type rowReader struct {
	sc  *bufio.Scanner
	t   *shadow.Table
	err error
}

func newRowReader(r io.Reader, t *shadow.Table) *rowReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	return &rowReader{sc: sc, t: t}
}

func (rr *rowReader) next() *row {
	if rr.err != nil || !rr.sc.Scan() {
		if rr.err == nil {
			rr.err = rr.sc.Err()
		}
		return nil
	}
	f := strings.Split(rr.sc.Text(), "\t")
	if len(f) != len(rr.t.Cols) {
		rr.err = fmt.Errorf("row with %d fields, want %d: %q", len(f), len(rr.t.Cols), clip(rr.sc.Text()))
		return nil
	}
	r := &row{raw: make([]string, len(f)), norm: make([]string, len(f))}
	for i, v := range f {
		if v == `\N` {
			r.raw[i], r.norm[i] = "NULL", "\x00NULL"
			continue
		}
		v = tsvUnescape(v)
		r.raw[i] = v
		r.norm[i] = shadow.Normalize(rr.t.Cols[i].Kind, v)
	}
	var k []string
	for _, p := range rr.t.PK {
		k = append(k, r.raw[p])
	}
	r.key = strings.Join(k, "\x00")
	return r
}

func tsvUnescape(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 == len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

func (r *VerifyReport) Write(w io.Writer) {
	fmt.Fprintf(w, "W = %s\n", pglogrepl.LSN(r.W))
	fmt.Fprintf(w, "%-14s %12s %12s %8s %8s %8s %10s %10s %7s\n", "table", "pg_rows", "ch_rows", "missing", "extra", "differ", "pg_MB", "ch_MB", "ch/pg")
	var pg, ch int64
	for _, t := range r.Tables {
		ratio := 0.0
		if t.PGBytes > 0 {
			ratio = float64(t.CHBytes) / float64(t.PGBytes)
		}
		fmt.Fprintf(w, "%-14s %12d %12d %8d %8d %8d %10.1f %10.1f %7.2f\n", t.Table, t.PGRows, t.CHRows, t.Missing, t.Extra, t.Differ,
			float64(t.PGBytes)/1e6, float64(t.CHBytes)/1e6, ratio)
		for _, e := range t.Examples {
			fmt.Fprintf(w, "    %s\n", e)
		}
		pg += t.PGBytes
		ch += t.CHBytes
	}
	fmt.Fprintf(w, "on disk: PostgreSQL %.1f MB, ClickHouse %.1f MB (%.0f%% smaller)\n", float64(pg)/1e6, float64(ch)/1e6, 100*(1-float64(ch)/float64(max(pg, 1))))
	if r.OK {
		fmt.Fprintln(w, "VERIFY OK: every row identical")
	} else {
		fmt.Fprintln(w, "VERIFY FAILED")
	}
}
