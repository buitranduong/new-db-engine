package sql

import (
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
)

// tsvector and tsquery as column and value types (docs/14 row "types: tsvector, tsquery"). Precedent: PostgreSQL 16
// src/backend/utils/adt/tsvector.c (tsvectorin/out/send/recv), tsvector_op.c (tsvector_concat, TS_execute via
// tsquery @@ tsvector, tsq_mcontains), tsquery.c (gettoken_query, makepol, infix, tsquerysend/recv), tsquery_op.c
// (&&, ||, !!, <->), ts_type.h (binary layout), doc/src/sgml/textsearch.sgml 12.1.
//
// In flight a value is Datum{K: KText, D: tagTSVector or tagTSQuery, S: canonical text}; the column stores the canonical
// text. The functions that build values from documents and queries (to_tsvector, to_tsquery, ... in funcs) call
// TSVectorFrom / TSQueryParse (exported below) or just return the canonical text, which every operator here also
// accepts from an untagged text operand.
//
// The 'simple' configuration needs no stemmer; 'english' needs a Snowball stemmer, which is not in go.mod: not done here.

const (
	tagTSVector = 30
	tagTSQuery  = 31
)

func isTSV(d Datum) bool { return d.K == KText && d.D == tagTSVector }
func isTSQ(d Datum) bool { return d.K == KText && d.D == tagTSQuery }

type tsPos struct {
	pos uint16
	w   uint8 // 0 = D (default), 1 = C, 2 = B, 3 = A
}

type tsLex struct {
	word string
	pos  []tsPos
}

type tsvec []tsLex

func tsWeightByte(w uint8) byte { return "DCBA"[w&3] }

func (v tsvec) text() string {
	var b strings.Builder
	for i, l := range v {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		for j := 0; j < len(l.word); j++ {
			if c := l.word[j]; c == '\'' || c == '\\' {
				b.WriteByte(c)
			}
			b.WriteByte(l.word[j])
		}
		b.WriteByte('\'')
		for k, p := range l.pos {
			if k == 0 {
				b.WriteByte(':')
			} else {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(int(p.pos)))
			if p.w != 0 {
				b.WriteByte(tsWeightByte(p.w))
			}
		}
	}
	return b.String()
}

// TSVectorFrom builds the canonical tsvector value from lexemes (any order, repeats allowed): they are sorted, merged,
// and their positions sorted and made unique (the highest weight wins), as PostgreSQL does.
func TSVectorFrom(lex []TSLexeme) Datum {
	in := make([]tsLex, len(lex))
	for i, l := range lex {
		in[i].word = l.Word
		for _, p := range l.Pos {
			in[i].pos = append(in[i].pos, tsPos{pos: p.Pos, w: p.Weight})
		}
	}
	return Datum{K: KText, D: tagTSVector, S: tsvNormalize(in).text()}
}

// TSLexeme and TSPos are the exported form of a tsvector entry: Weight is 0 for D (the default), 1 C, 2 B, 3 A.
type TSLexeme struct {
	Word string
	Pos  []TSPos
}

type TSPos struct {
	Pos    uint16
	Weight uint8
}

func tsvNormalize(in []tsLex) tsvec {
	sort.SliceStable(in, func(i, j int) bool { return in[i].word < in[j].word })
	var out tsvec
	for _, l := range in {
		if n := len(out); n > 0 && out[n-1].word == l.word {
			out[n-1].pos = append(out[n-1].pos, l.pos...)
			continue
		}
		out = append(out, tsLex{word: l.word, pos: append([]tsPos(nil), l.pos...)})
	}
	for i := range out {
		p := out[i].pos
		sort.SliceStable(p, func(a, b int) bool { return p[a].pos < p[b].pos })
		var u []tsPos
		for _, x := range p {
			if n := len(u); n > 0 && u[n-1].pos == x.pos {
				if x.w > u[n-1].w {
					u[n-1].w = x.w
				}
				continue
			}
			u = append(u, x)
		}
		if len(u) > 256 {
			u = u[:256]
		}
		out[i].pos = u
	}
	return out
}

func tsSyntax(kind, s string) error {
	return errf("42601", "syntax error in %s: %q", kind, s)
}

