package shadow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Schema changes on the source (docs/21 section 2).
//
// pgoutput announces a table's shape in a Relation message before the first
// change that uses a new shape. The tool compares that shape with the shape it
// last applied to STASK (kept in the STASK table shadow_schema, so a restart
// in the middle of a change still knows what was done) and applies the
// difference to STASK before the first change in the new shape:
//
//	ALTER TABLE .. ADD COLUMN          ADD COLUMN on STASK; if the column has a default on the
//	                                   source, the existing rows are filled from the source
//	ALTER TABLE .. DROP COLUMN         DROP COLUMN
//	ALTER TABLE .. RENAME COLUMN       RENAME COLUMN (told apart from drop+add by the attnum)
//	ALTER TABLE .. ALTER COLUMN TYPE   ALTER COLUMN TYPE (also precision and length changes)
//
// Everything else stops the tool with the exact DDL to run (primary key
// changes, a type STASK cannot hold) or is not carried (indexes, defaults,
// constraints, new tables: the tool replicates columns and rows, not the
// rest of the schema).

// shapeCol is one column of a table as the tool last applied it.
type shapeCol struct {
	Name   string `json:"name"`
	OID    uint32 `json:"oid"`
	Type   string `json:"type"` // format_type output
	Attnum int    `json:"attnum"`
}

// srcColNow is what the source catalog says about a column right now.
type srcColNow struct {
	Type       string
	Attnum     int
	HasDefault bool
}

// msgCol is a column of a Relation message.
type msgCol struct {
	Name string
	OID  uint32
}

type schemaOp struct {
	Kind     string // add, drop, rename, retype
	Col      string
	NewName  string // rename
	Type     string // add, retype: STASK type
	Backfill bool   // add: fill existing rows from the source
}

func (o schemaOp) sql(table string) string {
	t := qi(table)
	switch o.Kind {
	case "add":
		return "ALTER TABLE " + t + " ADD COLUMN " + qi(o.Col) + " " + o.Type
	case "drop":
		return "ALTER TABLE " + t + " DROP COLUMN " + qi(o.Col)
	case "rename":
		return "ALTER TABLE " + t + " RENAME COLUMN " + qi(o.Col) + " TO " + qi(o.NewName)
	default:
		return "ALTER TABLE " + t + " ALTER COLUMN " + qi(o.Col) + " TYPE " + o.Type
	}
}

