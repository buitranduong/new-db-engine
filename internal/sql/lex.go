// Package sql implements the SQL dialect of STASK: a PostgreSQL-flavoured
// subset (single-table queries, DML with RETURNING, upsert, transactions)
// executed against the authoritative core.
package sql

import (
	"fmt"
	"strconv"
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
		case (c == 'b' || c == 'B' || c == 'x' || c == 'X') && i+1 < len(src) && src[i+1] == '\'' && bitLitEnd(src, i+2) > 0:
			// B'0101' and X'1F' bit string literals: a marker identifier, then the letter and digits as a string
			end := bitLitEnd(src, i+2)
			out = append(out, token{tIdent, "stask_bitlit", i}, token{tString, strings.ToLower(src[i:i+1]) + src[i+2:end], i})
			i = end + 1
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
			qid, next, ok := scanQuotedIdent(src, i)
			if !ok {
				return nil, syntaxErr(i, "unterminated quoted identifier")
			}
			out = append(out, token{tQuoted, qid, start})
			i = next
		case c == '$' && dollarTagEnd(src, i) > 0:
			// $$ text $$ or $tag$ text $tag$: no escapes inside
			start, te := i, dollarTagEnd(src, i)
			tag := src[i : te+1]
			end := strings.Index(src[te+1:], tag)
			if end < 0 {
				return nil, syntaxErr(i, "unterminated dollar-quoted string")
			}
			out = append(out, token{tString, src[te+1 : te+1+end], start})
			i = te + 1 + end + len(tag)
		case c == '$' && i+1 < len(src) && isDigit(src[i+1]):
			start := i
			i++
			for i < len(src) && isDigit(src[i]) {
				i++
			}
			out = append(out, token{tParam, src[start+1 : i], start})
		case isDigit(c) || c == '.' && i+1 < len(src) && isDigit(src[i+1]):
			start := i
			if c == '0' && i+1 < len(src) && strings.IndexByte("xXoObB", src[i+1]) >= 0 { // 0x1F, 0o17, 0b101 (PostgreSQL 16)
				j := i + 2
				for j < len(src) && (isHexByte(src[j]) || src[j] == '_') {
					j++
				}
				v, perr := strconv.ParseUint(strings.ToLower(src[i:j]), 0, 64)
				if perr != nil {
					kind := map[byte]string{'x': "hexadecimal", 'o': "octal", 'b': "binary"}[src[i+1]|0x20]
					return nil, syntaxErr(start, fmt.Sprintf("invalid %s integer at or near %q", kind, src[i:j]))
				}
				out = append(out, token{tNumber, strconv.FormatUint(v, 10), start})
				i = j
				if i < len(src) && (src[i] == '_' || unicode.IsLetter(rune(src[i]))) {
					return nil, syntaxErr(start, fmt.Sprintf("trailing junk after numeric literal at or near %q", src[start:i+1]))
				}
				continue
			}
			seenDot := false
			for i < len(src) {
				if isDigit(src[i]) || src[i] == '_' && i > start && isDigit(src[i-1]) && i+1 < len(src) && isDigit(src[i+1]) {
					i++
				} else if src[i] == '.' && !seenDot && !(i+1 < len(src) && src[i+1] == '.') { // 1..n is 1, .., n
					seenDot = true
					i++
				} else {
					break
				}
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
			if i < len(src) && (src[i] == '_' || src[i] == 'e' || src[i] == 'E') {
				return nil, syntaxErr(start, fmt.Sprintf("trailing junk after numeric literal at or near %q", src[start:i+1]))
			}
			out = append(out, token{tNumber, strings.ReplaceAll(src[start:i], "_", ""), start})
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
			if three := jsonarrLex3(src[i:]); three != "" {
				out = append(out, token{tOp, three, start})
				i += 3
				continue
			}
			if strings.HasPrefix(src[i:], "-|-") || strings.HasPrefix(src[i:], "<->") {
				out = append(out, token{tOp, src[i : i+3], start})
				i += 3
				continue
			}
			switch two {
			case "<=", ">=", "<>", "!=", "||", "::", "!~", "~*", "->", "==", "<<", ">>", "^@",
				"#>", "#-", "@>", "<@", "@?", "@@", "?|", "?&", "&&", "&<", "&>", "!!":
				out = append(out, token{tOp, two, start})
				i += 2
				continue
			}
			if strings.IndexByte("=<>+-*/%(),.;[]?:~{}&|^#@", c) < 0 {
				return nil, syntaxErr(i, fmt.Sprintf("unexpected character %q", c))
			}
			out = append(out, token{tOp, string(c), start})
			i++
		}
	}
	return append(out, token{tEOF, "", len(src)}), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexByte(c byte) bool { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// dollarTagEnd is the index of the closing '$' of a dollar-quote tag that
// starts at src[i] ($$ or $name$), or -1 if there is none ($1 is a parameter).
func dollarTagEnd(src string, i int) int {
	j := i + 1
	for j < len(src) && (src[j] == '_' || src[j] < utf8.RuneSelf && unicode.IsLetter(rune(src[j])) ||
		j > i+1 && isDigit(src[j]) || src[j] >= utf8.RuneSelf) {
		j++
	}
	if j < len(src) && src[j] == '$' {
		return j
	}
	return -1
}
