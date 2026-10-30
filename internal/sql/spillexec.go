package sql

import (
	"encoding/binary"
	"errors"
	"math"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/spill"
)

// Spill to disk for the general executor (docs/12 P2.4, docs/05 §12).
//
// A query that runs past its memory budget (53200, membudget.go) is run a
// second time in spill mode when SpillDir is set. In spill mode a SELECT
// of the shapes listed at selPlan.spillable is executed as a pipeline of
// push sources instead of materialized relations: scan -> hash join ->
// filter -> hash aggregate -> projection -> DISTINCT -> ORDER BY ->
// LIMIT, where the join, aggregate, DISTINCT and sort are the
// internal/spill operators and keep at most opMem bytes each in memory.
// Other shapes run as before and fail with 53200 as before. The first
// pass is not wasted for queries that fit; one that does not fit pays
// the work done up to the point where the budget broke, once.

var (
	// SpillDir is the directory of temp files; empty disables spilling.
	SpillDir string
	// SpillMaxBytes caps the temp data of one query (0 = no limit).
	SpillMaxBytes int64
	// SpillOpMem is the memory of one spilling operator (0 = a sixth of
	// the query budget, or 64 MB without one).
	SpillOpMem int64
)

type spillState struct {
	m     *spill.Manager
	opMem int64
}

var errStop = errors.New("spill: enough rows") // LIMIT reached

func isMemErr(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "53200"
}

// runPlan runs the plan, in spill mode if the first run breaks the budget.
func (qx *qexec) runPlan(p qplan) (*relation, error) {
	rel, err := p.run(qx, nil)
	if err == nil || SpillDir == "" || !isMemErr(err) || qx.sp != nil {
		return rel, err
	}
	qx.release()
	qx.subCache = map[*boundSub]*relation{}
	qx.inSets = map[*boundSub]*inSet{}
	qx.hashSubs = nil
	qx.s.spilled = true
	qx.cteCache = map[*cteDef]*relation{}
	sm := spill.NewManager(SpillDir, SpillMaxBytes, qx.s.checkInterrupt)
	defer sm.Close()
	op := SpillOpMem
	if lim := qx.limit(); op <= 0 || lim > 0 && op > lim/6 {
		op = 64 << 20
		if lim > 0 {
			op = max(lim/6, 64<<10)
		}
	}
	qx.sp = &spillState{m: sm, opMem: op}
	rel, err = p.run(qx, nil)
	switch {
	case errors.Is(err, spill.ErrQuota):
		err = errf("53100", "%v (-spill-max-mb)", err)
	case errors.Is(err, spill.ErrCorrupt):
		err = errf(codeIOError, "%v", err)
	case errors.Is(err, spill.ErrSkew):
		err = errf("53200", "out of memory: %v even after spilling to disk", err)
	}
	return rel, err
}

// hold reserves n bytes of the budget for an operator until the returned
// function is called.
func (qx *qexec) hold(n int64) (func(), error) {
	if err := qx.chargeN(n); err != nil {
		return nil, err
	}
	return func() { qx.memUsed -= n }, nil
}

// ---- row codec ----

// The codec writes the four fields of Datum. A new field there must be
// added here (TestSpillCodecCoversDatum fails otherwise).
func encodeRow(dst []byte, r []Datum) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(r)))
	for i := range r {
		d := &r[i]
		var m byte
		if d.I != 0 {
			m |= 1
		}
		if math.Float64bits(d.F) != 0 {
			m |= 2
		}
		if d.S != "" {
			m |= 4
		}
		dst = append(dst, byte(d.K), m)
		if m&1 != 0 {
			dst = binary.AppendVarint(dst, d.I)
		}
		if m&2 != 0 {
			dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(d.F))
		}
		if m&4 != 0 {
			dst = binary.AppendUvarint(dst, uint64(len(d.S)))
			dst = append(dst, d.S...)
		}
	}
	return dst
}

func decodeRow(b []byte) ([]Datum, error) {
	bad := errors.New("spill: malformed row")
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return nil, bad
	}
	b = b[k:]
	r := make([]Datum, n)
	for i := range r {
		if len(b) < 2 {
			return nil, bad
		}
		r[i].K = Kind(b[0])
		m := b[1]
		b = b[2:]
		if m&1 != 0 {
			v, k := binary.Varint(b)
			if k <= 0 {
				return nil, bad
			}
			r[i].I, b = v, b[k:]
		}
		if m&2 != 0 {
			if len(b) < 8 {
				return nil, bad
			}
			r[i].F, b = math.Float64frombits(binary.LittleEndian.Uint64(b)), b[8:]
		}
		if m&4 != 0 {
			l, k := binary.Uvarint(b)
			if k <= 0 || l > uint64(len(b)-k) {
				return nil, bad
			}
			r[i].S, b = string(b[k:k+int(l)]), b[k+int(l):]
		}
	}
	return r, nil
}

