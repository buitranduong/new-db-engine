package sql

import (
	"strconv"
	"strings"
)

// The PL/pgSQL subset: parser and interpreter (see plsql.go for what is supported).

type plStmt interface{}

type plBlock struct {
	decls    []plDecl
	stmts    []plStmt
	handlers []plHandler
}

type plDecl struct {
	name, typ, init string
}

type plHandler struct {
	conds []string // condition names or SQLSTATE codes, lower case
	body  []plStmt
}

type plSQL struct {
	text    string
	into    []string
	perform bool
}
type plAssign struct{ name, expr string }
type plIf struct {
	conds  []string
	blocks [][]plStmt
	els    []plStmt
}
type plReturn struct{ expr string }
type plRaise struct {
	level   string
	format  string
	args    []string
	errcode string
	rethrow bool
}
type plExecute struct {
	expr  string
	into  []string
	using []string
}
type plNested struct{ b *plBlock }

type plp struct {
	toks   []token
	i      int
	src    string
	scope  map[string]bool
	closed bool
}

func plErr(format string, a ...any) *Error { return errf(codeFeature, format, a...) }

// plParse parses a function or DO body. names are the argument names (the scope for "x = e").
func plParse(src string, names []string) (*plBlock, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	q := &plp{toks: toks, src: src, scope: map[string]bool{"found": true}}
	for _, n := range names {
		if n != "" {
			q.scope[strings.ToLower(n)] = true
		}
	}
	// optional label <<name>>
	if q.opIs("<") && q.toks[q.i+1].k == tOp && q.toks[q.i+1].s == "<" {
		for !(q.opIs(">") && q.toks[q.i+1].k == tOp && q.toks[q.i+1].s == ">") && q.cur().k != tEOF {
			q.i++
		}
		q.i += 2
	}
	b, err := q.block()
	if err != nil {
		return nil, err
	}
	q.skipSemis()
	if q.cur().k != tEOF {
		return nil, q.syntax()
	}
	return b, nil
}

func (q *plp) cur() token { return q.toks[q.i] }
func (q *plp) kw(s string) bool {
	t := q.cur()
	return t.k == tIdent && t.s == s
}
func (q *plp) opIs(s string) bool {
	t := q.cur()
	return t.k == tOp && t.s == s
}
func (q *plp) syntax() *Error {
	return errf("42601", "syntax error at or near %q in PL/pgSQL body", q.cur().s)
}
func (q *plp) skipSemis() {
	for q.opIs(";") {
		q.i++
	}
}
func (q *plp) acceptKw(s string) bool {
	if q.kw(s) {
		q.i++
		return true
	}
	return false
}

// textUntil returns the source between the current token and the first token at depth 0 for
// which stop is true (not consumed).
func (q *plp) textUntil(stop func(t token) bool) (string, error) {
	start := q.cur().pos
	depth := 0
	for {
		t := q.cur()
		if t.k == tEOF {
			if depth == 0 {
				return strings.TrimSpace(q.src[start:t.pos]), nil
			}
			return "", q.syntax()
		}
		if t.k == tOp && t.s == "(" {
			depth++
		} else if t.k == tOp && t.s == ")" {
			depth--
		} else if depth == 0 && stop(t) {
			return strings.TrimSpace(q.src[start:t.pos]), nil
		}
		q.i++
	}
}

func (q *plp) block() (*plBlock, error) {
	b := &plBlock{}
	if q.acceptKw("declare") {
		for !q.kw("begin") {
			if q.cur().k == tEOF {
				return nil, q.syntax()
			}
			d, err := q.decl()
			if err != nil {
				return nil, err
			}
			b.decls = append(b.decls, d)
			q.scope[d.name] = true
		}
	}
	if !q.acceptKw("begin") {
		return nil, q.syntax()
	}
	var err error
	if b.stmts, err = q.stmts("end", "exception"); err != nil {
		return nil, err
	}
	if q.acceptKw("exception") {
		for q.acceptKw("when") {
			h := plHandler{}
			for {
				t := q.cur()
				if t.k != tIdent && t.k != tString {
					return nil, q.syntax()
				}
				q.i++
				c := t.s
				if t.k == tIdent && t.s == "sqlstate" {
					n := q.cur()
					q.i++
					c = n.s
				}
				h.conds = append(h.conds, strings.ToLower(c))
				if !q.acceptKw("or") {
					break
				}
			}
			if !q.acceptKw("then") {
				return nil, q.syntax()
			}
			if h.body, err = q.stmts("when", "end"); err != nil {
				return nil, err
			}
			b.handlers = append(b.handlers, h)
		}
	}
	if !q.acceptKw("end") {
		return nil, q.syntax()
	}
	if t := q.cur(); t.k == tIdent && !q.opIs(";") { // END label
		q.i++
	}
	return b, nil
}