// tsvParse is tsvectorin.
func tsvParse(s string) (tsvec, error) {
	var lex []tsLex
	i := 0
	isSp := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' }
	for {
		for i < len(s) && isSp(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		var w strings.Builder
		if s[i] == '\'' {
			i++
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					w.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						w.WriteByte('\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				w.WriteByte(c)
				i++
			}
			if !closed {
				return nil, tsSyntax("tsvector", s)
			}
		} else {
			for i < len(s) && !isSp(s[i]) && s[i] != ':' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				w.WriteByte(s[i])
				i++
			}
			if w.Len() == 0 {
				return nil, tsSyntax("tsvector", s)
			}
		}
		l := tsLex{word: w.String()}
		if i < len(s) && s[i] == ':' {
			i++
			for {
				st := i
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
				if i == st {
					return nil, tsSyntax("tsvector", s)
				}
				n, _ := strconv.Atoi(s[st:i])
				if n == 0 {
					return nil, errf("42601", "wrong position info in tsvector: %q", s)
				}
				if n > 16383 {
					n = 16383
				}
				p := tsPos{pos: uint16(n)}
				if i < len(s) {
					switch s[i] {
					case 'a', 'A':
						p.w = 3
						i++
					case 'b', 'B':
						p.w = 2
						i++
					case 'c', 'C':
						p.w = 1
						i++
					case 'd', 'D':
						i++
					}
				}
				l.pos = append(l.pos, p)
				if i < len(s) && s[i] == ',' {
					i++
					continue
				}
				break
			}
		}
		if i < len(s) && !isSp(s[i]) {
			return nil, tsSyntax("tsvector", s)
		}
		lex = append(lex, l)
	}
	return tsvNormalize(lex), nil
}

func tsvConcat(a, b tsvec) tsvec {
	var maxA uint16
	for _, l := range a {
		for _, p := range l.pos {
			if p.pos > maxA {
				maxA = p.pos
			}
		}
	}
	in := append([]tsLex(nil), a...)
	for _, l := range b {
		nl := tsLex{word: l.word}
		for _, p := range l.pos {
			np := int(p.pos) + int(maxA)
			if np > 16383 {
				np = 16383
			}
			nl.pos = append(nl.pos, tsPos{pos: uint16(np), w: p.w})
		}
		in = append(in, nl)
	}
	return tsvNormalize(in)
}

// ---- tsquery ----

type tsq struct {
	op     byte // 'v' operand, '!', '&', '|', '<' (phrase)
	word   string
	prefix bool
	weight uint8 // mask: A=8 B=4 C=2 D=1
	dist   int
	l, r   *tsq
}

func tsqPrec(op byte) int {
	switch op {
	case '|':
		return 1
	case '&':
		return 2
	case '<':
		return 3
	case '!':
		return 4
	}
	return 5
}

func (q *tsq) text() string {
	if q == nil {
		return ""
	}
	var b strings.Builder
	q.write(&b)
	return b.String()
}

func (q *tsq) write(b *strings.Builder) {
	switch q.op {
	case 'v':
		b.WriteByte('\'')
		for i := 0; i < len(q.word); i++ {
			if c := q.word[i]; c == '\'' || c == '\\' {
				b.WriteByte(c)
			}
			b.WriteByte(q.word[i])
		}
		b.WriteByte('\'')
		if q.prefix || q.weight != 0 {
			b.WriteByte(':')
			if q.prefix {
				b.WriteByte('*')
			}
			for i, ch := range "ABCD" {
				if q.weight&(8>>uint(i)) != 0 {
					b.WriteRune(ch)
				}
			}
		}
	case '!':
		b.WriteByte('!')
		if q.l.op != 'v' && q.l.op != '!' {
			b.WriteString("( ")
			q.l.write(b)
			b.WriteString(" )")
		} else {
			q.l.write(b)
		}
	default:
		child := func(c *tsq, right bool) {
			paren := tsqPrec(c.op) < tsqPrec(q.op) || q.op == '<' && right && c.op == '<'
			if c.op == 'v' || c.op == '!' {
				paren = false
			}
			if paren {
				b.WriteString("( ")
			}
			c.write(b)
			if paren {
				b.WriteString(" )")
			}
		}
		child(q.l, false)
		switch q.op {
		case '&':
			b.WriteString(" & ")
		case '|':
			b.WriteString(" | ")
		default:
			if q.dist == 1 {
				b.WriteString(" <-> ")
			} else {
				b.WriteString(" <" + strconv.Itoa(q.dist) + "> ")
			}
		}
		child(q.r, true)
	}
}

