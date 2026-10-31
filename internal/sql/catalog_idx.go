package sql

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	"stask/internal/schema"
)

// Secondary indexes and constraints in the catalog (docs/05 §18): the rows
// that pg_class, pg_attribute, pg_index, pg_constraint, pg_indexes and
// information_schema add to the primary-key ones of catalog_rels.go. The
// indexes a foreign key creates for itself (Internal) are not listed.

const (
	secIndexOIDBase = 0x48000000
	secConOIDBase   = 0x38000000
)

func hashName(s string) int64 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int64(h.Sum32() & 0x07ffffff)
}

func secIndexOID(name string) int64      { return secIndexOIDBase + hashName(name) }
func secConOID(table, name string) int64 { return secConOIDBase + hashName(table+"."+name) }
func fkConOID(t *schema.Table, n string) int64 {
	return secConOIDBase + 0x04000000 + hashName(t.Name+"."+n)
}

// listedIndexes are the secondary indexes clients see.
func listedIndexes(t *schema.Table) []schema.IndexDef {
	var out []schema.IndexDef
	for _, ix := range t.Indexes {
		if !ix.Internal {
			out = append(out, ix)
		}
	}
	return out
}

// attnums are the attribute numbers of the named columns.
func attnums(t *schema.Table, cols []string) []int {
	out := make([]int, len(cols))
	for i, c := range cols {
		if j, ok := t.Col(c); ok {
			out[i] = t.Columns[j].Num
		}
	}
	return out
}

func quoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}
	return strings.Join(q, ", ")
}

// attnumsIx are the indkey entries of ix: 0 for an expression.
func attnumsIx(t *schema.Table, ix schema.IndexDef) []int {
	out := attnums(t, ix.Columns)
	for k := range out {
		if ix.ElemExpr(k) != "" {
			out[k] = 0
		}
	}
	return out
}

// elemDef is element k of ix as pg_get_indexdef writes it: lower(email), or
// lower((email)::text) for a varchar column.
func elemDef(t *schema.Table, ix schema.IndexDef, k int) string {
	col := quoteIdent(ix.Columns[k])
	fn := ix.ElemExpr(k)
	coll := ""
	if k < len(ix.Colls) && ix.Colls[k] != "" {
		coll = " COLLATE " + quoteIdent(ix.Colls[k])
	}
	if fn == "" {
		return col + coll
	}
	if fn == "btrim" {
		fn = "TRIM(BOTH FROM %s)"
	} else if fn == "length" {
		fn = "length(%s)"
	} else {
		fn += "(%s)"
	}
	if j, ok := t.Col(ix.Columns[k]); ok && ctypeOf(t, j) == schema.TString {
		col = "(" + col + ")::text"
	}
	return fmt.Sprintf(fn, col) + coll
}

func ctypeOf(t *schema.Table, i int) schema.ColType {
	ct, _ := schema.ParseType(t.Columns[i].Type)
	return ct
}

// secIndexDef is pg_get_indexdef of a secondary index.
func secIndexDef(t *schema.Table, ix schema.IndexDef) string {
	elems := make([]string, len(ix.Columns))
	for k := range elems {
		elems[k] = elemDef(t, ix, k)
	}
	u := ""
	if ix.Unique {
		u = "UNIQUE "
	}
	return "CREATE " + u + "INDEX " + quoteIdent(ix.Name) + " ON public." + quoteIdent(t.Name) +
		" USING btree (" + strings.Join(elems, ", ") + ")" + wherePart(t, ix)
}

func wherePart(t *schema.Table, ix schema.IndexDef) string {
	if w := ix.PredTextT(t); w != "" {
		return " WHERE " + w
	}
	return ""
}

func fkActionChar(a string) string {
	switch a {
	case "restrict":
		return "r"
	case "cascade":
		return "c"
	case "set null":
		return "n"
	}
	return "a"
}

