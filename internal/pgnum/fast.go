package pgnum

import "strconv"

// fastString prints a finite Dec whose coefficient fits in 64 bits without math/big's conversion
// (the scan of a numeric column spends most of its time there).
func (d Dec) fastString() (string, bool) {
	if d.sp != finite {
		return "", false
	}
	var u uint64
	if d.coef != nil {
		if !d.coef.IsUint64() {
			return "", false
		}
		u = d.coef.Uint64()
	}
	var tmp [24]byte
	digits := strconv.AppendUint(tmp[:0], u, 10)
	sc := int(d.scale)
	out := make([]byte, 0, len(digits)+sc+3)
	if d.neg {
		out = append(out, '-')
	}
	if sc <= 0 {
		out = append(out, digits...)
		return string(out), true
	}
	if len(digits) <= sc {
		out = append(out, '0', '.')
		for i := len(digits); i < sc; i++ {
			out = append(out, '0')
		}
		out = append(out, digits...)
		return string(out), true
	}
	out = append(out, digits[:len(digits)-sc]...)
	out = append(out, '.')
	out = append(out, digits[len(digits)-sc:]...)
	return string(out), true
}