type tsqParser struct {
	s    string
	i    int
	orig string
}

func (p *tsqParser) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *tsqParser) peekOp() (byte, int) { // operator at the cursor: '&', '|', '<' (phrase, distance), 0 none
	p.ws()
	if p.i >= len(p.s) {
		return 0, 0
	}
	switch c := p.s[p.i]; c {
	case '&', '|':
		return c, 1
	case '<':
		if strings.HasPrefix(p.s[p.i:], "<->") {
			return '<', 1
		}
		j := p.i + 1
		for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
			j++
		}
		if j > p.i+1 && j < len(p.s) && p.s[j] == '>' {
			n, _ := strconv.Atoi(p.s[p.i+1 : j])
			return '<', n
		}
	}
	return 0, 0
}

func (p *tsqParser) or() (*tsq, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for {
		op, _ := p.peekOp()
		if op != '|' {
			return l, nil
		}
		p.i++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = &tsq{op: '|', l: l, r: r}
	}
}

func (p *tsqParser) and() (*tsq, error) {
	l, err := p.phrase()
	if err != nil {
		return nil, err
	}
	for {
		op, _ := p.peekOp()
		if op != '&' {
			return l, nil
		}
		p.i++
		r, err := p.phrase()
		if err != nil {
			return nil, err
		}
		l = &tsq{op: '&', l: l, r: r}
	}
}

func (p *tsqParser) phrase() (*tsq, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		op, d := p.peekOp()
		if op != '<' {
			return l, nil
		}
		if strings.HasPrefix(p.s[p.i:], "<->") {
			p.i += 3
		} else {
			for p.s[p.i] != '>' {
				p.i++
			}
			p.i++
		}
		if d > 16384 {
			return nil, errf("22023", "distance in phrase operator must be an integer value between zero and 16384 inclusive")
		}
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = &tsq{op: '<', dist: d, l: l, r: r}
	}
}

func (p *tsqParser) unary() (*tsq, error) {
	p.ws()
	if p.i >= len(p.s) {
		return nil, errf("42601", "no operand in tsquery: %q", p.orig)
	}
	switch c := p.s[p.i]; {
	case c == '!':
		p.i++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &tsq{op: '!', l: x}, nil
	case c == '(':
		p.i++
		x, err := p.or()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, tsSyntax("tsquery", p.orig)
		}
		p.i++
		return x, nil
	case c == '&' || c == '|' || c == ')' || c == ':':
		return nil, errf("42601", "syntax error in tsquery: %q", p.orig)
	}
	var w strings.Builder
	if p.s[p.i] == '\'' {
		p.i++
		closed := false
		for p.i < len(p.s) {
			c := p.s[p.i]
			if c == '\\' && p.i+1 < len(p.s) {
				w.WriteByte(p.s[p.i+1])
				p.i += 2
				continue
			}
			if c == '\'' {
				if p.i+1 < len(p.s) && p.s[p.i+1] == '\'' {
					w.WriteByte('\'')
					p.i += 2
					continue
				}
				p.i++
				closed = true
				break
			}
			w.WriteByte(c)
			p.i++
		}
		if !closed {
			return nil, tsSyntax("tsquery", p.orig)
		}
	} else {
		for p.i < len(p.s) && strings.IndexByte(" \t\n\r&|!()<:", p.s[p.i]) < 0 {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			w.WriteByte(p.s[p.i])
			p.i++
		}
		if w.Len() == 0 {
			return nil, tsSyntax("tsquery", p.orig)
		}
	}
	q := &tsq{op: 'v', word: w.String()}
	if p.i < len(p.s) && p.s[p.i] == ':' {
		p.i++
		for p.i < len(p.s) {
			switch c := p.s[p.i]; c {
			case '*':
				q.prefix = true
			case 'a', 'A':
				q.weight |= 8
			case 'b', 'B':
				q.weight |= 4
			case 'c', 'C':
				q.weight |= 2
			case 'd', 'D':
				q.weight |= 1
			default:
				return q, nil
			}
			p.i++
		}
	}
	return q, nil
}

// tsqParse is tsquery_in; an empty text is the empty query (nil).
func tsqParse(s string) (*tsq, error) {
	p := &tsqParser{s: s, orig: s}
	p.ws()
	if p.i >= len(p.s) {
		return nil, nil
	}
	q, err := p.or()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i < len(p.s) {
		return nil, tsSyntax("tsquery", s)
	}
	return q, nil
}

