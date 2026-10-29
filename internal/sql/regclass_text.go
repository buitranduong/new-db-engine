package sql

// regclass::text prints the relation's name, not its OID (Liquibase and
// Hibernate read pg_constraint.conrelid::regclass::text). The OID types
// are plain integers here, so only the explicit cast chain
// expr::regclass::text is resolved. Task jmixfix.

// regclassText evaluates e if it is x::regclass cast to a text type.
func (c *evalCtx) regclassText(e *Cast) (Datum, bool, error) {
	in, ok := e.X.(*Cast)
	if !ok || in.Type != "regclass" || kindOfType(e.Type) != KText {
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
