package sql

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// Execution of CREATE TABLE AS / SELECT INTO, CREATE TABLE (LIKE ..),
// materialized views and TRUNCATE ... CASCADE (docs/05, last section).

const (
	codeNotPopulated = "55000"
	codeWrongObject  = "42809"
)

// runExt runs the statements of this file inside the transaction tx.
func (s *Session) runExt(tx *core.Tx, st Stmt, params []Datum) (*Result, error) {
	switch st := st.(type) {
	case *CreateAs:
		return s.createAs(tx, st, params)
	case *RefreshMat:
		return s.refreshMat(tx, st, params)
	case *DropMat:
		return s.dropMat(st)
	case *Merge:
		return s.merge(tx, st, params)
	}
	return nil, errf(codeFeature, "unsupported statement %T", st)
}

// ---- result columns -> table columns ----

var arrayElemType = map[uint32]string{
	1016: "int[]", 1022: "float[]", 1000: "bool[]", 1185: "timestamptz[]", 1115: "timestamp[]",
	1231: "numeric[]", 2951: "uuid[]", 1009: "text[]", 1015: "string[]",
}

// colDefFor is the column a query output column becomes. src is the table
// column the output is a plain copy of (nil: unknown); a copy keeps its
// type modifier (numeric(12,2)), as PostgreSQL's does.
func colDefFor(c Column, src *schema.Column) (ColDef, error) {
	cd := ColDef{Name: c.Name}
	switch c.Kind {
	case KInt:
		cd.Type = "int"
		switch c.OID {
		case tyInt2:
			cd.PG = "int2"
		case tyInt4:
			cd.PG = "int4"
		}
	case KFloat:
		cd.Type = "float"
		if c.OID == tyFloat4 {
			cd.PG = "float4"
		}
	case KText, KNull:
		cd.Type = "text"
		n := int(c.Mod) - 4
		switch c.OID {
		case tyVarchar:
			cd.Type, cd.PG = "string", "varchar"
			if c.Mod >= 5 {
				cd.PG = "varchar(" + strconv.Itoa(n) + ")"
			}
		case tyBpchar:
			cd.Type, cd.PG = "string", "bpchar"
			if c.Mod >= 5 {
				cd.PG = "bpchar(" + strconv.Itoa(n) + ")"
			}
		case tyName:
			cd.Type, cd.PG = "string", "name"
		case tyTime:
			cd.Type, cd.PG = "string", "time"
		}
	case KBool:
		cd.Type = "bool"
	case KTime:
		cd.Type = "timestamptz"
	case KTimestamp:
		cd.Type = "timestamp"
	case KDate:
		cd.Type, cd.PG = "timestamp", "date"
	case KNumeric:
		cd.Type = "numeric"
		if src != nil && strings.HasPrefix(src.Type, "numeric(") {
			cd.Type = src.Type
		}
	case KUUID:
		cd.Type = "uuid"
	case KBytea:
		cd.Type = "bytea"
	case KJSON:
		cd.Type = "json"
	case KJSONB:
		cd.Type = "jsonb"
	case KArray:
		t, ok := arrayElemType[c.OID]
		if !ok {
			t = "text[]"
		}
		if src != nil && strings.HasSuffix(src.Type, "[]") {
			t = src.Type
		}
		cd.Type = t
	default:
		return cd, errf(codeFeature, "columns of type %s are not supported", c.Kind)
	}
	return cd, nil
}

