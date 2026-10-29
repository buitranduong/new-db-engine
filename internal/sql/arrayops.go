package sql

import (
	"strconv"
	"strings"

	"stask/internal/schema"
)

// PostgreSQL arrays beyond the text form of datum.go (docs/05 section 26,
// branch jsonarr). An array value stays a KArray datum (S = the PostgreSQL
// text form, I = the element kind); this file reads and writes that text
// as a rectangular, possibly multi-dimensional array, and implements the
// array operators and functions. Lower bounds are always 1.

type arr struct {
	ek   Kind
	dims []int
	el   []Datum
}

type arrParser struct {
	s    string
	i    int
	ek   Kind
	el   []Datum
	dims []int
	leaf bool
	nest bool
	bad  string
}

func badArray(s string) error {
	return errf(codeInvalidText, "malformed array literal: %q", s)
}

// parseArr reads array d (text form, nested braces for more dimensions).
func parseArr(d Datum) (*arr, error) {
	s := strings.TrimSpace(d.S)
	if strings.HasPrefix(s, "[") { // dimension decoration "[1:3]={...}"
		if j := strings.Index(s, "="); j > 0 {
			s = s[j+1:]
		}
	}
	ek := Kind(d.I)
	if d.K != KArray {
		ek = KText
	}
	p := &arrParser{s: s, ek: ek}
	if err := p.parse(0); err != nil {
		return nil, badArray(d.S)
	}
	p.skip()
	if p.i != len(p.s) {
		return nil, badArray(d.S)
	}
	a := &arr{ek: ek, el: p.el, dims: p.dims}
	if len(a.el) == 0 {
		a.dims = nil
	}
	if p.bad != "" {
		return nil, errf(codeInvalidText, "%s", p.bad)
	}
	return a, nil
}

func (p *arrParser) skip() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *arrParser) parse(depth int) error {
	p.skip()
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return errf(codeInvalidText, "x")
	}
	p.i++
	p.skip()
	n := 0
	leaf, nest := false, false
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return nil
	}
	for {
		p.skip()
		if p.i >= len(p.s) {
			return errf(codeInvalidText, "x")
		}
		if p.s[p.i] == '{' {
			if leaf {
				return errf(codeInvalidText, "x")
			}
			nest = true
			if err := p.parse(depth + 1); err != nil {
				return err
			}
		} else {
			if nest {
				return errf(codeInvalidText, "x")
			}
			leaf = true
			if err := p.elem(); err != nil {
				return err
			}
		}
		n++
		p.skip()
		if p.i >= len(p.s) {
			return errf(codeInvalidText, "x")
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] == '}' {
			p.i++
			break
		}
		return errf(codeInvalidText, "x")
	}
	for len(p.dims) <= depth {
		p.dims = append(p.dims, -1)
	}
	if p.dims[depth] < 0 {
		p.dims[depth] = n
	} else if p.dims[depth] != n {
		p.bad = "malformed array literal: multidimensional arrays must have sub-arrays with matching dimensions"
	}
	return nil
}

func (p *arrParser) elem() error {
	var b strings.Builder
	quoted := false
	if p.s[p.i] == '"' {
		quoted = true
		p.i++
		for p.i < len(p.s) && p.s[p.i] != '"' {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			b.WriteByte(p.s[p.i])
			p.i++
		}
		if p.i >= len(p.s) {
			return errf(codeInvalidText, "x")
		}
		p.i++
	} else {
		for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			b.WriteByte(p.s[p.i])
			p.i++
		}
	}
	e := b.String()
	if !quoted {
		e = strings.TrimSpace(e)
		if strings.EqualFold(e, "null") {
			p.el = append(p.el, Null)
			return nil
		}
		if e == "" {
			return errf(codeInvalidText, "x")
		}
	}
	d := TextD(e)
	if p.ek != KText && p.ek != KNull {
		v, err := coerce(d, p.ek)
		if err != nil {
			return err
		}
		d = v
	}
	p.el = append(p.el, d)
	return nil
}

