package sql

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// pg_trgm (PostgreSQL 16 appendix F.35): show_trgm, word_similarity, strict_word_similarity, the
// operators % <% %> <<% %>> <-> <<-> <->> <<<-> <->>>, and set_limit/show_limit. The functions need
// CREATE EXTENSION pg_trgm, as in PostgreSQL. Trigram extraction is the one of similarity()
// (ddlobjs_ext.go): lower-cased alphanumeric words padded with two blanks before and one after.
// Non-ASCII trigrams are compared as characters (PostgreSQL compares CRC hashes there, so the
// order of show_trgm differs for non-ASCII words).

func trgmWords(s string) [][]string {
	var out [][]string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		rs := []rune("  " + w + " ")
		var ts []string
		for i := 0; i+3 <= len(rs); i++ {
			ts = append(ts, string(rs[i:i+3]))
		}
		out = append(out, ts)
	}
	return out
}

func trgmSet(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range trgmWords(s) {
		for _, t := range w {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// wordSimilarity is the greatest similarity between the trigrams of a and any extent of the
// trigram sequence of b; with strict, extents begin and end at word boundaries of b.
func wordSimilarity(a, b string, strict bool) float64 {
	A := map[string]bool{}
	for _, w := range trgmWords(a) {
		for _, t := range w {
			A[t] = true
		}
	}
	type pos struct {
		t           string
		first, last bool
	}
	var seq []pos
	for _, w := range trgmWords(b) {
		for i, t := range w {
			seq = append(seq, pos{t, i == 0, i == len(w)-1})
		}
	}
	if len(A) == 0 || len(seq) == 0 {
		return 0
	}
	best := 0.0
	for i := range seq {
		if strict && !seq[i].first {
			continue
		}
		cnt := map[string]int{}
		common, uniq := 0, 0
		for j := i; j < len(seq); j++ {
			t := seq[j].t
			if cnt[t] == 0 {
				uniq++
				if A[t] {
					common++
				}
			}
			cnt[t]++
			if strict && !seq[j].last {
				continue
			}
			s := float64(float32(float64(common) / float64(len(A)+uniq-common)))
			if s > best {
				best = s
			}
		}
	}
	return best
}

func trgmGate(c *evalCtx, name string) error {
	if s := c.session(); s != nil && !s.extInstalled("pg_trgm") {
		return errf(codeUndefinedFunction, "function %s does not exist", name)
	}
	return nil
}

func trgmThreshold(c *evalCtx, name, def string) float64 {
	if s := c.session(); s != nil {
		if v, ok := s.vars[name]; ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return f
			}
		}
	}
	f, _ := strconv.ParseFloat(def, 64)
	return f
}

func real4(f float64) Datum {
	r := FloatD(float64(float32(f)))
	r.S = f4Mark
	return r
}

func init() {
	pg := func(names string, min, max int, kind Kind, ty uint32, impl func(c *evalCtx, a []Datum) (Datum, error)) {
		s := &fnSig{min: min, max: max, kind: kind, impl: func(c *evalCtx, a []Datum) (Datum, error) {
			if err := trgmGate(c, "pg_trgm"); err != nil {
				return Null, err
			}
			return impl(c, a)
		}}
		if ty != 0 {
			s.ty = tyOf(ty)
		}
		regFn(names, s)
	}
	pg("show_trgm", 1, 1, KArray, 0, func(c *evalCtx, a []Datum) (Datum, error) {
		ts := trgmSet(a[0].String())
		el := make([]Datum, len(ts))
		for i, t := range ts {
			el[i] = TextD(t)
		}
		d := arrayD(el)
		d.I = int64(KText)
		return d, nil
	})
	pg("word_similarity", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(wordSimilarity(a[0].String(), a[1].String(), false)), nil
	})
	pg("strict_word_similarity", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(wordSimilarity(a[0].String(), a[1].String(), true)), nil
	})
	pg("stask_trgm_sim", 2, 2, KBool, 0, func(c *evalCtx, a []Datum) (Datum, error) {
		return BoolD(similarity(a[0].String(), a[1].String()) >= trgmThreshold(c, "pg_trgm.similarity_threshold", "0.3")), nil
	})
	pg("stask_trgm_word", 2, 2, KBool, 0, func(c *evalCtx, a []Datum) (Datum, error) {
		return BoolD(wordSimilarity(a[0].String(), a[1].String(), false) >= trgmThreshold(c, "pg_trgm.word_similarity_threshold", "0.6")), nil
	})
	pg("stask_trgm_word_r", 2, 2, KBool, 0, func(c *evalCtx, a []Datum) (Datum, error) {
		return BoolD(wordSimilarity(a[1].String(), a[0].String(), false) >= trgmThreshold(c, "pg_trgm.word_similarity_threshold", "0.6")), nil
	})
	pg("stask_trgm_strict", 2, 2, KBool, 0, func(c *evalCtx, a []Datum) (Datum, error) {
		return BoolD(wordSimilarity(a[0].String(), a[1].String(), true) >= trgmThreshold(c, "pg_trgm.strict_word_similarity_threshold", "0.5")), nil
	})
	pg("stask_trgm_strict_r", 2, 2, KBool, 0, func(c *evalCtx, a []Datum) (Datum, error) {
		return BoolD(wordSimilarity(a[1].String(), a[0].String(), true) >= trgmThreshold(c, "pg_trgm.strict_word_similarity_threshold", "0.5")), nil
	})
	pg("stask_trgm_dist", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(1 - similarity(a[0].String(), a[1].String())), nil
	})
	pg("stask_trgm_word_dist", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(1 - wordSimilarity(a[0].String(), a[1].String(), false)), nil
	})
	pg("stask_trgm_word_dist_r", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(1 - wordSimilarity(a[1].String(), a[0].String(), false)), nil
	})
	pg("stask_trgm_strict_dist", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(1 - wordSimilarity(a[0].String(), a[1].String(), true)), nil
	})
	pg("stask_trgm_strict_dist_r", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(1 - wordSimilarity(a[1].String(), a[0].String(), true)), nil
	})
	pg("show_limit", 0, 0, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		return real4(trgmThreshold(c, "pg_trgm.similarity_threshold", "0.3")), nil
	})
	pg("set_limit", 1, 1, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		f, err := fnFloat(a[0])
		if err != nil {
			return Null, err
		}
		if f < 0 || f > 1 {
			return Null, errf(codeInvalidParameter, "\"pg_trgm.similarity_threshold\" must be in the range 0 to 1")
		}
		if s := c.session(); s != nil {
			s.vars["pg_trgm.similarity_threshold"] = strconv.FormatFloat(float64(float32(f)), 'g', -1, 32)
		}
		return real4(f), nil
	})
	// operator spellings; maps used by the lexer/parser
	for op, fn := range trgmOps {
		if op == "<->" { // shared with the tsquery phrase operator (stask_tsphrase, typesfuncs.go): dispatched below
			continue
		}
		otherOps[op] = fn
	}
	pg("stask_tsphrase", 2, 2, KFloat, tyFloat4, func(c *evalCtx, a []Datum) (Datum, error) {
		// reached only when neither side is a tsvector/tsquery value: pg_trgm distance
		return real4(1 - similarity(a[0].String(), a[1].String())), nil
	})
	for n, v := range map[string]string{"pg_trgm.similarity_threshold": "0.3", "pg_trgm.word_similarity_threshold": "0.6", "pg_trgm.strict_word_similarity_threshold": "0.5"} {
		if _, ok := settingDefaults[n]; !ok {
			settingDefaults[n] = v
		}
	}
}

