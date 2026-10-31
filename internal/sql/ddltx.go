package sql

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// Transactional DDL (docs/05 "DDL trong giao dịch", docs/14 Appendix L, O).
//
// Inside BEGIN .. COMMIT/ROLLBACK a journal makes DDL behave atomically.
// Two mechanisms:
//
//   - APPLY AT ONCE + UNDO: CREATE TABLE (also CTAS), CREATE INDEX, ADD
//     COLUMN, ADD CHECK/UNIQUE/FOREIGN KEY, RENAME, ALTER COLUMN TYPE /
//     DEFAULT / NOT NULL, CREATE/DROP/ALTER of views, sequences, types,
//     domains, schemas and COMMENT run at once (the rest of the transaction
//     sees them) after an UNDO step was journaled. ROLLBACK, ROLLBACK TO
//     SAVEPOINT, a failed COMMIT, the session ending or a crash run the
//     undo steps in reverse. Objects without data (views, sequences, types,
//     ...) are undone from a before-image taken when the statement ran.
//   - HIDE or DEFER: statements that destroy data are not run until COMMIT.
//     DROP TABLE and DROP COLUMN hide the object at once by renaming it to a
//     private name (undo: rename back; at COMMIT the hidden object is
//     dropped), so the transaction sees it as gone, the name is free for a
//     new object, and the dropped object cannot gain dependents. When the
//     object cannot be renamed (triggers, foreign keys from other tables,
//     dependent views, rows written by this transaction) and for DROP
//     INDEX / DROP CONSTRAINT, the drop is DEFERRED: validated now and run
//     after COMMIT; the object stays visible until then.
//
// The journal is two rows of the object catalog (kind "ddltx"): "o-<id>"
// holds the undo steps (rewritten, outside the transaction, before each
// DDL statement) and "c-<id>" holds the redo steps and is written INSIDE
// the user's transaction, so it commits atomically with the transaction's
// own rows. Recovery (once per engine, at the first session): "o-" and "c-"
// mean the transaction committed, so the redo steps run again; "o-" alone
// means it did not, so the undo steps run. All steps are idempotent. No
// on-disk format changes: these are ordinary rows.
//
// A redo step (DROP of a hidden object, or a deferred drop) runs with
// CASCADE: the transaction is durable, so the drop must win over a
// dependent object another session created meanwhile; what was dropped is
// reported in the COMMIT's notice. If a redo step still fails (an I/O
// error), COMMIT reports success with a WARNING notice and the journal rows
// stay; the step is retried at every start until it succeeds.

const ddlKind = "ddltx"

type ddlStep struct {
	Op       string               `json:"op"`
	Table    string               `json:"table,omitempty"`
	Names    []string             `json:"names,omitempty"`
	Before   []string             `json:"before,omitempty"`
	ID       uint32               `json:"id,omitempty"`
	Rel      uint32               `json:"rel,omitempty"` // CREATE TABLE: the RelID given to the table before it exists
	Cascade  bool                 `json:"cascade,omitempty"`
	Col      *schema.Column       `json:"col,omitempty"`
	Idx      []schema.IndexDef    `json:"idx,omitempty"`
	Views    []viewSnap           `json:"views,omitempty"`
	Seqs     []seqSnap            `json:"seqs,omitempty"`
	Objs     []objSnap            `json:"objs,omitempty"`
	Trigs    []trigSnap           `json:"trigs,omitempty"`
	Cols     []schema.Column      `json:"cols,omitempty"`
	Key      string               `json:"key,omitempty"`
	KeyCols  []string             `json:"key_cols,omitempty"`
	Substr   []schema.SubstrIndex `json:"substr,omitempty"`
	Gin      []schema.GinIndex    `json:"gin,omitempty"`
	Flag     bool                 `json:"flag,omitempty"`
	RoleSnap *roleSnap            `json:"role_snap,omitempty"`
	CDC      *cdcSnap             `json:"cdc,omitempty"`
}

type ddlJournal struct {
	id    string
	n     int             // counter for private names
	views map[string]bool // views this transaction created, replaced or dropped: read live, not at its snapshot
	undo  []ddlStep
	redo  []ddlStep
}

type ddlData struct {
	Undo []ddlStep `json:"undo,omitempty"`
	Redo []ddlStep `json:"redo,omitempty"`
}

// viewAt reads a view for the compiler: a transaction sees the view
// definitions of its snapshot (TestSQLViewSnapshot), except those its own
// DDL changed, which are applied at once and read live.
func (s *Session) viewAt(tx *core.Tx, name string) (*core.View, bool, error) {
	if s.dj != nil && s.dj.views[name] {
		tx = nil
	}
	return s.E.View(tx, name)
}

// djMark is a position in the journal (a savepoint remembers one).
type djMark struct{ undo, redo int }

func (s *Session) djMark() djMark {
	if s.dj == nil {
		return djMark{}
	}
	return djMark{len(s.dj.undo), len(s.dj.redo)}
}

// test hooks
var (
	// returns true to stop the COMMIT after the transaction is durable and
	// before the redo steps run (a crash at that point)
	ddlCrashAfterCommit func() bool
	// returns an error to make a redo step fail
	ddlRedoFault func(ddlStep) error
	// runs right before the transaction commits
	ddlBeforeCommit func()
)