// fmtElem prints one element as it appears in an array literal.
func fmtElem(e Datum) string {
	if e.IsNull() {
		return "NULL"
	}
	s := e.String()
	if s == "" || strings.EqualFold(s, "null") || strings.ContainsAny(s, "{},\"\\ \t\n\r") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

func (a *arr) format() string {
	if len(a.el) == 0 {
		return "{}"
	}
	var b strings.Builder
	pos := 0
	var rec func(d int)
	rec = func(d int) {
		b.WriteByte('{')
		for i := 0; i < a.dims[d]; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			if d == len(a.dims)-1 {
				b.WriteString(fmtElem(a.el[pos]))
				pos++
			} else {
				rec(d + 1)
			}
		}
		b.WriteByte('}')
	}
	rec(0)
	return b.String()
}

func (a *arr) datum() Datum {
	return Datum{K: KArray, S: a.format(), I: int64(a.ek)}
}

func oneDim(el []Datum, ek Kind) *arr {
	if len(el) == 0 {
		return &arr{ek: ek}
	}
	return &arr{ek: ek, dims: []int{len(el)}, el: el}
}

func arrDatum(el []Datum, ek Kind) Datum { return oneDim(el, ek).datum() }

// arrOf reads d as an array: arrays, and text in array literal form whose
// elements take kind hint.
func arrOf(d Datum, hint Kind) (*arr, error) {
	switch d.K {
	case KArray:
		return parseArr(d)
	case KText:
		return parseArr(Datum{K: KArray, S: d.S, I: int64(hint)})
	}
	return nil, errf(codeUndefinedFunction, "function does not exist for argument type %s", d.K)
}

func looksArrayLit(d Datum) bool {
	if d.K != KText {
		return false
	}
	t := strings.TrimSpace(d.S)
	return len(t) >= 2 && t[0] == '{' && t[len(t)-1] == '}'
}

// unifyKind is the element kind of ARRAY[...] over the kinds of its
// members (NULLs ignored); text members yield to a typed member (an
// untyped literal).
func unifyKind(ks []Kind) (Kind, error) {
	var k Kind
	num := false
	for _, x := range ks {
		switch {
		case x == KNull:
		case k == KNull:
			k = x
		case k == x:
		case k == KText: // untyped literal so far
			k = x
		case x == KText:
		case (k == KInt || k == KFloat || k == KNumeric) && (x == KInt || x == KFloat || x == KNumeric):
			num = true
			if k == KInt || x == KFloat {
				k = x
			}
			if k == KNumeric && x == KFloat {
				k = KFloat
			}
		case isTime(k) && (isTime(x) || x == KDate), k == KDate && isTime(x):
			k = KTime
		default:
			return KNull, errf("42804", "ARRAY types %s and %s cannot be matched", pgKindName(k), pgKindName(x))
		}
	}
	_ = num
	if k == KNull {
		k = KText
	}
	return k, nil
}

// arrayCtor is ARRAY[a, b, ...]: scalars, or arrays of equal shape.
func arrayCtor(args []Datum) (Datum, error) {
	anyArr := false
	for _, a := range args {
		if a.K == KArray {
			anyArr = true
		}
	}
	if anyArr {
		var subs []*arr
		for _, a := range args {
			if a.K != KArray {
				return Null, errf("42804", "multidimensional arrays must have array expressions with matching dimensions")
			}
			s, err := parseArr(a)
			if err != nil {
				return Null, err
			}
			subs = append(subs, s)
		}
		out := &arr{ek: subs[0].ek}
		for _, s := range subs {
			if len(s.dims) != len(subs[0].dims) {
				return Null, errf("2202E", "multidimensional arrays must have array expressions with matching dimensions")
			}
			for i := range s.dims {
				if s.dims[i] != subs[0].dims[i] {
					return Null, errf("2202E", "multidimensional arrays must have array expressions with matching dimensions")
				}
			}
			out.el = append(out.el, s.el...)
		}
		if len(out.el) == 0 {
			return out.datum(), nil
		}
		out.dims = append([]int{len(subs)}, subs[0].dims...)
		return out.datum(), nil
	}
	ks := make([]Kind, len(args))
	for i, a := range args {
		ks[i] = a.K
	}
	ek, err := unifyKind(ks)
	if err != nil {
		return Null, err
	}
	el := make([]Datum, len(args))
	for i, a := range args {
		if a.IsNull() || a.K == ek {
			el[i] = a
			continue
		}
		v, err := coerce(a, ek)
		if err != nil {
			return Null, err
		}
		el[i] = v
	}
	return arrDatum(el, ek), nil
}

