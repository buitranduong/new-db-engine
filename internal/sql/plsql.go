package sql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"stask/internal/core"
)

// CREATE FUNCTION / CREATE PROCEDURE / DO / CALL with a small subset of PL/pgSQL (docs/14
// appendix P). What runs: DECLARE of scalar variables, assignment (:=), IF / ELSIF / ELSE,
// PERFORM, RETURN, RAISE (NOTICE / WARNING / EXCEPTION with %), NULL, EXECUTE 'sql'
// [INTO v] [USING ...], any SQL statement (with SELECT/RETURNING ... INTO v), nested
// BEGIN .. END blocks and EXCEPTION WHEN <condition> THEN handlers (a savepoint per block
// that has handlers). FOUND, SQLSTATE and SQLERRM exist. LANGUAGE sql bodies are a list of
// statements whose last SELECT gives the result. Not supported (0A000 at CREATE/DO time):
// loops, CASE statements, cursors, GET DIAGNOSTICS, RETURN NEXT/QUERY, set-returning and
// trigger functions (a trigger function can be created but not attached), OUT parameters,
// default argument values, overloading by argument types (the name is the identity).
//
// Variables reach SQL as $n parameters (the identifier is replaced by a placeholder), so a
// variable that has the name of a column in the same statement wins over the column; PostgreSQL
// would report the clash as an error. Functions are stored in the object catalog (kind
// "func") and are called from expressions with the arguments evaluated first.

// CreateFunc is CREATE [OR REPLACE] FUNCTION / PROCEDURE.
type CreateFunc struct {
	Def       funcDef
	OrReplace bool
}

// DropFunc is DROP FUNCTION / PROCEDURE.
type DropFunc struct {
	Name     string
	IfExists bool
}

// DoStmt is DO [LANGUAGE x] 'body'.
type DoStmt struct{ Body string }

// CallStmt is CALL name(args).
type CallStmt struct{ Text string } // the call as "SELECT name(args)"

func (*CreateFunc) stmt() {}
func (*DropFunc) stmt()   {}
func (*DoStmt) stmt()     {}
func (*CallStmt) stmt()   {}

type funcArg struct {
	Name string `json:"n,omitempty"`
	Type string `json:"t"`
}

// funcDef is the stored definition.
type funcDef struct {
	Name      string    `json:"name"`
	Args      []funcArg `json:"args,omitempty"`
	Ret       string    `json:"ret"`  // internal type name, "void", "trigger"
	Lang      string    `json:"lang"` // sql, plpgsql
	Body      string    `json:"body"`
	Procedure bool      `json:"proc,omitempty"`
}

// ---- parsing ----