// sourceCols finds, for each output column of a query, the table column it
// copies. It understands a SELECT over tables (joins included), * and plain
// column references; every other output has none.
func (s *Session) sourceCols(q Stmt, n int) []*schema.Column {
	out := make([]*schema.Column, n)
	sel, ok := q.(*Select)
	if !ok {
		if so, ok := q.(*SetOp); ok {
			return s.sourceCols(so.L, n)
		}
		return out
	}
	type rel struct {
		qual string
		t    *schema.Table
	}
	var rels []rel
	var add func(f FromNode)
	add = func(f FromNode) {
		switch f := f.(type) {
		case *FromTable:
			if t, err := s.table(f.Name); err == nil && len(f.ColAliases) == 0 {
				q := f.Alias
				if q == "" {
					q = f.Name
				}
				rels = append(rels, rel{q, t})
			}
		case *FromJoin:
			add(f.L)
			add(f.R)
		}
	}
	if sel.Rel != nil {
		add(sel.Rel)
	} else if sel.From != "" {
		add(&FromTable{Name: sel.From, Alias: sel.Alias})
	}
	if len(rels) == 0 {
		return out
	}
	i := 0
	for _, it := range sel.Items {
		switch {
		case it.Star:
			for _, r := range rels {
				for _, ci := range r.t.VisibleCols() {
					if i < n {
						out[i] = &r.t.Columns[ci]
					}
					i++
				}
			}
		default:
			if cr, ok := it.X.(*ColRef); ok && i < n {
				var found *schema.Column
				cnt := 0
				for _, r := range rels {
					if cr.Table != "" && cr.Table != r.qual {
						continue
					}
					if ci, ok := r.t.Col(cr.Name); ok {
						found = &r.t.Columns[ci]
						cnt++
					}
				}
				if cnt == 1 {
					out[i] = found
				}
			}
			i++
		}
	}
	return out
}

// resultDefs are the columns of the table that holds the rows of res.
func (s *Session) resultDefs(res *Result, names []string, q Stmt) ([]ColDef, error) {
	if len(names) > len(res.Cols) {
		return nil, errf(codeSyntax, "too many column names were specified")
	}
	src := s.sourceCols(q, len(res.Cols))
	defs := make([]ColDef, len(res.Cols))
	for i, c := range res.Cols {
		cd, err := colDefFor(c, src[i])
		if err != nil {
			return nil, err
		}
		if i < len(names) {
			cd.Name = names[i]
		}
		defs[i] = cd
	}
	return defs, nil
}

// relExists reports whether a relation of this name is visible to the session.
func (s *Session) relExists(name string) bool {
	if _, ok := s.tempReal(name); ok {
		if r, _ := s.tempReal(name); r != name {
			return true
		}
	}
	return s.relationExists(name)
}

// ---- CREATE TABLE AS / CREATE MATERIALIZED VIEW ----

func (s *Session) createAs(tx *core.Tx, st *CreateAs, params []Datum) (*Result, error) {
	tag := "CREATE TABLE AS"
	what := "table"
	if st.Mat {
		tag, what = "CREATE MATERIALIZED VIEW", "materialized view"
	}
	_ = what
	exists := false
	if st.Temp {
		_, exists = s.tempReal(st.Name)
	} else {
		exists = s.relationExists(st.Name)
	}
	if exists {
		if st.IfNotExists {
			return &Result{Tag: "SELECT 0", Notice: fmt.Sprintf("relation %q already exists, skipping", st.Name)}, nil
		}
		return nil, errf(codeDuplicateTable, "relation %q already exists", st.Name)
	}
	q := st.Query
	if st.NoData {
		// only the output columns are needed: LIMIT 0 over the query
		q = &Select{Items: []SelItem{{Star: true}}, Rel: &FromSubquery{Q: st.Query, Alias: "q"}, Limit: &Lit{V: IntD(0)}}
	}
	res, err := s.runQuery(tx, q, params)
	if err != nil {
		return nil, err
	}
	defs, err := s.resultDefs(res, st.Cols, st.Query)
	if err != nil {
		return nil, err
	}
	ct := &CreateTable{Name: st.Name, Cols: defs, Options: map[string]string{},
		Ext: &tableExt{Temp: st.Temp, Unlogged: st.Unlogged, OnCommit: st.OnCommit}}
	if st.Mat {
		ct.Ext.Mat, ct.Ext.NoData = st.Text, st.NoData
	}
	if _, err := s.createTableExt(ct, true); err != nil {
		return nil, err
	}
	real := st.Name
	if st.Temp {
		real = s.mapTemp(st.Name)
	}
	fail := func(err error) (*Result, error) {
		if st.Temp {
			s.dropTempNamed(st.Name)
		} else {
			s.dropReal(real)
		}
		return nil, err
	}
	if len(res.Rows) > 0 && !st.NoData {
		ins := &Insert{Table: st.Name, Rows: make([][]Expr, len(res.Rows))}
		for i, r := range res.Rows {
			row := make([]Expr, len(r))
			for j, v := range r {
				row[j] = &Lit{V: v}
			}
			ins.Rows[i] = row
		}
		for _, d := range defs {
			ins.Cols = append(ins.Cols, d.Name)
		}
		if _, err := s.insert(tx, ins, params); err != nil {
			return fail(err)
		}
	}
	_ = tag
	return &Result{Tag: fmt.Sprintf("SELECT %d", len(res.Rows))}, nil
}

