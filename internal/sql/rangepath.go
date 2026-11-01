package sql

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"stask/internal/core"
	"stask/internal/schema"
)

// Ordered key-range scans of the primary tree (docs/14, row "rangefix").
//
// A WHERE that bounds the leading primary-key column(s) by <, <=, >, >=, BETWEEN or a prefix LIKE is
// turned into ONE range of the encoded row key, which the primary tree walks in key order instead of
// reading the whole table (PostgreSQL's btree scan keys from the same quals, _bt_preprocess_keys; InnoDB
// positions the cursor at the lower bound and stops at the upper one). The same range serves UPDATE and
// DELETE (which also lock only the rows it returns), INSERT .. SELECT, aggregates and the general
// executor's pushed filter. The scan is a SUPERSET of the matches: every returned row is checked against
// the whole WHERE again, so a range that is too wide is slow, never wrong. It also sees the
// transaction's own writes (core.Tx.ScanKeysOwn), which the batch of an import needs: its INSERT .. SELECT
// reads the rows its own UPDATE just wrote.
//
// Which keys are ordered how:
//   - a one-column text key is its own bytes, so SQL order = key order under a bytewise collation (a
//     range under another collation is not derived: boundsOf drops it, the scan stays whole);
//   - a one-column integer key is stored as decimal text, which is NOT numeric order; a range whose two
//     ends are non-negative and have the same number of digits is one text interval (strings of equal
//     length compare as numbers; shorter or longer keys that fall inside it are removed by the recheck),
//     any other integer range is left to the full scan;
//   - a composite key is the concatenation of order-preserving encodings (schema.AppendKeyValue): the
//     equalities on its first columns give a prefix, and a range on the next column bounds the interval.
//
// STASK_RANGESCAN=off (rangeScanOff) restores the full scans (the differential test and diagnosis).

var rangeScanOff atomic.Bool

func init() {
	if os.Getenv("STASK_RANGESCAN") == "off" {
		rangeScanOff.Store(true)
	}
}

// rangeStats counts, for the tests, how often a range scan ran.
var rangeStats struct{ scans atomic.Int64 }

// pkRange is the interval of row keys WHERE confines the table to.
type pkRange struct {
	lo, hi         string // hi == "" : no upper bound
	loIncl, hiIncl bool
	empty          bool   // no key can match
	twoSided       bool   // a bounded interval (or a prefix), not just a half line
	ordered        bool   // key order is SQL order of the single text key (an ORDER BY .. LIMIT may stop early)
	cond           string // for EXPLAIN
}

// canRange reports whether tx can read a table through the primary tree range.
func canRange(tx *core.Tx) bool {
	return tx != nil && !rangeScanOff.Load() && tx.Iso() != core.Serializable
}