func (s *Session) djPersist() error {
	b, _ := json.Marshal(ddlData{Undo: s.dj.undo})
	return s.E.PutObj(core.Obj{Kind: ddlKind, Name: "o-" + s.dj.id, Data: string(b)}, true)
}

func (s *Session) djStart() error {
	if s.dj != nil {
		return nil
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	s.dj = &ddlJournal{id: hex.EncodeToString(r[:])}
	return s.djPersist()
}

// djUndo journals an undo step before the statement it undoes runs.
func (s *Session) djUndo(st ddlStep) (int, error) {
	if err := s.djStart(); err != nil {
		return 0, err
	}
	s.dj.undo = append(s.dj.undo, st)
	return len(s.dj.undo) - 1, s.djPersist()
}

func (s *Session) djRedo(st ddlStep) error {
	if err := s.djStart(); err != nil {
		return err
	}
	s.dj.redo = append(s.dj.redo, st)
	return nil
}

// hiddenName returns a fresh private name for a hidden object.
func (s *Session) hiddenName(kind string) string {
	s.dj.n++
	return fmt.Sprintf("__txd_%s_%d_%s", s.dj.id[:8], s.dj.n, kind)
}

func colNames(ts *core.TableState) []string {
	var out []string
	for _, c := range ts.Schema().Columns {
		out = append(out, c.Name)
	}
	return out
}

func indexNames(ts *core.TableState) []string {
	var out []string
	for _, ix := range ts.Schema().Indexes {
		out = append(out, ix.Name)
	}
	return out
}

// constraintNames lists the constraints of a table with a kind prefix.
func constraintNames(t *schema.Table) []string {
	var out []string
	for _, c := range t.Checks {
		out = append(out, "ck:"+c.Name)
	}
	for _, f := range t.ForeignKeys {
		out = append(out, "fk:"+f.Name)
	}
	for _, ix := range t.Indexes {
		out = append(out, "ix:"+ix.Name)
	}
	return out
}

// txDDL intercepts the DDL statements of an explicit transaction.
func (s *Session) txDDL(st Stmt, params []Datum) (*Result, bool, error) {
	run := func() (*Result, error) {
		s.djBypass = true
		defer func() { s.djBypass = false }()
		return s.exec(st, params)
	}
	fail := func(err error) (*Result, bool, error) {
		s.markFailed()
		return nil, true, AsError(err)
	}
	switch st := st.(type) {
	case *CreateTable:
		if st.Ext != nil && st.Ext.Temp {
			return nil, false, nil
		}
		return s.djCreated(st.Name, run)
	case *CreateAs:
		if st.Temp {
			return nil, false, nil
		}
		return s.djCreated(st.Name, run)
	case *CreateIndex:
		tbl := st.Table
		if real, temp := s.tempReal(st.Table); temp { // a TEMP table is journaled under its engine name
			tbl = real
		}
		ts, err := s.tableForDDL(tbl)
		if err != nil {
			return nil, false, nil
		}
		if err := s.djDropBeforeCreate(st.Name); err != nil {
			return fail(err)
		}
		if _, err := s.djUndo(ddlStep{Op: "drop_indexes", Table: ts.Schema().Name, Before: indexNames(ts)}); err != nil {
			return fail(err)
		}
		res, err := run()
		return res, true, err
	case *AlterTable:
		return s.djAlter(st)
	case *CreateSubstrIndex:
		if _, temp := s.tempReal(st.Table); temp {
			return nil, false, nil
		}
		ts, err := s.E.UserTable(st.Table)
		if err != nil {
			return nil, false, nil
		}
		d := ts.Schema()
		return s.djRun(ddlStep{Op: "restore_substr", Table: d.Name, Substr: slices.Clone(d.Substr), Gin: slices.Clone(d.Gin)}, run)
	case *DropIndex:
		return s.djDropIndex(st.Names)
	case *DropSubstrIndex:
		return s.djDropIndex(st.Names)
	case *DropTable:
		return s.djDropTable(st)
	case *DropMat:
		for _, n := range st.Names {
			if _, err := s.matTable(n); err != nil {
				return nil, false, nil // missing or not a materialized view: the normal path reports it
			}
		}
		res, handled, err := s.djDropTable(&DropTable{Names: st.Names, IfExists: st.IfExists, Cascade: st.Cascade, Mat: true})
		if res != nil {
			res.Tag = "DROP MATERIALIZED VIEW"
		}
		return res, handled, err
	case *Truncate:
		return s.djTruncate(st, run)
	case *CreateView, *DropView:
		return s.djViews(st, run)
	case *CreateSequence, *DropSequence, *AlterSequence:
		return s.djSeqs(st, run)
	case *ObjDDL, *CommentOn:
		return s.djObjs(st, run)
	case *CreateTrigger, *DropTrigger:
		return s.djTriggers(st, run)
	case *CreatePartition:
		return s.djPartition(st, run)
	case *CreateFunc:
		return s.djFuncs(st.Def.Name, false, run)
	case *DropFunc:
		return s.djFuncs(st.Name, st.Cascade, run)
	case *RenameIndex:
		return s.djRenameIndex(st, run)
	case *CreateRole, *AlterRole, *DropRole, *Grant:
		return s.djRoles(run)
	case *CreatePublication, *DropPublication, *AlterReplicaIdentity:
		return s.djCDC(run)
	}
	return nil, false, nil
}

// substrNamed reports whether name is a trigram or GIN index.
func (s *Session) substrNamed(name string) bool {
	for _, ts := range s.E.UserTables() {
		t := ts.Schema()
		for _, x := range t.Substr {
			if x.Name == name {
				return true
			}
		}
		for _, x := range t.Gin {
			if x.Name == name {
				return true
			}
		}
	}
	return false
}

func (s *Session) djDropIndex(names []string) (*Result, bool, error) {
	for _, n := range names {
		if !s.indexDroppable(n) && !s.substrNamed(n) {
			return nil, false, nil // missing or constraint-backed: the normal path handles it
		}
	}
	for _, n := range names {
		op := "drop_index"
		if s.substrNamed(n) {
			op = "drop_substr"
		}
		if err := s.djRedo(ddlStep{Op: op, Names: []string{n}}); err != nil {
			s.markFailed()
			return nil, true, AsError(err)
		}
	}
	return &Result{Tag: "DROP INDEX"}, true, nil
}

// djDropBeforeCreate handles DROP INDEX x; CREATE INDEX x in one transaction
// (Liquibase and Jmix changelogs do this to change an index): the deferred
// drop is applied now, so the name is free, and its undo step rebuilds the
// old index on ROLLBACK. The pending redo step is removed, or COMMIT would
// drop the new index.
func (s *Session) djDropBeforeCreate(name string) error {
	if s.dj == nil || name == "" {
		return nil
	}
	at := slices.IndexFunc(s.dj.redo, func(r ddlStep) bool { return r.Op == "drop_index" && slices.Contains(r.Names, name) })
	if at < 0 {
		return nil
	}
	s.dj.redo = slices.Delete(s.dj.redo, at, at+1)
	for _, ts := range s.E.UserTables() {
		ix := ts.Schema().Index(name)
		if ix == nil {
			continue
		}
		if _, err := s.djUndo(ddlStep{Op: "create_index", Table: ts.Schema().Name, Idx: []schema.IndexDef{*ix}}); err != nil {
			return err
		}
		return s.E.DropIndex(ts.Schema().Name, name)
	}
	return nil
}

func (s *Session) indexDroppable(name string) bool {
	for _, ts := range s.E.UserTables() {
		if ix := ts.Schema().Index(name); ix != nil {
			return ix.Constraint == "" && !ix.Internal
		}
	}
	return false
}

// newRelMarker returns an unused RelID to recognize a table by before it
// exists (see djCreated).
func (s *Session) newRelMarker() uint32 {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		v := binary.BigEndian.Uint32(b[:])&0x3fffffff | 0x40000000
		taken := false
		for _, ts := range s.E.UserTables() {
			if ts.Schema().OID() == v {
				taken = true
				break
			}
		}
		if !taken {
			return v
		}
	}
}

