package sql

import (
	"strconv"
	"strings"
)

// Parse parses a string holding zero or more ';'-separated statements.
func Parse(src string) ([]Stmt, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: catalogTokens(toks), src: src}
	var out []Stmt
	for {
		for p.isOp(";") {
			p.next()
		}
		if p.peek().k == tEOF {
			return out, nil
		}
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if !p.isOp(";") && p.peek().k != tEOF {
			return nil, p.errHere("syntax error at or near %q", p.peek().s)
		}
	}
}

// ParseOne parses exactly one statement (extended query protocol).
func ParseOne(src string) (Stmt, error) {
	ss, err := Parse(src)
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

type parser struct {
	toks []token
	i    int
	src  string
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) peekN(n int) token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.k != tEOF {
		p.i++
	}
	return t
}
func (p *parser) isKw(kw string) bool { t := p.peek(); return t.k == tIdent && t.s == kw }
func (p *parser) isOp(op string) bool { t := p.peek(); return t.k == tOp && t.s == op }
func (p *parser) acceptKw(kws ...string) bool {
	for i, kw := range kws {
		if t := p.peekN(i); t.k != tIdent || t.s != kw {
			return false
		}
	}
	p.i += len(kws)
	return true
}
func (p *parser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.next()
		return true
	}
	return false
}

func (p *parser) errHere(format string, a ...any) *Error {
	e := errf(codeSyntax, format, a...)
	e.Pos = p.peek().pos + 1
	return e
}

func (p *parser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return p.errHere("syntax error at or near %q (expected %s)", p.peek().s, strings.ToUpper(kw))
	}
	return nil
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.errHere("syntax error at or near %q (expected %q)", p.peek().s, op)
	}
	return nil
}

// reserved words cannot be bare column aliases or identifiers in some places
var reserved = map[string]bool{
	"select": true, "from": true, "where": true, "group": true, "order": true, "by": true, "limit": true,
	"offset": true, "having": true, "on": true, "returning": true, "and": true, "or": true, "not": true,
	"as": true, "union": true, "set": true, "values": true, "into": true, "for": true, "is": true, "in": true,
	"between": true, "like": true, "ilike": true, "case": true, "when": true, "then": true, "else": true,
	"end": true, "asc": true, "desc": true, "distinct": true, "all": true, "null": true, "true": true,
	"false": true, "join": true, "inner": true, "left": true, "right": true, "outer": true, "cross": true,
	"using": true, "do": true, "nulls": true,
}

func (p *parser) ident() (string, error) {
	t := p.peek()
	if t.k == tQuoted || t.k == tIdent && !reserved[t.s] {
		p.next()
		return t.s, nil
	}
	return "", p.errHere("syntax error at or near %q (expected an identifier)", t.s)
}

// qualifiedName accepts schema.name and drops a "public." prefix.
func (p *parser) qualifiedName() (string, error) {
	n, err := p.ident()
	if err != nil {
		return "", err
	}
	if p.acceptOp(".") {
		if n == "information_schema" { // its views are named with the schema (catalog_info.go)
			v, err := p.ident()
			return infoPrefix + v, err
		}
		if n != "public" && n != "pg_catalog" {
			return "", errf(codeUndefinedTable, "schema %q does not exist", n)
		}
		return p.ident()
	}
	return n, nil
}

// tableList parses [ONLY] name [*] [, ...] (no inheritance: ONLY and *
// change nothing).
func (p *parser) tableList() ([]string, error) {
	var names []string
	for {
		p.acceptKw("only")
		n, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		p.acceptOp("*")
		names = append(names, n)
		if !p.acceptOp(",") {
			return names, nil
		}
	}
}

