package sql

// ArrayDatum builds an array datum from elements laid out row-major over
// dims (nil dims = empty array), for the binary-parameter decoder.
func ArrayDatum(el []Datum, dims []int, ek Kind) Datum {
	if len(el) == 0 {
		return (&arr{ek: ek}).datum()
	}
	return (&arr{ek: ek, dims: dims, el: el}).datum()
}
