package sql

import (
	"strings"
)

// Object DDL (branch ddlobjs): CREATE/DROP/ALTER of TYPE (enum), DOMAIN,
// SCHEMA and EXTENSION, and COMMENT ON. This file parses them; ddlobjs_exec.go
// runs them; the definitions live in the object catalog (core/objcat.go).

// ObjDDL is one object DDL statement.
type ObjDDL struct {
	CollOpts    map[string]string // CREATE COLLATION (...) options, lower case names
	CollFrom    string            // CREATE COLLATION name FROM existing
	Op          string            // create, drop, alter
	Class       string            // type, domain, schema, extension
	Names       []string
	IfExists    bool // DROP/ALTER IF EXISTS
	IfNotExists bool // CREATE IF NOT EXISTS / ADD VALUE IF NOT EXISTS
	Cascade     bool

	// CREATE TYPE .. AS ENUM
	Labels []string
	// CREATE TYPE .. AS (attrs) (compositetypes.go)
	Attrs []compAttr
	// ALTER TYPE: add_value, rename_value, rename; ALTER DOMAIN: set_default,
	// drop_default, set_not_null, drop_not_null, add_check, drop_constraint,
	// rename; ALTER SCHEMA: rename
	Action  string
	Value   string // ADD VALUE 'v' / RENAME VALUE 'v'
	Pos     string // before, after
	PosVal  string // the neighbour label
	NewName string // RENAME TO / RENAME VALUE .. TO
	// CREATE DOMAIN, ALTER DOMAIN
	BaseType, BasePG string
	Default          string
	NotNull          bool
	Checks           []CheckDef
	ConName          string // DROP CONSTRAINT
	// CREATE EXTENSION
	Schema  string
	Version string
}

func (*ObjDDL) stmt() {}

// CommentOn is COMMENT ON class name IS 'text'. For a column, Name is the
// table and Col the column.
type CommentOn struct {
	Class string // table, column, index, type, domain, schema, extension, view, sequence
	Name  string
	Col   string
	Text  string
	Null  bool
}

func (*CommentOn) stmt() {}

// dottedName reads ident [. ident ...].
func (p *parser) dottedName() ([]string, error) {
	var out []string
	for {
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
		if !p.acceptOp(".") {
			return out, nil
		}
	}
}

// typeQName is a type name, schema-qualified or not ("public." dropped).
func (p *parser) typeQName() (string, error) {
	n, err := p.ident()
	if err != nil {
		return "", err
	}
	if !p.acceptOp(".") {
		return n, nil
	}
	v, err := p.ident()
	if err != nil {
		return "", err
	}
	if n == "public" || n == "pg_catalog" {
		return v, nil
	}
	return n + "." + v, nil
}

func (p *parser) strLit() (string, error) {
	t := p.peek()
	if t.k != tString {
		return "", p.errHere("syntax error at or near %q", t.s)
	}
	p.next()
	return t.s, nil
}