var rowCodec = spill.Codec[[]Datum]{Enc: encodeRow, Dec: decodeRow, Size: rowBytes}

type rowSource = spill.Source[[]Datum]

// ---- SELECT in spill mode ----

// spillable: the shapes the pipeline covers. Window functions, set-
// returning functions in the select list, DISTINCT ON, LIMIT BY, grouping
// sets and aggregates whose state grows with the group (DISTINCT, ORDER BY
// inside the call, array_agg, string_agg, uniq, quantiles, percentiles)
// are not.
func (p *selPlan) spillable() bool {
	if p.from == nil || len(p.wins) > 0 || len(p.srfs) > 0 || p.distinctOn != nil || p.lbBy != nil || p.limitBy != nil {
		return false
	}
	if !p.grouped {
		return true
	}
	if len(p.sets) != 1 || p.zeroRollup || len(p.sets[0]) != len(p.groupExprs) {
		return false
	}
	for _, i := range p.sets[0] {
		if i < 0 {
			return false
		}
	}
	for _, a := range p.aggs {
		if a.f.Distinct || len(a.order) > 0 {
			return false
		}
		switch a.f.Name {
		case "count", "sum", "avg", "min", "max", "countif", "sumif", "avgif", "minif", "maxif",
			"bool_and", "bool_or", "every", "stddev", "stddev_samp", "stddevsamp", "stddev_pop", "stddevpop",
			"variance", "var_samp", "varsamp", "var_pop", "varpop", "argmax", "argmin", "any", "anylast":
		default:
			return false
		}
	}
	return true
}

// runSpill runs the select as a spilling pipeline; ok=false: not a shape
// it covers (run as usual).
func (p *selPlan) runSpill(qx *qexec, outer *evalCtx) (rel *relation, ok bool, err error) {
	if !p.spillable() {
		return nil, false, nil
	}
	nItems := len(p.items)
	limit, offset, err := limitValues(qx.ctx(outer), p.limit, p.offset)
	if err != nil {
		return nil, true, err
	}
	var res [][]Datum
	skip := offset
	final := func(o []Datum) error { // o: items, then sort keys
		if limit == 0 {
			return errStop
		}
		if skip > 0 {
			skip--
			return nil
		}
		vals := o[:nItems:nItems]
		if len(o) > nItems {
			vals = append([]Datum(nil), vals...)
		}
		res = append(res, vals)
		if err := qx.charge(vals); err != nil {
			return err
		}
		if limit >= 0 && int64(len(res)) >= limit {
			return errStop
		}
		return nil
	}

	src := p.spillFrom(qx, outer)
	if p.where != nil {
		src = p.spillWhere(qx, outer, src)
	}
	if p.grouped {
		src = p.spillGroup(qx, outer, src)
	}
	src = p.spillProject(qx, outer, src)
	if p.distinct {
		src = p.spillDistinct(qx, outer, src)
	}
	if len(p.order) > 0 {
		src = p.spillSort(qx, src)
	}
	if err := src(final); err != nil && err != errStop {
		return nil, true, err
	}
	rel = &relation{cols: append([]relCol(nil), p.cols...), rows: res}
	unifyKinds(rel)
	return rel, true, nil
}

func (p *selPlan) spillFrom(qx *qexec, outer *evalCtx) rowSource {
	return qx.sp.fromSource(qx, outer, p.from)
}

func (p *selPlan) spillWhere(qx *qexec, outer *evalCtx, in rowSource) rowSource {
	return func(yield func([]Datum) error) error {
		ctx := qx.ctx(outer)
		n := 0
		return in(func(r []Datum) error {
			if n++; n&4095 == 0 && qx.s.interrupted() {
				return core.ErrInterrupted
			}
			ctx.drow = r
			v, err := ctx.evalBool(p.where)
			if err != nil {
				return err
			}
			if v.Bool() {
				return yield(r)
			}
			return nil
		})
	}
}

