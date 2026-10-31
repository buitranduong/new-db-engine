package sql

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// overlay(string, new, start [, count]): replace count characters (default
// the length of new) of string from position start with new.
func pgOverlay(args []Datum) (Datum, error) {
	for _, a := range args {
		if a.IsNull() {
			return Null, nil
		}
	}
	s, nw := []rune(args[0].String()), []rune(args[1].String())
	start := int(args[2].I)
	n := len(nw)
	if len(args) == 4 {
		n = int(args[3].I)
	}
	if n < 0 {
		return Null, errf("22011", "negative substring length not allowed")
	}
	if start < 1 {
		return Null, errf("22011", "negative substring length not allowed")
	}
	head := min(start-1, len(s))
	tail := min(head+n, len(s))
	return TextD(string(s[:head]) + string(nw) + string(s[tail:])), nil
}

// formatSpec parses the part of a format() specifier after '%':
// [position$][-][width]type. It returns the index after the type letter.
func formatSpec(f string, i int) (pos int, left bool, width int, typ byte, next int, ok bool) {
	j := i
	for j < len(f) && f[j] >= '0' && f[j] <= '9' {
		j++
	}
	if j > i && j < len(f) && f[j] == '$' {
		pos, _ = strconv.Atoi(f[i:j])
		j++
	} else {
		j = i
	}
	if j < len(f) && f[j] == '-' {
		left = true
		j++
	}
	k := j
	for k < len(f) && f[k] >= '0' && f[k] <= '9' {
		k++
	}
	if k > j {
		width, _ = strconv.Atoi(f[j:k])
	}
	if k >= len(f) {
		return 0, false, 0, 0, k, false
	}
	return pos, left, width, f[k], k, true
}

func padFormat(s string, width int, left bool) string {
	n := width - utf8.RuneCountInString(s)
	if n <= 0 {
		return s
	}
	if left {
		return s + strings.Repeat(" ", n)
	}
	return strings.Repeat(" ", n) + s
}

// rowCompare expands a comparison of two row constructors, (a,b) op (c,d),
// into scalar comparisons (PostgreSQL compares rows column by column,
// lexicographically for the ordering operators). ok is false when the two
// sides are not rows of one length.
func rowCompare(op string, l, r Expr) (Expr, bool) {
	lr, ok1 := l.(*RowExpr)
	rr, ok2 := r.(*RowExpr)
	if !ok1 || !ok2 || len(lr.Items) != len(rr.Items) || len(lr.Items) == 0 {
		return nil, false
	}
	n := len(lr.Items)
	cmp := func(i int, o string) Expr { return &Binary{Op: o, L: lr.Items[i], R: rr.Items[i]} }
	switch op {
	case "=", "<>", "!=":
		join, o := "and", "="
		if op != "=" {
			join, o = "or", "<>"
		}
		out := cmp(0, o)
		for i := 1; i < n; i++ {
			out = &Binary{Op: join, L: out, R: cmp(i, o)}
		}
		return out, true
	case "<", "<=", ">", ">=":
		strict := op[:1] // < or >
		// last column uses the operator itself, earlier ones the strict form
		out := cmp(n-1, op)
		for i := n - 2; i >= 0; i-- {
			out = &Binary{Op: "or", L: cmp(i, strict), R: &Binary{Op: "and", L: cmp(i, "="), R: out}}
		}
		return out, true
	}
	return nil, false
}

// recordText is the text form of a row value: (1,a), NULL as nothing,
// fields with special characters double-quoted.
func recordText(vals []Datum) string {
	var b strings.Builder
	b.WriteByte('(')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		if v.IsNull() {
			continue
		}
		s := v.String()
		if v.K == KBool {
			s = map[bool]string{true: "t", false: "f"}[v.Bool()]
		}
		if s == "" || strings.ContainsAny(s, "(),\"\\ \t\n\r") {
			s = `"` + strings.NewReplacer(`"`, `""`, `\`, `\\`).Replace(s) + `"`
		}
		b.WriteString(s)
	}
	b.WriteByte(')')
	return b.String()
}