func (q *plp) decl() (plDecl, error) {
	t := q.cur()
	if t.k != tIdent && t.k != tQuoted {
		return plDecl{}, q.syntax()
	}
	q.i++
	d := plDecl{name: strings.ToLower(t.s)}
	q.acceptKw("constant")
	typ, err := q.textUntil(func(t token) bool {
		return t.k == tOp && (t.s == ";" || t.s == ":" || t.s == "=") || t.k == tIdent && (t.s == "default" || t.s == "not")
	})
	if err != nil {
		return d, err
	}
	d.typ = typ
	if q.acceptKw("not") {
		q.acceptKw("null")
	}
	if q.opIs(":") && q.toks[q.i+1].k == tOp && q.toks[q.i+1].s == "=" {
		q.i += 2
	} else if q.opIs("=") || q.acceptKw("default") {
		if q.opIs("=") {
			q.i++
		}
	}
	if !q.opIs(";") {
		if d.init, err = q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" }); err != nil {
			return d, err
		}
	}
	q.skipSemis()
	return d, nil
}

var plUnsupported = map[string]bool{
	"for": true, "while": true, "loop": true, "case": true, "foreach": true, "exit": true, "continue": true,
	"get": true, "open": true, "fetch": true, "close": true, "move": true,
}

func (q *plp) stmts(stop ...string) ([]plStmt, error) {
	var out []plStmt
	for {
		q.skipSemis()
		t := q.cur()
		if t.k == tEOF {
			return nil, q.syntax()
		}
		if t.k == tIdent {
			for _, s := range stop {
				if t.s == s {
					return out, nil
				}
			}
		}
		st, err := q.stmt()
		if err != nil {
			return nil, err
		}
		if st != nil {
			out = append(out, st)
		}
	}
}

func (q *plp) endStmt() error {
	if q.opIs(";") {
		q.i++
		return nil
	}
	if q.cur().k == tEOF {
		return nil
	}
	return q.syntax()
}

func (q *plp) stmt() (plStmt, error) {
	t := q.cur()
	if t.k == tIdent {
		if plUnsupported[t.s] {
			return nil, plErr("PL/pgSQL statement %s is not supported (no loops, CASE statements, cursors or GET DIAGNOSTICS)", strings.ToUpper(t.s))
		}
		switch t.s {
		case "begin", "declare":
			b, err := q.block()
			if err != nil {
				return nil, err
			}
			return &plNested{b}, q.endStmt()
		case "null":
			q.i++
			return nil, q.endStmt()
		case "if":
			return q.ifStmt()
		case "return":
			q.i++
			if q.kw("next") || q.kw("query") {
				return nil, plErr("RETURN NEXT / RETURN QUERY are not supported")
			}
			e, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
			if err != nil {
				return nil, err
			}
			return &plReturn{e}, q.endStmt()
		case "raise":
			return q.raiseStmt()
		case "perform":
			q.i++
			e, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
			if err != nil {
				return nil, err
			}
			return &plSQL{text: "SELECT " + e, perform: true}, q.endStmt()
		case "execute":
			return q.executeStmt()
		}
		// assignment: name := expr
		n := q.toks[q.i+1]
		if n.k == tOp && n.s == ":" && q.toks[q.i+2].k == tOp && q.toks[q.i+2].s == "=" ||
			n.k == tOp && n.s == "=" && q.scope[t.s] {
			skip := 2
			if n.s == ":" {
				skip = 3
			}
			q.i += skip
			e, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
			if err != nil {
				return nil, err
			}
			return &plAssign{t.s, e}, q.endStmt()
		}
		if n.k == tOp && n.s == "." && (t.s == "new" || t.s == "old") {
			return nil, plErr("NEW and OLD (trigger functions) are not supported")
		}
	}
	// SQL statement
	first := strings.ToLower(t.s)
	text, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
	if err != nil {
		return nil, err
	}
	st := &plSQL{text: text}
	if text, into, ok := splitInto(text, first); ok {
		st.text, st.into = text, into
	}
	return st, q.endStmt()
}

