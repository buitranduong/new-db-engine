package sql

import (
	"stask/internal/schema"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"
)

// Memory budget of the general executor, which holds scanned tables, join
// results and set-returning function results in memory (no spill to disk
// yet, docs/12 P2). A query past its budget, or one that would take the
// node past the shared one, fails with 53200 instead of the process
// running out of memory, as CockroachDB's --max-sql-memory monitors do.
// Rows are charged as they are materialized; the reservation is taken
// from the shared budget in steps of memStep and returned when the query
// ends.
var (
	QueryMemLimit int64 // bytes one query may hold (0 = no limit)
	SQLMemLimit   int64 // bytes all queries together may hold (0 = no limit)
	sqlMemUsed    atomic.Int64
)

const memStep = 1 << 20

var datumSize = int64(unsafe.Sizeof(Datum{}))

func rowBytes(r []Datum) int64 {
	n := 24 + int64(len(r))*datumSize
	for i := range r {
		n += int64(len(r[i].S))
	}
	return n
}

// charge accounts a materialized row.
func (qx *qexec) charge(r []Datum) error {
	qx.memUsed += rowBytes(r)
	if qx.memUsed <= qx.memReserved {
		return nil
	}
	if lim := qx.limit(); lim > 0 && qx.memUsed > lim {
		return qx.overBudget(lim)
	}
	step := max(int64(memStep), qx.memUsed-qx.memReserved)
	if SQLMemLimit > 0 && sqlMemUsed.Add(step) > SQLMemLimit {
		sqlMemUsed.Add(-step)
		return errf("53200", "out of memory: queries in progress hold the node's budget of %d MB (-sql-mem-mb)", SQLMemLimit>>20)
	}
	if SQLMemLimit <= 0 {
		sqlMemUsed.Add(step)
	}
	qx.memReserved += step
	return nil
}

func (qx *qexec) chargeAll(rows [][]Datum) error {
	for _, r := range rows {
		if err := qx.charge(r); err != nil {
			return err
		}
	}
	return nil
}

// release returns the query's reservation.
func (qx *qexec) release() {
	sqlMemUsed.Add(-qx.memReserved)
	qx.memReserved, qx.memUsed = 0, 0
}

// SQLMemInUse is the memory reserved by queries in progress.
func SQLMemInUse() int64 { return sqlMemUsed.Load() }

// work_mem is the per-statement memory budget a session may set
// (SET work_mem = '4MB'; a bare number is kB as in PostgreSQL). It can only
// tighten the node's -query-mem-mb budget. A statement that holds more
// than it sorts, joins and aggregates through the spill operators (temp
// files in -spill-dir) or, with spilling off or for shapes the spill path
// does not cover, fails with 53200. Unlike PostgreSQL's per-operator
// work_mem it bounds the whole statement; each spilling operator keeps a
// sixth of it. 0 / unset = only the node budget.
const WorkMemVar = "work_mem"

// workMemBytes parses the session's work_mem; 0 when unset or invalid.
func (s *Session) workMemBytes() int64 {
	v := strings.TrimSpace(strings.ToLower(s.vars[WorkMemVar]))
	if v == "" || v == "default" {
		return 0
	}
	mult := int64(1 << 10) // kB
	for _, u := range []struct {
		suf string
		m   int64
	}{{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"tb", 1 << 40}, {"b", 1}} {
		if strings.HasSuffix(v, u.suf) {
			v, mult = strings.TrimSpace(strings.TrimSuffix(v, u.suf)), u.m
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * mult
}

// limit is the budget of this query: the node's, tightened by work_mem.
func (qx *qexec) limit() int64 {
	l := QueryMemLimit
	if qx.s != nil {
		if w := qx.s.workMemBytes(); w > 0 && (l <= 0 || w < l) {
			l = w
		}
	}
	return l
}

func (qx *qexec) overBudget(lim int64) error {
	if qx.s != nil && qx.s.workMemBytes() == lim {
		return errf("53200", "out of memory: the query holds more than work_mem of %d kB", lim>>10)
	}
	return errf("53200", "out of memory: the query holds more than its budget of %d MB (-query-mem-mb)", lim>>20)
}

// errSimpleSpill tells runHere that the single-table path would hold more
// than the statement's budget and that the general executor, which spills,
// should run it.
var errSimpleSpill = errf("53200", "out of memory: single-table path over budget")

// simpleBudget is the budget the single-table executor holds itself to
// (group table, rows to sort or de-duplicate). The node's query budget
// applies; work_mem tightens it.
func (s *Session) simpleBudget() int64 {
	l := QueryMemLimit
	if w := s.workMemBytes(); w > 0 && (l <= 0 || w < l) {
		l = w
	}
	return l
}

// simpleOver is the error of the single-table path past its budget: the
// retry sentinel when spilling is on, 53200 otherwise.
func (s *Session) simpleOver(lim int64) error {
	if SpillDir != "" {
		return errSimpleSpill
	}
	return (&qexec{s: s}).overBudget(lim)
}

// schemaRowBytes estimates the memory of a stored row.
func schemaRowBytes(r schema.Row) int64 {
	n := 24 + int64(len(r))*int64(unsafe.Sizeof(schema.Value{}))
	for i := range r {
		n += int64(len(r[i].S))
	}
	return n
}
