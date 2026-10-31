package sql

import "strings"

// typesCall runs the functions of the added types (interval, ranges, macaddr8, bit strings; exttypes.go).
func (c *evalCtx) typesCall(f *Func, args []Datum) (Datum, bool, error) {
	if rngTagOfName(f.Name) != 0 {
		return rangeFunc(f.Name, args)
	}
	tagged := false
	for _, a := range args {
		if a.K == KText && a.D != 0 {
			tagged = true
			break
		}
	}
	if tagged {
		for _, a := range args {
			if a.IsNull() {
				return Null, true, nil
			}
		}
		switch f.Name {
		case "stask_jpath_match", "stask_overlap", "stask_contains", "stask_contained", "stask_tsphrase":
			if len(args) == 2 {
				op := map[string]string{"stask_jpath_match": "@@", "stask_overlap": "&&", "stask_contains": "@>", "stask_contained": "<@", "stask_tsphrase": "stask_tsphrase"}[f.Name]
				if d, ok, err := tsBinary(op, args[0], args[1]); ok {
					return d, true, err
				}
			}
			if f.Name == "stask_jpath_match" || f.Name == "stask_tsphrase" {
				return Null, false, nil
			}
			return rangeBinary(f.Name, args[0], args[1])
		case "stask_rng_overleft":
			return rangeBinary("&<", args[0], args[1])
		case "stask_rng_overright":
			return rangeBinary("&>", args[0], args[1])
		case "stask_rng_adjacent":
			return rangeBinary("-|-", args[0], args[1])
		}
		if d, ok, err := tsFunc(f.Name, args); ok {
			return d, true, err
		}
		if d, ok, err := bitFunc(f.Name, args); ok {
			return d, true, err
		}
		if d, ok, err := macFunc(f.Name, args); ok {
			return d, true, err
		}
		if d, ok, err := rangeFunc(f.Name, args); ok {
			return d, true, err
		}
		return Null, false, nil
	}
	switch f.Name {
	case "justify_days", "justify_hours", "justify_interval":
		if len(args) != 1 {
			return Null, false, nil
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		iv, err := coerce(args[0], KInterval)
		if err != nil {
			return Null, true, err
		}
		var r Datum
		switch f.Name {
		case "justify_days":
			r, err = justifyDays(iv)
		case "justify_hours":
			r, err = justifyHours(iv)
		default:
			r, err = justifyInterval(iv)
		}
		return r, true, err
	case "make_interval":
		if len(args) > 7 {
			return Null, false, nil
		}
		pos := map[string]int{"years": 0, "months": 1, "weeks": 2, "days": 3, "hours": 4, "mins": 5, "secs": 6}
		var slot [7]*Datum
		for i := range args {
			at := i
			if i < len(f.ArgNames) && f.ArgNames[i] != "" {
				p, ok := pos[strings.ToLower(f.ArgNames[i])]
				if !ok {
					return Null, true, errf("42883", "function make_interval(%s => ...) does not exist", f.ArgNames[i])
				}
				at = p
			}
			slot[at] = &args[i]
		}
		var v float64
		var n [6]int64
		for i := range slot {
			if slot[i] == nil {
				continue
			}
			if slot[i].IsNull() {
				return Null, true, nil
			}
			if i == 6 {
				x, err := coerce(*slot[i], KFloat)
				if err != nil {
					return Null, true, err
				}
				v = x.F
				continue
			}
			x, err := coerce(*slot[i], KInt)
			if err != nil {
				return Null, true, err
			}
			n[i] = x.I
		}
		r, err := makeInterval(n[0], n[1], n[2], n[3], n[4], n[5], v)
		return r, true, err
	case "isfinite":
		if len(args) == 1 && args[0].K == KInterval {
			return BoolD(true), true, nil
		}
	}
	return Null, false, nil
}

var typesFuncNames = func() map[string]bool {
	m := map[string]bool{"length": true, "bit_length": true, "octet_length": true, "bit_count": true, "get_bit": true, "set_bit": true,
		"substr": true, "substring": true, "overlay": true, "strpos": true, "trunc": true, "macaddr8_set7bit": true,
		"justify_days": true, "justify_hours": true, "justify_interval": true, "make_interval": true, "isfinite": true,
		"stask_contains": true, "stask_jpath_match": true, "stask_tsnot": true, "stask_tsphrase": true, "numnode": true, "querytree": true, "stask_contained": true, "stask_overlap": true, "stask_rng_overleft": true, "stask_rng_overright": true,
		"stask_rng_adjacent": true, "isempty": true, "lower": true, "upper": true, "lower_inc": true, "upper_inc": true,
		"lower_inf": true, "upper_inf": true, "range_merge": true}
	for _, t := range rngTypes {
		m[t.name] = true
	}
	return m
}()

// typesFuncKind is the result kind of the functions of typesCall.
func typesFuncKind(name string, args []Kind) (Kind, bool) {
	switch strings.ToLower(name) {
	case "justify_days", "justify_hours", "justify_interval", "make_interval":
		return KInterval, true
	case "stask_rng_overleft", "stask_rng_overright", "stask_rng_adjacent", "isempty", "lower_inc", "upper_inc", "lower_inf", "upper_inf":
		return KBool, true
	case "range_merge", "macaddr8_set7bit", "set_bit":
		return KText, true
	case "bit_count", "get_bit", "numnode":
		return KInt, true
	case "stask_tsnot", "stask_tsphrase", "querytree":
		return KText, true
	case "trunc":
		if len(args) == 1 && args[0] == KText { // trunc(macaddr)
			return KText, true
		}
	}
	if rngTagOfName(name) != 0 {
		return KText, true
	}
	return KNull, false
}
