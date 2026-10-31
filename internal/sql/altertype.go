package sql

import (
	"errors"
	"math"

	"stask/internal/core"
	"stask/internal/schema"
)

// ALTER TABLE t ALTER COLUMN c [SET DATA] TYPE type [USING c::type].
//
// Rows are not rewritten: the column keeps its number, the old type is
// recorded (schema.Column.PrevType, TypeLSN) and rows stored before the
// ALTER are read through schema.ConvertValue, like rows before an ADD COLUMN
// read its default. Because that read cannot fail, every existing row is
// converted here first, and the ALTER fails (22P02, 22003) if one does not,
// as PostgreSQL's rewrite would. As in PostgreSQL, a conversion that needs
// an explicit cast (text to a number, boolean to integer) requires USING.
//
// A column converted once cannot be converted again until the table is
// rewritten; key, search, time-series and columnar tables are refused.
func (s *Session) alterColumnType(table string, d *schema.Table, i int, a AlterAction) error {
	c := &d.Columns[i]
	to, err := schema.ParseType(a.Type)
	if err != nil {
		return errf(codeFeature, "type %q is not supported", a.Type)
	}
	from := c.T
	if a.Collation != "" {
		if kindOf(to) != KText {
			return errf("42804", "collations are not supported by type %s", a.Type)
		}
		c.Collation = a.Collation
		noteCollatedColumns(s.E)
	} else if c.Collation != "" && kindOf(to) == KText {
		c.Collation = "" // the new type without COLLATE takes the default collation back
	}
	if c.Type == a.Type {
		c.PG = a.PG
		return nil
	}
	if schema.SameEncoding(from, to) || from == to && from == schema.TNumeric { // varchar <-> text stored alike; numeric(p,s) modifier: new rows only
		c.Type, c.PG = a.Type, a.PG
		return nil
	}
	// the pairs P1.1 added (time to timestamp(tz), int/float to numeric) convert
	// without loss and cannot fail, so they need neither USING nor a row check
	lossless := !schema.BasicConvertible(from, to) && schema.ConvertibleTypes(from, to)
	switch {
	case !schema.BasicConvertible(from, to) && !lossless:
		return errf(codeFeature, "ALTER COLUMN %q TYPE %s is not supported: the column is %s", c.Name, a.Type, c.Type)
	case !a.Using && !lossless && !schema.AssignmentCast(from, to):
		return errf("42804", "column %q cannot be cast automatically to type %s (use ALTER COLUMN ... TYPE %s USING %s::%s)", c.Name, a.Type, a.Type, c.Name, a.Type)
	case c.PrevType != "":
		return errf(codeFeature, "column %q was converted from %s by an earlier ALTER; rewrite the table before converting it again", c.Name, c.PrevType)
	case d.Columnar || d.TimeColumn != "" || len(d.Search) > 0 || c.Name == d.Key || c.Name == d.SeriesColumn:
		return errf(codeFeature, "ALTER COLUMN TYPE is not supported on a key column or on columnar, time-series and search tables")
	case c.Identity != "":
		return errf(codeFeature, "ALTER COLUMN TYPE is not supported on an identity column")
	}
	for _, ck := range d.Checks {
		if x, err := parseCheck(ck.Expr); err == nil && refersToCol(x, c.Name) {
			return errf(codeFeature, "cannot alter type of column %q: CHECK constraint %q refers to it", c.Name, ck.Name)
		}
	}
	// every stored row must convert
	ts, err := s.E.Table(table)
	if err != nil {
		return err
	}
	base := ts.Schema()
	bi, ok := base.Col(c.Name)
	if !ok {
		return errf(codeUndefinedColumn, "column %q of relation %q does not exist", c.Name, d.Name)
	}
	var verr error
	if lossless {
		goto converted
	}
	err = s.E.ScanSnapshot(table, s.E.DurableLSN(), func(_ string, r schema.Row, _ *core.Version) bool {
		if bi >= len(r) {
			return true
		}
		if _, err := schema.TryConvert(from, to, r[bi]); err != nil {
			verr = convertError(err, a.Type, r[bi], from)
			return false
		}
		return true
	})
	if err != nil {
		return err
	}
	if verr != nil {
		return verr
	}
converted:
	if lossless && c.Missing != nil {
		m := schema.ConvertValue(from, to, *c.Missing)
		c.Missing = &m
	} else if c.Missing != nil {
		m, err := schema.TryConvert(from, to, *c.Missing)
		if err != nil {
			return convertError(err, a.Type, *c.Missing, from)
		}
		c.Missing = &m
	}
	c.PrevType, c.Type, c.PG, c.TypeLSN = c.Type, a.Type, a.PG, math.MaxUint64
	return nil
}

func isStrType(t schema.ColType) bool { return t == schema.TString || t == schema.TText }

func convertError(err error, typ string, v schema.Value, from schema.ColType) error {
	shown := ""
	if isStrType(from) {
		shown = v.S
	}
	if errors.Is(err, schema.ErrOutOfRange) {
		return errf(codeNumericOutOfRange, "value %q is out of range for type %s", shown, typ)
	}
	return errf(codeInvalidText, "invalid input syntax for type %s: %q", typ, shown)
}
