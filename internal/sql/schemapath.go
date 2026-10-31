package sql

import "stask/internal/core"

// A view or materialized view stores its query as text, and an unqualified name in it is resolved
// when the text is parsed. PostgreSQL resolves it once, at CREATE time, so the view keeps working
// whatever search_path a later session has (CREATE SCHEMA s; SET search_path = s; CREATE TABLE t
// ...; CREATE VIEW v AS SELECT * FROM t; then SELECT * FROM v from any session). Here the
// search_path in force at CREATE is kept in a catalog object of kind "vpath" (only when it is not
// the default path, so nothing changes for existing databases) and the text is parsed again with
// that path (parseOwned). Precedent: PostgreSQL stores the parse tree with resolved OIDs.

// noteOwnerPath records (or clears) the search_path the query of view/matview owner was written under.
func (s *Session) noteOwnerPath(owner string) {
	if s.customPath() {
		_ = s.E.PutObj(core.Obj{Kind: "vpath", Name: owner, Data: s.vars["search_path"]}, true)
	} else if _, ok := s.E.Obj("vpath", owner); ok {
		_ = s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: "vpath", Name: owner}, Delete: true}})
	}
}

// parseOwned parses the stored query text of a view or materialized view with the search_path it
// was created under.
func (s *Session) parseOwned(owner, text string) (Stmt, error) {
	if s != nil && s.E != nil {
		if o, ok := s.E.Obj("vpath", owner); ok {
			saved, had := s.vars["search_path"]
			s.vars["search_path"] = o.Data
			defer func() {
				if had {
					s.vars["search_path"] = saved
				} else {
					delete(s.vars, "search_path")
				}
			}()
			return s.ParseOne(text)
		}
	}
	return ParseOne(text)
}
