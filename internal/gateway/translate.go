package gateway

import (
	"fmt"
	"strings"
	"unicode"
)

// token kinds of the small lexer used for shapes and translation
const (
	tWord   = iota
	tQuoted // "identifier"
	tString // 'literal' (also E'..')
	tNumber
	tOp
	tSpace
)

type token struct {
	kind int
	s    string
}

func lex(sql string) []token {
	var out []token
	r := []rune(sql)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			j := i
			for j < len(r) && unicode.IsSpace(r[j]) {
				j++
			}
			out = append(out, token{tSpace, " "})
			i = j
		case c == '-' && i+1 < len(r) && r[i+1] == '-':
			for i < len(r) && r[i] != '\n' {
				i++
			}
		case c == '\'' || ((c == 'E' || c == 'e') && i+1 < len(r) && r[i+1] == '\''):
			j := i
			if c != '\'' {
				j++
			}
			j++
			for j < len(r) {
				if r[j] == '\'' {
					if j+1 < len(r) && r[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				if r[j] == '\\' && c != '\'' {
					j++
				}
				j++
			}
			out = append(out, token{tString, string(r[i:min(j+1, len(r))])})
			i = j + 1
		case c == '"':
			j := i + 1
			for j < len(r) && !(r[j] == '"' && (j+1 >= len(r) || r[j+1] != '"')) {
				if r[j] == '"' {
					j++
				}
				j++
			}
			out = append(out, token{tQuoted, string(r[i:min(j+1, len(r))])})
			i = j + 1
		case unicode.IsDigit(c):
			j := i
			for j < len(r) && (unicode.IsDigit(r[j]) || r[j] == '.') {
				j++
			}
			out = append(out, token{tNumber, string(r[i:j])})
			i = j
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || r[j] == '$') {
				j++
			}
			out = append(out, token{tWord, string(r[i:j])})
			i = j
		default:
			j := i + 1
			if j < len(r) && strings.ContainsRune("|:<>=!", c) && strings.ContainsRune("|:=>", r[j]) {
				j++
			}
			out = append(out, token{tOp, string(r[i:j])})
			i = j
		}
	}
	return out
}

// Shape is the statement with its literals replaced by '?', so the
// decisions about one form of query are shared by all its executions.
func Shape(sql string) string {
	var b strings.Builder
	for _, t := range lex(sql) {
		switch t.kind {
		case tString, tNumber:
			b.WriteByte('?')
		case tWord:
			b.WriteString(strings.ToLower(t.s))
		default:
			b.WriteString(t.s)
		}
	}
	return strings.TrimSpace(b.String())
}

// readOnlyCandidate: one SELECT (or WITH .. SELECT) statement without
// locking clauses or functions that write. Anything else stays on PostgreSQL.
func readOnlyCandidate(sql string) (string, bool) {
	s := strings.TrimSpace(sql)
	s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	toks := lex(s)
	first := ""
	for _, t := range toks {
		if t.kind == tWord {
			first = strings.ToLower(t.s)
			break
		}
	}
	if first != "select" && first != "with" {
		return "", false
	}
	for i, t := range toks {
		if t.kind == tOp && t.s == ";" {
			return "", false // several statements
		}
		if t.kind == tWord {
			switch strings.ToLower(t.s) {
			case "insert", "update", "delete", "merge", "nextval", "setval", "into", "pg_advisory_lock", "pg_sleep", "currval", "lastval", "txid_current":
				return "", false
			case "for":
				if n := nextWord(toks, i); n == "update" || n == "share" || n == "no" || n == "key" {
					return "", false
				}
			}
		}
	}
	return s, true
}

func nextWord(toks []token, i int) string {
	for j := i + 1; j < len(toks); j++ {
		if toks[j].kind == tWord {
			return strings.ToLower(toks[j].s)
		}
		if toks[j].kind != tSpace {
			return ""
		}
	}
	return ""
}

// typedLiteral maps PostgreSQL typed literals (type 'text') to ClickHouse.
var typedLiteral = map[string]string{
	"timestamptz": "parseDateTime64BestEffort(%s, 6, 'UTC')",
	"timestamp":   "parseDateTime64BestEffort(%s, 6)",
	"date":        "toDate32(%s)",
}

// Translate rewrites a PostgreSQL SELECT for ClickHouse: the tables keep
// their names (the views of docs/35 section 8, read under the gate of
// section 7) and the PostgreSQL functions and literals
// with another spelling are mapped. It returns an error for what it knows
// it cannot translate; the rest is caught by the result comparison before
// a shape is ever answered from ClickHouse.
func Translate(sql string) (string, error) {
	if r, ok := rewriteTopN(sql); ok {
		sql = r
	}
	toks := lex(sql)
	var b strings.Builder
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		next := nextTok(toks, i)
		switch {
		case t.kind == tWord && strings.EqualFold(t.s, "md5") && next.s == "(":
			b.WriteString("lower(hex(MD5")
			j := skipTo(toks, i, "(")
			end := matchParen(toks, j)
			if end < 0 {
				return "", fmt.Errorf("unbalanced parentheses")
			}
			inner, err := Translate(join(toks[j : end+1]))
			if err != nil {
				return "", err
			}
			b.WriteString(inner + "))")
			i = end
		case t.kind == tWord && typedLiteral[strings.ToLower(t.s)] != "" && next.kind == tString && next.s[0] == '\'':
			// timestamptz '2026-10-01 00:00:00+00' and the like
			b.WriteString(fmt.Sprintf(typedLiteral[strings.ToLower(t.s)], next.s))
			i = skipTo(toks, i+1, next.s)
		case t.kind == tQuoted:
			b.WriteString("`" + strings.ReplaceAll(t.s[1:len(t.s)-1], `""`, `"`) + "`")
		case t.kind == tString && (t.s[0] == 'E' || t.s[0] == 'e'):
			return "", fmt.Errorf("escape string literal")
		case t.kind == tOp && t.s == "~":
			return "", fmt.Errorf("regular expression operator")
		default:
			b.WriteString(t.s)
		}
	}
	return b.String(), nil
}

func nextTok(toks []token, i int) token {
	for j := i + 1; j < len(toks); j++ {
		if toks[j].kind != tSpace {
			return toks[j]
		}
	}
	return token{}
}

func skipTo(toks []token, i int, s string) int {
	for j := i; j < len(toks); j++ {
		if toks[j].s == s {
			return j
		}
	}
	return -1
}

func matchParen(toks []token, open int) int {
	d := 0
	for j := open; j < len(toks); j++ {
		switch toks[j].s {
		case "(":
			d++
		case ")":
			d--
			if d == 0 {
				return j
			}
		}
	}
	return -1
}

func join(toks []token) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.s)
	}
	return b.String()
}
