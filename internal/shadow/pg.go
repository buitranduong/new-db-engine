// Package shadow keeps STASK in sync with a live PostgreSQL (the system of
// record) using logical replication (pgoutput), without serving clients, and
// compares the two. It talks to STASK only through the PostgreSQL protocol
// (pgconn), as any client would.
package shadow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// FatalError marks an error that a reconnect cannot fix (schema drift,
// divergence, bad configuration). Run returns it instead of retrying.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

func fatalf(format string, a ...any) error { return &FatalError{fmt.Errorf(format, a...)} }

// IsFatal reports whether err is (or wraps) a FatalError.
func IsFatal(err error) bool { var f *FatalError; return errors.As(err, &f) }

// connect opens one connection. replication=true makes it a logical
// replication walsender connection. TimeZone is pinned to UTC and DateStyle
// to ISO on both systems so text output of dates is comparable.
func connect(ctx context.Context, url string, replication bool) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["application_name"] = "stashadow"
	cfg.RuntimeParams["timezone"] = "UTC"
	cfg.RuntimeParams["datestyle"] = "ISO, YMD"
	if replication {
		cfg.RuntimeParams["replication"] = "database"
	}
	cfg.ConnectTimeout = 10 * time.Second
	return pgconn.ConnectConfig(ctx, cfg)
}

// Rows is a fully buffered text result; a nil Cell is SQL NULL.
type Rows struct {
	Cols []string
	OIDs []uint32
	Data [][]*string
	Tag  string
}

// query runs one statement with text parameters (nil arg = NULL) and returns
// all rows as text.
func query(ctx context.Context, c *pgconn.PgConn, sql string, args ...*string) (*Rows, error) {
	pv := make([][]byte, len(args))
	for i, a := range args {
		if a != nil {
			pv[i] = []byte(*a)
		}
	}
	rr := c.ExecParams(ctx, sql, pv, nil, nil, nil)
	out := &Rows{}
	for _, f := range rr.FieldDescriptions() {
		out.Cols = append(out.Cols, f.Name)
		out.OIDs = append(out.OIDs, f.DataTypeOID)
	}
	for rr.NextRow() {
		vals := rr.Values()
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v != nil {
				s := string(v)
				row[i] = &s
			}
		}
		out.Data = append(out.Data, row)
	}
	tag, err := rr.Close()
	out.Tag = tag.String()
	if err != nil {
		return nil, err
	}
	if len(out.Cols) == 0 {
		for _, f := range rr.FieldDescriptions() {
			out.Cols = append(out.Cols, f.Name)
			out.OIDs = append(out.OIDs, f.DataTypeOID)
		}
	}
	return out, nil
}

func s(v string) *string { return &v }

// exec runs a statement list in the simple protocol, ignoring rows.
func exec(ctx context.Context, c *pgconn.PgConn, sql string) error {
	_, err := c.Exec(ctx, sql).ReadAll()
	return err
}

// qi quotes an identifier.
func qi(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func qiList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = qi(n)
	}
	return strings.Join(q, ", ")
}

// ql quotes a string literal.
func ql(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

func isPgCode(err error, codes ...string) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		for _, c := range codes {
			if pe.Code == c {
				return true
			}
		}
	}
	return false
}

func asPgErr(err error, target **pgconn.PgError) bool { return errors.As(err, target) }
