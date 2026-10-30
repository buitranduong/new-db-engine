package main

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

type variant struct {
	name string
	cmp  func(a, b string) int
	show bool
}

var mu sync.Mutex

func mk(tag language.Tag, pre func(string) string, opts ...collate.Option) func(a, b string) int {
	c := collate.New(tag, opts...)
	return func(a, b string) int {
		if a == b {
			return 0
		}
		if pre != nil {
			a, b = pre(a), pre(b)
		}
		r := c.CompareString(a, b)
		if r == 0 {
			return strings.Compare(a, b)
		}
		return r
	}
}

var stroke = strings.NewReplacer("đ", "d̵", "Đ", "D̵", "Ð", "D̵")

func variants() []variant {
	return append(v0(), variants2()...)
}
func v0() []variant {
	return []variant{
		{"bytes(C)", strings.Compare, false},
		{"cldr-en", mk(language.AmericanEnglish, nil), true},
		{"cldr-vi", mk(language.Vietnamese, nil), false},
		{"cldr-und", mk(language.Und, nil), false},
		{"en+stroke", mk(language.AmericanEnglish, stroke.Replace), true},
		{"vi+stroke", mk(language.Vietnamese, stroke.Replace), false},
	}
}

func strip(classes string) func(string) string {
	return func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if unicode.IsSpace(r) || unicode.IsPunct(r) || (strings.Contains(classes, "S") && unicode.IsSymbol(r) && !unicode.Is(unicode.Sc, r)) {
				continue
			}
			b.WriteRune(r)
		}
		return b.String()
	}
}

// shifted: compare with ignorables removed; ties by the full (non-ignorable) strings.
func mkShifted(tag language.Tag, pre func(string) string, st func(string) string, opts ...collate.Option) func(a, b string) int {
	c := collate.New(tag, opts...)
	return func(a, b string) int {
		if a == b {
			return 0
		}
		pa, pb := a, b
		if pre != nil {
			pa, pb = pre(a), pre(b)
		}
		if r := c.CompareString(st(pa), st(pb)); r != 0 {
			return r
		}
		if r := c.CompareString(pa, pb); r != 0 {
			return r
		}
		return strings.Compare(a, b)
	}
}

func mkShiftedB(tag language.Tag, st func(string) string, opts ...collate.Option) func(a, b string) int {
	c := collate.New(tag, opts...)
	return func(a, b string) int {
		if a == b {
			return 0
		}
		if r := c.CompareString(st(a), st(b)); r != 0 {
			return r
		}
		return strings.Compare(a, b)
	}
}

func ignorable(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || (unicode.IsSymbol(r) && !unicode.Is(unicode.Sc, r))
}

func tieKey(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	k := func(r rune) rune {
		if ignorable(r) {
			return r
		}
		return r + 0x110000
	}
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if x, y := k(ra[i]), k(rb[i]); x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return len(ra) - len(rb)
}

func mkShiftedC(tag language.Tag, opts ...collate.Option) func(a, b string) int {
	c := collate.New(tag, opts...)
	st := strip("S")
	return func(a, b string) int {
		if a == b {
			return 0
		}
		if r := c.CompareString(st(a), st(b)); r != 0 {
			return r
		}
		if r := tieKey(a, b); r != 0 {
			return r
		}
		return strings.Compare(a, b)
	}
}

func variants2() []variant {
	return []variant{
		{"shiftC(PZS+tiekey)", mkShiftedC(language.AmericanEnglish), true},
		{"shiftC vi", mkShiftedC(language.Vietnamese), false},
		{"shiftB(PZS)", mkShiftedB(language.AmericanEnglish, strip("S")), true},
		{"shiftB(PZ)", mkShiftedB(language.AmericanEnglish, strip("")), false},
		{"shiftB(PZS) vi", mkShiftedB(language.Vietnamese, strip("S")), false},
		{"shift(PZ)", mkShifted(language.AmericanEnglish, nil, strip("")), true},
		{"shift(PZS)", mkShifted(language.AmericanEnglish, nil, strip("S")), false},
		{"shift(PZ)+stroke", mkShifted(language.AmericanEnglish, stroke.Replace, strip("")), true},
		{"shift(PZS)+stroke", mkShifted(language.AmericanEnglish, stroke.Replace, strip("S")), false},
	}
}
