package proj

import (
	"math"
	"math/rand"
	"testing"
)

// Both float encodings must round-trip bit-exactly.
func TestFloatEncodingsLossless(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := map[string][]float64{}
	var dec, noisy, ints, mixed []float64
	for i := 0; i < 8192; i++ {
		dec = append(dec, math.Round((25+5*math.Sin(float64(i)/30)+rng.Float64()*0.2)*10)/10)
		noisy = append(noisy, rng.NormFloat64()*1e3)
		ints = append(ints, float64(rng.Intn(100)))
		if i%100 == 0 {
			mixed = append(mixed, math.NaN())
		} else {
			mixed = append(mixed, 0.1*float64(i))
		}
	}
	cases["decimal"], cases["noisy"], cases["ints"], cases["nan"] = dec, noisy, ints, mixed
	for name, col := range cases {
		enc := encodeFloats(col)
		got := decodeFloats(enc, len(col))
		for i := range col {
			if math.Float64bits(got[i]) != math.Float64bits(col[i]) && !(math.IsNaN(got[i]) && math.IsNaN(col[i])) {
				t.Fatalf("%s[%d]: %v != %v", name, i, got[i], col[i])
			}
		}
		t.Logf("%-8s mode=%d %.2f bytes/value", name, enc[0], float64(len(enc))/float64(len(col)))
	}
	if encodeFloats(dec)[0] != 2 || encodeFloats(ints)[0] != 1 {
		t.Fatal("decimal encoding not chosen")
	}
}
