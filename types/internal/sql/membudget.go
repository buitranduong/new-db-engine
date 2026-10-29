package sql

import (
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
	if QueryMemLimit > 0 && qx.memUsed > QueryMemLimit {
		return errf("53200", "out of memory: the query holds more than its budget of %d MB (-query-mem-mb)", QueryMemLimit>>20)
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
