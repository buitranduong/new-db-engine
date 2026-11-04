package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// r2Equiv checks that the ranked form of R2 returns the rows of the window
// form, in the same order, on one database (docs/14 PGRANK): every
// category, three price bands, pages 1-5, plus the prefix shape (mult 3)
// and a restricted sub-population. Both forms read one snapshot.
func r2Equiv(ctx context.Context, dsn string) error {
	c, err := Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return err
	}
	defer c.Exec(ctx, "ROLLBACK")
	defer func(f string, r bool) { r2Filter, R2Ranked = f, r }(r2Filter, R2Ranked)
	bands := [][2]int{{1000000, 3000000}, {500000, 900000}, {2000000, 4500000}}
	type shape struct {
		page, mult int
		lo         string
	}
	shapes := []shape{{1, 1, ""}, {2, 1, ""}, {3, 1, ""}, {4, 1, ""}, {5, 1, ""}, {1, 3, ""}, {2, 1, "095"}}
	checks, rows := 0, 0
	var slow [2]time.Duration
	for _, cat := range cats {
		for _, b := range bands {
			r2Filter = fmt.Sprintf("category = '%s' AND price BETWEEN %d AND %d", cat, b[0], b[1])
			for _, s := range shapes {
				var got [2][][]string
				for i, ranked := range []bool{false, true} {
					R2Ranked = ranked
					t0 := time.Now()
					r, err := QueryText(ctx, c, R2Query(s.page, s.mult, s.lo))
					if err != nil {
						return fmt.Errorf("%s page %d ranked=%v: %w", r2Filter, s.page, ranked, err)
					}
					slow[i] = max(slow[i], time.Since(t0))
					got[i] = r
				}
				if a, b := join(got[0]), join(got[1]); a != b {
					return fmt.Errorf("%s page %d mult %d lo %q: window form %d rows, ranked form %d rows, first difference at row %d",
						r2Filter, s.page, s.mult, s.lo, len(got[0]), len(got[1]), firstDiff(got[0], got[1]))
				}
				checks++
				rows += len(got[0])
			}
		}
	}
	if rows == 0 {
		return fmt.Errorf("no rows in any check: the data is not loaded")
	}
	logf("R2_EQUIV OK: %d checks, %d rows identical in order; slowest window form %.1f ms, slowest ranked form %.1f ms",
		checks, rows, float64(slow[0].Microseconds())/1000, float64(slow[1].Microseconds())/1000)
	return nil
}

func join(rs [][]string) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(strings.Join(r, "\x1f"))
		b.WriteByte('\n')
	}
	return b.String()
}

func firstDiff(a, b [][]string) int {
	for i := range min(len(a), len(b)) {
		if strings.Join(a[i], "\x1f") != strings.Join(b[i], "\x1f") {
			return i
		}
	}
	return min(len(a), len(b))
}
