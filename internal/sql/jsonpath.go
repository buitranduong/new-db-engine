package sql

import (
	"strconv"
	"strings"

	"stask/internal/pgnum"
)

// A small SQL/JSON path evaluator (branch jsonarr): lax and strict modes,
// $ and @, .key, ."key", .*, [n], [*], [a to b], ?(filter) with == != <> <
// <= > >= && || ! exists() and parentheses, the methods .size() .type()
// .double() .abs() .floor() .ceiling(), and the functions jsonb_path_exists
// / match / query / query_first / query_array and the operators @? and @@.
// Also: variables ($name, from the vars argument), arithmetic + - * / %, like_regex,
// starts with, last in subscripts, .** and .keyvalue(). Not supported: .datetime() and the other
// datetime methods (42601).

type jpTok struct {
	k byte // 'i' ident, 's' string, 'n' number, 'o' operator, 0 end
	s string
}

func jpLex(src string) ([]jpTok, error) {
	var out []jpTok
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '$' && i+1 < len(src) && (src[i+1] == '_' || src[i+1] >= 'a' && src[i+1] <= 'z' || src[i+1] >= 'A' && src[i+1] <= 'Z'):
			j := i + 1
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			out = append(out, jpTok{'v', src[i+1 : j]})
			i = j
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return nil, errf(codeSyntax, "syntax error in jsonpath: unterminated string")
			}
			s, err := strconv.Unquote(src[i : j+1])
			if err != nil {
				s = src[i+1 : j]
			}
			out = append(out, jpTok{'s', s})
			i = j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && strings.IndexByte("0123456789.eE", src[j]) >= 0 {
				j++
			}
			out = append(out, jpTok{'n', src[i:j]})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			out = append(out, jpTok{'i', src[i:j]})
			i = j
		default:
			if i+1 < len(src) {
				switch src[i : i+2] {
				case "==", "!=", "<>", "<=", ">=", "&&", "||":
					out = append(out, jpTok{'o', src[i : i+2]})
					i += 2
					continue
				}
			}
			if strings.IndexByte("$@.*[](),?<>!+-/%", c) < 0 {
				return nil, errf(codeSyntax, "syntax error in jsonpath: unexpected %q", string(c))
			}
			out = append(out, jpTok{'o', string(c)})
			i++
		}
	}
	return append(out, jpTok{}), nil
}

type jpNode struct {
	op   string // "path", "lit", "cmp", "and", "or", "not", "exists"
	cmp  string
	lit  *jn
	root byte // '$' or '@'
	step []jpStep
	l, r *jpNode
	name string // variable name; pattern of like_regex
	flag string
}

type jpStep struct {
	kind   string // key, wild, idx, all, method, filter
	key    string
	lo, hi int
	pred   *jpNode
	subs   []jpSub // array subscripts (idx)
}

// jpSub is one subscript: lo [to hi], each a number or last[-n].
type jpSub struct {
	lo, hi         int
	loLast, hiLast bool
	single         bool
}

type jpParser struct {
	t      []jpTok
	i      int
	strict bool
}

func (p *jpParser) peek() jpTok { return p.t[p.i] }
func (p *jpParser) isOp(s string) bool {
	return p.t[p.i].k == 'o' && p.t[p.i].s == s
}
func (p *jpParser) acceptOp(s string) bool {
	if p.isOp(s) {
		p.i++
		return true
	}
	return false
}
func jpErr() error { return errf(codeSyntax, "syntax error in jsonpath") }

func parseJsonPath(src string) (*jpParser, *jpNode, error) {
	toks, err := jpLex(src)
	if err != nil {
		return nil, nil, err
	}
	p := &jpParser{t: toks}
	if t := p.peek(); t.k == 'i' && (t.s == "lax" || t.s == "strict") {
		p.strict = t.s == "strict"
		p.i++
	}
	n, err := p.or()
	if err != nil {
		return nil, nil, err
	}
	if p.peek().k != 0 {
		return nil, nil, jpErr()
	}
	return p, n, nil
}

