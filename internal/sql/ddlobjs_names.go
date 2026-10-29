package sql

import (
	"strings"
)

// Schemas and the search path (branch ddlobjs).
//
// A relation (table, view, sequence) or type in schema s is stored under
// the name "s.name"; the public schema keeps the bare name, so everything
// created before schemas existed stays where it was. The parser resolves a
// name once, when the statement is parsed: "s.t" stays as it is, and an
// unqualified name is looked up along the session's search_path (or, for a
// CREATE, takes the first existing schema of the path). With the default
// path nothing is looked up and no behavior changes.

const defaultSearchPath = `"$user", public`

// normSearchPath is a search_path value as SHOW prints it.
func normSearchPath(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "default") {
		return defaultSearchPath
	}
	var out []string
	for _, f := range strings.Split(raw, ",") {
		f = strings.Trim(strings.TrimSpace(f), `'"`)
		if f == "" {
			continue
		}
		out = append(out, quoteIdent(f))
	}
	if len(out) == 0 {
		return `""`
	}
	return strings.Join(out, ", ")
}

// pathList is the schemas of the search path, in order ("$user" resolved,
// schemas that do not exist dropped).
func (s *Session) pathList() []string {
	var out []string
	for _, f := range strings.Split(s.vars["search_path"], ",") {
		f = strings.Trim(strings.TrimSpace(f), `"`)
		switch {
		case f == "":
		case f == "$user":
			if u := s.currentUser(); s.schemaExists(u) {
				out = append(out, u)
			}
		case s.schemaExists(f):
			out = append(out, f)
		}
	}
	return out
}

// customPath: the search path is anything but the default.
func (s *Session) customPath() bool {
	v := s.vars["search_path"]
	return v != defaultSearchPath && v != "public" && v != ""
}

func (s *Session) schemaExists(name string) bool {
	switch name {
	case "public", "pg_catalog", "information_schema":
		return true
	}
	_, ok := s.E.Obj("schema", name)
	return ok
}

// pathCandidates are the stored names an unqualified name can have, in
// search order.
func (s *Session) pathCandidates(name string) []string {
	var out []string
	for _, sch := range s.pathList() {
		if sch == "public" {
			out = append(out, name)
		} else {
			out = append(out, sch+"."+name)
		}
	}
	return out
}

// createTarget is the stored name a CREATE of name makes: a qualified name
// needs its schema; an unqualified one goes to the first schema of the path.
func (s *Session) createTarget(name string) (string, error) {
	if sch, _, ok := splitSchema(name); ok {
		if !s.schemaExists(sch) {
			return "", errf("3F000", "schema %q does not exist", sch)
		}
		return name, nil
	}
	if !s.customPath() {
		return name, nil
	}
	for _, sch := range s.pathList() {
		if sch == "public" {
			return name, nil
		}
		if sch != "pg_catalog" && sch != "information_schema" {
			return sch + "." + name, nil
		}
	}
	return "", errf("3F000", "no schema has been selected to create in")
}

// splitSchema splits a stored name into its schema and bare name; ok is
// false for a public relation.
func splitSchema(name string) (sch, bare string, ok bool) {
	if strings.HasPrefix(name, infoPrefix) {
		return "", name, false
	}
	i := strings.IndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return "", name, false
	}
	return name[:i], name[i+1:], true
}

// searchRel resolves an unqualified relation name along the search path.
func (p *parser) searchRel(n string) string {
	s := p.sess
	if s == nil || !s.customPath() {
		return n
	}
	for _, c := range s.pathCandidates(n) {
		if s.relationExists(c) {
			return c
		}
	}
	return n
}

// qualifiedNameNew is qualifiedName for the name a CREATE makes.
func (p *parser) qualifiedNameNew() (string, error) {
	if t := p.peekN(1); t.k == tOp && t.s == "." {
		n, err := p.qualifiedName()
		if err == nil && p.sess != nil {
			if sch, _, ok := splitSchema(n); ok && !p.sess.schemaExists(sch) {
				return "", errf("3F000", "schema %q does not exist", sch)
			}
		}
		return n, err
	}
	n, err := p.ident()
	if err != nil || p.sess == nil {
		return n, err
	}
	return p.sess.createTarget(n)
}

// Parse parses src for this session, resolving relation names along its
// search path.
func (s *Session) Parse(src string) ([]Stmt, error) { return parseWith(src, s) }

// ParseOne is ParseOne for this session.
func (s *Session) ParseOne(src string) (Stmt, error) {
	ss, err := s.Parse(src)
	if err != nil {
		return nil, err
	}
	switch len(ss) {
	case 0:
		return nil, nil
	case 1:
		return ss[0], nil
	}
	return nil, errf(codeSyntax, "cannot insert multiple commands into a prepared statement")
}

// bareName is the part of a stored name after its schema.
func bareName(name string) string {
	_, b, _ := splitSchema(name)
	return b
}