// extTable applies the table options of c.Ext to the table being created.
func (s *Session) extTable(c *CreateTable, t *schema.Table) {
	if isTempName(c.Name) {
		t.System = true
	}
	if e := c.Ext; e != nil {
		t.Unlogged = e.Unlogged
		t.MatView = e.Mat
		t.MatUnpopulated = e.Mat != "" && e.NoData
	}
}

// createTableExt creates a table with LIKE elements, TEMP / UNLOGGED /
// ON COMMIT, or the result columns of a CTAS.
func (s *Session) createTableExt(c *CreateTable, inTx bool) (*Result, error) {
	cc := *c
	e := *c.Ext
	cc.Ext = &tableExt{Unlogged: e.Unlogged, Mat: e.Mat, NoData: e.NoData, done: true}
	if err := s.expandLikes(&cc, e.Likes); err != nil {
		return nil, err
	}
	var post []*CreateIndex
	if len(e.Likes) > 0 {
		post = s.likeIndexes(&cc, e.Likes)
	}
	if e.Temp {
		if _, dup := s.tempReal(c.Name); dup {
			if c.IfNotExists {
				return &Result{Tag: "CREATE TABLE", Notice: fmt.Sprintf("relation %q already exists, skipping", c.Name)}, nil
			}
			return nil, errf(codeDuplicateTable, "relation %q already exists", c.Name)
		}
		cc.Name = s.newTempName(c.Name)
	}
	res, err := s.createTable(&cc)
	if err != nil {
		return nil, err
	}
	if e.Temp {
		s.registerTemp(c.Name, cc.Name, e.OnCommit)
	}
	for _, ix := range post {
		ix.Table = cc.Name
		if _, err := s.createIndex(ix); err != nil {
			if e.Temp {
				s.dropTempNamed(c.Name)
			} else {
				s.dropReal(cc.Name)
			}
			return nil, err
		}
	}
	if e.Temp && !inTx && !s.explicit && e.OnCommit == "drop" {
		s.dropTempNamed(c.Name) // an autocommit CREATE is its own transaction
	}
	return res, nil
}

// expandLikes splices the columns (and constraints) of the LIKE sources
// into c.
func (s *Session) expandLikes(c *CreateTable, likes []likeDef) error {
	if len(likes) == 0 {
		return nil
	}
	cols := slices.Clone(c.Cols)
	shift := 0
	for _, l := range likes {
		src, err := s.table(l.Table)
		if err != nil {
			return errf(codeUndefinedTable, "relation %q does not exist", l.Table)
		}
		var add []ColDef
		for _, ci := range src.VisibleCols() {
			sc := src.Columns[ci]
			cd := ColDef{Name: sc.Name, Type: sc.Type, PG: sc.PG, NotNull: sc.NotNull || sc.Name == src.Key && src.HasPK()}
			if sc.Identity != "" {
				if l.Iden {
					cd.Identity = sc.Identity
					cd.IntType = "bigint"
					switch sc.PG {
					case "int2":
						cd.IntType = "smallint"
					case "int4":
						cd.IntType = "integer"
					}
					cd.NotNull = true
				}
			} else if l.Defaults && sc.Default != "" {
				cd.Default = sc.Default
			}
			add = append(add, cd)
		}
		at := min(l.At+shift, len(cols))
		cols = slices.Insert(cols, at, add...)
		shift += len(add)
		if l.Constraints {
			for _, ck := range src.Checks {
				c.Checks = append(c.Checks, CheckDef{Name: "", Expr: ck.Expr})
			}
		}
		if l.Indexes {
			if src.HasPK() {
				if len(src.KeyCols) > 1 {
					if c.Key == "" && c.KeyCols == nil {
						c.KeyCols = slices.Clone(src.KeyCols)
					}
				} else if c.Key == "" && c.KeyCols == nil {
					c.Key = src.Key
				}
			}
			for _, ix := range src.Indexes {
				if ix.Internal || !ix.Unique || ix.Constraint == "" {
					continue
				}
				c.Constraints = append(c.Constraints, &ConstraintDef{Kind: "unique", Cols: slices.Clone(ix.Columns)})
			}
		}
	}
	c.Cols = cols
	return nil
}