func (p *jpParser) or() (*jpNode, error) {
	l, err := p.and()
	for err == nil && p.acceptOp("||") {
		var r *jpNode
		if r, err = p.and(); err == nil {
			l = &jpNode{op: "or", l: l, r: r}
		}
	}
	return l, err
}

func (p *jpParser) and() (*jpNode, error) {
	l, err := p.not()
	for err == nil && p.acceptOp("&&") {
		var r *jpNode
		if r, err = p.not(); err == nil {
			l = &jpNode{op: "and", l: l, r: r}
		}
	}
	return l, err
}

func (p *jpParser) not() (*jpNode, error) {
	if p.acceptOp("!") {
		x, err := p.not()
		return &jpNode{op: "not", l: x}, err
	}
	l, err := p.additive()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.k == 'i' && t.s == "like_regex" {
		p.i++
		pt := p.peek()
		if pt.k != 's' {
			return nil, jpErr()
		}
		p.i++
		n := &jpNode{op: "likere", l: l, name: pt.s}
		if ft := p.peek(); ft.k == 'i' && ft.s == "flag" {
			p.i++
			fs := p.peek()
			if fs.k != 's' {
				return nil, jpErr()
			}
			p.i++
			n.flag = fs.s
		}
		return n, nil
	}
	if t := p.peek(); t.k == 'i' && t.s == "starts" && p.t[p.i+1].k == 'i' && p.t[p.i+1].s == "with" {
		p.i += 2
		r, err := p.additive()
		if err != nil {
			return nil, err
		}
		return &jpNode{op: "starts", l: l, r: r}, nil
	}
	for _, op := range []string{"==", "!=", "<>", "<=", ">=", "<", ">"} {
		if p.acceptOp(op) {
			r, err := p.additive()
			if err != nil {
				return nil, err
			}
			if op == "<>" {
				op = "!="
			}
			return &jpNode{op: "cmp", cmp: op, l: l, r: r}, nil
		}
	}
	return l, nil
}

func (p *jpParser) additive() (*jpNode, error) {
	l, err := p.mult()
	for err == nil && (p.isOp("+") || p.isOp("-")) {
		op := p.peek().s
		p.i++
		var r *jpNode
		if r, err = p.mult(); err == nil {
			l = &jpNode{op: "arith", cmp: op, l: l, r: r}
		}
	}
	return l, err
}

func (p *jpParser) mult() (*jpNode, error) {
	l, err := p.unary()
	for err == nil && (p.isOp("*") || p.isOp("/") || p.isOp("%")) {
		op := p.peek().s
		p.i++
		var r *jpNode
		if r, err = p.unary(); err == nil {
			l = &jpNode{op: "arith", cmp: op, l: l, r: r}
		}
	}
	return l, err
}

func (p *jpParser) unary() (*jpNode, error) {
	if p.isOp("-") || p.isOp("+") {
		op := p.peek().s
		p.i++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &jpNode{op: "neg", cmp: op, l: x}, nil
	}
	return p.primary()
}

