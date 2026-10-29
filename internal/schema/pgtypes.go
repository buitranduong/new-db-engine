package schema

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"stask/internal/pgnum"
)

// Helpers of the PostgreSQL types: uuid, bytea, json and jsonb. Values of
// these types are held in Value.S: uuid as its canonical 36-character text
// (stored as 16 bytes), bytea as raw bytes, json as given, jsonb
// normalized as PostgreSQL prints it.

// ParseUUID reads PostgreSQL uuid input (optional braces, hyphens after
// any group of four digits, either case) and returns the canonical text.
func ParseUUID(s string) (string, error) {
	t := strings.TrimSpace(s)
	if len(t) >= 2 && t[0] == '{' && t[len(t)-1] == '}' {
		t = t[1 : len(t)-1]
	}
	var hx [32]byte
	n := 0
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c == '-' && n%4 == 0 && n > 0 && n < 32 && t[i-1] != '-' {
			continue
		}
		if n >= 32 || !isHex(c) {
			return "", fmt.Errorf("invalid input syntax for type uuid: %q", s)
		}
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		hx[n] = c
		n++
	}
	if n != 32 || strings.HasSuffix(t, "-") {
		return "", fmt.Errorf("invalid input syntax for type uuid: %q", s)
	}
	h := string(hx[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// UUIDBytes is the 16 bytes of a canonical uuid text.
func UUIDBytes(canon string) ([16]byte, error) {
	var out [16]byte
	h := strings.ReplaceAll(canon, "-", "")
	if len(h) != 32 {
		return out, errors.New("bad uuid")
	}
	_, err := hex.Decode(out[:], []byte(h))
	return out, err
}

// UUIDString is the canonical text of 16 bytes.
func UUIDString(b []byte) string {
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ParseBytea reads bytea input: the hex format (\x4142) or the escape
// format (backslash-doubled and \nnn octal escapes).
func ParseBytea(s string) (string, error) {
	if strings.HasPrefix(s, `\x`) {
		h := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, s[2:])
		b, err := hex.DecodeString(h)
		if err != nil {
			return "", fmt.Errorf("invalid hexadecimal data in bytea: %q", s)
		}
		return string(b), nil
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '\\':
			out = append(out, '\\')
			i++
		case i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && s[i+2] >= '0' && s[i+2] <= '7' && s[i+3] >= '0' && s[i+3] <= '7':
			out = append(out, (s[i+1]-'0')<<6|(s[i+2]-'0')<<3|(s[i+3]-'0'))
			i += 3
		default:
			return "", fmt.Errorf("invalid input syntax for type bytea: %q", s)
		}
	}
	return string(out), nil
}

// FormatBytea is the hex output format.
func FormatBytea(raw string) string { return `\x` + hex.EncodeToString([]byte(raw)) }

// ValidJSON checks the syntax of json input (RFC 8259).
func ValidJSON(s string) error {
	p := jparser{s: s}
	if _, err := p.value(); err != nil {
		return err
	}
	p.ws()
	if p.i != len(p.s) {
		return p.errf("trailing characters")
	}
	return nil
}

// NormalizeJSONB parses s and prints it as PostgreSQL's jsonb output: keys
// sorted by length then bytes without duplicates (the last wins), ": " and
// ", " separators, numbers as numeric.
func NormalizeJSONB(s string) (string, error) {
	p := jparser{s: s}
	v, err := p.value()
	if err != nil {
		return "", err
	}
	p.ws()
	if p.i != len(p.s) {
		return "", p.errf("trailing characters")
	}
	var b strings.Builder
	if err := writeJSONB(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

type jkind uint8

const (
	jNull jkind = iota
	jBool
	jNum
	jStr
	jArr
	jObj
)

type jval struct {
	k    jkind
	s    string // string value, number text, "true"/"false"
	arr  []*jval
	keys []string
	vals []*jval
}

type jparser struct {
	s     string
	i     int
	depth int
}

func (p *jparser) errf(f string, a ...any) error {
	return fmt.Errorf("invalid input syntax for type json: "+f+" at position %d", append(a, p.i)...)
}

func (p *jparser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) value() (*jval, error) {
	p.ws()
	if p.i >= len(p.s) {
		return nil, p.errf("unexpected end of input")
	}
	if p.depth > 1000 {
		return nil, p.errf("too deep")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		p.i++
		p.depth++
		defer func() { p.depth-- }()
		v := &jval{k: jObj}
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return v, nil
		}
		for {
			p.ws()
			if p.i >= len(p.s) || p.s[p.i] != '"' {
				return nil, p.errf("expected a string key")
			}
			k, err := p.str()
			if err != nil {
				return nil, err
			}
			p.ws()
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return nil, p.errf("expected \":\"")
			}
			p.i++
			x, err := p.value()
			if err != nil {
				return nil, err
			}
			v.keys, v.vals = append(v.keys, k), append(v.vals, x)
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == '}' {
				p.i++
				return v, nil
			}
			return nil, p.errf("expected \",\" or \"}\"")
		}
	case c == '[':
		p.i++
		p.depth++
		defer func() { p.depth-- }()
		v := &jval{k: jArr}
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return v, nil
		}
		for {
			x, err := p.value()
			if err != nil {
				return nil, err
			}
			v.arr = append(v.arr, x)
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == ']' {
				p.i++
				return v, nil
			}
			return nil, p.errf("expected \",\" or \"]\"")
		}
	case c == '"':
		s, err := p.str()
		return &jval{k: jStr, s: s}, err
	case c == 't' && strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return &jval{k: jBool, s: "true"}, nil
	case c == 'f' && strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return &jval{k: jBool, s: "false"}, nil
	case c == 'n' && strings.HasPrefix(p.s[p.i:], "null"):
		p.i += 4
		return &jval{k: jNull}, nil
	case c == '-' || c >= '0' && c <= '9':
		st := p.i
		if p.s[p.i] == '-' {
			p.i++
		}
		d0 := p.i
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		if p.i == d0 || p.s[d0] == '0' && p.i-d0 > 1 {
			return nil, p.errf("invalid number")
		}
		if p.i < len(p.s) && p.s[p.i] == '.' {
			p.i++
			f0 := p.i
			for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
				p.i++
			}
			if p.i == f0 {
				return nil, p.errf("invalid number")
			}
		}
		if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
			p.i++
			if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
				p.i++
			}
			e0 := p.i
			for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
				p.i++
			}
			if p.i == e0 {
				return nil, p.errf("invalid number")
			}
		}
		return &jval{k: jNum, s: p.s[st:p.i]}, nil
	}
	return nil, p.errf("unexpected character %q", p.s[p.i])
}

