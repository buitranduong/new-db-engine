package sql

import "stask/internal/core"

// More transactional DDL (branch sqlgaps2; docs/05 "DDL trong giao dịch"):
// CREATE/DROP FUNCTION and PROCEDURE (before-image of the function object),
// ALTER INDEX .. RENAME, ALTER TYPE / DOMAIN .. RENAME TO and ALTER TYPE ..
// RENAME VALUE (the inverse statement as the undo step: the data is
// rewritten back by the same code that rewrote it). PostgreSQL docs, "Transactional
// DDL": every DDL command except a few (DROP DATABASE, ...) rolls back.

// djFuncs journals CREATE [OR REPLACE] and DROP of a function or procedure.
func (s *Session) djFuncs(name string, cascade bool, run func() (*Result, error)) (*Result, bool, error) {
	if cascade {
		return nil, false, nil // CASCADE drops triggers, which the before-image does not hold
	}
	return s.djRun(ddlStep{Op: "restore_objs", Objs: s.objSnaps("func", name)}, run)
}

// djRenameIndex journals ALTER INDEX .. RENAME TO.
func (s *Session) djRenameIndex(st *RenameIndex, run func() (*Result, error)) (*Result, bool, error) {
	tbl, ok := s.E.IndexNameTaken(st.Name)
	if !ok || st.Name == st.NewName {
		return nil, false, nil // the normal path reports it
	}
	if _, taken := s.E.IndexNameTaken(st.NewName); taken || s.relationExists(st.NewName) {
		return nil, false, nil
	}
	_ = tbl
	return s.djRun(ddlStep{Op: "rename_index", Table: st.NewName, Names: []string{st.Name}}, run)
}

// djRenameType journals ALTER TYPE / DOMAIN .. RENAME TO and ALTER TYPE ..
// RENAME VALUE. ok=false: not one of them, or one that fails (the normal
// path reports the error).
func (s *Session) djRenameType(st *ObjDDL, run func() (*Result, error)) (*Result, bool, error) {
	ut, err := s.findType(st.Names[0])
	if err != nil {
		return nil, false, nil
	}
	switch st.Action {
	case "rename":
		nn := st.NewName
		if sch, _, ok := splitSchema(ut.Name); ok {
			nn = sch + "." + st.NewName
		}
		if s.typeNameTaken(nn) {
			return nil, false, nil
		}
		return s.djRun(ddlStep{Op: "rename_type", Table: nn, Names: []string{bareName(ut.Name)}, Flag: ut.Kind == "domain"}, run)
	case "rename_value":
		if ut.Kind != "enum" {
			return nil, false, nil
		}
		if _, ok := ut.ord[st.Value]; !ok {
			return nil, false, nil
		}
		if _, ok := ut.ord[st.NewName]; ok {
			return nil, false, nil
		}
		return s.djRun(ddlStep{Op: "rename_enum_value", Table: ut.Name, Names: []string{st.NewName, st.Value}}, run)
	}
	return nil, false, nil
}

// moreStepRun runs the undo steps of this file; ok=false: not one of them.
func (s *Session) moreStepRun(st ddlStep) (stmt Stmt, done bool, err error) {
	switch st.Op {
	case "rename_index": // Table: current name, Names[0]: name to restore
		tbl, ok := s.E.IndexNameTaken(st.Table)
		if !ok {
			return nil, true, nil
		}
		if _, taken := s.E.IndexNameTaken(st.Names[0]); taken || s.relationExists(st.Names[0]) {
			return nil, true, nil
		}
		return nil, true, s.renameIndexOf(tbl, st.Table, st.Names[0])
	case "rename_type": // Table: current name, Names[0]: bare name to restore
		ut, err := s.findType(st.Table)
		if err != nil {
			return nil, true, nil
		}
		old := st.Names[0]
		if sch, _, ok := splitSchema(ut.Name); ok {
			old = sch + "." + old
		}
		if s.typeNameTaken(old) {
			return nil, true, nil
		}
		class := "type"
		if st.Flag {
			class = "domain"
		}
		return &ObjDDL{Op: "alter", Class: class, Action: "rename", Names: []string{st.Table}, NewName: st.Names[0]}, false, nil
	case "rename_enum_value": // Table: type, Names: [current label, label to restore]
		ut, err := s.findType(st.Table)
		if err != nil || ut.Kind != "enum" {
			return nil, true, nil
		}
		if _, ok := ut.ord[st.Names[0]]; !ok {
			return nil, true, nil
		}
		if _, ok := ut.ord[st.Names[1]]; ok {
			return nil, true, nil
		}
		return &ObjDDL{Op: "alter", Class: "type", Action: "rename_value", Names: []string{st.Table}, Value: st.Names[0], NewName: st.Names[1]}, false, nil
	}
	return nil, false, nil
}

// refreshFuncs re-reads the process-wide function caches of restored function objects.
func (s *Session) refreshFuncs(names []string) {
	for _, n := range names {
		noteFuncTypes(n, s.funcsNamed(n))
	}
}

// tableForDDL finds a table to journal DDL on: a user table, or the engine
// table behind a session's TEMP table (a system table, named by tempReal).
func (s *Session) tableForDDL(name string) (*core.TableState, error) {
	if isTempName(name) {
		return s.E.Table(name)
	}
	return s.E.UserTable(name)
}

// djPartition journals CREATE TABLE .. PARTITION OF: a partition is a view
// plus an entry in its parent's "part" object, both restored by the undo
// step. ATTACH / DETACH PARTITION move rows and drop or create tables and
// stay applied at once.
func (s *Session) djPartition(st *CreatePartition, run func() (*Result, error)) (*Result, bool, error) {
	if _, ok := s.partInfo(st.Parent); !ok || s.relationExists(st.Name) {
		return nil, false, nil // the normal path reports it
	}
	return s.djRun(ddlStep{Op: "restore_views", Views: s.viewSnaps([]string{st.Name}), Objs: s.objSnaps("part", st.Parent)}, run)
}
