package sql

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// PostgreSQL's system catalogs (pg_catalog) and the information_schema
// views, as far as psql's \d family, pgjdbc's DatabaseMetaData, Liquibase
// and DBeaver read them (docs/05 §12). They are virtual tables: rows are
// built from the engine's schema when a query reads them, so ordinary
// statements pay nothing.
//
// OIDs are fixed: PostgreSQL's own for the built-in namespaces, types and
// catalog relations; relOIDBase + the table's OID for a table (as
// pgoutput's Relation messages say); for the primary-key index that plus
// indexOIDBase; for a sequence seqOIDBase + a hash of its name.

const (
	nsPgCatalog   = 11
	nsPublic      = 2200
	nsInfoSchema  = 13000
	ownerOID      = 10 // the one role
	databaseOID   = 16383
	indexOIDBase  = 0x40000000
	seqOIDBase    = 0x60000000
	btreeAMOID    = 403
	heapAMOID     = 2
	pgClassOID    = 1259
	pgConstrOID   = 2606
	pgTypeRelOID  = 1247
	pgProcRelOID  = 1255
	pgNspRelOID   = 2615
	pgAttrRelOID  = 1249
	pgIndexRelOID = 2610
)

// catalogRels are the relations of pg_catalog by name, with the OID each
// has in PostgreSQL (for 'pg_class'::regclass).
var catalogRels = map[string]int64{
	"pg_type": pgTypeRelOID, "pg_attribute": pgAttrRelOID, "pg_proc": pgProcRelOID, "pg_class": pgClassOID,
	"pg_database": 1262, "pg_namespace": pgNspRelOID, "pg_constraint": pgConstrOID, "pg_index": pgIndexRelOID,
	"pg_attrdef": 2604, "pg_description": 2609, "pg_am": 2601, "pg_sequence": 2224, "pg_authid": 1260,
	"pg_trigger": 2620, "pg_inherits": 2611, "pg_depend": 2608, "pg_collation": 3456,
	"pg_auth_members": 1261, "pg_policy": 3256, "pg_statistic_ext": 3381, "pg_publication_namespace": 6237,
	"pg_publication_rel": 6106, "pg_publication": 6104, "pg_rewrite": 2618, "pg_enum": 3501, "pg_range": 3541,
	"pg_stat_activity": 12007, "pg_tablespace": 1213, "pg_extension": 3079, "pg_settings": 12000, "pg_roles": 12001, "pg_user": 12002,
	"pg_tables": 12003, "pg_indexes": 12004, "pg_views": 12005, "pg_matviews": 12006,
}

// pgType is a row of pg_type.
type pgType struct {
	oid      int64
	name     string
	len      int64
	cat      string // typcategory
	elem     int64  // element type of an array type
	array    int64  // array type of this type
	sqlName  string // format_type's spelling
	infoName string // information_schema's data_type
}

