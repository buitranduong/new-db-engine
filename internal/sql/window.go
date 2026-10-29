package sql

import (
	"sort"
)

// Window functions of the general executor: each call's value is appended
// to every working row (slots after the grouped columns), partition by
// partition in ORDER BY order.

func computeWindows(ctx *evalCtx, rows [][]Datum, wins []*winCall) ([][]Datum, error) {
	vals := make([][]Datum, len(wins))
	for wi, w := range wins {
		v, err := windowValues(ctx, rows, w)
		if err != nil {
			return nil, err
		}
		vals[wi] = v
	}
	for i, r := range rows {
		nr := make([]Datum, len(r), len(r)+len(wins))
		copy(nr, r)
		for wi := range wins {
			nr = append(nr, vals[wi][i])
		}
		rows[i] = nr
	}
	return rows, nil
}

func windowValues(ctx *evalCtx, rows [][]Datum, w *winCall) ([]Datum, error) {
	n := len(rows)
	okeys := make([][]Datum, n)
	args := make([][]Datum, n)
	parts := map[string][]int{}
	var porder []string
	for i, r := range rows {
		ctx.drow = r
		key, err := evalKey(ctx, w.part)
		if err != nil {
			return nil, err
		}
		if _, ok := parts[key]; !ok {
			porder = append(porder, key)
		}
		parts[key] = append(parts[key], i)
		for _, s := range w.order {
			v, err := ctx.eval(s.x)
			if err != nil {
				return nil, err
			}
			okeys[i] = append(okeys[i], v)
		}
		for _, a := range w.args {
			v, err := ctx.eval(a)
			if err != nil {
				return nil, err
			}
			args[i] = append(args[i], v)
		}
	}
	out := make([]Datum, n)
	for _, pk := range porder {
		idx := parts[pk]
		var serr error
		sort.SliceStable(idx, func(a, b int) bool {
			c, err := cmpSort(okeys[idx[a]], okeys[idx[b]], w.order)
			if err != nil && serr == nil {
				serr = err
			}
			return c < 0
		})
		if serr != nil {
			return nil, serr
		}
		if err := windowPartition(ctx, w, idx, okeys, args, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func windowPartition(ctx *evalCtx, w *winCall, idx []int, okeys, args [][]Datum, out []Datum) error {
	n := len(idx)
	// peer groups: rows with equal ORDER BY keys
	peer := make([]int, n) // peer group number
	first := make([]int, n)
	last := make([]int, n)
	for i := 0; i < n; i++ {
		if i > 0 {
			c, err := cmpSort(okeys[idx[i-1]], okeys[idx[i]], w.order)
			if err != nil {
				return err
			}
			if c == 0 {
				peer[i], first[i] = peer[i-1], first[i-1]
				continue
			}
			peer[i] = peer[i-1] + 1
		}
		first[i] = i
	}
	for i := n - 1; i >= 0; i-- {
		if i < n-1 && peer[i+1] == peer[i] {
			last[i] = last[i+1]
		} else {
			last[i] = i
		}
	}
	arg := func(i, a int) Datum {
		if a < len(args[idx[i]]) {
			return args[idx[i]][a]
		}
		return Null
	}
	intArg := func(i, a int, def int64) (int64, error) {
		if a >= len(args[idx[i]]) {
			return def, nil
		}
		v := arg(i, a)
		if v.IsNull() {
			return 0, errf(codeInvalidParameter, "argument of %s must not be null", w.f.Name)
		}
		v, err := coerce(v, KInt)
		return v.I, err
	}
	switch w.f.Name {
	case "row_number":
		for i := range idx {
			out[idx[i]] = IntD(int64(i + 1))
		}
		return nil
	case "rank":
		for i := range idx {
			out[idx[i]] = IntD(int64(first[i] + 1))
		}
		return nil
	case "dense_rank":
		for i := range idx {
			out[idx[i]] = IntD(int64(peer[i] + 1))
		}
		return nil
	case "percent_rank":
		for i := range idx {
			v := 0.0
			if n > 1 {
				v = float64(first[i]) / float64(n-1)
			}
			out[idx[i]] = FloatD(v)
		}
		return nil
	case "cume_dist":
		for i := range idx {
			out[idx[i]] = FloatD(float64(last[i]+1) / float64(n))
		}
		return nil
	case "ntile":
		for i := range idx {
			k, err := intArg(i, 0, 1)
			if err != nil {
				return err
			}
			if k <= 0 {
				return errf(codeInvalidParameter, "argument of ntile must be greater than zero")
			}
			size, extra := int64(n)/k, int64(n)%k
			// the first extra buckets hold size+1 rows
			pos := int64(i)
			var b int64
			if pos < extra*(size+1) {
				b = pos / (size + 1)
			} else {
				b = extra + (pos-extra*(size+1))/max(size, 1)
			}
			out[idx[i]] = IntD(b + 1)
		}
		return nil
	case "lag", "lead":
		for i := range idx {
			off, err := intArg(i, 1, 1)
			if err != nil {
				return err
			}
			if w.f.Name == "lag" {
				off = -off
			}
			j := int64(i) + off
			if j >= 0 && j < int64(n) {
				out[idx[i]] = arg(int(j), 0)
			} else {
				out[idx[i]] = arg(i, 2)
			}
		}
		return nil
	}
	// frame-based: value functions and aggregates
	bounds, err := frameBounds(ctx, w, idx, okeys, peer, first, last)
	if err != nil {
		return err
	}
	switch w.f.Name {
	case "first_value", "last_value", "nth_value":
		for i := range idx {
			lo, hi := bounds[i][0], bounds[i][1]
			v := Null
			switch w.f.Name {
			case "first_value":
				if lo <= hi {
					v = arg(lo, 0)
				}
			case "last_value":
				if lo <= hi {
					v = arg(hi, 0)
				}
			default:
				k, err := intArg(i, 1, 1)
				if err != nil {
					return err
				}
				if k <= 0 {
					return errf(codeInvalidParameter, "argument of nth_value must be greater than zero")
				}
				if j := lo + int(k) - 1; j <= hi {
					v = arg(j, 0)
				}
			}
			out[idx[i]] = v
		}
		return nil
	}
	if !aggNames[w.f.Name] {
		return errf(codeUndefinedFunction, "window function %s does not exist", w.f.Name)
	}
	call := &aggCall{f: w.f}
	call.args = w.args
	incremental := true
	for i := range idx {
		if bounds[i][0] != 0 || i > 0 && bounds[i][1] < bounds[i-1][1] {
			incremental = false
		}
	}
	var state aggImpl
	fed := -1
	for i := range idx {
		lo, hi := bounds[i][0], bounds[i][1]
		if !incremental || state == nil {
			if state, err = newAgg(call); err != nil {
				return err
			}
			fed = lo - 1
		}
		for j := fed + 1; j <= hi; j++ {
			if err := state.add(args[idx[j]], nil); err != nil {
				return err
			}
		}
		fed = max(fed, hi)
		v, err := state.result()
		if err != nil {
			return err
		}
		out[idx[i]] = v
	}
	return nil
}

// frameBounds gives each row's frame as inclusive positions in the
// partition (lo > hi: empty).
func frameBounds(ctx *evalCtx, w *winCall, idx []int, okeys [][]Datum, peer, first, last []int) ([][2]int, error) {
	n := len(idx)
	out := make([][2]int, n)
	f := w.frame
	if f == nil {
		for i := range out {
			if len(w.order) == 0 {
				out[i] = [2]int{0, n - 1}
			} else {
				out[i] = [2]int{0, last[i]}
			}
		}
		return out, nil
	}
	offset := func(b FrameBound) (Datum, error) {
		if b.N == nil {
			return Null, nil
		}
		v, err := ctx.eval(b.N)
		if err != nil {
			return Null, err
		}
		if v.IsNull() {
			return Null, errf(codeInvalidParameter, "frame offset must not be null")
		}
		return v, nil
	}
	so, err := offset(f.Start)
	if err != nil {
		return nil, err
	}
	eo, err := offset(f.End)
	if err != nil {
		return nil, err
	}
	// peer group boundaries, for GROUPS
	var gfirst, glast []int
	if f.Mode == "groups" {
		for i := 0; i < n; i++ {
			if i == 0 || peer[i] != peer[i-1] {
				gfirst = append(gfirst, i)
				glast = append(glast, last[i])
			}
		}
	}
	rangePos := func(i int, d Datum, sign int, start bool) (int, error) {
		if len(w.order) != 1 {
			return 0, errf(codeWindowing, "RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column")
		}
		cur := okeys[idx[i]][0]
		if cur.IsNull() {
			if start {
				return first[i], nil
			}
			return last[i], nil
		}
		if w.order[0].desc {
			sign = -sign
		}
		op := "+"
		if sign < 0 {
			op = "-"
		}
		target, err := (&evalCtx{}).binary(&Binary{Op: op, L: &Lit{cur}, R: &Lit{d}})
		if err != nil {
			return 0, err
		}
		before := func(v Datum) bool { // v sorts before target
			if v.IsNull() {
				return false
			}
			c, _ := compare(v, target)
			if w.order[0].desc {
				c = -c
			}
			return c < 0
		}
		if start {
			j := 0
			for j < n && (okeys[idx[j]][0].IsNull() != cur.IsNull() || before(okeys[idx[j]][0])) {
				j++
			}
			return j, nil
		}
		j := n - 1
		for j >= 0 && (okeys[idx[j]][0].IsNull() != cur.IsNull() || !before(okeys[idx[j]][0]) && func() bool {
			c, _ := compare(okeys[idx[j]][0], target)
			if w.order[0].desc {
				c = -c
			}
			return c > 0
		}()) {
			j--
		}
		return j, nil
	}
	pos := func(i int, b FrameBound, d Datum, start bool) (int, error) {
		switch b.Kind {
		case "unbounded preceding":
			return 0, nil
		case "unbounded following":
			return n - 1, nil
		case "current row":
			switch f.Mode {
			case "rows":
				return i, nil
			}
			if start {
				return first[i], nil
			}
			return last[i], nil
		}
		sign := 1
		if b.Kind == "preceding" {
			sign = -1
		}
		switch f.Mode {
		case "rows":
			k, err := coerce(d, KInt)
			if err != nil {
				return 0, err
			}
			return i + sign*int(k.I), nil
		case "groups":
			k, err := coerce(d, KInt)
			if err != nil {
				return 0, err
			}
			g := peer[i] + sign*int(k.I)
			if g < 0 {
				if start {
					return 0, nil
				}
				return -1, nil
			}
			if g >= len(gfirst) {
				if start {
					return n, nil
				}
				return n - 1, nil
			}
			if start {
				return gfirst[g], nil
			}
			return glast[g], nil
		}
		return rangePos(i, d, sign, start)
	}
	for i := 0; i < n; i++ {
		lo, err := pos(i, f.Start, so, true)
		if err != nil {
			return nil, err
		}
		hi := n - 1
		if f.End.Kind != "" {
			if hi, err = pos(i, f.End, eo, false); err != nil {
				return nil, err
			}
		} else if f.Mode == "rows" {
			hi = i
		} else {
			hi = last[i]
		}
		out[i] = [2]int{max(lo, 0), min(hi, n-1)}
	}
	return out, nil
}