// pkRangeOf derives the key interval from the top-level conjuncts of where, or ok=false.
func pkRangeOf(t *schema.Table, alias string, where Expr, ctx *evalCtx) (r pkRange, ok bool) {
	if where == nil || rangeScanOff.Load() || t.System || !t.HasPK() {
		return r, false
	}
	if !t.Composite() {
		if !usableCol(t, t.Key) {
			return r, false
		}
		ct := t.Columns[t.KeyIndex()].T
		b := boundsOf(t, alias, where, ctx)[t.Key]
		if b == nil || b.nothing || b.eq != nil || b.hasIn || b.lo == nil && b.hi == nil {
			return r, false
		}
		switch ct {
		case schema.TString, schema.TText:
			r = pkRange{lo: "", loIncl: true, twoSided: b.lo != nil && b.hi != nil, ordered: true}
			if b.lo != nil {
				r.lo, r.loIncl = b.lo.S, b.loIncl
			}
			if b.hi != nil {
				r.hi, r.hiIncl = b.hi.S, b.hiIncl
				if r.hi == "" && !r.hiIncl {
					r.empty = true // key < '' matches nothing
				} else if r.hi == "" {
					r.hi, r.hiIncl = "\x00", false // key <= '': only the empty key
				}
			}
		case schema.TInt:
			if b.lo == nil || b.hi == nil {
				return r, false
			}
			lo, hi := b.lo.I, b.hi.I
			if !b.loIncl {
				if lo == 1<<63-1 {
					return r, false
				}
				lo++
			}
			if !b.hiIncl {
				if hi == -1<<63 {
					return r, false
				}
				hi--
			}
			if lo > hi {
				r = pkRange{empty: true, twoSided: true}
				break
			}
			if lo < 0 || len(strconv.FormatInt(lo, 10)) != len(strconv.FormatInt(hi, 10)) {
				return r, false
			}
			r = pkRange{lo: strconv.FormatInt(lo, 10), loIncl: true, hi: strconv.FormatInt(hi, 10), hiIncl: true, twoSided: true}
		default:
			return r, false
		}
		r.cond = pkRangeText(t.Key, b)
		return r, true
	}
	bounds := boundsOf(t, alias, where, ctx)
	var prefix []byte
	var conds []string
	n := 0
	for ; n < len(t.KeyCols); n++ {
		name := t.KeyCols[n]
		b := bounds[name]
		if b == nil || b.nothing || b.eq == nil || !usableCol(t, name) {
			break
		}
		i, _ := t.Col(name)
		prefix = schema.AppendKeyValue(prefix, t.Columns[i].T, *b.eq)
		conds = append(conds, name+" = "+valStr(*b.eq))
	}
	var lo, hi *schema.Value
	var loIncl, hiIncl bool
	if n < len(t.KeyCols) {
		name := t.KeyCols[n]
		if b := bounds[name]; b != nil && !b.nothing && b.eq == nil && !b.hasIn && usableCol(t, name) && (b.lo != nil || b.hi != nil) {
			lo, hi, loIncl, hiIncl = b.lo, b.hi, b.loIncl, b.hiIncl
			conds = append(conds, pkRangeText(name, b))
		}
	}
	if n == 0 && lo == nil && hi == nil {
		return r, false
	}
	r = pkRange{loIncl: true, twoSided: n > 0 || lo != nil && hi != nil}
	p := string(prefix)
	r.lo, r.hi = p, core.PrefixEnd(p) // "" when the prefix is empty or all 0xff: unbounded
	if n == 0 {
		r.lo = "\x01" // after nothing: present values start with 0x01; NULL (0xff) never satisfies a comparison
		r.hi = "\x02"
	}
	if lo != nil || hi != nil {
		i, _ := t.Col(t.KeyCols[n])
		ct := t.Columns[i].T
		if lo != nil {
			e := string(schema.AppendKeyValue(append([]byte(nil), prefix...), ct, *lo))
			r.lo = e
			if !loIncl {
				r.lo = core.PrefixEnd(e)
			}
		}
		if hi != nil {
			e := string(schema.AppendKeyValue(append([]byte(nil), prefix...), ct, *hi))
			r.hi = e
			if hiIncl {
				r.hi = core.PrefixEnd(e)
			}
		}
		if r.lo != "" && r.hi != "" && r.lo >= r.hi {
			r.empty = true
		}
	}
	r.cond = "(" + strings.Join(t.KeyCols, ", ") + ") " + strings.Join(conds, " AND ")
	return r, true
}

// scanKeyRange visits, in key order, the rows of the interval that satisfy match.
func (s *Session) scanKeyRange(tx *core.Tx, t *schema.Table, r pkRange, match func(schema.Row) (bool, error),
	fn func(schema.Row) (bool, error)) error {
	if r.empty {
		return nil
	}
	rangeStats.scans.Add(1)
	var ferr error
	serr := tx.ScanKeysOwn(t.Name, r.lo, r.loIncl, r.hi, r.hiIncl, func(_ string, row schema.Row) bool {
		m, err := match(row)
		if err != nil {
			ferr = err
			return false
		}
		if !m {
			return true
		}
		cont, err := fn(row)
		if err != nil {
			ferr = err
			return false
		}
		return cont
	})
	if serr != nil {
		return serr
	}
	return ferr
}