// splitInto removes a PL/pgSQL "INTO [STRICT] v1, v2" from a SELECT or from the RETURNING of a
// DML statement.
func splitInto(text, first string) (string, []string, bool) {
	toks, err := lex(text)
	if err != nil {
		return text, nil, false
	}
	depth := 0
	after := first != "insert" && first != "update" && first != "delete"
	for i, t := range toks {
		if t.k == tOp && t.s == "(" {
			depth++
		} else if t.k == tOp && t.s == ")" {
			depth--
		} else if depth == 0 && t.k == tIdent && t.s == "returning" {
			after = true
		} else if depth == 0 && after && t.k == tIdent && t.s == "into" && i > 0 {
			j := i + 1
			if toks[j].k == tIdent && toks[j].s == "strict" {
				j++
			}
			var names []string
			for toks[j].k == tIdent || toks[j].k == tQuoted {
				names = append(names, strings.ToLower(toks[j].s))
				j++
				if toks[j].k == tOp && toks[j].s == "," {
					j++
					continue
				}
				break
			}
			if len(names) == 0 {
				return text, nil, false
			}
			end := toks[j].pos
			return text[:t.pos] + " " + text[end:], names, true
		}
	}
	return text, nil, false
}

func (q *plp) ifStmt() (plStmt, error) {
	q.i++ // if
	st := &plIf{}
	for {
		c, err := q.textUntil(func(t token) bool { return t.k == tIdent && t.s == "then" })
		if err != nil {
			return nil, err
		}
		q.i++ // then
		body, err := q.stmts("elsif", "elseif", "else", "end")
		if err != nil {
			return nil, err
		}
		st.conds, st.blocks = append(st.conds, c), append(st.blocks, body)
		if q.acceptKw("elsif") || q.acceptKw("elseif") {
			continue
		}
		if q.acceptKw("else") {
			if st.els, err = q.stmts("end"); err != nil {
				return nil, err
			}
		}
		break
	}
	if !q.acceptKw("end") || !q.acceptKw("if") {
		return nil, q.syntax()
	}
	return st, q.endStmt()
}

func (q *plp) raiseStmt() (plStmt, error) {
	q.i++
	r := &plRaise{level: "exception"}
	if q.opIs(";") {
		r.rethrow = true
		return r, q.endStmt()
	}
	if t := q.cur(); t.k == tIdent {
		switch t.s {
		case "notice", "warning", "info", "log", "debug", "exception":
			r.level = t.s
			q.i++
		}
	}
	t := q.cur()
	if t.k != tString {
		return nil, plErr("RAISE needs a message string")
	}
	r.format = t.s
	q.i++
	for q.opIs(",") {
		q.i++
		e, err := q.textUntil(func(t token) bool {
			return t.k == tOp && (t.s == "," || t.s == ";") || t.k == tIdent && t.s == "using"
		})
		if err != nil {
			return nil, err
		}
		r.args = append(r.args, e)
	}
	if q.acceptKw("using") {
		for {
			opt := q.cur()
			q.i++
			if !q.opIs("=") {
				return nil, q.syntax()
			}
			q.i++
			e, err := q.textUntil(func(t token) bool { return t.k == tOp && (t.s == "," || t.s == ";") })
			if err != nil {
				return nil, err
			}
			if opt.s == "errcode" {
				r.errcode = strings.Trim(e, "'")
			}
			if !q.opIs(",") {
				break
			}
			q.i++
		}
	}
	return r, q.endStmt()
}

func (q *plp) executeStmt() (plStmt, error) {
	q.i++
	e, err := q.textUntil(func(t token) bool {
		return t.k == tOp && t.s == ";" || t.k == tIdent && (t.s == "into" || t.s == "using")
	})
	if err != nil {
		return nil, err
	}
	x := &plExecute{expr: e}
	if q.acceptKw("into") {
		q.acceptKw("strict")
		for {
			t := q.cur()
			q.i++
			x.into = append(x.into, strings.ToLower(t.s))
			if !q.opIs(",") {
				break
			}
			q.i++
		}
	}
	if q.acceptKw("using") {
		for {
			a, err := q.textUntil(func(t token) bool { return t.k == tOp && (t.s == "," || t.s == ";") })
			if err != nil {
				return nil, err
			}
			x.using = append(x.using, a)
			if !q.opIs(",") {
				break
			}
			q.i++
		}
	}
	return x, q.endStmt()
}

// ---- interpreter ----

