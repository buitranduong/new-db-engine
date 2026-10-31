package sql

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Full text search functions (PostgreSQL 16 chapter 12 / section 9.13): to_tsvector, to_tsquery,
// plainto_tsquery, phraseto_tsquery, websearch_to_tsquery, the @@ match, ts_rank, ts_rank_cd,
// ts_headline, setweight, strip, length, numnode, tsvector_to_array, array_to_tsvector, ts_lexize.
//
// Values are carried in their PostgreSQL text form (Datum KText, F = 3614 for tsvector and 3615 for
// tsquery); the text-search types themselves belong to branch compat-types. Configurations: 'simple'
// (lower-casing) and 'english' (stop words of english.stop and the Porter2 stemmer in fn_stem.go);
// others report 42704. The parser is the default parser reduced to words, numbers, hyphenated
// compounds, hosts, e-mail addresses, versions and signed numbers (no URLs or file paths).
// Precedent: PostgreSQL docs chapter 12, src/backend/tsearch (wparser_def.c, to_tsany.c, tsrank.c,
// tsquery.c), checked against postgres:16 in suite group 17.

const (
	tyTSVector = 3614
	tyTSQuery  = 3615
)

func tsvD(s string) Datum { return Datum{K: KText, S: s, F: tyTSVector} }
func tsqD(s string) Datum { return Datum{K: KText, S: s, F: tyTSQuery} }

var englishStop = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`i me my myself we our ours ourselves you your yours yourself yourselves he him his himself she her hers herself it its itself they them their theirs themselves what which who whom this that these those am is are was were be been being have has had having do does did doing a an the and but if or because as until while of at by for with about against between into through during before after above below to from up down in out on off over under again further then once here there when where why how all any both each few more most other some such no nor not only own same so than too very s t can will just don should now`) {
		m[w] = true
	}
	return m
}()

// ---- configurations and the parser ----

type tsConfig int

const (
	cfgSimple tsConfig = iota
	cfgEnglish
)

func (c *evalCtx) tsDefaultConfig() tsConfig {
	if s := c.session(); s != nil {
		if v, ok := s.vars["default_text_search_config"]; ok {
			if cf, err := parseTSConfig(v); err == nil {
				return cf
			}
		}
	}
	return cfgEnglish
}

func parseTSConfig(name string) (tsConfig, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimPrefix(n, "pg_catalog.")
	n = strings.Trim(n, `"`)
	switch n {
	case "simple":
		return cfgSimple, nil
	case "english":
		return cfgEnglish, nil
	}
	return 0, errf(codeUndefinedObject, "text search configuration \"%s\" does not exist", name)
}

type tsTok struct {
	text   string
	class  string // word, num, int, host, email, version, compound
	member bool   // a part of the compound token before it
}

func isAlnumRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// tsTokens splits text into the tokens the default parser would emit (a reduced class set).
func tsTokens(text string) []tsTok {
	rs := []rune(text)
	var out []tsTok
	i := 0
	n := len(rs)
	runEnd := func(j int) int {
		for j < n && isAlnumRune(rs[j]) {
			j++
		}
		return j
	}
	classOf := func(s string) string {
		letters, digits := false, false
		for _, r := range s {
			if unicode.IsLetter(r) {
				letters = true
			} else if unicode.IsDigit(r) {
				digits = true
			}
		}
		switch {
		case letters && digits:
			return "num"
		case letters:
			return "word"
		}
		_ = digits
		return "int"
	}
	for i < n {
		r := rs[i]
		if !isAlnumRune(r) {
			// a signed number: "-20" after a non-alphanumeric
			if (r == '-' || r == '+') && i+1 < n && unicode.IsDigit(rs[i+1]) {
				j := runEnd(i + 1)
				if allDigits(rs[i+1 : j]) {
					out = append(out, tsTok{text: string(rs[i:j]), class: "int"})
					i = j
					continue
				}
			}
			i++
			continue
		}
		j := runEnd(i)
		first := string(rs[i:j])
		// e-mail
		if j < n && rs[j] == '@' && j+1 < n && isAlnumRune(rs[j+1]) {
			k := j + 1
			k = runEnd(k)
			dots := 0
			for k+1 < n && (rs[k] == '.' || rs[k] == '-') && isAlnumRune(rs[k+1]) {
				if rs[k] == '.' {
					dots++
				}
				k = runEnd(k + 1)
			}
			if dots > 0 {
				out = append(out, tsTok{text: string(rs[i:k]), class: "email"})
				i = k
				continue
			}
		}
		// dotted: float, version, host
		if j+1 < n && rs[j] == '.' && isAlnumRune(rs[j+1]) {
			segs := []string{first}
			k := j
			for k+1 < n && rs[k] == '.' && isAlnumRune(rs[k+1]) {
				e := runEnd(k + 1)
				segs = append(segs, string(rs[k+1:e]))
				k = e
			}
			allNum := true
			for _, s := range segs {
				if classOf(s) != "int" {
					allNum = false
				}
			}
			last := segs[len(segs)-1]
			switch {
			case allNum && len(segs) == 2:
				out = append(out, tsTok{text: string(rs[i:k]), class: "float"})
				i = k
				continue
			case allNum:
				out = append(out, tsTok{text: string(rs[i:k]), class: "version"})
				i = k
				continue
			case classOf(last) == "word" && utf8.RuneCountInString(last) >= 2:
				out = append(out, tsTok{text: string(rs[i:k]), class: "host"})
				i = k
				continue
			}
		}
		// hyphenated compound
		if j+1 < n && rs[j] == '-' && isAlnumRune(rs[j+1]) {
			parts := []string{first}
			k := j
			for k+1 < n && rs[k] == '-' && isAlnumRune(rs[k+1]) {
				e := runEnd(k + 1)
				if classOf(string(rs[k+1:e])) == "int" {
					break
				}
				parts = append(parts, string(rs[k+1:e]))
				k = e
			}
			hasLetter := len(parts) > 1 && classOf(parts[0]) != "int"
			if hasLetter {
				out = append(out, tsTok{text: string(rs[i:k]), class: "compound"})
				for _, p := range parts {
					out = append(out, tsTok{text: p, class: classOf(p), member: true})
				}
				i = k
				continue
			}
		}
		out = append(out, tsTok{text: first, class: classOf(first)})
		i = j
	}
	return out
}

