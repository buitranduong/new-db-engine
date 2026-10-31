package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Load path (docs/14, branch importbin). Rows that do not exist yet, and the
// history rows (always new), are written with COPY ... FROM STDIN inside the
// batch transaction. Rows that change an existing row stay on INSERT ... ON
// CONFLICT DO UPDATE (COPY cannot update).
//
// Load modes, from the fastest to the most portable:
//
//	binary  COPY ... FROM STDIN WITH (FORMAT binary), PostgreSQL's binary COPY
//	        format (pgx CopyFrom): no text parsing on the server
//	text    COPY ... FROM STDIN (text format), for a server or proxy without
//	        binary COPY
//	insert  multi-row INSERT ... ON CONFLICT (the original path)
//
// A server refusing a mode (SQLSTATE 0A000 feature not supported, 42601 syntax,
// 22P03 invalid binary representation, 08P01 protocol violation) moves the
// run one step down; the batch is redone in its own transaction, so
// resumability is unchanged: the checkpoint is still written after the commit.

const (
	loadBinary = "binary"
	loadText   = "text"
	loadInsert = "insert"
)

func validLoad(m string) bool { return m == loadBinary || m == loadText || m == loadInsert }

// errModeRefused wraps the server's refusal of the current load mode.
type errModeRefused struct{ err error }

func (e *errModeRefused) Error() string { return "load mode refused: " + e.err.Error() }
func (e *errModeRefused) Unwrap() error { return e.err }

func modeRefusal(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case "0A000", "42601", "22P03", "08P01":
		return true
	}
	return false
}

// nextMode is the mode below m.
func nextMode(m string) string {
	if m == loadBinary {
		return loadText
	}
	return loadInsert
}

func splitIdent(name string) []string {
	parts := strings.Split(strings.ToLower(name), ".")
	for i := range parts {
		parts[i] = strings.Trim(parts[i], `"`)
	}
	return parts
}

// copyRows loads rows (values in the order of cols) into table with the
// current load mode inside tx. In insert mode it returns errUseInsert.
func (a *applier) copyRows(ctx context.Context, tx pgx.Tx, table string, cols []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	lc := make([]string, len(cols))
	for i, c := range cols {
		lc[i] = strings.ToLower(c)
	}
	var err error
	switch a.load {
	case loadBinary:
		if a.testRefuse == loadBinary || a.testRefuse == "all" {
			err = &pgconn.PgError{Code: "0A000", Message: "test: COPY BINARY refused"}
			break
		}
		var n int64
		n, err = tx.CopyFrom(ctx, pgx.Identifier(splitIdent(table)), lc, pgx.CopyFromRows(rows))
		if err == nil && n != int64(len(rows)) {
			err = fmt.Errorf("COPY into %s loaded %d rows, expected %d", table, n, len(rows))
		}
	case loadText:
		if a.testRefuse == loadText || a.testRefuse == "all" {
			err = &pgconn.PgError{Code: "0A000", Message: "test: COPY refused"}
			break
		}
		var tag pgconn.CommandTag
		q := fmt.Sprintf("COPY %s (%s) FROM STDIN", table, strings.Join(lc, ", "))
		tag, err = tx.Conn().PgConn().CopyFrom(ctx, bytes.NewReader(textCopy(rows)), q)
		if err == nil && tag.RowsAffected() != int64(len(rows)) {
			err = fmt.Errorf("COPY into %s loaded %d rows, expected %d", table, tag.RowsAffected(), len(rows))
		}
	}
	if err != nil && modeRefusal(err) {
		return &errModeRefused{err}
	}
	return err
}

// textCopy encodes rows in COPY text format.
func textCopy(rows [][]any) []byte {
	var b bytes.Buffer
	for _, r := range rows {
		for i, v := range r {
			if i > 0 {
				b.WriteByte('\t')
			}
			switch x := v.(type) {
			case nil:
				b.WriteString(`\N`)
			case string:
				for j := 0; j < len(x); j++ {
					switch c := x[j]; c {
					case '\\':
						b.WriteString(`\\`)
					case '\t':
						b.WriteString(`\t`)
					case '\n':
						b.WriteString(`\n`)
					case '\r':
						b.WriteString(`\r`)
					default:
						b.WriteByte(c)
					}
				}
			case int64:
				b.WriteString(strconv.FormatInt(x, 10))
			case float64:
				b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
			case bool:
				if x {
					b.WriteByte('t')
				} else {
					b.WriteByte('f')
				}
			case time.Time:
				b.WriteString(x.UTC().Format("2006-01-02 15:04:05.999999") + "+00")
			default:
				b.WriteString(fmt.Sprint(x))
			}
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}
