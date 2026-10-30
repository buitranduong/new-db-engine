package sql

import (
	"strconv"
	"strings"

	"stask/internal/schema"
)

// supfix: catalog details SQLAlchemy / Superset / admin pages read (docs/14 Appendix E items 9, 12):
// regclass::text names, rough relation statistics and sizes, confmatchtype.

// supfixMatchType is pg_constraint.confmatchtype: 's' (simple) for foreign keys, a space otherwise.
func supfixMatchType(typ string) string {
	if typ == "f" {
		return "s"
	}
	return " "
}

// relApprox is the rough size of a table: pages (8 kB) and tuples, from the live row count and the
// column count. It is an estimate, not statistics (there is no ANALYZE).
func relApprox(live int64, ncols int) (pages int64, bytes int64) {
	if live <= 0 {
		return 0, 0
	}
	bytes = live * int64(32+12*ncols)
	pages = (bytes + 8191) / 8192
	return pages, pages * 8192
}

// supfixRelStats fills relpages and reltuples of a pg_class row of table t.
func supfixRelStats(c *catSnap, t *schema.Table, row []Datum) {
	ts, err := c.s.E.UserTable(t.Name)
	if err != nil || len(row) < 11 {
		return
	}
	live := ts.LiveRows()
	pages, _ := relApprox(live, len(t.Columns))
	row[9] = IntD(pages)
	row[10] = FloatD(float64(live))
}

// supfixRelSize is pg_relation_size / pg_total_relation_size(regclass or name): a rough size.
func (s *Session) supfixRelSize(arg Datum, total bool) Datum {
	var name string
	if arg.K == KInt {
		for _, t := range s.catSnap().tables {
			if tableOID(t) == arg.I {
				name = t.Name
			}
		}
	} else if arg.K == KText {
		d, err := s.regclass(arg.S)
		if err != nil {
			return IntD(0)
		}
		for _, t := range s.catSnap().tables {
			if tableOID(t) == d.I {
				name = t.Name
			}
		}
	}
	if name == "" {
		if arg.K == KInt && arg.I > 0 || arg.K == KText && catalogRels[arg.S] > 0 {
			return IntD(65536) // a system catalog: a few pages (its real size is not tracked)
		}
		return IntD(0)
	}
	ts, err := s.E.UserTable(name)
	if err != nil {
		return IntD(0)
	}
	sch := ts.Schema()
	_, b := relApprox(ts.LiveRows(), len(sch.Columns))
	if total { // plus one page per index (primary key and secondary) and the toast table
		n := len(sch.Indexes) + 1
		if sch.HasPK() {
			n++
		}
		b += int64(n) * 8192
	}
	return IntD(b)
}

// relNameByOID is the name regclass prints for an OID.
func (s *Session) relNameByOID(oid int64) (string, bool) {
	for n, o := range catalogRels {
		if o == oid {
			return n, true
		}
	}
	c := s.catSnap()
	for _, t := range c.tables {
		if tableOID(t) == oid {
			return t.Name, true
		}
		if t.HasPK() && indexOID(t) == oid {
			return pkeyName(t), true
		}
	}
	for _, q := range c.seqs {
		if seqOID(q.Name) == oid {
			return q.Name, true
		}
	}
	for _, v := range c.views {
		if viewOID(v.Name) == oid {
			return v.Name, true
		}
	}
	if _, ix := c.secIndexByOID(oid); ix != nil {
		return ix.Name, true
	}
	return "", false
}

// supfixRegclassText evaluates x::regclass::text (and to_regclass(..)::text): the relation name,
// quoted when needed, as PostgreSQL prints a regclass. ok is false when e is not that shape.
func (c *evalCtx) supfixRegclassText(e *Cast) (Datum, bool, error) {
	if kindOfType(e.Type) != KText {
		return Null, false, nil
	}
	switch in := e.X.(type) {
	case *Cast:
		if in.Type != "regclass" {
			return Null, false, nil
		}
	case *Func:
		if in.Name != "to_regclass" {
			return Null, false, nil
		}
	default:
		return Null, false, nil
	}
	s := c.session()
	if s == nil {
		return Null, false, nil
	}
	v, err := c.eval(e.X)
	if err != nil {
		return Null, true, err
	}
	if v.IsNull() {
		return Null, true, nil
	}
	if v.K != KInt {
		return TextD(v.String()), true, nil
	}
	if v.I == 0 {
		return TextD("-"), true, nil
	}
	if n, ok := s.relNameByOID(v.I); ok {
		return TextD(quoteIdent(n)), true, nil
	}
	return TextD(strconv.FormatInt(v.I, 10)), true, nil
}

var _ = strings.ToLower
