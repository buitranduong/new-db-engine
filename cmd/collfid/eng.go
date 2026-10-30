package main

import (
	"fmt"
	"os"
	"strings"

	"stask/internal/core"
	"stask/internal/sql"
)

// engineOrder loads vals into a STASK table (with an index on v, to show the
// planner does not use it for ORDER BY under a non-C collation) and returns
// the ids in the order SELECT ... ORDER BY v gives under collation coll.
func engineOrder(vals []string, coll string) ([]int, error) {
	dir, err := os.MkdirTemp("", "collfid-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := sql.SetDefaultCollation(coll); err != nil {
		return nil, err
	}
	defer sql.SetDefaultCollation("C")
	e, err := core.Open(core.Options{Dir: dir, SegmentSize: 4 << 20, NoCheckpoint: true, Logf: func(string, ...any) {}})
	if err != nil {
		return nil, err
	}
	defer e.Close()
	s := sql.NewSession(e)
	run := func(q string) (*sql.Result, error) {
		stmts, err := sql.Parse(q)
		if err != nil {
			return nil, err
		}
		var r *sql.Result
		for _, st := range stmts {
			if r, err = s.Exec(st, nil); err != nil {
				return nil, err
			}
		}
		return r, nil
	}
	if _, err := run(`CREATE TABLE cf (id BIGINT PRIMARY KEY, v TEXT); CREATE INDEX cf_v ON cf (v)`); err != nil {
		return nil, err
	}
	for i := 0; i < len(vals); i += 500 {
		var b strings.Builder
		b.WriteString("INSERT INTO cf VALUES ")
		for j := i; j < len(vals) && j < i+500; j++ {
			if j > i {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,'%s')", j, strings.ReplaceAll(vals[j], "'", "''"))
		}
		if _, err := run(b.String()); err != nil {
			return nil, err
		}
	}
	r, err := run(`SELECT id FROM cf ORDER BY v`)
	if err != nil {
		return nil, err
	}
	ids := make([]int, len(r.Rows))
	for i, row := range r.Rows {
		ids[i] = int(row[0].I)
	}
	return ids, nil
}