// djCreated runs a statement that creates the table name, with an undo
// step. The undo step is written before the table exists and the table's
// ID is not known until then, so the table is given a RelID chosen here and
// journaled with the step: the undo drops only a table that carries it, so
// a crash between the journal write and the CREATE, or a name taken by
// another session meanwhile, cannot make the undo drop someone else's table.
func (s *Session) djCreated(name string, run func() (*Result, error)) (*Result, bool, error) {
	if _, err := s.E.Table(name); err == nil || s.isView(name) {
		return nil, false, nil // exists: the statement's own error or IF NOT EXISTS
	}
	rel := s.newRelMarker()
	i, err := s.djUndo(ddlStep{Op: "drop_table", Table: name, Rel: rel})
	if err != nil {
		s.markFailed()
		return nil, true, AsError(err)
	}
	s.djRel = rel
	res, err := run()
	s.djRel = 0
	if err == nil {
		if ts, e2 := s.E.Table(name); e2 == nil && ts.Schema().OID() == rel {
			s.dj.undo[i].ID = ts.Schema().ID
			_ = s.djPersist()
		}
	}
	return res, true, err
}

func (s *Session) djFail(err error) (*Result, bool, error) {
	s.markFailed()
	return nil, true, AsError(err)
}

// djAlter journals an ALTER TABLE one action at a time (PostgreSQL runs the
// actions as one statement; inside a transaction a failing action aborts
// the transaction anyway, and the journal undoes the actions already done,
// also after ROLLBACK TO SAVEPOINT).
func (s *Session) djAlter(st *AlterTable) (*Result, bool, error) {
	if real, temp := s.tempReal(st.Name); temp {
		// a TEMP table is journaled under its engine name; renames change the session's
		// name map, which the journal does not hold
		for _, a := range st.Actions {
			if a.Kind == "rename_table" {
				return nil, false, nil
			}
		}
		cp := *st
		cp.Name = real
		st = &cp
	}
	if _, err := s.tableForDDL(st.Name); err != nil {
		return nil, false, nil
	}
	journaled := false
	for _, a := range st.Actions {
		switch a.Kind {
		case "add_column", "drop_column", "rename_column", "rename_table", "set_type", "set_default", "drop_default",
			"set_not_null", "drop_not_null", "add_check", "drop_constraint":
			journaled = true
		case "add_constraint":
			journaled = journaled || a.Constraint != nil && (a.Constraint.Kind == "unique" || a.Constraint.Kind == "foreign")
		}
		if a.Kind == "add_pk" {
			// the table has no rows (ADD PRIMARY KEY refuses otherwise); the
			// rows this transaction wrote were encoded with the old key
			if ts, err := s.tableForDDL(st.Name); err == nil && s.tx != nil && s.tx.HasWrites(ts.Schema().Name) {
				return s.djFail(errf(codeFeature, "ADD PRIMARY KEY on table %q after writing rows to it in the same transaction is not supported", st.Name))
			}
			journaled = true
		}
		if a.Kind == "add_constraint" && (a.Constraint == nil || a.Constraint.Kind != "unique" && a.Constraint.Kind != "foreign") {
			return nil, false, nil
		}
	}
	if !journaled {
		return nil, false, nil
	}
	res := &Result{Tag: "ALTER TABLE"}
	name := st.Name
	one := func(a AlterAction) error {
		s.djBypass = true
		defer func() { s.djBypass = false }()
		r, err := s.exec(&AlterTable{Name: name, IfExists: st.IfExists, Actions: []AlterAction{a}}, nil)
		if err != nil {
			return err
		}
		if r != nil && r.Notice != "" {
			res.Notice = r.Notice
		}
		return nil
	}
	for _, a := range st.Actions {
		ts, err := s.tableForDDL(name)
		if err != nil {
			return s.djFail(errf(codeUndefinedTable, "relation %q does not exist", name))
		}
		t := ts.Schema()
		var undo *ddlStep
		switch a.Kind {
		case "add_column":
			undo = &ddlStep{Op: "drop_columns", Table: t.Name, Before: colNames(ts)}
		case "rename_column":
			undo = &ddlStep{Op: "rename_column", Table: t.Name, Names: []string{a.NewName, a.Column}}
		case "rename_table":
			undo = &ddlStep{Op: "rename_table", Table: a.NewName, Names: []string{t.Name}}
		case "set_type", "set_default", "drop_default", "set_not_null", "drop_not_null":
			if i := colIndex(t, a.Column); i >= 0 {
				c := t.Columns[i]
				undo = &ddlStep{Op: "restore_col", Table: t.Name, Col: &c}
			}
		case "add_pk":
			var owned []string
			for _, q := range s.tableSequences(t.Name) {
				owned = append(owned, q.Name)
			}
			undo = &ddlStep{Op: "restore_key", Table: t.Name, Cols: slices.Clone(t.Columns), Key: t.Key, KeyCols: slices.Clone(t.KeyCols),
				Seqs: s.seqSnaps(owned)} // the hidden row id column is dropped with its sequence
		case "add_check", "add_constraint":
			undo = &ddlStep{Op: "drop_constraints", Table: t.Name, Before: constraintNames(t)}
		case "drop_column":
			if r, err := s.djDropColumn(t, a, &name); err != nil {
				return s.djFail(err)
			} else if r != nil {
				res.Notice = r.Notice
			}
			continue
		case "drop_constraint":
			if err := s.djDropConstraint(t, a); err != nil {
				return s.djFail(err)
			}
			continue
		}
		if undo != nil {
			if _, err := s.djUndo(*undo); err != nil {
				return s.djFail(err)
			}
		}
		if err := one(a); err != nil {
			s.markFailed()
			return nil, true, err
		}
		if a.Kind == "rename_table" {
			name = a.NewName
		}
	}
	return res, true, nil
}