func (p *jpParser) primary() (*jpNode, error) {
	t := p.peek()
	switch {
	case t.k == 'n':
		p.i++
		return &jpNode{op: "lit", lit: &jn{t: '#', s: t.s}}, nil
	case t.k == 's':
		p.i++
		return &jpNode{op: "lit", lit: &jn{t: 's', s: t.s}}, nil
	case t.k == 'v':
		p.i++
		return &jpNode{op: "var", name: t.s}, nil
	case t.k == 'i' && (t.s == "true" || t.s == "false"):
		p.i++
		return &jpNode{op: "lit", lit: &jn{t: 'b', s: t.s}}, nil
	case t.k == 'i' && t.s == "null":
		p.i++
		return &jpNode{op: "lit", lit: &jn{t: 'n'}}, nil
	case t.k == 'i' && t.s == "exists" && p.t[p.i+1].s == "(":
		p.i += 2
		x, err := p.or()
		if err != nil || !p.acceptOp(")") {
			return nil, jpErr()
		}
		return &jpNode{op: "exists", l: x}, nil
	case p.isOp("("):
		p.i++
		x, err := p.or()
		if err != nil || !p.acceptOp(")") {
			return nil, jpErr()
		}
		return x, nil
	case p.isOp("$") || p.isOp("@"):
		p.i++
		n := &jpNode{op: "path", root: t.s[0]}
		for {
			switch {
			case p.acceptOp("."):
				t := p.peek()
				switch {
				case p.isOp("*") && p.t[p.i+1].k == 'o' && p.t[p.i+1].s == "*":
					p.i += 2
					n.step = append(n.step, jpStep{kind: "anyall"})
				case p.acceptOp("*"):
					n.step = append(n.step, jpStep{kind: "wild"})
				case t.k == 'i' || t.k == 's':
					p.i++
					if t.k == 'i' && p.isOp("(") {
						p.i++
						if !p.acceptOp(")") {
							return nil, jpErr()
						}
						n.step = append(n.step, jpStep{kind: "method", key: t.s})
					} else {
						n.step = append(n.step, jpStep{kind: "key", key: t.s})
					}
				default:
					return nil, jpErr()
				}
			case p.acceptOp("["):
				if p.acceptOp("*") {
					n.step = append(n.step, jpStep{kind: "all"})
				} else {
					st := jpStep{kind: "idx"}
					atom := func() (int, bool, error) {
						t := p.peek()
						switch {
						case t.k == 'n':
							p.i++
							v, err := strconv.Atoi(t.s)
							return v, false, err
						case t.k == 'i' && t.s == "last":
							p.i++
							off := 0
							if p.isOp("-") || p.isOp("+") {
								sign := 1
								if p.peek().s == "-" {
									sign = -1
								}
								p.i++
								nt := p.peek()
								if nt.k != 'n' {
									return 0, false, jpErr()
								}
								p.i++
								v, err := strconv.Atoi(nt.s)
								off = sign * v
								return off, true, err
							}
							return 0, true, nil
						}
						return 0, false, jpErr()
					}
					for {
						lo, loLast, err := atom()
						if err != nil {
							return nil, err
						}
						sub := jpSub{lo: lo, hi: lo, loLast: loLast, hiLast: loLast, single: true}
						if t := p.peek(); t.k == 'i' && t.s == "to" {
							p.i++
							hi, hiLast, err := atom()
							if err != nil {
								return nil, err
							}
							sub.hi, sub.hiLast, sub.single = hi, hiLast, false
						}
						st.subs = append(st.subs, sub)
						if !p.acceptOp(",") {
							break
						}
					}
					n.step = append(n.step, st)
				}
				if !p.acceptOp("]") {
					return nil, jpErr()
				}
			case p.isOp("?") && p.t[p.i+1].s == "(":
				p.i += 2
				x, err := p.or()
				if err != nil || !p.acceptOp(")") {
					return nil, jpErr()
				}
				n.step = append(n.step, jpStep{kind: "filter", pred: x})
			default:
				return n, nil
			}
		}
	}
	return nil, jpErr()
}

type jpEval struct {
	strict bool
	root   *jn
	vars   *jn
}

// anyAll appends it and every value nested in it, depth first.
func anyAll(it *jn, out *[]*jn) {
	*out = append(*out, it)
	for _, k := range it.kids {
		anyAll(k, out)
	}
}

