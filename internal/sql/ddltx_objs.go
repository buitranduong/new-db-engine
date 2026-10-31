package sql

import (
	"slices"
	"strings"

	"stask/internal/core"
)

// Transactional DDL for the objects that have no rows of their own: views,
// sequences, and the object catalog (types, domains, schemas, extensions,
// comments), plus TRUNCATE. See ddltx.go for the journal.
//
// These statements run at once and their undo is a before-image of the
// objects the statement can touch, taken (and journaled) before it runs.

type viewSnap struct {
	Name   string    `json:"name"`
	Exists bool      `json:"exists,omitempty"`
	Def    core.View `json:"def"`
}

type seqSnap struct {
	Name   string        `json:"name"`
	Exists bool          `json:"exists,omitempty"`
	Def    core.Sequence `json:"def"`
	Last   int64         `json:"last,omitempty"`
	Called bool          `json:"called,omitempty"`
}

type objSnap struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Exists bool   `json:"exists,omitempty"`
	Data   string `json:"data,omitempty"`
}

func (s *Session) viewSnaps(names []string) []viewSnap {
	var out []viewSnap
	for _, n := range names {
		v, ok, _ := s.E.View(nil, n)
		sn := viewSnap{Name: n, Exists: ok}
		if ok && v != nil {
			sn.Def = *v
		}
		out = append(out, sn)
	}
	return out
}

func (s *Session) seqSnaps(names []string) []seqSnap {
	var out []seqSnap
	all := s.E.Sequences()
	for _, n := range names {
		sn := seqSnap{Name: n}
		if i := slices.IndexFunc(all, func(q core.Sequence) bool { return q.Name == n }); i >= 0 {
			sn.Exists, sn.Def = true, all[i]
			sn.Last, sn.Called, _ = s.E.SequenceState(n)
		}
		out = append(out, sn)
	}
	return out
}

func (s *Session) objSnaps(kind string, names ...string) []objSnap {
	var out []objSnap
	for _, n := range names {
		o, ok := s.E.Obj(kind, n)
		out = append(out, objSnap{Kind: kind, Name: n, Exists: ok, Data: o.Data})
	}
	return out
}

// commentSnaps snapshots the comments that a statement on the relations or
// objects with the given comment keys can delete or move.
func (s *Session) commentSnaps(keys ...string) []objSnap {
	var out []objSnap
	seen := map[string]bool{}
	for _, k := range keys {
		for _, name := range append([]string{k}, s.commentNamesUnder(k)...) {
			if !seen[name] {
				seen[name] = true
				out = append(out, s.objSnaps("comment", name)...)
			}
		}
	}
	return out
}

// commentNamesUnder lists the column comments of a relation key "rel:x".
func (s *Session) commentNamesUnder(k string) []string {
	rel, ok := strings.CutPrefix(k, "rel:")
	if !ok {
		return nil
	}
	var out []string
	for _, o := range s.E.Objs("comment") {
		if strings.HasPrefix(o.Name, "col:"+rel+"#") {
			out = append(out, o.Name)
		}
	}
	return out
}

