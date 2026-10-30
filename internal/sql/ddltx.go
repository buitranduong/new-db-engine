package sql

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"stask/internal/core"
)

// Transactional DDL for the common schema changes (docs/05, docs/14 TXDDL).
//
// Inside BEGIN .. COMMIT/ROLLBACK a journal makes these statements behave
// atomically:
//
//   - CREATE TABLE (also CTAS), CREATE INDEX, ALTER TABLE .. ADD COLUMN are
//     applied at once (so the rest of the transaction sees them) and an UNDO
//     step is journaled first; ROLLBACK (or a failed COMMIT, or the session
//     ending, or a crash) runs the undo steps in reverse.
//   - DROP TABLE, DROP INDEX, ALTER TABLE .. DROP COLUMN destroy data, so
//     they are DEFERRED: validated now, journaled as REDO steps and applied
//     after COMMIT. The transaction still sees the object until then.
//
// The journal is two rows of the object catalog (kind "ddltx"): "o-<id>"
// holds the undo steps (rewritten, outside the transaction, before each DDL
// statement) and "c-<id>" holds the redo steps and is written INSIDE the
// user's transaction, so it is committed atomically with the transaction's
// own rows. Recovery (once per engine, at the first session): an "o-" row
// with a "c-" row means the transaction committed, so the redo steps are
// run again (they are idempotent); an "o-" row alone means it did not, so
// the undo steps run. No on-disk format changes: these are ordinary rows.
//
// Limits: other sessions see created objects before COMMIT (and still see
// dropped ones after a deferred DROP until COMMIT); a deferred drop that
// fails when applied (a dependent object was added meanwhile) is logged and
// retried once at the next start; ROLLBACK TO SAVEPOINT does not undo DDL;
// every other DDL statement (views, sequences, constraints, comments,
// RENAME, TRUNCATE, ...) is still applied at once and not rolled back.

const ddlKind = "ddltx"

type ddlStep struct {
	Op      string   `json:"op"`
	Table   string   `json:"table,omitempty"`
	Names   []string `json:"names,omitempty"`
	Before  []string `json:"before,omitempty"`
	ID      uint32   `json:"id,omitempty"`
	Cascade bool     `json:"cascade,omitempty"`
}

type ddlJournal struct {
	id   string
	undo []ddlStep
	redo []ddlStep
}

type ddlData struct {
	Undo []ddlStep `json:"undo,omitempty"`
	Redo []ddlStep `json:"redo,omitempty"`
}

// test hook: returns true to stop the COMMIT after the transaction is
// durable and before the redo steps run (a crash at that point).
var ddlCrashAfterCommit func() bool

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
		if _, temp := s.tempReal(st.Table); temp {
			return nil, false, nil
		}
		ts, err := s.E.UserTable(st.Table)
		if err != nil {
			return nil, false, nil
		}
		if _, err := s.djUndo(ddlStep{Op: "drop_indexes", Table: ts.Schema().Name, Before: indexNames(ts)}); err != nil {
			return fail(err)
		}
		res, err := run()
		return res, true, err
	case *AlterTable:
		return s.djAlter(st, run)
	case *DropIndex:
		return s.djDropIndex(st.Names)
	case *DropSubstrIndex:
		return s.djDropIndex(st.Names)
	case *DropTable:
		return s.djDropTable(st)
	}
	return nil, false, nil
}

func (s *Session) djDropIndex(names []string) (*Result, bool, error) {
	for _, n := range names {
		if !s.indexDroppable(n) {
			return nil, false, nil // missing, trigram or constraint-backed: the normal path handles it
		}
	}
	for _, n := range names {
		if err := s.djRedo(ddlStep{Op: "drop_index", Names: []string{n}}); err != nil {
			s.markFailed()
			return nil, true, AsError(err)
		}
	}
	return &Result{Tag: "DROP INDEX"}, true, nil
}