func (e *jpEval) steps(in []*jn, steps []jpStep) ([]*jn, error) {
	for _, st := range steps {
		var out []*jn
		for _, it := range in {
			switch st.kind {
			case "key":
				cands := []*jn{it}
				if it.t == 'a' && !e.strict {
					cands = it.kids
				}
				for _, c := range cands {
					if c.t == 'o' {
						if v := c.field(st.key); v != nil {
							out = append(out, v)
						} else if e.strict {
							return nil, errf("2203A", "JSON object does not contain key %q", st.key)
						}
					} else if e.strict {
						return nil, errf("2203C", "jsonpath member accessor can only be applied to an object")
					}
				}
			case "wild":
				cands := []*jn{it}
				if it.t == 'a' && !e.strict {
					cands = it.kids
				}
				for _, c := range cands {
					if c.t == 'o' {
						out = append(out, c.kids...)
					}
				}
			case "all":
				if it.t == 'a' {
					out = append(out, it.kids...)
				} else if !e.strict {
					out = append(out, it)
				} else {
					return nil, errf("22039", "jsonpath wildcard array accessor can only be applied to an array")
				}
			case "idx":
				arr := []*jn{it}
				if it.t == 'a' {
					arr = it.kids
				} else if e.strict {
					return nil, errf("22039", "jsonpath array accessor can only be applied to an array")
				}
				for _, sb := range st.subs {
					lo, hi := sb.lo, sb.hi
					if sb.loLast {
						lo = len(arr) - 1 + sb.lo
					}
					if sb.hiLast {
						hi = len(arr) - 1 + sb.hi
					}
					if e.strict && (lo < 0 || hi >= len(arr) || lo > hi && !sb.single) {
						return nil, errf("22033", "jsonpath array subscript is out of bounds")
					}
					for i := max(lo, 0); i <= hi && i < len(arr); i++ {
						out = append(out, arr[i])
					}
				}
			case "anyall":
				anyAll(it, &out)
			case "filter":
				items := []*jn{it}
				if it.t == 'a' && !e.strict {
					items = it.kids
				}
				for _, c := range items {
					ok, err := e.truth(st.pred, c)
					if err != nil {
						return nil, err
					}
					if ok {
						out = append(out, c)
					}
				}
			case "method":
				switch st.key {
				case "size":
					if it.t == 'a' {
						out = append(out, &jn{t: '#', s: strconv.Itoa(len(it.kids))})
					} else {
						out = append(out, &jn{t: '#', s: "1"})
					}
				case "type":
					out = append(out, &jn{t: 's', s: jtype(it)})
				case "keyvalue":
					if it.t != 'o' {
						return nil, errf("22039", "jsonpath item method .keyvalue() can only be applied to an object")
					}
					for i, k := range it.keys {
						out = append(out, &jn{t: 'o', keys: []string{"key", "value", "id"}, kids: []*jn{{t: 's', s: k}, it.kids[i], {t: '#', s: "0"}}})
					}
				case "double", "abs", "floor", "ceiling":
					x, err := pgnum.Parse(it.s)
					if it.t != '#' && it.t != 's' || err != nil {
						return nil, errf("22036", "jsonpath item method .%s() can only be applied to a numeric value", st.key)
					}
					switch st.key {
					case "abs":
						if x.Sign() < 0 {
							x = x.Neg()
						}
					case "floor":
						x = x.Floor()
					case "ceiling":
						x = x.Ceil()
					}
					out = append(out, &jn{t: '#', s: x.String()})
				default:
					return nil, errf(codeSyntax, "jsonpath method .%s() is not supported", st.key)
				}
			}
		}
		in = out
	}
	return in, nil
}

// seq evaluates a value-producing node in context item cur.
func (e *jpEval) seq(n *jpNode, cur *jn) ([]*jn, error) {
	switch n.op {
	case "lit":
		return []*jn{n.lit}, nil
	case "var":
		if e.vars == nil || e.vars.field(n.name) == nil {
			return nil, errf("42704", "could not find jsonpath variable \"%s\"", n.name)
		}
		return []*jn{e.vars.field(n.name)}, nil
	case "arith", "neg":
		return e.arith(n, cur)
	case "path":
		base := e.root
		if n.root == '@' {
			base = cur
		}
		return e.steps([]*jn{base}, n.step)
	}
	ok, err := e.truth(n, cur)
	if err != nil {
		return nil, err
	}
	return []*jn{{t: 'b', s: map[bool]string{true: "true", false: "false"}[ok]}}, nil
}

