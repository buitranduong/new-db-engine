package sql

import (
	"stask/internal/core"
)

// run executes the join over the materialized rows of its inputs: a hash join in RAM (the right side is
// the build side, or the left one when it is much smaller), with the right table read by key when the
// left side is small (joinpush.go).
func (p *joinPlan) run(qx *qexec, outer *evalCtx) ([][]Datum, error) {
	lrows, err := p.l.run(qx, outer)
	if err != nil {
		return nil, err
	}
	sp, lateral := p.r.(*subPlan)
	lateral = lateral && sp.lateral
	var rrows [][]Datum
	if !lateral {
		have := false
		if !joinPushOff.Load() {
			var ok bool
			if rrows, ok, err = p.indexJoinRows(qx, outer, lrows); err != nil {
				return nil, err
			}
			have = ok
		}
		if !have {
			if rrows, err = p.r.run(qx, outer); err != nil {
				return nil, err
			}
		}
	}
	ctx := qx.ctx(outer)
	nr := len(p.r.cols())
	key := func(xs []Expr, row []Datum) (string, bool, error) {
		ctx.drow = row
		vals := make([]Datum, len(xs))
		for i, x := range xs {
			v, err := ctx.eval(x)
			if err != nil {
				return "", false, err
			}
			if v.IsNull() {
				return "", false, nil
			}
			vals[i] = v
		}
		return groupKey(normKeys(vals)), true, nil
	}
	combined := func(l, r []Datum) []Datum {
		row := make([]Datum, 0, p.nl+nr)
		if l == nil {
			row = append(row, make([]Datum, p.nl)...)
		} else {
			row = append(row, l...)
		}
		if r == nil {
			row = append(row, make([]Datum, nr)...)
		} else {
			row = append(row, r...)
		}
		for _, m := range p.merge { // the raw left copies (see compileFrom), before any merge
			row = append(row, row[m[0]])
		}
		return row
	}
	steps := 0 // join pairs visited: polls for cancel every few thousand
	tick := func() error {
		if steps++; steps >= 4096 {
			if steps = 0; qx.s.interrupted() {
				return core.ErrInterrupted
			}
		}
		return nil
	}
	rMatched := make([]bool, len(rrows))
	var out [][]Datum
	if !lateral && !joinPushOff.Load() && len(p.lkeys) > 0 && len(rrows) >= buildLeftMi && len(lrows)*2 <= len(rrows) {
		// the left side is the smaller one: build the hash table on it and probe with the right rows;
		// the output is put back in the order of the left-build loop below (left row, then right row)
		joinStats.buildLeft.Add(1)
		index := map[string][]int{}
		for i, l := range lrows {
			k, ok, err := key(p.lkeys, l)
			if err != nil {
				return nil, err
			}
			if ok {
				index[k] = append(index[k], i)
			}
		}
		byLeft := make([][][]Datum, len(lrows))
		for ri, r := range rrows {
			k, ok, err := key(p.rkeys, r)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			for _, li := range index[k] {
				if err := tick(); err != nil {
					return nil, err
				}
				if p.any && len(byLeft[li]) > 0 {
					continue
				}
				row := combined(lrows[li], r)
				if p.residual != nil {
					ctx.drow = row
					v, err := ctx.evalBool(p.residual)
					if err != nil {
						return nil, err
					}
					if !v.Bool() {
						continue
					}
				}
				rMatched[ri] = true
				byLeft[li] = append(byLeft[li], row)
				if err := qx.charge(row); err != nil {
					return nil, err
				}
			}
		}
		for li, l := range lrows {
			if len(byLeft[li]) > 0 {
				out = append(out, byLeft[li]...)
			} else if p.kind == "left" || p.kind == "full" {
				out = append(out, combined(l, nil))
			}
		}
		return p.unmatchedRight(out, rrows, rMatched, combined), nil
	}
	var index map[string][]int
	buildIndex := func(rows [][]Datum) error {
		index = map[string][]int{}
		if len(p.rkeys) == 0 {
			return nil
		}
		for i, r := range rows {
			k, ok, err := key(p.rkeys, r)
			if err != nil {
				return err
			}
			if ok {
				index[k] = append(index[k], i)
			}
		}
		return nil
	}
	if !lateral {
		if err := buildIndex(rrows); err != nil {
			return nil, err
		}
	}
	for _, l := range lrows {
		if err := tick(); err != nil {
			return nil, err
		}
		if lateral {
			lctx := qx.ctx(outer)
			lctx.drow = l
			rel, err := sp.p.run(qx, lctx)
			if err != nil {
				return nil, err
			}
			rrows = rel.rows
			if err := buildIndex(rrows); err != nil {
				return nil, err
			}
		}
		var cand []int
		if len(p.lkeys) > 0 {
			k, ok, err := key(p.lkeys, l)
			if err != nil {
				return nil, err
			}
			if ok {
				cand = index[k]
			}
		} else {
			cand = make([]int, len(rrows))
			for i := range cand {
				cand[i] = i
			}
		}
		matched := false
		for _, ri := range cand {
			if err := tick(); err != nil {
				return nil, err
			}
			row := combined(l, rrows[ri])
			if p.residual != nil {
				ctx.drow = row
				v, err := ctx.evalBool(p.residual)
				if err != nil {
					return nil, err
				}
				if !v.Bool() {
					continue
				}
			}
			matched = true
			if !lateral {
				rMatched[ri] = true
			}
			out = append(out, row)
			if err := qx.charge(row); err != nil {
				return nil, err
			}
			if p.any {
				break
			}
		}
		if !matched && (p.kind == "left" || p.kind == "full") {
			out = append(out, combined(l, nil))
		}
	}
	return p.unmatchedRight(out, rrows, rMatched, combined), nil
}

// unmatchedRight appends the right rows no left row matched, for RIGHT and FULL joins.
func (p *joinPlan) unmatchedRight(out, rrows [][]Datum, rMatched []bool, combined func(l, r []Datum) []Datum) [][]Datum {
	if p.kind == "right" || p.kind == "full" {
		for i, r := range rrows {
			if !rMatched[i] {
				row := combined(nil, r)
				for _, m := range p.merge {
					row[m[0]] = row[m[1]]
				}
				out = append(out, row)
			}
		}
	}
	return out
}
