package sql

// Data-modifying CTEs and WITH before INSERT/UPDATE/DELETE:
//
//	WITH ins AS (INSERT INTO t ... RETURNING id) SELECT id FROM ins
//	WITH s AS (SELECT ...) UPDATE t SET ... FROM s WHERE ...
//
// The data-modifying CTEs run first, in order, in one transaction; their
// RETURNING rows become a VALUES list standing in for the CTE; then the main
// statement runs. (PostgreSQL runs everything against one snapshot and in
// unspecified order; the difference shows only when the main query reads the
// table a CTE changed.)

// WithDML is a statement with data-modifying CTEs. Main is a query (whose With
// holds the CTEs) or, for WITH ... INSERT/UPDATE/DELETE, the DML statement and
// With holds the CTEs.
type WithDML struct {
	With []CTE
	Main Stmt
}

func (*WithDML) stmt() {}

func isDMLStmt(st Stmt) bool {
	switch st.(type) {
	case *Insert, *Update, *Delete:
		return true
	}
	return false
}

// withOf returns the CTEs of a query statement.
func withOf(st Stmt) []CTE {
	switch m := st.(type) {
	case *Select:
		return m.With
	case *SetOp:
		return m.With
	}
	return nil
}

func hasDMLCTE(ctes []CTE) bool {
	for _, c := range ctes {
		if isDMLStmt(c.Q) {
			return true
		}
	}
	return false
}

// kindTypeName names a Kind as a cast target.
func kindTypeName(k Kind) string {
	switch k {
	case KInt:
		return "int"
	case KFloat:
		return "float"
	case KBool:
		return "bool"
	case KTime:
		return "timestamptz"
	case KTimestamp:
		return "timestamp"
	case KDate:
		return "date"
	case KNumeric:
		return "numeric"
	case KUUID:
		return "uuid"
	case KBytea:
		return "bytea"
	case KJSON:
		return "json"
	case KJSONB:
		return "jsonb"
	case KInterval:
		return "interval"
	}
	return "text"
}

// typedLit is v as a literal that keeps the column's type.
func typedLit(c Column, v Datum) Expr {
	spec := ""
	switch c.OID {
	case tyInt2:
		spec = "int2"
	case tyInt4:
		spec = "int4"
	case tyFloat4:
		spec = "float4"
	}
	if c.Kind == KText || c.Kind == KArray {
		return &Lit{v}
	}
	return &Cast{X: &Lit{v}, Type: kindTypeName(c.Kind), PG: spec}
}

// valuesQuery turns a result into a query returning the same rows (the
// stand-in of an executed data-modifying CTE). aliases rename the columns.
func valuesQuery(cols []Column, rows [][]Datum, aliases []string) Stmt {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
		if i < len(aliases) {
			names[i] = aliases[i]
		}
	}
	if len(rows) == 0 {
		sel := &Select{Where: &Lit{BoolD(false)}}
		for i, c := range cols {
			sel.Items = append(sel.Items, SelItem{X: typedLit(c, Null), Alias: names[i]})
		}
		return sel
	}
	fv := &FromValues{Alias: "v", ColAliases: names}
	for _, r := range rows {
		row := make([]Expr, len(cols))
		for i, c := range cols {
			row[i] = typedLit(c, r[i])
		}
		fv.Rows = append(fv.Rows, row)
	}
	return &Select{Items: []SelItem{{Star: true}}, Rel: fv}
}

// substFrom replaces references to the CTEs in a FROM tree by their queries.
func substFrom(f FromNode, ctes []CTE) FromNode {
	switch n := f.(type) {
	case *FromTable:
		for _, c := range ctes {
			if c.Name == n.Name && n.AsOf == nil {
				alias := n.Alias
				if alias == "" {
					alias = n.Name
				}
				cols := n.ColAliases
				if cols == nil {
					cols = c.Cols
				}
				return &FromSubquery{Q: c.Q, Alias: alias, ColAliases: cols}
			}
		}
	case *FromJoin:
		cp := *n
		cp.L, cp.R = substFrom(n.L, ctes), substFrom(n.R, ctes)
		return &cp
	}
	return f
}

func (s *Session) withDML(w *WithDML, params []Datum) (*Result, error) {
	auto := !s.explicit
	if auto {
		if _, err := s.exec(&Begin{}, nil); err != nil {
			return nil, err
		}
	}
	res, err := s.runWithDML(w, params)
	if auto {
		if err != nil {
			s.exec(&Rollback{}, nil)
			return nil, err
		}
		if _, cerr := s.exec(&Commit{}, nil); cerr != nil {
			return nil, cerr
		}
	}
	return res, err
}

func (s *Session) runWithDML(w *WithDML, params []Datum) (*Result, error) {
	ctes := w.With
	if q := withOf(w.Main); q != nil {
		ctes = q
	}
	resolved := make([]CTE, len(ctes))
	for i, c := range ctes {
		resolved[i] = c
		if isDMLStmt(c.Q) {
			r, err := s.exec(c.Q, params)
			if err != nil {
				return nil, err
			}
			if !r.HasRows {
				return nil, errf("0A000", "WITH query %q does not have a RETURNING clause", c.Name)
			}
			resolved[i].Q = valuesQuery(r.Cols, r.Rows, c.Cols)
			resolved[i].Cols = nil
		}
	}
	return s.exec(mainWith(w.Main, resolved), params)
}

// mainWith is the main statement with the resolved CTEs in place.
func mainWith(main Stmt, ctes []CTE) Stmt {
	switch m := main.(type) {
	case *Select:
		cp := *m
		cp.With, cp.cached = ctes, nil
		return &cp
	case *SetOp:
		cp := *m
		cp.With = ctes
		return &cp
	case *Insert:
		cp := *m
		if q, ok := m.Query.(*Select); ok && q.With == nil {
			qc := *q
			qc.With, qc.cached = ctes, nil
			cp.Query = &qc
		}
		return &cp
	case *Update:
		cp := *m
		cp.From = substFrom(m.From, ctes)
		return &cp
	case *Delete:
		cp := *m
		cp.Using = substFrom(m.Using, ctes)
		return &cp
	}
	return main
}

// describeWithDML types the main statement with each data-modifying CTE
// described by its RETURNING list.
func (s *Session) describeWithDML(w *WithDML) (*Description, error) {
	ctes := w.With
	if q := withOf(w.Main); q != nil {
		ctes = q
	}
	resolved := make([]CTE, len(ctes))
	n := maxParam(w.Main)
	for i, c := range ctes {
		resolved[i] = c
		if isDMLStmt(c.Q) {
			d, err := s.Describe(c.Q)
			if err != nil {
				return nil, err
			}
			resolved[i].Q = valuesQuery(d.Cols, nil, c.Cols)
			resolved[i].Cols = nil
			n = max(n, maxParam(c.Q))
		}
	}
	d, err := s.Describe(mainWith(w.Main, resolved))
	if err != nil {
		return nil, err
	}
	for len(d.Params) < n {
		d.Params = append(d.Params, KText)
	}
	return d, nil
}
