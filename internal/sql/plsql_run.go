package sql

import (
	"regexp"
	"strconv"
	"strings"

	"stask/internal/core"
)

// The PL/pgSQL subset: parser and interpreter (see plsql.go for what is supported).

type plStmt interface{}

type plBlock struct {
	label    string
	decls    []plDecl
	stmts    []plStmt
	handlers []plHandler
}

type plDecl struct {
	name, typ, init string
	cursor          *plCurDecl
}

type plHandler struct {
	conds []string // condition names or SQLSTATE codes, lower case
	body  []plStmt
}

type plSQL struct {
	text    string
	into    []string
	perform bool
	strict  bool
}
type plAssign struct {
	name, expr string
	idx        string // a[idx] := expr (one dimension)
}
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
	errcode string // condition name or SQLSTATE given before USING
	opts    map[string]string
	rethrow bool
}
type plExecute struct {
	expr   string
	into   []string
	using  []string
	strict bool
}
type plNested struct{ b *plBlock }

// plLoop is LOOP, WHILE, FOR i IN a..b and FOR rec IN query.
type plLoop struct {
	label string
	kind  string // loop, while, int, query
	cond  string // while
	v     string // int, query: the loop variable
	lo    string
	hi    string
	by    string
	rev   bool
	query string
	body  []plStmt
	qexec bool     // FOR r IN EXECUTE expr
	using []string // its USING values
	vars  []string // FOR a, b IN query: several targets
	cur   string   // FOR r IN cursor [(args)]
	args  []string
}

// plExit is EXIT / CONTINUE [label] [WHEN cond].
type plExit struct {
	cont  bool
	label string
	when  string
}

// plCase is the CASE statement: searched (subject == "") or simple.
type plCase struct {
	subject string
	tests   [][]string // simple: the values of each WHEN; searched: one condition each
	blocks  [][]plStmt
	els     []plStmt
	hasElse bool
}

type plp struct {
	toks   []token
	i      int
	src    string
	scope  map[string]bool
	closed bool
	curs   map[string]bool // declared cursor variables
}

func plErr(format string, a ...any) *Error { return errf(codeFeature, format, a...) }