// str reads a JSON string at p.i (on the opening quote) and returns it
// decoded.
func (p *jparser) str() (string, error) {
	p.i++
	var b []byte
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			if !utf8.Valid(b) {
				return "", p.errf("invalid UTF-8")
			}
			return string(b), nil
		case c < 0x20:
			return "", p.errf("control character in string")
		case c == '\\':
			p.i++
			if p.i >= len(p.s) {
				return "", p.errf("unterminated string")
			}
			switch e := p.s[p.i]; e {
			case '"', '\\', '/':
				b = append(b, e)
			case 'b':
				b = append(b, '\b')
			case 'f':
				b = append(b, '\f')
			case 'n':
				b = append(b, '\n')
			case 'r':
				b = append(b, '\r')
			case 't':
				b = append(b, '\t')
			case 'u':
				r, n, err := p.unicode()
				if err != nil {
					return "", err
				}
				b = utf8.AppendRune(b, r)
				p.i += n
				continue
			default:
				return "", p.errf("invalid escape")
			}
			p.i++
		default:
			b = append(b, c)
			p.i++
		}
	}
	return "", p.errf("unterminated string")
}

// unicode reads \uXXXX (and a following low surrogate) with p.i on the u;
// it returns the rune and the number of bytes consumed after p.i-1's
// backslash position (p.i advanced by the caller).
func (p *jparser) unicode() (rune, int, error) {
	hex4 := func(at int) (rune, bool) {
		if at+4 > len(p.s) {
			return 0, false
		}
		v, err := strconv.ParseUint(p.s[at:at+4], 16, 32)
		return rune(v), err == nil
	}
	r, ok := hex4(p.i + 1)
	if !ok {
		return 0, 0, p.errf("invalid unicode escape")
	}
	n := 5
	if r >= 0xD800 && r < 0xDC00 {
		if strings.HasPrefix(p.s[p.i+5:], `\u`) {
			if lo, ok := hex4(p.i + 7); ok && lo >= 0xDC00 && lo < 0xE000 {
				return 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00), 11, nil
			}
		}
		return 0, 0, p.errf("invalid unicode surrogate pair")
	}
	if r >= 0xDC00 && r < 0xE000 {
		return 0, 0, p.errf("invalid unicode surrogate pair")
	}
	return r, n, nil
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

func writeJSONB(b *strings.Builder, v *jval) error {
	switch v.k {
	case jNull:
		b.WriteString("null")
	case jBool:
		b.WriteString(v.s)
	case jNum:
		d, err := pgnum.Parse(v.s)
		if err != nil {
			return fmt.Errorf("invalid input syntax for type json: number %q out of range", v.s)
		}
		b.WriteString(d.String())
	case jStr:
		if strings.ContainsRune(v.s, 0) {
			return errors.New("unsupported Unicode escape sequence: \\u0000 cannot be converted to text")
		}
		writeJSONString(b, v.s)
	case jArr:
		b.WriteByte('[')
		for i, x := range v.arr {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writeJSONB(b, x); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case jObj:
		last := map[string]int{}
		for i, k := range v.keys {
			last[k] = i
		}
		keys := make([]string, 0, len(last))
		for k := range last {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if len(keys[i]) != len(keys[j]) {
				return len(keys[i]) < len(keys[j])
			}
			return bytes.Compare([]byte(keys[i]), []byte(keys[j])) < 0
		})
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSONString(b, k)
			b.WriteString(": ")
			if err := writeJSONB(b, v.vals[last[k]]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	}
	return nil
}
