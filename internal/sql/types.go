package sql

// Static typing, used to describe prepared statements (parameter and
// result column types) before they run.

// typeOf infers the result kind of x. Unknown kinds are reported as text,
// which is also how PostgreSQL treats untyped literals.
func typeOf(x Expr, sc *scope, pk map[int]Kind) Kind {
	switch e := x.(type) {
	case *Lit:
		if e.V.K == KNull {
			return KText
		}
		return e.V.K
	case *Param:
		if k, ok := pk[e.N]; ok {
			return k
		}
		return KText
	case *ColRef:
		if i, err := sc.resolve(e); err == nil {
			return kindOf(sc.t.Columns[i].T)
		}
		return KText
	case *Unary:
		if e.Op == "not" {
			return KBool
		}
		return typeOf(e.X, sc, pk)
	case *Binary:
		switch e.Op {
		case "and", "or", "=", "<>", "<", "<=", ">", ">=":
			return KBool
		case "||":
			return KText
		}
		l, r := typeOf(e.L, sc, pk), typeOf(e.R, sc, pk)
		switch {
		case (l == KTime || l == KDate) && (r == KTime || r == KDate):
			if l == KDate && r == KDate {
				return KInt
			}
			return KInterval
		case l == KDate && r == KInt:
			return KDate
		case l == KTime || r == KTime || l == KDate && r == KInterval:
			return KTime
		case l == KInterval || r == KInterval:
			return KInterval
		case l == KFloat || r == KFloat:
			return KFloat
		case l == KInt && r == KInt:
			return KInt
		case l == KInt || r == KInt:
			return KInt
		}
		return KFloat
	case *IsNull, *InList, *Between, *Like:
		return KBool
	case *Cast:
		return kindOfType(e.Type)
	case *Case:
		for _, w := range e.Whens {
			if k := typeOf(w.Then, sc, pk); k != KText || e.Else == nil {
				return k
			}
		}
		return typeOf(e.Else, sc, pk)
	case *Func:
		arg := func() Kind {
			if len(e.Args) == 0 {
				return KText
			}
			return typeOf(e.Args[0], sc, pk)
		}
		switch e.Name {
		case "count", "length", "char_length":
			return KInt
		case "avg", "round", "floor", "ceil", "ceiling", "trunc", "extract_epoch", "epoch":
			if e.Name == "round" && len(e.Args) == 1 && arg() == KInt {
				return KInt
			}
			return KFloat
		case "sum":
			if arg() == KInt {
				return KInt
			}
			return KFloat
		case "min", "max", "abs", "coalesce", "nullif":
			return arg()
		case "now", "transaction_timestamp", "statement_timestamp", "clock_timestamp", "to_timestamp":
			return KTime
		case "nextval", "currval", "setval", "lastval":
			return KInt
		}
		kinds := make([]Kind, len(e.Args))
		for i, a := range e.Args {
			kinds[i] = typeOf(a, sc, pk)
		}
		return funcKind(e.Name, kinds)
	}
	return KText
}

// paramKinds infers parameter kinds from where parameters are used.
func paramKinds(st Stmt, sc *scope) map[int]Kind {
	pk := map[int]Kind{}
	set := func(x Expr, k Kind) {
		if p, ok := x.(*Param); ok && k != KNull {
			if _, done := pk[p.N]; !done {
				pk[p.N] = k
			}
		}
	}
	var visit func(x Expr)
	visit = func(x Expr) {
		walk(x, func(e Expr) {
			switch e := e.(type) {
			case *Binary:
				switch e.Op {
				case "and", "or":
					set(e.L, KBool)
					set(e.R, KBool)
				case "||":
					set(e.L, KText)
					set(e.R, KText)
				default:
					if _, ok := e.L.(*Param); !ok {
						set(e.R, typeOf(e.L, sc, pk))
					}
					if _, ok := e.R.(*Param); !ok {
						set(e.L, typeOf(e.R, sc, pk))
					}
				}
			case *InList:
				k := typeOf(e.X, sc, pk)
				for _, it := range e.List {
					set(it, k)
				}
			case *Between:
				k := typeOf(e.X, sc, pk)
				set(e.Lo, k)
				set(e.Hi, k)
			case *Like:
				set(e.X, KText)
				set(e.Pat, KText)
			case *Cast:
				set(e.X, kindOfType(e.Type))
			case *Unary:
				if e.Op == "not" {
					set(e.X, KBool)
				}
			case *Func:
				switch e.Name {
				case "substr", "substring", "round", "trunc":
					for i, a := range e.Args {
						if i > 0 {
							set(a, KInt)
						}
					}
				case "to_timestamp":
					for _, a := range e.Args {
						set(a, KFloat)
					}
				}
			}
		})
	}
	assigns := func(set2 []Assign) {
		for _, a := range set2 {
			if sc.t != nil {
				if i, ok := sc.t.Col(a.Col); ok {
					set(a.X, kindOf(sc.t.Columns[i].T))
				}
			}
			visit(a.X)
		}
	}
	items := func(its []SelItem) {
		for _, it := range its {
			visit(it.X)
		}
	}
	switch st := st.(type) {
	case *Select:
		items(st.Items)
		visit(st.Where)
		for _, g := range st.GroupBy {
			visit(g)
		}
		visit(st.Having)
		for _, o := range st.OrderBy {
			visit(o.X)
		}
		set(st.Limit, KInt)
		set(st.Offset, KInt)
		visit(st.Limit)
		visit(st.Offset)
	case *Insert:
		if sc.t != nil {
			for _, row := range st.Rows {
				for j, x := range row {
					ci := j
					if st.Cols != nil && j < len(st.Cols) {
						ci, _ = sc.t.Col(st.Cols[j])
					}
					if ci < len(sc.t.Columns) {
						set(x, kindOf(sc.t.Columns[ci].T))
					}
					visit(x)
				}
			}
		}
		if st.OnConflict != nil {
			assigns(st.OnConflict.Set)
			visit(st.OnConflict.Where)
		}
		items(st.Returning)
	case *Update:
		assigns(st.Set)
		visit(st.Where)
		items(st.Returning)
	case *Delete:
		visit(st.Where)
		items(st.Returning)
	}
	return pk
}

