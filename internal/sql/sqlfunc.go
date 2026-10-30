package sql

import (
	"encoding/json"
	"strconv"
	"strings"

	"stask/internal/core"
)

// User-defined SQL functions (task crmcompat):
//
//	CREATE [OR REPLACE] FUNCTION name(argtypes) RETURNS type
//	    AS $$ SELECT expression of $1, $2 ... $$ LANGUAGE sql [IMMUTABLE | STABLE | STRICT ...]
//	DROP FUNCTION [IF EXISTS] name [(argtypes)]
//
// Only LANGUAGE sql with a single SELECT of one expression is supported
// (what Jmix applications use for helpers such as unaccent_lower); any other
// language is refused with 0A000. A call is expanded where it is parsed: the
// stored expression is parsed again with $n replaced by the call's argument
// expressions, so the planner sees ordinary expressions and indexes on them
// are matched as usual. The definition is a row of the object catalog
// (core/objcat.go, kind "func", name "<name>/<number of arguments>"), so it is
// durable and replicated like other DDL. Overloads are told apart by the
// number of arguments only; a definition whose body calls its own name with
// the same number of arguments (PostgreSQL's way of adding a varchar overload
// of a text function) is accepted and ignored when the name is already defined.

// CreateFunction is CREATE [OR REPLACE] FUNCTION.
type CreateFunction struct {
	Name      string
	OrReplace bool
	NArgs     int
	Ret       string
	Lang      string
	Body      string // the SQL text of the body, as given
}

// DropFunction is DROP FUNCTION.
type DropFunction struct {
	Name     string
	IfExists bool
	NArgs    int // -1: any
}

func (*CreateFunction) stmt() {}
func (*DropFunction) stmt()   {}

type funcDef struct {
	Expr string `json:"expr"`
	Ret  string `json:"ret,omitempty"`
}

func funcObjName(name string, nargs int) string { return name + "/" + strconv.Itoa(nargs) }

// argTypeList parses (type, name type, ...) and returns the number of arguments.
func (p *parser) argTypeList() (int, error) {
	if err := p.expectOp("("); err != nil {
		return 0, err
	}
	if p.acceptOp(")") {
		return 0, nil
	}
	n, depth, any := 1, 0, false
	for {
		t := p.next()
		switch {
		case t.k == tEOF:
			return 0, p.errHere("syntax error at end of input")
		case t.k == tOp && t.s == "(":
			depth++
		case t.k == tOp && t.s == ")":
			if depth == 0 {
				return n, nil
			}
			depth--
		case t.k == tOp && t.s == "," && depth == 0:
			n++
		}
		any = true
	}
	_ = any
	return n, nil
}

func (p *parser) createFunction() (Stmt, bool, error) {
	save := p.i
	replace := p.acceptKw("or", "replace")
	if !p.acceptKw("function") {
		p.i = save
		return nil, false, nil
	}
	c := &CreateFunction{OrReplace: replace}
	name, err := p.qualifiedName()
	if err != nil {
		return nil, true, err
	}
	c.Name = bareName(name)
	if c.NArgs, err = p.argTypeList(); err != nil {
		return nil, true, err
	}
	for {
		switch {
		case p.acceptKw("returns"):
			start := p.i
			for !(p.peek().k == tEOF || p.peek().k == tIdent && (p.peek().s == "as" || p.peek().s == "language" ||
				p.peek().s == "immutable" || p.peek().s == "stable" || p.peek().s == "volatile" || p.peek().s == "strict" ||
				p.peek().s == "parallel" || p.peek().s == "cost" || p.peek().s == "leakproof" || p.peek().s == "security") || p.isOp(";")) {
				p.next()
			}
			var w []string
			for _, t := range p.toks[start:p.i] {
				w = append(w, t.s)
			}
			c.Ret = strings.Join(w, " ")
		case p.acceptKw("as"):
			t := p.next()
			if t.k != tString {
				return nil, true, p.errHere("syntax error at or near %q", t.s)
			}
			c.Body = t.s
		case p.acceptKw("language"):
			if c.Lang, err = p.ident(); err != nil {
				return nil, true, err
			}
		case p.acceptKw("immutable"), p.acceptKw("stable"), p.acceptKw("volatile"), p.acceptKw("strict"), p.acceptKw("leakproof"),
			p.acceptKw("called", "on", "null", "input"), p.acceptKw("returns", "null", "on", "null", "input"):
		case p.acceptKw("parallel"):
			p.next()
		case p.acceptKw("cost"):
			p.next()
		case p.acceptKw("security"):
			p.next()
		default:
			if c.Lang == "" || c.Body == "" {
				return nil, true, errf(codeSyntax, "CREATE FUNCTION needs AS and LANGUAGE")
			}
			return c, true, nil
		}
	}
}

func (p *parser) dropFunction() (Stmt, bool, error) {
	if !p.acceptKw("function") {
		return nil, false, nil
	}
	d := &DropFunction{IfExists: p.acceptKw("if", "exists"), NArgs: -1}
	name, err := p.qualifiedName()
	if err != nil {
		return nil, true, err
	}
	d.Name = bareName(name)
	if p.isOp("(") {
		if d.NArgs, err = p.argTypeList(); err != nil {
			return nil, true, err
		}
	}
	if !p.acceptKw("cascade") {
		p.acceptKw("restrict")
	}
	return d, true, nil
}