func (e *jpEval) truth(n *jpNode, cur *jn) (bool, error) {
	switch n.op {
	case "and":
		a, err := e.truth(n.l, cur)
		if err != nil || !a {
			return false, err
		}
		return e.truth(n.r, cur)
	case "or":
		a, err := e.truth(n.l, cur)
		if err != nil || a {
			return a, err
		}
		return e.truth(n.r, cur)
	case "not":
		a, err := e.truth(n.l, cur)
		return !a, err
	case "exists":
		s, err := e.seq(n.l, cur)
		if err != nil && !e.strict {
			return false, nil
		}
		return len(s) > 0, err
	case "likere":
		ls, err := e.seq(n.l, cur)
		if err != nil {
			return false, err
		}
		re, err := pgRegexCompile(n.name, jpRegexFlags(n.flag))
		if err != nil {
			return false, err
		}
		for _, a := range ls {
			if a.t == 's' && re.MatchString(a.s) {
				return true, nil
			}
		}
		return false, nil
	case "starts":
		ls, err := e.seq(n.l, cur)
		if err != nil {
			return false, err
		}
		rs, err := e.seq(n.r, cur)
		if err != nil {
			return false, err
		}
		for _, a := range ls {
			for _, b := range rs {
				if a.t == 's' && b.t == 's' && strings.HasPrefix(a.s, b.s) {
					return true, nil
				}
			}
		}
		return false, nil
	case "cmp":
		ls, err := e.seq(n.l, cur)
		if err != nil {
			return false, err
		}
		rs, err := e.seq(n.r, cur)
		if err != nil {
			return false, err
		}
		for _, a := range ls {
			for _, b := range rs {
				if c, ok := jpCompare(a, b); ok && jpCmpOK(n.cmp, c) {
					return true, nil
				}
			}
		}
		return false, nil
	}
	s, err := e.seq(n, cur)
	if err != nil {
		return false, err
	}
	return len(s) > 0 && s[0].t == 'b' && s[0].s == "true", nil
}

func jpCmpOK(op string, c int) bool {
	switch op {
	case "==":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}
	return c >= 0
}

func jpCompare(a, b *jn) (int, bool) {
	if a.t != b.t {
		return 0, false
	}
	switch a.t {
	case 'n':
		return 0, true
	case '#':
		x, e1 := pgnum.Parse(a.s)
		y, e2 := pgnum.Parse(b.s)
		if e1 != nil || e2 != nil {
			return 0, false
		}
		return pgnum.Cmp(x, y), true
	case 's', 'b':
		return strings.Compare(a.s, b.s), true
	}
	return 0, false
}

func jsonPathRun(args []Datum) ([]*jn, *jpParser, *jpNode, bool, error) {
	j, _, err := jsonIn(args[0])
	if err != nil {
		return nil, nil, nil, false, err
	}
	p, n, err := parseJsonPath(args[1].String())
	if err != nil {
		return nil, nil, nil, false, err
	}
	silent := len(args) >= 4 && args[3].Bool()
	e := &jpEval{strict: p.strict, root: j}
	if len(args) >= 3 && !args[2].IsNull() {
		vj, _, verr := jsonIn(args[2])
		if verr != nil {
			return nil, nil, nil, false, verr
		}
		if vj.t != 'o' {
			return nil, nil, nil, false, errf("22023", "\"vars\" argument is not an object")
		}
		e.vars = vj
	}
	s, err := e.seq(n, j)
	if err != nil && silent && AsError(err).Code != "42704" {
		return nil, p, n, true, nil
	}
	return s, p, n, false, err
}