// plParse parses a function or DO body. names are the argument names (the scope for "x = e").
func plParse(src string, names []string) (*plBlock, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	q := &plp{toks: toks, src: src, scope: map[string]bool{"found": true}, curs: map[string]bool{}}
	for _, n := range names {
		if n != "" {
			q.scope[strings.ToLower(n)] = true
		}
	}
	// optional label <<name>>
	if q.opIs("<<") {
		for !q.opIs(">>") && q.cur().k != tEOF {
			q.i++
		}
		q.i++
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
			if depth == 0 && stop(t) {
				return strings.TrimSpace(q.src[start:t.pos]), nil
			}
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
	if q.kw("alias") {
		q.i++
		q.acceptKw("for")
		src, err := q.textUntil(q.semi())
		if err != nil {
			return d, err
		}
		d.typ, d.init = "alias", strings.ToLower(src)
		q.skipSemis()
		return d, nil
	}
	if ok, err := q.declCursor(&d); ok || err != nil {
		if ok {
			q.curs[d.name] = true
		}
		return d, err
	}
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
	if q.opIs("<<") { // <<label>> before a loop or block
		label := q.toks[q.i+1].s
		q.i += 2
		for q.cur().k != tEOF && !q.opIs(">>") {
			q.i++
		}
		q.i++
		if q.kw("loop") || q.kw("while") || q.kw("for") {
			return q.loopStmt(label)
		}
		if q.kw("foreach") {
			return q.foreachStmt(label)
		}
		if q.kw("declare") || q.kw("begin") {
			b, err := q.block()
			if err != nil {
				return nil, err
			}
			b.label = strings.ToLower(label)
			return &plNested{b}, q.endStmt()
		}
		return q.stmt()
	}
	if t.k == tIdent {
		if st, ok, err := q.ext(); ok {
			return st, err
		}
		switch t.s {
		case "loop", "while", "for":
			return q.loopStmt("")
		case "case":
			return q.caseStmt()
		case "exit", "continue":
			return q.exitStmt()
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
			if q.acceptKw("next") {
				e, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
				if err != nil {
					return nil, err
				}
				return &plReturnNext{e}, q.endStmt()
			}
			if q.acceptKw("query") {
				rq := &plReturnQuery{}
				var err error
				if q.acceptKw("execute") {
					rq.exec = true
					if rq.query, err = q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" || t.k == tIdent && t.s == "using" }); err != nil {
						return nil, err
					}
					if q.acceptKw("using") {
						if rq.using, err = q.usingList(); err != nil {
							return nil, err
						}
					}
				} else if rq.query, err = q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" }); err != nil {
					return nil, err
				}
				return rq, q.endStmt()
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
			return &plAssign{name: t.s, expr: e}, q.endStmt()
		}
		// array element assignment: a[i] := expr
		if n.k == tOp && n.s == "[" && q.scope[t.s] {
			j, depth := q.i+2, 1
			for depth > 0 && j < len(q.toks) && q.toks[j].k != tEOF {
				if q.toks[j].k == tOp && q.toks[j].s == "[" {
					depth++
				} else if q.toks[j].k == tOp && q.toks[j].s == "]" {
					depth--
				}
				j++
			}
			if j+1 < len(q.toks) && q.toks[j].k == tOp && (q.toks[j].s == "=" || q.toks[j].s == ":" && q.toks[j+1].k == tOp && q.toks[j+1].s == "=") {
				idx := strings.TrimSpace(q.src[q.toks[q.i+1].pos+1 : q.toks[j-1].pos])
				q.i = j + 1
				if q.toks[j].s == ":" {
					q.i++
				}
				e, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
				if err != nil {
					return nil, err
				}
				return &plAssign{name: t.s, expr: e, idx: idx}, q.endStmt()
			}
		}
		// record field assignment: NEW.col := expr (also with =), rec.col := expr
		if n.k == tOp && n.s == "." && q.i+4 < len(q.toks) && (q.toks[q.i+2].k == tIdent || q.toks[q.i+2].k == tQuoted) {
			f := q.toks[q.i+2]
			a := q.toks[q.i+3]
			if a.k == tOp && a.s == ":" && q.toks[q.i+4].k == tOp && q.toks[q.i+4].s == "=" ||
				a.k == tOp && a.s == "=" && (t.s == "new" || t.s == "old" || q.scope[t.s]) {
				skip := 4
				if a.s == ":" {
					skip = 5
				}
				q.i += skip
				e, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
				if err != nil {
					return nil, err
				}
				return &plAssign{name: t.s + "." + f.s, expr: e}, q.endStmt()
			}
		}
	}
	// SQL statement
	first := strings.ToLower(t.s)
	text, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
	if err != nil {
		return nil, err
	}
	st := &plSQL{text: text}
	if text, into, strict, ok := splitInto(text, first); ok {
		st.text, st.into, st.strict = text, into, strict
	}
	return st, q.endStmt()
}

// splitInto removes a PL/pgSQL "INTO [STRICT] v1, v2" from a SELECT or from the RETURNING of a
// DML statement.
func splitInto(text, first string) (string, []string, bool, bool) {
	toks, err := lex(text)
	if err != nil {
		return text, nil, false, false
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
			strict := false
			if toks[j].k == tIdent && toks[j].s == "strict" {
				j++
				strict = true
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
				return text, nil, false, false
			}
			end := toks[j].pos
			return text[:t.pos] + " " + text[end:], names, strict, true
		}
	}
	return text, nil, false, false
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
	r := &plRaise{level: "exception", opts: map[string]string{}}
	if q.opIs(";") {
		r.rethrow = true
		return r, q.endStmt()
	}
	if t := q.cur(); t.k == tIdent {
		switch t.s {
		case "notice", "warning", "info", "log", "debug", "debug1", "debug2", "debug3", "debug4", "debug5", "exception":
			r.level = t.s
			q.i++
		}
	}
	t := q.cur()
	if t.k == tIdent && t.s == "sqlstate" && q.toks[q.i+1].k == tString {
		r.errcode = q.toks[q.i+1].s
		q.i += 2
		t = q.cur()
	} else if t.k == tIdent && t.s != "using" { // RAISE unique_violation USING ...
		if _, ok := plConditions[t.s]; ok {
			r.errcode = t.s
			q.i++
			t = q.cur()
		}
	}
	switch {
	case t.k == tString:
		r.format = t.s
		q.i++
	case t.k == tIdent && t.s == "using":
	case t.k == tOp && t.s == ";":
		if r.errcode == "" {
			return nil, plErr("RAISE needs a message string")
		}
	default:
		return nil, plErr("RAISE needs a message string")
	}
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
			if !q.opIs("=") && !(q.opIs(":") && q.toks[q.i+1].k == tOp && q.toks[q.i+1].s == "=") {
				return nil, q.syntax()
			}
			if q.opIs(":") {
				q.i++
			}
			q.i++
			e, err := q.textUntil(func(t token) bool { return t.k == tOp && (t.s == "," || t.s == ";") })
			if err != nil {
				return nil, err
			}
			r.opts[opt.s] = e
			if !q.opIs(",") {
				break
			}
			q.i++
		}
	}
	return r, q.endStmt()
}

