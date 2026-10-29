package sql

// columnRest3 finishes a three-part column reference schema.table.column;
// table was read and the next token is the second ".". The schema part is
// not checked (STASK has one schema namespace for relations), as for the
// two-part form schema.table in FROM. SQLAlchemy 2.0 writes
// pg_catalog.pg_class.relname.
func (p *parser) columnRest3(table string) (Expr, error) {
	p.next() // .
	if p.isOp("*") {
		return nil, p.errHere("syntax error at or near %q", "*")
	}
	n, err := p.ident()
	if err != nil {
		return nil, err
	}
	return &ColRef{Table: table, Name: n}, nil
}
