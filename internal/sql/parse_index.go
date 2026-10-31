package sql

import (
	"slices"

	"stask/internal/schema"
)

// CREATE INDEX, DROP INDEX, and the UNIQUE and REFERENCES constraints
// (docs/05 §18). The syntax follows PostgreSQL's; what STASK does not do is
// refused with 0A000 rather than ignored: partial index predicates beyond the closed form
// of schema/indexpred.go, expressions other than f(column),
// access methods other than btree (hash and brin run as btree), deferrable constraints, MATCH
// FULL, and ON UPDATE / ON DELETE SET DEFAULT. An index element may be an
// immutable function of one column (schema/indexexpr.go).

// ConstraintDef is a UNIQUE or FOREIGN KEY constraint, from a column or a
// table constraint.
type ConstraintDef struct {
	Kind     string // "unique" or "foreign"
	Name     string // empty: generated
	Cols     []string
	RefTable string
	RefCols  []string // empty: the primary key of RefTable
	OnDelete string   // "no action" (default), "restrict", "cascade", "set null"
}

// CreateIndex is CREATE [UNIQUE] INDEX.
type CreateIndex struct {
	Name        string // empty: generated
	Table       string
	Cols        []string // the column of each element
	Colls       []string // the COLLATE name of each element ("" for none); nil if no element has one
	Exprs       []string // the function applied to it (canonical name), "" for none; nil if no element has one
	Unique      bool
	IfNotExists bool
	Where       []schema.IndexPred // partial index predicate
}

// DropIndex is DROP INDEX.
type DropIndex struct {
	Names    []string
	IfExists bool
}

func (*CreateIndex) stmt() {}
func (*DropIndex) stmt()   {}

// createIndex parses CREATE [UNIQUE] INDEX after CREATE; ok=false (with
// nothing consumed) if the statement is not one.
func (p *parser) createIndex() (st Stmt, ok bool, err error) {
	save := p.i
	unique := p.acceptKw("unique")
	if !p.acceptKw("index") {
		p.i = save
		return nil, false, nil
	}
	c := &CreateIndex{Unique: unique}
	p.acceptKw("concurrently") // accepted: the build blocks writers to the table instead
	c.IfNotExists = p.acceptKw("if", "not", "exists")
	if !p.isKw("on") {
		if c.Name, err = p.ident(); err != nil {
			return nil, true, err
		}
	}
	if err = p.expectKw("on"); err != nil {
		return nil, true, err
	}
	p.acceptKw("only")
	if c.Table, err = p.qualifiedName(); err != nil {
		return nil, true, err
	}
	if p.acceptKw("using") {
		m, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		if m == "hash" || m == "brin" { // equality and range lookups work through the one B-tree
			m = "btree"
		}
		if m != "btree" {
			return nil, true, errf(codeFeature, "index access method %q is not supported (only btree)", m)
		}
	}
	if err = p.expectOp("("); err != nil {
		return nil, true, err
	}
	for {
		paren := p.acceptOp("(") // CREATE INDEX ON t ((lower(x))) is PostgreSQL's spelling
		col, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		fn := ""
		if p.acceptOp("(") {
			f, ok := schema.CanonExprFunc(col)
			if !ok {
				return nil, true, errf(codeFeature, "function %s cannot be used in an index expression (lower, upper, trim, btrim, ltrim, rtrim, length, char_length, abs on one column)", col)
			}
			fn = f
			inner := p.acceptOp("(") // lower((name)::text): the cast PostgreSQL writes
			if col, err = p.ident(); err != nil || p.isOp(".") {
				return nil, true, errf(codeFeature, "indexes on expressions are limited to a function of one column")
			}
			if inner {
				if err = p.expectOp(")"); err != nil {
					return nil, true, errf(codeFeature, "indexes on expressions are limited to a function of one column")
				}
			}
			for p.acceptOp("::") { // the argument cast is ignored: lower and friends take text
				if _, err = p.ident(); err != nil {
					return nil, true, err
				}
				p.acceptKw("varying")
			}
			if err = p.expectOp(")"); err != nil {
				return nil, true, errf(codeFeature, "indexes on expressions are limited to a function of one column")
			}
		} else if p.isOp(".") {
			return nil, true, errf(codeFeature, "indexes on expressions are not supported")
		}
		if paren {
			if err = p.expectOp(")"); err != nil {
				return nil, true, errf(codeFeature, "indexes on expressions are limited to a function of one column")
			}
		}
		c.Cols = append(c.Cols, col)
		c.Exprs = append(c.Exprs, fn)
		coll, err := p.lastCollName()
		if err != nil {
			return nil, true, err
		}
		c.Colls = append(c.Colls, coll)
		// the order is accepted: an index is read forwards or backwards
		if err = p.indexElemTail(); err != nil {
			return nil, true, err
		}
		if !p.acceptOp(",") {
			break
		}
	}
	if err = p.expectOp(")"); err != nil {
		return nil, true, err
	}
	if !slices.ContainsFunc(c.Exprs, func(e string) bool { return e != "" }) {
		c.Exprs = nil
	}
	if !slices.ContainsFunc(c.Colls, func(e string) bool { return e != "" }) {
		c.Colls = nil
	}
	for { // INCLUDE and storage parameters do not change results: accepted, not stored
		switch {
		case p.acceptKw("include"):
			if err = p.skipParens(); err != nil {
				return nil, true, err
			}
		case p.acceptKw("with"):
			if err = p.skipParens(); err != nil {
				return nil, true, err
			}
		case p.acceptKw("tablespace"):
			if _, err = p.ident(); err != nil {
				return nil, true, err
			}
		case p.acceptKw("where"):
			if c.Where, err = p.indexPredicate(); err != nil {
				return nil, true, err
			}
		default:
			return c, true, nil
		}
	}
}