// dropColumnCheck validates DROP COLUMN the way ALTER TABLE does when it
// runs, so a deferred or hidden drop cannot fail later.
func (s *Session) dropColumnCheck(t *schema.Table, col string) error {
	i := colIndex(t, col)
	switch {
	case i < 0:
		return errf(codeUndefinedColumn, "column %q of relation %q does not exist", col, t.Name)
	case col == t.Key || slices.Contains(t.KeyCols, col):
		return errf(codeFeature, "cannot drop column %q: it is part of the primary key of %q, which every STASK table needs", col, t.Name)
	case slices.ContainsFunc(t.Substr, func(x schema.SubstrIndex) bool { return x.Col == col }):
		return errf(codeFeature, "cannot drop column %q: a trigram index covers it (DROP INDEX first)", col)
	case slices.Contains(t.Search, col):
		return errf(codeFeature, "cannot drop column %q: the table's search view indexes it", col)
	case t.TimeColumn != "":
		return errf(codeFeature, "DROP COLUMN is not supported yet on a time-series table")
	}
	for _, r := range s.E.FKRefs(t.Name) {
		if r.Child != t.Name && slices.Contains(r.FK.RefCols, col) {
			return errf(codeDependentObjects, "cannot drop column %s of table %s because constraint %s on table %s depends on it", col, t.Name, r.FK.Name, r.Child)
		}
	}
	return s.alterTableViews(&AlterTable{Name: t.Name, Actions: []AlterAction{{Kind: "drop_column", Column: col}}})
}