// loopStmt parses LOOP, WHILE and FOR; the current token is the keyword.
func (q *plp) loopStmt(label string) (plStmt, error) {
	l := &plLoop{label: label}
	switch {
	case q.acceptKw("loop"):
		l.kind = "loop"
	case q.acceptKw("while"):
		l.kind = "while"
		c, err := q.textUntil(func(t token) bool { return t.k == tIdent && t.s == "loop" })
		if err != nil {
			return nil, err
		}
		l.cond = c
		q.i++
	default: // for
		q.i++
		v := q.cur()
		if v.k != tIdent && v.k != tQuoted {
			return nil, q.syntax()
		}
		q.i++
		l.v = strings.ToLower(v.s)
		for q.opIs(",") { // FOR a, b IN query
			q.i++
			n, err := q.identTok()
			if err != nil {
				return nil, err
			}
			l.vars = append(l.vars, n)
		}
		if !q.acceptKw("in") {
			return nil, q.syntax()
		}
		l.rev = q.acceptKw("reverse")
		txt, err := q.textUntil(func(t token) bool { return t.k == tIdent && (t.s == "loop" || t.s == "using") })
		if err != nil {
			return nil, err
		}
		if q.acceptKw("using") {
			if l.using, err = q.usingList(); err != nil {
				return nil, err
			}
		}
		if !q.acceptKw("loop") {
			return nil, q.syntax()
		}
		lt := strings.ToLower(strings.TrimSpace(txt))
		name := lt
		if i := strings.IndexByte(name, '('); i >= 0 {
			name = strings.TrimSpace(name[:i])
		}
		if a, b, ok := splitRange(txt); ok {
			l.kind, l.lo, l.hi = "int", a, b
			if m := plByRe.FindStringSubmatch(b); m != nil {
				l.hi, l.by = m[1], m[2]
			}
		} else if strings.HasPrefix(lt, "execute") && (len(lt) == 7 || lt[7] == ' ' || lt[7] == '(' || lt[7] == '\n') {
			l.kind, l.query, l.qexec = "query", strings.TrimSpace(txt[7:]), true
		} else if q.curs[name] {
			l.kind, l.cur = "cursor", name
			if i := strings.IndexByte(txt, '('); i >= 0 {
				inner := strings.TrimSpace(txt[i+1 : strings.LastIndexByte(txt, ')')])
				if inner != "" {
					l.args = splitTopCommas(inner)
				}
			}
		} else {
			l.kind, l.query = "query", txt
		}
		q.scope[l.v] = true
	}
	var err error
	if l.body, err = q.stmts("end"); err != nil {
		return nil, err
	}
	if !q.acceptKw("end") || !q.acceptKw("loop") {
		return nil, q.syntax()
	}
	if t := q.cur(); t.k == tIdent && !q.opIs(";") { // END LOOP label
		q.i++
	}
	return l, q.endStmt()
}

// splitTopCommas splits at commas outside parentheses and quotes.
func splitTopCommas(s string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

var plByRe = regexp.MustCompile(`(?is)^(.*?)\s+by\s+(.*)$`)

// splitRange splits "a .. b" at the first top-level "..".
func splitRange(s string) (string, string, bool) {
	depth := 0
	var quote byte
	for i := 0; i+1 < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == '.' && s[i+1] == '.' && depth == 0:
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:]), true
		}
	}
	return "", "", false
}

func (q *plp) exitStmt() (plStmt, error) {
	e := &plExit{cont: q.cur().s == "continue"}
	q.i++
	if t := q.cur(); t.k == tIdent && t.s != "when" {
		e.label = t.s
		q.i++
	}
	if q.acceptKw("when") {
		c, err := q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" })
		if err != nil {
			return nil, err
		}
		e.when = c
	}
	return e, q.endStmt()
}