// pgTypes are the types STASK's values have (pgwire's OIDs) and the ones
// its DDL accepts.
var pgTypes = []pgType{
	{16, "bool", 1, "B", 0, 1000, "boolean", "boolean"},
	{17, "bytea", -1, "U", 0, 1001, "bytea", "bytea"},
	{18, "char", 1, "Z", 0, 1002, `"char"`, `"char"`},
	{19, "name", 64, "S", 0, 1003, "name", "name"},
	{20, "int8", 8, "N", 0, 1016, "bigint", "bigint"},
	{21, "int2", 2, "N", 0, 1005, "smallint", "smallint"},
	{22, "int2vector", -1, "A", 21, 1006, "int2vector", "ARRAY"},
	{23, "int4", 4, "N", 0, 1007, "integer", "integer"},
	{25, "text", -1, "S", 0, 1009, "text", "text"},
	{26, "oid", 4, "N", 0, 1028, "oid", "oid"},
	{700, "float4", 4, "N", 0, 1021, "real", "real"},
	{701, "float8", 8, "N", 0, 1022, "double precision", "double precision"},
	{1000, "_bool", -1, "A", 16, 0, "boolean[]", "ARRAY"},
	{1005, "_int2", -1, "A", 21, 0, "smallint[]", "ARRAY"},
	{1007, "_int4", -1, "A", 23, 0, "integer[]", "ARRAY"},
	{1009, "_text", -1, "A", 25, 0, "text[]", "ARRAY"},
	{1016, "_int8", -1, "A", 20, 0, "bigint[]", "ARRAY"},
	{114, "json", -1, "U", 0, 199, "json", "json"},
	{199, "_json", -1, "A", 114, 0, "json[]", "ARRAY"},
	{1001, "_bytea", -1, "A", 17, 0, "bytea[]", "ARRAY"},
	{1015, "_varchar", -1, "A", 1043, 0, "character varying[]", "ARRAY"},
	{1022, "_float8", -1, "A", 701, 0, "double precision[]", "ARRAY"},
	{1115, "_timestamp", -1, "A", 1114, 0, "timestamp without time zone[]", "ARRAY"},
	{1185, "_timestamptz", -1, "A", 1184, 0, "timestamp with time zone[]", "ARRAY"},
	{1231, "_numeric", -1, "A", 1700, 0, "numeric[]", "ARRAY"},
	{2951, "_uuid", -1, "A", 2950, 0, "uuid[]", "ARRAY"},
	{3802, "jsonb", -1, "U", 0, 3807, "jsonb", "jsonb"},
	{3807, "_jsonb", -1, "A", 3802, 0, "jsonb[]", "ARRAY"},
	{1042, "bpchar", -1, "S", 0, 1014, "character", "character"},
	{1043, "varchar", -1, "S", 0, 1015, "character varying", "character varying"},
	{1082, "date", 4, "D", 0, 1182, "date", "date"},
	{1114, "timestamp", 8, "D", 0, 1115, "timestamp without time zone", "timestamp without time zone"},
	{1184, "timestamptz", 8, "D", 0, 1185, "timestamp with time zone", "timestamp with time zone"},
	{1186, "interval", 16, "T", 0, 1187, "interval", "interval"},
	{1700, "numeric", -1, "N", 0, 1231, "numeric", "numeric"},
	{2205, "regclass", 4, "N", 0, 2210, "regclass", "regclass"},
	{2950, "uuid", 16, "U", 0, 2951, "uuid", "uuid"},
}

var pgTypeByOID = func() map[int64]*pgType {
	m := map[int64]*pgType{}
	for i := range pgTypes {
		m[pgTypes[i].oid] = &pgTypes[i]
	}
	return m
}()

// catRel is a virtual catalog relation.
type catRel struct {
	cols []relCol
	rows func(c *catSnap) [][]Datum
}

var catalogTables = map[string]*catRel{}

// defCat declares a catalog relation; cols is "name:kind ..." with kinds
// i (integer), t (text), b (bool), a (array), f (float).
func defCat(name, cols string, rows func(c *catSnap) [][]Datum) {
	var rc []relCol
	for _, f := range strings.Fields(cols) {
		n, k, _ := strings.Cut(f, ":")
		kind := map[string]Kind{"i": KInt, "t": KText, "b": KBool, "a": KArray, "f": KFloat, "T": KTime}[k]
		rc = append(rc, relCol{name: n, kind: kind})
	}
	catalogTables[name] = &catRel{cols: rc, rows: rows}
	virtualTables[name] = true
	tableFuncCols[name] = rc
}

// catSnap is the schema as one catalog query sees it.
type catSnap struct {
	s      *Session
	tables []*schema.Table
	seqs   []core.Sequence
}

func (s *Session) catSnap() *catSnap {
	c := &catSnap{s: s, seqs: s.E.Sequences()}
	for _, ts := range s.E.UserTables() {
		c.tables = append(c.tables, ts.Schema())
	}
	sort.Slice(c.tables, func(i, j int) bool { return c.tables[i].Name < c.tables[j].Name })
	return c
}

func tableOID(t *schema.Table) int64 { return relOIDBase + int64(t.OID()) }
func indexOID(t *schema.Table) int64 { return indexOIDBase + tableOID(t) }
func seqOID(name string) int64 {
	h := fnv.New32a()
	h.Write([]byte(name))
	return seqOIDBase + int64(h.Sum32()&0x0fffffff)
}
func pkeyName(t *schema.Table) string { return t.Name + "_pkey" }