func (p *parser) statement() (Stmt, error) {
	t := p.peek()
	if t.k == tOp && t.s == "(" {
		return p.queryStmt()
	}
	if t.k != tIdent {
		return nil, p.errHere("syntax error at or near %q", t.s)
	}
	switch t.s {
	case "select", "with", "values", "table":
		return p.queryStmt()
	case "insert":
		return p.insertStmt()
	case "update":
		return p.updateStmt()
	case "delete":
		return p.deleteStmt()
	case "create":
		return p.createStmt()
	case "drop":
		p.next()
		if p.acceptKw("sequence") {
			d := &DropSequence{IfExists: p.acceptKw("if", "exists")}
			var err error
			if d.Names, err = p.tableList(); err != nil {
				return nil, err
			}
			if !p.acceptKw("cascade") {
				p.acceptKw("restrict")
			}
			return d, nil
		}
		if p.acceptKw("publication") {
			d := &DropPublication{IfExists: p.acceptKw("if", "exists")}
			var err error
			d.Name, err = p.ident()
			return d, err
		}
		if p.acceptKw("view") {
			return p.dropView()
		}
		if err := p.expectKw("table"); err != nil {
			return nil, err
		}
		d := &DropTable{IfExists: p.acceptKw("if", "exists")}
		var err error
		d.Names, err = p.tableList()
		// only views depend on a table (no foreign keys yet): CASCADE drops them
		if d.Cascade = p.acceptKw("cascade"); !d.Cascade {
			p.acceptKw("restrict")
		}
		return d, err
	case "truncate":
		p.next()
		p.acceptKw("table")
		t := &Truncate{}
		var err error
		if t.Names, err = p.tableList(); err != nil {
			return nil, err
		}
		if t.Restart = p.acceptKw("restart", "identity"); !t.Restart {
			p.acceptKw("continue", "identity")
		}
		if !p.acceptKw("cascade") {
			p.acceptKw("restrict")
		}
		return t, nil
	case "begin", "start":
		p.next()
		if t.s == "start" {
			if err := p.expectKw("transaction"); err != nil {
				return nil, err
			}
		} else if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		b := &Begin{}
		for {
			switch {
			case p.acceptKw("isolation", "level"):
				switch {
				case p.acceptKw("serializable"):
					b.Iso = "serializable"
				case p.acceptKw("repeatable", "read"):
					b.Iso = "repeatable read"
				case p.acceptKw("read", "committed"):
					b.Iso = "read committed"
				case p.acceptKw("read", "uncommitted"):
					b.Iso = "read committed"
				default:
					return nil, p.errHere("unknown isolation level %q", p.peek().s)
				}
			case p.acceptKw("read", "write"), p.acceptKw("read", "only"), p.acceptKw("deferrable"),
				p.acceptKw("not", "deferrable"):
			case p.acceptOp(","):
			default:
				return b, nil
			}
		}
	case "commit", "end":
		p.next()
		p.acceptKw("transaction")
		p.acceptKw("work")
		return &Commit{}, nil
	case "rollback", "abort":
		p.next()
		p.acceptKw("transaction")
		p.acceptKw("work")
		return &Rollback{}, nil
	case "set":
		p.next()
		p.acceptKw("session")
		p.acceptKw("local")
		var name []string
		for {
			t := p.next()
			if t.k != tIdent && t.k != tQuoted {
				return nil, p.errHere("syntax error in SET")
			}
			name = append(name, t.s)
			if !p.acceptOp(".") {
				break
			}
		}
		// SET TRANSACTION ISOLATION LEVEL ..., SET x TO v, SET x = v
		var val []string
		if !p.acceptKw("to") {
			p.acceptOp("=")
		}
		for p.peek().k != tEOF && !p.isOp(";") {
			val = append(val, p.next().s)
		}
		return &SetVar{Name: strings.Join(name, "."), Value: strings.Join(val, " ")}, nil
	case "listen":
		p.next()
		ch, err := p.ident()
		return &Listen{Channel: ch}, err
	case "unlisten":
		p.next()
		if p.acceptOp("*") {
			return &Unlisten{All: true}, nil
		}
		ch, err := p.ident()
		return &Unlisten{Channel: ch}, err
	case "notify":
		p.next()
		ch, err := p.ident()
		if err != nil {
			return nil, err
		}
		n := &Notify{Channel: ch}
		if p.acceptOp(",") {
			t := p.next()
			if t.k != tString {
				return nil, p.errHere("syntax error at or near %q", t.s)
			}
			n.Payload = t.s
		}
		return n, nil
	case "alter":
		p.next()
		if p.acceptKw("sequence") {
			a := &AlterSequence{IfExists: p.acceptKw("if", "exists")}
			var err error
			if a.Name, err = p.qualifiedName(); err != nil {
				return nil, err
			}
			if a.Opts, err = p.seqOpts(true); err != nil {
				return nil, err
			}
			return a, nil
		}
		if err := p.expectKw("table"); err != nil {
			return nil, err
		}
		ifExists := p.acceptKw("if", "exists")
		p.acceptKw("only")
		name, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		if !p.acceptKw("replica", "identity") {
			return p.alterTable(name, ifExists)
		}
		switch {
		case p.acceptKw("default"):
			return &AlterReplicaIdentity{Table: name}, nil
		case p.acceptKw("full"):
			return &AlterReplicaIdentity{Table: name, Full: true}, nil
		}
		return nil, errf(codeFeature, "only REPLICA IDENTITY DEFAULT and FULL are supported")
	case "show":
		p.next()
		var name []string
		for p.peek().k != tEOF && !p.isOp(";") {
			name = append(name, p.next().s)
		}
		return &Show{Name: strings.Join(name, " ")}, nil
	}
	return nil, errf(codeFeature, "statement %q is not supported", strings.ToUpper(t.s))
}

// ---- DDL ----