// djDropColumn hides (rename) or defers one DROP COLUMN.
func (s *Session) djDropColumn(t *schema.Table, a AlterAction, name *string) (*Result, error) {
	if colIndex(t, a.Column) < 0 && a.IfExists {
		return &Result{Tag: "ALTER TABLE", Notice: fmt.Sprintf("column %q of relation %q does not exist, skipping", a.Column, t.Name)}, nil
	}
	if err := s.dropColumnCheck(t, a.Column); err != nil {
		return nil, err
	}
	if err := s.djStart(); err != nil {
		return nil, err
	}
	// a CHECK on the column goes with it (a rename would refuse)
	hide := true
	for _, ck := range t.Checks {
		if x, err := parseCheck(ck.Expr); err == nil && refersToCol(x, a.Column) {
			hide = false
		}
	}
	if hide && !s.tx.HasWrites(t.Name) {
		hid := s.hiddenName(a.Column)
		if _, err := s.djUndo(ddlStep{Op: "rename_column", Table: t.Name, Names: []string{hid, a.Column}}); err != nil {
			return nil, err
		}
		s.djBypass = true
		_, err := s.exec(&AlterTable{Name: t.Name, Actions: []AlterAction{{Kind: "rename_column", Column: a.Column, NewName: hid}}}, nil)
		s.djBypass = false
		if err != nil {
			return nil, err
		}
		return nil, s.djRedo(ddlStep{Op: "drop_column", Table: t.Name, Names: []string{hid}})
	}
	return nil, s.djRedo(ddlStep{Op: "drop_column", Table: t.Name, Names: []string{a.Column}})
}

// djDropConstraint defers DROP CONSTRAINT (it may drop an index or a check,
// and dependent foreign keys must be refused now).
func (s *Session) djDropConstraint(t *schema.Table, a AlterAction) error {
	found := slices.ContainsFunc(t.Checks, func(c schema.Check) bool { return c.Name == a.Column }) ||
		slices.ContainsFunc(t.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == a.Column }) ||
		slices.ContainsFunc(t.Indexes, func(ix schema.IndexDef) bool { return ix.Constraint == a.Column })
	if !found {
		if a.IfExists {
			return nil
		}
		return errf("42704", "constraint %q of relation %q does not exist", a.Column, t.Name)
	}
	for _, ix := range t.Indexes {
		if ix.Constraint != a.Column {
			continue
		}
		for _, ref := range s.E.FKRefs(t.Name) {
			if slices.Equal(ref.FK.RefCols, ix.Columns) {
				return errf(codeDependentObjects, "cannot drop constraint %s on table %s because other objects depend on it: constraint %s on table %s depends on index %s",
					a.Column, t.Name, ref.FK.Name, ref.Child, ix.Name)
			}
		}
	}
	return s.djRedo(ddlStep{Op: "drop_constraint", Table: t.Name, Names: []string{a.Column}})
}

// hideable: DROP TABLE can rename the table out of the way.
func (s *Session) djHideable(n string, set map[string]bool) bool {
	ts, err := s.E.UserTable(n)
	if err != nil || ts.Schema().MatView != "" || s.tx.HasWrites(n) || s.E.Triggers(n) != nil {
		return false // rows written by this transaction would conflict with the rename at COMMIT
	}
	for _, r := range s.E.FKRefs(n) {
		if r.Child != n {
			return false
		}
	}
	deps, _ := s.viewDependents(map[string]bool{n: true})
	return len(deps) == 0
}