func (p *parser) createFunction() (Stmt, bool, error) {
	save := p.i
	replace := p.acceptKw("or", "replace")
	proc := false
	switch {
	case p.acceptKw("function"):
	case p.acceptKw("procedure"):
		proc = true
	default:
		p.i = save
		return nil, false, nil
	}
	c := &CreateFunc{OrReplace: replace}
	names, err := p.dottedName()
	if err != nil {
		return nil, true, err
	}
	c.Def.Name = names[len(names)-1]
	c.Def.Procedure = proc
	if err := p.expectOp("("); err != nil {
		return nil, true, err
	}
	for !p.acceptOp(")") {
		if len(c.Def.Args) > 0 {
			if err := p.expectOp(","); err != nil {
				return nil, true, err
			}
		}
		switch {
		case p.isKw("out"), p.isKw("inout"), p.isKw("variadic"):
			return nil, true, errf(codeFeature, "%s parameters are not supported", strings.ToUpper(p.peek().s))
		}
		p.acceptKw("in")
		var a funcArg
		t := p.peek()
		nt := p.peekN(1)
		if t.k == tIdent || t.k == tQuoted {
			// "name type" or just "type": a name is followed by another word
			if (nt.k == tIdent || nt.k == tQuoted) && !(nt.k == tIdent && (nt.s == "precision" || nt.s == "varying")) {
				a.Name = t.s
				p.next()
			}
		}
		ty, err := p.typeName()
		if err != nil {
			return nil, true, err
		}
		a.Type = ty
		if p.isKw("default") || p.isOp("=") {
			return nil, true, errf(codeFeature, "default values of function arguments are not supported")
		}
		c.Def.Args = append(c.Def.Args, a)
	}
	c.Def.Ret = "void"
	var bodies []string
	for p.peek().k != tEOF && !p.isOp(";") {
		switch {
		case p.acceptKw("returns"):
			switch {
			case p.isKw("setof"), p.isKw("table"):
				return nil, true, errf(codeFeature, "set-returning functions are not supported")
			case p.acceptKw("void"):
				c.Def.Ret = "void"
			case p.acceptKw("trigger"):
				c.Def.Ret = "trigger"
			default:
				if c.Def.Ret, err = p.typeName(); err != nil {
					return nil, true, err
				}
			}
		case p.acceptKw("language"):
			t := p.next()
			c.Def.Lang = strings.ToLower(t.s)
		case p.acceptKw("as"):
			for {
				s, err := p.strLit()
				if err != nil {
					return nil, true, err
				}
				bodies = append(bodies, s)
				if !p.acceptOp(",") {
					break
				}
			}
		case p.acceptKw("cost"), p.acceptKw("rows"):
			p.next()
		case p.acceptKw("parallel"):
			p.next()
		case p.acceptKw("set"):
			for p.peek().k != tEOF && !p.isOp(";") && !p.isKw("as") && !p.isKw("language") {
				p.next()
			}
		case p.acceptKw("security"):
			p.next()
		case p.acceptKw("external"):
		case p.acceptKw("called"), p.acceptKw("returns", "null"):
			for p.acceptKw("on") || p.acceptKw("null") || p.acceptKw("input") {
			}
		default:
			// IMMUTABLE STABLE VOLATILE STRICT LEAKPROOF [NOT] WINDOW and the like
			p.next()
		}
	}
	if len(bodies) == 0 {
		return nil, true, errf("42P13", "no function body specified")
	}
	c.Def.Body = bodies[0]
	if c.Def.Lang == "" {
		return nil, true, errf("42P13", "no language specified")
	}
	if c.Def.Lang != "sql" && c.Def.Lang != "plpgsql" {
		return nil, true, errf(codeFeature, "language %q is not supported (sql and plpgsql only)", c.Def.Lang)
	}
	return c, true, nil
}

func (p *parser) dropFunction() (Stmt, bool, error) {
	if !p.acceptKw("function") && !p.acceptKw("procedure") {
		return nil, false, nil
	}
	d := &DropFunc{IfExists: p.acceptKw("if", "exists")}
	names, err := p.dottedName()
	if err != nil {
		return nil, true, err
	}
	d.Name = names[len(names)-1]
	if p.isOp("(") { // the argument types identify an overload; the name is the identity here
		if err := p.skipParens(); err != nil {
			return nil, true, err
		}
	}
	if !p.acceptKw("cascade") {
		p.acceptKw("restrict")
	}
	return d, true, nil
}

func (p *parser) doStmt() (Stmt, error) {
	p.next() // do
	if p.acceptKw("language") {
		p.next()
	}
	body, err := p.strLit()
	if err != nil {
		return nil, err
	}
	if p.acceptKw("language") {
		p.next()
	}
	return &DoStmt{Body: body}, nil
}

func (p *parser) callStmt() (Stmt, error) {
	p.next() // call
	start := p.peek().pos
	for p.peek().k != tEOF && !p.isOp(";") {
		p.next()
	}
	end := len(p.src)
	if p.peek().k != tEOF {
		end = p.peek().pos
	}
	return &CallStmt{Text: "SELECT " + p.src[start:end]}, nil
}

// ---- function catalog ----

// userFuncKinds caches the result kind of every function this process has seen, for
// funcKind (which has no session): a name maps to the kind of its latest definition.
var userFuncKinds sync.Map // name -> Kind

func userFuncKind(name string) (Kind, bool) {
	k, ok := userFuncKinds.Load(name)
	if !ok {
		return 0, false
	}
	return k.(Kind), true
}