func (s *Session) indexDroppable(name string) bool {
	for _, ts := range s.E.UserTables() {
		if ix := ts.Schema().Index(name); ix != nil {
			return ix.Constraint == "" && !ix.Internal
		}
	}
	return false
}

// djCreated runs a statement that creates the table name, with an undo step.
func (s *Session) djCreated(name string, run func() (*Result, error)) (*Result, bool, error) {
	if _, err := s.E.Table(name); err == nil || s.isView(name) {
		return nil, false, nil // exists: the statement's own error or IF NOT EXISTS
	}
	i, err := s.djUndo(ddlStep{Op: "drop_table", Table: name})
	if err != nil {
		s.markFailed()
		return nil, true, AsError(err)
	}
	res, err := run()
	if err == nil {
		if ts, e2 := s.E.Table(name); e2 == nil {
			s.dj.undo[i].ID = ts.Schema().ID
			_ = s.djPersist()
		}
	}
	return res, true, err
}

func (s *Session) djAlter(st *AlterTable, run func() (*Result, error)) (*Result, bool, error) {
	if _, temp := s.tempReal(st.Name); temp {
		return nil, false, nil
	}
	ts, err := s.E.UserTable(st.Name)
	if err != nil {
		return nil, false, nil
	}
	var drops, rest []AlterAction
	adds := false
	for _, a := range st.Actions {
		switch a.Kind {
		case "drop_column":
			drops = append(drops, a)
		case "add_column":
			adds = true
			rest = append(rest, a)
		default:
			rest = append(rest, a)
		}
	}
	if len(drops) == 0 && !adds {
		return nil, false, nil
	}
	fail := func(err error) (*Result, bool, error) {
		s.markFailed()
		return nil, true, AsError(err)
	}
	// early validation of what is deferred
	key := ts.Schema()
	var defer_ []ddlStep
	for _, a := range drops {
		i := colIndex(key, a.Column)
		switch {
		case i < 0 && a.IfExists:
			continue
		case i < 0:
			return fail(errf(codeUndefinedColumn, "column %q of relation %q does not exist", a.Column, key.Name))
		case a.Column == key.Key || slices.Contains(key.KeyCols, a.Column):
			return fail(errf(codeFeature, "cannot drop column %q: it is part of the primary key of %q, which every STASK table needs", a.Column, key.Name))
		}
		defer_ = append(defer_, ddlStep{Op: "drop_column", Table: key.Name, Names: []string{a.Column}})
	}
	if len(drops) > 0 {
		if err := s.alterTableViews(&AlterTable{Name: st.Name, Actions: drops}); err != nil {
			return fail(err)
		}
	}
	res := &Result{Tag: "ALTER TABLE"}
	if len(rest) > 0 {
		if adds {
			if _, err := s.djUndo(ddlStep{Op: "drop_columns", Table: key.Name, Before: colNames(ts)}); err != nil {
				return fail(err)
			}
		}
		cp := *st
		cp.Actions = rest
		s.djBypass = true
		var err error
		res, err = s.exec(&cp, nil)
		s.djBypass = false
		if err != nil {
			return nil, true, err
		}
	}
	for _, d := range defer_ {
		if err := s.djRedo(d); err != nil {
			return fail(err)
		}
	}
	return res, true, nil
}

func (s *Session) djDropTable(st *DropTable) (*Result, bool, error) {
	if st.Mat || len(st.Names) == 0 {
		return nil, false, nil
	}
	set := map[string]bool{}
	for _, n := range st.Names {
		if isTempName(s.mapTemp(n)) {
			return nil, false, nil
		}
		ts, err := s.E.UserTable(n)
		if err != nil || ts.Schema().MatView != "" {
			return nil, false, nil // the normal path reports (or skips) it
		}
		set[n] = true
	}
	fail := func(err error) (*Result, bool, error) {
		s.markFailed()
		return nil, true, AsError(err)
	}
	for n := range set {
		for _, r := range s.E.FKRefs(n) {
			if r.Child != n && !set[r.Child] && !st.Cascade {
				return fail(errf(codeDependentObjects, "cannot drop table %s because other objects depend on it: constraint %s on table %s depends on table %s", n, r.FK.Name, r.Child, n))
			}
		}
	}
	if _, _, err := s.checkDependents(set, "table", st.Cascade); err != nil {
		return fail(err)
	}
	if err := s.djRedo(ddlStep{Op: "drop_tables", Names: slices.Clone(st.Names), Cascade: st.Cascade}); err != nil {
		return fail(err)
	}
	return &Result{Tag: "DROP TABLE"}, true, nil
}