var plConditions = map[string]string{
	"unique_violation": "23505", "foreign_key_violation": "23503", "not_null_violation": "23502",
	"check_violation": "23514", "duplicate_object": "42710", "duplicate_table": "42P07",
	"duplicate_column": "42701", "duplicate_function": "42723", "duplicate_schema": "42P06",
	"duplicate_alias": "42712", "duplicate_database": "42P04", "undefined_table": "42P01",
	"undefined_column": "42703", "undefined_object": "42704", "undefined_function": "42883",
	"invalid_text_representation": "22P02", "division_by_zero": "22012", "raise_exception": "P0001",
	"no_data_found": "P0002", "too_many_rows": "P0003", "syntax_error": "42601",
	"insufficient_privilege": "42501", "feature_not_supported": "0A000", "invalid_schema_name": "3F000",
	"dependent_objects_still_exist": "2BP01", "string_data_right_truncation": "22001",
	"numeric_value_out_of_range": "22003", "serialization_failure": "40001", "deadlock_detected": "40P01",
}

func condMatches(c string, e *Error) bool {
	if c == "others" {
		return e.Code != codeQueryCanceled
	}
	if code, ok := plConditions[c]; ok {
		return strings.EqualFold(code, e.Code)
	}
	return strings.EqualFold(c, e.Code)
}

type plFlow int

const (
	flowNext plFlow = iota
	flowReturn
)

type plRun struct {
	s    *Session
	vars map[string]Datum
	ret  Datum
	spN  int
}

func (s *Session) plRunBlock(b *plBlock, vars map[string]Datum) (Datum, error) {
	r := &plRun{s: s, vars: vars}
	if _, err := r.block(b, nil); err != nil {
		return Null, err
	}
	return r.ret, nil
}

func (r *plRun) eval(text string) (Datum, error) {
	if strings.TrimSpace(text) == "" {
		return Null, nil
	}
	res, err := r.sql("SELECT " + text)
	if err != nil {
		return Null, err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return Null, nil
	}
	return res.Rows[0][0], nil
}

func (r *plRun) sql(text string) (*Result, error) {
	text, params, err := sqlWithVars(text, r.vars)
	if err != nil {
		return nil, err
	}
	sts, err := parseWith(text, r.s)
	if err != nil {
		return nil, err
	}
	var last *Result
	for _, st := range sts {
		if last, err = r.s.exec(st, params); err != nil {
			return nil, err
		}
	}
	if last == nil {
		last = &Result{}
	}
	return last, nil
}

func (r *plRun) block(b *plBlock, outer *Error) (plFlow, error) {
	saved := map[string]*Datum{}
	for _, d := range b.decls {
		if v, ok := r.vars[d.name]; ok {
			c := v
			saved[d.name] = &c
		} else {
			saved[d.name] = nil
		}
		v := Null
		if d.init != "" {
			var err error
			if v, err = r.eval(d.init); err != nil {
				return flowNext, err
			}
		}
		if k := kindOfTypeText(d.typ); !v.IsNull() && v.K != k && k != KNull {
			var err error
			if v, err = coerce(v, k); err != nil {
				return flowNext, err
			}
		}
		r.vars[d.name] = v
	}
	defer func() {
		for n, v := range saved {
			if v == nil {
				delete(r.vars, n)
			} else {
				r.vars[n] = *v
			}
		}
	}()
	if len(b.handlers) == 0 {
		return r.list(b.stmts, outer)
	}
	// a savepoint makes the block's writes undoable when a handler takes over
	r.spN++
	sp := "__pl_sp" + strconv.Itoa(r.spN) + "_" + strconv.Itoa(r.s.plDepth)
	if _, err := r.s.exec(&SavepointStmt{sp}, nil); err != nil {
		return flowNext, err
	}
	flow, err := r.list(b.stmts, outer)
	if err == nil {
		r.s.exec(&ReleaseStmt{sp}, nil)
		return flow, nil
	}
	pe := AsError(err)
	for _, h := range b.handlers {
		for _, c := range h.conds {
			if !condMatches(c, pe) {
				continue
			}
			if _, rerr := r.s.exec(&RollbackToStmt{sp}, nil); rerr != nil {
				return flowNext, rerr
			}
			r.s.exec(&ReleaseStmt{sp}, nil)
			saveS, saveM := r.vars["sqlstate"], r.vars["sqlerrm"]
			r.vars["sqlstate"], r.vars["sqlerrm"] = TextD(pe.Code), TextD(pe.Msg)
			flow, herr := r.list(h.body, pe)
			if saveS.IsNull() {
				delete(r.vars, "sqlstate")
				delete(r.vars, "sqlerrm")
			} else {
				r.vars["sqlstate"], r.vars["sqlerrm"] = saveS, saveM
			}
			return flow, herr
		}
	}
	r.s.exec(&ReleaseStmt{sp}, nil)
	return flowNext, err
}