func allDigits(rs []rune) bool {
	for _, r := range rs {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(rs) > 0
}

// normalize applies the configuration's dictionary to a token; keep is false for a stop word.
func (cf tsConfig) normalize(t tsTok) (string, bool) {
	w := strings.ToLower(t.text)
	if cf == cfgEnglish {
		switch t.class {
		case "word", "num", "compound":
			if englishStop[w] {
				return "", false
			}
			return englishStem(w), true
		}
	}
	return w, true
}

// ---- tsvector ----

type tsPos struct {
	p  int
	wt byte // 0 D, 1 C, 2 B, 3 A
}

type tsLex struct {
	w   string
	pos []tsPos
}

func (cf tsConfig) toVector(text string) []tsLex {
	m := map[string]*tsLex{}
	pos := 0
	for _, t := range tsTokens(text) {
		pos++
		lex, keep := cf.normalize(t)
		if !keep || lex == "" {
			continue
		}
		l := m[lex]
		if l == nil {
			l = &tsLex{w: lex}
			m[lex] = l
		}
		p := pos
		if p > 16383 {
			p = 16383
		}
		if len(l.pos) < 256 && (len(l.pos) == 0 || l.pos[len(l.pos)-1].p != p) {
			l.pos = append(l.pos, tsPos{p: p})
		}
	}
	return sortedVector(m)
}

func sortedVector(m map[string]*tsLex) []tsLex {
	out := make([]tsLex, 0, len(m))
	for _, l := range m {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].w < out[j].w })
	return out
}

