package sql

import "stask/internal/schema"

// extParamOIDs gives the PostgreSQL type of the parameters that stand for a value of one of the added types
// (ranges, bit strings, macaddr8, interval with a typmod): clients such as pgx pick the binary encoding from the
// type the server announces in ParameterDescription, which a Kind alone (text) cannot express. It reads the
// places where PostgreSQL infers a parameter type from a column: INSERT values, UPDATE SET, comparisons and
// operators against a column of a single table, and casts.
func extParamOIDs(st Stmt, t *schema.Table, n int) []uint32 {
	var out []uint32
	set := func(p *Param, oid uint32) {
		if oid == 0 || p.N < 1 || p.N > n {
			return
		}
		if out == nil {
			out = make([]uint32, n)
		}
		if out[p.N-1] == 0 {
			out[p.N-1] = oid
		}
	}
	colOID := func(name string) uint32 {
		if t == nil {
			return 0
		}
		i, ok := t.Col(name)
		if !ok || !extIsSpec(t.Columns[i].PG) {
			return 0
		}
		return specTy(t.Columns[i].PG).oid
	}
	visit := func(x Expr) {
		walk(x, func(e Expr) {
			switch e := e.(type) {
			case *Cast:
				if p, ok := e.X.(*Param); ok && e.PG != "" && extIsSpec(e.PG) {
					set(p, specTy(e.PG).oid)
				}
			case *Binary:
				l, r := e.L, e.R
				if c, ok := l.(*ColRef); ok {
					if p, ok := r.(*Param); ok && e.Op != "<<" && e.Op != ">>" {
						set(p, colOID(c.Name))
					}
				}
				if c, ok := r.(*ColRef); ok {
					if p, ok := l.(*Param); ok {
						set(p, colOID(c.Name))
					}
				}
			}
		})
	}
	switch s := st.(type) {
	case *Insert:
		for _, row := range s.Rows {
			for j, x := range row {
				col := ""
				if s.Cols != nil && j < len(s.Cols) {
					col = s.Cols[j]
				} else if s.Cols == nil && t != nil && j < len(t.Columns) {
					col = t.Columns[j].Name
				}
				if p, ok := x.(*Param); ok {
					set(p, colOID(col))
				}
				visit(x)
			}
		}
	case *Update:
		for _, a := range s.Set {
			if p, ok := a.X.(*Param); ok {
				set(p, colOID(a.Col))
			}
			visit(a.X)
		}
		visit(s.Where)
	case *Delete:
		visit(s.Where)
	case *Select:
		for _, it := range s.Items {
			visit(it.X)
		}
		visit(s.Where)
	}
	return out
}