// spillProject evaluates the select list and the sort keys (appended after
// the items) and, after grouping, HAVING.
func (p *selPlan) spillProject(qx *qexec, outer *evalCtx, in rowSource) rowSource {
	return func(yield func([]Datum) error) error {
		ctx := qx.ctx(outer)
		return in(func(r []Datum) error {
			ctx.drow = r
			if p.grouped && p.having != nil {
				v, err := ctx.evalBool(p.having)
				if err != nil {
					return err
				}
				if !v.Bool() {
					return nil
				}
			}
			o := make([]Datum, len(p.items), len(p.items)+len(p.order))
			var err error
			for i, x := range p.items {
				if o[i], err = ctx.eval(x); err != nil {
					return err
				}
			}
			for _, s := range p.order {
				var v Datum
				if s.out >= 0 {
					v = o[s.out]
				} else if v, err = ctx.eval(s.x); err != nil {
					return err
				}
				o = append(o, v)
			}
			return yield(o)
		})
	}
}

type distinctRow struct{ row []Datum }

func (p *selPlan) spillDistinct(qx *qexec, outer *evalCtx, in rowSource) rowSource {
	return func(yield func([]Datum) error) error {
		release, err := qx.hold(qx.sp.opMem)
		if err != nil {
			return err
		}
		defer release()
		n := len(p.items)
		a := &spill.HashAgg[[]Datum, *distinctRow]{M: qx.sp.m, C: rowCodec, MemLimit: qx.sp.opMem,
			Key:    func(r []Datum) (string, error) { return groupKey(normKeys(r[:n])), nil },
			New:    func(r []Datum) (*distinctRow, error) { return &distinctRow{r}, nil },
			Update: func(*distinctRow, []Datum) error { return nil },
			Size:   func(d *distinctRow) int64 { return rowBytes(d.row) },
			Finish: func(d *distinctRow) error { return yield(d.row) }}
		return a.Run(in)
	}
}

func (p *selPlan) spillSort(qx *qexec, in rowSource) rowSource {
	return func(yield func([]Datum) error) error {
		release, err := qx.hold(qx.sp.opMem)
		if err != nil {
			return err
		}
		defer release()
		n := len(p.items)
		var cerr error
		s := &spill.Sorter[[]Datum]{M: qx.sp.m, C: rowCodec, MemLimit: qx.sp.opMem,
			Cmp: func(a, b []Datum) int {
				c, err := cmpSort(a[n:], b[n:], p.order)
				if err != nil && cerr == nil {
					cerr = err
				}
				return c
			}}
		defer s.Close()
		if err := in(s.Add); err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
		if err := s.Iter(yield); err != nil {
			return err
		}
		return cerr
	}
}

type gstate struct {
	rep, keys []Datum
	aggs      []aggImpl
}

// spillGroup is selPlan.group over a stream: one group per distinct key,
// rows of groups that no longer fit in memory partitioned on disk.
func (p *selPlan) spillGroup(qx *qexec, outer *evalCtx, in rowSource) rowSource {
	return func(yield func([]Datum) error) error {
		release, err := qx.hold(qx.sp.opMem)
		if err != nil {
			return err
		}
		defer release()
		ctx := qx.ctx(outer)
		vals := make([]Datum, len(p.groupExprs))
		evalVals := func(r []Datum) error {
			ctx.drow = r
			for i, x := range p.groupExprs {
				v, err := ctx.eval(x)
				if err != nil {
					return err
				}
				vals[i] = v
			}
			return nil
		}
		newState := func(rep []Datum) (*gstate, error) {
			g := &gstate{rep: rep, keys: append([]Datum(nil), vals...)}
			for _, a := range p.aggs {
				impl, err := newAgg(a)
				if err != nil {
					return nil, err
				}
				g.aggs = append(g.aggs, impl)
			}
			return g, nil
		}
		emitted := 0
		finish := func(g *gstate) error {
			row := make([]Datum, 0, p.nFrom+len(p.groupExprs)+len(p.aggs)+1)
			if g.rep != nil {
				row = append(row, g.rep[:p.nFrom]...)
			} else {
				row = append(row, make([]Datum, p.nFrom)...)
			}
			row = append(row, g.keys...)
			for _, a := range g.aggs {
				v, err := a.result()
				if err != nil {
					return err
				}
				row = append(row, v)
			}
			row = append(row, IntD(0))
			emitted++
			return yield(row)
		}
		var kb []byte
		a := &spill.HashAgg[[]Datum, *gstate]{M: qx.sp.m, C: rowCodec, MemLimit: qx.sp.opMem,
			Key: func(r []Datum) (string, error) {
				if err := evalVals(r); err != nil {
					return "", err
				}
				kb = appendGroupKey(kb[:0], normKeys(vals))
				return string(kb), nil
			},
			New: func(r []Datum) (*gstate, error) { // Key has just evaluated vals for r
				return newState(r)
			},
			Update: func(g *gstate, r []Datum) error {
				ctx.drow = r
				for ai, ag := range p.aggs {
					if ag.filter != nil {
						v, err := ctx.evalBool(ag.filter)
						if err != nil {
							return err
						}
						if !v.Bool() {
							continue
						}
					}
					args := make([]Datum, len(ag.args))
					for i, x := range ag.args {
						v, err := ctx.eval(x)
						if err != nil {
							return err
						}
						args[i] = v
					}
					if err := g.aggs[ai].add(args, nil); err != nil {
						return err
					}
				}
				return nil
			},
			Size: func(g *gstate) int64 {
				return 128 + rowBytes(g.rep) + rowBytes(g.keys) + 64*int64(len(g.aggs))
			},
			Finish: finish}
		if err := a.Run(in); err != nil {
			return err
		}
		if emitted == 0 && len(p.groupExprs) == 0 { // an empty grouping set yields a row without input
			g, err := newState(nil)
			if err != nil {
				return err
			}
			return finish(g)
		}
		return nil
	}
}

