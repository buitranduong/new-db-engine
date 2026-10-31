package sql

import (
	"strconv"
	"strings"
	"sync"
)

// CREATE FUNCTION / CREATE PROCEDURE / DO / CALL with a small subset of PL/pgSQL (docs/14
// appendix P). What runs: DECLARE of scalar variables, assignment (:=), IF / ELSIF / ELSE,
// PERFORM, RETURN, RAISE (NOTICE / WARNING / EXCEPTION with %), NULL, EXECUTE 'sql'
// [INTO v] [USING ...], any SQL statement (with SELECT/RETURNING ... INTO v), nested
// BEGIN .. END blocks and EXCEPTION WHEN <condition> THEN handlers (a savepoint per block
// that has handlers). FOUND, SQLSTATE and SQLERRM exist. LANGUAGE sql bodies are a list of
// statements whose last SELECT gives the result. LOOP / WHILE / FOR, EXIT / CONTINUE and the
// CASE statement exist, and trigger functions (RETURNS trigger, trigfunc.go) with NEW / OLD.
// Not supported (0A000 at CREATE/DO time): cursors, GET DIAGNOSTICS, FOREACH, RETURN NEXT/QUERY,
// set-returning functions, OUT parameters, default argument values, overloading by argument
// types (the name is the identity).
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
	Procedure bool
	Name      string
	IfExists  bool
	Cascade   bool
	HasTypes  bool     // DROP FUNCTION name(types): the overload with those argument types
	Types     []string // their PostgreSQL names
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
	PG   string `json:"pg,omitempty"` // the PostgreSQL type as declared (int4, varchar(20)...), for pg_proc
	Mode string `json:"m,omitempty"`  // "" IN, o OUT, b INOUT, v VARIADIC, t column of RETURNS TABLE
	Def  string `json:"d,omitempty"`  // DEFAULT expression text
}

