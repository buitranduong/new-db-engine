package sql

// CREATE INDEX, DROP INDEX, and the UNIQUE and REFERENCES constraints
// (docs/05 §18). The syntax follows PostgreSQL's; what STASK does not do is
// refused with 0A000 rather than ignored: partial and expression indexes,
// access methods other than btree, INCLUDE, deferrable constraints, MATCH
// FULL, and ON UPDATE / ON DELETE SET DEFAULT.

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
	Cols        []string
	Unique      bool
	IfNotExists bool
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
		if m != "btree" {
			return nil, true, errf(codeFeature, "index access method %q is not supported (only btree)", m)
		}
	}
	if err = p.expectOp("("); err != nil {
		return nil, true, err
	}
	for {
		if p.isOp("(") {
			return nil, true, errf(codeFeature, "indexes on expressions are not supported")
		}
		col, err := p.ident()
		if err != nil {
			return nil, true, err
		}
		if p.isOp("(") || p.isOp(".") {
			return nil, true, errf(codeFeature, "indexes on expressions are not supported")
		}
		c.Cols = append(c.Cols, col)
		// the order is accepted: an index is read forwards or backwards
		if !p.acceptKw("asc") {
			p.acceptKw("desc")
		}
		if p.acceptKw("nulls") {
			if !p.acceptKw("first") {
				p.acceptKw("last")
			}
		}
		if !p.acceptOp(",") {
			break
		}
	}
	if err = p.expectOp(")"); err != nil {
		return nil, true, err
	}
	switch {
	case p.isKw("where"):
		return nil, true, errf(codeFeature, "partial indexes are not supported")
	case p.isKw("include"):
		return nil, true, errf(codeFeature, "INCLUDE columns are not supported")
	case p.isKw("with"):
		return nil, true, errf(codeFeature, "index storage parameters are not supported")
	}
	return c, true, nil
}

func (p *parser) dropIndex() (Stmt, error) {
	d := &DropIndex{}
	p.acceptKw("concurrently")
	d.IfExists = p.acceptKw("if", "exists")
	var err error
	if d.Names, err = p.tableList(); err != nil {
		return nil, err
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