// createObj parses what follows CREATE for the object classes of this file.
func (p *parser) createObj() (Stmt, bool, error) {
	switch {
	case p.acceptKw("type"):
		c := &ObjDDL{Op: "create", Class: "type"}
		n, err := p.typeQName()
		if err != nil {
			return nil, true, err
		}
		c.Names = []string{n}
		if err := p.expectKw("as"); err != nil {
			return nil, true, err
		}
		if p.isOp("(") {
			c.Action = "composite"
			return c, true, p.compositeAttrs(c)
		}
		if !p.acceptKw("enum") {
			return nil, true, errf(codeFeature, "only CREATE TYPE .. AS ENUM or (attributes) is supported")
		}
		if err := p.expectOp("("); err != nil {
			return nil, true, err
		}
		if !p.acceptOp(")") {
			for {
				l, err := p.strLit()
				if err != nil {
					return nil, true, err
				}
				c.Labels = append(c.Labels, l)
				if p.acceptOp(")") {
					break
				}
				if err := p.expectOp(","); err != nil {
					return nil, true, err
				}
			}
		}
		return c, true, nil
	case p.acceptKw("collation"):
		return p.createCollation()
	case p.acceptKw("domain"):
		c := &ObjDDL{Op: "create", Class: "domain"}
		n, err := p.typeQName()
		if err != nil {
			return nil, true, err
		}
		c.Names = []string{n}
		p.acceptKw("as")
		if c.BaseType, err = p.typeName(); err != nil {
			return nil, true, err
		}
		c.BasePG = p.pgSpec
		if strings.HasPrefix(c.BasePG, "ut:") {
			return nil, true, errf(codeFeature, "a domain over another user-defined type is not supported")
		}
		if c.BaseType == "date" { // stored as a timestamp at midnight (colDef)
			c.BaseType = "timestamp"
		}
		for {
			var cname string
			named := p.acceptKw("constraint")
			if named {
				if cname, err = p.ident(); err != nil {
					return nil, true, err
				}
			}
			switch {
			case p.acceptKw("not", "null"):
				c.NotNull = true
			case p.acceptKw("null"):
			case p.acceptKw("default") && !named:
				start := p.peek().pos
				if _, err := p.addExpr(); err != nil {
					return nil, true, err
				}
				c.Default = strings.TrimSpace(p.src[start:p.peek().pos])
			case p.isKw("check"):
				ck, err := p.checkDef(cname)
				if err != nil {
					return nil, true, err
				}
				c.Checks = append(c.Checks, ck)
			default:
				if named {
					return nil, true, p.errHere("syntax error at or near %q", p.peek().s)
				}
				return c, true, nil
			}
		}
	case p.acceptKw("schema"):
		c := &ObjDDL{Op: "create", Class: "schema", IfNotExists: p.acceptKw("if", "not", "exists")}
		if p.isKw("authorization") {
			p.next()
			n, err := p.ident()
			c.Names = []string{n}
			return c, true, err
		}
		n, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		c.Names = []string{n}
		if p.acceptKw("authorization") {
			if _, err := p.ident(); err != nil {
				return nil, true, err
			}
		}
		if p.peek().k != tEOF && !p.isOp(";") {
			return nil, true, errf(codeFeature, "CREATE SCHEMA with schema elements is not supported")
		}
		return c, true, nil
	case p.acceptKw("extension"):
		c := &ObjDDL{Op: "create", Class: "extension", IfNotExists: p.acceptKw("if", "not", "exists")}
		n, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		c.Names = []string{n}
		p.acceptKw("with")
		for {
			switch {
			case p.acceptKw("schema"):
				if c.Schema, err = p.ident(); err != nil {
					return nil, true, err
				}
			case p.acceptKw("version"):
				if p.peek().k == tString {
					c.Version, err = p.strLit()
				} else {
					c.Version, err = p.ident()
				}
				if err != nil {
					return nil, true, err
				}
			case p.acceptKw("cascade"):
				c.Cascade = true
			case p.acceptKw("from"):
				return nil, true, errf(codeFeature, "CREATE EXTENSION .. FROM is not supported")
			default:
				return c, true, nil
			}
		}
	}
	return nil, false, nil
}

// dropObj parses what follows DROP.
func (p *parser) dropObj() (Stmt, bool, error) {
	var class string
	switch {
	case p.acceptKw("type"):
		class = "type"
	case p.acceptKw("domain"):
		class = "domain"
	case p.acceptKw("schema"):
		class = "schema"
	case p.acceptKw("extension"):
		class = "extension"
	case p.acceptKw("collation"):
		class = "collation"
	default:
		return nil, false, nil
	}
	d := &ObjDDL{Op: "drop", Class: class, IfExists: p.acceptKw("if", "exists")}
	for {
		var n string
		var err error
		if class == "type" || class == "domain" {
			n, err = p.typeQName()
		} else {
			n, err = p.ident()
		}
		if err != nil {
			return nil, true, err
		}
		d.Names = append(d.Names, n)
		if !p.acceptOp(",") {
			break
		}
	}
	if d.Cascade = p.acceptKw("cascade"); !d.Cascade {
		p.acceptKw("restrict")
	}
	return d, true, nil
}