func (a *arr) ndims() int { return len(a.dims) }

// sub1 are the elements of a one-dimensional (or empty) array.
func (a *arr) oneD() error {
	if len(a.dims) > 1 {
		return errf("22000", "argument must be empty or one-dimensional array")
	}
	return nil
}

// cmpElem orders two elements, NULL last.
func cmpElem(x, y Datum) (int, error) {
	switch {
	case x.IsNull() && y.IsNull():
		return 0, nil
	case x.IsNull():
		return 1, nil
	case y.IsNull():
		return -1, nil
	}
	return compare(x, y)
}

// compareArrays orders two arrays as PostgreSQL's btree does: element by
// element (NULL last), then by length, then by shape.
func compareArrays(a, b Datum) (int, error) {
	x, err := arrOf(a, Kind(b.I))
	if err != nil {
		return 0, err
	}
	y, err := arrOf(b, Kind(a.I))
	if err != nil {
		return 0, err
	}
	for i := 0; i < len(x.el) && i < len(y.el); i++ {
		c, err := cmpElem(x.el[i], y.el[i])
		if err != nil || c != 0 {
			return c, err
		}
	}
	switch {
	case len(x.el) < len(y.el):
		return -1, nil
	case len(x.el) > len(y.el):
		return 1, nil
	}
	for i := 0; i < len(x.dims) && i < len(y.dims); i++ {
		if x.dims[i] != y.dims[i] {
			if x.dims[i] < y.dims[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return len(x.dims) - len(y.dims), nil
}

func containsElem(set []Datum, e Datum) bool {
	if e.IsNull() {
		return false
	}
	for _, s := range set {
		if s.IsNull() {
			continue
		}
		if c, err := compare(s, e); err == nil && c == 0 {
			return true
		}
	}
	return false
}

// arrayOp is @>, <@ and && on arrays.
func arrayOp(op string, l, r Datum) (Datum, error) {
	x, err := arrOf(l, Kind(r.I))
	if err != nil {
		return Null, err
	}
	y, err := arrOf(r, Kind(l.I))
	if err != nil {
		return Null, err
	}
	switch op {
	case "stask_contained":
		x, y = y, x
		fallthrough
	case "stask_contains":
		for _, e := range y.el {
			if !containsElem(x.el, e) {
				return BoolD(false), nil
			}
		}
		return BoolD(true), nil
	}
	for _, e := range y.el { // &&
		if containsElem(x.el, e) {
			return BoolD(true), nil
		}
	}
	return BoolD(false), nil
}

// arrayConcat is a || b for arrays and elements; ok is false when neither
// side is an array.
func arrayConcat(l, r Datum) (Datum, bool, error) {
	la, ra := l.K == KArray, r.K == KArray
	if !la && !ra {
		return Null, false, nil
	}
	if la && !ra && looksArrayLit(r) {
		ra = true
	}
	if ra && !la && looksArrayLit(l) {
		la = true
	}
	switch {
	case la && ra:
		if l.K == KArray && r.K == KArray && !compatKinds(Kind(l.I), Kind(r.I)) {
			return Null, true, errf(codeUndefinedFunction, "operator does not exist: %s[] || %s[]", pgKindName(Kind(l.I)), pgKindName(Kind(r.I)))
		}
		x, err := arrOf(l, Kind(r.I))
		if err != nil {
			return Null, true, err
		}
		y, err := arrOf(r, Kind(l.I))
		if err != nil {
			return Null, true, err
		}
		d, err := catArr(x, y)
		return d, true, err
	case la:
		x, err := arrOf(l, KText)
		if err != nil {
			return Null, true, err
		}
		d, err := appendElem(x, r, false)
		return d, true, err
	}
	y, err := arrOf(r, KText)
	if err != nil {
		return Null, true, err
	}
	d, err := appendElem(y, l, true)
	return d, true, err
}

func catArr(x, y *arr) (Datum, error) {
	if len(x.el) == 0 {
		return y.datum(), nil
	}
	if len(y.el) == 0 {
		return x.datum(), nil
	}
	ek := x.ek
	if ek == KText {
		ek = y.ek
	}
	out := &arr{ek: ek}
	switch {
	case len(x.dims) == len(y.dims):
		for i := 1; i < len(x.dims); i++ {
			if x.dims[i] != y.dims[i] {
				return Null, errf("2202E", "cannot concatenate incompatible arrays")
			}
		}
		out.dims = append([]int{x.dims[0] + y.dims[0]}, x.dims[1:]...)
		out.el = append(append([]Datum(nil), x.el...), y.el...)
	case len(x.dims) == len(y.dims)+1:
		for i := 1; i < len(x.dims); i++ {
			if x.dims[i] != y.dims[i-1] {
				return Null, errf("2202E", "cannot concatenate incompatible arrays")
			}
		}
		out.dims = append([]int{x.dims[0] + 1}, x.dims[1:]...)
		out.el = append(append([]Datum(nil), x.el...), y.el...)
	case len(y.dims) == len(x.dims)+1:
		for i := 1; i < len(y.dims); i++ {
			if y.dims[i] != x.dims[i-1] {
				return Null, errf("2202E", "cannot concatenate incompatible arrays")
			}
		}
		out.dims = append([]int{y.dims[0] + 1}, y.dims[1:]...)
		out.el = append(append([]Datum(nil), x.el...), y.el...)
	default:
		return Null, errf("2202E", "cannot concatenate incompatible arrays")
	}
	return out.datum(), nil
}

// appendElem adds e after (or, with front, before) a one-dimensional array.
func appendElem(a *arr, e Datum, front bool) (Datum, error) {
	if err := a.oneD(); err != nil {
		return Null, err
	}
	ek := a.ek
	if !e.IsNull() && e.K != ek {
		switch {
		case ek == KText && len(a.el) == 0:
			ek = e.K
		case ek == KText:
			e = TextD(e.String())
		default:
			v, err := coerce(e, ek)
			if err != nil {
				return Null, err
			}
			e = v
		}
	}
	var el []Datum
	if front {
		el = append([]Datum{e}, a.el...)
	} else {
		el = append(append([]Datum(nil), a.el...), e)
	}
	return arrDatum(el, ek), nil
}

func intArg(d Datum) (int, error) {
	v, err := coerce(d, KInt)
	if err != nil {
		return 0, err
	}
	return int(v.I), nil
}

func sliceBound(d Datum, def int) (int, bool, error) {
	if d.IsNull() {
		return def, false, nil
	}
	n, err := intArg(d)
	return n, true, err
}

// arraySubscript is a[i] and a[i][j]...: NULL out of range.
func arraySubscript(args []Datum) (Datum, bool, error) {
	if args[0].K != KArray {
		return Null, false, nil
	}
	a, err := parseArr(args[0])
	if err != nil {
		return Null, true, err
	}
	if len(args)-1 != len(a.dims) {
		return Null, true, nil
	}
	pos, stride := 0, 1
	for i := len(args) - 1; i >= 1; i-- {
		if args[i].IsNull() {
			return Null, true, nil
		}
		n, err := intArg(args[i])
		if err != nil {
			return Null, true, err
		}
		if n < 1 || n > a.dims[i-1] {
			return Null, true, nil
		}
		pos += (n - 1) * stride
		stride *= a.dims[i-1]
	}
	return a.el[pos], true, nil
}

// arraySlice is a[lo:hi] (a missing bound is NULL).
func arraySlice(args []Datum) (Datum, bool, error) {
	if args[0].K != KArray {
		return Null, false, nil
	}
	a, err := parseArr(args[0])
	if err != nil {
		return Null, true, err
	}
	if len(a.dims) == 0 {
		return arrDatum(nil, a.ek), true, nil
	}
	lo, _, err := sliceBound(args[1], 1)
	if err != nil {
		return Null, true, err
	}
	hi, _, err := sliceBound(args[2], a.dims[0])
	if err != nil {
		return Null, true, err
	}
	lo, hi = max(lo, 1), min(hi, a.dims[0])
	if lo > hi {
		return arrDatum(nil, a.ek), true, nil
	}
	rowLen := len(a.el) / a.dims[0]
	out := &arr{ek: a.ek, el: append([]Datum(nil), a.el[(lo-1)*rowLen:hi*rowLen]...)}
	out.dims = append([]int{hi - lo + 1}, a.dims[1:]...)
	return out.datum(), true, nil
}

func (a *arr) dimsText() string {
	var b strings.Builder
	for _, n := range a.dims {
		b.WriteString("[1:" + strconv.Itoa(n) + "]")
	}
	return b.String()
}

// stringToArray is string_to_array(s, delim [, null_string]).
func stringToArray(args []Datum) (Datum, error) {
	if args[0].IsNull() {
		return Null, nil
	}
	s := args[0].String()
	var nullStr *string
	if len(args) > 2 && !args[2].IsNull() {
		n := args[2].String()
		nullStr = &n
	}
	var parts []string
	switch {
	case s == "":
		return arrDatum(nil, KText), nil
	case args[1].IsNull():
		for _, r := range s {
			parts = append(parts, string(r))
		}
	case args[1].String() == "":
		parts = []string{s}
	default:
		parts = strings.Split(s, args[1].String())
	}
	el := make([]Datum, len(parts))
	for i, p := range parts {
		if nullStr != nil && p == *nullStr {
			el[i] = Null
		} else {
			el[i] = TextD(p)
		}
	}
	return arrDatum(el, KText), nil
}

func arrayToString(args []Datum) (Datum, error) {
	a, err := arrOf(args[0], KText)
	if err != nil {
		return Null, err
	}
	if args[1].IsNull() {
		return Null, nil
	}
	delim := args[1].String()
	nullStr, hasNull := "", len(args) > 2 && !args[2].IsNull()
	if hasNull {
		nullStr = args[2].String()
	}
	var parts []string
	for _, e := range a.el {
		switch {
		case !e.IsNull():
			parts = append(parts, e.String())
		case hasNull:
			parts = append(parts, nullStr)
		}
	}
	return TextD(strings.Join(parts, delim)), nil
}

// arrayCall runs the array functions; ok is false for other names.
func arrayCall(name string, args []Datum) (Datum, bool, error) {
	n := len(args)
	argErr := func() (Datum, bool, error) {
		return Null, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, n)
	}
	switch name {
	case "array":
		d, err := arrayCtor(args)
		return d, true, err
	case "stask_subscript":
		if n >= 2 {
			return arraySubscript(args)
		}
	case "stask_slice":
		if n == 3 {
			if args[0].IsNull() {
				return Null, true, nil
			}
			return arraySlice(args)
		}
	case "stask_anyarr":
		return anyArr(args)
	case "stask_contains", "stask_contained", "stask_overlap":
		if n != 2 {
			return argErr()
		}
		if args[0].IsNull() || args[1].IsNull() {
			return Null, true, nil
		}
		if args[0].K == KJSONB || args[1].K == KJSONB || args[0].K == KJSON || args[1].K == KJSON {
			if name == "stask_overlap" {
				return Null, true, errf(codeUndefinedFunction, "operator does not exist: jsonb && jsonb")
			}
			return Null, false, nil
		}
		d, err := arrayOp(name, args[0], args[1])
		return d, true, err
	case "array_append", "array_prepend", "array_cat":
		if n != 2 {
			return argErr()
		}
		switch name {
		case "array_append":
			if args[0].IsNull() {
				return arrDatum([]Datum{args[1]}, args[1].K), true, nil
			}
			a, err := arrOf(args[0], KText)
			if err != nil {
				return Null, true, err
			}
			d, err := appendElem(a, args[1], false)
			return d, true, err
		case "array_prepend":
			if args[1].IsNull() {
				return arrDatum([]Datum{args[0]}, args[0].K), true, nil
			}
			a, err := arrOf(args[1], KText)
			if err != nil {
				return Null, true, err
			}
			d, err := appendElem(a, args[0], true)
			return d, true, err
		}
		switch {
		case args[0].IsNull():
			return args[1], true, nil
		case args[1].IsNull():
			return args[0], true, nil
		}
		x, err := arrOf(args[0], Kind(args[1].I))
		if err != nil {
			return Null, true, err
		}
		y, err := arrOf(args[1], Kind(args[0].I))
		if err != nil {
			return Null, true, err
		}
		d, err := catArr(x, y)
		return d, true, err
	case "array_remove", "array_replace":
		want := 2
		if name == "array_replace" {
			want = 3
		}
		if n != want {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		a, err := arrOf(args[0], KText)
		if err != nil {
			return Null, true, err
		}
		if name == "array_remove" {
			if err := a.oneD(); err != nil {
				return Null, true, errf("0A000", "removing elements from multidimensional arrays is not supported")
			}
		}
		same := func(x Datum) bool {
			if x.IsNull() || args[1].IsNull() {
				return x.IsNull() && args[1].IsNull()
			}
			c, err := compare(x, args[1])
			return err == nil && c == 0
		}
		var el []Datum
		for _, e := range a.el {
			switch {
			case !same(e):
				el = append(el, e)
			case name == "array_replace":
				to := args[2]
				if !to.IsNull() && to.K != a.ek {
					if v, err := coerce(to, a.ek); err == nil {
						to = v
					}
				}
				el = append(el, to)
			}
		}
		out := &arr{ek: a.ek, el: el, dims: a.dims}
		if name == "array_remove" {
			out = oneDim(el, a.ek)
		}
		return out.datum(), true, nil
	case "array_position", "array_positions":
		if n < 2 || n > 3 || name == "array_positions" && n != 2 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		a, err := arrOf(args[0], KText)
		if err != nil {
			return Null, true, err
		}
		if err := a.oneD(); err != nil {
			return Null, true, errf("0A000", "searching for elements in multidimensional arrays is not supported")
		}
		start := 1
		if n == 3 && !args[2].IsNull() {
			if start, err = intArg(args[2]); err != nil {
				return Null, true, err
			}
		}
		var found []Datum
		for i, e := range a.el {
			if i+1 < start {
				continue
			}
			match := false
			if e.IsNull() || args[1].IsNull() {
				match = e.IsNull() && args[1].IsNull()
			} else if c, err := compare(e, args[1]); err == nil && c == 0 {
				match = true
			}
			if match {
				if name == "array_position" {
					return IntD(int64(i + 1)), true, nil
				}
				found = append(found, IntD(int64(i+1)))
			}
		}
		if name == "array_position" {
			return Null, true, nil
		}
		return arrDatum(found, KInt), true, nil
	case "array_length", "array_upper", "array_lower":
		if n != 2 || args[0].K != KArray {
			break
		}
		if args[0].IsNull() || args[1].IsNull() {
			return Null, true, nil
		}
		a, err := parseArr(args[0])
		if err != nil {
			return Null, true, err
		}
		dim, err := intArg(args[1])
		if err != nil {
			return Null, true, err
		}
		if dim < 1 || dim > len(a.dims) {
			return Null, true, nil
		}
		if name == "array_lower" {
			return IntD(1), true, nil
		}
		return IntD(int64(a.dims[dim-1])), true, nil
	case "cardinality":
		if n != 1 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		if args[0].K == KArray {
			a, err := parseArr(args[0])
			return IntD(int64(len(a.el))), true, err
		}
	case "array_ndims", "array_dims":
		if n != 1 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		a, err := arrOf(args[0], KText)
		if err != nil {
			return Null, true, err
		}
		if len(a.dims) == 0 {
			return Null, true, nil
		}
		if name == "array_ndims" {
			return IntD(int64(len(a.dims))), true, nil
		}
		return TextD(a.dimsText()), true, nil
	case "array_fill":
		if n < 2 || n > 3 {
			return argErr()
		}
		if args[1].IsNull() {
			return Null, true, errf("22004", "dimension array or low bound array cannot be null")
		}
		da, err := arrOf(args[1], KInt)
		if err != nil {
			return Null, true, err
		}
		total := 1
		var dims []int
		for _, e := range da.el {
			if e.IsNull() {
				return Null, true, errf("22004", "dimension values cannot be null")
			}
			v, _ := coerce(e, KInt)
			if v.I < 0 {
				return Null, true, errf("54000", "array size exceeds the maximum allowed (134217727)")
			}
			dims = append(dims, int(v.I))
			total *= int(v.I)
			if total > 50_000_000 {
				return Null, true, errf("54000", "array size exceeds the maximum allowed (%d)", 50_000_000)
			}
		}
		if total == 0 {
			return arrDatum(nil, args[0].K), true, nil
		}
		el := make([]Datum, total)
		for i := range el {
			el[i] = args[0]
		}
		ek := args[0].K
		if ek == KNull {
			ek = KText
		}
		return (&arr{ek: ek, dims: dims, el: el}).datum(), true, nil
	case "array_to_string":
		if n < 2 || n > 3 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		d, err := arrayToString(args)
		return d, true, err
	case "string_to_array":
		if n < 2 || n > 3 {
			return argErr()
		}
		d, err := stringToArray(args)
		return d, true, err
	}
	return Null, false, nil
}

// anyArr is x op ANY|ALL (array).
func anyArr(args []Datum) (Datum, bool, error) {
	if args[1].IsNull() {
		return Null, true, nil
	}
	a, err := arrOf(args[1], KText)
	if err != nil {
		return Null, true, err
	}
	op, all := args[2].String(), args[3].Bool()
	x := args[0]
	if x.IsNull() {
		if len(a.el) == 0 {
			return BoolD(all), true, nil
		}
		return Null, true, nil
	}
	res, sawNull := all, false
	for _, e := range a.el {
		if e.IsNull() {
			sawNull = true
			continue
		}
		cmp, err := compare(x, e)
		if err != nil {
			return Null, true, err
		}
		var ok bool
		switch op {
		case "=":
			ok = cmp == 0
		case "<>":
			ok = cmp != 0
		case "<":
			ok = cmp < 0
		case "<=":
			ok = cmp <= 0
		case ">":
			ok = cmp > 0
		default:
			ok = cmp >= 0
		}
		if all && !ok {
			return BoolD(false), true, nil
		}
		if !all && ok {
			return BoolD(true), true, nil
		}
	}
	if sawNull {
		return Null, true, nil
	}
	return BoolD(res), true, nil
}

// elemKindOfSpec is the element kind of a column type like "int[]".
func elemKindOfType(t string) (Kind, bool) {
	e, ok := schema.ElemType(t)
	if !ok {
		return KNull, false
	}
	et, err := schema.ParseType(e)
	if err != nil {
		return KNull, false
	}
	return kindOf(et), true
}