// ---- FROM in spill mode ----

func (sp *spillState) fromSource(qx *qexec, outer *evalCtx, fp fromPlan) rowSource {
	switch f := fp.(type) {
	case *tablePlan:
		return func(yield func([]Datum) error) error {
			cols := f.t.Columns
			var err error
			serr := qx.tx.ScanAll(f.t.Name, func(_ string, r schema.Row) bool {
				d := make([]Datum, len(cols))
				for i, c := range cols {
					d[i] = fromValue(c.T, r[i])
				}
				err = yield(d)
				return err == nil
			})
			if err == nil {
				err = serr
			}
			return err
		}
	case *joinPlan:
		if sub, lateral := f.r.(*subPlan); !(lateral && sub.lateral) {
			return sp.joinSource(qx, outer, f)
		}
	}
	return func(yield func([]Datum) error) error { // materialized, as in normal mode
		rows, err := fp.run(qx, outer)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := yield(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// joinSource is joinPlan.run over streams: the right side is the build
// side of a Grace hash join (or held in memory for a join without equality
// keys, charged to the budget).
func (sp *spillState) joinSource(qx *qexec, outer *evalCtx, p *joinPlan) rowSource {
	left, right := sp.fromSource(qx, outer, p.l), sp.fromSource(qx, outer, p.r)
	return func(yield func([]Datum) error) error {
		release, err := qx.hold(sp.opMem)
		if err != nil {
			return err
		}
		defer release()
		ctx := qx.ctx(outer)
		nr := len(p.r.cols())
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
			return row
		}
		steps := 0
		onProbe := func(l []Datum, cands []*spill.Entry[[]Datum]) error {
			if steps++; steps >= 4096 {
				if steps = 0; qx.s.interrupted() {
					return core.ErrInterrupted
				}
			}
			matched := false
			for _, e := range cands {
				row := combined(l, e.Row)
				if p.residual != nil {
					ctx.drow = row
					v, err := ctx.evalBool(p.residual)
					if err != nil {
						return err
					}
					if !v.Bool() {
						continue
					}
				}
				matched, e.Matched = true, true
				if err := yield(row); err != nil {
					return err
				}
				if p.any {
					break
				}
			}
			if !matched && (p.kind == "left" || p.kind == "full") {
				return yield(combined(l, nil))
			}
			return nil
		}
		onBuildDone := func(es []*spill.Entry[[]Datum]) error {
			if p.kind != "right" && p.kind != "full" {
				return nil
			}
			for _, e := range es {
				if e.Matched {
					continue
				}
				row := combined(nil, e.Row)
				for _, m := range p.merge {
					row[m[0]] = row[m[1]]
				}
				if err := yield(row); err != nil {
					return err
				}
			}
			return nil
		}
		if len(p.lkeys) == 0 { // no equality: every pair is a candidate
			var all []*spill.Entry[[]Datum]
			if err := right(func(r []Datum) error {
				all = append(all, &spill.Entry[[]Datum]{Row: r})
				return qx.charge(r)
			}); err != nil {
				return err
			}
			if err := left(func(l []Datum) error { return onProbe(l, all) }); err != nil {
				return err
			}
			return onBuildDone(all)
		}
		key := func(xs []Expr) func(r []Datum) (string, bool, error) {
			kctx := qx.ctx(outer)
			vals := make([]Datum, len(xs))
			return func(r []Datum) (string, bool, error) {
				kctx.drow = r
				for i, x := range xs {
					v, err := kctx.eval(x)
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
		}
		j := &spill.Join[[]Datum]{M: sp.m, C: rowCodec, MemLimit: sp.opMem,
			BuildKey: key(p.rkeys), ProbeKey: key(p.lkeys)}
		return j.Run(spill.Source[[]Datum](right), spill.Source[[]Datum](left), onProbe, onBuildDone)
	}
}