// likeIndexes are the plain indexes INCLUDING INDEXES copies (the table name
// is filled in once the table has its final name).
func (s *Session) likeIndexes(c *CreateTable, likes []likeDef) []*CreateIndex {
	var out []*CreateIndex
	for _, l := range likes {
		if !l.Indexes {
			continue
		}
		src, err := s.table(l.Table)
		if err != nil {
			continue
		}
		for _, ix := range src.Indexes {
			if ix.Internal || ix.Constraint != "" {
				continue
			}
			out = append(out, &CreateIndex{Cols: slices.Clone(ix.Columns), Unique: ix.Unique})
		}
	}
	return out
}

// ---- materialized views ----

// checkMatRead refuses to read a materialized view that has no data.
func (s *Session) matGuard(t *schema.Table) error {
	if t.MatUnpopulated && s.refreshing != t.Name {
		return errf(codeNotPopulated, "materialized view %q has not been populated", t.Name)
	}
	return nil
}

// rejectMatWrite is the 42809 for INSERT / UPDATE / DELETE on a materialized view.
func (s *Session) rejectMatWrite(table string) error {
	if ts, err := s.userTable(table); err == nil && ts.Schema().MatView != "" {
		return errf(codeWrongObject, "cannot change materialized view %q", table)
	}
	return nil
}