// trgmOps are the pg_trgm operators other than % (which is the modulo token): spelling -> function.
var trgmOps = map[string]string{
	"<%": "stask_trgm_word", "%>": "stask_trgm_word_r", "<<%": "stask_trgm_strict", "%>>": "stask_trgm_strict_r",
	"<->": "stask_trgm_dist", "<<->": "stask_trgm_word_dist", "<->>": "stask_trgm_word_dist_r",
	"<<<->": "stask_trgm_strict_dist", "<->>>": "stask_trgm_strict_dist_r",
}

// trgmOpAt returns the pg_trgm operator at the start of s, longest first.
func trgmOpAt(s string) string {
	for _, op := range []string{"<<<->", "<->>>", "<<->", "<->>", "<<%", "%>>", "<->", "<%", "%>"} {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return ""
}

// isTextLit reports a string literal that is not a number (so `x % 'abc'` is text similarity).
func isTextLit(e Expr) bool {
	l, ok := e.(*Lit)
	if !ok || l.V.K != KText {
		return false
	}
	_, err := strconv.ParseFloat(strings.TrimSpace(l.V.S), 64)
	return err != nil
}

func isAnyTextLit(e Expr) bool {
	l, ok := e.(*Lit)
	return ok && l.V.K == KText
}

// trgmPercent is `text % text` evaluated on two text values (the executor met no literal to tell).
func (c *evalCtx) trgmPercent(l, r Datum) (Datum, error) {
	if err := trgmGate(c, "pg_trgm"); err != nil {
		return Null, err
	}
	return BoolD(similarity(l.String(), r.String()) >= trgmThreshold(c, "pg_trgm.similarity_threshold", "0.3")), nil
}