func (s *Session) djDropTable(st *DropTable) (*Result, bool, error) {
	if len(st.Names) == 0 {
		return nil, false, nil
	}
	set := map[string]bool{}
	var names []string
	res := &Result{Tag: "DROP TABLE"}
	for _, n := range st.Names {
		if isTempName(s.mapTemp(n)) {
			return nil, false, nil
		}
		ts, err := s.E.UserTable(n)
		if err != nil {
			if s.isView(n) || !st.IfExists {
				return nil, false, nil // the normal path reports it
			}
			res.Notice = fmt.Sprintf("table %q does not exist, skipping", n)
			continue
		}
		if (ts.Schema().MatView != "") != st.Mat {
			return nil, false, nil // the normal path reports the wrong kind
		}
		if !set[n] {
			set[n] = true
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return res, true, nil
	}
	for _, n := range names {
		for _, r := range s.E.FKRefs(n) {
			if r.Child != n && !set[r.Child] && !st.Cascade {
				return s.djFail(errf(codeDependentObjects, "cannot drop table %s because other objects depend on it: constraint %s on table %s depends on table %s", n, r.FK.Name, r.Child, n))
			}
		}
	}
	if _, _, err := s.checkDependents(set, "table", st.Cascade); err != nil {
		return s.djFail(err)
	}
	var deferred []string
	for _, n := range names {
		if len(names) == 1 && s.djHideable(n, set) {
			if err := s.djHideTable(n); err != nil {
				return s.djFail(err)
			}
			continue
		}
		deferred = append(deferred, n)
	}
	if len(deferred) > 0 {
		if err := s.djRedo(ddlStep{Op: "drop_tables", Names: deferred, Cascade: true, Flag: st.Mat}); err != nil {
			return s.djFail(err)
		}
	}
	return res, true, nil
}

// djHideTable renames a table (and its owned sequences) to private names;
// the redo step drops it at COMMIT.
func (s *Session) djHideTable(n string) error {
	if err := s.djStart(); err != nil {
		return err
	}
	hid := s.hiddenName("t")
	var pairs []string
	for _, q := range s.tableSequences(n) {
		pairs = append(pairs, q.Name, s.hiddenName("s"))
	}
	// index names are global, so the indexes go (a new table of this name
	// may want them); unhide builds them again
	ts, err := s.E.UserTable(n)
	if err != nil {
		return err
	}
	idx := slices.Clone(ts.Schema().Indexes)
	if _, err := s.djUndo(ddlStep{Op: "unhide_table", Table: hid, Names: []string{n}, Before: pairs, Idx: idx}); err != nil {
		return err
	}
	for _, ix := range idx {
		if err := s.E.DropIndex(n, ix.Name); err != nil {
			return err
		}
	}
	s.djBypass = true
	_, err = s.exec(&AlterTable{Name: n, Actions: []AlterAction{{Kind: "rename_table", NewName: hid}}}, nil)
	s.djBypass = false
	if err != nil {
		return err
	}
	for i := 0; i < len(pairs); i += 2 {
		if err := s.renameSeq(pairs[i], pairs[i+1]); err != nil {
			return err
		}
	}
	return s.djRedo(ddlStep{Op: "drop_tables", Names: []string{hid}, Cascade: true})
}

// renameSeq renames a sequence, keeping its state (idempotent).
func (s *Session) renameSeq(from, to string) error {
	var def *core.Sequence
	for _, q := range s.E.Sequences() {
		if q.Name == from {
			q := q
			def = &q
		}
	}
	if def == nil {
		return nil
	}
	last, called, err := s.E.SequenceState(from)
	if err != nil {
		return err
	}
	if _, _, err := s.E.SequenceState(to); err == nil {
		if err := s.E.DropSequence(to); err != nil { // left by an interrupted rename
			return err
		}
	}
	d := *def
	d.Name = to
	if err := s.E.CreateSequence(d); err != nil {
		return err
	}
	if err := s.E.SetVal(to, last, called); err != nil {
		return err
	}
	return s.E.DropSequence(from)
}

func (s *Session) stepExec(stmt Stmt) error {
	prev := s.djBypass
	s.djBypass = true
	defer func() { s.djBypass = prev }()
	_, err := s.exec(stmt, nil)
	return err
}

// ddlStepRun applies one undo or redo step (idempotent).
func (s *Session) ddlStepRun(st ddlStep) (notice string, err error) {
	var stmt Stmt
	switch st.Op {
	case "drop_table":
		ts, err := s.E.Table(st.Table)
		if err != nil || st.ID != 0 && ts.Schema().ID != st.ID || st.Rel != 0 && ts.Schema().OID() != st.Rel {
			return "", nil
		}
		stmt = &DropTable{Names: []string{st.Table}, IfExists: true, Cascade: true, Mat: ts.Schema().MatView != ""}
	case "drop_tables":
		stmt = &DropTable{Names: st.Names, IfExists: true, Cascade: st.Cascade, Mat: st.Flag}
	case "unhide_table":
		if _, err := s.E.Table(st.Table); err == nil {
			if err := s.stepExec(&AlterTable{Name: st.Table, Actions: []AlterAction{{Kind: "rename_table", NewName: st.Names[0]}}}); err != nil {
				return "", err
			}
		}
		for i := 0; i+1 < len(st.Before); i += 2 {
			if err := s.renameSeq(st.Before[i+1], st.Before[i]); err != nil {
				return "", err
			}
		}
		if ts, err := s.E.Table(st.Names[0]); err == nil {
			for _, ix := range st.Idx {
				if ts.Schema().Index(ix.Name) != nil {
					continue
				}
				if err := s.E.CreateIndex(st.Names[0], ix); err != nil {
					return "", err
				}
			}
		}
		return "", nil
	case "rename_column": // Names: [current name, name to restore]
		ts, err := s.E.Table(st.Table)
		if err != nil || colIndex(ts.Schema(), st.Names[0]) < 0 || colIndex(ts.Schema(), st.Names[1]) >= 0 {
			return "", nil
		}
		stmt = &AlterTable{Name: st.Table, Actions: []AlterAction{{Kind: "rename_column", Column: st.Names[0], NewName: st.Names[1]}}}
	case "rename_table": // Table: current name, Names[0]: name to restore
		if _, err := s.E.Table(st.Table); err != nil || s.relationExists(st.Names[0]) {
			return "", nil
		}
		stmt = &AlterTable{Name: st.Table, Actions: []AlterAction{{Kind: "rename_table", NewName: st.Names[0]}}}
	case "restore_col":
		ts, err := s.E.Table(st.Table)
		if err != nil || st.Col == nil {
			return "", nil
		}
		old := *st.Col
		i := slices.IndexFunc(ts.Schema().Columns, func(c schema.Column) bool { return c.Num == old.Num })
		if i < 0 {
			return "", nil
		}
		return "", asErr(s.E.AlterTable(st.Table, func(d *schema.Table) error {
			if j := slices.IndexFunc(d.Columns, func(c schema.Column) bool { return c.Num == old.Num }); j >= 0 {
				d.Columns[j] = old
			}
			return nil
		}))
	case "drop_indexes":
		ts, err := s.E.Table(st.Table)
		if err != nil {
			return "", nil
		}
		var names []string
		for _, ix := range ts.Schema().Indexes {
			if !slices.Contains(st.Before, ix.Name) && ix.Constraint == "" && !ix.Internal {
				names = append(names, ix.Name)
			}
		}
		if len(names) == 0 {
			return "", nil
		}
		if isTempName(st.Table) { // DROP INDEX does not reach a TEMP table (a system table)
			for _, n := range names {
				if err := s.E.DropIndex(st.Table, n); err != nil {
					return "", err
				}
			}
			return "", nil
		}
		stmt = &DropIndex{Names: names, IfExists: true}
	case "drop_index":
		stmt = &DropIndex{Names: st.Names, IfExists: true}
	case "create_index":
		ts, err := s.E.Table(st.Table)
		if err != nil || len(st.Idx) != 1 || ts.Schema().Index(st.Idx[0].Name) != nil {
			return "", nil
		}
		return "", s.E.CreateIndex(st.Table, st.Idx[0])
	case "drop_constraints":
		return "", s.undoConstraints(st)
	case "drop_constraint":
		ts, err := s.E.Table(st.Table)
		if err != nil {
			return "", nil
		}
		t := ts.Schema()
		if !slices.Contains(constraintNames(t), "ck:"+st.Names[0]) && !slices.Contains(constraintNames(t), "fk:"+st.Names[0]) &&
			!slices.ContainsFunc(t.Indexes, func(ix schema.IndexDef) bool { return ix.Constraint == st.Names[0] }) {
			return "", nil
		}
		stmt = &AlterTable{Name: st.Table, Actions: []AlterAction{{Kind: "drop_constraint", Column: st.Names[0], IfExists: true}}}
	case "drop_columns", "drop_column":
		ts, err := s.E.Table(st.Table)
		if err != nil {
			return "", nil
		}
		cols := st.Names
		if st.Op == "drop_columns" {
			cols = nil
			for _, c := range colNames(ts) {
				if !slices.Contains(st.Before, c) {
					cols = append(cols, c)
				}
			}
		}
		var acts []AlterAction
		for _, c := range cols {
			if colIndex(ts.Schema(), c) >= 0 {
				acts = append(acts, AlterAction{Kind: "drop_column", Column: c, IfExists: true})
			}
		}
		if len(acts) == 0 {
			return "", nil
		}
		stmt = &AlterTable{Name: st.Table, IfExists: true, Actions: acts}
	case "restore_views":
		if err := s.restoreViews(st.Views); err != nil {
			return "", err
		}
		return "", s.restoreObjs(st.Objs)
	case "restore_seqs":
		if err := s.restoreSeqs(st.Seqs); err != nil {
			return "", err
		}
		return "", s.restoreObjs(st.Objs)
	case "restore_objs":
		return "", s.restoreObjs(st.Objs)
	case "rename_index", "rename_type", "rename_enum_value":
		var done bool
		var err error
		if stmt, done, err = s.moreStepRun(st); done || err != nil {
			return "", err
		}
	case "restore_trigs":
		return "", s.restoreTrigs(st.Trigs)
	case "restore_key":
		ts, err := s.E.Table(st.Table)
		if err != nil || len(st.Cols) == 0 || ts.Schema().Key == st.Key && slices.Equal(ts.Schema().KeyCols, st.KeyCols) {
			return "", nil
		}
		if err := s.restoreSeqs(st.Seqs); err != nil { // before: the columns default to them
			return "", err
		}
		return "", asErr(s.E.AlterTable(st.Table, func(d *schema.Table) error {
			d.Columns = slices.Clone(st.Cols)
			d.Key, d.KeyCols = st.Key, slices.Clone(st.KeyCols)
			d.Dropped = slices.DeleteFunc(d.Dropped, func(c schema.Column) bool {
				return slices.ContainsFunc(st.Cols, func(x schema.Column) bool { return x.Num == c.Num })
			})
			return nil
		}))
	case "mat_unpopulated":
		if _, err := s.E.Table(st.Table); err != nil {
			return "", nil
		}
		return "", asErr(s.E.AlterTable(st.Table, func(d *schema.Table) error { d.MatUnpopulated = st.Flag; return nil }))
	case "restore_substr":
		if _, err := s.E.Table(st.Table); err != nil {
			return "", nil
		}
		return "", asErr(s.E.AlterTable(st.Table, func(d *schema.Table) error {
			d.Substr, d.Gin = slices.Clone(st.Substr), slices.Clone(st.Gin)
			return nil
		}))
	case "drop_substr":
		stmt = &DropSubstrIndex{Names: st.Names, IfExists: true}
	case "restore_roles":
		if st.RoleSnap == nil {
			return "", nil
		}
		return "", s.E.RestoreRoles(st.RoleSnap.Roles)
	case "restore_cdc":
		if st.CDC == nil {
			return "", nil
		}
		return "", s.E.CDCRestore(st.CDC.Pubs, st.CDC.Ident)
	default:
		return "", fmt.Errorf("unknown ddl journal step %q", st.Op)
	}
	if ddlRedoFault != nil {
		if err := ddlRedoFault(st); err != nil {
			return "", err
		}
	}
	prev := s.djBypass
	s.djBypass = true
	defer func() { s.djBypass = prev }()
	res, err := s.exec(stmt, nil)
	if res != nil {
		notice = res.Notice
	}
	return notice, err
}

// undoConstraints drops the constraints a table gained since the step was
// journaled.
func (s *Session) undoConstraints(st ddlStep) error {
	ts, err := s.E.Table(st.Table)
	if err != nil {
		return nil
	}
	t := ts.Schema()
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, f := range t.ForeignKeys {
		if !slices.Contains(st.Before, "fk:"+f.Name) {
			_, err := s.dropConstraint(t.Name, f.Name)
			note(err)
		}
	}
	for _, c := range t.Checks {
		if !slices.Contains(st.Before, "ck:"+c.Name) {
			note(s.stepExec(&AlterTable{Name: t.Name, Actions: []AlterAction{{Kind: "drop_constraint", Column: c.Name, IfExists: true}}}))
		}
	}
	if ts2, err := s.E.Table(st.Table); err == nil {
		for _, ix := range ts2.Schema().Indexes {
			switch {
			case slices.Contains(st.Before, "ix:"+ix.Name) || ix.Internal:
			case ix.Constraint != "":
				_, err := s.dropConstraint(t.Name, ix.Constraint)
				note(err)
			default:
				note(asErr(s.E.DropIndex(t.Name, ix.Name)))
			}
		}
	}
	return firstErr
}

// ddlSteps runs steps (in reverse for undo) and returns the first error and
// the notices of the steps.
func (s *Session) ddlSteps(steps []ddlStep, rev bool) (notes []string, firstErr error) {
	for i := range steps {
		st := steps[i]
		if rev {
			st = steps[len(steps)-1-i]
		}
		note, err := s.ddlStepRun(st)
		if note != "" && strings.Contains(note, "cascades") {
			notes = append(notes, note)
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s %s: %w", st.Op, st.Table+strings.Join(st.Names, ","), err)
		}
	}
	return notes, firstErr
}

// djRollbackTo undoes the DDL done since a savepoint (ROLLBACK TO).
func (s *Session) djRollbackTo(m djMark) error {
	j := s.dj
	if j == nil || len(j.undo) <= m.undo && len(j.redo) <= m.redo {
		return nil
	}
	if len(j.redo) > m.redo {
		j.redo = j.redo[:m.redo]
	}
	if len(j.undo) <= m.undo {
		return nil
	}
	steps := slices.Clone(j.undo[m.undo:])
	prev := s.djBypass
	s.djBypass = true
	_, err := s.ddlSteps(steps, true)
	s.djBypass = prev
	if err != nil {
		return err // the steps stay journaled: the final ROLLBACK retries them
	}
	j.undo = j.undo[:m.undo]
	return s.djPersist()
}

// ddlUndo runs after the transaction ended without committing.
func (s *Session) ddlUndo() {
	j := s.dj
	if j == nil {
		return
	}
	s.dj = nil
	if _, err := s.ddlSteps(j.undo, true); err != nil {
		s.E.Logf("ddl journal: undo left %s for the next start: %v", j.id, err)
		return
	}
	_ = s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: ddlKind, Name: "o-" + j.id}, Delete: true}})
}