func (s *Session) matTable(name string) (*schema.Table, error) {
	ts, err := s.userTable(name)
	if err != nil {
		if s.isView(name) {
			return nil, errf(codeWrongObject, "%q is not a materialized view", name)
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", name)
	}
	t := ts.Schema()
	if t.MatView == "" {
		return nil, errf(codeWrongObject, "%q is not a materialized view", name)
	}
	return t, nil
}

// rowSig is a canonical text of the values of a row at the given columns.
func rowSig(r schema.Row, cols []int) string {
	var b strings.Builder
	for _, ci := range cols {
		v := r[ci]
		switch {
		case v.Null:
			b.WriteString("N;")
		default:
			b.WriteString("v")
			b.WriteString(strconv.FormatInt(v.I, 10))
			b.WriteByte(',')
			b.WriteString(strconv.FormatUint(math.Float64bits(v.F), 16))
			b.WriteByte(',')
			b.WriteString(strconv.Itoa(len(v.S)))
			b.WriteByte(':')
			b.WriteString(v.S)
			b.WriteByte(';')
		}
	}
	return b.String()
}

func (s *Session) refreshMat(tx *core.Tx, st *RefreshMat, params []Datum) (*Result, error) {
	t, err := s.matTable(st.Name)
	if err != nil {
		return nil, err
	}
	if st.NoData {
		if st.Concurrently {
			return nil, errf(codeSyntax, "CONCURRENTLY and WITH NO DATA options cannot be used together")
		}
		if err := s.E.TruncateTable(t.Name); err != nil {
			return nil, AsError(err)
		}
		return &Result{Tag: "REFRESH MATERIALIZED VIEW"}, asErr(s.E.AlterTable(t.Name, func(d *schema.Table) error {
			d.MatUnpopulated = true
			return nil
		}))
	}
	if st.Concurrently {
		if t.MatUnpopulated {
			return nil, errf(codeNotPopulated, "CONCURRENTLY cannot be used when the materialized view is not populated")
		}
		uniq := false
		for _, ix := range t.Indexes {
			if ix.Unique && !ix.Internal {
				uniq = true
			}
		}
		if !uniq {
			return nil, errf(codeNotPopulated, "cannot refresh materialized view %q concurrently (create a unique index with no WHERE clause on one or more columns of the materialized view)", st.Name)
		}
	}
	q, err := ParseOne(t.MatView)
	if err != nil {
		return nil, err
	}
	s.refreshing = t.Name
	defer func() { s.refreshing = "" }()
	res, err := s.runQuery(tx, q, params)
	if err != nil {
		return nil, err
	}
	vis := t.VisibleCols()
	if len(res.Cols) != len(vis) {
		return nil, errf(codeFeature, "materialized view %q: the query now returns %d columns, not %d", st.Name, len(res.Cols), len(vis))
	}
	// the multiset difference between the rows now stored and the new rows:
	// only what changed is written
	old := map[string][]string{}
	err = tx.ScanAll(t.Name, func(key string, r schema.Row) bool {
		sg := rowSig(r, vis)
		old[sg] = append(old[sg], key)
		return true
	})
	if err != nil {
		return nil, err
	}
	var add [][]Expr
	for _, r := range res.Rows {
		row := make(schema.Row, len(t.Columns))
		for j, ci := range vis {
			if row[ci], err = toValueCol(&t.Columns[ci], r[j]); err != nil {
				return nil, err
			}
		}
		sg := rowSig(row, vis)
		if keys := old[sg]; len(keys) > 0 {
			old[sg] = keys[:len(keys)-1]
			continue
		}
		lits := make([]Expr, len(r))
		for j, v := range r {
			lits[j] = &Lit{V: v}
		}
		add = append(add, lits)
	}
	for _, keys := range old {
		for _, k := range keys {
			if err := tx.Delete(t.Name, k); err != nil {
				return nil, err
			}
		}
	}
	if len(add) > 0 {
		ins := &Insert{Table: st.Name, Rows: add}
		for _, ci := range vis {
			ins.Cols = append(ins.Cols, t.Columns[ci].Name)
		}
		if _, err := s.insert(tx, ins, params); err != nil {
			return nil, err
		}
	}
	if t.MatUnpopulated {
		if err := s.E.AlterTable(t.Name, func(d *schema.Table) error { d.MatUnpopulated = false; return nil }); err != nil {
			return nil, AsError(err)
		}
	}
	return &Result{Tag: "REFRESH MATERIALIZED VIEW"}, nil
}

func (s *Session) dropMat(st *DropMat) (*Result, error) {
	res := &Result{Tag: "DROP MATERIALIZED VIEW"}
	var todo []string
	for _, n := range st.Names {
		if _, err := s.matTable(n); err != nil {
			if e, ok := err.(*Error); ok && e.Code == codeUndefinedTable && st.IfExists {
				res.Notice = fmt.Sprintf("materialized view %q does not exist, skipping", n)
				continue
			}
			return nil, err
		}
		todo = append(todo, n)
	}
	if len(todo) > 0 {
		if _, err := s.dropOrTruncate(&DropTable{Names: todo, Cascade: st.Cascade}); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ---- TRUNCATE ... CASCADE ----

// truncateClosure adds to names the tables with a foreign key to them
// (transitively), for TRUNCATE ... CASCADE; note lists the added tables.
func (s *Session) truncateClosure(names []string) (all []string, added []string) {
	all = slices.Clone(names)
	for i := 0; i < len(all); i++ {
		for _, r := range s.E.FKRefs(all[i]) {
			if !slices.Contains(all, r.Child) {
				all = append(all, r.Child)
				added = append(added, r.Child)
			}
		}
	}
	return all, added
}