// funcDef is the stored definition.
type funcDef struct {
	Name      string    `json:"name"`
	Args      []funcArg `json:"args,omitempty"`
	Ret       string    `json:"ret"`  // internal type name, "void", "trigger"
	Lang      string    `json:"lang"` // sql, plpgsql
	Body      string    `json:"body"`
	Procedure bool      `json:"proc,omitempty"`
	RetPG     string    `json:"retpg,omitempty"`
	Vol       string    `json:"vol,omitempty"`    // provolatile: i, s ("" is volatile)
	Strict    bool      `json:"strict,omitempty"` // STRICT / RETURNS NULL ON NULL INPUT
	Set       bool      `json:"set,omitempty"`    // RETURNS SETOF / RETURNS TABLE
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
		var a funcArg
		switch {
		case p.acceptKw("out"):
			a.Mode = "o"
		case p.acceptKw("inout"):
			a.Mode = "b"
		case p.acceptKw("variadic"):
			a.Mode = "v"
		default:
			p.acceptKw("in")
		}
		t := p.peek()
		nt := p.peekN(1)
		if t.k == tIdent || t.k == tQuoted {
			// "name type" or just "type": a name is followed by another word
			if (nt.k == tIdent || nt.k == tQuoted) && !(nt.k == tIdent && (nt.s == "precision" || nt.s == "varying")) {
				a.Name = t.s
				p.next()
			}
		}
		ty, err := p.funcTypeName()
		if err != nil {
			return nil, true, err
		}
		a.Type, a.PG = ty, p.pgSpec
		if p.isKw("default") || p.isOp("=") {
			p.next()
			start := p.peek().pos
			depth := 0
			for p.peek().k != tEOF && !(depth == 0 && (p.isOp(",") || p.isOp(")"))) {
				if p.isOp("(") {
					depth++
				} else if p.isOp(")") {
					depth--
				}
				p.next()
			}
			a.Def = strings.TrimSpace(p.src[start:p.peek().pos])
		}
		c.Def.Args = append(c.Def.Args, a)
	}
	c.Def.Ret = "void"
	retSeen := false
	var bodies []string
	for p.peek().k != tEOF && !p.isOp(";") {
		switch {
		case p.acceptKw("returns"):
			retSeen = true
			switch {
			case p.acceptKw("table"):
				if err := p.expectOp("("); err != nil {
					return nil, true, err
				}
				for !p.acceptOp(")") {
					if len(c.Def.Args) > 0 && c.Def.Args[len(c.Def.Args)-1].Mode == "t" {
						if err := p.expectOp(","); err != nil {
							return nil, true, err
						}
					}
					col := funcArg{Mode: "t"}
					col.Name = p.next().s
					if col.Type, err = p.typeName(); err != nil {
						return nil, true, err
					}
					col.PG = p.pgSpec
					c.Def.Args = append(c.Def.Args, col)
				}
				c.Def.Ret, c.Def.Set = "record", true
			case p.acceptKw("setof"):
				if c.Def.Ret, err = p.funcTypeName(); err != nil {
					return nil, true, err
				}
				c.Def.RetPG, c.Def.Set = p.pgSpec, true
			case p.acceptKw("void"):
				c.Def.Ret = "void"
			case p.acceptKw("trigger"):
				c.Def.Ret = "trigger"
			default:
				if c.Def.Ret, err = p.funcTypeName(); err != nil {
					return nil, true, err
				}
				c.Def.RetPG = p.pgSpec
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
		case p.acceptKw("immutable"):
			c.Def.Vol = "i"
		case p.acceptKw("stable"):
			c.Def.Vol = "s"
		case p.acceptKw("volatile"):
			c.Def.Vol = ""
		case p.acceptKw("strict"):
			c.Def.Strict = true
		case p.acceptKw("external"):
		case p.acceptKw("called"), p.acceptKw("returns", "null"):
			for p.acceptKw("on") || p.acceptKw("null") || p.acceptKw("input") {
			}
		default:
			// IMMUTABLE STABLE VOLATILE STRICT LEAKPROOF [NOT] WINDOW and the like
			p.next()
		}
	}
	if outs := c.Def.outs(); !retSeen && len(outs) > 0 && !proc {
		if len(outs) == 1 {
			c.Def.Ret, c.Def.RetPG = outs[0].Type, outs[0].PG
		} else {
			c.Def.Ret = "record"
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

// funcTypeName is typeName that also accepts the name of a table (its row type) and "record".
func (p *parser) funcTypeName() (string, error) {
	save := p.i
	t := p.peek()
	if (t.k == tIdent || t.k == tQuoted) && p.sess != nil && t.s != "record" { // a composite type: the function returns a row
		if ut, uerr := p.sess.findType(t.s); uerr == nil && ut.isComposite() {
			p.next()
			p.pgSpec = ""
			return ut.Name, nil
		}
	}
	ty, err := p.typeName()
	if err == nil {
		return ty, nil
	}
	p.i = save
	if t.k == tIdent && t.s == "refcursor" {
		p.next()
		p.pgSpec = ""
		return "string", nil
	}
	if (t.k == tIdent || t.k == tQuoted) && p.sess != nil {
		name := t.s
		if t.s == "record" {
			p.next()
			p.pgSpec = ""
			return "record", nil
		}
		if _, terr := p.sess.E.UserTable(name); terr == nil {
			p.next()
			p.pgSpec = ""
			return name, nil
		}
	}
	return "", err
}

func (p *parser) dropFunction() (Stmt, bool, error) {
	proc := false
	if p.acceptKw("function") {
	} else if p.acceptKw("procedure") {
		proc = true
	} else {
		return nil, false, nil
	}
	d := &DropFunc{Procedure: proc, IfExists: p.acceptKw("if", "exists")}
	names, err := p.dottedName()
	if err != nil {
		return nil, true, err
	}
	d.Name = names[len(names)-1]
	if p.isOp("(") { // the argument types identify an overload
		p.next()
		d.HasTypes = true
		for !p.acceptOp(")") {
			if len(d.Types) > 0 {
				if err := p.expectOp(","); err != nil {
					return nil, true, err
				}
			}
			out := p.acceptKw("out")
			p.acceptKw("in")
			p.acceptKw("variadic")
			t, nt := p.peek(), p.peekN(1)
			if (t.k == tIdent || t.k == tQuoted) && (nt.k == tIdent || nt.k == tQuoted) && !(nt.k == tIdent && (nt.s == "precision" || nt.s == "varying")) {
				p.next()
			}
			ty, err := p.funcTypeName()
			if err != nil {
				return nil, true, err
			}
			if !out {
				d.Types = append(d.Types, funcTypeName(ty, p.pgSpec))
			}
		}
	}
	if p.acceptKw("cascade") {
		d.Cascade = true
	} else {
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

// ---- running ----

const maxPLDepth = 32

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
	skip := assignTargets(toks)
	idx := map[string]int{}
	last := 0
	paramFor := func(name string) string {
		n, seen := idx[name]
		if !seen {
			params = append(params, vars[name])
			n = len(params)
			idx[name] = n
		}
		return "$" + strconv.Itoa(n)
	}
	for i, t := range toks {
		name, width := "", 0
		switch t.k {
		case tIdent:
			if i > 0 && toks[i-1].k == tOp && toks[i-1].s == "." || skip[i] {
				continue
			}
			if m, isRec := vars[t.s+".*"]; isRec && !(i+1 < len(toks) && toks[i+1].k == tOp && toks[i+1].s == "(") {
				var fields []string
				if m.S != "" {
					fields = strings.Split(m.S, ",")
				}
				nextDot := i+1 < len(toks) && toks[i+1].k == tOp && toks[i+1].s == "."
				switch {
				case nextDot && i+2 < len(toks) && toks[i+2].k == tOp && toks[i+2].s == "*": // rec.*
					refs := make([]string, len(fields))
					afterSelect := i > 0 && toks[i-1].k == tIdent && toks[i-1].s == "select"
					for k, f := range fields {
						refs[k] = paramFor(t.s + "." + f)
						if afterSelect { // SELECT rec.* keeps the field names as column names
							refs[k] += ` AS "` + strings.ReplaceAll(f, `"`, `""`) + `"`
						}
					}
					b.WriteString(src[last:t.pos])
					b.WriteString(strings.Join(refs, ", "))
					last = toks[i+2].pos + 1
					continue
				case !nextDot:
					refs := make([]string, len(fields))
					for k, f := range fields {
						refs[k] = paramFor(t.s + "." + f)
					}
					if i >= 2 && toks[i-1].k == tOp && toks[i-1].s == "(" && toks[i-2].k == tIdent &&
						i+1 < len(toks) && toks[i+1].k == tOp && toks[i+1].s == ")" {
						if fn, ok := map[string]string{"to_jsonb": "jsonb_build_object", "to_json": "json_build_object",
							"row_to_json": "json_build_object", "jsonb_pretty": ""}[toks[i-2].s]; ok && fn != "" {
							args := make([]string, 0, 2*len(fields))
							for k, f := range fields {
								args = append(args, "'"+strings.ReplaceAll(f, "'", "''")+"'", refs[k])
							}
							b.WriteString(src[last:toks[i-2].pos])
							b.WriteString(fn + "(" + strings.Join(args, ", ") + ")")
							last = toks[i+1].pos + 1
							continue
						}
					}
					b.WriteString(src[last:t.pos])
					b.WriteString("ROW(" + strings.Join(refs, ", ") + ")")
					last = t.pos + len(t.s)
					continue
				}
			}
			if i+1 < len(toks) && toks[i+1].k == tOp && toks[i+1].s == "(" {
				continue
			}
			if i+1 < len(toks) && toks[i+1].k == tOp && toks[i+1].s == "." {
				// a record field: NEW.col, OLD.col, rec.col (variables named "new.col")
				if i+2 < len(toks) && (toks[i+2].k == tIdent || toks[i+2].k == tQuoted) {
					if _, ok := vars[t.s+"."+toks[i+2].s]; ok {
						name = t.s + "." + toks[i+2].s
						width = toks[i+2].pos + len(toks[i+2].s) - t.pos
						if toks[i+2].k == tQuoted {
							width += 2
						}
					}
				}
				if name == "" {
					continue
				}
				break
			}
			if t.s == "tg_argv" && i+3 < len(toks) && toks[i+1].k == tOp && toks[i+1].s == "[" && toks[i+2].k == tNumber &&
				toks[i+3].k == tOp && toks[i+3].s == "]" {
				name = "tg_argv[" + toks[i+2].s + "]"
				width = toks[i+3].pos + 1 - t.pos
				if _, ok := vars[name]; !ok {
					vars[name] = Null // out of range: NULL
				}
				break
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
	res, err := s.inTransaction("CALL", func() (string, error) {
		sts, err := parseWith(st.Text, s)
		if err != nil {
			return "", err
		}
		s.lastProc = nil
		for _, x := range sts {
			if _, err := s.exec(x, nil); err != nil {
				return "", err
			}
		}
		return s.plNotice, nil
	})
	if err == nil && s.lastProc != nil {
		res.Cols, res.Rows, res.HasRows = s.lastProc.cols, s.lastProc.rows, true
		s.lastProc = nil
	}
	return res, err
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

// assignTargets marks the tokens that name a column and must not be replaced by a variable of the same name:
// the targets of UPDATE ... SET a = ..., b = ... and the column list of INSERT INTO t (a, b).
func assignTargets(toks []token) map[int]bool {
	if len(toks) == 0 || toks[0].k != tIdent {
		return nil
	}
	var skip map[int]bool
	mark := func(i int) {
		if skip == nil {
			skip = map[int]bool{}
		}
		skip[i] = true
	}
	switch toks[0].s {
	case "update":
		depth, inSet := 0, false
		for i, t := range toks {
			switch {
			case t.k == tOp && t.s == "(":
				depth++
			case t.k == tOp && t.s == ")":
				depth--
			case depth == 0 && t.k == tIdent && t.s == "set" && !inSet:
				inSet = true
			case depth == 0 && inSet && t.k == tIdent && (t.s == "where" || t.s == "from" || t.s == "returning"):
				inSet = false
			case inSet && depth == 0 && (t.k == tIdent || t.k == tQuoted) && i > 0 && i+1 < len(toks) &&
				(toks[i-1].k == tIdent && toks[i-1].s == "set" || toks[i-1].k == tOp && toks[i-1].s == ",") &&
				toks[i+1].k == tOp && toks[i+1].s == "=":
				mark(i)
			}
		}
	case "insert":
		i := 1
		if i < len(toks) && toks[i].k == tIdent && toks[i].s == "into" {
			i++
		}
		if i < len(toks) && (toks[i].k == tIdent || toks[i].k == tQuoted) {
			i++ // the table name, possibly qualified
			if i+1 < len(toks) && toks[i].k == tOp && toks[i].s == "." {
				i += 2
			}
		}
		if i < len(toks) && toks[i].k == tOp && toks[i].s == "(" && (i+1 >= len(toks) || !(toks[i+1].k == tIdent && toks[i+1].s == "select")) {
			for j := i + 1; j < len(toks) && !(toks[j].k == tOp && toks[j].s == ")"); j++ {
				if toks[j].k == tIdent || toks[j].k == tQuoted {
					mark(j)
				}
			}
		}
	}
	return skip
}
