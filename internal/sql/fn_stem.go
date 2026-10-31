package sql

import "strings"

// englishStem is the Snowball "english" (Porter2) stemmer that PostgreSQL's english_stem
// dictionary uses. Precedent: snowballstem.org/algorithms/english/stemmer.html, checked word by word
// against ts_lexize('english_stem', w) of postgres:16 (testdata/english_stem.golden).

func isStemVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u', 'y':
		return true
	}
	return false
}

var stemDoubles = []string{"bb", "dd", "ff", "gg", "mm", "nn", "pp", "rr", "tt"}

var stemExceptions1 = map[string]string{
	"skis": "ski", "skies": "sky", "dying": "die", "lying": "lie", "tying": "tie", "idly": "idl", "gently": "gentl",
	"ugly": "ugli", "early": "earli", "only": "onli", "singly": "singl",
	"sky": "sky", "news": "news", "howe": "howe", "atlas": "atlas", "cosmos": "cosmos", "bias": "bias", "andes": "andes",
}

var stemExceptions2 = map[string]bool{"inning": true, "outing": true, "canning": true, "herring": true, "earring": true,
	"proceed": true, "exceed": true, "succeed": true}

// regionStart returns the index where the region after the first non-vowel following a vowel begins,
// searching from start.
func regionStart(w []byte, start int) int {
	for i := start + 1; i < len(w); i++ {
		if !isStemVowel(w[i]) && isStemVowel(w[i-1]) {
			return i + 1
		}
	}
	return len(w)
}

