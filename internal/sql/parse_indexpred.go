package sql

import (
	"strings"

	"stask/internal/schema"
)

// indexElemTail parses what may follow an index element: an operator class, ASC/DESC,
// NULLS FIRST/LAST. COLLATE is dropped earlier, by catalogTokens. An operator class is
// accepted and ignored (the pattern classes are byte order too).
func (p *parser) indexElemTail() error {
	for {
		switch {
		case p.acceptKw("asc"), p.acceptKw("desc"):
		case p.acceptKw("nulls"):
			if !p.acceptKw("first") {
				p.acceptKw("last")
			}
		default:
			t := p.peek()
			if (t.k == tIdent || t.k == tQuoted) && strings.HasSuffix(t.s, "_ops") {
				p.next()
				continue
			}
			return nil
		}
	}
}

// skipParens consumes a balanced ( ... ) group; the current token must be "(".
func (p *parser) skipParens() error {
	if err := p.expectOp("("); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t := p.next()
		switch {
		case t.k == tEOF:
			return p.errHere("syntax error: unbalanced parentheses")
		case t.k == tOp && t.s == "(":
			depth++
		case t.k == tOp && t.s == ")":
			depth--
		}
	}
	return nil
}

// indexPredicate parses the WHERE of a partial index after the keyword (see
// schema/indexpred.go for the accepted forms).
func (p *parser) indexPredicate() ([]schema.IndexPred, error) {
	var out []schema.IndexPred
	for {
		terms, err := p.indexPredTerm()
		if err != nil {
			return nil, err
		}
		out = append(out, terms...)
		if !p.acceptKw("and") {
			if t := p.peek(); t.k != tEOF && !(t.k == tOp && (t.s == ";" || t.s == ")")) && !p.isKw("include") && !p.isKw("with") && !p.isKw("tablespace") {
				return nil, predUnsupported()
			}
			return out, nil
		}
	}
}

func predUnsupported() error {
	return errf(codeFeature, "partial index predicates are limited to AND of: col IS [NOT] NULL, col <op> literal (=, <>, <, <=, >, >=), col, NOT col")
}

func (p *parser) indexPredTerm() ([]schema.IndexPred, error) {
	if p.acceptOp("(") {
		terms, err := p.indexPredicate()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, predUnsupported()
		}
		return terms, nil
	}
	neg := p.acceptKw("not")
	col, err := p.ident()
	if err != nil {
		return nil, predUnsupported()
	}
	if p.isOp(".") {
		return nil, predUnsupported()
	}
	one := func(op, lit string) ([]schema.IndexPred, error) {
		return []schema.IndexPred{{Col: col, Op: op, Lit: lit}}, nil
	}
	switch {
	case p.acceptKw("is"):
		if neg {
			return nil, predUnsupported()
		}
		not := p.acceptKw("not")
		switch {
		case p.acceptKw("null"):
			if not {
				return one("notnull", "")
			}
			return one("null", "")
		case !not && p.acceptKw("true"):
			return one("eq", "true")
		case !not && p.acceptKw("false"):
			return one("eq", "false")
		}
		return nil, predUnsupported()
	case p.isOp("=") || p.isOp("<>") || p.isOp("!=") || p.isOp("<") || p.isOp("<=") || p.isOp(">") || p.isOp(">="):
		if neg {
			return nil, predUnsupported()
		}
		op := map[string]string{"=": "eq", "<>": "ne", "!=": "ne", "<": "lt", "<=": "le", ">": "gt", ">=": "ge"}[p.next().s]
		t := p.next()
		lit := t.s
		switch {
		case t.k == tNumber, t.k == tString:
		case t.k == tIdent && (t.s == "true" || t.s == "false"):
		case t.k == tOp && t.s == "-" && p.peek().k == tNumber:
			lit = "-" + p.next().s
		default:
			return nil, predUnsupported()
		}
		if p.acceptOp("::") { // 'x'::text
			if _, err := p.ident(); err != nil {
				return nil, predUnsupported()
			}
		}
		return one(op, lit)
	}
	if neg {
		return one("eq", "false")
	}
	return one("eq", "true")
}
