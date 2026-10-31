package sql

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Regular-expression functions (PostgreSQL 16 section 9.7.3): regexp_replace with start/N/flags,
// regexp_match(es), regexp_split_to_array/table, regexp_count, regexp_instr, regexp_substr,
// regexp_like, string_to_table.
//
// PostgreSQL Advanced Regular Expressions are translated into Go (RE2) syntax: the default is
// "dot matches newline, ^ and $ only at the ends of the string" (flag n/p/w change that), \y \Y \m \M
// become word boundaries, \w is Unicode aware, leading embedded options (?i) are read here. Back
// references and lookaround (not expressible in RE2) are reported with SQLSTATE 2201B for now.
// Precedent: PostgreSQL docs 9.7.3 and 9.7.3.5 (regex flags), src/backend/utils/adt/regexp.c.

type pgRegex struct {
	re *regexp.Regexp
}

var pgRegexCache sync.Map

func pgRegexCompile(pat, flags string) (*regexp.Regexp, error) {
	key := flags + "\x00" + pat
	if v, ok := pgRegexCache.Load(key); ok {
		return v.(*regexp.Regexp), nil
	}
	icase, quote, expanded := false, false, false
	nl := "s" // s: default, n: newline-sensitive, p: partial, w: inverse partial
	apply := func(fl string, embedded bool) error {
		for _, c := range fl {
			switch c {
			case 'i':
				icase = true
			case 'c':
				icase = false
			case 'q':
				quote = true
			case 'x':
				expanded = true
			case 't':
				expanded = false
			case 'n', 's', 'p', 'w':
				nl = string(c)
			case 'g', 'b', 'e':
				if embedded && c == 'g' {
					return errf(codeInvalidRegex, "invalid regular expression: quantifier operand invalid")
				}
			default:
				return errf(codeInvalidParameter, "invalid regular expression option: \"%c\"", c)
			}
		}
		return nil
	}
	if err := apply(flags, false); err != nil {
		return nil, err
	}
	body := pat
	if strings.HasPrefix(body, "***=") {
		quote, body = true, body[4:]
	} else if strings.HasPrefix(body, "***:") {
		body = body[4:]
	}
	if !quote && strings.HasPrefix(body, "(?") {
		if j := strings.IndexByte(body, ')'); j > 2 {
			opts := body[2:j]
			if isAlphaOpts(opts) {
				if err := apply(opts, true); err != nil {
					return nil, err
				}
				body = body[j+1:]
			}
		}
	}
	var goPat string
	if quote {
		goPat = regexp.QuoteMeta(body)
	} else {
		var err error
		if goPat, err = translateARE(body, expanded); err != nil {
			return nil, err
		}
	}
	prefix := ""
	if icase {
		prefix += "i"
	}
	switch nl {
	case "s":
		prefix += "s"
	case "n":
		prefix += "m"
	case "w":
		prefix += "ms"
	}
	if prefix != "" {
		goPat = "(?" + prefix + ")" + goPat
	}
	re, err := regexp.Compile(goPat)
	if err != nil {
		msg := err.Error()
		msg = strings.TrimPrefix(msg, "error parsing regexp: ")
		return nil, errf(codeInvalidRegex, "invalid regular expression: %s", msg)
	}
	pgRegexCache.Store(key, re)
	return re, nil
}

func isAlphaOpts(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("bceimnpqstwx", c) {
			return false
		}
	}
	return true
}