// ddlStepRun applies one undo or redo step (idempotent).
func (s *Session) ddlStepRun(st ddlStep) error {
	var stmt Stmt
	switch st.Op {
	case "drop_table":
		ts, err := s.E.Table(st.Table)
		if err != nil || st.ID != 0 && ts.Schema().ID != st.ID {
			return nil
		}
		stmt = &DropTable{Names: []string{st.Table}, IfExists: true, Cascade: true, Mat: ts.Schema().MatView != ""}
	case "drop_tables":
		stmt = &DropTable{Names: st.Names, IfExists: true, Cascade: st.Cascade}
	case "drop_indexes":
		ts, err := s.E.Table(st.Table)
		if err != nil {
			return nil
		}
		var names []string
		for _, ix := range ts.Schema().Indexes {
			if !slices.Contains(st.Before, ix.Name) && ix.Constraint == "" && !ix.Internal {
				names = append(names, ix.Name)
			}
		}
		if len(names) == 0 {
			return nil
		}
		stmt = &DropIndex{Names: names, IfExists: true}
	case "drop_index":
		stmt = &DropIndex{Names: st.Names, IfExists: true}
	case "drop_columns", "drop_column":
		ts, err := s.E.Table(st.Table)
		if err != nil {
			return nil
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
			return nil
		}
		stmt = &AlterTable{Name: st.Table, IfExists: true, Actions: acts}
	default:
		return fmt.Errorf("unknown ddl journal step %q", st.Op)
	}
	_, err := s.exec(stmt, nil)
	return err
}

func (s *Session) ddlSteps(steps []ddlStep, rev bool) (firstErr error) {
	for i := range steps {
		st := steps[i]
		if rev {
			st = steps[len(steps)-1-i]
		}
		if err := s.ddlStepRun(st); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s %s: %w", st.Op, st.Table+strings.Join(st.Names, ","), err)
		}
	}
	return firstErr
}

// ddlUndo runs after the transaction ended without committing.
func (s *Session) ddlUndo() {
	j := s.dj
	if j == nil {
		return
	}
	s.dj = nil
	if err := s.ddlSteps(j.undo, true); err != nil {
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

// ddlAfterCommit runs after the transaction committed.
func (s *Session) ddlAfterCommit() {
	j := s.dj
	if j == nil {
		return
	}
	s.dj = nil
	if ddlCrashAfterCommit != nil && ddlCrashAfterCommit() {
		return
	}
	if err := s.ddlSteps(j.redo, false); err != nil {
		s.E.Logf("ddl journal: deferred DDL of a committed transaction failed (retried once at the next start): %v", err)
		return
	}
	s.ddlForget(j.id)
}

func (s *Session) ddlForget(id string) {
	_ = s.E.WriteObjs([]core.ObjOp{
		{O: core.Obj{Kind: ddlKind, Name: "o-" + id}, Delete: true},
		{O: core.Obj{Kind: ddlKind, Name: "c-" + id}, Delete: true}})
}

var recovered sync.Map // *core.Engine -> *sync.Once

// recoverDDL finishes or undoes the DDL transactions a crash interrupted
// (once per engine; sessions of a replica do nothing).
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
				err = s.ddlSteps(c.Redo, false)
			} else {
				err = s.ddlSteps(m["o"].Undo, true)
			}
			if err != nil {
				s.E.Logf("ddl journal recovery %s: %v", id, err)
			}
			s.ddlForget(id)
		}
	})
}