func quoteLex(w string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(w, `\`, `\\`), "'", "''") + "'"
}

func vectorString(v []tsLex) string {
	var b strings.Builder
	for i, l := range v {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quoteLex(l.w))
		for j, p := range l.pos {
			if j == 0 {
				b.WriteByte(':')
			} else {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(p.p))
			if p.wt > 0 {
				b.WriteByte("DCBA"[p.wt])
			}
		}
	}
	return b.String()
}

// parseVector reads the text form of a tsvector.
func parseVector(s string) ([]tsLex, error) {
	bad := func() ([]tsLex, error) {
		return nil, errf("42601", "syntax error in tsvector: \"%s\"", s)
	}
	m := map[string]*tsLex{}
	i := 0
	for {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		var w strings.Builder
		if s[i] == '\'' {
			i++
			for {
				if i >= len(s) {
					return bad()
				}
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						w.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				w.WriteByte(s[i])
				i++
			}
		} else {
			for i < len(s) && s[i] != ' ' && s[i] != ':' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				w.WriteByte(s[i])
				i++
			}
		}
		l := m[w.String()]
		if l == nil {
			l = &tsLex{w: w.String()}
			m[w.String()] = l
		}
		if i < len(s) && s[i] == ':' {
			i++
			for {
				st := i
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
				if st == i {
					return bad()
				}
				p, _ := strconv.Atoi(s[st:i])
				wt := byte(0)
				if i < len(s) && strings.IndexByte("ABCDabcd", s[i]) >= 0 {
					wt = byte(strings.IndexByte("dcba", s[i]|0x20))
					i++
				}
				l.pos = append(l.pos, tsPos{min(p, 16383), wt})
				if i < len(s) && s[i] == ',' {
					i++
					continue
				}
				break
			}
		}
	}
	v := sortedVector(m)
	for k := range v {
		sort.SliceStable(v[k].pos, func(a, b int) bool { return v[k].pos[a].p < v[k].pos[b].p })
	}
	return v, nil
}

func vecOf(d Datum) ([]tsLex, error) {
	if d.F != tyTSVector && d.K == KText && d.F != 0 {
		return nil, errf(codeUndefinedFunction, "function does not exist for a tsquery argument")
	}
	return parseVector(d.S)
}

// ---- tsquery ----

type tsq struct {
	op     byte // 'v' operand, '!' not, '&' and, '|' or, '<' phrase
	w      string
	prefix bool
	mask   byte // weights: 1 D, 2 C, 4 B, 8 A; 0 = any
	dist   int
	l, r   *tsq
}

func prio(n *tsq) int {
	switch n.op {
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

func (n *tsq) infix(b *strings.Builder, parent int, right bool) {
	switch n.op {
	case 'v':
		b.WriteString(quoteLex(n.w))
		if n.prefix || n.mask != 0 {
			b.WriteByte(':')
			if n.prefix {
				b.WriteByte('*')
			}
			for i, c := range "DCBA" {
				if n.mask&(1<<uint(i)) != 0 {
					b.WriteRune(c)
				}
			}
		}
		return
	case '!':
		p := prio(n)
		if p < parent {
			b.WriteString("( ")
		}
		b.WriteByte('!')
		n.l.infix(b, p, false)
		if p < parent {
			b.WriteString(" )")
		}
		return
	}
	p := prio(n)
	paren := p < parent || n.op == '<' && p == parent && right
	if paren {
		b.WriteString("( ")
	}
	n.l.infix(b, p, false)
	switch n.op {
	case '&':
		b.WriteString(" & ")
	case '|':
		b.WriteString(" | ")
	default:
		if n.dist == 1 {
			b.WriteString(" <-> ")
		} else {
			b.WriteString(" <" + strconv.Itoa(n.dist) + "> ")
		}
	}
	n.r.infix(b, p, true)
	if paren {
		b.WriteString(" )")
	}
}

func (n *tsq) String() string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	n.infix(&b, 0, false)
	return b.String()
}

func tsqErr(kind, s string) error {
	return errf("42601", "%s in tsquery: \"%s\"", kind, s)
}

type tsqParser struct {
	s      string
	i      int
	cf     tsConfig
	norm   bool // normalize operands through the configuration
	carry  int
	lastOK bool
}

func (p *tsqParser) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n') {
		p.i++
	}
}

// parse returns the tree, or nil (with ok) when only stop words remain.
func (p *tsqParser) parseOr() (*tsq, bool, error) {
	l, lok, err := p.parseAnd()
	if err != nil {
		return nil, false, err
	}
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '|' {
			p.i++
			r, rok, err := p.parseAnd()
			if err != nil {
				return nil, false, err
			}
			l, lok = tsqCombine('|', l, lok, r, rok, 0), lok || rok
			continue
		}
		return l, lok, nil
	}
}

func tsqCombine(op byte, l *tsq, lok bool, r *tsq, rok bool, dist int) *tsq {
	switch {
	case !lok && !rok:
		return nil
	case !lok:
		return r
	case !rok:
		return l
	}
	return &tsq{op: op, l: l, r: r, dist: dist}
}

func (p *tsqParser) parseAnd() (*tsq, bool, error) {
	l, lok, err := p.parsePhrase()
	if err != nil {
		return nil, false, err
	}
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '&' {
			p.i++
			r, rok, err := p.parsePhrase()
			if err != nil {
				return nil, false, err
			}
			l, lok = tsqCombine('&', l, lok, r, rok, 0), lok || rok
			continue
		}
		return l, lok, nil
	}
}

func (p *tsqParser) parsePhrase() (*tsq, bool, error) {
	l, lok, err := p.parseNot()
	if err != nil {
		return nil, false, err
	}
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '<' {
			j := p.i + 1
			dist := 1
			if j < len(p.s) && p.s[j] == '-' {
				j++
			} else {
				st := j
				for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
					j++
				}
				if st == j {
					return nil, false, tsqErr("syntax error", p.s)
				}
				dist, _ = strconv.Atoi(p.s[st:j])
				if dist > 16384 {
					return nil, false, errf("22023", "distance in phrase operator must not be greater than 16384")
				}
			}
			if j >= len(p.s) || p.s[j] != '>' {
				return nil, false, tsqErr("syntax error", p.s)
			}
			p.i = j + 1
			r, rok, err := p.parseNot()
			if err != nil {
				return nil, false, err
			}
			switch {
			case lok && rok:
				l = &tsq{op: '<', l: l, r: r, dist: dist + p.carry}
				p.carry = 0
			case lok && !rok:
				// the stopped operand widens the next distance
				p.carry += dist
			case !lok && rok:
				l, lok = r, true
				continue
			}
			lok = lok || rok
			continue
		}
		return l, lok, nil
	}
}

func (p *tsqParser) parseNot() (*tsq, bool, error) {
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '!' {
		p.i++
		x, ok, err := p.parseNot()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		return &tsq{op: '!', l: x}, true, nil
	}
	return p.parseAtom()
}

func (p *tsqParser) parseAtom() (*tsq, bool, error) {
	p.ws()
	if p.i >= len(p.s) {
		return nil, false, tsqErr("no operand", p.s)
	}
	c := p.s[p.i]
	if c == '(' {
		p.i++
		x, ok, err := p.parseOr()
		if err != nil {
			return nil, false, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, false, tsqErr("syntax error", p.s)
		}
		p.i++
		return x, ok, nil
	}
	if strings.IndexByte("&|)<:*", c) >= 0 {
		return nil, false, tsqErr("syntax error", p.s)
	}
	var w strings.Builder
	if c == '\'' {
		p.i++
		for {
			if p.i >= len(p.s) {
				return nil, false, tsqErr("syntax error", p.s)
			}
			if p.s[p.i] == '\'' {
				if p.i+1 < len(p.s) && p.s[p.i+1] == '\'' {
					w.WriteByte('\'')
					p.i += 2
					continue
				}
				p.i++
				break
			}
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			w.WriteByte(p.s[p.i])
			p.i++
		}
	} else {
		for p.i < len(p.s) && strings.IndexByte(" \t\n&|!()<:", p.s[p.i]) < 0 {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			w.WriteByte(p.s[p.i])
			p.i++
		}
	}
	prefix, mask := false, byte(0)
	if p.i < len(p.s) && p.s[p.i] == ':' {
		p.i++
		for p.i < len(p.s) && strings.IndexByte("*ABCDabcd", p.s[p.i]) >= 0 {
			switch ch := p.s[p.i] | 0x20; ch {
			case '*' | 0x20:
				prefix = true
			default:
				mask |= 1 << uint(strings.IndexByte("dcba", ch))
			}
			p.i++
		}
	}
	text := w.String()
	if !p.norm {
		return &tsq{op: 'v', w: text, prefix: prefix, mask: mask}, true, nil
	}
	return p.operand(text, prefix, mask), p.lastOK, nil
}

// operand normalizes one query word; nil (lastOK false) when it is a stop word.
func (p *tsqParser) operand(text string, prefix bool, mask byte) *tsq {
	var parts []*tsq
	var pos []int
	n := 0
	for _, t := range tsTokens(text) {
		n++
		lex, keep := p.cf.normalize(t)
		if !keep || lex == "" {
			continue
		}
		parts = append(parts, &tsq{op: 'v', w: lex, mask: mask})
		pos = append(pos, n)
	}
	if len(parts) == 0 {
		p.lastOK = false
		return nil
	}
	parts[len(parts)-1].prefix = prefix
	res := parts[0]
	for k := 1; k < len(parts); k++ {
		res = &tsq{op: '<', l: res, r: parts[k], dist: pos[k] - pos[k-1]}
	}
	p.lastOK = true
	return res
}

// parseToTSQuery is to_tsquery(config, text).
func parseToTSQuery(cf tsConfig, text string) (*tsq, error) {
	p := &tsqParser{s: text, cf: cf, norm: true}
	p.ws()
	if p.i >= len(p.s) {
		return nil, nil
	}
	x, ok, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i < len(p.s) {
		return nil, tsqErr("syntax error", text)
	}
	if !ok {
		return nil, nil
	}
	return x, nil
}

// plainQuery builds plainto_tsquery (and: true) or phraseto_tsquery (and: false).
func plainQuery(cf tsConfig, text string, and bool) *tsq {
	var parts []*tsq
	var pos []int
	n := 0
	for _, t := range tsTokens(text) {
		n++
		lex, keep := cf.normalize(t)
		if !keep || lex == "" {
			continue
		}
		parts = append(parts, &tsq{op: 'v', w: lex})
		pos = append(pos, n)
	}
	if len(parts) == 0 {
		return nil
	}
	res := parts[0]
	for k := 1; k < len(parts); k++ {
		if and {
			res = &tsq{op: '&', l: res, r: parts[k]}
		} else {
			res = &tsq{op: '<', l: res, r: parts[k], dist: pos[k] - pos[k-1]}
		}
	}
	return res
}

// webQuery is websearch_to_tsquery: words ANDed, "phrases", `or`, and -negation.
func webQuery(cf tsConfig, text string) *tsq {
	type term struct {
		neg  bool
		q    *tsq
		isOr bool
	}
	var terms []term
	rs := []rune(text)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			terms = append(terms, term{q: plainQuery(cf, string(rs[i+1:j]), false)})
			i = j + 1
		case r == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]):
			j := i + 1
			if rs[j] == '"' {
				k := j + 1
				for k < len(rs) && rs[k] != '"' {
					k++
				}
				terms = append(terms, term{neg: true, q: plainQuery(cf, string(rs[j+1:k]), false)})
				i = k + 1
				continue
			}
			for j < len(rs) && !unicode.IsSpace(rs[j]) {
				j++
			}
			terms = append(terms, term{neg: true, q: plainQuery(cf, string(rs[i+1:j]), true)})
			i = j
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '"' {
				j++
			}
			w := string(rs[i:j])
			if strings.EqualFold(w, "or") && !(len(terms) > 0 && terms[len(terms)-1].isOr) {
				terms = append(terms, term{isOr: true})
			} else {
				terms = append(terms, term{q: plainQuery(cf, w, false)})
			}
			i = j
		}
	}
	var res, group *tsq
	flush := func() {
		if group != nil {
			if res == nil {
				res = group
			} else {
				res = &tsq{op: '|', l: res, r: group}
			}
			group = nil
		}
	}
	for _, t := range terms {
		if t.isOr {
			flush()
			continue
		}
		if t.q == nil {
			continue
		}
		q := t.q
		if t.neg {
			q = &tsq{op: '!', l: q}
		}
		if group == nil {
			group = q
		} else {
			group = &tsq{op: '&', l: group, r: q}
		}
	}
	flush()
	return res
}

func qString(q *tsq) string { return q.String() }

func queryOf(d Datum) (*tsq, error) {
	p := &tsqParser{s: d.S}
	p.ws()
	if p.i >= len(p.s) {
		return nil, nil
	}
	x, _, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i < len(p.s) {
		return nil, tsqErr("syntax error", d.S)
	}
	return x, nil
}

// ---- matching ----

func lexMatches(v []tsLex, n *tsq) []tsLex {
	var out []tsLex
	if n.prefix {
		for _, l := range v {
			if strings.HasPrefix(l.w, n.w) {
				out = append(out, l)
			}
		}
		return out
	}
	i := sort.Search(len(v), func(i int) bool { return v[i].w >= n.w })
	if i < len(v) && v[i].w == n.w {
		out = append(out, v[i])
	}
	return out
}

// exec evaluates the query on the vector: whether it matches and, for phrase operators, the
// positions where the matched expression ends.
func (n *tsq) exec(v []tsLex) (bool, []int) {
	switch n.op {
	case 'v':
		var ps []int
		ok := false
		for _, l := range lexMatches(v, n) {
			if len(l.pos) == 0 {
				if n.mask == 0 {
					ok = true
				}
				continue
			}
			for _, p := range l.pos {
				if n.mask == 0 || n.mask&(1<<p.wt) != 0 {
					ok = true
					ps = append(ps, p.p)
				}
			}
		}
		sort.Ints(ps)
		return ok, ps
	case '!':
		ok, _ := n.l.exec(v)
		return !ok, nil
	case '&':
		a, pa := n.l.exec(v)
		b, pb := n.r.exec(v)
		return a && b, mergePos(pa, pb)
	case '|':
		a, pa := n.l.exec(v)
		b, pb := n.r.exec(v)
		return a || b, mergePos(pa, pb)
	}
	a, pa := n.l.exec(v)
	b, pb := n.r.exec(v)
	if !a || !b {
		return false, nil
	}
	var out []int
	for _, y := range pb {
		for _, x := range pa {
			if y-x == n.dist {
				out = append(out, y)
				break
			}
		}
	}
	return len(out) > 0, out
}

func mergePos(a, b []int) []int {
	out := append(append([]int(nil), a...), b...)
	sort.Ints(out)
	k := 0
	for i, x := range out {
		if i == 0 || x != out[i-1] {
			out[k] = x
			k++
		}
	}
	return out[:k]
}

func tsMatch(v []tsLex, q *tsq) bool {
	if q == nil {
		return false
	}
	ok, _ := q.exec(v)
	return ok
}

// ---- ranking ----

var defRankWeights = [4]float32{0.1, 0.2, 0.4, 1.0}

func rankWeights(a []Datum) ([4]float32, error) {
	w := defRankWeights
	if len(a) == 0 {
		return w, nil
	}
	arr, err := arrOf(a[0], KFloat)
	if err != nil {
		return w, err
	}
	if len(arr.el) < 4 {
		return w, errf("2202E", "array of weight is too short")
	}
	for i := 0; i < 4; i++ {
		if arr.el[i].IsNull() {
			return w, errf("22004", "array of weight must not contain nulls")
		}
		f, err := fnFloat(arr.el[i])
		if err != nil {
			return w, err
		}
		if f < 0 || f > 1 {
			return w, errf("22023", "weight out of range")
		}
		w[i] = float32(f)
	}
	return w, nil
}

// sortedOperands is the distinct operands in the order tsrank.c visits them (SortAndUniqItems).
func sortedOperands(q *tsq) []*tsq {
	var ops []*tsq
	queryOperands(q, &ops)
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].w != ops[j].w {
			return ops[i].w < ops[j].w
		}
		return !ops[i].prefix && ops[j].prefix
	})
	return ops
}

func queryOperands(n *tsq, out *[]*tsq) {
	if n == nil {
		return
	}
	if n.op == 'v' {
		for _, o := range *out {
			if o.w == n.w && o.prefix == n.prefix {
				return
			}
		}
		*out = append(*out, n)
		return
	}
	queryOperands(n.l, out)
	queryOperands(n.r, out)
}

func wordDistance(w int) float64 {
	if w > 100 {
		return 1e-30
	}
	return float64(float32(1.0 / (1.005 + 0.05*math.Exp(float64(float32(w))/1.5-2))))
}

type rankEntry struct {
	pos []tsPos // nil: no positions (POSNULL)
}

func entriesFor(v []tsLex, o *tsq) []rankEntry {
	var out []rankEntry
	for _, l := range lexMatches(v, o) {
		out = append(out, rankEntry{pos: l.pos})
	}
	return out
}

func posWeight(w [4]float32, p tsPos) float32 { return w[p.wt] }

func rankOr(v []tsLex, ops []*tsq, w [4]float32) float32 {
	var res float32
	size := len(ops)
	for _, o := range ops {
		for _, e := range entriesFor(v, o) {
			ps := e.pos
			if ps == nil {
				ps = []tsPos{{p: 0, wt: 0}}
			}
			var resj float32
			wjm := float32(-1.0)
			jm := 0
			for j, p := range ps {
				resj = resj + posWeight(w, p)/float32((j+1)*(j+1))
				if posWeight(w, p) > wjm {
					wjm = posWeight(w, p)
					jm = j
				}
			}
			tmp := wjm + resj - wjm/float32((jm+1)*(jm+1))
			res = float32(float64(res) + float64(tmp)/1.64493406685)
		}
	}
	if size > 0 {
		res = res / float32(size)
	}
	return res
}

func rankAnd(v []tsLex, ops []*tsq, w [4]float32) float32 {
	if len(ops) < 2 {
		return rankOr(v, ops, w)
	}
	res := float32(-1.0)
	prev := make([][]tsPos, len(ops))
	prevNull := make([]bool, len(ops))
	for i, o := range ops {
		for _, e := range entriesFor(v, o) {
			ps := e.pos
			null := false
			if ps == nil {
				ps = []tsPos{{p: 0, wt: 0}}
				null = true
			}
			for k := 0; k < i; k++ {
				if prev[k] == nil {
					continue
				}
				for _, a := range ps {
					for _, c := range prev[k] {
						dist := a.p - c.p
						if dist < 0 {
							dist = -dist
						}
						if dist != 0 || null || prevNull[k] {
							if dist == 0 {
								dist = 16383
							}
							curw := float32(math.Sqrt(float64(posWeight(w, a) * posWeight(w, c) * float32(wordDistance(dist)))))
							if res < 0 {
								res = curw
							} else {
								res = float32(1.0 - (1.0-float64(res))*(1.0-float64(curw)))
							}
						}
					}
				}
			}
			prev[i], prevNull[i] = ps, null
		}
	}
	return res
}

func vectorLength(v []tsLex) int {
	n := 0
	for _, l := range v {
		if len(l.pos) == 0 {
			n++
		} else {
			n += len(l.pos)
		}
	}
	return n
}

func tsRank(v []tsLex, q *tsq, w [4]float32, method int) float32 {
	if len(v) == 0 || q == nil {
		return 0
	}
	ops := sortedOperands(q)
	var res float32
	if q.op == '&' || q.op == '<' {
		res = rankAnd(v, ops, w)
	} else {
		res = rankOr(v, ops, w)
	}
	if res < 0 {
		res = 1e-20
	}
	if method&1 != 0 && len(v) > 0 {
		res = float32(float64(res) / (math.Log(float64(vectorLength(v)+1)) / math.Log(2.0)))
	}
	if method&2 != 0 {
		if l := vectorLength(v); l > 0 {
			res = res / float32(l)
		}
	}
	if method&8 != 0 && len(v) > 0 {
		res = res / float32(len(v))
	}
	if method&16 != 0 && len(v) > 0 {
		res = float32(float64(res) / (math.Log(float64(len(v)+1)) / math.Log(2.0)))
	}
	if method&32 != 0 {
		res = res / (res + 1)
	}
	return res
}

// tsRankCD is the cover density rank: the minimal windows of positions that satisfy the query.
func tsRankCD(v []tsLex, q *tsq, w [4]float32, method int) float32 {
	if len(v) == 0 || q == nil {
		return 0
	}
	type item struct {
		pos int
		wt  byte
		op  int
	}
	var ops []*tsq
	queryOperands(q, &ops)
	var items []item
	for oi, o := range ops {
		for _, l := range lexMatches(v, o) {
			for _, p := range l.pos {
				items = append(items, item{p.p, p.wt, oi})
			}
		}
	}
	if len(items) == 0 {
		return 0
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].pos < items[j].pos })
	satisfied := func(lo, hi int) bool {
		sub := make([]tsLex, 0, 4)
		have := map[int][]tsPos{}
		for _, it := range items[lo : hi+1] {
			have[it.op] = append(have[it.op], tsPos{it.pos, it.wt})
		}
		for oi, ps := range have {
			sub = append(sub, tsLex{w: ops[oi].w, pos: ps})
		}
		sort.Slice(sub, func(a, b int) bool { return sub[a].w < sub[b].w })
		ok, _ := q.execExact(sub)
		return ok
	}
	invws := [4]float64{}
	for i := range invws {
		invws[i] = 1.0 / float64(w[i])
	}
	var wdoc, sumDist, prevExt float64
	nExt := 0
	start := 0
	for start < len(items) {
		// shortest window beginning at or after start that satisfies the query
		found := false
		var lo, hi int
		for l := start; l < len(items) && !found; l++ {
			for h := l; h < len(items); h++ {
				if satisfied(l, h) {
					lo, hi, found = l, h, true
					break
				}
			}
		}
		if !found {
			break
		}
		// shrink the left edge
		for lo < hi && satisfied(lo+1, hi) {
			lo++
		}
		var inv float64
		for _, it := range items[lo : hi+1] {
			inv += invws[it.wt]
		}
		cpos := float64(hi-lo+1) / inv
		p, qq := items[lo].pos, items[hi].pos
		noise := (qq - p) - (hi - lo)
		if noise < 0 {
			noise = (hi - lo) / 2
		}
		wdoc += cpos / float64(1+noise)
		cur := float64(qq+p) / 2.0
		if nExt > 0 && cur > prevExt {
			sumDist += 1.0 / (cur - prevExt)
		}
		prevExt = cur
		nExt++
		start = lo + 1
	}
	if method&1 != 0 && len(v) > 0 {
		wdoc /= math.Log(float64(vectorLength(v) + 1))
	}
	if method&2 != 0 {
		if l := vectorLength(v); l > 0 {
			wdoc /= float64(l)
		}
	}
	if method&4 != 0 && nExt > 0 && sumDist > 0 {
		wdoc /= float64(nExt) / sumDist
	}
	if method&8 != 0 && len(v) > 0 {
		wdoc /= float64(len(v))
	}
	if method&16 != 0 && len(v) > 0 {
		wdoc /= math.Log(float64(len(v)+1)) / math.Log(2.0)
	}
	if method&32 != 0 {
		wdoc /= wdoc + 1
	}
	return float32(wdoc)
}

// execExact evaluates the query where every operand is exact (the window's lexemes are the matches).
func (n *tsq) execExact(v []tsLex) (bool, []int) { return n.exec(v) }

// ---- ts_headline ----

type hlOpts struct {
	start, stop        string
	minWords, maxWords int
	shortWord          int
	highlightAll       bool
	maxFragments       int
	fragDelim          string
}

func parseHLOpts(s string) (hlOpts, error) {
	o := hlOpts{start: "<b>", stop: "</b>", minWords: 15, maxWords: 35, shortWord: 3, fragDelim: " ... "}
	if s == "" {
		return o, nil
	}
	for _, part := range splitHLOpts(s) {
		kv := strings.SplitN(part, "=", 2)
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		if k == "" {
			continue
		}
		val := ""
		if len(kv) == 2 {
			val = strings.TrimSpace(kv[1])
			val = strings.Trim(val, `"`)
		}
		num := func() (int, error) {
			n, err := strconv.Atoi(val)
			if err != nil {
				return 0, errf("22023", "invalid value for \"%s\" option: \"%s\"", k, val)
			}
			return n, nil
		}
		var err error
		switch k {
		case "startsel":
			o.start = val
		case "stopsel":
			o.stop = val
		case "fragmentdelimiter":
			o.fragDelim = val
		case "maxwords":
			o.maxWords, err = num()
		case "minwords":
			o.minWords, err = num()
		case "shortword":
			o.shortWord, err = num()
		case "maxfragments":
			o.maxFragments, err = num()
		case "highlightall":
			o.highlightAll = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "t")
		default:
			return o, errf("22023", "unrecognized headline parameter: \"%s\"", k)
		}
		if err != nil {
			return o, err
		}
	}
	if !o.highlightAll {
		switch {
		case o.minWords >= o.maxWords:
			return o, errf("22023", "MinWords should be less than MaxWords")
		case o.minWords <= 0:
			return o, errf("22023", "MinWords should be positive")
		case o.maxWords <= 0:
			return o, errf("22023", "MaxWords should be positive")
		}
	}
	return o, nil
}

