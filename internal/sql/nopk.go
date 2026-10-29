package sql

import (
	"fmt"
	"slices"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// Tables without a PRIMARY KEY and with a composite one (docs/05, schema/pkey.go).
// The table keeps one internal key column: hidden __rowid (filled by an
// owned sequence) or hidden __pk (the encoded composite key).

// hiddenKey adds the hidden key column CREATE TABLE needs when the table
// has no one-column PRIMARY KEY. It returns the sequence to create for
// __rowid, if any.
func (s *Session) hiddenKey(c *CreateTable, t *schema.Table) (*core.Sequence, error) {
	switch {
	case c.Key != "":
		return nil, nil
	case len(c.KeyCols) > 1:
		for _, name := range c.KeyCols {
			i := colIndex(t, name)
			if i < 0 {
				return nil, errf(codeUndefinedColumn, "column %q named in key does not exist", name)
			}
			if !schema.IndexableType(ctype(t, i)) {
				return nil, errf(codeFeature, "column %q of type %s cannot be part of a composite primary key", name, t.Columns[i].Type)
			}
			t.Columns[i].NotNull = true
		}
		if t.Columnar {
			return nil, errf(codeFeature, "a columnar table needs a one-column PRIMARY KEY")
		}
		t.Key = schema.KeyCol
		t.Columns = append([]schema.Column{{Name: schema.KeyCol, Type: "string", NotNull: true, Hidden: true}}, t.Columns...)
		return nil, nil
	}
	name := c.Name + "__" + schema.RowIDCol + "_seq"
	if s.relationExists(name) {
		return nil, errf(codeDuplicateTable, "relation %q already exists", name)
	}
	d := core.Sequence{Name: name, Owner: c.Name + "." + schema.RowIDCol}
	if err := applySeqOpts(&d, &SeqOpts{Type: "bigint"}, true); err != nil {
		return nil, err
	}
	t.Key = schema.RowIDCol
	t.Columns = append([]schema.Column{{Name: schema.RowIDCol, Type: "int", NotNull: true, Hidden: true,
		Default: fmt.Sprintf("nextval('\"%s\"')", strings.ReplaceAll(strings.ReplaceAll(name, `"`, `""`), "'", "''"))}}, t.Columns...)
	return &d, nil
}

// hiddenSequence reports whether q is the sequence of a __rowid column,
// which clients do not see.
func hiddenSequence(q core.Sequence) bool { return strings.HasSuffix(q.Owner, "."+schema.RowIDCol) }

// dupKeyErr is the 23505 for a row whose primary key exists already.
func dupKeyErr(t *schema.Table, key string) *Error {
	cols, vals := t.KeyDisplay(key)
	return errf(codeUniqueViolation, "duplicate key value violates unique constraint \"%s\": Key (%s)=(%s) already exists", pkeyName(t), cols, vals)
}

// addPrimaryKey is ALTER TABLE ... ADD PRIMARY KEY (cols) on a table
// without one. Rows are stored under their key, so a table that has rows
// would need every one re-keyed: that is refused (the table must be empty).
// The hidden __rowid column goes (its sequence with it); a composite key
// gets the hidden __pk column.
func (s *Session) addPrimaryKey(table string, d *schema.Table, cols []string, dropped map[string]bool) error {
	if d.HasPK() {
		return errf("42P16", "multiple primary keys for table %q are not allowed", d.Name)
	}
	if d.Columnar || d.TimeColumn != "" || len(d.Search) > 0 {
		return errf(codeFeature, "ADD PRIMARY KEY is not supported on columnar, time-series and search tables")
	}
	for _, name := range cols {
		i := colIndex(d, name)
		if i < 0 {
			return errf(codeUndefinedColumn, "column %q of relation %q does not exist", name, d.Name)
		}
		if len(cols) == 1 {
			if ct := ctype(d, i); ct != schema.TInt && ct != schema.TString && ct != schema.TUUID {
				return errf(codeFeature, "a one-column PRIMARY KEY must be of type integer, varchar or uuid, not %s", d.Columns[i].Type)
			}
		} else if !schema.IndexableType(ctype(d, i)) {
			return errf(codeFeature, "column %q of type %s cannot be part of a composite primary key", name, d.Columns[i].Type)
		}
	}
	empty := true
	err := s.E.ScanSnapshot(table, s.E.DurableLSN(), func(string, schema.Row, *core.Version) bool {
		empty = false
		return false
	})
	if err != nil {
		return err
	}
	if !empty {
		return errf(codeFeature, "ADD PRIMARY KEY on table %q is not supported while it has rows: every row would be re-keyed", d.Name)
	}
	for _, name := range cols {
		d.Columns[colIndex(d, name)].NotNull = true
	}
	old := colIndex(d, d.Key)
	dropped[d.Key] = true
	d.Columns = slices.Delete(d.Columns, old, old+1)
	if len(cols) == 1 {
		d.Key, d.KeyCols = cols[0], nil
		return nil
	}
	d.Key, d.KeyCols = schema.KeyCol, slices.Clone(cols)
	d.Columns = append(d.Columns, schema.Column{Name: schema.KeyCol, Type: "string", NotNull: true, Hidden: true})
	return nil
}
