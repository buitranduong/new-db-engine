package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func bg() context.Context { return context.Background() }

// mustConn opens a SQL connection (UTC, so text forms of timestamps agree).
func mustConn(t *T, url string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg(), 15*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	if _, err := c.Exec(ctx, "SET TimeZone TO 'UTC'"); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	t.OnCleanup(func() { c.Close(bg()) })
	return c
}

// ex runs statements, failing the scenario on the first error.
func ex(t *T, c *pgx.Conn, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := c.Exec(bg(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// exErr runs one statement and returns its error.
func exErr(c *pgx.Conn, q string, args ...any) error {
	_, err := c.Exec(bg(), q, args...)
	return err
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// scalar returns the first column of the first row as text.
func scalar(t *T, c *pgx.Conn, q string, args ...any) string {
	t.Helper()
	var s *string
	if err := c.QueryRow(bg(), q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if s == nil {
		return "NULL"
	}
	return *s
}

// dump returns a table as key -> row text (every column ::text, the first
// column is the key unless keyCols are given).
func dump(t *T, c *pgx.Conn, table string, keyCols ...string) map[string]string {
	t.Helper()
	rows, err := c.Query(bg(), "SELECT column_name FROM information_schema.columns WHERE table_name = $1 ORDER BY ordinal_position", table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	var cols []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		cols = append(cols, n)
	}
	rows.Close()
	if len(cols) == 0 {
		t.Fatalf("table %s has no columns", table)
	}
	if len(keyCols) == 0 {
		keyCols = cols[:1]
	}
	var sel []string
	for _, n := range cols {
		sel = append(sel, `"`+n+`"::text`)
	}
	rs, err := c.Query(bg(), "SELECT "+strings.Join(sel, ", ")+" FROM "+table)
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer rs.Close()
	out := map[string]string{}
	for rs.Next() {
		vals := make([]*string, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			t.Fatalf("dump %s scan: %v", table, err)
		}
		m := map[string]string{}
		var line []string
		for i, v := range vals {
			s := "\x00NULL"
			if v != nil {
				s = *v
			}
			m[cols[i]] = s
			line = append(line, cols[i]+"="+strings.ReplaceAll(s, "\x00NULL", "NULL"))
		}
		var k []string
		for _, kc := range keyCols {
			k = append(k, m[kc])
		}
		out[strings.Join(k, "|")] = strings.Join(line, ";")
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	return out
}

// diffMaps describes up to n differences between two dumps ("" if equal).
func diffMaps(a, b map[string]string, an, bn string, n int) string {
	var d []string
	for _, k := range sortedKeys(a) {
		if v, ok := b[k]; !ok {
			d = append(d, fmt.Sprintf("only in %s: %s", an, a[k]))
		} else if v != a[k] {
			d = append(d, fmt.Sprintf("differs: %s: %s  vs  %s: %s", an, a[k], bn, v))
		}
	}
	for _, k := range sortedKeys(b) {
		if _, ok := a[k]; !ok {
			d = append(d, fmt.Sprintf("only in %s: %s", bn, b[k]))
		}
	}
	if len(d) == 0 {
		return ""
	}
	sort.Strings(d)
	more := ""
	if len(d) > n {
		more = fmt.Sprintf(" (+%d more)", len(d)-n)
		d = d[:n]
	}
	return fmt.Sprintf("%d differences: %s%s", len(d), strings.Join(d, " | "), more)
}

var oidType = map[uint32]string{16: "boolean", 17: "bytea", 20: "bigint", 21: "smallint", 23: "integer", 25: "text", 114: "json",
	700: "real", 701: "double precision", 1042: "char", 1043: "varchar", 1082: "date", 1083: "time", 1114: "timestamp", 1184: "timestamptz",
	1700: "numeric", 2950: "uuid", 3802: "jsonb", 1016: "bigint[]", 1009: "text[]", 1231: "numeric[]", 1000: "boolean[]", 2951: "uuid[]",
	1185: "timestamptz[]", 1022: "double precision[]"}

func typeOf(oid uint32) string {
	if s, ok := oidType[oid]; ok {
		return s
	}
	return "text"
}

// Applier replays decoded transactions into destination tables over SQL: the
// subscriber of a logical replication setup (pg's apply worker). A
// transaction whose commit LSN was applied already is skipped, which is
// what makes at-least-once delivery safe.
type Applier struct {
	c *pgx.Conn
	// Connect, if set, makes the applier crash-safe like a real subscriber:
	// the position (applied_lsn table) is written in the same transaction as
	// the changes, and after a broken connection Resync reopens it and reads
	// the position back, so a transaction whose COMMIT outcome is unknown
	// is neither lost nor applied twice.
	Connect func() (*pgx.Conn, error)
	dst     map[string]string // source relation -> destination table
	Last    pglogrepl.LSN
	LastEnd pglogrepl.LSN // end LSN of the last applied transaction: where to restart
	Applied int
	Dups    int
	Changes int
}

func newApplier(c *pgx.Conn, dst map[string]string) *Applier { return &Applier{c: c, dst: dst} }

func qi(s string) string { return `"` + s + `"` }

func (a *Applier) Apply(tx *TxEvt) error {
	if tx.CommitLSN <= a.Last {
		a.Dups++
		return nil
	}
	ctx := bg()
	if _, err := a.c.Exec(ctx, "BEGIN"); err != nil {
		return err
	}
	for i := range tx.Chgs {
		if err := a.change(&tx.Chgs[i]); err != nil {
			a.c.Exec(ctx, "ROLLBACK")
			return fmt.Errorf("apply %s: %w", tx.Chgs[i].String(), err)
		}
		a.Changes++
	}
	if a.Connect != nil {
		if _, err := a.c.Exec(ctx, "UPDATE applied_lsn SET lsn = $1, endl = $2 WHERE id = 1", int64(tx.CommitLSN), int64(tx.EndLSN)); err != nil {
			a.c.Exec(ctx, "ROLLBACK")
			return err
		}
	}
	if _, err := a.c.Exec(ctx, "COMMIT"); err != nil {
		return err
	}
	a.Last, a.LastEnd = tx.CommitLSN, tx.EndLSN
	a.Applied++
	return nil
}

// Resync reopens the connection and reads the applied position back.
func (a *Applier) Resync() error {
	var err error
	for i := 0; i < 240; i++ {
		if a.c != nil {
			a.c.Close(bg())
		}
		var c *pgx.Conn
		if c, err = a.Connect(); err == nil {
			var l, e int64
			if err = c.QueryRow(bg(), "SELECT lsn, endl FROM applied_lsn WHERE id = 1").Scan(&l, &e); err == nil {
				a.c = c
				a.Last, a.LastEnd = pglogrepl.LSN(l), pglogrepl.LSN(e)
				return nil
			}
			c.Close(bg())
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func argOf(c *Chg, tp Tuple, name string) any {
	v := tp[name]
	if v.Null {
		return nil
	}
	var oid uint32
	for _, col := range c.Cols {
		if col.Name == name {
			oid = col.DataType
		}
	}
	return norm(oid, v)
}

func (a *Applier) change(c *Chg) error {
	dst, ok := a.dst[c.Rel]
	if !ok {
		return fmt.Errorf("no destination for %s", c.Rel)
	}
	ctx := bg()
	if c.Op == 'T' {
		var ds []string
		for _, r := range c.TruncOf {
			ds = append(ds, a.dst[r])
		}
		_, err := a.c.Exec(ctx, "TRUNCATE "+strings.Join(ds, ", "))
		return err
	}
	var names, ph []string
	var args []any
	for i, col := range c.Cols {
		if c.Op == 'D' {
			break
		}
		if c.New[col.Name].Unchanged {
			continue
		}
		names = append(names, qi(col.Name))
		args = append(args, argOf(c, c.New, col.Name))
		ph = append(ph, fmt.Sprintf("$%d::%s", len(args), typeOf(col.DataType)))
		_ = i
	}
	where := func(from Tuple, args []any) (string, []any) {
		keys := c.KeyCols()
		if len(keys) == 0 || c.Ident == 'f' && c.OldKind == 'O' && false {
			keys = nil
			for _, col := range c.Cols {
				keys = append(keys, col.Name)
			}
		}
		var w []string
		for _, k := range keys {
			var oid uint32
			for _, col := range c.Cols {
				if col.Name == k {
					oid = col.DataType
				}
			}
			args = append(args, argOf(c, from, k))
			w = append(w, fmt.Sprintf("%s IS NOT DISTINCT FROM $%d::%s", qi(k), len(args), typeOf(oid)))
		}
		return strings.Join(w, " AND "), args
	}
	switch c.Op {
	case 'I':
		_, err := a.c.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", dst, strings.Join(names, ","), strings.Join(ph, ",")), args...)
		return err
	case 'U':
		from := c.Old
		if c.OldKind == 0 {
			from = c.New
		}
		var set []string
		for i := range names {
			set = append(set, names[i]+" = "+ph[i])
		}
		w, wargs := where(from, args)
		tag, err := a.c.Exec(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", dst, strings.Join(set, ","), w), wargs...)
		if err == nil && tag.RowsAffected() != 1 {
			err = fmt.Errorf("UPDATE hit %d rows, want 1", tag.RowsAffected())
		}
		return err
	case 'D':
		w, wargs := where(c.Old, nil)
		tag, err := a.c.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", dst, w), wargs...)
		if err == nil && tag.RowsAffected() != 1 {
			err = fmt.Errorf("DELETE hit %d rows, want 1", tag.RowsAffected())
		}
		return err
	}
	return fmt.Errorf("unknown op %c", c.Op)
}

func ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(bg(), d)
}

func pgxConnect(ctx context.Context, url string) (*pgx.Conn, error) { return pgx.Connect(ctx, url) }