func (q *plp) caseStmt() (plStmt, error) {
	q.i++ // case
	c := &plCase{}
	if !q.kw("when") {
		s, err := q.textUntil(func(t token) bool { return t.k == tIdent && t.s == "when" })
		if err != nil {
			return nil, err
		}
		c.subject = s
	}
	for q.acceptKw("when") {
		txt, err := q.textUntil(func(t token) bool { return t.k == tIdent && t.s == "then" })
		if err != nil {
			return nil, err
		}
		q.i++
		c.tests = append(c.tests, []string{txt})
		body, err := q.stmts("when", "else", "end")
		if err != nil {
			return nil, err
		}
		c.blocks = append(c.blocks, body)
	}
	if q.acceptKw("else") {
		c.hasElse = true
		var err error
		if c.els, err = q.stmts("end"); err != nil {
			return nil, err
		}
	}
	if !q.acceptKw("end") || !q.acceptKw("case") {
		return nil, q.syntax()
	}
	return c, q.endStmt()
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
		x.strict = q.acceptKw("strict")
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
	flowExit
	flowContinue
)

type plRun struct {
	s    *Session
	vars map[string]Datum
	ret  Datum
	spN  int

	// trigger functions: the transaction the statement runs in (nested statements
	// run in it and not in a transaction of their own), the NEW fields assigned,
	// and what RETURN said.
	tx        *core.Tx
	dirty     map[string]bool
	retMode   string // "new", "old", "null" or "" (a value)
	returned  bool
	flowLabel string // label of the EXIT / CONTINUE in flight
	iters     int

	kinds    map[string]Kind // declared kinds of the variables
	fn       *funcDef        // the function being run (nil: DO block, trigger)
	cols     []Column        // its result columns
	outNames []string        // its OUT parameters
	rows     [][]Datum       // RETURN NEXT / RETURN QUERY
	retRow   []Datum         // RETURN of a record
	rowCount int64           // GET DIAGNOSTICS ROW_COUNT
	opened   []string        // cursors this run opened
	curSeq   int
	curDefs  map[string]*plCurDecl
	labels   map[string]string // label.variable -> variable, for the blocks being run
}

