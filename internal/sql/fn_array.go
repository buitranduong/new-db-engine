package sql

// Array functions added by compat-funcs: trim_array.

func init() {
	regFn("trim_array", &fnSig{min: 2, max: 2, kind: KArray, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		arr, err := arrOf(a[0], KText)
		if err != nil {
			return Null, err
		}
		n, err := fnInt(a[1])
		if err != nil {
			return Null, err
		}
		if len(arr.dims) > 1 {
			return Null, errf("2202E", "trim_array requires a one-dimensional array")
		}
		if n < 0 || int(n) > len(arr.el) {
			return Null, errf("2202E", "number of elements to trim must be between 0 and %d", len(arr.el))
		}
		return arrDatum(arr.el[:len(arr.el)-int(n)], arr.ek), nil
	}})
}