// TSQueryParse reads tsquery input and returns the canonical value.
func TSQueryParse(s string) (Datum, error) {
	q, err := tsqParse(s)
	if err != nil {
		return Null, err
	}
	return Datum{K: KText, D: tagTSQuery, S: q.text()}, nil
}

// TSVectorParse reads tsvector input and returns the canonical value.
func TSVectorParse(s string) (Datum, error) {
	v, err := tsvParse(s)
	if err != nil {
		return Null, err
	}
	return Datum{K: KText, D: tagTSVector, S: v.text()}, nil
}

func (q *tsq) nodes() int {
	if q == nil {
		return 0
	}
	return 1 + q.l.nodes() + q.r.nodes()
}

// ---- matching ----

type tsRes struct {
	ok    bool
	pos   []int
	nopos bool // matched an entry that carries no positions
}

func unionPos(a, b []int) []int {
	m := map[int]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		m[x] = true
	}
	out := make([]int, 0, len(m))
	for x := range m {
		out = append(out, x)
	}
	sort.Ints(out)
	return out
}

func interPos(a, b []int) []int {
	m := map[int]bool{}
	for _, x := range a {
		m[x] = true
	}
	var out []int
	for _, x := range b {
		if m[x] {
			out = append(out, x)
		}
	}
	return out
}

// tsvCompare is tsvector_cmp: by the number of lexemes, then lexeme by lexeme (word, positions).
func tsvCompare(a, b tsvec) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	for i := range a {
		if c := strings.Compare(a[i].word, b[i].word); c != 0 {
			return c
		}
		pa, pb := a[i].pos, b[i].pos
		if (len(pa) > 0) != (len(pb) > 0) {
			if len(pa) > 0 {
				return -1
			}
			return 1
		}
		if len(pa) != len(pb) {
			if len(pa) < len(pb) {
				return -1
			}
			return 1
		}
		for j := range pa {
			x, y := uint16(pa[j].w)<<14|pa[j].pos, uint16(pb[j].w)<<14|pb[j].pos
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}

func (v tsvec) eval(q *tsq) tsRes {
	switch q.op {
	case 'v':
		var res tsRes
		i := sort.Search(len(v), func(i int) bool { return v[i].word >= q.word })
		for ; i < len(v); i++ {
			l := v[i]
			if q.prefix {
				if !strings.HasPrefix(l.word, q.word) {
					break
				}
			} else if l.word != q.word {
				break
			}
			if len(l.pos) == 0 {
				if q.weight == 0 || q.weight&1 != 0 {
					res.ok, res.nopos = true, true
				}
				continue
			}
			for _, p := range l.pos {
				if q.weight == 0 || q.weight&(1<<p.w) != 0 {
					res.ok = true
					res.pos = append(res.pos, int(p.pos))
				}
			}
		}
		sort.Ints(res.pos)
		return res
	case '!':
		return tsRes{ok: !v.eval(q.l).ok}
	case '&':
		a, b := v.eval(q.l), v.eval(q.r)
		return tsRes{ok: a.ok && b.ok, pos: interPos(a.pos, b.pos), nopos: a.nopos || b.nopos}
	case '|':
		a, b := v.eval(q.l), v.eval(q.r)
		return tsRes{ok: a.ok || b.ok, pos: unionPos(a.pos, b.pos), nopos: a.nopos || b.nopos}
	}
	a, b := v.eval(q.l), v.eval(q.r)
	if !a.ok || !b.ok || len(a.pos) == 0 || len(b.pos) == 0 {
		return tsRes{}
	}
	var out []int
	for _, y := range b.pos {
		for _, x := range a.pos {
			if y-x == q.dist {
				out = append(out, y)
				break
			}
		}
	}
	return tsRes{ok: len(out) > 0, pos: out}
}

func (q *tsq) operands(m map[string]bool) {
	if q == nil {
		return
	}
	if q.op == 'v' {
		m[q.word] = true
	}
	q.l.operands(m)
	q.r.operands(m)
}

// clean is querytree's removal of NOT subtrees: (node, true when the whole subtree became "true").
func (q *tsq) clean() (*tsq, bool) {
	switch q.op {
	case 'v':
		return q, false
	case '!':
		return nil, true
	}
	l, lt := q.l.clean()
	r, rt := q.r.clean()
	switch q.op {
	case '|':
		if lt || rt {
			return nil, true
		}
	default:
		if lt && rt {
			return nil, true
		}
		if lt {
			return r, false
		}
		if rt {
			return l, false
		}
	}
	return &tsq{op: q.op, dist: q.dist, l: l, r: r}, false
}

// ---- binary ----

func tsvBinaryOut(v tsvec) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(v)))
	for _, l := range v {
		b = append(b, l.word...)
		b = append(b, 0)
		b = binary.BigEndian.AppendUint16(b, uint16(len(l.pos)))
		for _, p := range l.pos {
			b = binary.BigEndian.AppendUint16(b, uint16(p.w)<<14|p.pos)
		}
	}
	return b
}

