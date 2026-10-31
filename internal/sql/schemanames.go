package sql

import (
	"strings"

	"stask/internal/schema"
)

// Names in a non-public schema are stored as "schema.name" (ddlobjs_names.go). PostgreSQL prints
// an object name relative to the search path: regclass/regtype output, format_type, nextval(..)
// defaults and pg_get_constraintdef omit the schema when it is on the path, and write
// schema.name otherwise; pg_get_indexdef and pg_get_serial_sequence always qualify the table /
// sequence. Precedent: PostgreSQL regclassout, format_type_extended and generate_relation_name
// (ruleutils.c) use RelationIsVisible / TypeIsVisible and quote_qualified_identifier.

// schemaVisible: the schema is on the session's search path (pg_catalog always is).
func (s *Session) schemaVisible(sch string) bool {
	if s == nil {
		return sch == "public"
	}
	if sch == "pg_catalog" {
		return true
	}
	for _, p := range s.pathList() {
		if p == sch {
			return true
		}
	}
	return false
}

// qualDisplay is schema.bare, or just bare when the schema is visible, each part quoted.
func (s *Session) qualDisplay(sch, bare string) string {
	if s.schemaVisible(sch) {
		return quoteIdent(bare)
	}
	return quoteIdent(sch) + "." + quoteIdent(bare)
}

// relDisplay prints the stored name of a table, view, sequence or type as regclass / regtype do.
func (s *Session) relDisplay(stored string) string {
	return s.qualDisplay(schemaOfName(stored), bareName(stored))
}

// qualified is schema.bare always (pg_get_indexdef, pg_get_serial_sequence).
func qualified(stored string) string {
	return quoteIdent(schemaOfName(stored)) + "." + quoteIdent(bareName(stored))
}

// stripOwnSchema drops the schema prefix of a name derived from a table of schema sch
// ("s13.child_pkey" of a table in s13 shows as child_pkey).
func stripOwnSchema(name, sch string) string {
	if sch != "public" && strings.HasPrefix(name, sch+".") {
		return name[len(sch)+1:]
	}
	return name
}

func init() {
	// constraint names derived from the table name carry the table's schema prefix
	wrapCat("pg_constraint", func(c *catSnap, rows [][]Datum) [][]Datum {
		ns := c.nsInfo()
		if len(ns) == 0 {
			return rows
		}
		tabs := map[int64]string{}
		for _, t := range c.tables {
			tabs[tableOID(t)] = t.Name
		}
		for _, r := range rows {
			tn, ok := tabs[r[7].I]
			if !ok {
				continue
			}
			if o, sch, _ := ns.split(tn); sch != "public" {
				r[1] = TextD(stripOwnSchema(r[1].S, sch))
				r[2] = IntD(o)
			}
		}
		return rows
	})
}

// prettyIndexDef: pg_get_indexdef(oid, 0, true) names the table relative to the search path.
func (s *Session) prettyIndexDef(def string, t *schema.Table, pretty bool) string {
	if !pretty {
		return def
	}
	return strings.Replace(def, " ON "+qualified(t.Name)+" ", " ON "+s.relDisplay(t.Name)+" ", 1)
}