const maxPLIterations = 20_000_000

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
	if v, ok := r.vars[strings.ToLower(strings.TrimSpace(text))]; ok && isPlainName(text) {
		return v, nil // a variable on its own: no query
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

// exec runs one statement: in the transaction of the trigger being run, if any.
func (r *plRun) exec(st Stmt, params []Datum) (*Result, error) {
	if r.tx != nil {
		switch st.(type) {
		case *Select, *SetOp:
			return r.s.runQuery(r.tx, st, params)
		case *Insert, *Update, *Delete:
			return r.s.runHere(r.tx, st, params)
		}
	}
	return r.s.exec(st, params)
}

func (r *plRun) sql(text string) (*Result, error) {
	vars := r.vars
	if (len(r.labels) > 0 || r.fn != nil && r.fn.Name != "") && strings.Contains(text, ".") {
		vars = r.qualified() // label.variable and function.parameter names
	}
	text, params, err := sqlWithVars(text, vars)
	if err != nil {
		return nil, err
	}
	sts, err := parseWith(text, r.s)
	if err != nil {
		return nil, err
	}
	var last *Result
	for _, st := range sts {
		if last, err = r.exec(st, params); err != nil {
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
	if r.kinds == nil {
		r.kinds = map[string]Kind{}
	}
	for _, d := range b.decls {
		for _, key := range []string{d.name, d.name + ".*"} {
			if _, seen := saved[key]; seen {
				continue
			}
			if v, ok := r.vars[key]; ok {
				c := v
				saved[key] = &c
			} else {
				saved[key] = nil
			}
		}
		if err := r.declare(d, saved); err != nil {
			return flowNext, err
		}
	}
	if b.label != "" {
		if r.labels == nil {
			r.labels = map[string]string{}
		}
		for _, d := range b.decls {
			r.labels[b.label+"."+d.name] = d.name
		}
		defer func() {
			for _, d := range b.decls {
				delete(r.labels, b.label+"."+d.name)
			}
		}()
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
	var tsp *core.Savepoint // trigger mode: the savepoint is the transaction's own
	if r.tx != nil {
		tsp = r.tx.Savepoint()
	} else if _, err := r.s.exec(&SavepointStmt{sp}, nil); err != nil {
		return flowNext, err
	}
	release := func() {
		if r.tx == nil {
			r.s.exec(&ReleaseStmt{sp}, nil)
		}
	}
	flow, err := r.list(b.stmts, outer)
	if err == nil {
		release()
		return flow, nil
	}
	pe := AsError(err)
	for _, h := range b.handlers {
		for _, c := range h.conds {
			if !condMatches(c, pe) {
				continue
			}
			if r.tx != nil {
				r.tx.RollbackTo(tsp)
			} else if _, rerr := r.s.exec(&RollbackToStmt{sp}, nil); rerr != nil {
				return flowNext, rerr
			}
			release()
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
	release()
	return flowNext, err
}

// declare gives the variable of d its initial value.
func (r *plRun) declare(d plDecl, saved map[string]*Datum) error {
	typ := strings.ToLower(strings.TrimSpace(d.typ))
	switch {
	case d.cursor != nil:
		if r.curDefs == nil {
			r.curDefs = map[string]*plCurDecl{}
		}
		r.curDefs[d.name] = d.cursor
		r.vars[d.name] = Null
		return nil
	case typ == "alias":
		r.vars[d.name] = r.vars[d.init]
		return nil
	case typ == "record":
		r.vars[d.name+".*"] = TextD("")
		return nil
	case strings.HasSuffix(typ, "%rowtype") || r.isTableType(typ):
		tbl := strings.TrimSuffix(typ, "%rowtype")
		t, err := r.s.E.UserTable(strings.TrimSpace(tbl))
		if err != nil {
			return errf("42P01", "relation %q does not exist", tbl)
		}
		var names []string
		for _, ci := range t.Schema().VisibleCols() {
			c := &t.Schema().Columns[ci]
			names = append(names, strings.ToLower(c.Name))
			key := d.name + "." + strings.ToLower(c.Name)
			if _, seen := saved[key]; !seen {
				if v, ok := r.vars[key]; ok {
					cv := v
					saved[key] = &cv
				} else {
					saved[key] = nil
				}
			}
			r.vars[key] = Null
			r.kinds[key] = kindOfType(c.Type)
		}
		r.vars[d.name+".*"] = TextD(strings.Join(names, ","))
		return nil
	case strings.HasSuffix(typ, "%type"):
		ref := strings.TrimSuffix(typ, "%type")
		var k Kind
		if k0, ok := r.kinds[strings.TrimSpace(ref)]; ok {
			k = k0
		} else if i := strings.LastIndexByte(ref, '.'); i > 0 {
			t, err := r.s.E.UserTable(strings.TrimSpace(ref[:i]))
			if err != nil {
				return errf("42P01", "relation %q does not exist", ref[:i])
			}
			ci, ok := t.Schema().Col(strings.TrimSpace(ref[i+1:]))
			if !ok {
				return errf("42703", "column %q does not exist", ref[i+1:])
			}
			k = kindOfType(t.Schema().Columns[ci].Type)
		}
		r.kinds[d.name] = k
		typ = ""
	}
	v := Null
	if d.init != "" {
		var err error
		if v, err = r.eval(d.init); err != nil {
			return err
		}
	}
	k := KNull
	if typ != "" {
		k = kindOfTypeText(d.typ)
		if k != KNull {
			r.kinds[d.name] = k
		}
	} else {
		k = r.kinds[d.name]
	}
	if !v.IsNull() && v.K != k && k != KNull {
		var err error
		if v, err = coerce(v, k); err != nil {
			return err
		}
	}
	r.vars[d.name] = v
	return nil
}

// isTableType reports whether typ names a table (a composite type for a variable).
func (r *plRun) isTableType(typ string) bool {
	if typ == "" || strings.ContainsAny(typ, " ()[]%.") {
		return false
	}
	switch typ {
	case "int", "integer", "text", "bigint", "smallint", "numeric", "boolean", "bool", "date", "uuid", "jsonb", "json",
		"refcursor", "bytea", "real", "timestamp", "timestamptz", "varchar", "float8", "float4", "interval", "time", "record", "void":
		return false
	}
	_, err := r.s.E.UserTable(typ)
	return err == nil
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
		if err != nil || flow != flowNext {
			return flow, err
		}
	}
	return flowNext, nil
}

func (r *plRun) set(name string, v Datum) error {
	if _, ok := r.vars[name]; !ok {
		return errf("42703", "variable %q does not exist", name)
	}
	if k, ok := r.kinds[name]; ok && k != KNull && !v.IsNull() && v.K != k {
		c, err := coerce(v, k)
		if err != nil {
			return err
		}
		v = c
	}
	r.vars[name] = v
	return nil
}

func (r *plRun) one(st plStmt, cur *Error) (plFlow, error) {
	switch st := st.(type) {
	case *plNested:
		return r.block(st.b, cur)
	case *plAssign:
		if st.idx != "" {
			return flowNext, r.assignElem(st)
		}
		if r.isRecord(st.name) {
			return flowNext, r.assignRecord(st)
		}
		v, err := r.eval(st.expr)
		if err != nil {
			return flowNext, err
		}
		if old, ok := r.vars[st.name]; ok && !v.IsNull() && !old.IsNull() && old.K != v.K {
			if v, err = coerce(v, old.K); err != nil {
				return flowNext, err
			}
		}
		if strings.HasPrefix(st.name, "new.") {
			if r.dirty == nil {
				r.dirty = map[string]bool{}
			}
			r.dirty[st.name[4:]] = true
		}
		return flowNext, r.set(st.name, v)
	case *plReturn:
		r.returned = true
		if n := strings.ToLower(strings.TrimSpace(st.expr)); r.isRecord(n) && n != "new" && n != "old" {
			r.retRow = r.recordValues(n)
			return flowReturn, nil
		}
		switch strings.ToLower(strings.TrimSpace(st.expr)) {
		case "new", "old":
			r.retMode = strings.ToLower(strings.TrimSpace(st.expr))
			return flowReturn, nil
		case "null", "":
			r.retMode = "null"
			return flowReturn, nil
		}
		v, err := r.eval(st.expr)
		r.ret = v
		return flowReturn, err
	case *plLoop:
		return r.loop(st, cur)
	case *plExit:
		if st.when != "" {
			v, err := r.eval(st.when)
			if err != nil {
				return flowNext, err
			}
			if !v.Bool() {
				return flowNext, nil
			}
		}
		r.flowLabel = strings.ToLower(st.label)
		if st.cont {
			return flowContinue, nil
		}
		return flowExit, nil
	case *plCase:
		for i, tests := range st.tests {
			var cond string
			if st.subject == "" {
				cond = tests[0]
			} else {
				cond = "(" + st.subject + ") IN (" + tests[0] + ")"
			}
			v, err := r.eval(cond)
			if err != nil {
				return flowNext, err
			}
			if v.Bool() {
				return r.list(st.blocks[i], cur)
			}
		}
		if !st.hasElse {
			return flowNext, errf("20000", "case not found")
		}
		return r.list(st.els, cur)
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
		r.noteCount(res)
		n := int(r.rowCount)
		if len(st.into) > 0 && n > 1 && st.strict {
			return flowNext, errf("P0003", "query returned more than one row")
		}
		if len(st.into) > 0 && n == 0 && st.strict {
			return flowNext, errf("P0002", "query returned no rows")
		}
		r.vars["found"] = BoolD(n > 0)
		if res.Notice != "" {
			r.queueNotice("notice", "00000", res.Notice, "", "")
		}
		if len(st.into) > 0 {
			if err := r.intoTargets(st.into, res); err != nil {
				return flowNext, err
			}
		}
		return flowNext, nil
	case *plExecute:
		res, err := r.runDynamic(st.expr, st.using)
		if err != nil {
			return flowNext, err
		}
		if len(st.into) > 0 {
			if st.strict && len(res.Rows) != 1 {
				if len(res.Rows) == 0 {
					return flowNext, errf("P0002", "query returned no rows")
				}
				return flowNext, errf("P0003", "query returned more than one row")
			}
			return flowNext, r.intoTargets(st.into, res)
		}
		return flowNext, nil
	case *plGetDiag:
		return flowNext, r.runGetDiag(st, cur)
	case *plForeach:
		return r.foreach(st, cur)
	case *plOpen:
		return flowNext, r.openCursor(st)
	case *plFetch:
		return flowNext, r.fetchCursor(st)
	case *plClose:
		name, err := r.curName(st.cur, false)
		if err != nil {
			return flowNext, err
		}
		_, err = r.s.exec(&CloseCursor{Name: name}, nil)
		return flowNext, err
	case *plAssert:
		v, err := r.eval(st.cond)
		if err != nil {
			return flowNext, err
		}
		if v.IsNull() || !v.Bool() {
			msg := "assertion failed"
			if st.msg != "" {
				m, err := r.eval(st.msg)
				if err != nil {
					return flowNext, err
				}
				msg = m.String()
			}
			return flowNext, errf("P0004", "%s", msg)
		}
		return flowNext, nil
	case *plReturnNext:
		row, err := r.returnRow(st.expr)
		if err != nil {
			return flowNext, err
		}
		r.rows = append(r.rows, row)
		return flowNext, nil
	case *plReturnQuery:
		var res *Result
		var err error
		if st.exec {
			res, err = r.runDynamic(st.query, st.using)
		} else {
			res, err = r.sql(st.query)
		}
		if err != nil {
			return flowNext, err
		}
		r.addRows(res.Rows)
		r.vars["found"] = BoolD(len(res.Rows) > 0)
		return flowNext, nil
	case *plRaise:
		return flowNext, r.runRaise(st, cur)
	}
	return flowNext, errf(codeInternal, "unexpected PL/pgSQL node %T", st)
}

// assignElem handles a[i] := expr on a one-dimensional array.
func (r *plRun) assignElem(st *plAssign) error {
	v, err := r.eval(st.expr)
	if err != nil {
		return err
	}
	iv, err := r.eval(st.idx)
	if err != nil {
		return err
	}
	if iv.K != KInt {
		if iv, err = coerce(iv, KInt); err != nil {
			return err
		}
	}
	arr, ok := r.vars[st.name]
	if !ok {
		return errf("42703", "variable %q does not exist", st.name)
	}
	elems := parseArrayText(arr)
	i := int(iv.I)
	if i < 1 {
		return errf("2202E", "array subscript out of range")
	}
	for len(elems) < i {
		elems = append(elems, Null)
	}
	elems[i-1] = v
	nv := arrayD(elems)
	if arr.K == KArray && arr.I != 0 {
		nv.I = arr.I
	}
	r.vars[st.name] = nv
	return nil
}

// assignRecord handles "rec := ROW(...)" and "rec := other_rec".
func (r *plRun) assignRecord(st *plAssign) error {
	if cols, vals, ok, err := r.rowConstructor(st.expr); ok || err != nil {
		if err == nil {
			r.setRecord(st.name, cols, vals)
		}
		return err
	}
	src := strings.ToLower(strings.TrimSpace(st.expr))
	if r.isRecord(src) {
		fs := r.recFields(src)
		cols := make([]Column, len(fs))
		for i, f := range fs {
			cols[i] = Column{Name: f}
		}
		r.setRecord(st.name, cols, r.recordValues(src))
		return nil
	}
	res, err := r.sql("SELECT " + st.expr)
	if err != nil {
		return err
	}
	var row []Datum
	if len(res.Rows) > 0 {
		row = res.Rows[0]
	}
	r.setRecord(st.name, res.Cols, row)
	return nil
}

// snapshotVar saves variable v and its record fields; the returned func restores them.
func (r *plRun) snapshotVar(v string) func() {
	saved := map[string]Datum{}
	for k, d := range r.vars {
		if k == v || strings.HasPrefix(k, v+".") {
			saved[k] = d
		}
	}
	return func() {
		for k := range r.vars {
			if k == v || strings.HasPrefix(k, v+".") {
				delete(r.vars, k)
			}
		}
		for k, d := range saved {
			r.vars[k] = d
		}
	}
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

// loop runs LOOP, WHILE and FOR; EXIT and CONTINUE end or restart the innermost loop (or
// the one named by their label).
func (r *plRun) loop(l *plLoop, cur *Error) (plFlow, error) {
	// body runs once; done is true when the loop is over (and flow is what to return)
	body := func() (done bool, flow plFlow, err error) {
		if r.iters++; r.iters > maxPLIterations {
			return true, flowNext, errf("54000", "PL/pgSQL loop exceeded %d iterations", maxPLIterations)
		}
		flow, err = r.list(l.body, cur)
		if err != nil {
			return true, flowNext, err
		}
		switch flow {
		case flowReturn:
			return true, flowReturn, nil
		case flowExit, flowContinue:
			if r.flowLabel != "" && r.flowLabel != strings.ToLower(l.label) {
				return true, flow, nil // aimed at an outer loop
			}
			r.flowLabel = ""
			return flow == flowExit, flowNext, nil
		}
		return false, flowNext, nil
	}
	switch l.kind {
	case "loop":
		for {
			if done, flow, err := body(); done || err != nil {
				return flow, err
			}
		}
	case "while":
		for {
			v, err := r.eval(l.cond)
			if err != nil {
				return flowNext, err
			}
			if !v.Bool() {
				return flowNext, nil
			}
			if done, flow, err := body(); done || err != nil {
				return flow, err
			}
		}
	case "int":
		num := func(e string) (int64, error) {
			v, err := r.eval(e)
			if err != nil {
				return 0, err
			}
			if v.K != KInt {
				if v, err = coerce(v, KInt); err != nil {
					return 0, err
				}
			}
			return v.I, nil
		}
		lo, err := num(l.lo)
		if err != nil {
			return flowNext, err
		}
		hi, err := num(l.hi)
		if err != nil {
			return flowNext, err
		}
		by := int64(1)
		if l.by != "" {
			if by, err = num(l.by); err != nil {
				return flowNext, err
			}
			if by <= 0 {
				return flowNext, errf("22023", "BY value of FOR loop must be greater than zero")
			}
		}
		saved, had := r.vars[l.v]
		defer func() {
			if had {
				r.vars[l.v] = saved
			} else {
				delete(r.vars, l.v)
			}
		}()
		if l.rev {
			lo, hi = hi, lo
		}
		for i := lo; (!l.rev && i <= hi) || (l.rev && i >= hi); {
			r.vars[l.v] = IntD(i)
			if done, flow, err := body(); done || err != nil {
				return flow, err
			}
			if l.rev {
				i -= by
			} else {
				i += by
			}
		}
		return flowNext, nil
	case "query", "cursor":
		var res *Result
		var err error
		switch {
		case l.kind == "cursor":
			if err = r.openCursor(&plOpen{cur: l.cur, args: l.args, bound: true}); err != nil {
				return flowNext, err
			}
			name := r.vars[l.cur].String()
			sts, perr := parseWith("FETCH ALL FROM "+quoteIdent(name), r.s)
			if perr != nil {
				return flowNext, perr
			}
			res, err = r.s.exec(sts[0], nil)
			r.s.exec(&CloseCursor{Name: name}, nil)
		case l.qexec:
			res, err = r.runDynamic(l.query, l.using)
		default:
			res, err = r.sql(l.query)
			if err == nil {
				r.noteCount(res)
			}
		}
		if err != nil {
			return flowNext, err
		}
		var names []string
		for _, c := range res.Cols {
			names = append(names, strings.ToLower(c.Name))
		}
		restore := r.snapshotVar(l.v)
		defer restore()
		targets := append([]string{l.v}, l.vars...)
		_, declared := r.vars[l.v]
		scalar := len(l.vars) > 0 || declared && !r.isRecord(l.v)
		for _, row := range res.Rows {
			switch {
			case len(l.vars) > 0:
				for i, t := range targets {
					v := Null
					if i < len(row) {
						v = row[i]
					}
					r.vars[t] = v
				}
			case scalar:
				if len(row) > 0 {
					r.vars[l.v] = row[0]
				}
			default:
				r.setRecord(l.v, res.Cols, row)
				if len(row) == 1 {
					r.vars[l.v] = row[0]
					delete(r.vars, l.v+".*")
				}
			}
			r.vars["found"] = BoolD(true)
			if done, flow, err := body(); done || err != nil {
				return flow, err
			}
		}
		_ = names
		r.vars["found"] = BoolD(len(res.Rows) > 0)
		return flowNext, nil
	}
	return flowNext, errf(codeInternal, "unknown loop kind %q", l.kind)
}

// isPlainName reports whether s is name or name.name (letters, digits, underscores).
func isPlainName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' && i > 0) {
			return false
		}
	}
	return true
}

// qualified is the variables plus the label.variable and function.parameter names that refer to them.
func (r *plRun) qualified() map[string]Datum {
	m := make(map[string]Datum, len(r.vars)+len(r.labels)+8)
	for k, v := range r.vars {
		m[k] = v
	}
	for q, n := range r.labels {
		if v, ok := r.vars[n]; ok {
			m[q] = v
		}
	}
	if r.fn != nil {
		fn := strings.ToLower(r.fn.Name)
		for _, a := range r.fn.Args {
			if a.Name != "" {
				if v, ok := r.vars[strings.ToLower(a.Name)]; ok {
					m[fn+"."+strings.ToLower(a.Name)] = v
				}
			}
		}
	}
	return m
}