func tsvFromBinary(raw []byte) (tsvec, bool) {
	if len(raw) < 4 {
		return nil, false
	}
	n := int(binary.BigEndian.Uint32(raw))
	p := 4
	var lex []tsLex
	for i := 0; i < n; i++ {
		e := strings.IndexByte(string(raw[p:]), 0)
		if e < 0 || p+e+3 > len(raw) {
			return nil, false
		}
		l := tsLex{word: string(raw[p : p+e])}
		p += e + 1
		np := int(binary.BigEndian.Uint16(raw[p:]))
		p += 2
		if p+2*np > len(raw) {
			return nil, false
		}
		for k := 0; k < np; k++ {
			x := binary.BigEndian.Uint16(raw[p:])
			p += 2
			l.pos = append(l.pos, tsPos{pos: x & 0x3fff, w: uint8(x >> 14)})
		}
		lex = append(lex, l)
	}
	if p != len(raw) {
		return nil, false
	}
	return tsvNormalize(lex), true
}

func (q *tsq) binary(b []byte) []byte {
	switch q.op {
	case 'v':
		b = append(b, 1, q.weight)
		pf := byte(0)
		if q.prefix {
			pf = 1
		}
		b = append(b, pf)
		b = append(b, q.word...)
		return append(b, 0)
	case '!':
		b = append(b, 2, 1)
		return q.l.binary(b)
	}
	code := map[byte]byte{'&': 2, '|': 3, '<': 4}[q.op]
	b = append(b, 2, code)
	if q.op == '<' {
		b = binary.BigEndian.AppendUint16(b, uint16(q.dist))
	}
	b = q.r.binary(b)
	return q.l.binary(b)
}

func tsqBinaryOut(q *tsq) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(q.nodes()))
	if q == nil {
		return b
	}
	return q.binary(b)
}

func tsqFromBinary(raw []byte) (*tsq, bool) {
	if len(raw) < 4 {
		return nil, false
	}
	n := int(binary.BigEndian.Uint32(raw))
	if n == 0 {
		return nil, len(raw) == 4
	}
	p := 4
	var read func() (*tsq, bool)
	count := 0
	read = func() (*tsq, bool) {
		count++
		if count > n || p >= len(raw) {
			return nil, false
		}
		t := raw[p]
		p++
		if t == 1 {
			if p+2 >= len(raw) {
				return nil, false
			}
			q := &tsq{op: 'v', weight: raw[p], prefix: raw[p+1] != 0}
			p += 2
			e := strings.IndexByte(string(raw[p:]), 0)
			if e < 0 {
				return nil, false
			}
			q.word = string(raw[p : p+e])
			p += e + 1
			return q, true
		}
		if p >= len(raw) {
			return nil, false
		}
		code := raw[p]
		p++
		if code == 1 {
			x, ok := read()
			return &tsq{op: '!', l: x}, ok
		}
		q := &tsq{op: map[byte]byte{2: '&', 3: '|', 4: '<'}[code]}
		if q.op == 0 {
			return nil, false
		}
		if code == 4 {
			if p+2 > len(raw) {
				return nil, false
			}
			q.dist = int(binary.BigEndian.Uint16(raw[p:]))
			p += 2
		}
		var ok1, ok2 bool
		q.r, ok1 = read()
		q.l, ok2 = read()
		return q, ok1 && ok2
	}
	q, ok := read()
	return q, ok && p == len(raw) && count == n
}

// ---- operators and functions ----

