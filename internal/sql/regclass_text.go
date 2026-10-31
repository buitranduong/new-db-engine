package sql

// regclass::text prints the relation's name, not its OID (Liquibase and
// Hibernate read pg_constraint.conrelid::regclass::text). The OID types
// are plain integers here, so only the explicit cast chain
// expr::regclass::text is resolved. Task jmixfix.

// regclassText evaluates e if it is x::regclass cast to a text type.
func (c *evalCtx) regclassText(e *Cast) (Datum, bool, error) {
	in, ok := e.X.(*Cast)
	if !ok || kindOfType(e.Type) != KText {
		return Null, false, nil
	}
	switch in.Type {
	case "regclass":
	case "regtype", "regproc", "regprocedure", "regnamespace":
		return c.regOtherText(in)
	default:
		return Null, false, nil
	}
	s := c.session()
	if s == nil {
		return Null, false, nil
	}
	v, err := c.eval(in)
	if err != nil {
		return Null, true, err
	}
	if v.IsNull() {
		return Null, true, nil
	}
	if v.K != KInt {
		return TextD(v.String()), true, nil
	}
	return TextD(s.relnameByOID(v.I)), true, nil
}

// regOtherText prints x::regtype / regproc / regprocedure / regnamespace as text.
func (c *evalCtx) regOtherText(in *Cast) (Datum, bool, error) {
	s := c.session()
	if s == nil {
		return Null, false, nil
	}
	v, err := c.eval(in)
	if err != nil {
		return Null, true, err
	}
	if v.IsNull() {
		return Null, true, nil
	}
	if v.K != KInt {
		return TextD(v.String()), true, nil
	}
	switch in.Type {
	case "regtype":
		if n := formatType(uint32(v.I), -1); n != "" {
			return TextD(n), true, nil
		}
		if n, ok := map[int64]string{2249: "record", 2278: "void", 2279: "trigger", 2281: "internal", 2283: "anyelement", 2276: "any"}[v.I]; ok {
			return TextD(n), true, nil
		}
	case "regnamespace":
		switch v.I {
		case nsPgCatalog:
			return TextD("pg_catalog"), true, nil
		case nsPublic:
			return TextD("public"), true, nil
		case nsInfoSchema:
			return TextD("information_schema"), true, nil
		}
	case "regproc", "regprocedure":
		for _, d := range allFuncs(s.E) {
			if d.oid() == v.I {
				if in.Type == "regprocedure" {
					return TextD(d.Name + "(" + d.sig() + ")"), true, nil
				}
				if len(s.funcsNamed(d.Name)) > 1 { // an overloaded name is written with its schema
					return TextD("public." + d.Name), true, nil
				}
				return TextD(d.Name), true, nil
			}
		}
	}
	return TextD(itoa(v.I)), true, nil
}

// relnameByOID is the name of a table, view, sequence or index with an
// OID, quoted as regclass output is; the decimal OID if there is none.
func (s *Session) relnameByOID(oid int64) string {
	for _, ts := range s.E.UserTables() {
		sch := ts.Schema()
		switch {
		case tableOID(sch) == oid:
			return quoteIdent(sch.Name)
		case sch.HasPK() && indexOID(sch) == oid:
			return quoteIdent(pkeyName(sch))
		}
		for _, ix := range sch.Indexes {
			if !ix.Internal && secIndexOID(ix.Name) == oid {
				return quoteIdent(ix.Name)
			}
		}
	}
	for _, v := range s.E.Views() {
		if viewOID(v.Name) == oid {
			return quoteIdent(v.Name)
		}
	}
	if n := s.seqNameByOID(oid); n != itoa(oid) {
		return quoteIdent(n)
	}
	return itoa(oid)
}
