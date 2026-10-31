package sql

import (
	"bytes"
	"math/bits"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/unicode/norm"
)

// String and bytea functions (PostgreSQL 16 sections 9.4 and 9.5) that were missing or wrong:
// character_length, to_hex, unistr, parse_ident, convert*, get/set_byte, get/set_bit, bit_count,
// bytea overlay/position/trim, decode(.., 'escape'), normalize, regex and SIMILAR forms of substring.

func isBytea(d Datum) bool { return d.K == KBytea }

func init() {
	regFn("character_length", &fnSig{min: 1, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		return IntD(int64(utf8.RuneCountInString(a[0].String()))), nil
	}})
	regFn("to_hex", &fnSig{min: 1, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n, err := fnInt(a[0])
		if err != nil {
			return Null, err
		}
		if n >= -1<<31 && n < 1<<31 {
			return TextD(strconv.FormatUint(uint64(uint32(int32(n))), 16)), nil
		}
		return TextD(strconv.FormatUint(uint64(n), 16)), nil
	}})
	regFn("unistr", &fnSig{min: 1, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		s := a[0].String()
		var b strings.Builder
		for i := 0; i < len(s); {
			if s[i] != '\\' {
				b.WriteByte(s[i])
				i++
				continue
			}
			bad := errf("22025", "invalid Unicode escape")
			if i+1 >= len(s) {
				return Null, bad
			}
			n := 0
			switch {
			case s[i+1] == '\\':
				b.WriteByte('\\')
				i += 2
				continue
			case s[i+1] == '+':
				n = 6
				i += 2
			case s[i+1] == 'u':
				n = 4
				i += 2
			case s[i+1] == 'U':
				n = 8
				i += 2
			default:
				n = 4
				i++
			}
			if i+n > len(s) {
				return Null, bad
			}
			v, err := strconv.ParseUint(s[i:i+n], 16, 32)
			if err != nil {
				return Null, bad
			}
			i += n
			b.WriteRune(rune(v))
		}
		return TextD(b.String()), nil
	}})
	regFn("parse_ident", &fnSig{min: 1, max: 2, kind: KArray, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		strict := len(a) < 2 || a[1].Bool()
		parts, err := parseIdent(a[0].String(), strict)
		if err != nil {
			return Null, err
		}
		el := make([]Datum, len(parts))
		for i, p := range parts {
			el[i] = TextD(p)
		}
		d := arrayD(el)
		d.I = int64(KText)
		return d, nil
	}})

	// ---- encodings ----
	regFn("convert_from", &fnSig{min: 2, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		s, err := decodeBytes(a[0].S, a[1].String())
		return TextD(s), err
	}})
	regFn("convert_to", &fnSig{min: 2, kind: KBytea, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		b, err := encodeText(a[0].String(), a[1].String())
		return Datum{K: KBytea, S: b}, err
	}})
	regFn("convert", &fnSig{min: 3, kind: KBytea, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		s, err := decodeBytes(a[0].S, a[1].String())
		if err != nil {
			return Null, err
		}
		b, err := encodeText(s, a[2].String())
		return Datum{K: KBytea, S: b}, err
	}})

	// ---- bytea byte and bit access ----
	regFn("get_byte", &fnSig{min: 2, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n, err := fnInt(a[1])
		if err != nil {
			return Null, err
		}
		if n < 0 || n >= int64(len(a[0].S)) {
			return Null, errf("2202E", "index %d out of valid range, 0..%d", n, len(a[0].S)-1)
		}
		return IntD(int64(a[0].S[n])), nil
	}})
	regFn("set_byte", &fnSig{min: 3, kind: KBytea, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n, err := fnInt(a[1])
		if err != nil {
			return Null, err
		}
		v, err := fnInt(a[2])
		if err != nil {
			return Null, err
		}
		if n < 0 || n >= int64(len(a[0].S)) {
			return Null, errf("2202E", "index %d out of valid range, 0..%d", n, len(a[0].S)-1)
		}
		b := []byte(a[0].S)
		b[n] = byte(v)
		return Datum{K: KBytea, S: string(b)}, nil
	}})
	regFn("get_bit", &fnSig{min: 2, kindFn: func(k []Kind) Kind {
		if k[0] == KBytea {
			return KInt
		}
		return kUnhandled
	}, ty: func(a []pgTy) pgTy { return pgTy{oid: tyInt4, mod: -1} }, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if !isBytea(a[0]) {
			return Null, errFnFall
		}
		n, err := fnInt(a[1])
		if err != nil {
			return Null, err
		}
		if n < 0 || n >= int64(len(a[0].S))*8 {
			return Null, errf("2202E", "index %d out of valid range, 0..%d", n, len(a[0].S)*8-1)
		}
		return IntD(int64(a[0].S[n/8]>>(n%8)) & 1), nil
	}})
	regFn("set_bit", &fnSig{min: 3, kindFn: func(k []Kind) Kind {
		if k[0] == KBytea {
			return KBytea
		}
		return kUnhandled
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if !isBytea(a[0]) {
			return Null, errFnFall
		}
		n, err := fnInt(a[1])
		if err != nil {
			return Null, err
		}
		v, err := fnInt(a[2])
		if err != nil {
			return Null, err
		}
		if n < 0 || n >= int64(len(a[0].S))*8 {
			return Null, errf("2202E", "index %d out of valid range, 0..%d", n, len(a[0].S)*8-1)
		}
		if v != 0 && v != 1 {
			return Null, errf(codeInvalidParameter, "new bit must be 0 or 1")
		}
		b := []byte(a[0].S)
		if v == 1 {
			b[n/8] |= 1 << (n % 8)
		} else {
			b[n/8] &^= 1 << (n % 8)
		}
		return Datum{K: KBytea, S: string(b)}, nil
	}})
	regFn("bit_count", &fnSig{min: 1, kindFn: func(k []Kind) Kind {
		if k[0] == KBytea {
			return KInt
		}
		return kUnhandled
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if !isBytea(a[0]) {
			return Null, errFnFall
		}
		n := 0
		for i := 0; i < len(a[0].S); i++ {
			n += bits.OnesCount8(a[0].S[i])
		}
		return IntD(int64(n)), nil
	}})

	// ---- bytea forms of text functions ----
	byteaOr := func(k Kind) func([]Kind) Kind {
		return func(a []Kind) Kind {
			if len(a) > 0 && a[0] == KBytea {
				return k
			}
			return kUnhandled
		}
	}
	regFn("strpos", &fnSig{min: 2, kindFn: func(a []Kind) Kind {
		if a[0] == KBytea {
			return KInt
		}
		return kUnhandled
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if !isBytea(a[0]) {
			return Null, errFnFall
		}
		i := bytes.Index([]byte(a[0].S), []byte(a[1].S))
		return IntD(int64(i + 1)), nil
	}})
	regFn("overlay", &fnSig{min: 3, max: 4, kindFn: byteaOr(KBytea), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if !isBytea(a[0]) {
			return Null, errFnFall
		}
		from, err := fnInt(a[2])
		if err != nil {
			return Null, err
		}
		l := int64(len(a[1].S))
		if len(a) == 4 {
			if l, err = fnInt(a[3]); err != nil {
				return Null, err
			}
		}
		if from < 1 {
			return Null, errf("2202E", "negative substring length not allowed")
		}
		s := a[0].S
		pre := min(from-1, int64(len(s)))
		post := min(max(pre+l, pre), int64(len(s)))
		return Datum{K: KBytea, S: s[:pre] + a[1].S + s[post:]}, nil
	}})
	for _, name := range []string{"btrim", "ltrim", "rtrim"} {
		name := name
		regFn(name, &fnSig{min: 1, max: 2, kindFn: byteaOr(KBytea), impl: func(c *evalCtx, a []Datum) (Datum, error) {
			if !isBytea(a[0]) || len(a) < 2 {
				return Null, errFnFall
			}
			s, cut := a[0].S, a[1].S
			switch name {
			case "ltrim":
				s = strings.TrimLeft(s, cut)
			case "rtrim":
				s = strings.TrimRight(s, cut)
			default:
				s = strings.Trim(s, cut)
			}
			return Datum{K: KBytea, S: s}, nil
		}})
	}
	regFn("decode", &fnSig{min: 2, kindFn: func(a []Kind) Kind { return KBytea }, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if strings.ToLower(a[1].String()) != "escape" {
			return Null, errFnFall
		}
		s := a[0].String()
		var b []byte
		for i := 0; i < len(s); i++ {
			if s[i] != '\\' {
				b = append(b, s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '\\' {
				b = append(b, '\\')
				i++
				continue
			}
			if i+3 < len(s) && isOct(s[i+1]) && isOct(s[i+2]) && isOct(s[i+3]) {
				v, _ := strconv.ParseUint(s[i+1:i+4], 8, 16)
				if v > 255 {
					return Null, errf(codeInvalidText, "invalid input syntax for type bytea")
				}
				b = append(b, byte(v))
				i += 3
				continue
			}
			return Null, errf(codeInvalidText, "invalid input syntax for type bytea")
		}
		return Datum{K: KBytea, S: string(b)}, nil
	}})

	regFn("normalize", &fnSig{min: 1, max: 2, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		form := "NFC"
		if len(a) > 1 {
			form = strings.ToUpper(a[1].String())
		}
		var f norm.Form
		switch form {
		case "NFC":
			f = norm.NFC
		case "NFD":
			f = norm.NFD
		case "NFKC":
			f = norm.NFKC
		case "NFKD":
			f = norm.NFKD
		default:
			return Null, errf(codeInvalidParameter, "invalid normalization form: %s", a[1].String())
		}
		return TextD(f.String(a[0].String())), nil
	}})

	// substring(text, pattern) is the POSIX regex form; substring(text, pattern, escape) is the SIMILAR form
	regFn("substr", &fnSig{min: 1, max: 3, kindFn: func(a []Kind) Kind {
		if len(a) >= 2 && a[1] == KText {
			return KText
		}
		return kUnhandled
	}, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		if len(a) < 2 || a[1].K != KText || a[0].K == KBytea {
			return Null, errFnFall
		}
		pat := a[1].String()
		if len(a) == 3 {
			var err error
			if pat, err = similarEscapeRegex(pat, a[2].String(), true); err != nil {
				return Null, err
			}
		}
		re, err := pgRegexCompile(pat, "")
		if err != nil {
			return Null, err
		}
		s := a[0].String()
		m := re.FindStringSubmatchIndex(s)
		if m == nil {
			return Null, nil
		}
		if re.NumSubexp() == 0 {
			return TextD(s[m[0]:m[1]]), nil
		}
		if m[2] < 0 {
			return Null, nil
		}
		return TextD(s[m[2]:m[3]]), nil
	}})
}

