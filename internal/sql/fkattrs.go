package sql

import (
	"stask/internal/core"
	"stask/internal/schema"
)

// Constraint attributes shared by the constraint parsers: [NOT] DEFERRABLE,
// INITIALLY DEFERRED | IMMEDIATE, NOT VALID. Foreign keys honour them (core/fkmore.go).
// UNIQUE and PRIMARY KEY accept DEFERRABLE but check immediately (docs/14, compat-ddl):
// a transient duplicate inside a transaction is still refused.

// constraintAttr parses one attribute into c; handled=false if the next tokens are not one.
func (p *parser) constraintAttr(c *ConstraintDef) (handled bool, err error) {
	switch {
	case p.acceptKw("not", "deferrable"):
		c.notDeferrable = true
	case p.acceptKw("deferrable"):
		c.Deferrable = true
	case p.acceptKw("initially", "deferred"):
		c.Deferrable, c.InitiallyDeferred = true, true
	case p.acceptKw("initially", "immediate"):
	case p.acceptKw("not", "valid"):
		c.NotValid = true
	default:
		return false, nil
	}
	if c.notDeferrable && c.Deferrable {
		return true, errf(codeSyntax, "conflicting constraint properties")
	}
	if c.notDeferrable && c.InitiallyDeferred {
		return true, errf(codeSyntax, "constraint declared INITIALLY DEFERRED must be DEFERRABLE")
	}
	return true, nil
}

// skipConstraintAttrs accepts the attributes after a UNIQUE or PRIMARY KEY constraint.
func (p *parser) skipConstraintAttrs() error {
	var c ConstraintDef
	for {
		ok, err := p.constraintAttr(&c)
		if err != nil || !ok {
			return err
		}
	}
}

// SetConstraints is SET CONSTRAINTS { ALL | name, ... } { DEFERRED | IMMEDIATE }.
type SetConstraints struct {
	Names    []string // nil: ALL
	Deferred bool
}

func (*SetConstraints) stmt() {}

func (p *parser) setConstraints() (Stmt, error) {
	st := &SetConstraints{}
	if !p.acceptKw("all") {
		for {
			n, err := p.qualifiedName()
			if err != nil {
				return nil, err
			}
			st.Names = append(st.Names, bareName(n))
			if !p.acceptOp(",") {
				break
			}
		}
	}
	switch {
	case p.acceptKw("deferred"):
		st.Deferred = true
	case p.acceptKw("immediate"):
	default:
		return nil, p.errHere("syntax error at or near %q", p.peek().s)
	}
	return st, nil
}

func (s *Session) setConstraints(st *SetConstraints) (*Result, error) {
	if !s.explicit {
		return &Result{Tag: "SET CONSTRAINTS", Notice: "SET CONSTRAINTS can only be used in transaction blocks"}, nil
	}
	if err := s.tx.SetConstraints(st.Names, st.Deferred); err != nil {
		s.markFailed()
		if fe := fkErrorOf(err); fe != nil {
			return nil, fe
		}
		return nil, AsError(err)
	}
	return &Result{Tag: "SET CONSTRAINTS"}, nil
}

func init() {
	// ON DELETE/UPDATE SET DEFAULT: the engine asks the SQL layer for a column's default
	core.FKDefault = func(sch *schema.Table, col string) (schema.Value, error) {
		i := colIndex(sch, col)
		if i < 0 {
			return schema.Value{Null: true}, nil
		}
		text := columnDefaultText(&sch.Columns[i])
		if text == "" {
			return schema.Value{Null: true}, nil
		}
		x, err := parseDefault(text)
		if err != nil {
			return schema.Value{}, err
		}
		d, err := (&evalCtx{now: nowMicros()}).eval(x)
		if err != nil {
			return schema.Value{}, err
		}
		return (*Session)(nil).valCol(&sch.Columns[i], d)
	}
}