// tsOperand reads d as a tsvector / tsquery (tagged, or untagged text read as the other side's type).
func tsvOperand(d Datum) (tsvec, error) {
	if d.K != KText {
		return nil, errf(codeUndefinedFunction, "operator does not exist")
	}
	return tsvParse(d.S)
}

func tsqOperand(d Datum) (*tsq, error) {
	if d.K != KText {
		return nil, errf(codeUndefinedFunction, "operator does not exist")
	}
	return tsqParse(d.S)
}

func tsqD(q *tsq) Datum { return Datum{K: KText, D: tagTSQuery, S: q.text()} }

func tsqCombine(op byte, a, b *tsq, dist int) *tsq {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return &tsq{op: op, dist: dist, l: a, r: b}
}

// tsBinary evaluates an operator over tsvector/tsquery operands; ok=false when neither is one.
func tsBinary(op string, l, r Datum) (Datum, bool, error) {
	if !isTSV(l) && !isTSQ(l) && !isTSV(r) && !isTSQ(r) {
		return Null, false, nil
	}
	switch op {
	case "||":
		if isTSV(l) || isTSV(r) {
			a, err := tsvOperand(l)
			if err != nil {
				return Null, true, err
			}
			b, err := tsvOperand(r)
			if err != nil {
				return Null, true, err
			}
			return Datum{K: KText, D: tagTSVector, S: tsvConcat(a, b).text()}, true, nil
		}
		fallthrough
	case "&&", "stask_tsphrase":
		if !isTSQ(l) && !isTSQ(r) {
			return Null, false, nil
		}
		a, err := tsqOperand(l)
		if err != nil {
			return Null, true, err
		}
		b, err := tsqOperand(r)
		if err != nil {
			return Null, true, err
		}
		switch op {
		case "&&":
			return tsqD(tsqCombine('&', a, b, 0)), true, nil
		case "||":
			return tsqD(tsqCombine('|', a, b, 0)), true, nil
		}
		return tsqD(tsqCombine('<', a, b, 1)), true, nil
	case "@@":
		var v tsvec
		var q *tsq
		var err error
		if isTSV(l) || isTSQ(r) {
			if v, err = tsvOperand(l); err == nil {
				q, err = tsqOperand(r)
			}
		} else {
			if v, err = tsvOperand(r); err == nil {
				q, err = tsqOperand(l)
			}
		}
		if err != nil {
			return Null, true, err
		}
		if q == nil {
			return BoolD(false), true, nil
		}
		return BoolD(v.eval(q).ok), true, nil
	case "@>", "<@":
		a, err := tsqOperand(l)
		if err != nil {
			return Null, true, err
		}
		b, err := tsqOperand(r)
		if err != nil {
			return Null, true, err
		}
		if op == "<@" {
			a, b = b, a
		}
		ma, mb := map[string]bool{}, map[string]bool{}
		a.operands(ma)
		b.operands(mb)
		for w := range mb {
			if !ma[w] {
				return BoolD(false), true, nil
			}
		}
		return BoolD(true), true, nil
	case "=", "<>", "<", "<=", ">", ">=":
		return Null, false, nil
	}
	return Null, false, nil
}

// tsFunc runs the functions of the two types; ok=false if args[0] is neither.
func tsFunc(name string, args []Datum) (Datum, bool, error) {
	if len(args) == 0 {
		return Null, false, nil
	}
	a := args[0]
	switch name {
	case "length":
		if len(args) == 1 && isTSV(a) {
			v, err := tsvOperand(a)
			return IntD(int64(len(v))), true, err
		}
	case "numnode":
		if len(args) == 1 && isTSQ(a) {
			q, err := tsqOperand(a)
			return IntD(int64(q.nodes())), true, err
		}
	case "querytree":
		if len(args) == 1 && isTSQ(a) {
			q, err := tsqOperand(a)
			if err != nil {
				return Null, true, err
			}
			if q == nil {
				return TextD(""), true, nil
			}
			c, isTrue := q.clean()
			if isTrue || c == nil {
				return TextD("T"), true, nil
			}
			return TextD(c.text()), true, nil
		}
	case "stask_tsnot":
		if len(args) == 1 && (isTSQ(a) || a.K == KText) {
			q, err := tsqOperand(a)
			if err != nil {
				return Null, true, err
			}
			if q == nil {
				return a, true, nil
			}
			return tsqD(&tsq{op: '!', l: q}), true, nil
		}
	}
	return Null, false, nil
}
