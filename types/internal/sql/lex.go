// Package sql implements the SQL dialect of STASK: a PostgreSQL-flavoured
// subset (single-table queries, DML with RETURNING, upsert, transactions)
// executed against the authoritative core.
package sql

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tQuoted // "quoted identifier"
	tNumber
	tString
	tParam // $n
	tOp
)

type token struct {
	k   tokKind
	s   string // identifiers are lower-cased, keywords included
	pos int
}

func lex(src string) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, syntaxErr(i, "unterminated comment")
			}
			i += end + 4
		case c == '\'' || (c == 'E' || c == 'e') && i+1 < len(src) && src[i+1] == '\'':
			start := i
			escapes := c != '\''
			if escapes {
				i++
			}
			i++
			var b strings.Builder
			for {
				if i >= len(src) {
					return nil, syntaxErr(start, "unterminated string literal")
				}
				if src[i] == '\'' {
					if i+1 < len(src) && src[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				if escapes && src[i] == '\\' && i+1 < len(src) {
					switch src[i+1] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(src[i+1])
					}
					i += 2
					continue
				}
				b.WriteByte(src[i])
				i++
			}
			out = append(out, token{tString, b.String(), start})
		case c == '"':
			start := i
			end := strings.IndexByte(src[i+1:], '"')
			if end < 0 {
				return nil, syntaxErr(i, "unterminated quoted identifier")
			}
			out = append(out, token{tQuoted, src[i+1 : i+1+end], start})
			i += end + 2
		case c == '$' && i+1 < len(src) && isDigit(src[i+1]):
			start := i
			i++
			for i < len(src) && isDigit(src[i]) {
				i++
			}
			out = append(out, token{tParam, src[start+1 : i], start})
		case isDigit(c) || c == '.' && i+1 < len(src) && isDigit(src[i+1]):
			start := i
			for i < len(src) && (isDigit(src[i]) || src[i] == '.') {
				i++
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && isDigit(src[j]) {
					i = j
					for i < len(src) && isDigit(src[i]) {
						i++
					}
				}
			}
			out = append(out, token{tNumber, src[start:i], start})
		case c == '_' || c < utf8.RuneSelf && unicode.IsLetter(rune(c)) || c >= utf8.RuneSelf:
			start := i
			for i < len(src) {
				r, n := utf8.DecodeRuneInString(src[i:])
				if r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r) {
					i += n
					continue
				}
				break
			}
			if i == start {
				return nil, syntaxErr(i, "unexpected character")
			}
			out = append(out, token{tIdent, strings.ToLower(src[start:i]), start})
		default:
			start := i
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			if strings.HasPrefix(src[i:], "!~*") {
				out = append(out, token{tOp, "!~*", start})
				i += 3
				continue
			}
			switch two {
			case "<=", ">=", "<>", "!=", "||", "::", "!~", "~*", "->", "==":
				out = append(out, token{tOp, two, start})
				i += 2
				continue
			}
			if strings.IndexByte("=<>+-*/%(),.;[]?:~{}&", c) < 0 {
				return nil, syntaxErr(i, fmt.Sprintf("unexpected character %q", c))
			}
			out = append(out, token{tOp, string(c), start})
			i++
		}
	}
	return append(out, token{tEOF, "", len(src)}), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