func kindOfTypeText(t string) Kind {
	toks, err := lex(t)
	if err != nil {
		return KNull
	}
	p := &parser{toks: toks, src: t}
	n, err := p.typeName()
	if err != nil {
		return KNull
	}
	return kindOfType(n)
}

func (r *plRun) list(stmts []plStmt, cur *Error) (plFlow, error) {
	for _, st := range stmts {
		flow, err := r.one(st, cur)
		if err != nil || flow == flowReturn {
			return flow, err
		}
	}
	return flowNext, nil
}

func (r *plRun) set(name string, v Datum) error {
	if _, ok := r.vars[name]; !ok {
		return errf("42703", "variable %q does not exist", name)
	}
	r.vars[name] = v
	return nil
}

func (r *plRun) one(st plStmt, cur *Error) (plFlow, error) {
	switch st := st.(type) {
	case *plNested:
		return r.block(st.b, cur)
	case *plAssign:
		v, err := r.eval(st.expr)
		if err != nil {
			return flowNext, err
		}
		if old, ok := r.vars[st.name]; ok && !v.IsNull() && !old.IsNull() && old.K != v.K {
			if v, err = coerce(v, old.K); err != nil {
				return flowNext, err
			}
		}
		return flowNext, r.set(st.name, v)
	case *plReturn:
		v, err := r.eval(st.expr)
		r.ret = v
		return flowReturn, err
	case *plIf:
		for i, c := range st.conds {
			v, err := r.eval(c)
			if err != nil {
				return flowNext, err
			}
			if v.Bool() {
				return r.list(st.blocks[i], cur)
			}
		}
		return r.list(st.els, cur)
	case *plSQL:
		res, err := r.sql(st.text)
		if err != nil {
			return flowNext, err
		}
		n := len(res.Rows)
		if !res.HasRows {
			if f := strings.Fields(res.Tag); len(f) > 0 {
				n, _ = strconv.Atoi(f[len(f)-1])
			}
		}
		r.vars["found"] = BoolD(n > 0)
		if res.Notice != "" {
			r.s.plNotice = res.Notice
		}
		for i, name := range st.into {
			v := Null
			if len(res.Rows) > 0 && i < len(res.Rows[0]) {
				v = res.Rows[0][i]
			}
			if err := r.set(name, v); err != nil {
				return flowNext, err
			}
		}
		return flowNext, nil
	case *plExecute:
		q, err := r.eval(st.expr)
		if err != nil {
			return flowNext, err
		}
		var params []Datum
		for _, a := range st.using {
			v, err := r.eval(a)
			if err != nil {
				return flowNext, err
			}
			params = append(params, v)
		}
		sts, err := parseWith(q.String(), r.s)
		if err != nil {
			return flowNext, err
		}
		var res *Result
		for _, x := range sts {
			if res, err = r.s.exec(x, params); err != nil {
				return flowNext, err
			}
		}
		for i, name := range st.into {
			v := Null
			if res != nil && len(res.Rows) > 0 && i < len(res.Rows[0]) {
				v = res.Rows[0][i]
			}
			if err := r.set(name, v); err != nil {
				return flowNext, err
			}
		}
		return flowNext, nil
	case *plRaise:
		if st.rethrow {
			if cur == nil {
				return flowNext, errf("0Z002", "RAISE without parameters cannot be used outside an exception handler")
			}
			return flowNext, cur
		}
		var args []string
		for _, a := range st.args {
			v, err := r.eval(a)
			if err != nil {
				return flowNext, err
			}
			args = append(args, v.String())
		}
		msg := raiseFormat(st.format, args)
		if st.level != "exception" {
			r.s.plNotice = msg
			return flowNext, nil
		}
		code := "P0001"
		if st.errcode != "" {
			code = st.errcode
			if c, ok := plConditions[strings.ToLower(code)]; ok {
				code = c
			}
		}
		return flowNext, errf(code, "%s", msg)
	}
	return flowNext, errf(codeInternal, "unexpected PL/pgSQL node %T", st)
}

// raiseFormat replaces each % by the next argument; %% is a percent sign.
func raiseFormat(f string, args []string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			b.WriteByte(f[i])
			continue
		}
		if i+1 < len(f) && f[i+1] == '%' {
			b.WriteByte('%')
			i++
			continue
		}
		if n < len(args) {
			b.WriteString(args[n])
			n++
		}
	}
	return b.String()
}