func (p *parser) dropIndex() (Stmt, error) {
	d := &DropIndex{}
	p.acceptKw("concurrently")
	d.IfExists = p.acceptKw("if", "exists")
	var err error
	if d.Names, err = p.tableList(); err != nil {
		return nil, err
	}
	for i, n := range d.Names { // indexes have one namespace here
		d.Names[i] = bareName(n)
	}
	if !p.acceptKw("cascade") {
		p.acceptKw("restrict")
	}
	return d, nil
}

func (p *parser) identList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
		if p.acceptOp(")") {
			return out, nil
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
	}
}

// uniqueDef parses the rest of a table constraint UNIQUE (cols).
func (p *parser) uniqueDef(name string) (*ConstraintDef, error) {
	if p.acceptKw("nulls") { // NULLS [NOT] DISTINCT
		if p.acceptKw("not", "distinct") {
			return nil, errf(codeFeature, "UNIQUE NULLS NOT DISTINCT is not supported")
		}
		p.acceptKw("distinct")
	}
	cols, err := p.identList()
	if err != nil {
		return nil, err
	}
	return &ConstraintDef{Kind: "unique", Name: name, Cols: cols}, nil
}

// referencesDef parses REFERENCES tbl [(cols)] [options] after the keyword;
// cols are the referencing columns.
func (p *parser) referencesDef(name string, cols []string) (*ConstraintDef, error) {
	c := &ConstraintDef{Kind: "foreign", Name: name, Cols: cols, OnDelete: "no action"}
	var err error
	if c.RefTable, err = p.qualifiedName(); err != nil {
		return nil, err
	}
	if p.isOp("(") {
		if c.RefCols, err = p.identList(); err != nil {
			return nil, err
		}
	}
	action := func(update bool) (string, error) {
		switch {
		case p.acceptKw("no", "action"):
			return "no action", nil
		case p.acceptKw("restrict"):
			return "restrict", nil
		case p.acceptKw("cascade"):
			if update {
				return "", errf(codeFeature, "ON UPDATE CASCADE is not supported")
			}
			return "cascade", nil
		case p.acceptKw("set", "null"):
			if update {
				return "", errf(codeFeature, "ON UPDATE SET NULL is not supported")
			}
			return "set null", nil
		case p.isKw("set"):
			return "", errf(codeFeature, "SET DEFAULT referential actions are not supported")
		}
		return "", p.errHere("syntax error at or near %q", p.peek().s)
	}
	for {
		switch {
		case p.acceptKw("match", "simple"):
		case p.isKw("match"):
			return nil, errf(codeFeature, "MATCH FULL is not supported")
		case p.acceptKw("on", "delete"):
			if c.OnDelete, err = action(false); err != nil {
				return nil, err
			}
		case p.acceptKw("on", "update"):
			if _, err = action(true); err != nil {
				return nil, err
			}
		case p.acceptKw("not", "deferrable"), p.acceptKw("deferrable", "initially", "immediate"), p.acceptKw("initially", "immediate"):
		case p.isKw("deferrable"), p.isKw("initially"):
			return nil, errf(codeFeature, "deferrable constraints are not supported")
		default:
			return c, nil
		}
	}
}

// foreignDef parses the rest of a table constraint FOREIGN KEY (cols) REFERENCES ...
func (p *parser) foreignDef(name string) (*ConstraintDef, error) {
	if err := p.expectKw("key"); err != nil {
		return nil, err
	}
	cols, err := p.identList()
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("references"); err != nil {
		return nil, err
	}
	return p.referencesDef(name, cols)
}