// bodyExpr is the expression of "SELECT expr [;]".
func bodyExpr(body string) (string, bool) {
	b := strings.TrimSpace(body)
	b = strings.TrimSpace(strings.TrimSuffix(b, ";"))
	if len(b) < 7 || !strings.EqualFold(b[:6], "select") || b[6] != ' ' && b[6] != '\n' && b[6] != '\t' && b[6] != '\r' && b[6] != '(' {
		return "", false
	}
	return strings.TrimSpace(b[6:]), true
}

func (s *Session) createFunction(c *CreateFunction) (*Result, error) {
	res := &Result{Tag: "CREATE FUNCTION"}
	if !strings.EqualFold(c.Lang, "sql") {
		return nil, errf(codeFeature, "LANGUAGE %s functions are not supported (only LANGUAGE sql with a single SELECT expression)", strings.ToLower(c.Lang))
	}
	expr, ok := bodyExpr(c.Body)
	if !ok {
		return nil, errf(codeFeature, "a SQL function body must be a single SELECT of one expression")
	}
	if strings.EqualFold(c.Ret, "trigger") || strings.EqualFold(c.Ret, "void") || strings.HasPrefix(strings.ToLower(c.Ret), "table") || strings.HasPrefix(strings.ToLower(c.Ret), "setof") {
		return nil, errf(codeFeature, "functions returning %s are not supported", c.Ret)
	}
	// the body must parse as an expression of the parameters
	sub, err := s.expandParser(expr, nil, 0)
	if err != nil {
		return nil, AsError(err)
	}
	if _, err := sub.exprAll(); err != nil {
		return nil, AsError(err)
	}
	key := funcObjName(c.Name, c.NArgs)
	if old, exists := s.E.Obj("func", key); exists {
		self := strings.Contains(strings.ToLower(expr), strings.ToLower(c.Name)+"(")
		if self {
			_ = old
			res.Notice = "function " + c.Name + " with this number of arguments already exists, skipping"
			return res, nil
		}
		if !c.OrReplace {
			return nil, errf("42723", "function %q already exists with same argument types", c.Name)
		}
	}
	data, _ := json.Marshal(funcDef{Expr: expr, Ret: c.Ret})
	if err := s.E.PutObj(core.Obj{Kind: "func", Name: key, Data: string(data)}, true); err != nil {
		return nil, AsError(err)
	}
	return res, nil
}

func (s *Session) dropFunction(d *DropFunction) (*Result, error) {
	res := &Result{Tag: "DROP FUNCTION"}
	var ops []core.ObjOp
	for _, o := range s.E.Objs("func") {
		n, a, _ := strings.Cut(o.Name, "/")
		if n == d.Name && (d.NArgs < 0 || strconv.Itoa(d.NArgs) == a) {
			ops = append(ops, core.ObjOp{O: core.Obj{Kind: "func", Name: o.Name}, Delete: true})
		}
	}
	if len(ops) == 0 {
		if d.IfExists {
			res.Notice = "function " + d.Name + " does not exist, skipping"
			return res, nil
		}
		return nil, errf(codeUndefinedFunction, "function %s does not exist", d.Name)
	}
	return res, asErr(s.E.WriteObjs(ops))
}

const maxFuncDepth = 16

// expandParser is a parser over a function body whose $n are replaced by args.
func (s *Session) expandParser(expr string, args []Expr, depth int) (*parser, error) {
	toks, err := lex(expr)
	if err != nil {
		return nil, err
	}
	return &parser{toks: catalogTokens(toks), src: expr, sess: s, subst: args, funcDepth: depth, substOn: true}, nil
}

// exprAll parses one expression and requires the end of input.
func (p *parser) exprAll() (Expr, error) {
	x, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.peek().k != tEOF {
		return nil, p.errHere("syntax error at or near %q", p.peek().s)
	}
	return x, nil
}

// expandUserFunc replaces a call of a user-defined SQL function by its body.
func (p *parser) expandUserFunc(f *Func) (Expr, bool, error) {
	if p.sess == nil || p.sess.E == nil || f.Star || f.Distinct || f.Over != nil || f.Filter != nil || len(f.Order) > 0 || f.Params != nil {
		return nil, false, nil
	}
	o, ok := p.sess.E.Obj("func", funcObjName(f.Name, len(f.Args)))
	if !ok {
		return nil, false, nil
	}
	if p.funcDepth >= maxFuncDepth {
		return nil, true, errf("54001", "stack depth limit exceeded in function %s", f.Name)
	}
	var d funcDef
	if err := json.Unmarshal([]byte(o.Data), &d); err != nil {
		return nil, true, err
	}
	sub, err := p.sess.expandParser(d.Expr, f.Args, p.funcDepth+1)
	if err != nil {
		return nil, true, err
	}
	x, err := sub.exprAll()
	return x, true, err
}
