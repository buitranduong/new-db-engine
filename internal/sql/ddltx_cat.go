package sql

import "stask/internal/core"

// Transactional GRANT / REVOKE / CREATE, ALTER, DROP ROLE and the CDC
// catalog (publications, REPLICA IDENTITY): applied at once, undone from a
// before-image of the whole catalog taken (and journaled) before the
// statement. See ddltx.go for the journal.

type cdcSnap struct {
	Pubs  []core.Publication `json:"pubs,omitempty"`
	Ident []string           `json:"ident,omitempty"`
}

type roleSnap struct {
	Roles []core.Role `json:"roles,omitempty"`
}

func (s *Session) djRoles(run func() (*Result, error)) (*Result, bool, error) {
	if !s.isSuper() {
		return nil, false, nil // refused by the normal path
	}
	return s.djRun(ddlStep{Op: "restore_roles", RoleSnap: &roleSnap{Roles: s.E.Grants()}}, run)
}

func (s *Session) djCDC(run func() (*Result, error)) (*Result, bool, error) {
	pubs, ident := s.E.CDCSnapshot()
	return s.djRun(ddlStep{Op: "restore_cdc", CDC: &cdcSnap{Pubs: pubs, Ident: ident}}, run)
}

// DROP TYPE / DOMAIN / SCHEMA ... CASCADE drops columns, tables, views and
// sequences. Under the journal each of those goes through its own journaled
// statement (DROP COLUMN and DROP TABLE hide or defer, DROP VIEW and DROP
// SEQUENCE keep a before-image), after one before-image of the catalog rows
// of the type or schema itself.

// cdAlter and cdDrop are the statements a cascading drop runs: journaled
// while s.djCascade is set, plain otherwise.
func (s *Session) cdAlter(st *AlterTable) (*Result, error) {
	if s.inCascade {
		if res, handled, err := s.txDDL(st, nil); handled {
			return res, err
		}
	}
	return s.alterTable(st)
}

func (s *Session) cdDrop(st Stmt) (*Result, error) {
	if s.inCascade {
		if res, handled, err := s.txDDL(st, nil); handled {
			return res, err
		}
	}
	switch st := st.(type) {
	case *DropView:
		return s.dropViews(st)
	case *DropTable:
		return s.dropOrTruncate(st)
	case *DropSequence:
		return s.seqDDL(st)
	}
	return nil, errf(codeFeature, "unsupported statement %T", st)
}

func (s *Session) djCascade(st *ObjDDL, kinds []string, run func() (*Result, error)) (*Result, bool, error) {
	var snaps []objSnap
	var ckeys []string
	for _, n := range st.Names {
		names := []string{n}
		if st.Class != "schema" {
			if ut, err := s.findType(n); err == nil && ut.Name != n {
				names = append(names, ut.Name)
			}
		}
		for _, m := range names {
			for _, k := range kinds {
				snaps = append(snaps, s.objSnaps(k, m)...)
			}
			switch st.Class {
			case "schema":
				ckeys = append(ckeys, "schema:"+m)
				// the types of the schema go with it
				_, _, _, types := s.schemaMembers(m)
				for _, tn := range types {
					snaps = append(snaps, s.objSnaps("enum", tn)...)
					snaps = append(snaps, s.objSnaps("domain", tn)...)
					ckeys = append(ckeys, "type:"+tn)
				}
			default:
				ckeys = append(ckeys, "type:"+m)
			}
		}
	}
	undo := ddlStep{Op: "restore_objs", Objs: append(snaps, s.commentSnaps(ckeys...)...)}
	if _, err := s.djUndo(undo); err != nil {
		return s.djFail(err)
	}
	s.inCascade = true
	defer func() { s.inCascade = false }()
	res, err := run()
	return res, true, err
}
