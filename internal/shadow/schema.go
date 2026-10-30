package shadow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Column is one source column.
type Column struct {
	Name    string
	OID     uint32
	Type    string // format_type output, e.g. numeric(12,2)
	Kind    Kind
	NotNull bool
}

// Table is a source table as found in the catalog of the source database.
type Table struct {
	Name      string
	Cols      []Column
	PK        []int // indexes into Cols, in key order; empty = no primary key
	ReplIdent byte  // pg_class.relreplident: d default, f full, i index, n nothing
}

func (t *Table) colIndex(name string) int {
	for i, c := range t.Cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func (t *Table) pkNames() []string {
	n := make([]string, len(t.PK))
	for i, p := range t.PK {
		n[i] = t.Cols[p].Name
	}
	return n
}

func (t *Table) colNames() []string {
	n := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		n[i] = c.Name
	}
	return n
}

// CreateDDL returns the STASK CREATE TABLE for the table.
func (t *Table) CreateDDL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + qi(t.Name) + " (")
	for i, c := range t.Cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(qi(c.Name) + " " + ddlType(c.Kind, c.Type))
		if c.NotNull {
			b.WriteString(" NOT NULL")
		}
	}
	if len(t.PK) > 0 {
		b.WriteString(", PRIMARY KEY (" + qiList(t.pkNames()) + ")")
	}
	b.WriteString(")")
	return b.String()
}

// Check reports unsupported column types and tables whose replica identity
// cannot carry UPDATE/DELETE keys.
func (t *Table) Check() error {
	for _, c := range t.Cols {
		if c.Kind == KUnsupported {
			return fmt.Errorf("table %s: column %s has type %s (oid %d), which stashadow does not support (supported: bool, int2/4/8, text, varchar, char, numeric, float4/8, uuid, bytea, timestamp, timestamptz, date, time, json, jsonb)", t.Name, c.Name, c.Type, c.OID)
		}
	}
	if len(t.PK) == 0 && t.ReplIdent != 'f' {
		return fmt.Errorf("table %s has no primary key and REPLICA IDENTITY is not FULL, so the source cannot even UPDATE/DELETE it through logical replication; run on the source: ALTER TABLE %s REPLICA IDENTITY FULL (or start with -set-replica-identity)", t.Name, qi(t.Name))
	}
	return nil
}

func pgTextArray(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + strings.ReplaceAll(strings.ReplaceAll(n, `\`, `\\`), `"`, `\"`) + `"`
	}
	return "{" + strings.Join(q, ",") + "}"
}

// LoadSourceTables reads the catalog of the source for the named public tables.
func LoadSourceTables(ctx context.Context, c *pgconn.PgConn, names []string) ([]*Table, error) {
	arr := pgTextArray(names)
	rows, err := query(ctx, c, `select c.relname, a.attname, a.atttypid::int, format_type(a.atttypid, a.atttypmod), a.attnotnull::text, c.relreplident::text
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		join pg_attribute a on a.attrelid = c.oid
		where n.nspname = 'public' and c.relkind in ('r','p') and c.relname = any($1::text[]) and a.attnum > 0 and not a.attisdropped
		order by c.relname, a.attnum`, &arr)
	if err != nil {
		return nil, fmt.Errorf("read source catalog: %w", err)
	}
	by := map[string]*Table{}
	for _, r := range rows.Data {
		t := by[*r[0]]
		if t == nil {
			t = &Table{Name: *r[0], ReplIdent: (*r[5])[0]}
			by[t.Name] = t
		}
		var oid int
		fmt.Sscan(*r[2], &oid)
		t.Cols = append(t.Cols, Column{Name: *r[1], OID: uint32(oid), Type: *r[3], Kind: kindOfOID(uint32(oid)), NotNull: *r[4] == "true"})
	}
	pks, err := query(ctx, c, `select c.relname, a.attname
		from pg_index i join pg_class c on c.oid = i.indrelid join pg_namespace n on n.oid = c.relnamespace
		cross join lateral unnest(i.indkey) with ordinality as k(attnum, ord)
		join pg_attribute a on a.attrelid = c.oid and a.attnum = k.attnum
		where i.indisprimary and n.nspname = 'public' and c.relname = any($1::text[])
		order by c.relname, k.ord`, &arr)
	if err != nil {
		return nil, fmt.Errorf("read source primary keys: %w", err)
	}
	for _, r := range pks.Data {
		if t := by[*r[0]]; t != nil {
			t.PK = append(t.PK, t.colIndex(*r[1]))
		}
	}
	var out []*Table
	for _, n := range names {
		t := by[n]
		if t == nil {
			return nil, fmt.Errorf("source has no table public.%s", n)
		}
		out = append(out, t)
	}
	return out, nil
}

// stColumns returns the column names of a STASK table, or nil, false when the
// table does not exist.
func stColumns(ctx context.Context, c *pgconn.PgConn, table string) ([]string, bool, error) {
	rr := c.ExecParams(ctx, "SELECT * FROM "+qi(table)+" WHERE false", nil, nil, nil, nil)
	var cols []string
	for _, f := range rr.FieldDescriptions() {
		cols = append(cols, f.Name)
	}
	_, err := rr.Close()
	if err != nil {
		if isPgCode(err, "42P01") || strings.Contains(err.Error(), "does not exist") {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(cols) == 0 {
		for _, f := range rr.FieldDescriptions() {
			cols = append(cols, f.Name)
		}
	}
	return cols, true, nil
}
