package main

import "fmt"

// SIM-number generators. Both are deterministic and injective in i (no
// "seen" map: at 20M keys a map would be the largest thing on the heap and
// would drown the numbers this tool exists to measure).

// simbenchNum is cmd/simbench numStr: 0 + nine digits, a permutation of the
// first 1e9 residues (uniform; no shared prefixes).
func simbenchNum(i int64) string { return fmt.Sprintf("0%09d", (i*7919+12345)%1000000000) }

// prefixes follow cmd/searchbench: Vietnamese mobile prefixes (090/091/098..)
// with a weight each; a few are much more common, which is what makes
// trigram postings of the popular 3-grams very long.
var prefixes = []struct {
	p string
	w int
}{
	{"090", 8}, {"093", 6}, {"089", 4}, {"070", 3}, {"079", 3}, {"077", 3},
	{"091", 8}, {"094", 6}, {"088", 4}, {"083", 3}, {"084", 3}, {"085", 3},
	{"096", 8}, {"097", 8}, {"098", 8}, {"086", 4}, {"032", 3}, {"033", 3}, {"034", 3}, {"035", 3},
	{"092", 3}, {"056", 2}, {"058", 2}, {"099", 2}, {"059", 1},
}

var prefixCum []int // cumulative weights, len(prefixes)+1

func init() {
	prefixCum = make([]int, len(prefixes)+1)
	for i, p := range prefixes {
		prefixCum[i+1] = prefixCum[i] + p.w
	}
}

// prefixNum is a 10-digit number: weighted prefix + a 7-digit tail that is
// distinct within a prefix (rank*7919 mod 1e7; 7919 is coprime to 1e7), so
// the number is unique for i up to 1e7 * W / max weight (> 1e8 here).
func prefixNum(i int64) string {
	W := int64(prefixCum[len(prefixes)])
	r := int(i % W)
	p := 0
	for prefixCum[p+1] <= r {
		p++
	}
	rank := (i/W)*int64(prefixes[p].w) + int64(r-prefixCum[p])
	return fmt.Sprintf("%s%07d", prefixes[p].p, (rank*7919+12345)%10000000)
}

func numFn(dist string) (func(int64) string, error) {
	switch dist {
	case "simbench":
		return simbenchNum, nil
	case "prefix":
		return prefixNum, nil
	}
	return nil, fmt.Errorf("unknown -dist %q (simbench | prefix)", dist)
}

var carriers = []string{"viettel", "mobifone", "vinaphone", "vietnamobile"}

func carrierOf(i int64) string { return carriers[i%4] }
func priceOf(i int64) int64    { return 100000 + (i*37%500)*10000 }
func statusOf(i int64) string {
	switch h := i % 20; {
	case h < 17:
		return "available"
	case h < 19:
		return "blocked"
	}
	return "inactive"
}