func isOct(c byte) bool { return c >= '0' && c <= '7' }

// similarEscapeRegex converts a SQL SIMILAR TO pattern to a POSIX regular expression
// (PostgreSQL similar_escape); with marks, escape+" delimits the part substring() returns.
func similarEscapeRegex(pat, esc string, marks bool) (string, error) {
	if len([]rune(esc)) > 1 {
		return "", errf("22019", "invalid escape string")
	}
	var b strings.Builder
	b.WriteString("^(?:")
	rs := []rune(pat)
	inSet := false
	open := true
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if esc != "" && string(r) == esc {
			if i+1 >= len(rs) {
				return "", errf("22025", "invalid escape string")
			}
			i++
			if rs[i] == '"' && marks {
				if open {
					b.WriteString(")(")
				} else {
					b.WriteString(")(?:")
				}
				open = !open
				continue
			}
			b.WriteString(`\` + string(rs[i]))
			continue
		}
		if inSet {
			b.WriteRune(r)
			if r == ']' {
				inSet = false
			}
			continue
		}
		switch r {
		case '[':
			inSet = true
			b.WriteRune(r)
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		case '.', '\\', '$', '^':
			b.WriteString(`\` + string(r))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(")$")
	out := b.String()
	if marks {
		// the first marker opens the capture group: ^(?:A)(B)(?:C)$ ; without markers the whole is one group
		out = strings.Replace(out, "^(?:", "^(?:", 1)
	}
	return out, nil
}

func decodeBytes(raw, enc string) (string, error) {
	switch strings.ToUpper(strings.NewReplacer("-", "", "_", "").Replace(enc)) {
	case "UTF8", "UTF", "UNICODE":
		if !utf8.ValidString(raw) {
			return "", errf("22021", "invalid byte sequence for encoding \"UTF8\": 0x%02x", firstBad(raw))
		}
		return raw, nil
	case "LATIN1", "ISO88591", "SQLASCII":
		return charmap.ISO8859_1.NewDecoder().String(raw)
	case "WIN1252", "WINDOWS1252":
		s, err := charmap.Windows1252.NewDecoder().String(raw)
		return s, err
	case "LATIN9", "ISO885915":
		return charmap.ISO8859_15.NewDecoder().String(raw)
	}
	return "", errf(codeInvalidParameter, "invalid source encoding name \"%s\"", enc)
}

func firstBad(s string) byte {
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && w == 1 {
			return s[i]
		}
		i += w
	}
	return 0
}

func encodeText(s, enc string) (string, error) {
	switch strings.ToUpper(strings.NewReplacer("-", "", "_", "").Replace(enc)) {
	case "UTF8", "UTF", "UNICODE":
		return s, nil
	case "LATIN1", "ISO88591":
		return encodeCharmap(charmap.ISO8859_1, s, "LATIN1")
	case "WIN1252", "WINDOWS1252":
		return encodeCharmap(charmap.Windows1252, s, "WIN1252")
	case "LATIN9", "ISO885915":
		return encodeCharmap(charmap.ISO8859_15, s, "LATIN9")
	case "SQLASCII":
		return s, nil
	}
	return "", errf(codeInvalidParameter, "invalid destination encoding name \"%s\"", enc)
}

func encodeCharmap(cm *charmap.Charmap, s, name string) (string, error) {
	var out []byte
	for _, r := range s {
		b, ok := cm.EncodeRune(r)
		if !ok {
			return "", errf("22P05", "character with byte sequence 0x%x in encoding \"UTF8\" has no equivalent in encoding \"%s\"", string(r), name)
		}
		out = append(out, b)
	}
	return string(out), nil
}

// parseIdent splits a possibly qualified SQL identifier into its parts.
func parseIdent(s string, strict bool) ([]string, error) {
	bad := func(detail string) error {
		return errf(codeInvalidParameter, "string is not a valid identifier: \"%s\"", s)
	}
	var parts []string
	i := 0
	skipWS := func() {
		for i < len(s) && unicode.IsSpace(rune(s[i])) {
			i++
		}
	}
	skipWS()
	for {
		if i >= len(s) {
			return nil, bad("")
		}
		if s[i] == '"' {
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed || b.Len() == 0 {
				return nil, bad("")
			}
			parts = append(parts, b.String())
		} else {
			st := i
			for i < len(s) {
				r, w := utf8.DecodeRuneInString(s[i:])
				if r == '_' || r == '$' && i > st || unicode.IsLetter(r) || (unicode.IsDigit(r) && i > st) || r >= 0x80 {
					i += w
					continue
				}
				break
			}
			if i == st {
				return nil, bad("")
			}
			parts = append(parts, strings.ToLower(s[st:i]))
		}
		skipWS()
		if i < len(s) && s[i] == '.' {
			i++
			skipWS()
			continue
		}
		break
	}
	if i < len(s) && strict {
		return nil, bad("")
	}
	return parts, nil
}