// alterObj parses what follows ALTER.
func (p *parser) alterObj() (Stmt, bool, error) {
	var class string
	switch {
	case p.acceptKw("type"):
		class = "type"
	case p.acceptKw("domain"):
		class = "domain"
	case p.acceptKw("schema"):
		class = "schema"
	case p.acceptKw("extension"):
		class = "extension"
	default:
		return nil, false, nil
	}
	a := &ObjDDL{Op: "alter", Class: class}
	var n string
	var err error
	if class == "type" || class == "domain" {
		n, err = p.typeQName()
	} else {
		n, err = p.ident()
	}
	if err != nil {
		return nil, true, err
	}
	a.Names = []string{n}
	fail := func() (Stmt, bool, error) {
		return nil, true, p.errHere("syntax error at or near %q", p.peek().s)
	}
	switch class {
	case "type":
		switch {
		case p.acceptKw("add", "value"):
			a.Action = "add_value"
			a.IfNotExists = p.acceptKw("if", "not", "exists")
			if a.Value, err = p.strLit(); err != nil {
				return nil, true, err
			}
			if p.acceptKw("before") {
				a.Pos = "before"
			} else if p.acceptKw("after") {
				a.Pos = "after"
			}
			if a.Pos != "" {
				if a.PosVal, err = p.strLit(); err != nil {
					return nil, true, err
				}
			}
		case p.acceptKw("rename", "value"):
			a.Action = "rename_value"
			if a.Value, err = p.strLit(); err != nil {
				return nil, true, err
			}
			if err := p.expectKw("to"); err != nil {
				return nil, true, err
			}
			if a.NewName, err = p.strLit(); err != nil {
				return nil, true, err
			}
		case p.acceptKw("rename", "to"):
			a.Action = "rename"
			if a.NewName, err = p.ident(); err != nil {
				return nil, true, err
			}
		case p.acceptKw("owner", "to"):
			a.Action = "owner"
			if _, err = p.ident(); err != nil {
				return nil, true, err
			}
		default:
			return fail()
		}
	case "domain":
		switch {
		case p.acceptKw("set", "default"):
			a.Action = "set_default"
			start := p.peek().pos
			if _, err := p.addExpr(); err != nil {
				return nil, true, err
			}
			a.Default = strings.TrimSpace(p.src[start:p.peek().pos])
		case p.acceptKw("drop", "default"):
			a.Action = "drop_default"
		case p.acceptKw("set", "not", "null"):
			a.Action = "set_not_null"
		case p.acceptKw("drop", "not", "null"):
			a.Action = "drop_not_null"
		case p.acceptKw("add"):
			a.Action = "add_check"
			var cname string
			if p.acceptKw("constraint") {
				if cname, err = p.ident(); err != nil {
					return nil, true, err
				}
			}
			if !p.isKw("check") {
				return nil, true, errf(codeFeature, "only CHECK constraints are supported on domains")
			}
			ck, err := p.checkDef(cname)
			if err != nil {
				return nil, true, err
			}
			a.Checks = []CheckDef{ck}
		case p.acceptKw("drop", "constraint"):
			a.Action = "drop_constraint"
			a.IfExists = p.acceptKw("if", "exists")
			if a.ConName, err = p.ident(); err != nil {
				return nil, true, err
			}
			if a.Cascade = p.acceptKw("cascade"); !a.Cascade {
				p.acceptKw("restrict")
			}
		case p.acceptKw("rename", "to"):
			a.Action = "rename"
			if a.NewName, err = p.ident(); err != nil {
				return nil, true, err
			}
		case p.acceptKw("owner", "to"):
			a.Action = "owner"
			if _, err = p.ident(); err != nil {
				return nil, true, err
			}
		default:
			return fail()
		}
	case "schema":
		if p.acceptKw("owner", "to") {
			a.Action = "owner"
			_, err = p.ident()
			return a, true, err
		}
		if p.acceptKw("rename", "to") {
			a.Action = "rename"
			a.NewName, err = p.ident()
			return a, true, err
		}
		return fail()
	case "extension":
		return nil, true, errf(codeFeature, "ALTER EXTENSION is not supported")
	}
	return a, true, nil
}

// commentOn parses COMMENT ON (after the word COMMENT); ok is false for the
// object classes that are only accepted (compat_stmts.go).
func (p *parser) commentOn() (Stmt, bool, error) {
	save := p.i
	if !p.acceptKw("on") {
		return nil, false, nil
	}
	class := ""
	for _, c := range []string{"table", "column", "index", "type", "domain", "schema", "extension", "view", "sequence"} {
		if p.acceptKw(c) {
			class = c
			break
		}
	}
	if class == "" {
		p.i = save
		return nil, false, nil
	}
	parts, err := p.dottedName()
	if err != nil {
		return nil, true, err
	}
	if err := p.expectKw("is"); err != nil {
		return nil, true, err
	}
	c := &CommentOn{Class: class}
	if p.acceptKw("null") {
		c.Null = true
	} else if c.Text, err = p.strLit(); err != nil {
		return nil, true, err
	}
	rel := func(ps []string) string { // [schema.]name; "public." is dropped
		if len(ps) == 2 && ps[0] != "public" && ps[0] != "pg_catalog" {
			return ps[0] + "." + ps[1]
		}
		return ps[len(ps)-1]
	}
	switch class {
	case "column":
		if len(parts) < 2 || len(parts) > 3 {
			return nil, true, errf(codeSyntax, "improper qualified name (too many dotted names): %s", strings.Join(parts, "."))
		}
		c.Col = parts[len(parts)-1]
		c.Name = rel(parts[:len(parts)-1])
	case "schema", "extension":
		c.Name = parts[len(parts)-1]
	default:
		if len(parts) > 2 {
			return nil, true, errf(codeSyntax, "improper qualified name (too many dotted names): %s", strings.Join(parts, "."))
		}
		c.Name = rel(parts)
		if class == "index" && len(parts) == 2 { // indexes live in one namespace here
			c.Name = parts[1]
		}
	}
	return c, true, nil
}