func (p *parser) createStmt() (Stmt, error) {
	p.next()
	if st, ok, err := p.createView(); ok {
		return st, err
	}
	if p.acceptKw("publication") {
		return p.createPublication()
	}
	if p.acceptKw("sequence") {
		c := &CreateSequence{IfNotExists: p.acceptKw("if", "not", "exists")}
		var err error
		if c.Name, err = p.qualifiedName(); err != nil {
			return nil, err
		}
		if c.Opts, err = p.seqOpts(false); err != nil {
			return nil, err
		}
		return c, nil
	}
	if !p.acceptKw("table") {
		return nil, errf(codeFeature, "only CREATE TABLE is supported")
	}
	c := &CreateTable{IfNotExists: p.acceptKw("if", "not", "exists"), Options: map[string]string{}}
	var err error
	if c.Name, err = p.qualifiedName(); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		var cname string
		if p.acceptKw("constraint") {
			var err error
			if cname, err = p.ident(); err != nil {
				return nil, err
			}
		}
		if p.isKw("check") {
			ck, err := p.checkDef(cname)
			if err != nil {
				return nil, err
			}
			c.Checks = append(c.Checks, ck)
		} else if p.acceptKw("primary", "key") {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			k, err := p.ident()
			if err != nil {
				return nil, err
			}
			if p.isOp(",") {
				return nil, errf(codeFeature, "composite primary keys are not supported")
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			if c.Key != "" && c.Key != k {
				return nil, errf(codeSyntax, "multiple primary keys for table %q are not allowed", c.Name)
			}
			c.Key = k
		} else {
			cd, err := p.colDef()
			if err != nil {
				return nil, err
			}
			if cd.Primary {
				if c.Key != "" {
					return nil, errf(codeSyntax, "multiple primary keys for table %q are not allowed", c.Name)
				}
				c.Key = cd.Name
			}
			c.Cols = append(c.Cols, cd)
		}
		if p.acceptOp(")") {
			break
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("with") {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		for {
			k, err := p.ident()
			if err != nil {
				return nil, err
			}
			v := "true"
			if p.acceptOp("=") {
				t := p.next()
				if t.k == tEOF || t.k == tOp {
					return nil, p.errHere("syntax error in WITH options")
				}
				v = t.s
			}
			c.Options[k] = v
			if p.acceptOp(")") {
				break
			}
			if err := p.expectOp(","); err != nil {
				return nil, err
			}
		}
	}
	if c.Key == "" {
		return nil, errf(codeFeature, "table %q needs a single-column PRIMARY KEY", c.Name)
	}
	return c, nil
}

func (p *parser) colDef() (ColDef, error) {
	var cd ColDef
	var err error
	if cd.Name, err = p.ident(); err != nil {
		return cd, err
	}
	raw := p.peek().s
	if cd.Type, err = p.typeName(); err != nil {
		return cd, err
	}
	switch raw {
	case "smallserial", "serial", "bigserial":
		cd.Serial, cd.NotNull = true, true
		cd.IntType = map[string]string{"smallserial": "smallint", "serial": "integer", "bigserial": "bigint"}[raw]
	case "smallint", "int2":
		cd.IntType = "smallint"
	case "int", "integer", "int4":
		cd.IntType = "integer"
	default:
		if cd.Type == "int" {
			cd.IntType = "bigint"
		}
	}
	switch cd.Type {
	case "date": // stored as a timestamp at midnight
		cd.Type = "time"
	case "interval", "array":
		return cd, errf(codeFeature, "columns of type %s are not supported", cd.Type)
	}
	for {
		switch {
		case p.acceptKw("primary", "key"):
			cd.Primary, cd.NotNull = true, true
		case p.acceptKw("not", "null"):
			cd.NotNull = true
		case p.acceptKw("null"):
		case p.acceptKw("default"):
			// b_expr in PostgreSQL's grammar: no AND/OR/NOT/IS, so that
			// "DEFAULT 0 NOT NULL" ends the default before NOT
			start := p.peek().pos
			if _, err := p.addExpr(); err != nil {
				return cd, err
			}
			cd.Default = strings.TrimSpace(p.src[start:p.peek().pos])
		case p.acceptKw("generated"):
			switch {
			case p.acceptKw("always"):
				cd.Identity = "always"
			case p.acceptKw("by", "default"):
				cd.Identity = "by default"
			default:
				return cd, p.errHere("syntax error at or near %q", p.peek().s)
			}
			if !p.acceptKw("as", "identity") {
				return cd, errf(codeFeature, "generated columns are not supported")
			}
			if cd.IntType == "" {
				return cd, errf("22023", "identity column type must be smallint, integer, or bigint")
			}
			cd.NotNull = true
			if p.acceptOp("(") {
				o, err := p.seqOpts(false)
				if err != nil {
					return cd, err
				}
				cd.SeqOpts = &o
				if err := p.expectOp(")"); err != nil {
					return cd, err
				}
			}
		case p.isKw("check"), p.isKw("constraint"):
			var name string
			if p.acceptKw("constraint") {
				if name, err = p.ident(); err != nil {
					return cd, err
				}
				if !p.isKw("check") {
					return cd, errf(codeFeature, "only named CHECK column constraints are supported")
				}
			}
			ck, err := p.checkDef(name)
			if err != nil {
				return cd, err
			}
			cd.Checks = append(cd.Checks, ck)
		case p.isKw("unique"), p.isKw("references"):
			return cd, errf(codeFeature, "column constraint %s is not supported", strings.ToUpper(p.peek().s))
		default:
			return cd, nil
		}
	}
}

// typeName maps PostgreSQL type names to schema types.
func (p *parser) typeName() (string, error) {
	t := p.next()
	if t.k != tIdent {
		return "", p.errHere("syntax error: expected a type name")
	}
	skipMods := func() {
		if p.acceptOp("(") {
			for !p.acceptOp(")") && p.peek().k != tEOF {
				p.next()
			}
		}
	}
	var out string
	switch t.s {
	case "nullable", "lowcardinality": // ClickHouse wrappers
		if err := p.expectOp("("); err != nil {
			return "", err
		}
		inner, err := p.typeName()
		if err != nil {
			return "", err
		}
		return inner, p.expectOp(")")
	case "int", "integer", "int2", "int4", "int8", "bigint", "smallint", "serial", "bigserial", "smallserial",
		"int16", "int32", "int64", "uint8", "uint16", "uint32", "uint64", "tinyint", "signed", "unsigned":
		out = "int"
	case "float32", "float64", "decimal32", "decimal64", "decimal128", "double_precision":
		skipMods()
		out = "float"
	case "fixedstring", "json", "jsonb", "enum8", "enum16", "ipv4", "ipv6", "bpchar":
		skipMods()
		out = "string"
	case "datetime", "datetime64":
		skipMods()
		out = "time"
	case "date32":
		out = "date"
	case "interval":
		for p.peek().k == tIdent && intervalUnits[p.peek().s] != [2]int64{} {
			p.next() // INTERVAL DAY TO SECOND etc.
			if p.acceptKw("to") {
				p.next()
			}
		}
		out = "interval"
	case "float", "float4", "float8", "real", "numeric", "decimal":
		skipMods()
		out = "float"
	case "double":
		p.acceptKw("precision")
		out = "float"
	case "text":
		out = "text"
	case "regclass", "regtype", "oid", "regproc", "regnamespace": // OID types (catalog_funcs.go)
		out = t.s
	case "int2vector", "oidvector":
		out = "array"
	case "varchar", "char", "character", "string", "uuid", "name":
		p.acceptKw("varying")
		skipMods()
		out = "string"
	case "timestamp", "timestamptz":
		skipMods()
		if p.acceptKw("with") || p.acceptKw("without") {
			p.acceptKw("time")
			p.acceptKw("zone")
		}
		out = "time"
	case "date":
		out = "date"
	case "bool", "boolean":
		out = "bool"
	default:
		return "", errf(codeFeature, "type %q is not supported", t.s)
	}
	if p.acceptOp("[") {
		if err := p.expectOp("]"); err != nil {
			return "", err
		}
		return "array", nil
	}
	return out, nil
}

// ---- DML ----

func (p *parser) insertStmt() (Stmt, error) {
	p.next()
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	ins := &Insert{}
	var err error
	if ins.Table, err = p.qualifiedName(); err != nil {
		return nil, err
	}
	if p.acceptKw("as") {
		if _, err := p.ident(); err != nil {
			return nil, err
		}
	}
	if p.acceptOp("(") {
		for {
			c, err := p.ident()
			if err != nil {
				return nil, err
			}
			ins.Cols = append(ins.Cols, c)
			if p.acceptOp(")") {
				break
			}
			if err := p.expectOp(","); err != nil {
				return nil, err
			}
		}
	}
	if p.acceptKw("overriding") {
		switch {
		case p.acceptKw("system", "value"):
			ins.Overriding = "system"
		case p.acceptKw("user", "value"):
			ins.Overriding = "user"
		default:
			return nil, p.errHere("syntax error at or near %q", p.peek().s)
		}
	}
	if p.acceptKw("default", "values") {
		ins.Rows = [][]Expr{{}}
	} else if p.isKw("select") || p.isKw("with") || p.isOp("(") {
		q, err := p.queryStmt()
		if err != nil {
			return nil, err
		}
		ins.Query = q
	} else {
		if err := p.expectKw("values"); err != nil {
			return nil, err
		}
		for {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			row, err := p.exprList(")")
			if err != nil {
				return nil, err
			}
			ins.Rows = append(ins.Rows, row)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("on", "conflict") {
		oc := &OnConflict{}
		if p.acceptOp("(") {
			for !p.acceptOp(")") {
				if p.peek().k == tEOF {
					return nil, p.errHere("unterminated conflict target")
				}
				p.next()
			}
		}
		if err := p.expectKw("do"); err != nil {
			return nil, err
		}
		switch {
		case p.acceptKw("nothing"):
			oc.DoNothing = true
		case p.acceptKw("update"):
			if err := p.expectKw("set"); err != nil {
				return nil, err
			}
			if oc.Set, err = p.assignments(); err != nil {
				return nil, err
			}
			if p.acceptKw("where") {
				if oc.Where, err = p.expr(); err != nil {
					return nil, err
				}
			}
		default:
			return nil, p.errHere("expected NOTHING or UPDATE")
		}
		ins.OnConflict = oc
	}
	if ins.Returning, err = p.returning(); err != nil {
		return nil, err
	}
	return ins, nil
}

func (p *parser) assignments() ([]Assign, error) {
	var out []Assign
	for {
		c, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		out = append(out, Assign{c, x})
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *parser) returning() ([]SelItem, error) {
	if !p.acceptKw("returning") {
		return nil, nil
	}
	return p.selectItems()
}

func (p *parser) tableAlias() (string, string, error) {
	name, err := p.qualifiedName()
	if err != nil {
		return "", "", err
	}
	alias := ""
	if p.acceptKw("as") {
		if alias, err = p.ident(); err != nil {
			return "", "", err
		}
	} else if t := p.peek(); t.k == tIdent && !reserved[t.s] || t.k == tQuoted {
		alias = p.next().s
	}
	return name, alias, nil
}

func (p *parser) updateStmt() (Stmt, error) {
	p.next()
	u := &Update{}
	var err error
	if u.Table, u.Alias, err = p.tableAlias(); err != nil {
		return nil, err
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	if u.Set, err = p.assignments(); err != nil {
		return nil, err
	}
	if p.acceptKw("from") {
		if u.From, err = p.fromList(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("where") {
		if u.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	u.Returning, err = p.returning()
	return u, err
}

func (p *parser) deleteStmt() (Stmt, error) {
	p.next()
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	d := &Delete{}
	var err error
	if d.Table, d.Alias, err = p.tableAlias(); err != nil {
		return nil, err
	}
	if p.acceptKw("using") {
		if d.Using, err = p.fromList(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("where") {
		if d.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	d.Returning, err = p.returning()
	return d, err
}

func (p *parser) selectItems() ([]SelItem, error) {
	var items []SelItem
	for {
		var it SelItem
		if p.acceptOp("*") {
			it.Star = true
		} else if t, t2, t3 := p.peek(), p.peekN(1), p.peekN(2); (t.k == tIdent || t.k == tQuoted) &&
			t2.k == tOp && t2.s == "." && t3.k == tOp && t3.s == "*" {
			p.i += 3
			it.Star = true
		} else {
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			it.X = x
			if p.acceptKw("as") {
				t := p.next()
				if t.k != tIdent && t.k != tQuoted {
					return nil, p.errHere("syntax error: expected an alias")
				}
				it.Alias = t.s
			} else if p.canAlias() {
				it.Alias = p.next().s
			}
		}
		items = append(items, it)
		if !p.acceptOp(",") {
			return items, nil
		}
	}
}

// ---- expressions ----

func (p *parser) exprList(end string) ([]Expr, error) {
	var out []Expr
	if p.acceptOp(end) {
		return out, nil
	}
	for {
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		out = append(out, x)
		if p.acceptOp(end) {
			return out, nil
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
	}
}

func (p *parser) expr() (Expr, error) {
	x, err := p.orExpr()
	if err != nil || !p.acceptOp("?") { // ClickHouse: cond ? a : b
		return x, err
	}
	a, err := p.expr()
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(":"); err != nil {
		return nil, err
	}
	b, err := p.expr()
	return &Func{Name: "if", Args: []Expr{x, a, b}}, err
}

func (p *parser) orExpr() (Expr, error) {
	l, err := p.andExpr()
	for err == nil && p.acceptKw("or") {
		var r Expr
		if r, err = p.andExpr(); err == nil {
			l = &Binary{"or", l, r}
		}
	}
	return l, err
}

func (p *parser) andExpr() (Expr, error) {
	l, err := p.notExpr()
	for err == nil && p.acceptKw("and") {
		var r Expr
		if r, err = p.notExpr(); err == nil {
			l = &Binary{"and", l, r}
		}
	}
	return l, err
}

func (p *parser) notExpr() (Expr, error) {
	if p.acceptKw("not") {
		x, err := p.notExpr()
		return &Unary{"not", x}, err
	}
	return p.predicate()
}

func (p *parser) predicate() (Expr, error) {
	l, err := p.addExpr()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.k == tOp && (t.s == "=" || t.s == "==" || t.s == "<>" || t.s == "!=" || t.s == "<" || t.s == "<=" || t.s == ">" || t.s == ">="):
			p.next()
			op := t.s
			if op == "!=" {
				op = "<>"
			}
			if op == "==" {
				op = "="
			}
			if (p.isKw("any") || p.isKw("some") || p.isKw("all")) && p.peekN(1).s == "(" {
				all := p.next().s == "all"
				p.next()
				if !p.isKw("select") && !p.isKw("with") && !p.isKw("values") {
					// x op ANY (array expression), as catalog queries write it
					arr, err := p.expr()
					if err != nil {
						return nil, err
					}
					if err := p.expectOp(")"); err != nil {
						return nil, err
					}
					l = &Func{Name: "stask_anyarr", Args: []Expr{l, arr, &Lit{TextD(op)}, &Lit{BoolD(all)}}}
					continue
				}
				q, err := p.queryStmt()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				l = &AnyAll{X: l, Op: op, All: all, Q: q}
				continue
			}
			r, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			l = &Binary{op, l, r}
		case t.k == tOp && (t.s == "~" || t.s == "!~" || t.s == "~*" || t.s == "!~*"):
			p.next()
			r, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			var x Expr = &Func{Name: "regexp_like", Args: []Expr{l, r, &Lit{TextD(map[bool]string{true: "i", false: ""}[strings.HasSuffix(t.s, "*")])}}}
			if strings.HasPrefix(t.s, "!") {
				x = &Unary{"not", x}
			}
			l = x
		case p.acceptKw("is"):
			not := p.acceptKw("not")
			switch {
			case p.acceptKw("null"):
				l = &IsNull{l, not}
			case p.acceptKw("distinct", "from"):
				r, err := p.addExpr()
				if err != nil {
					return nil, err
				}
				name := "is_distinct_from"
				if not {
					name = "is_not_distinct_from"
				}
				l = &Func{Name: name, Args: []Expr{l, r}}
			case p.acceptKw("true"), p.acceptKw("false"):
				want := p.toks[p.i-1].s == "true"
				// IS [NOT] TRUE/FALSE never yields NULL
				x := Expr(&Binary{"=", &Func{Name: "coalesce", Args: []Expr{l, &Lit{BoolD(!want)}}}, &Lit{BoolD(want)}})
				if not {
					x = &Unary{"not", x}
				}
				l = x
			default:
				return nil, p.errHere("syntax error after IS")
			}
		default:
			not := false
			if p.isKw("global") && (p.peekN(1).s == "in" || p.peekN(1).s == "not") {
				p.next() // ClickHouse GLOBAL IN: one server here
			}
			if p.isKw("not") && (p.peekN(1).s == "in" || p.peekN(1).s == "between" || p.peekN(1).s == "like" || p.peekN(1).s == "ilike") {
				p.next()
				not = true
			}
			switch {
			case p.acceptKw("in"):
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				if p.isKw("select") || p.isKw("with") || p.isKw("values") {
					q, err := p.queryStmt()
					if err != nil {
						return nil, err
					}
					if err := p.expectOp(")"); err != nil {
						return nil, err
					}
					l = &InSub{X: l, Q: q, Not: not}
					continue
				}
				list, err := p.exprList(")")
				if err != nil {
					return nil, err
				}
				l = &InList{l, list, not}
			case p.acceptKw("between"):
				sym := p.acceptKw("symmetric")
				lo, err := p.addExpr()
				if err != nil {
					return nil, err
				}
				if err := p.expectKw("and"); err != nil {
					return nil, err
				}
				hi, err := p.addExpr()
				if err != nil {
					return nil, err
				}
				if sym { // either order of the bounds
					l = &Binary{"or", &Between{l, lo, hi, false}, &Between{l, hi, lo, false}}
					if not {
						l = &Unary{"not", l}
					}
				} else {
					l = &Between{l, lo, hi, not}
				}
			case p.acceptKw("like"), p.acceptKw("ilike"):
				ic := p.toks[p.i-1].s == "ilike"
				pat, err := p.addExpr()
				if err != nil {
					return nil, err
				}
				l = &Like{l, pat, not, ic}
			default:
				if not {
					return nil, p.errHere("syntax error at or near NOT")
				}
				return l, nil
			}
		}
	}
}

func (p *parser) addExpr() (Expr, error) {
	l, err := p.mulExpr()
	for err == nil && (p.isOp("+") || p.isOp("-") || p.isOp("||")) {
		op := p.next().s
		var r Expr
		if r, err = p.mulExpr(); err == nil {
			l = &Binary{op, l, r}
		}
	}
	return l, err
}

func (p *parser) mulExpr() (Expr, error) {
	l, err := p.unary()
	for err == nil && (p.isOp("*") || p.isOp("/") || p.isOp("%") || p.isOp("&")) {
		op := p.next().s
		var r Expr
		if r, err = p.unary(); err == nil {
			l = &Binary{op, l, r}
		}
	}
	return l, err
}

func (p *parser) unary() (Expr, error) {
	if p.acceptOp("-") {
		x, err := p.unary()
		if lit, ok := x.(*Lit); ok && err == nil {
			switch lit.V.K {
			case KInt:
				return &Lit{IntD(-lit.V.I)}, nil
			case KFloat:
				return &Lit{FloatD(-lit.V.F)}, nil
			}
		}
		return &Unary{"-", x}, err
	}
	if p.acceptOp("+") {
		return p.unary()
	}
	x, err := p.primary()
	for err == nil {
		switch {
		case p.acceptOp("::"):
			var t string
			if t, err = p.typeName(); err == nil {
				x = &Cast{x, t}
			}
			continue
		case p.isOp("[") && p.i > 0 && (p.toks[p.i-1].s == ")" || p.toks[p.i-1].s == "]" && p.toks[p.i-1].k == tOp ||
			p.toks[p.i-1].k == tIdent && !reserved[p.toks[p.i-1].s] || p.toks[p.i-1].k == tQuoted): // (array)[i], col[i]
			p.next()
			var i Expr
			if i, err = p.expr(); err == nil {
				x, err = &Func{Name: "stask_subscript", Args: []Expr{x, i}}, p.expectOp("]")
			}
			continue
		case p.isOp(".") && p.i > 0 && p.toks[p.i-1].s == ")" && p.toks[p.i-1].k == tOp: // (record).field
			p.next()
			var f string
			if f, err = p.ident(); err == nil {
				x = &Func{Name: "stask_field", Args: []Expr{x, &Lit{TextD(f)}}}
			}
			continue
		}
		break
	}
	return x, err
}

func (p *parser) primary() (Expr, error) {
	t := p.peek()
	if p.acceptKw("default") {
		return &DefaultExpr{}, nil
	}
	switch t.k {
	case tNumber:
		p.next()
		if !strings.ContainsAny(t.s, ".eE") {
			if i, err := strconv.ParseInt(t.s, 10, 64); err == nil {
				return &Lit{IntD(i)}, nil
			}
		}
		f, err := strconv.ParseFloat(t.s, 64)
		if err != nil {
			return nil, syntaxErr(t.pos, "invalid number "+t.s)
		}
		return &Lit{FloatD(f)}, nil
	case tString:
		p.next()
		return &Lit{TextD(t.s)}, nil
	case tParam:
		p.next()
		n, err := strconv.Atoi(t.s)
		if err != nil || n < 1 || n > 65535 {
			return nil, syntaxErr(t.pos, "bad parameter $"+t.s)
		}
		return &Param{n}, nil
	case tOp:
		if t.s == "(" {
			p.next()
			if p.isKw("select") || p.isKw("with") || p.isKw("values") || p.isOp("(") && p.peekN(1).s == "select" {
				q, err := p.queryStmt()
				if err != nil {
					return nil, err
				}
				return &Subquery{Q: q}, p.expectOp(")")
			}
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if p.acceptOp(",") { // row constructor (a, b)
				rest, err := p.exprList(")")
				if err != nil {
					return nil, err
				}
				return &RowExpr{Items: append([]Expr{x}, rest...)}, nil
			}
			return x, p.expectOp(")")
		}
		if t.s == "[" { // ClickHouse array literal
			p.next()
			list, err := p.exprList("]")
			return &Func{Name: "array", Args: list}, err
		}
		return nil, p.errHere("syntax error at or near %q", t.s)
	case tQuoted:
		p.next()
		if p.isOp("(") { // "substring"(x, 7): a function named in quotes, as DBeaver writes it
			return p.funcCall(t.s)
		}
		return p.columnRest(t.s)
	case tIdent:
		switch t.s {
		case "null":
			p.next()
			return &Lit{Null}, nil
		case "true", "false":
			p.next()
			return &Lit{BoolD(t.s == "true")}, nil
		case "case":
			return p.caseExpr()
		case "cast":
			p.next()
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("as"); err != nil {
				return nil, err
			}
			ty, err := p.typeName()
			if err != nil {
				return nil, err
			}
			return &Cast{x, ty}, p.expectOp(")")
		case "exists":
			if p.peekN(1).s == "(" {
				p.next()
				p.next()
				q, err := p.queryStmt()
				if err != nil {
					return nil, err
				}
				return &Exists{Q: q}, p.expectOp(")")
			}
		case "array":
			if p.peekN(1).s == "[" {
				p.next()
				p.next()
				list, err := p.exprList("]")
				return &Func{Name: "array", Args: list}, err
			}
			if p.peekN(1).s == "(" && (p.peekN(2).s == "select" || p.peekN(2).s == "with") {
				p.next()
				p.next()
				q, err := p.queryStmt()
				if err != nil {
					return nil, err
				}
				return &Subquery{Q: q, Array: true}, p.expectOp(")")
			}
		case "interval":
			// INTERVAL '1 day', INTERVAL '2' HOUR, ClickHouse INTERVAL 2 HOUR
			if k := p.peekN(1).k; k == tString || k == tNumber {
				p.next()
				v := p.next().s
				if u := p.peek(); u.k == tIdent && intervalUnits[u.s] != [2]int64{} || u.k == tIdent && (u.s == "microsecond" || u.s == "microseconds") {
					v += " " + p.next().s
				}
				return &Cast{&Lit{TextD(v)}, "interval"}, nil
			}
		case "current_date":
			p.next()
			return &Cast{&Func{Name: "now"}, "date"}, nil
		case "timestamp", "timestamptz", "date", "boolean", "int", "integer", "bigint", "text":
			// typed literal: TIMESTAMP '2024-01-01'
			if p.peekN(1).k == tString {
				ty, err := p.typeName()
				if err != nil {
					return nil, err
				}
				s := p.next()
				return &Cast{&Lit{TextD(s.s)}, ty}, nil
			}
		case "current_timestamp", "localtimestamp":
			p.next()
			return &Func{Name: "now"}, nil
		}
		if reserved[t.s] && !((t.s == "left" || t.s == "right") && p.peekN(1).s == "(") {
			return nil, p.errHere("syntax error at or near %q", t.s)
		}
		p.next()
		if p.isOp("(") {
			return p.funcCall(t.s)
		}
		return p.columnRest(t.s)
	}
	return nil, p.errHere("syntax error at end of input")
}

func (p *parser) columnRest(first string) (Expr, error) {
	if p.acceptOp(".") {
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		if p.isOp("(") && (first == "pg_catalog" || first == "public") {
			return p.funcCall(n) // pg_catalog.set_config(...)
		}
		return &ColRef{Table: first, Name: n}, nil
	}
	return &ColRef{Name: first}, nil
}

func (p *parser) funcCall(name string) (Expr, error) {
	p.next()      // (
	switch name { // SQL-standard special forms
	case "extract":
		field := p.next().s
		if err := p.expectKw("from"); err != nil {
			return nil, err
		}
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		return &Func{Name: "date_part", Args: []Expr{&Lit{TextD(strings.ToLower(field))}, x}}, p.expectOp(")")
	case "position":
		a, err := p.addExpr()
		if err != nil {
			return nil, err
		}
		if p.acceptKw("in") {
			b, err := p.expr()
			if err != nil {
				return nil, err
			}
			return &Func{Name: "strpos", Args: []Expr{b, a}}, p.expectOp(")")
		}
		if err := p.expectOp(","); err != nil { // ClickHouse position(haystack, needle)
			return nil, err
		}
		rest, err := p.exprList(")")
		return &Func{Name: "strpos", Args: append([]Expr{a}, rest...)}, err
	case "substring", "substr":
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		if p.isKw("from") || p.isKw("for") {
			args := []Expr{x, &Lit{IntD(1)}}
			if p.acceptKw("from") {
				if args[1], err = p.expr(); err != nil {
					return nil, err
				}
			}
			if p.acceptKw("for") {
				n, err := p.expr()
				if err != nil {
					return nil, err
				}
				args = append(args, n)
			}
			return &Func{Name: "substr", Args: args}, p.expectOp(")")
		}
		if p.acceptOp(")") {
			return &Func{Name: "substr", Args: []Expr{x}}, nil
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
		rest, err := p.exprList(")")
		return &Func{Name: "substr", Args: append([]Expr{x}, rest...)}, err
	case "trim":
		fn := "btrim"
		switch {
		case p.acceptKw("both"):
		case p.acceptKw("leading"):
			fn = "ltrim"
		case p.acceptKw("trailing"):
			fn = "rtrim"
		}
		var chars, x Expr
		var err error
		if p.acceptKw("from") {
			x, err = p.expr()
		} else {
			if x, err = p.expr(); err == nil && p.acceptKw("from") {
				chars = x
				x, err = p.expr()
			}
		}
		if err != nil {
			return nil, err
		}
		args := []Expr{x}
		if chars != nil {
			args = append(args, chars)
		}
		return &Func{Name: fn, Args: args}, p.expectOp(")")
	}
	f := &Func{Name: name}
	args := func() error {
		if p.acceptOp(")") {
			return nil
		}
		if p.acceptOp("*") {
			f.Star = true
			return p.expectOp(")")
		}
		f.Distinct = p.acceptKw("distinct")
		if !f.Distinct {
			p.acceptKw("all")
		}
		for {
			x, err := p.expr()
			if err != nil {
				return err
			}
			f.Args = append(f.Args, x)
			if p.acceptOp(",") {
				continue
			}
			if p.acceptKw("order", "by") {
				if f.Order, err = p.orderList(); err != nil {
					return err
				}
			}
			return p.expectOp(")")
		}
	}
	if err := args(); err != nil {
		return nil, err
	}
	if p.isOp("(") { // ClickHouse parametric aggregate: name(params)(args)
		f.Params, f.Args, f.Distinct = f.Args, nil, false
		p.next()
		if err := args(); err != nil {
			return nil, err
		}
	}
	if name == "count" && len(f.Args) == 0 && !f.Star { // ClickHouse count()
		f.Star = true
	}
	if p.acceptKw("within", "group") {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		if err := p.expectKw("order"); err != nil {
			return nil, err
		}
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		var err error
		if f.Within, err = p.orderList(); err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("filter") {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		if err := p.expectKw("where"); err != nil {
			return nil, err
		}
		var err error
		if f.Filter, err = p.expr(); err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("over") {
		if p.isOp("(") {
			w, err := p.windowSpec()
			if err != nil {
				return nil, err
			}
			f.Over = w
		} else {
			n, err := p.ident()
			if err != nil {
				return nil, err
			}
			f.Over = &WindowSpec{Ref: n}
		}
	}
	return f, nil
}

func (p *parser) caseExpr() (Expr, error) {
	p.next()
	c := &Case{}
	var err error
	if !p.isKw("when") {
		if c.Operand, err = p.expr(); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("when") {
		var w When
		if w.Cond, err = p.expr(); err != nil {
			return nil, err
		}
		if err := p.expectKw("then"); err != nil {
			return nil, err
		}
		if w.Then, err = p.expr(); err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, w)
	}
	if len(c.Whens) == 0 {
		return nil, p.errHere("CASE requires at least one WHEN")
	}
	if p.acceptKw("else") {
		if c.Else, err = p.expr(); err != nil {
			return nil, err
		}
	}
	return c, p.expectKw("end")
}

func (p *parser) createPublication() (Stmt, error) {
	c := &CreatePublication{}
	var err error
	if c.Name, err = p.ident(); err != nil {
		return nil, err
	}
	if p.acceptKw("for") {
		if p.acceptKw("all", "tables") {
			c.AllTables = true
		} else {
			if err := p.expectKw("table"); err != nil {
				return nil, err
			}
			for {
				p.acceptKw("only")
				t, err := p.qualifiedName()
				if err != nil {
					return nil, err
				}
				c.Tables = append(c.Tables, t)
				if !p.acceptOp(",") {
					break
				}
			}
		}
	}
	if p.acceptKw("with") {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		for {
			k, err := p.ident()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp("="); err != nil {
				return nil, err
			}
			v := p.next()
			switch k {
			case "publish":
				c.Publish = v.s
			case "publish_via_partition_root":
			default:
				return nil, errf(codeSyntax, "unrecognized publication parameter: %q", k)
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// seqOpts parses the options of CREATE SEQUENCE, ALTER SEQUENCE (alter:
// RESTART too) and an IDENTITY column.
func (p *parser) seqOpts(alter bool) (SeqOpts, error) {
	var o SeqOpts
	num := func() (*int64, error) {
		neg := p.acceptOp("-")
		if !neg {
			p.acceptOp("+")
		}
		t := p.next()
		v, err := strconv.ParseInt(t.s, 10, 64)
		if t.k != tNumber || err != nil {
			return nil, syntaxErr(t.pos, "expected an integer")
		}
		if neg {
			v = -v
		}
		return &v, nil
	}
	var err error
	for {
		switch {
		case p.acceptKw("as"):
			if o.Type = p.next().s; o.Type != "smallint" && o.Type != "integer" && o.Type != "bigint" &&
				o.Type != "int" && o.Type != "int2" && o.Type != "int4" && o.Type != "int8" {
				return o, errf("22023", "sequence type must be smallint, integer, or bigint")
			}
		case p.acceptKw("increment"):
			p.acceptKw("by")
			o.Inc, err = num()
		case p.acceptKw("minvalue"):
			o.Min, err = num()
		case p.acceptKw("maxvalue"):
			o.Max, err = num()
		case p.acceptKw("no", "minvalue"):
			o.NoMin = true
		case p.acceptKw("no", "maxvalue"):
			o.NoMax = true
		case p.acceptKw("start"):
			p.acceptKw("with")
			o.Start, err = num()
		case p.acceptKw("cache"):
			_, err = num() // values are not cached per session
		case p.acceptKw("cycle"):
			o.Cycle = new(bool)
			*o.Cycle = true
		case p.acceptKw("no", "cycle"):
			o.Cycle = new(bool)
		case p.acceptKw("owned", "by"):
			// ownership comes from SERIAL/IDENTITY only
			if !p.acceptKw("none") {
				_, err = p.qualifiedName()
			}
		case alter && p.acceptKw("restart"):
			o.Restart = true
			if p.acceptKw("with") || p.peek().k == tNumber || p.isOp("-") {
				o.RestartWith, err = num()
			}
		default:
			return o, nil
		}
		if err != nil {
			return o, err
		}
	}
}

// checkDef parses CHECK (expr) [NO INHERIT].
func (p *parser) checkDef(name string) (CheckDef, error) {
	p.next()
	if err := p.expectOp("("); err != nil {
		return CheckDef{}, err
	}
	start := p.peek().pos
	if _, err := p.expr(); err != nil {
		return CheckDef{}, err
	}
	ck := CheckDef{Name: name, Expr: strings.TrimSpace(p.src[start:p.peek().pos])}
	if err := p.expectOp(")"); err != nil {
		return ck, err
	}
	p.acceptKw("no", "inherit")
	return ck, nil
}

// alterTable parses the actions of ALTER TABLE name.
func (p *parser) alterTable(name string, ifExists bool) (Stmt, error) {
	a := &AlterTable{Name: name, IfExists: ifExists}
	for {
		var act AlterAction
		var err error
		switch {
		case p.acceptKw("add"):
			var cname string
			if p.acceptKw("constraint") {
				if cname, err = p.ident(); err != nil {
					return nil, err
				}
			}
			if p.isKw("check") {
				act.Kind = "add_check"
				if act.Check, err = p.checkDef(cname); err != nil {
					return nil, err
				}
				break
			}
			if cname != "" || p.isKw("primary") || p.isKw("unique") || p.isKw("foreign") {
				return nil, errf(codeFeature, "only CHECK constraints can be added")
			}
			p.acceptKw("column")
			act.Kind, act.IfExists = "add_column", p.acceptKw("if", "not", "exists")
			if act.Col, err = p.colDef(); err != nil {
				return nil, err
			}
		case p.acceptKw("drop"):
			if p.acceptKw("constraint") {
				act.Kind, act.IfExists = "drop_constraint", p.acceptKw("if", "exists")
				if act.Column, err = p.ident(); err != nil {
					return nil, err
				}
			} else {
				p.acceptKw("column")
				act.Kind, act.IfExists = "drop_column", p.acceptKw("if", "exists")
				if act.Column, err = p.ident(); err != nil {
					return nil, err
				}
			}
			if !p.acceptKw("cascade") {
				p.acceptKw("restrict")
			}
		case p.acceptKw("rename"):
			if p.acceptKw("to") {
				act.Kind = "rename_table"
				if act.NewName, err = p.qualifiedName(); err != nil {
					return nil, err
				}
				break
			}
			if p.acceptKw("constraint") {
				return nil, errf(codeFeature, "RENAME CONSTRAINT is not supported")
			}
			p.acceptKw("column")
			act.Kind = "rename_column"
			if act.Column, err = p.ident(); err != nil {
				return nil, err
			}
			if err := p.expectKw("to"); err != nil {
				return nil, err
			}
			if act.NewName, err = p.ident(); err != nil {
				return nil, err
			}
		case p.acceptKw("alter"):
			p.acceptKw("column")
			if act.Column, err = p.ident(); err != nil {
				return nil, err
			}
			switch {
			case p.acceptKw("set", "default"):
				act.Kind = "set_default"
				start := p.peek().pos
				if _, err := p.addExpr(); err != nil {
					return nil, err
				}
				act.Expr = strings.TrimSpace(p.src[start:p.peek().pos])
			case p.acceptKw("drop", "default"):
				act.Kind = "drop_default"
			case p.acceptKw("set", "not", "null"):
				act.Kind = "set_not_null"
			case p.acceptKw("drop", "not", "null"):
				act.Kind = "drop_not_null"
			case p.acceptKw("set", "data", "type"), p.acceptKw("type"):
				act.Kind = "set_type"
				if _, err := p.typeName(); err != nil {
					return nil, err
				}
				if p.acceptKw("using") {
					if _, err := p.expr(); err != nil {
						return nil, err
					}
				}
			default:
				return nil, p.errHere("syntax error at or near %q", p.peek().s)
			}
		case p.acceptKw("owner", "to"):
			act.Kind = "owner" // no roles yet: accepted and ignored (pg_dump writes it)
			p.next()
		default:
			return nil, p.errHere("syntax error at or near %q", p.peek().s)
		}
		a.Actions = append(a.Actions, act)
		if !p.acceptOp(",") {
			return a, nil
		}
	}
}