func jsonPathCall(name string, args []Datum) (Datum, bool, error) {
	switch name {
	case "stask_jpath_exists", "stask_jpath_match", "jsonb_path_exists", "jsonb_path_match", "jsonb_path_query_first", "jsonb_path_query_array":
	default:
		return Null, false, nil
	}
	if len(args) < 2 {
		return Null, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, len(args))
	}
	for _, a := range args[:2] {
		if a.IsNull() {
			return Null, true, nil
		}
	}
	s, _, n, silenced, err := jsonPathRun(args)
	if err != nil {
		if name == "stask_jpath_exists" || name == "stask_jpath_match" {
			return Null, true, nil // the operators are silent
		}
		return Null, true, err
	}
	_ = silenced
	switch name {
	case "stask_jpath_exists", "jsonb_path_exists":
		return BoolD(len(s) > 0), true, nil
	case "stask_jpath_match", "jsonb_path_match":
		if len(s) == 1 && s[0].t == 'b' {
			return BoolD(s[0].s == "true"), true, nil
		}
		if len(s) == 1 && s[0].t == 'n' {
			return Null, true, nil
		}
		return Null, true, errf("22038", "single boolean result is expected")
	case "jsonb_path_query_first":
		if len(s) == 0 {
			return Null, true, nil
		}
		d, err := jsonbOf(s[0])
		return d, true, err
	}
	d, err := jsonbOf(&jn{t: 'a', kids: s})
	_ = n
	return d, true, err
}

func jsonPathQuery(args []Datum) ([][]Datum, error) {
	if len(args) < 2 || args[1].IsNull() {
		return nil, nil
	}
	s, _, _, _, err := jsonPathRun(args)
	if err != nil {
		return nil, err
	}
	var out [][]Datum
	for _, x := range s {
		d, err := jsonbOf(x)
		if err != nil {
			return nil, err
		}
		out = append(out, []Datum{d})
	}
	return out, nil
}

// jpRegexFlags maps the flag letters of like_regex (i s m x q) to the engine's.
func jpRegexFlags(f string) string {
	out := ""
	for _, c := range f {
		switch c {
		case 'i':
			out += "i"
		case 'm':
			out += "n"
		case 'q':
			out += "q"
		case 'x':
			out += "x"
		}
	}
	return out
}

// arith evaluates unary and binary arithmetic over singleton numeric operands (lax mode unwraps arrays).
func (e *jpEval) arith(n *jpNode, cur *jn) ([]*jn, error) {
	num := func(x *jpNode) ([]pgnum.Dec, error) {
		s, err := e.seq(x, cur)
		if err != nil {
			return nil, err
		}
		var out []pgnum.Dec
		for _, it := range s {
			items := []*jn{it}
			if it.t == 'a' && !e.strict {
				items = it.kids
			}
			for _, c := range items {
				if c.t != '#' {
					return nil, errf("22038", "operand of unary jsonpath operator %s is not a numeric value", n.cmp)
				}
				d, err := pgnum.Parse(c.s)
				if err != nil {
					return nil, errf("22038", "operand of jsonpath operator %s is not a numeric value", n.cmp)
				}
				out = append(out, d)
			}
		}
		return out, nil
	}
	if n.op == "neg" {
		xs, err := num(n.l)
		if err != nil {
			return nil, err
		}
		var out []*jn
		for _, x := range xs {
			if n.cmp == "-" {
				x = x.Neg()
			}
			out = append(out, &jn{t: '#', s: x.String()})
		}
		return out, nil
	}
	ls, err := num(n.l)
	if err != nil {
		return nil, err
	}
	rs, err := num(n.r)
	if err != nil {
		return nil, err
	}
	if len(ls) != 1 || len(rs) != 1 {
		return nil, errf("22038", "left operand of jsonpath operator %s is not a single numeric value", n.cmp)
	}
	a, b := ls[0], rs[0]
	var r pgnum.Dec
	switch n.cmp {
	case "+":
		r = pgnum.Add(a, b)
	case "-":
		r = pgnum.Sub(a, b)
	case "*":
		r = pgnum.Mul(a, b)
	case "/":
		if b.IsZero() {
			return nil, errf(codeDivisionByZero, "division by zero")
		}
		r, err = pgnum.Div(a, b)
	case "%":
		if b.IsZero() {
			return nil, errf(codeDivisionByZero, "division by zero")
		}
		r, err = pgnum.Mod(a, b)
	}
	if err != nil {
		return nil, err
	}
	return []*jn{{t: '#', s: r.String()}}, nil
}
