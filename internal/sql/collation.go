package sql

// Text collation (docs/14 Appendix W). STASK stores and indexes text as
// bytes. A database default collation (staskd -collation, SetDefaultCollation)
// and COLLATE "name" on an expression change only how text is ORDERED
// (ORDER BY, < <= > >=, BETWEEN, min/max, greatest/least, window ORDER BY,
// DISTINCT ON, set-operation ordering). Equality, GROUP BY, DISTINCT,
// UNIQUE and primary keys stay bytewise: the collator result is refined by
// a byte comparison, so two texts compare equal only if their bytes do
// (PostgreSQL deterministic collations). Nothing on disk depends on the
// collation, so changing the flag between restarts is safe.
//
// The collator is golang.org/x/text/collate (CLDR, BSD-3) plus the glibc
// adjustments that raised the measured agreement with PostgreSQL on
// glibc 2.41 en_US.utf8 from 87% to 100% of pairs (cmd/collfid, Appendix W):
// spaces, punctuation and non-currency symbols are ignorable at the first
// levels (glibc >= 2.28, "alternate shifted"), and strings that differ
// only by ignorables are ordered by comparing them character by character
// with ignorables below every other character.

import (
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Collation is one named text ordering.
type Collation struct {
	Name     string // as reported by pg_collation / SHOW lc_collate
	bytewise bool
	glibc    bool // glibc-style ignorables (see above); false: plain CLDR (und-x-icu)
	pool     *sync.Pool
}

// Bytewise reports whether the collation orders text exactly as the
// stored (byte-ordered) indexes do. A nil collation is the default one.
func (c *Collation) Bytewise() bool {
	if c == nil {
		c = DefaultCollation()
	}
	return c.bytewise
}

var (
	collC       = &Collation{Name: "C.UTF-8", bytewise: true}
	defaultColl atomic.Pointer[Collation]
	collCache   sync.Map // normalized name -> *Collation
)

func init() { defaultColl.Store(collC) }

// DefaultCollation is the database default collation.
func DefaultCollation() *Collation { return defaultColl.Load() }

// SetDefaultCollation sets the database default collation by name (C,
// POSIX, en_US.utf8, vi_VN.utf8, und-x-icu, ...). It is a process-wide
// setting made once at start.
func SetDefaultCollation(name string) error {
	if n := strings.ToLower(strings.TrimSpace(name)); n == "" || n == "default" {
		name = "C"
	}
	c, err := LookupCollation(name)
	if err != nil {
		return err
	}
	defaultColl.Store(c)
	settingDefaults["lc_collate"], settingDefaults["lc_ctype"] = c.Name, c.Name
	return nil
}

// LookupCollation resolves a collation name; "" and "default" give the
// database default.
func LookupCollation(name string) (*Collation, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimPrefix(n, "pg_catalog.")
	if n == "" || n == "default" {
		return DefaultCollation(), nil
	}
	if v, ok := userColls.Load(n); ok { // CREATE COLLATION (collddl.go)
		return v.(*Collation), nil
	}
	if v, ok := collCache.Load(n); ok {
		return v.(*Collation), nil
	}
	c, err := buildCollation(name, n)
	if err != nil {
		return nil, err
	}
	v, _ := collCache.LoadOrStore(n, c)
	return v.(*Collation), nil
}

func buildCollation(orig, n string) (*Collation, error) {
	bad := errf("42704", "collation \"%s\" for encoding \"UTF8\" does not exist", orig)
	switch n {
	case "c", "c.utf8", "c.utf-8", "posix", "ucs_basic":
		name := "C.UTF-8"
		if n == "posix" {
			name = "POSIX"
		}
		return &Collation{Name: name, bytewise: true}, nil
	}
	// "und-x-icu", "vi-x-icu", "vi-vn-x-icu", "en_us.utf8", "vi_vn", "en_US.UTF-8"
	icu := strings.HasSuffix(n, "-x-icu")
	t := strings.TrimSuffix(n, "-x-icu")
	if i := strings.IndexByte(t, 0x2e); i >= 0 {
		t = t[:i]
	}
	t = strings.ReplaceAll(t, "_", "-")
	var tag language.Tag
	if t == "und" {
		tag = language.Und
	} else {
		var err error
		if tag, err = language.Parse(t); err != nil || tag == language.Und {
			return nil, bad
		}
		if _, _, conf := language.NewMatcher(collate.Supported()).Match(tag); conf == language.No {
			return nil, bad
		}
	}
	c := &Collation{Name: orig, glibc: !icu}
	c.pool = &sync.Pool{New: func() any { return collate.New(tag) }}
	return c, nil
}

// glibcIgnorable: glibc >= 2.28 (ISO 14651_t1) ignores spaces, punctuation
// and every symbol but currency signs at the first levels. Measured against
// PostgreSQL on glibc 2.41: all of Z*, P*, Sm, Sk and So up to U+2BFF
// except 6 letterlike So, and Sc except 5 (docs/14 Appendix W).
func glibcIgnorable(r rune) bool {
	if r < 0x80 {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '$')
	}
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) && !unicode.Is(unicode.Sc, r)
}

func hasIgnorable(s string) bool {
	for _, r := range s {
		if glibcIgnorable(r) {
			return true
		}
	}
	return false
}

func stripIgnorable(s string) string {
	if !hasIgnorable(s) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if !glibcIgnorable(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tieOrder compares two strings character by character with ignorable
// characters below all others.
func tieOrder(a, b string) int {
	key := func(r rune) rune {
		if glibcIgnorable(r) {
			return r
		}
		return r + 0x110000
	}
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if x, y := key(ra[i]), key(rb[i]); x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return len(ra) - len(rb)
}

// CollateCompare orders a and b under c; c == nil is the database default.
// The result is 0 only if the bytes are equal.
func CollateCompare(c *Collation, a, b string) int {
	if a == b {
		return 0
	}
	if c == nil {
		c = DefaultCollation()
	}
	if c.bytewise {
		return strings.Compare(a, b)
	}
	co := c.pool.Get().(*collate.Collator)
	var r int
	if c.glibc {
		r = co.CompareString(stripIgnorable(a), stripIgnorable(b))
		if r == 0 {
			r = tieOrder(a, b)
		}
	} else {
		r = co.CompareString(a, b)
	}
	c.pool.Put(co)
	if r != 0 {
		if r < 0 {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