// planSchema computes the operations that bring STASK (which has stCols) from
// the shape `known` to the shape of the Relation message, and the new known
// shape. now is the source catalog (types, attnums, defaults). Operations
// already done (a restart after a crash in the middle) are left out.
func planSchema(table string, known []shapeCol, msg []msgCol, stCols map[string]bool, now map[string]srcColNow) ([]schemaOp, []shapeCol, error) {
	byName := map[string]shapeCol{}
	for _, k := range known {
		byName[k.Name] = k
	}
	inMsg := map[string]bool{}
	for _, m := range msg {
		inMsg[m.Name] = true
	}
	var removed []shapeCol
	for _, k := range known {
		if !inMsg[k.Name] {
			removed = append(removed, k)
		}
	}
	var ops []schemaOp
	newKnown := make([]shapeCol, 0, len(msg))
	renamedFrom := map[string]string{} // new name -> old name
	usedOld := map[string]bool{}
	var added []msgCol
	for _, m := range msg {
		if _, ok := byName[m.Name]; !ok {
			added = append(added, m)
		}
	}
	for _, a := range added {
		for _, r := range removed {
			if usedOld[r.Name] || r.OID != a.OID || r.Attnum == 0 {
				continue
			}
			if n, ok := now[a.Name]; ok && n.Attnum == r.Attnum {
				renamedFrom[a.Name] = r.Name
				usedOld[r.Name] = true
				break
			}
		}
	}
	for _, r := range removed {
		if usedOld[r.Name] {
			continue
		}
		if stCols[r.Name] {
			ops = append(ops, schemaOp{Kind: "drop", Col: r.Name})
		}
	}
	for _, m := range msg {
		k := kindOfOID(m.OID)
		n, haveNow := now[m.Name]
		typ := n.Type
		if !haveNow || typ == "" {
			typ = defaultFormatType(m.OID)
		}
		sc := shapeCol{Name: m.Name, OID: m.OID, Type: typ, Attnum: n.Attnum}
		if old, ok := byName[m.Name]; ok {
			if sc.Attnum == 0 {
				sc.Attnum = old.Attnum
			}
			if !haveNow {
				sc.Type = old.Type
			}
			if k == KUnsupported {
				return nil, nil, fmt.Errorf("column %s has unsupported type oid %d", m.Name, m.OID)
			}
			if old.OID != m.OID || (haveNow && old.Type != n.Type && ddlType(k, old.Type) != ddlType(k, n.Type)) {
				ops = append(ops, schemaOp{Kind: "retype", Col: m.Name, Type: ddlType(k, sc.Type)})
			}
		} else if from, ok := renamedFrom[m.Name]; ok {
			if k == KUnsupported {
				return nil, nil, fmt.Errorf("column %s has unsupported type oid %d", m.Name, m.OID)
			}
			if stCols[from] && !stCols[m.Name] {
				ops = append(ops, schemaOp{Kind: "rename", Col: from, NewName: m.Name})
			} else if stCols[from] && stCols[m.Name] {
				ops = append(ops, schemaOp{Kind: "drop", Col: from})
			}
			if old := byName[from]; old.OID != m.OID {
				ops = append(ops, schemaOp{Kind: "retype", Col: m.Name, Type: ddlType(k, sc.Type)})
			}
		} else {
			if k == KUnsupported {
				return nil, nil, fmt.Errorf("new column %s has type oid %d, which stashadow does not support", m.Name, m.OID)
			}
			if !stCols[m.Name] {
				ops = append(ops, schemaOp{Kind: "add", Col: m.Name, Type: ddlType(k, sc.Type), Backfill: haveNow && n.HasDefault})
			}
		}
		newKnown = append(newKnown, sc)
	}
	// order: drops and renames first (a rename can free a name an add needs)
	var first, rest []schemaOp
	for _, o := range ops {
		if o.Kind == "drop" || o.Kind == "rename" {
			first = append(first, o)
		} else {
			rest = append(rest, o)
		}
	}
	return append(first, rest...), newKnown, nil
}

// defaultFormatType is the type text for a column whose source type text is
// unknown (the column is gone from the source by now).
func defaultFormatType(oid uint32) string {
	switch oid {
	case 21:
		return "smallint"
	case 23:
		return "integer"
	case 20:
		return "bigint"
	case 1043:
		return "character varying"
	case 1042:
		return "character"
	}
	return ""
}

// ---- persistence and application ----

const schemaTable = "shadow_schema"

func ensureSchemaTable(ctx context.Context, st *pgconn.PgConn) error {
	if cols, ok, err := stColumns(ctx, st, schemaTable); err != nil {
		return err
	} else if ok && len(cols) > 0 {
		return nil
	}
	return exec(ctx, st, "CREATE TABLE "+schemaTable+" (slot text NOT NULL, tbl text NOT NULL, shape text NOT NULL, PRIMARY KEY (slot, tbl))")
}

func loadShape(ctx context.Context, st *pgconn.PgConn, slot, table string) ([]shapeCol, bool, error) {
	r, err := query(ctx, st, "SELECT shape FROM "+schemaTable+" WHERE slot = $1 AND tbl = $2", &slot, &table)
	if err != nil {
		return nil, false, err
	}
	if len(r.Data) == 0 {
		return nil, false, nil
	}
	var cols []shapeCol
	if err := json.Unmarshal([]byte(*r.Data[0][0]), &cols); err != nil {
		return nil, false, err
	}
	return cols, true, nil
}

func saveShape(ctx context.Context, st *pgconn.PgConn, slot, table string, cols []shapeCol) error {
	b, _ := json.Marshal(cols)
	js := string(b)
	if _, err := query(ctx, st, "DELETE FROM "+schemaTable+" WHERE slot = $1 AND tbl = $2", &slot, &table); err != nil {
		return err
	}
	_, err := query(ctx, st, "INSERT INTO "+schemaTable+" (slot, tbl, shape) VALUES ($1, $2, $3)", &slot, &table, &js)
	return err
}