func (s *Session) restoreViews(snaps []viewSnap) error {
	for i := len(snaps) - 1; i >= 0; i-- {
		sn := snaps[i]
		_, cur, _ := s.E.View(nil, sn.Name)
		switch {
		case sn.Exists:
			if err := s.E.PutView(sn.Def, true); err != nil {
				return err
			}
		case cur:
			if err := s.E.DropViews([]string{sn.Name}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Session) restoreSeqs(snaps []seqSnap) error {
	for i := len(snaps) - 1; i >= 0; i-- {
		sn := snaps[i]
		_, _, err := s.E.SequenceState(sn.Name)
		cur := err == nil
		switch {
		case !sn.Exists && cur:
			if err := s.E.DropSequence(sn.Name); err != nil {
				return err
			}
		case sn.Exists:
			if cur {
				if err := s.E.AlterSequence(sn.Name, func(d *core.Sequence) (*int64, error) { *d = sn.Def; return nil, nil }); err != nil {
					return err
				}
			} else if err := s.E.CreateSequence(sn.Def); err != nil {
				return err
			}
			// a sequence's value is not transactional (nextval), but ALTER ..
			// RESTART and TRUNCATE .. RESTART IDENTITY are: put it back
			if cur, called, err := s.E.SequenceState(sn.Name); err != nil || cur != sn.Last || called != sn.Called {
				if err := s.E.SetVal(sn.Name, sn.Last, sn.Called); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Session) restoreObjs(snaps []objSnap) error {
	var ops []core.ObjOp
	for i := len(snaps) - 1; i >= 0; i-- {
		sn := snaps[i]
		_, cur := s.E.Obj(sn.Kind, sn.Name)
		switch {
		case sn.Exists:
			ops = append(ops, core.ObjOp{O: core.Obj{Kind: sn.Kind, Name: sn.Name, Data: sn.Data}, Replace: true})
		case cur:
			ops = append(ops, core.ObjOp{O: core.Obj{Kind: sn.Kind, Name: sn.Name}, Delete: true})
		}
	}
	if len(ops) == 0 {
		return nil
	}
	if err := s.E.WriteObjs(dedupOps(ops)); err != nil {
		return err
	}
	loadUserTypes(s.E)
	var fns []string
	for _, op := range ops {
		if op.O.Kind == "func" {
			fns = append(fns, op.O.Name)
		}
	}
	s.refreshFuncs(fns)
	return nil
}

// djViews journals CREATE [OR REPLACE] VIEW and DROP VIEW.
func (s *Session) djViews(st Stmt, run func() (*Result, error)) (*Result, bool, error) {
	var names []string
	var comments []string
	switch st := st.(type) {
	case *CreateView:
		names = []string{st.Name}
	case *DropView:
		set := map[string]bool{}
		for _, n := range st.Names {
			set[n] = true
			names = append(names, n)
			comments = append(comments, "rel:"+n)
		}
		deps, _ := s.viewDependents(set)
		for _, d := range deps {
			names = append(names, d)
			comments = append(comments, "rel:"+d)
		}
	}
	undo := ddlStep{Op: "restore_views", Views: s.viewSnaps(names), Objs: s.commentSnaps(comments...)}
	if err := s.djStart(); err != nil {
		return s.djFail(err)
	}
	if s.dj.views == nil {
		s.dj.views = map[string]bool{}
	}
	for _, n := range names {
		s.dj.views[n] = true
	}
	return s.djRun(undo, run)
}

// djRun journals an undo step, then runs the statement.
func (s *Session) djRun(undo ddlStep, run func() (*Result, error)) (*Result, bool, error) {
	if _, err := s.djUndo(undo); err != nil {
		return s.djFail(err)
	}
	res, err := run()
	return res, true, err
}

// djSeqs journals CREATE, DROP and ALTER SEQUENCE.
func (s *Session) djSeqs(st Stmt, run func() (*Result, error)) (*Result, bool, error) {
	var names []string
	switch st := st.(type) {
	case *CreateSequence:
		names = []string{st.Name}
	case *DropSequence:
		names = st.Names
	case *AlterSequence:
		names = []string{st.Name}
	}
	var keys []string
	if d, ok := st.(*DropSequence); ok {
		for _, n := range d.Names {
			keys = append(keys, "seq:"+n)
		}
	}
	undo := ddlStep{Op: "restore_seqs", Seqs: s.seqSnaps(names), Objs: s.commentSnaps(keys...)}
	return s.djRun(undo, run)
}

// objKinds are the catalog kinds a CREATE/DROP/ALTER of a class touches.
func objKinds(class string) []string {
	switch class {
	case "type", "domain":
		return []string{"enum", "domain"}
	case "schema":
		return []string{"schema"}
	case "extension":
		return []string{"ext"}
	case "event trigger":
		return []string{"evtrig"}
	}
	return nil
}

// djObjs journals the statements of ddlobjs.go. Statements that change
// other objects' rows (ALTER TYPE .. RENAME, ADD/RENAME VALUE rewrite
// columns; CASCADE drops columns, tables, views) are not journaled and
// stay applied at once.
func (s *Session) djObjs(st Stmt, run func() (*Result, error)) (*Result, bool, error) {
	var undo ddlStep
	switch st := st.(type) {
	case *CommentOn:
		name := st.Name
		switch st.Class {
		case "table", "view", "column", "sequence":
			name = s.resolveRel(name)
		case "type", "domain":
			if ut, err := s.findType(name); err == nil {
				name = ut.Name
			}
		}
		undo = ddlStep{Op: "restore_objs", Objs: s.objSnaps("comment", commentKey(st.Class, name, st.Col), commentKey(st.Class, st.Name, st.Col))}
	case *ObjDDL:
		kinds := objKinds(st.Class)
		if kinds == nil {
			return nil, false, nil
		}
		if st.Cascade {
			if st.Op == "drop" && (st.Class == "type" || st.Class == "domain" || st.Class == "schema") {
				return s.djCascade(st, kinds, run)
			}
			return nil, false, nil
		}
		switch st.Class + " " + st.Op {
		case "type alter":
			if st.Action == "rename" || st.Action == "rename_value" {
				return s.djRenameType(st, run)
			}
		case "domain alter":
			if st.Action == "rename" {
				return s.djRenameType(st, run)
			}
		case "schema alter":
			return nil, false, nil // renames are refused; OWNER is a no-op
		}
		names := slices.Clone(st.Names)
		if st.NewName != "" {
			names = append(names, st.NewName)
		}
		var resolved []string
		for _, n := range names {
			resolved = append(resolved, n)
			if st.Class == "type" || st.Class == "domain" {
				if ut, err := s.findType(n); err == nil && ut.Name != n {
					resolved = append(resolved, ut.Name)
				}
			}
		}
		var snaps []objSnap
		var ckeys []string
		for _, n := range resolved {
			for _, k := range kinds {
				snaps = append(snaps, s.objSnaps(k, n)...)
			}
			switch st.Class {
			case "type", "domain":
				ckeys = append(ckeys, "type:"+n)
			case "schema":
				ckeys = append(ckeys, "schema:"+n)
			case "extension":
				ckeys = append(ckeys, "ext:"+n)
			}
		}
		undo = ddlStep{Op: "restore_objs", Objs: append(snaps, s.commentSnaps(ckeys...)...)}
	}
	return s.djRun(undo, run)
}

// djTruncate runs TRUNCATE inside a transaction as a delete of every row
// by this transaction, so ROLLBACK discards it. O(rows) instead of O(1),
// and the DELETE triggers do not fire (as for TRUNCATE).
func (s *Session) djTruncate(st *Truncate, run func() (*Result, error)) (*Result, bool, error) {
	names := s.mapTemps(st.Names)
	for _, n := range names {
		ts, err := s.userTable(n)
		if err != nil || ts.Schema().MatView != "" {
			return nil, false, nil // the normal path reports it
		}
	}
	all := names
	var added []string
	if st.Cascade {
		all, added = s.truncateClosure(names)
	}
	for _, n := range all {
		for _, r := range s.E.FKRefs(n) {
			if r.Child != n && !slices.Contains(all, r.Child) {
				return nil, false, nil // refused by the normal path
			}
		}
	}
	res := &Result{Tag: "TRUNCATE TABLE"}
	if len(added) > 0 {
		res.Notice = "truncate cascades to table " + strings.Join(added, ", ")
	}
	var seqNames []string
	if st.Restart {
		for _, n := range all {
			for _, q := range s.tableSequences(n) {
				seqNames = append(seqNames, q.Name)
			}
		}
	}
	if len(seqNames) > 0 {
		if _, err := s.djUndo(ddlStep{Op: "restore_seqs", Seqs: s.seqSnaps(seqNames)}); err != nil {
			return s.djFail(err)
		}
	}
	// children first, so the foreign keys between the tables hold
	order := slices.Clone(all)
	slices.Reverse(order)
	s.noTrig = true
	defer func() { s.noTrig = false }()
	s.djBypass = true
	defer func() { s.djBypass = false }()
	for _, n := range order {
		if _, err := s.exec(&Delete{Table: n}, nil); err != nil {
			s.markFailed()
			return nil, true, err
		}
	}
	for _, q := range seqNames {
		if err := s.E.AlterSequence(q, func(d *core.Sequence) (*int64, error) { return &d.Start, nil }); err != nil {
			return s.djFail(err)
		}
	}
	return res, true, nil
}

type trigSnap struct {
	Table  string       `json:"table"`
	Name   string       `json:"name"`
	Exists bool         `json:"exists,omitempty"`
	Def    core.Trigger `json:"def"`
}

func (s *Session) restoreTrigs(snaps []trigSnap) error {
	for i := len(snaps) - 1; i >= 0; i-- {
		sn := snaps[i]
		_, cur := s.E.Trigger(sn.Table, sn.Name)
		switch {
		case sn.Exists:
			if _, err := s.E.Table(sn.Table); err != nil {
				continue // the table is gone: so are its triggers
			}
			def := sn.Def
			if err := s.E.PutTrigger(&def, true); err != nil {
				return err
			}
		case cur:
			if err := s.E.DropTrigger(sn.Table, sn.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// djTriggers journals CREATE [OR REPLACE] TRIGGER and DROP TRIGGER.
func (s *Session) djTriggers(st Stmt, run func() (*Result, error)) (*Result, bool, error) {
	var table, name string
	switch st := st.(type) {
	case *CreateTrigger:
		table, name = st.Table, st.Name
	case *DropTrigger:
		table, name = st.Table, st.Name
		if table == "" {
			for _, tg := range s.E.AllTriggers() {
				if tg.Name == name {
					table = tg.Table
					break
				}
			}
		}
	}
	sn := trigSnap{Table: table, Name: name}
	if tg, ok := s.E.Trigger(table, name); ok {
		sn.Exists, sn.Def = true, *tg
	}
	return s.djRun(ddlStep{Op: "restore_trigs", Trigs: []trigSnap{sn}}, run)
}
