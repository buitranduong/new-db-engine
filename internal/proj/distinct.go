package proj

import (
	"fmt"
	"strconv"
	"strings"

	"stask/internal/schema"
)

// count(DISTINCT x) on the columnar view, in two phases as Spark's
// RewriteDistinctAggregates does: phase 1 groups by the group columns plus
// x (the dense and partitioned paths apply unchanged) and computes the
// other aggregates as partials (count, sum, min, max); phase 2 groups the
// phase-1 rows by the group columns, counting the non-NULL x and merging
// the partials. All distinct aggregates must be on the same column.

func hasDistinct(as []Agg) bool {
	for _, a := range as {
		if a.Distinct {
			return true
		}
	}
	return false
}

// distinctCol checks the distinct aggregates of q and returns their column.
func distinctCol(t *schema.Table, q *AggQuery) (int, error) {
	x := ""
	for _, a := range q.Aggs {
		if !a.Distinct {
			continue
		}
		if !strings.EqualFold(a.Fn, "count") || a.Col == "" {
			return 0, fmt.Errorf("DISTINCT is supported for count(column) only")
		}
		if x != "" && a.Col != x {
			return 0, fmt.Errorf("DISTINCT aggregates must all be on the same column")
		}
		x = a.Col
	}
	ci, ok := t.Col(x)
	if !ok {
		return 0, fmt.Errorf("unknown column %q", x)
	}
	if t.Columns[ci].T == schema.TFloat {
		return 0, fmt.Errorf("count(DISTINCT %s): float columns are not supported", x)
	}
	return ci, nil
}

// aggregateDistinct runs q (with distinct aggregates) through run, which
// must answer queries without them.
func aggregateDistinct(t *schema.Table, q *AggQuery, run func(*AggQuery) (*Result, error)) (*Result, error) {
	xi, err := distinctCol(t, q)
	if err != nil {
		return nil, err
	}
	outer := make([]Agg, len(q.Aggs))
	for i, a := range q.Aggs {
		outer[i] = Agg{Fn: a.Fn, Col: a.Col, As: a.As}
	}
	specs, err := compileAggs(t, outer)
	if err != nil {
		return nil, err
	}
	ng := len(q.GroupBy)
	inner := &AggQuery{Table: q.Table, Filters: q.Filters, GroupBy: q.GroupBy, GroupTrunc: q.GroupTrunc,
		Workers: q.Workers, Spawn: q.Spawn, Keep: q.Keep, Interrupted: q.Interrupted}
	xpos := -1 // position of x among the phase-1 group values
	for i, g := range q.GroupBy {
		if ci, _ := t.Col(g); ci == xi && (i >= len(q.GroupTrunc) || q.GroupTrunc[i] == "") {
			xpos = i
		}
	}
	if xpos < 0 {
		inner.GroupBy = append(append([]string(nil), q.GroupBy...), t.Columns[xi].Name)
		xpos = ng
	}
	// partial columns of aggregate i: count, then sum / min / max as needed
	type partial struct{ n, sum, min, max int }
	parts := make([]partial, len(q.Aggs))
	add := func(fn, col string) int {
		inner.Aggs = append(inner.Aggs, Agg{Fn: fn, Col: col, As: "p" + strconv.Itoa(len(inner.Aggs))})
		return len(inner.GroupBy) + len(inner.Aggs) - 1
	}
	for i, a := range q.Aggs {
		parts[i] = partial{-1, -1, -1, -1}
		if a.Distinct {
			continue
		}
		parts[i].n = add("count", a.Col)
		switch specs[i].fn {
		case "sum", "avg":
			parts[i].sum = add("sum", a.Col)
		case "min":
			parts[i].min = add("min", a.Col)
		case "max":
			parts[i].max = add("max", a.Col)
		}
	}
	if len(inner.Aggs) == 0 {
		add("count", "")
	}
	res, err := run(inner)
	if err != nil {
		return nil, err
	}

	groups := map[string]*group{}
	var order []*group
	var kb []byte
	for _, row := range res.Rows {
		kb = kb[:0]
		for _, v := range row[:ng] {
			switch x := v.(type) {
			case nil:
				kb = append(kb, 0)
			case string:
				kb = append(append(kb, 1), x...)
			case int64:
				kb = strconv.AppendInt(append(kb, 2), x, 10)
			default:
				kb = fmt.Appendf(append(kb, 3), "%v", x)
			}
			kb = append(kb, 0xff)
		}
		g := groups[string(kb)]
		if g == nil {
			g = &group{vals: append([]any(nil), row[:ng]...), accs: newAccs(len(specs))}
			groups[string(kb)] = g
			order = append(order, g)
		}
		for i := range specs {
			a := &g.accs[i]
			if q.Aggs[i].Distinct {
				if row[xpos] != nil {
					a.n++
				}
				continue
			}
			p, s := parts[i], &specs[i]
			n := row[p.n].(int64)
			if n == 0 {
				continue
			}
			b := acc{n: n}
			b.min, b.max, b.imin, b.imax = a.min, a.max, a.imin, a.imax
			if p.sum >= 0 {
				if s.isInt {
					b.isum = row[p.sum].(int64)
				} else {
					b.fsum = row[p.sum].(float64)
				}
			}
			for _, m := range []int{p.min, p.max} {
				if m < 0 {
					continue
				}
				if s.isInt {
					b.imin, b.imax = min(b.imin, row[m].(int64)), max(b.imax, row[m].(int64))
				} else {
					b.min, b.max = min(b.min, row[m].(float64)), max(b.max, row[m].(float64))
				}
			}
			a.merge(&b)
		}
	}
	for i := range specs {
		specs[i].nullEmpty = q.SQLNulls
	}
	seq := func(yield func(*group) bool) {
		for _, g := range order {
			if !yield(g) {
				return
			}
		}
	}
	return finish(q.GroupBy, specs, seq, len(order), q, res.Scanned), nil
}