func shapeOf(t *Table, attnums map[string]int) []shapeCol {
	out := make([]shapeCol, len(t.Cols))
	for i, c := range t.Cols {
		out[i] = shapeCol{Name: c.Name, OID: c.OID, Type: c.Type, Attnum: attnums[c.Name]}
	}
	return out
}

// sourceNow reads types, attnums and defaults of a source table.
func sourceNow(ctx context.Context, src *pgconn.PgConn, table string) (map[string]srcColNow, error) {
	rows, err := query(ctx, src, `select a.attname, format_type(a.atttypid, a.atttypmod), a.attnum::int, a.atthasdef::text
		from pg_attribute a where a.attrelid = ('public.' || quote_ident($1))::regclass and a.attnum > 0 and not a.attisdropped`, &table)
	if err != nil {
		return nil, err
	}
	out := map[string]srcColNow{}
	for _, r := range rows.Data {
		n, _ := strconv.Atoi(*r[2])
		out[*r[0]] = srcColNow{Type: *r[1], Attnum: n, HasDefault: *r[3] == "true"}
	}
	return out, nil
}

// evolve applies a schema change announced by a Relation message to STASK.
// It returns the (possibly updated) table description.
func (r *runner) evolve(ctx context.Context, m relMsg, t *Table, stcols map[string]map[string]bool) error {
	known, ok := r.shapes[t.Name]
	now, err := sourceNow(ctx, r.srcConn, t.Name)
	if err != nil {
		return fmt.Errorf("read source catalog of %s: %w", t.Name, err)
	}
	if !ok {
		known = shapeOf(t, attnumsOf(now))
	}
	ops, newKnown, err := planSchema(t.Name, known, m.cols, stcols[t.Name], now)
	if err != nil {
		return &FatalError{&DDLError{Table: t.Name, Msg: err.Error()}}
	}
	if len(ops) == 0 && ok && shapeEqual(known, newKnown) {
		return nil
	}
	if r.cfg.SchemaChanges == "stop" && len(ops) > 0 {
		var sqls []string
		for _, o := range ops {
			sqls = append(sqls, o.sql(t.Name))
		}
		return &FatalError{&DDLError{Table: t.Name, Msg: "the source changed this table; run on STASK, then restart (-schema-changes stop): " + strings.Join(sqls, "; ")}}
	}
	// primary key changes are not carried
	for _, p := range t.PK {
		name := t.Cols[p].Name
		found := false
		for _, c := range m.cols {
			if c.Name == name {
				found = true
			}
		}
		if !found {
			return &FatalError{&DDLError{Table: t.Name, Msg: fmt.Sprintf("primary key column %s was dropped or renamed on the source; fix STASK by hand and run with -reset", name)}}
		}
	}
	if len(ops) > 0 {
		dc, err := r.ddlConn(ctx)
		if err != nil {
			return err
		}
		for _, o := range ops {
			r.cfg.Logf("schema change on %s: %s", t.Name, o.sql(t.Name))
			if err := exec(ctx, dc, o.sql(t.Name)); err != nil {
				return &FatalError{&DDLError{Table: t.Name, Msg: fmt.Sprintf("STASK rejected %q: %v", o.sql(t.Name), err)}}
			}
			switch o.Kind {
			case "add":
				stcols[t.Name][o.Col] = true
			case "drop":
				delete(stcols[t.Name], o.Col)
			case "rename":
				delete(stcols[t.Name], o.Col)
				stcols[t.Name][o.NewName] = true
			}
			if o.Kind == "add" && o.Backfill {
				if err := r.backfill(ctx, dc, t, o.Col); err != nil {
					return err
				}
			}
		}
		r.cfg.Stats.schemaChanges.Add(int64(len(ops)))
	}
	if r.stConn != nil {
		dc, err := r.ddlConn(ctx)
		if err != nil {
			return err
		}
		if err := saveShape(ctx, dc, r.cfg.Slot, t.Name, newKnown); err != nil {
			return err
		}
	}
	r.shapes[t.Name] = newKnown
	// the table description follows the new shape
	pkNames := t.pkNames()
	t.Cols = t.Cols[:0]
	for _, c := range newKnown {
		t.Cols = append(t.Cols, Column{Name: c.Name, OID: c.OID, Type: c.Type, Kind: kindOfOID(c.OID)})
	}
	t.PK = t.PK[:0]
	for _, n := range pkNames {
		t.PK = append(t.PK, t.colIndex(n))
	}
	return nil
}