// ddlMark writes the commit marker, with the redo steps, in tx itself.
func (s *Session) ddlMark(tx *core.Tx) error {
	if s.dj == nil {
		return nil
	}
	b, _ := json.Marshal(ddlData{Redo: s.dj.redo})
	return tx.PutObj(core.Obj{Kind: ddlKind, Name: "c-" + s.dj.id, Data: string(b)})
}

// ddlAfterCommit runs after the transaction committed; it returns the
// notice for the COMMIT's result.
func (s *Session) ddlAfterCommit() string {
	j := s.dj
	if j == nil {
		return ""
	}
	s.dj = nil
	if ddlCrashAfterCommit != nil && ddlCrashAfterCommit() {
		return ""
	}
	var notes []string
	var err error
	for try := 0; try < 3; try++ {
		notes, err = s.ddlSteps(j.redo, false)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(try+1) * 20 * time.Millisecond)
	}
	if err != nil {
		s.E.Logf("ddl journal: deferred DDL of a committed transaction failed (retried at every start): %v", err)
		return fmt.Sprintf("WARNING: the transaction committed but its deferred DDL failed (%v); it is retried at the next start", err)
	}
	s.ddlForget(j.id)
	return strings.Join(notes, "; ")
}

func (s *Session) ddlForget(id string) {
	_ = s.E.WriteObjs([]core.ObjOp{
		{O: core.Obj{Kind: ddlKind, Name: "o-" + id}, Delete: true},
		{O: core.Obj{Kind: ddlKind, Name: "c-" + id}, Delete: true}})
}