// maxParam is the highest $n used in st.
func maxParam(st Stmt) int {
	n := 0
	f := func(x Expr) {
		walk(x, func(e Expr) {
			if p, ok := e.(*Param); ok && p.N > n {
				n = p.N
			}
		})
	}
	items := func(its []SelItem) {
		for _, it := range its {
			f(it.X)
		}
	}
	switch st := st.(type) {
	case *Select:
		items(st.Items)
		f(st.Where)
		for _, g := range st.GroupBy {
			f(g)
		}
		f(st.Having)
		for _, o := range st.OrderBy {
			f(o.X)
		}
		f(st.Limit)
		f(st.Offset)
	case *Insert:
		for _, r := range st.Rows {
			for _, x := range r {
				f(x)
			}
		}
		if st.OnConflict != nil {
			for _, a := range st.OnConflict.Set {
				f(a.X)
			}
			f(st.OnConflict.Where)
		}
		items(st.Returning)
	case *Update:
		for _, a := range st.Set {
			f(a.X)
		}
		f(st.Where)
		items(st.Returning)
	case *Delete:
		f(st.Where)
		items(st.Returning)
	}
	return n
}

// Description of a prepared statement.
type Description struct {
	Params  []Kind   // kind of $1..$n
	Cols    []Column // result columns
	HasRows bool
}

// Describe types a statement without running it.
func (s *Session) Describe(st Stmt) (*Description, error) {
	d := &Description{}
	sc := &scope{}
	var table string
	var ret []SelItem
	switch st := st.(type) {
	case *SetOp:
		return s.describeQuery(st)
	case *Select:
		if needsGeneral(st) || s.isSequence(st.From) {
			return s.describeQuery(st)
		}
		table = st.From
		sc.alias = st.Alias
		d.HasRows = true
	case *Insert:
		table, ret = st.Table, st.Returning
	case *Update:
		table, ret = st.Table, st.Returning
	case *Delete:
		table, ret = st.Table, st.Returning
	case *Show:
		d.HasRows = true
		d.Cols = []Column{{st.Name, KText}}
		return d, nil
	default:
		return d, nil
	}
	if table != "" {
		t, err := s.table(table)
		if err != nil {
			return nil, err
		}
		sc.t = t
	}
	pk := paramKinds(st, sc)
	n := maxParam(st)
	d.Params = make([]Kind, n)
	for i := range d.Params {
		if k, ok := pk[i+1]; ok {
			d.Params[i] = k
		} else {
			d.Params[i] = KText
		}
	}
	if sel, ok := st.(*Select); ok {
		for _, it := range sel.Items {
			if it.Star {
				if sc.t == nil {
					return nil, errf(codeSyntax, "SELECT * with no tables specified is not valid")
				}
				for _, c := range sc.t.Columns {
					d.Cols = append(d.Cols, Column{c.Name, kindOf(c.T)})
				}
				continue
			}
			d.Cols = append(d.Cols, Column{itemName(it), typeOf(it.X, sc, pk)})
		}
		return d, nil
	}
	if ret != nil {
		d.HasRows = true
		d.Cols = returningCols(sc, ret, pk)
	}
	return d, nil
}