func retKind(ret string) Kind {
	if ret == "void" || ret == "trigger" {
		return KText
	}
	return kindOfType(ret)
}

func (s *Session) loadFunc(name string) (*funcDef, bool) {
	o, ok := s.E.Obj("func", name)
	if !ok {
		return nil, false
	}
	var d funcDef
	if json.Unmarshal([]byte(o.Data), &d) != nil {
		return nil, false
	}
	userFuncKinds.Store(name, retKind(d.Ret))
	return &d, true
}

func (s *Session) createFunc(c *CreateFunc) (*Result, error) {
	d := c.Def
	if d.Lang == "plpgsql" && d.Ret != "trigger" {
		if _, err := plParse(d.Body, d.argNames()); err != nil {
			return nil, err
		}
	}
	if _, exists := s.E.Obj("func", d.Name); exists && !c.OrReplace {
		return nil, errf("42723", "function %q already exists with same argument types", d.Name)
	}
	b, _ := json.Marshal(d)
	if err := s.E.PutObj(core.Obj{Kind: "func", Name: d.Name, Data: string(b)}, true); err != nil {
		return nil, AsError(err)
	}
	userFuncKinds.Store(d.Name, retKind(d.Ret))
	if d.Procedure {
		return &Result{Tag: "CREATE PROCEDURE"}, nil
	}
	return &Result{Tag: "CREATE FUNCTION"}, nil
}

func (d funcDef) argNames() []string {
	out := make([]string, len(d.Args))
	for i, a := range d.Args {
		out[i] = a.Name
	}
	return out
}

func (s *Session) dropFunc(d *DropFunc) (*Result, error) {
	if _, ok := s.E.Obj("func", d.Name); !ok {
		if d.IfExists {
			return &Result{Tag: "DROP FUNCTION", Notice: fmt.Sprintf("function %s() does not exist, skipping", d.Name)}, nil
		}
		return nil, errf(codeUndefinedFunction, "function %s does not exist", d.Name)
	}
	if err := s.E.WriteObjs([]core.ObjOp{{O: core.Obj{Kind: "func", Name: d.Name}, Delete: true}}); err != nil {
		return nil, AsError(err)
	}
	userFuncKinds.Delete(d.Name)
	return &Result{Tag: "DROP FUNCTION"}, nil
}

var funcsLoaded sync.Map // *core.Engine -> true

// loadUserFuncs records the result kinds of the functions stored in e, once per engine, so
// that the first query after a restart already types them.
func loadUserFuncs(e *core.Engine) {
	if _, done := funcsLoaded.LoadOrStore(e, true); done {
		return
	}
	for _, o := range e.Objs("func") {
		var d funcDef
		if json.Unmarshal([]byte(o.Data), &d) == nil {
			userFuncKinds.Store(o.Name, retKind(d.Ret))
		}
	}
}

// callUser calls the user function named by f with evaluated args; ok=false if there is none.
func (c *evalCtx) callUser(f *Func, args []Datum) (Datum, bool, error) {
	s := c.session()
	if s == nil {
		return Null, false, nil
	}
	if _, known := userFuncKinds.Load(f.Name); !known {
		return Null, false, nil
	}
	d, ok := s.loadFunc(f.Name)
	if !ok {
		return Null, false, nil
	}
	v, err := s.runFunc(d, args)
	return v, true, err
}

// ---- running ----

const maxPLDepth = 32

func (s *Session) runFunc(d *funcDef, args []Datum) (Datum, error) {
	if d.Ret == "trigger" {
		return Null, errf(codeFeature, "trigger functions cannot be called directly")
	}
	if len(args) != len(d.Args) {
		return Null, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", d.Name, len(args))
	}
	if s.plDepth >= maxPLDepth {
		return Null, errf("54001", "stack depth limit exceeded")
	}
	s.plDepth++
	defer func() { s.plDepth-- }()
	vars := map[string]Datum{}
	for i, a := range d.Args {
		v := args[i]
		if k := kindOfType(a.Type); !v.IsNull() && v.K != k {
			var err error
			if v, err = coerce(v, k); err != nil {
				return Null, err
			}
		}
		vars[fmt.Sprintf("$%d", i+1)] = v
		if a.Name != "" {
			vars[strings.ToLower(a.Name)] = v
		}
	}
	var res Datum
	var err error
	if d.Lang == "sql" {
		res, err = s.runSQLBody(d.Body, vars)
	} else {
		var blk *plBlock
		if blk, err = plParse(d.Body, d.argNames()); err != nil {
			return Null, err
		}
		res, err = s.plRunBlock(blk, vars)
	}
	if err != nil {
		return Null, err
	}
	if d.Ret == "void" {
		return TextD(""), nil
	}
	if k := retKind(d.Ret); !res.IsNull() && res.K != k {
		return coerce(res, k)
	}
	return res, nil
}