var recovered sync.Map // *core.Engine -> *sync.Once

// recoverDDL finishes or undoes the DDL transactions a crash interrupted
// (once per engine; sessions of a replica do nothing). A journal whose
// steps fail stays for the next start.
func (s *Session) recoverDDL() {
	if s.E.ReadOnly() {
		return
	}
	o, _ := recovered.LoadOrStore(s.E, new(sync.Once))
	o.(*sync.Once).Do(func() {
		_ = s.E.EnsureObjs()
		objs := s.E.Objs(ddlKind)
		if len(objs) == 0 {
			return
		}
		byID := map[string]map[string]ddlData{}
		for _, ob := range objs {
			kind, id, _ := strings.Cut(ob.Name, "-")
			var d ddlData
			_ = json.Unmarshal([]byte(ob.Data), &d)
			if byID[id] == nil {
				byID[id] = map[string]ddlData{}
			}
			byID[id][kind] = d
		}
		for id, m := range byID {
			var err error
			if c, ok := m["c"]; ok {
				_, err = s.ddlSteps(c.Redo, false)
			} else {
				_, err = s.ddlSteps(m["o"].Undo, true)
			}
			if err != nil {
				s.E.Logf("ddl journal recovery %s: %v (kept, retried at the next start)", id, err)
				continue
			}
			s.ddlForget(id)
		}
	})
}