// colTypeOID is the type of a column, as pgwire describes it.
func colTypeOID(c schema.Column) int64 { return int64(PgTypeOID(c)) }

// typmodOf is atttypmod: numeric(p,s) is (p<<16|s)+4, else -1.
func typmodOf(c schema.Column) int64 {
	if p, sc, ok := schema.NumericMod(c.Type); ok {
		return int64(p<<16|sc) + 4
	}
	return -1
}

// keyCol is the primary-key column.
func keyCol(t *schema.Table) schema.Column { return t.Columns[t.KeyIndex()] }

// defaultText is a column default as pg_get_expr shows it: PostgreSQL
// shows the sequence argument of nextval as a regclass.
func defaultText(c schema.Column) string {
	d := c.Default
	if strings.HasPrefix(d, "nextval('") && strings.HasSuffix(d, "')") {
		return d[:len(d)-1] + "::regclass)"
	}
	return d
}

// hasDef: identity columns have no pg_attrdef row in PostgreSQL.
func hasDef(c schema.Column) bool { return c.Default != "" && c.Identity == "" }

func identityChar(c schema.Column) string {
	switch c.Identity {
	case "always":
		return "a"
	case "by default":
		return "d"
	}
	return ""
}

func attrdefOID(t *schema.Table, num int) int64 { return 0x20000000 + tableOID(t)*64 + int64(num) }
func checkOID(t *schema.Table, i int) int64     { return 0x30000000 + tableOID(t)*64 + int64(i) }
func pkeyConOID(t *schema.Table) int64          { return 0x30000000 + tableOID(t)*64 + 63 }

func boolStr(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

func i2vec(n ...int) Datum {
	el := make([]Datum, len(n))
	for i, v := range n {
		el[i] = IntD(int64(v))
	}
	d := arrayD(el)
	d.I = int64(KInt)
	return d
}

// seqOwner is the table and column owning a sequence, if any.
func (c *catSnap) seqOwner(q core.Sequence) (*schema.Table, *schema.Column) {
	tn, cn, ok := strings.Cut(q.Owner, ".")
	if !ok {
		return nil, nil
	}
	for _, t := range c.tables {
		if t.Name == tn {
			if i, ok := t.Col(cn); ok {
				return t, &t.Columns[i]
			}
		}
	}
	return nil, nil
}

func seqTypeOID(t string) int64 {
	switch t {
	case "smallint":
		return 21
	case "integer":
		return 23
	}
	return 20
}

// hasNul reports whether a name cannot be a PostgreSQL identifier as is.
func needsQuote(s string) bool {
	if s == "" || reserved[s] {
		return true
	}
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || r == '_' || i > 0 && r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

func quoteIdent(s string) string {
	if !needsQuote(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// vecText is an int2vector or oidvector value as PostgreSQL prints it:
// the elements separated by spaces ("1 2"), which for the one-column
// indexes here is "1". Clients (DBeaver) split it themselves.
func vecText(n ...int) Datum {
	s := make([]string, len(n))
	for i, v := range n {
		s[i] = strconv.Itoa(v)
	}
	return TextD(strings.Join(s, " "))
}

// maxNum is the highest attnum of a table, dropped columns included
// (pg_class.relnatts counts them).
func maxNum(t *schema.Table) int {
	n := 0
	for _, c := range t.Columns {
		n = max(n, c.Num)
	}
	for _, c := range t.Dropped {
		n = max(n, c.Num)
	}
	return n
}

// relOptions are the WITH (...) options of a table as pg_class.reloptions.
func relOptions(t *schema.Table) Datum {
	var o []Datum
	add := func(s string) { o = append(o, TextD(s)) }
	if t.Columnar {
		add("columnar=true")
	}
	if len(t.Search) > 0 {
		add("search=" + strings.Join(t.Search, ","))
	}
	if t.AppendOnly {
		add("append_only=true")
	}
	if t.TimeColumn != "" {
		add("time_column=" + t.TimeColumn)
	}
	if t.SeriesColumn != "" {
		add("series_column=" + t.SeriesColumn)
	}
	if len(o) == 0 {
		return Null
	}
	return arrayD(o)
}