// fkDef is pg_get_constraintdef of a foreign key.
func fkDef(fk schema.ForeignKey) string {
	d := "FOREIGN KEY (" + quoteList(fk.Columns) + ") REFERENCES " + quoteIdent(fk.RefTable) + "(" + quoteList(fk.RefCols) + ")"
	switch fk.OnDelete {
	case "restrict":
		d += " ON DELETE RESTRICT"
	case "cascade":
		d += " ON DELETE CASCADE"
	case "set null":
		d += " ON DELETE SET NULL"
	}
	return d
}

// referencedIndexOID is the index of the parent table that a foreign key
// relies on: the primary key's, or the unique constraint's.
func (c *catSnap) referencedIndexOID(fk schema.ForeignKey) int64 {
	for _, p := range c.tables {
		if p.Name != fk.RefTable {
			continue
		}
		for _, ix := range p.Indexes {
			if ix.Unique && !ix.HasExpr() && len(ix.Where) == 0 && slices.Equal(ix.Columns, fk.RefCols) {
				return secIndexOID(ix.Name)
			}
		}
		return indexOID(p)
	}
	return 0
}

func (c *catSnap) tableNamed(name string) *schema.Table {
	for _, t := range c.tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// secIndexRows adds the secondary indexes to pg_class (rel), pg_attribute
// (att), pg_index and pg_indexes through the callers' row builders.
func secIndexRows(c *catSnap, f func(t *schema.Table, ix schema.IndexDef)) {
	for _, t := range c.tables {
		for _, ix := range listedIndexes(t) {
			f(t, ix)
		}
	}
}

func secConstraintRows(c *catSnap, rows *[][]Datum) {
	row := func(oid int64, name, typ string, rel, ind, frel int64, upd, del string, key, fkey Datum) {
		*rows = append(*rows, []Datum{IntD(oid), TextD(name), IntD(nsPublic), TextD(typ), BoolD(false), BoolD(false),
			BoolD(true), IntD(rel), IntD(0), IntD(ind), IntD(0), IntD(frel), TextD(upd), TextD(del), TextD(supfixMatchType(typ)),
			BoolD(true), IntD(0), BoolD(false), key, fkey, Null, Null, Null, Null, Null, Null})
	}
	for _, t := range c.tables {
		for _, ix := range t.Indexes {
			if ix.Constraint != "" {
				row(secConOID(t.Name, ix.Constraint), ix.Constraint, "u", tableOID(t), secIndexOID(ix.Name), 0, " ", " ",
					i2vec(attnums(t, ix.Columns)...), Null)
			}
		}
		for _, fk := range t.ForeignKeys {
			p := c.tableNamed(fk.RefTable)
			if p == nil {
				continue
			}
			row(fkConOID(t, fk.Name), fk.Name, "f", tableOID(t), c.referencedIndexOID(fk), tableOID(p), "a", fkActionChar(fk.OnDelete),
				i2vec(attnums(t, fk.Columns)...), i2vec(attnums(p, fk.RefCols)...))
		}
	}
}

// secIndexByOID finds a secondary index by its OID.
func (c *catSnap) secIndexByOID(oid int64) (*schema.Table, *schema.IndexDef) {
	for _, t := range c.tables {
		for i := range t.Indexes {
			if !t.Indexes[i].Internal && secIndexOID(t.Indexes[i].Name) == oid {
				return t, &t.Indexes[i]
			}
		}
	}
	return nil, nil
}

// secConstraintDef is pg_get_constraintdef of a unique or foreign key constraint.
func (c *catSnap) secConstraintDef(oid int64) (string, bool) {
	for _, t := range c.tables {
		for _, ix := range t.Indexes {
			if ix.Constraint != "" && secConOID(t.Name, ix.Constraint) == oid {
				return "UNIQUE (" + quoteList(ix.Columns) + ")", true
			}
		}
		for _, fk := range t.ForeignKeys {
			if fkConOID(t, fk.Name) == oid {
				return fkDef(fk), true
			}
		}
	}
	return "", false
}