// sqlWithVars replaces the variables named in src (and $k for the k-th function argument) by
// fresh $n placeholders and returns the new text and the values.
func sqlWithVars(src string, vars map[string]Datum) (string, []Datum, error) {
	if len(vars) == 0 {
		return src, nil, nil
	}
	toks, err := lex(src)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var params []Datum
	idx := map[string]int{}
	last := 0
	for i, t := range toks {
		name, width := "", 0
		switch t.k {
		case tIdent:
			if i > 0 && toks[i-1].k == tOp && toks[i-1].s == "." {
				continue
			}
			if i+1 < len(toks) && toks[i+1].k == tOp && (toks[i+1].s == "(" || toks[i+1].s == ".") {
				continue
			}
			name, width = t.s, len(t.s)
		case tParam:
			name, width = "$"+t.s, len(t.s)+1
		default:
			continue
		}
		v, ok := vars[name]
		if !ok {
			continue
		}
		n, seen := idx[name]
		if !seen {
			params = append(params, v)
			n = len(params)
			idx[name] = n
		}
		b.WriteString(src[last:t.pos])
		b.WriteString("$" + strconv.Itoa(n))
		last = t.pos + width
	}
	b.WriteString(src[last:])
	return b.String(), params, nil
}

// runSQLBody runs a LANGUAGE sql body: the result is the first column of the first row of the
// last statement that returns rows.
func (s *Session) runSQLBody(body string, vars map[string]Datum) (Datum, error) {
	text, params, err := sqlWithVars(body, vars)
	if err != nil {
		return Null, err
	}
	sts, err := parseWith(text, s)
	if err != nil {
		return Null, err
	}
	res := Null
	for _, st := range sts {
		r, err := s.exec(st, params)
		if err != nil {
			return Null, err
		}
		if r.HasRows {
			res = Null
			if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
				res = r.Rows[0][0]
			}
		}
	}
	return res, nil
}

func (s *Session) execDo(st *DoStmt) (*Result, error) {
	blk, err := plParse(st.Body, nil)
	if err != nil {
		return nil, err
	}
	return s.inTransaction("DO", func() (string, error) {
		_, err := s.plRunBlock(blk, map[string]Datum{})
		return s.plNotice, err
	})
}

func (s *Session) execCall(st *CallStmt) (*Result, error) {
	return s.inTransaction("CALL", func() (string, error) {
		sts, err := parseWith(st.Text, s)
		if err != nil {
			return "", err
		}
		for _, x := range sts {
			if _, err := s.exec(x, nil); err != nil {
				return "", err
			}
		}
		return s.plNotice, nil
	})
}

// inTransaction runs f as one atomic unit: in the open transaction if there is one, else in
// a transaction of its own that commits when f succeeds.
func (s *Session) inTransaction(tag string, f func() (string, error)) (*Result, error) {
	s.plNotice = ""
	own := !s.explicit
	if own {
		if _, err := s.exec(&Begin{}, nil); err != nil {
			return nil, err
		}
	}
	notice, err := f()
	if own {
		if err != nil {
			s.exec(&Rollback{}, nil)
			return nil, err
		}
		if _, cerr := s.exec(&Commit{}, nil); cerr != nil {
			return nil, cerr
		}
	} else if err != nil {
		s.markFailed()
		return nil, err
	}
	return &Result{Tag: tag, Notice: notice}, nil
}
