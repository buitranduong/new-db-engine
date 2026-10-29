package shadow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Applier sends the changes of one source transaction to STASK as one STASK
// transaction. The control row (applied LSN) is updated in the same
// transaction, so after any crash the control row says exactly which source
// transactions are in STASK.
type Applier struct {
	c         *pgconn.PgConn
	slot      string
	chunk     int
	pending   []stmt
	started   bool
	hasPost   bool
	txRows    int
	anomalies int64
	strict    bool
	logf      func(string, ...any)
	stmtsSent int64
	prep      map[string]string // SQL text -> prepared statement name on this connection
}

const ctlTable = "shadow_ctl"

func newApplier(c *pgconn.PgConn, slot string, chunk int, strict bool, logf func(string, ...any)) *Applier {
	if chunk <= 0 {
		chunk = 500
	}
	return &Applier{c: c, slot: slot, chunk: chunk, strict: strict, logf: logf, prep: map[string]string{}}
}

func (a *Applier) anomaly(msg string) error {
	a.anomalies++
	if a.strict {
		return fatalf("anomaly (strict): %s", msg)
	}
	if a.anomalies <= 20 {
		a.logf("ANOMALY: %s", msg)
	}
	return nil
}

func (a *Applier) queue(s stmt) {
	a.pending = append(a.pending, s)
	a.hasPost = a.hasPost || s.post != nil
}

// begin starts a new STASK transaction (BEGIN is sent with the first chunk).
func (a *Applier) begin() {
	a.started = false
	a.pending = a.pending[:0]
	a.hasPost = false
	a.txRows = 0
}

// add queues statements of one change. A statement with a post hook forces an
// immediate flush so follow-up statements keep their order.
func (a *Applier) add(ctx context.Context, ss ...stmt) error {
	for _, s := range ss {
		a.queue(s)
	}
	a.txRows++
	if a.hasPost || len(a.pending) >= a.chunk {
		return a.flush(ctx, false)
	}
	return nil
}

// flush sends the pending statements in one pipeline and reads the results.
func (a *Applier) flush(ctx context.Context, withCommit bool) error {
	if !a.started {
		a.pending = append([]stmt{{sql: "BEGIN"}}, a.pending...)
		a.started = true
	}
	batch := a.pending
	a.pending = nil
	a.hasPost = false
	if len(batch) == 0 {
		return nil
	}
	p := a.c.StartPipeline(ctx)
	for _, s := range batch {
		name, ok := a.prep[s.sql]
		if !ok && len(a.prep) < 1500 && len(s.args) > 0 {
			name = "p" + strconv.Itoa(len(a.prep))
			p.SendPrepare(name, s.sql, nil)
			a.prep[s.sql] = name
			ok = true
		}
		if ok {
			p.SendQueryPrepared(name, s.args, nil, nil)
		} else {
			p.SendQueryParams(s.sql, s.args, nil, nil, nil)
		}
	}
	p.Sync()
	if err := p.Flush(); err != nil {
		p.Close()
		return err
	}
	var first error
	i := 0
loop:
	for {
		r, err := p.GetResults()
		if err != nil {
			if first == nil {
				first = fmt.Errorf("statement %q: %w", clip(batch[min(i, len(batch)-1)].sql), err)
			}
			if !isPgErr(err) {
				break loop
			}
			i++
			continue
		}
		switch x := r.(type) {
		case *pgconn.ResultReader:
			tag, err := x.Close()
			if err != nil {
				if first == nil {
					first = fmt.Errorf("statement %q: %w", clip(batch[min(i, len(batch)-1)].sql), err)
				}
			} else if first == nil && i < len(batch) && batch[i].post != nil {
				if perr := batch[i].post(a, tag); perr != nil {
					first = perr
				}
			}
			i++
		case *pgconn.StatementDescription:
			// result of a Parse in this pipeline: no statement index
		case *pgconn.PipelineSync:
			break loop
		case nil:
			break loop
		}
	}
	cerr := p.Close()
	a.stmtsSent += int64(len(batch))
	if first != nil {
		return first
	}
	return cerr
}

func isPgErr(err error) bool { var pe *pgconn.PgError; return errors.As(err, &pe) }

func clip(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

// commit finishes the STASK transaction: drains statements with hooks, then
// updates the control row and commits, all in one round trip.
func (a *Applier) commit(ctx context.Context, endLSN uint64, commitUs int64) error {
	for a.hasPost {
		if err := a.flush(ctx, false); err != nil {
			return err
		}
	}
	upd := stmt{
		sql: "UPDATE " + ctlTable + " SET applied_lsn = $1, ntxs = ntxs + 1, nrows = nrows + $2, commit_us = $3 WHERE slot = $4",
		args: [][]byte{
			[]byte(fmt.Sprint(int64(endLSN))), []byte(fmt.Sprint(a.txRows)), []byte(fmt.Sprint(commitUs)), []byte(a.slot),
		},
		post: func(a *Applier, tag pgconn.CommandTag) error {
			if tag.RowsAffected() != 1 {
				return fatalf("control row for slot %q missing in STASK (%s)", a.slot, tag)
			}
			return nil
		},
	}
	a.queue(upd)
	a.queue(stmt{sql: "COMMIT"})
	a.hasPost = false
	err := a.flush(ctx, true)
	a.started = false
	return err
}

// abandon is called when a STASK transaction is dropped; the connection is
// closed by the caller, which rolls it back.
func (a *Applier) abandon() { a.pending = nil; a.started = false; a.hasPost = false }

func describeStmt(s stmt) string {
	var b strings.Builder
	b.WriteString(s.sql)
	return b.String()
}
