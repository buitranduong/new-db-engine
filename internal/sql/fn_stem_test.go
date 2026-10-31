package sql

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestEnglishStem compares englishStem with ts_lexize('english_stem', word) of postgres:16 for 11968
// words (testdata/english_stem.golden: tab separated word and stem, empty stem = stop word).
func TestEnglishStem(t *testing.T) {
	f, err := os.Open("testdata/english_stem.golden")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	bad, n := 0, 0
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		n++
		if got := englishStem(parts[0]); got != parts[1] {
			bad++
			if bad <= 15 {
				t.Errorf("stem(%q) = %q, postgres says %q", parts[0], got, parts[1])
			}
		}
	}
	if n < 11000 {
		t.Errorf("golden file too small: %d words", n)
	}
	if bad > 0 {
		t.Errorf("%d of %d words differ", bad, n)
	}
}