func englishStem(word string) string {
	w := []byte(word)
	if len(w) <= 2 {
		return word
	}
	if w[0] == '\'' {
		w = w[1:]
	}
	if r, ok := stemExceptions1[string(w)]; ok {
		return r
	}
	if len(w) <= 2 {
		return string(w)
	}
	// mark consonant y
	if w[0] == 'y' {
		w[0] = 'Y'
	}
	for i := 1; i < len(w); i++ {
		if w[i] == 'y' && isStemVowel(w[i-1]) {
			w[i] = 'Y'
		}
	}
	isV := func(b byte) bool { return b != 'Y' && isStemVowel(b) }
	r1 := len(w)
	r2 := len(w)
	for _, p := range []string{"gener", "commun", "arsen"} {
		if strings.HasPrefix(string(w), p) {
			r1 = len(p)
			goto haveR1
		}
	}
	r1 = len(w)
	for i := 1; i < len(w); i++ {
		if !isV(w[i]) && isV(w[i-1]) {
			r1 = i + 1
			break
		}
	}
haveR1:
	r2 = len(w)
	for i := r1 + 1; i < len(w); i++ {
		if !isV(w[i]) && isV(w[i-1]) {
			r2 = i + 1
			break
		}
	}
	has := func(suf string) bool { return strings.HasSuffix(string(w), suf) }
	cut := func(n int) { w = w[:len(w)-n] }
	app := func(s string) { w = append(w, s...) }
	inR1 := func(n int) bool { return len(w)-n >= r1 }
	inR2 := func(n int) bool { return len(w)-n >= r2 }
	hasVowel := func(end int) bool {
		for i := 0; i < end && i < len(w); i++ {
			if isV(w[i]) {
				return true
			}
		}
		return false
	}
	shortSyllableEnd := func(end int) bool { // does w[:end] end in a short syllable
		if end == 2 {
			return isV(w[0]) && !isV(w[1])
		}
		if end >= 3 {
			c := w[end-1]
			return !isV(w[end-3]) && isV(w[end-2]) && !isV(c) && c != 'w' && c != 'x' && c != 'Y'
		}
		return false
	}
	isShort := func() bool { return r1 >= len(w) && shortSyllableEnd(len(w)) }

	// step 0
	for _, s := range []string{"'s'", "'s", "'"} {
		if has(s) {
			cut(len(s))
			break
		}
	}
	// step 1a
	switch {
	case has("sses"):
		cut(2)
	case has("ied") || has("ies"):
		if len(w) > 4 {
			cut(2)
		} else {
			cut(1)
		}
	case has("us") || has("ss"):
	case has("s"):
		if hasVowel(len(w) - 2) {
			cut(1)
		}
	}
	if stemExceptions2[string(w)] {
		return string(w)
	}
	// step 1b
	switch {
	case has("eedly"):
		if inR1(5) {
			cut(3)
		}
	case has("eed"):
		if inR1(3) {
			cut(1)
		}
	default:
		for _, s := range []string{"ingly", "edly", "ing", "ed"} {
			if has(s) {
				if hasVowel(len(w) - len(s)) {
					cut(len(s))
					switch {
					case has("at") || has("bl") || has("iz"):
						app("e")
					default:
						dbl := false
						for _, d := range stemDoubles {
							if has(d) {
								dbl = true
							}
						}
						if dbl {
							cut(1)
						} else if isShort() {
							app("e")
						}
					}
				}
				break
			}
		}
	}
	// step 1c
	if len(w) > 2 && (w[len(w)-1] == 'y' || w[len(w)-1] == 'Y') && !isV(w[len(w)-2]) {
		w[len(w)-1] = 'i'
	}
	// step 2
	step2 := []struct{ s, r string }{
		{"ization", "ize"}, {"ational", "ate"}, {"fulness", "ful"}, {"ousness", "ous"}, {"iveness", "ive"},
		{"tional", "tion"}, {"biliti", "ble"}, {"lessli", "less"}, {"entli", "ent"}, {"ation", "ate"}, {"alism", "al"},
		{"aliti", "al"}, {"ousli", "ous"}, {"iviti", "ive"}, {"fulli", "ful"}, {"enci", "ence"}, {"anci", "ance"},
		{"abli", "able"}, {"izer", "ize"}, {"ator", "ate"}, {"alli", "al"}, {"bli", "ble"}, {"ogi", "og"}, {"li", ""},
	}
	for _, p := range step2 {
		if has(p.s) {
			if inR1(len(p.s)) {
				switch p.s {
				case "ogi":
					if len(w) >= 4 && w[len(w)-4] == 'l' {
						cut(3)
						app("og")
					}
				case "li":
					if len(w) >= 3 && strings.IndexByte("cdeghkmnrt", w[len(w)-3]) >= 0 {
						cut(2)
					}
				default:
					cut(len(p.s))
					app(p.r)
				}
			}
			break
		}
	}
	// step 3
	step3 := []struct{ s, r string }{
		{"ational", "ate"}, {"tional", "tion"}, {"alize", "al"}, {"icate", "ic"}, {"iciti", "ic"}, {"ative", ""},
		{"ical", "ic"}, {"ness", ""}, {"ful", ""},
	}
	for _, p := range step3 {
		if has(p.s) {
			if inR1(len(p.s)) {
				if p.s == "ative" {
					if inR2(len(p.s)) {
						cut(len(p.s))
					}
				} else {
					cut(len(p.s))
					app(p.r)
				}
			}
			break
		}
	}
	// step 4
	step4 := []string{"ement", "ance", "ence", "able", "ible", "ment", "ant", "ent", "ism", "ate", "iti", "ous", "ive", "ize", "ion", "al", "er", "ic"}
	for _, s := range step4 {
		if has(s) {
			if inR2(len(s)) {
				if s == "ion" {
					if len(w) >= 4 && (w[len(w)-4] == 's' || w[len(w)-4] == 't') {
						cut(3)
					}
				} else {
					cut(len(s))
				}
			}
			break
		}
	}
	// step 5
	if has("e") {
		if inR2(1) || inR1(1) && !shortSyllableEnd(len(w)-1) {
			cut(1)
		}
	} else if has("l") && inR2(1) && len(w) >= 2 && w[len(w)-2] == 'l' {
		cut(1)
	}
	for i := range w {
		if w[i] == 'Y' {
			w[i] = 'y'
		}
	}
	return string(w)
}