func attnumsOf(now map[string]srcColNow) map[string]int {
	out := make(map[string]int, len(now))
	for k, v := range now {
		out[k] = v.Attnum
	}
	return out
}

func shapeEqual(a, b []shapeCol) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type relMsg struct{ cols []msgCol }

// ddlConn is a second STASK connection, used for DDL and backfills, so that
// they do not interleave with the open apply transaction.
func (r *runner) ddlConn(ctx context.Context) (*pgconn.PgConn, error) {
	if r.stConn != nil {
		return r.stConn, nil
	}
	c, err := connect(ctx, r.cfg.StaskURL, false)
	if err != nil {
		return nil, err
	}
	r.stConn = c
	return c, nil
}

// backfill fills a new column of existing STASK rows from the source (the
// column has a default there, so old rows hold a value). The stream carries
// later changes of those rows in full, which overwrite what is written here.
func (r *runner) backfill(ctx context.Context, dc *pgconn.PgConn, t *Table, col string) error {
	if len(t.PK) == 0 {
		r.cfg.Logf("WARNING: %s.%s has a default on the source but %s has no primary key: existing STASK rows keep NULL there; run with -reset to copy again", t.Name, col, t.Name)
		return nil
	}
	pk := t.pkNames()
	sel := "SELECT " + qiList(pk) + ", " + qi(col) + " FROM " + qi(t.Name)
	var where []string
	for i, p := range pk {
		where = append(where, qi(p)+" = $"+strconv.Itoa(i+2))
	}
	upd := "UPDATE " + qi(t.Name) + " SET " + qi(col) + " = $1 WHERE " + strings.Join(where, " AND ")
	rr := r.srcConn.ExecParams(ctx, sel, nil, nil, nil, nil)
	var batch []stmt
	total := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := execBatch(ctx, dc, batch)
		total += len(batch)
		batch = batch[:0]
		return err
	}
	for rr.NextRow() {
		v := rr.Values()
		args := make([][]byte, 0, len(v))
		args = append(args, append([]byte(nil), v[len(v)-1]...))
		if v[len(v)-1] == nil {
			args[0] = nil
		}
		for i := 0; i < len(v)-1; i++ {
			args = append(args, append([]byte(nil), v[i]...))
		}
		batch = append(batch, stmt{sql: upd, args: args})
		if len(batch) >= 1000 {
			if err := flush(); err != nil {
				rr.Close()
				return fmt.Errorf("backfill %s.%s: %w", t.Name, col, err)
			}
		}
	}
	if _, err := rr.Close(); err != nil {
		return fmt.Errorf("backfill %s.%s: read source: %w", t.Name, col, err)
	}
	if err := flush(); err != nil {
		return fmt.Errorf("backfill %s.%s: %w", t.Name, col, err)
	}
	r.cfg.Logf("backfilled %s.%s from the source: %d rows", t.Name, col, total)
	return nil
}

// execBatch sends statements in one pipeline and returns the first error.
func execBatch(ctx context.Context, c *pgconn.PgConn, batch []stmt) error {
	p := c.StartPipeline(ctx)
	for _, s := range batch {
		p.SendQueryParams(s.sql, s.args, nil, nil, nil)
	}
	p.Sync()
	if err := p.Flush(); err != nil {
		p.Close()
		return err
	}
	var first error
loop:
	for {
		res, err := p.GetResults()
		if err != nil {
			if first == nil {
				first = err
			}
			if !isPgErr(err) {
				break loop
			}
			continue
		}
		switch x := res.(type) {
		case *pgconn.ResultReader:
			if _, err := x.Close(); err != nil && first == nil {
				first = err
			}
		case *pgconn.PipelineSync, nil:
			break loop
		}
	}
	if err := p.Close(); first == nil {
		first = err
	}
	return first
}