func splitHLOpts(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQ = !inQ
			cur.WriteByte(c)
		case c == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

type hlWord struct {
	text string // the token text
	sep  string // text between the previous token and this one
	word bool   // counts as a word
	hit  bool
}

// tsHeadline marks the query terms of text; the displayed excerpt is the shortest cover of the
// query terms extended to MinWords..MaxWords (the first MinWords words when nothing matches).
func tsHeadline(cf tsConfig, text string, q *tsq, o hlOpts) string {
	var ops []*tsq
	queryOperands(q, &ops)
	positive := map[string]bool{}
	var prefixes []string
	var collect func(n *tsq, neg bool)
	collect = func(n *tsq, neg bool) {
		if n == nil {
			return
		}
		switch n.op {
		case 'v':
			if !neg {
				if n.prefix {
					prefixes = append(prefixes, n.w)
				} else {
					positive[n.w] = true
				}
			}
		case '!':
			collect(n.l, !neg)
		default:
			collect(n.l, neg)
			collect(n.r, neg)
		}
	}
	collect(q, false)
	// split text into tokens keeping the separators
	rs := []rune(text)
	var words []hlWord
	i := 0
	for i < len(rs) {
		j := i
		for j < len(rs) && !isAlnumRune(rs[j]) {
			j++
		}
		sep := string(rs[i:j])
		if j >= len(rs) {
			words = append(words, hlWord{sep: sep})
			break
		}
		k := j
		for k < len(rs) && (isAlnumRune(rs[k]) || (rs[k] == '-' || rs[k] == '.' || rs[k] == '@') && k+1 < len(rs) && isAlnumRune(rs[k+1])) {
			k++
		}
		tok := string(rs[j:k])
		lex, keep := cf.normalize(tsTok{text: tok, class: "word"})
		hit := false
		if keep {
			if positive[lex] {
				hit = true
			}
			for _, pf := range prefixes {
				if strings.HasPrefix(lex, pf) {
					hit = true
				}
			}
		}
		words = append(words, hlWord{text: tok, sep: sep, word: true, hit: hit})
		i = k
	}
	hi := len(words) - 1
	if hi >= 0 && !words[hi].word {
		hi--
	}
	if !o.highlightAll && hi >= 0 {
		// cover: smallest span containing all positive terms; otherwise the first hit; otherwise the start
		need := map[string]bool{}
		for k := range positive {
			need[k] = true
		}
		bestLo, bestHi := -1, -1
		for l := 0; l <= hi; l++ {
			if !words[l].word {
				continue
			}
			seen := map[string]bool{}
			for h := l; h <= hi; h++ {
				if !words[h].word || !words[h].hit {
					continue
				}
				lex, _ := cf.normalize(tsTok{text: words[h].text, class: "word"})
				seen[lex] = true
				if len(seen) >= len(need) && len(need) > 0 || len(need) == 0 {
					if bestLo < 0 || h-l < bestHi-bestLo {
						bestLo, bestHi = l, h
					}
					break
				}
			}
		}
		if bestLo < 0 {
			for l := 0; l <= hi; l++ {
				if words[l].hit {
					bestLo, bestHi = l, l
					break
				}
			}
		}
		if bestLo >= 0 {
			lo, hi2 := bestLo, bestHi
			count := 0
			for k := lo; k <= hi2; k++ {
				if words[k].word {
					count++
				}
			}
			// extend to the right then left up to MaxWords, at least MinWords
			for count < o.minWords && (hi2 < hi || lo > 0) {
				if hi2 < hi && count < o.maxWords {
					hi2++
					if words[hi2].word {
						count++
					}
				} else if lo > 0 && count < o.maxWords {
					lo--
					if words[lo].word {
						count++
					}
				} else {
					break
				}
			}
			for count < o.maxWords && hi2 < hi && count < o.minWords {
				hi2++
				count++
			}
			hi = hi2
			words = words[lo:]
			hi -= lo
		} else {
			// nothing matches: the first MinWords words
			count := 0
			end := -1
			for k := 0; k <= hi; k++ {
				if words[k].word {
					count++
					end = k
					if count >= o.minWords {
						break
					}
				}
			}
			hi = end
		}
	}
	var b strings.Builder
	for k := 0; k <= hi && k < len(words); k++ {
		w := words[k]
		if k > 0 {
			b.WriteString(w.sep)
		}
		if !w.word {
			continue
		}
		if w.hit {
			b.WriteString(o.start + w.text + o.stop)
		} else {
			b.WriteString(w.text)
		}
	}
	return b.String()
}

// ---- registration ----

func init() {
	otherOps["@@"] = "stask_atat"

	cfgAndText := func(c *evalCtx, a []Datum) (tsConfig, string, error) {
		if len(a) == 1 {
			return c.tsDefaultConfig(), a[0].String(), nil
		}
		cf, err := parseTSConfig(a[0].String())
		return cf, a[1].String(), err
	}
	regFn("to_tsvector", &fnSig{min: 1, max: 2, kind: KText, ty: tyOf(tyTSVector), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		cf, text, err := cfgAndText(c, a)
		if err != nil {
			return Null, err
		}
		return tsvD(vectorString(cf.toVector(text))), nil
	}})
	regFn("to_tsquery", &fnSig{min: 1, max: 2, kind: KText, ty: tyOf(tyTSQuery), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		cf, text, err := cfgAndText(c, a)
		if err != nil {
			return Null, err
		}
		q, err := parseToTSQuery(cf, text)
		if err != nil {
			return Null, err
		}
		return tsqD(qString(q)), nil
	}})
	regFn("plainto_tsquery", &fnSig{min: 1, max: 2, kind: KText, ty: tyOf(tyTSQuery), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		cf, text, err := cfgAndText(c, a)
		if err != nil {
			return Null, err
		}
		return tsqD(qString(plainQuery(cf, text, true))), nil
	}})
	regFn("phraseto_tsquery", &fnSig{min: 1, max: 2, kind: KText, ty: tyOf(tyTSQuery), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		cf, text, err := cfgAndText(c, a)
		if err != nil {
			return Null, err
		}
		return tsqD(qString(plainQuery(cf, text, false))), nil
	}})
	regFn("websearch_to_tsquery", &fnSig{min: 1, max: 2, kind: KText, ty: tyOf(tyTSQuery), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		cf, text, err := cfgAndText(c, a)
		if err != nil {
			return Null, err
		}
		return tsqD(qString(webQuery(cf, text))), nil
	}})

	regFn("stask_atat", &fnSig{min: 2, max: 2, kind: KBool, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if a[0].K == KJSON || a[0].K == KJSONB {
			d, ok, err := c.callJSONArr(&Func{Name: "stask_jpath_match"}, a)
			if !ok && err == nil {
				return Null, errf(codeUndefinedFunction, "operator does not exist: jsonb @@")
			}
			return d, err
		}
		vec, qry := a[0], a[1]
		if a[0].F == tyTSQuery || a[1].F == tyTSVector {
			vec, qry = a[1], a[0]
		}
		var v []tsLex
		var q *tsq
		var err error
		if vec.F == tyTSVector {
			v, err = parseVector(vec.S)
		} else {
			v = c.tsDefaultConfig().toVector(vec.String())
		}
		if err != nil {
			return Null, err
		}
		if qry.F == tyTSQuery {
			q, err = queryOf(qry)
		} else {
			q = plainQuery(c.tsDefaultConfig(), qry.String(), true)
		}
		if err != nil {
			return Null, err
		}
		return BoolD(tsMatch(v, q)), nil
	}})

	regFn("setweight", &fnSig{min: 2, max: 3, kind: KText, ty: tyOf(tyTSVector), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		v, err := parseVector(a[0].S)
		if err != nil {
			return Null, err
		}
		w := strings.ToUpper(a[1].String())
		if len(w) != 1 || strings.IndexByte("ABCD", w[0]) < 0 {
			return Null, errf("22023", "unrecognized weight: \"%s\"", a[1].String())
		}
		wt := byte(strings.IndexByte("DCBA", w[0]))
		var only map[string]bool
		if len(a) == 3 {
			arr, err := arrOf(a[2], KText)
			if err != nil {
				return Null, err
			}
			only = map[string]bool{}
			for _, e := range arr.el {
				if !e.IsNull() {
					only[e.String()] = true
				}
			}
		}
		for i := range v {
			if only != nil && !only[v[i].w] {
				continue
			}
			for j := range v[i].pos {
				v[i].pos[j].wt = wt
			}
		}
		return tsvD(vectorString(v)), nil
	}})
	regFn("strip", &fnSig{min: 1, max: 1, kind: KText, ty: tyOf(tyTSVector), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		v, err := parseVector(a[0].S)
		if err != nil {
			return Null, err
		}
		for i := range v {
			v[i].pos = nil
		}
		return tsvD(vectorString(v)), nil
	}})
	regFn("length", &fnSig{min: 1, max: 1, kindFn: func([]Kind) Kind { return kUnhandled }, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if a[0].F != tyTSVector {
			return Null, errFnFall
		}
		v, err := parseVector(a[0].S)
		return IntD(int64(len(v))), err
	}})
	regFn("numnode", &fnSig{min: 1, max: 1, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		q, err := queryOf(a[0])
		if err != nil {
			return Null, err
		}
		var cnt func(n *tsq) int
		cnt = func(n *tsq) int {
			if n == nil {
				return 0
			}
			return 1 + cnt(n.l) + cnt(n.r)
		}
		return IntD(int64(cnt(q))), nil
	}})
	regFn("tsvector_to_array", &fnSig{min: 1, max: 1, kind: KArray, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		v, err := parseVector(a[0].S)
		if err != nil {
			return Null, err
		}
		el := make([]Datum, len(v))
		for i, l := range v {
			el[i] = TextD(l.w)
		}
		d := arrayD(el)
		d.I = int64(KText)
		return d, nil
	}})
	regFn("array_to_tsvector", &fnSig{min: 1, max: 1, kind: KText, ty: tyOf(tyTSVector), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		arr, err := arrOf(a[0], KText)
		if err != nil {
			return Null, err
		}
		m := map[string]*tsLex{}
		for _, e := range arr.el {
			if e.IsNull() {
				return Null, errf("22004", "lexeme array may not contain nulls")
			}
			if e.String() == "" {
				return Null, errf("22023", "lexeme array may not contain empty strings")
			}
			m[e.String()] = &tsLex{w: e.String()}
		}
		return tsvD(vectorString(sortedVector(m))), nil
	}})

	rank := func(cd bool) func(c *evalCtx, a []Datum) (Datum, error) {
		return func(c *evalCtx, a []Datum) (Datum, error) {
			var wa []Datum
			if a[0].K == KArray {
				wa, a = a[:1], a[1:]
			}
			if len(a) < 2 || len(a) > 3 {
				return Null, errf(codeUndefinedFunction, "function ts_rank with %d arguments does not exist", len(a))
			}
			w, err := rankWeights(wa)
			if err != nil {
				return Null, err
			}
			v, err := parseVector(a[0].S)
			if err != nil {
				return Null, err
			}
			q, err := queryOf(a[1])
			if err != nil {
				return Null, err
			}
			method := 0
			if len(a) == 3 {
				m, err := fnInt(a[2])
				if err != nil {
					return Null, err
				}
				method = int(m)
			}
			if cd {
				return real4(float64(tsRankCD(v, q, w, method))), nil
			}
			r := tsRank(v, q, w, method)
			d := FloatD(float64(r))
			d.S = f4Mark
			return d, nil
		}
	}
	regFn("ts_rank", &fnSig{min: 2, max: 4, kind: KFloat, ty: tyOf(tyFloat4), impl: rank(false)})
	regFn("ts_rank_cd", &fnSig{min: 2, max: 4, kind: KFloat, ty: tyOf(tyFloat4), impl: rank(true)})

	regFn("ts_headline", &fnSig{min: 2, max: 4, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		cf := c.tsDefaultConfig()
		// forms: (text, query [, opts]) and (config, text, query [, opts])
		if len(a) >= 3 && a[2].F == tyTSQuery || len(a) == 4 {
			var err error
			if cf, err = parseTSConfig(a[0].String()); err != nil {
				return Null, err
			}
			a = a[1:]
		}
		opts := ""
		if len(a) == 3 {
			opts = a[2].String()
		}
		o, err := parseHLOpts(opts)
		if err != nil {
			return Null, err
		}
		q, err := queryOf(a[1])
		if err != nil {
			return Null, err
		}
		if q == nil {
			return TextD(a[0].String()), nil
		}
		return TextD(tsHeadline(cf, a[0].String(), q, o)), nil
	}})
	regFn("ts_lexize", &fnSig{min: 2, max: 2, kind: KArray, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		name := strings.TrimPrefix(strings.ToLower(a[0].String()), "pg_catalog.")
		var lex string
		keep := true
		switch name {
		case "english_stem":
			lex, keep = strings.ToLower(a[1].String()), true
			if englishStop[lex] {
				keep = false
			} else {
				lex = englishStem(lex)
			}
		case "simple":
			lex = strings.ToLower(a[1].String())
		default:
			return Null, errf(codeUndefinedObject, "text search dictionary \"%s\" does not exist", a[0].String())
		}
		if !keep {
			return Datum{K: KArray, S: "{}", I: int64(KText)}, nil
		}
		d := arrayD([]Datum{TextD(lex)})
		d.I = int64(KText)
		return d, nil
	}})
	settingDefaults["default_text_search_config"] = "pg_catalog.english"
}

// ftsBinary is || on two tsvectors (positions of the right one follow the left one).
func ftsBinary(op string, l, r Datum) (Datum, bool, error) {
	if op != "||" || l.F != tyTSVector || r.F != tyTSVector {
		return Null, false, nil
	}
	lv, err := parseVector(l.S)
	if err != nil {
		return Null, true, err
	}
	rv, err := parseVector(r.S)
	if err != nil {
		return Null, true, err
	}
	off := 0
	for _, x := range lv {
		for _, p := range x.pos {
			off = max(off, p.p)
		}
	}
	m := map[string]*tsLex{}
	for _, x := range lv {
		x := x
		m[x.w] = &x
	}
	for _, x := range rv {
		cur := m[x.w]
		if cur == nil {
			cur = &tsLex{w: x.w}
			m[x.w] = cur
		}
		for _, p := range x.pos {
			cur.pos = append(cur.pos, tsPos{min(p.p+off, 16383), p.wt})
		}
	}
	for _, x := range m {
		sort.SliceStable(x.pos, func(i, j int) bool { return x.pos[i].p < x.pos[j].p })
	}
	return tsvD(vectorString(sortedVector(m))), true, nil
}