func translateARE(p string, expanded bool) (string, error) {
	var b strings.Builder
	inSet := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if inSet {
			switch c {
			case ']':
				inSet = false
				b.WriteByte(c)
			case '[':
				if i+1 < len(p) && (p[i+1] == ':' || p[i+1] == '.' || p[i+1] == '=') {
					end := strings.Index(p[i+2:], string(p[i+1])+"]")
					if end >= 0 {
						b.WriteString(p[i : i+2+end+2])
						i += 2 + end + 1
						continue
					}
				}
				b.WriteString(`\[`)
			case '\\':
				if i+1 >= len(p) {
					return "", errf(codeInvalidRegex, "invalid regular expression: invalid escape \\ sequence")
				}
				i++
				n, err := translateEscape(p, &i, true)
				if err != nil {
					return "", err
				}
				b.WriteString(n)
			default:
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\\':
			if i+1 >= len(p) {
				return "", errf(codeInvalidRegex, "invalid regular expression: invalid escape \\ sequence")
			}
			i++
			n, err := translateEscape(p, &i, false)
			if err != nil {
				return "", err
			}
			b.WriteString(n)
		case '[':
			inSet = true
			b.WriteByte('[')
			if i+1 < len(p) && p[i+1] == '^' {
				b.WriteByte('^')
				i++
			}
			if i+1 < len(p) && p[i+1] == ']' {
				b.WriteString(`\]`)
				i++
			}
		case '(':
			if strings.HasPrefix(p[i:], "(?") {
				switch {
				case strings.HasPrefix(p[i:], "(?:"):
					b.WriteString("(?:")
					i += 2
				case strings.HasPrefix(p[i:], "(?=") || strings.HasPrefix(p[i:], "(?!") ||
					strings.HasPrefix(p[i:], "(?<=") || strings.HasPrefix(p[i:], "(?<!"):
					return "", errf(codeInvalidRegex, "invalid regular expression: lookaround constraints are not supported")
				default:
					return "", errf(codeInvalidRegex, "invalid regular expression: quantifier operand invalid")
				}
			} else {
				b.WriteByte('(')
			}
		case ' ', '\t', '\n', '\r':
			if !expanded {
				b.WriteByte(c)
			}
		case '#':
			if expanded {
				for i < len(p) && p[i] != '\n' {
					i++
				}
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// translateEscape handles the escape whose letter is p[*i].
func translateEscape(p string, i *int, inSet bool) (string, error) {
	e := p[*i]
	switch e {
	case 'y', 'm', 'M':
		return `\b`, nil
	case 'Y':
		return `\B`, nil
	case 'A':
		return `\A`, nil
	case 'Z':
		return `\z`, nil
	case 'w':
		if inSet {
			return `\pL\pN_`, nil
		}
		return `[\pL\pN_]`, nil
	case 'W':
		if inSet {
			return `\W`, nil
		}
		return `[^\pL\pN_]`, nil
	case 'e':
		return `\x1b`, nil
	case 'a':
		return `\x07`, nil
	case 'x', 'u', 'U':
		max := map[byte]int{'x': 8, 'u': 4, 'U': 8}[e]
		j := *i + 1
		for j < len(p) && j-*i-1 < max && isHex(p[j]) {
			j++
		}
		if j == *i+1 {
			return "", errf(codeInvalidRegex, "invalid regular expression: invalid escape \\ sequence")
		}
		h := p[*i+1 : j]
		*i = j - 1
		return `\x{` + h + `}`, nil
	case '0':
		return `\x00`, nil
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if inSet {
			return `\` + string(e), nil
		}
		return "", errf(codeInvalidRegex, "invalid regular expression: back references are not supported")
	}
	if e < 0x80 && (e >= 'a' && e <= 'z' || e >= 'A' && e <= 'Z') && !strings.ContainsRune("dDsSntrfvbBpP", rune(e)) {
		return "", errf(codeInvalidRegex, "invalid regular expression: invalid escape \\ sequence")
	}
	if e == 'b' && !inSet {
		return `\x08`, nil // in an ARE, \b is a backspace
	}
	return `\` + string(e), nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// charPos converts a byte offset in s to a 1-based character position.
func charPos(s string, byteOff int) int { return utf8.RuneCountInString(s[:byteOff]) + 1 }

// byteOff converts a 1-based character position to a byte offset (len(s) when past the end).
func byteOff(s string, pos int) int {
	n := 0
	for i := range s {
		n++
		if n == pos {
			return i
		}
	}
	return len(s)
}

func reArgs(a []Datum, flagsAt int) (string, error) {
	if flagsAt < len(a) {
		return a[flagsAt].String(), nil
	}
	return "", nil
}

func init() {
	regFn("regexp_like", &fnSig{min: 2, max: 3, kind: KBool, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		fl, _ := reArgs(a, 2)
		re, err := pgRegexCompile(a[1].String(), fl)
		if err != nil {
			return Null, err
		}
		return BoolD(re.MatchString(a[0].String())), nil
	}})

	regFn("regexp_replace", &fnSig{min: 3, max: 6, kind: KText, impl: regexpReplace})

	regFn("regexp_match", &fnSig{min: 2, max: 3, kind: KArray, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		fl, _ := reArgs(a, 2)
		if strings.Contains(fl, "g") {
			return Null, errf(codeInvalidParameter, "regexp_match() does not support the \"global\" option")
		}
		re, err := pgRegexCompile(a[1].String(), strings.ReplaceAll(fl, "g", ""))
		if err != nil {
			return Null, err
		}
		s := a[0].String()
		m := re.FindStringSubmatchIndex(s)
		if m == nil {
			return Null, nil
		}
		return matchArray(s, m, re.NumSubexp()), nil
	}})

	regFn("regexp_split_to_array", &fnSig{min: 2, max: 3, kind: KArray, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		parts, err := regexpSplit(a)
		if err != nil {
			return Null, err
		}
		el := make([]Datum, len(parts))
		for i, p := range parts {
			el[i] = TextD(p)
		}
		return arrayD(el), nil
	}})

	regFn("regexp_count", &fnSig{min: 2, max: 4, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		s := a[0].String()
		start, err := startArg(a, 2)
		if err != nil {
			return Null, err
		}
		fl, _ := reArgs(a, 3)
		re, err := pgRegexCompile(a[1].String(), fl)
		if err != nil {
			return Null, err
		}
		off := byteOff(s, start)
		if start > utf8.RuneCountInString(s)+1 {
			return IntD(0), nil
		}
		return IntD(int64(len(re.FindAllStringIndex(s[off:], -1)))), nil
	}})

	regFn("regexp_instr", &fnSig{min: 2, max: 7, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		s := a[0].String()
		start, err := startArg(a, 2)
		if err != nil {
			return Null, err
		}
		n := int64(1)
		if len(a) > 3 {
			if n, err = fnInt(a[3]); err != nil {
				return Null, err
			}
			if n < 1 {
				return Null, errf(codeInvalidParameter, "invalid value for parameter \"n\": %d", n)
			}
		}
		endopt := int64(0)
		if len(a) > 4 {
			if endopt, err = fnInt(a[4]); err != nil {
				return Null, err
			}
			if endopt != 0 && endopt != 1 {
				return Null, errf(codeInvalidParameter, "invalid value for parameter \"endoption\": %d", endopt)
			}
		}
		fl, _ := reArgs(a, 5)
		sub := int64(0)
		if len(a) > 6 {
			if sub, err = fnInt(a[6]); err != nil {
				return Null, err
			}
		}
		re, err := pgRegexCompile(a[1].String(), fl)
		if err != nil {
			return Null, err
		}
		if sub < 0 || int(sub) > re.NumSubexp() {
			return Null, errf(codeInvalidParameter, "invalid value for parameter \"subexpr\": %d", sub)
		}
		m := nthMatch(re, s, start, int(n))
		if m == nil || m[2*sub] < 0 {
			return IntD(0), nil
		}
		if endopt == 1 {
			return IntD(int64(charPos(s, m[2*sub+1]))), nil
		}
		return IntD(int64(charPos(s, m[2*sub]))), nil
	}})

	regFn("regexp_substr", &fnSig{min: 2, max: 6, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		s := a[0].String()
		start, err := startArg(a, 2)
		if err != nil {
			return Null, err
		}
		n := int64(1)
		if len(a) > 3 {
			if n, err = fnInt(a[3]); err != nil {
				return Null, err
			}
			if n < 1 {
				return Null, errf(codeInvalidParameter, "invalid value for parameter \"n\": %d", n)
			}
		}
		fl, _ := reArgs(a, 4)
		sub := int64(0)
		if len(a) > 5 {
			if sub, err = fnInt(a[5]); err != nil {
				return Null, err
			}
		}
		re, err := pgRegexCompile(a[1].String(), fl)
		if err != nil {
			return Null, err
		}
		if sub < 0 || int(sub) > re.NumSubexp() {
			return Null, errf(codeInvalidParameter, "invalid value for parameter \"subexpr\": %d", sub)
		}
		m := nthMatch(re, s, start, int(n))
		if m == nil || m[2*sub] < 0 {
			return Null, nil
		}
		return TextD(s[m[2*sub]:m[2*sub+1]]), nil
	}})

	srfDefs["regexp_matches"] = srfDef{[]string{"regexp_matches"}, []Kind{KArray}}
	srfDefs["regexp_split_to_table"] = srfDef{[]string{"regexp_split_to_table"}, []Kind{KText}}
	srfDefs["string_to_table"] = srfDef{[]string{"string_to_table"}, []Kind{KText}}
}

func startArg(a []Datum, at int) (int, error) {
	if at >= len(a) || a[at].K == KText && at == len(a)-1 && at != 2 {
		return 1, nil
	}
	v, err := fnInt(a[at])
	if err != nil {
		return 0, err
	}
	if v < 1 {
		return 0, errf(codeInvalidParameter, "invalid value for parameter \"start\": %d", v)
	}
	return int(v), nil
}

// nthMatch returns the submatch indexes (relative to s) of the n-th match at or after character start.
func nthMatch(re *regexp.Regexp, s string, start, n int) []int {
	off := byteOff(s, start)
	if start > utf8.RuneCountInString(s)+1 {
		return nil
	}
	all := re.FindAllStringSubmatchIndex(s[off:], n)
	if len(all) < n {
		return nil
	}
	m := append([]int(nil), all[n-1]...)
	for i := range m {
		if m[i] >= 0 {
			m[i] += off
		}
	}
	return m
}

func matchArray(s string, m []int, groups int) Datum {
	var el []Datum
	if groups == 0 {
		el = []Datum{TextD(s[m[0]:m[1]])}
	} else {
		for g := 1; g <= groups; g++ {
			if m[2*g] < 0 {
				el = append(el, Null)
			} else {
				el = append(el, TextD(s[m[2*g]:m[2*g+1]]))
			}
		}
	}
	d := arrayD(el)
	d.I = int64(KText)
	return d
}

func regexpReplace(c *evalCtx, a []Datum) (Datum, error) {
	s, pat, repl := a[0].String(), a[1].String(), a[2].String()
	flags := ""
	start, n := 1, 0
	switch {
	case len(a) == 4 && a[3].K == KText:
		flags = a[3].String()
	case len(a) >= 4:
		v, err := fnInt(a[3])
		if err != nil {
			return Null, err
		}
		if v < 1 {
			return Null, errf(codeInvalidParameter, "invalid value for parameter \"start\": %d", v)
		}
		start = int(v)
		if len(a) >= 5 {
			nv, err := fnInt(a[4])
			if err != nil {
				return Null, err
			}
			if nv < 0 {
				return Null, errf(codeInvalidParameter, "invalid value for parameter \"n\": %d", nv)
			}
			n = int(nv)
			if len(a) == 6 {
				flags = a[5].String()
			}
		}
	}
	global := strings.Contains(flags, "g")
	if len(a) >= 5 && !global {
		// with start and N given, N selects the match (0 = first unless g)
	}
	re, err := pgRegexCompile(pat, strings.ReplaceAll(flags, "g", ""))
	if err != nil {
		return Null, err
	}
	off := byteOff(s, start)
	if start > utf8.RuneCountInString(s)+1 {
		return TextD(s), nil
	}
	head, tail := s[:off], s[off:]
	var out strings.Builder
	out.WriteString(head)
	matches := re.FindAllStringSubmatchIndex(tail, -1)
	pos := 0
	for i, m := range matches {
		idx := i + 1
		replaceThis := false
		switch {
		case n > 0:
			replaceThis = idx == n
		case global:
			replaceThis = true
		default:
			replaceThis = idx == 1
		}
		if !replaceThis {
			continue
		}
		out.WriteString(tail[pos:m[0]])
		out.WriteString(expandRepl(repl, tail, m))
		pos = m[1]
		if n > 0 || !global {
			break
		}
	}
	out.WriteString(tail[pos:])
	return TextD(out.String()), nil
}

// expandRepl is PostgreSQL's replacement-string expansion: \1..\9, \& and \\.
func expandRepl(repl, s string, m []int) string {
	if !strings.Contains(repl, `\`) {
		return repl
	}
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c != '\\' || i+1 >= len(repl) {
			b.WriteByte(c)
			continue
		}
		n := repl[i+1]
		switch {
		case n == '\\':
			b.WriteByte('\\')
			i++
		case n == '&':
			b.WriteString(s[m[0]:m[1]])
			i++
		case n >= '1' && n <= '9':
			g := int(n - '0')
			if 2*g+1 < len(m) && m[2*g] >= 0 {
				b.WriteString(s[m[2*g]:m[2*g+1]])
			}
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// regexpSplit is the field list of regexp_split_to_array/table (args: string, pattern [, flags]).
func regexpSplit(a []Datum) ([]string, error) {
	s := a[0].String()
	fl, _ := reArgs(a, 2)
	if strings.Contains(fl, "g") {
		return nil, errf(codeInvalidParameter, "regexp_split_to_table() does not support the \"global\" option")
	}
	re, err := pgRegexCompile(a[1].String(), fl)
	if err != nil {
		return nil, err
	}
	var out []string
	last, pos := 0, 0
	for pos < len(s) {
		loc := re.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		ms, me := pos+loc[0], pos+loc[1]
		if me == ms {
			// an empty match: never split at the start of a field; step one character
			if ms == last {
				_, w := utf8.DecodeRuneInString(s[ms:])
				pos = ms + w
				if pos >= len(s) {
					break
				}
				continue
			}
		}
		out = append(out, s[last:ms])
		last = me
		pos = me
		if me == ms {
			_, w := utf8.DecodeRuneInString(s[ms:])
			pos = ms + w
		}
	}
	out = append(out, s[last:])
	return out, nil
}

// fnSRFRows runs the set-returning functions of this task (jsonarr.go srfRows calls it first).
func fnSRFRows(name string, a []Datum) ([][]Datum, bool, error) {
	switch name {
	case "jsonb_path_query_tz":
		rows, err := jsonPathQuery(a)
		return rows, true, err
	case "pg_input_error_info":
		if len(a) != 2 {
			return nil, true, errf(codeUndefinedFunction, "function pg_input_error_info with %d arguments does not exist", len(a))
		}
		return pgInputErrorRows(a), true, nil
	case "regexp_matches":
		if len(a) < 2 || len(a) > 3 {
			return nil, true, errf(codeUndefinedFunction, "function regexp_matches with %d arguments does not exist", len(a))
		}
		if hasNull(a) {
			return nil, true, nil
		}
		s := a[0].String()
		fl, _ := reArgs(a, 2)
		re, err := pgRegexCompile(a[1].String(), strings.ReplaceAll(fl, "g", ""))
		if err != nil {
			return nil, true, err
		}
		var out [][]Datum
		if strings.Contains(fl, "g") {
			for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
				out = append(out, []Datum{matchArray(s, m, re.NumSubexp())})
			}
		} else if m := re.FindStringSubmatchIndex(s); m != nil {
			out = append(out, []Datum{matchArray(s, m, re.NumSubexp())})
		}
		return out, true, nil
	case "regexp_split_to_table":
		if len(a) < 2 || len(a) > 3 {
			return nil, true, errf(codeUndefinedFunction, "function regexp_split_to_table with %d arguments does not exist", len(a))
		}
		if hasNull(a) {
			return nil, true, nil
		}
		parts, err := regexpSplit(a)
		if err != nil {
			return nil, true, err
		}
		out := make([][]Datum, len(parts))
		for i, p := range parts {
			out[i] = []Datum{TextD(p)}
		}
		return out, true, nil
	case "string_to_table":
		if len(a) < 2 || len(a) > 3 {
			return nil, true, errf(codeUndefinedFunction, "function string_to_table with %d arguments does not exist", len(a))
		}
		if a[0].IsNull() {
			return nil, true, nil
		}
		var nullStr *string
		if len(a) == 3 && !a[2].IsNull() {
			x := a[2].String()
			nullStr = &x
		}
		var parts []string
		s := a[0].String()
		switch {
		case a[1].IsNull():
			parts = []string{s}
		case a[1].String() == "":
			parts = []string{s}
		default:
			if s == "" {
				return nil, true, nil
			}
			parts = strings.Split(s, a[1].String())
		}
		out := make([][]Datum, len(parts))
		for i, p := range parts {
			if nullStr != nil && p == *nullStr {
				out[i] = []Datum{Null}
			} else {
				out[i] = []Datum{TextD(p)}
			}
		}
		return out, true, nil
	}
	return nil, false, nil
}

var _ = strconv.Itoa
