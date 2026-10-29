package sql

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"stask/internal/pgnum"
	"stask/internal/schema"
)

// json and jsonb operators and functions (docs/05 section 26, branch
// jsonarr). Values stay KJSON (text as given) and KJSONB (normalized text);
// operators parse the text into a jn tree, work on it and print it again.
// jsonb results are normalized by schema.NormalizeJSONB, so key order and
// spacing equal PostgreSQL's; json results are slices of the input text.

type jn struct {
	t    byte // 'n' null, 'b' bool, '#' number, 's' string, 'a' array, 'o' object
	s    string
	raw  string
	kids []*jn
	keys []string
}

type jparse struct {
	s string
	i int
}

func (p *jparse) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *jparse) value() *jn {
	p.ws()
	start := p.i
	n := &jn{}
	switch c := p.s[p.i]; {
	case c == '{':
		n.t = 'o'
		p.i++
		p.ws()
		if p.s[p.i] == '}' {
			p.i++
			break
		}
		for {
			p.ws()
			k := p.str()
			p.ws()
			p.i++ // :
			v := p.value()
			n.keys = append(n.keys, k)
			n.kids = append(n.kids, v)
			p.ws()
			c := p.s[p.i]
			p.i++
			if c == '}' {
				break
			}
		}
	case c == '[':
		n.t = 'a'
		p.i++
		p.ws()
		if p.s[p.i] == ']' {
			p.i++
			break
		}
		for {
			n.kids = append(n.kids, p.value())
			p.ws()
			c := p.s[p.i]
			p.i++
			if c == ']' {
				break
			}
		}
	case c == '"':
		n.t = 's'
		n.s = p.str()
	case c == 't':
		n.t, n.s = 'b', "true"
		p.i += 4
	case c == 'f':
		n.t, n.s = 'b', "false"
		p.i += 5
	case c == 'n':
		n.t = 'n'
		p.i += 4
	default:
		n.t = '#'
		for p.i < len(p.s) && strings.IndexByte("+-0123456789.eE", p.s[p.i]) >= 0 {
			p.i++
		}
		n.s = p.s[start:p.i]
	}
	n.raw = p.s[start:p.i]
	return n
}

func (p *jparse) str() string {
	start := p.i
	p.i++
	for p.s[p.i] != '"' {
		if p.s[p.i] == '\\' {
			p.i++
		}
		p.i++
	}
	p.i++
	var out string
	if err := json.Unmarshal([]byte(p.s[start:p.i]), &out); err != nil {
		return p.s[start+1 : p.i-1]
	}
	return out
}

// parseJN reads JSON text (checked by schema.ValidJSON).
func parseJN(src string) (*jn, error) {
	if err := schema.ValidJSON(src); err != nil {
		return nil, errf(codeInvalidText, "%v", err)
	}
	p := &jparse{s: src}
	return p.value(), nil
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteString(string("0123456789abcdef"[c>>4]))
			b.WriteString(string("0123456789abcdef"[c&15]))
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// compact prints the tree as JSON without whitespace (objects keep the
// order and duplicates they have; normalizing is the caller's business).
func (n *jn) compact() string {
	var b strings.Builder
	n.write(&b)
	return b.String()
}

func (n *jn) write(b *strings.Builder) {
	switch n.t {
	case 'n':
		b.WriteString("null")
	case 'b', '#':
		b.WriteString(n.s)
	case 's':
		b.WriteString(jsonQuote(n.s))
	case 'a':
		b.WriteByte('[')
		for i, k := range n.kids {
			if i > 0 {
				b.WriteByte(',')
			}
			k.write(b)
		}
		b.WriteByte(']')
	case 'o':
		b.WriteByte('{')
		for i, k := range n.kids {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonQuote(n.keys[i]))
			b.WriteByte(':')
			k.write(b)
		}
		b.WriteByte('}')
	}
}

// jsonbOf makes a jsonb datum of a tree.
func jsonbOf(n *jn) (Datum, error) {
	s, err := schema.NormalizeJSONB(n.compact())
	if err != nil {
		return Null, errf(codeInvalidText, "%v", err)
	}
	return Datum{K: KJSONB, S: s}, nil
}

// outJSON makes the result of kind k (KJSON keeps the node's own text).
func outJSON(n *jn, k Kind) (Datum, error) {
	if k == KJSON {
		if n.raw != "" {
			return Datum{K: KJSON, S: n.raw}, nil
		}
		return Datum{K: KJSON, S: n.compact()}, nil
	}
	return jsonbOf(n)
}

// jsonIn reads a datum as a JSON tree and the kind of json value it was.
func jsonIn(d Datum) (*jn, Kind, error) {
	k := d.K
	if k != KJSON && k != KJSONB {
		if k != KText {
			return nil, k, errf(codeUndefinedFunction, "operator does not exist: %s", pgKindName(k))
		}
		k = KJSONB
	}
	n, err := parseJN(d.S)
	return n, k, err
}

func (n *jn) field(key string) *jn {
	if n.t != 'o' {
		return nil
	}
	for i := len(n.keys) - 1; i >= 0; i-- {
		if n.keys[i] == key {
			return n.kids[i]
		}
	}
	return nil
}

func (n *jn) index(i int) *jn {
	if n.t != 'a' {
		return nil
	}
	if i < 0 {
		i += len(n.kids)
	}
	if i < 0 || i >= len(n.kids) {
		return nil
	}
	return n.kids[i]
}

// text is the ->> form of a node: strings unquoted, null as NULL.
func (n *jn) text(k Kind) Datum {
	switch n.t {
	case 'n':
		return Null
	case 's':
		return TextD(n.s)
	case '#':
		if k == KJSONB {
			if d, err := pgnum.Parse(n.s); err == nil {
				return TextD(d.String())
			}
		}
	}
	if k == KJSON && n.raw != "" {
		return TextD(n.raw)
	}
	return TextD(n.compact())
}

func pathOf(d Datum) ([]string, error) {
	a, err := arrOf(d, KText)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(a.el))
	for i, e := range a.el {
		if e.IsNull() {
			return nil, errf("22004", "path element at position %d is null", i+1)
		}
		out[i] = e.String()
	}
	return out, nil
}

func (n *jn) at(path []string) *jn {
	cur := n
	for _, p := range path {
		switch cur.t {
		case 'o':
			cur = cur.field(p)
		case 'a':
			i, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return nil
			}
			cur = cur.index(i)
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

// contains is jsonb @>.
func jcontains(a, b *jn) bool {
	switch {
	case a.t == 'o' && b.t == 'o':
		for i, k := range b.keys {
			v := a.field(k)
			if v == nil || !jcontains(v, b.kids[i]) {
				return false
			}
		}
		return true
	case a.t == 'a' && b.t == 'a':
		for _, y := range b.kids {
			found := false
			for _, x := range a.kids {
				if y.t == 'a' || y.t == 'o' {
					if x.t == y.t && jcontains(x, y) {
						found = true
						break
					}
				} else if jequal(x, y) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	case a.t == 'a' && b.t != 'a' && b.t != 'o': // an array contains a primitive it holds
		for _, x := range a.kids {
			if jequal(x, b) {
				return true
			}
		}
		return false
	}
	return a.t == b.t && a.t != 'a' && a.t != 'o' && jequal(a, b)
}

func jequal(a, b *jn) bool {
	if a.t != b.t {
		return false
	}
	switch a.t {
	case 'n':
		return true
	case '#':
		x, e1 := pgnum.Parse(a.s)
		y, e2 := pgnum.Parse(b.s)
		return e1 == nil && e2 == nil && pgnum.Cmp(x, y) == 0
	case 's', 'b':
		return a.s == b.s
	}
	return a.compact() == b.compact()
}

func jexists(a *jn, key string) bool {
	switch a.t {
	case 'o':
		return a.field(key) != nil
	case 'a':
		for _, x := range a.kids {
			if x.t == 's' && x.s == key {
				return true
			}
		}
	case 's':
		return a.s == key
	}
	return false
}

// jconcat is jsonb || jsonb.
func jconcat(a, b *jn) *jn {
	if a.t == 'o' && b.t == 'o' {
		out := &jn{t: 'o', keys: append([]string(nil), a.keys...), kids: append([]*jn(nil), a.kids...)}
		for i, k := range b.keys {
			out.keys = append(out.keys, k)
			out.kids = append(out.kids, b.kids[i])
		}
		return out
	}
	list := func(x *jn) []*jn {
		if x.t == 'a' {
			return x.kids
		}
		return []*jn{x}
	}
	return &jn{t: 'a', kids: append(append([]*jn(nil), list(a)...), list(b)...)}
}

// jdelKey is jsonb - text.
func jdelKey(a *jn, key string) (*jn, error) {
	switch a.t {
	case 'o':
		out := &jn{t: 'o'}
		for i, k := range a.keys {
			if k != key {
				out.keys = append(out.keys, k)
				out.kids = append(out.kids, a.kids[i])
			}
		}
		return out, nil
	case 'a':
		out := &jn{t: 'a'}
		for _, x := range a.kids {
			if !(x.t == 's' && x.s == key) {
				out.kids = append(out.kids, x)
			}
		}
		return out, nil
	}
	return nil, errf("22023", "cannot delete from scalar")
}

func jdelIndex(a *jn, i int) (*jn, error) {
	if a.t != 'a' {
		if a.t == 'o' {
			return nil, errf("22023", "cannot delete from object using integer index")
		}
		return nil, errf("22023", "cannot delete from scalar")
	}
	if i < 0 {
		i += len(a.kids)
	}
	if i < 0 || i >= len(a.kids) {
		return a, nil
	}
	out := &jn{t: 'a', kids: append(append([]*jn(nil), a.kids[:i]...), a.kids[i+1:]...)}
	return out, nil
}

// jdelPath is jsonb #- text[].
func jdelPath(a *jn, path []string) (*jn, error) {
	if len(path) == 0 {
		return a, nil
	}
	if a.t != 'o' && a.t != 'a' {
		return a, nil
	}
	if len(path) == 1 {
		if a.t == 'o' {
			return jdelKey(a, path[0])
		}
		i, err := strconv.Atoi(path[0])
		if err != nil {
			return a, nil
		}
		return jdelIndex(a, i)
	}
	var child *jn
	idx := -1
	if a.t == 'o' {
		for i := len(a.keys) - 1; i >= 0; i-- {
			if a.keys[i] == path[0] {
				idx = i
				break
			}
		}
	} else {
		i, err := strconv.Atoi(path[0])
		if err != nil {
			return a, nil
		}
		if i < 0 {
			i += len(a.kids)
		}
		if i >= 0 && i < len(a.kids) {
			idx = i
		}
	}
	if idx < 0 {
		return a, nil
	}
	child = a.kids[idx]
	nc, err := jdelPath(child, path[1:])
	if err != nil {
		return nil, err
	}
	out := &jn{t: a.t, keys: a.keys, kids: append([]*jn(nil), a.kids...)}
	out.kids[idx] = nc
	return out, nil
}

// jset is jsonb_set (insert false) and jsonb_insert (insert true).
func jset(a *jn, path []string, nv *jn, create, insert, after bool) (*jn, error) {
	if len(path) == 0 {
		return a, nil
	}
	switch a.t {
	case 'o':
		idx := -1
		for i := len(a.keys) - 1; i >= 0; i-- {
			if a.keys[i] == path[0] {
				idx = i
				break
			}
		}
		if len(path) == 1 {
			out := &jn{t: 'o', keys: append([]string(nil), a.keys...), kids: append([]*jn(nil), a.kids...)}
			switch {
			case idx >= 0 && insert:
				return nil, errf("22023", "cannot replace existing key")
			case idx >= 0:
				out.kids[idx] = nv
			case create:
				out.keys = append(out.keys, path[0])
				out.kids = append(out.kids, nv)
			default:
				return a, nil
			}
			return out, nil
		}
		if idx < 0 {
			return a, nil
		}
		c, err := jset(a.kids[idx], path[1:], nv, create, insert, after)
		if err != nil {
			return nil, err
		}
		out := &jn{t: 'o', keys: a.keys, kids: append([]*jn(nil), a.kids...)}
		out.kids[idx] = c
		return out, nil
	case 'a':
		i, err := strconv.Atoi(strings.TrimSpace(path[0]))
		if err != nil {
			return nil, errf("22P02", "path element at position 1 is not an integer: %q", path[0])
		}
		n := len(a.kids)
		if i < 0 {
			i += n
		}
		if len(path) == 1 {
			out := &jn{t: 'a', kids: append([]*jn(nil), a.kids...)}
			switch {
			case i >= 0 && i < n && insert:
				at := i
				if after {
					at++
				}
				out.kids = append(append(append([]*jn(nil), a.kids[:at]...), nv), a.kids[at:]...)
			case i >= 0 && i < n:
				out.kids[i] = nv
			case !create:
				return a, nil
			case i < 0:
				out.kids = append([]*jn{nv}, a.kids...)
			default:
				out.kids = append(out.kids, nv)
			}
			return out, nil
		}
		if i < 0 || i >= n {
			return a, nil
		}
		c, err := jset(a.kids[i], path[1:], nv, create, insert, after)
		if err != nil {
			return nil, err
		}
		out := &jn{t: 'a', kids: append([]*jn(nil), a.kids...)}
		out.kids[i] = c
		return out, nil
	}
	return a, nil
}

func jstrip(n *jn) *jn {
	switch n.t {
	case 'o':
		out := &jn{t: 'o'}
		for i, k := range n.kids {
			if k.t == 'n' {
				continue
			}
			out.keys = append(out.keys, n.keys[i])
			out.kids = append(out.kids, jstrip(k))
		}
		return out
	case 'a':
		out := &jn{t: 'a'}
		for _, k := range n.kids {
			out.kids = append(out.kids, jstrip(k))
		}
		return out
	}
	return n
}

func jpretty(n *jn, b *strings.Builder, ind int) {
	pad := func(k int) { b.WriteString(strings.Repeat("    ", k)) }
	switch n.t {
	case 'a':
		b.WriteString("[\n")
		for i, k := range n.kids {
			if i > 0 {
				b.WriteString(",\n")
			}
			pad(ind + 1)
			jpretty(k, b, ind+1)
		}
		b.WriteString("\n")
		pad(ind)
		b.WriteString("]")
	case 'o':
		b.WriteString("{\n")
		ord := jsortedIdx(n)
		for i, j := range ord {
			if i > 0 {
				b.WriteString(",\n")
			}
			pad(ind + 1)
			b.WriteString(jsonQuote(n.keys[j]) + ": ")
			jpretty(n.kids[j], b, ind+1)
		}
		b.WriteString("\n")
		pad(ind)
		b.WriteString("}")
	default:
		n.write(b)
	}
}

// jsortedIdx are the member indexes of an object in jsonb order (length,
// then bytes; the last of equal keys).
func jsortedIdx(n *jn) []int {
	last := map[string]int{}
	for i, k := range n.keys {
		last[k] = i
	}
	idx := make([]int, 0, len(last))
	for _, i := range last {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, c int) bool {
		x, y := n.keys[idx[a]], n.keys[idx[c]]
		if len(x) != len(y) {
			return len(x) < len(y)
		}
		return bytes.Compare([]byte(x), []byte(y)) < 0
	})
	return idx
}

func jtype(n *jn) string {
	switch n.t {
	case 'o':
		return "object"
	case 'a':
		return "array"
	case 's':
		return "string"
	case '#':
		return "number"
	case 'b':
		return "boolean"
	}
	return "null"
}

// toJSONNode converts a SQL value as to_json does.
func toJSONNode(d Datum) *jn {
	switch d.K {
	case KNull:
		return &jn{t: 'n'}
	case KInt:
		return &jn{t: '#', s: strconv.FormatInt(d.I, 10)}
	case KFloat:
		s := d.String()
		if s == "NaN" || s == "Infinity" || s == "-Infinity" {
			return &jn{t: 's', s: s}
		}
		return &jn{t: '#', s: s}
	case KNumeric:
		if s := d.S; s == "NaN" || s == "Infinity" || s == "-Infinity" {
			return &jn{t: 's', s: s}
		}
		return &jn{t: '#', s: d.S}
	case KBool:
		return &jn{t: 'b', s: map[bool]string{true: "true", false: "false"}[d.I != 0]}
	case KTime:
		return &jn{t: 's', s: goTime(d.I).Format("2006-01-02T15:04:05.999999") + goTime(d.I).Format("-07:00")}
	case KTimestamp:
		return &jn{t: 's', s: goTime(d.I).Format("2006-01-02T15:04:05.999999")}
	case KJSON, KJSONB:
		if n, err := parseJN(d.S); err == nil {
			return n
		}
	case KArray:
		if a, err := parseArr(d); err == nil {
			return arrNode(a)
		}
	}
	return &jn{t: 's', s: d.String()}
}

func arrNode(a *arr) *jn {
	if len(a.el) == 0 {
		return &jn{t: 'a'}
	}
	pos := 0
	var rec func(d int) *jn
	rec = func(d int) *jn {
		n := &jn{t: 'a'}
		for i := 0; i < a.dims[d]; i++ {
			if d == len(a.dims)-1 {
				n.kids = append(n.kids, elemNode(a.el[pos]))
				pos++
			} else {
				n.kids = append(n.kids, rec(d+1))
			}
		}
		return n
	}
	return rec(0)
}

func elemNode(d Datum) *jn { return toJSONNode(d) }

// jsonOpCall implements the json operators and functions; ok is false for
// other names.
func jsonOpCall(name string, args []Datum) (Datum, bool, error) {
	n := len(args)
	argErr := func() (Datum, bool, error) {
		return Null, true, errf(codeUndefinedFunction, "function %s with %d arguments does not exist", name, n)
	}
	isJ := func(d Datum) bool { return d.K == KJSON || d.K == KJSONB }
	nullAny := func() bool {
		for _, a := range args {
			if a.IsNull() {
				return true
			}
		}
		return false
	}
	switch name {
	case "stask_jarrow", "stask_jarrow2":
		if n != 2 {
			return argErr()
		}
		if nullAny() {
			return Null, true, nil
		}
		j, k, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		var r *jn
		if args[1].K == KInt {
			r = j.index(int(args[1].I))
		} else {
			r = j.field(args[1].String())
		}
		if r == nil {
			return Null, true, nil
		}
		if name == "stask_jarrow2" {
			return r.text(k), true, nil
		}
		d, err := outJSON(r, k)
		return d, true, err
	case "stask_jhash", "stask_jhash2", "json_extract_path", "jsonb_extract_path", "json_extract_path_text", "jsonb_extract_path_text":
		if n < 2 {
			return argErr()
		}
		if nullAny() {
			return Null, true, nil
		}
		j, k, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		var path []string
		if name == "stask_jhash" || name == "stask_jhash2" {
			if path, err = pathOf(args[1]); err != nil {
				return Null, true, err
			}
		} else {
			for _, a := range args[1:] {
				path = append(path, a.String())
			}
		}
		r := j.at(path)
		if r == nil {
			return Null, true, nil
		}
		if name == "stask_jhash2" || strings.HasSuffix(name, "_text") {
			return r.text(k), true, nil
		}
		d, err := outJSON(r, k)
		return d, true, err
	case "stask_contains", "stask_contained":
		if n != 2 || !(isJ(args[0]) || isJ(args[1])) {
			return Null, false, nil
		}
		if nullAny() {
			return Null, true, nil
		}
		a, _, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		b, _, err := jsonIn(args[1])
		if err != nil {
			return Null, true, err
		}
		if name == "stask_contained" {
			a, b = b, a
		}
		return BoolD(jcontains(a, b)), true, nil
	case "stask_jexists", "stask_jexists_any", "stask_jexists_all":
		if n != 2 {
			return argErr()
		}
		if nullAny() {
			return Null, true, nil
		}
		a, _, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		if name == "stask_jexists" {
			return BoolD(jexists(a, args[1].String())), true, nil
		}
		keys, err := arrOf(args[1], KText)
		if err != nil {
			return Null, true, err
		}
		all := name == "stask_jexists_all"
		res := all
		for _, e := range keys.el {
			if e.IsNull() {
				continue
			}
			h := jexists(a, e.String())
			if all && !h {
				return BoolD(false), true, nil
			}
			if !all && h {
				return BoolD(true), true, nil
			}
		}
		return BoolD(res), true, nil
	case "stask_jhash_minus":
		if n != 2 {
			return argErr()
		}
		if nullAny() {
			return Null, true, nil
		}
		a, _, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		path, err := pathOf(args[1])
		if err != nil {
			return Null, true, err
		}
		r, err := jdelPath(a, path)
		if err != nil {
			return Null, true, err
		}
		d, err := jsonbOf(r)
		return d, true, err
	case "to_json", "to_jsonb":
		if n != 1 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		k := KJSON
		if name == "to_jsonb" {
			k = KJSONB
		}
		d, err := outJSONFresh(toJSONNode(args[0]), k)
		return d, true, err
	case "json_build_object", "jsonb_build_object", "json_build_array", "jsonb_build_array":
		return jsonBuild(name, args)
	case "jsonb_set", "jsonb_insert", "jsonb_set_lax":
		if name == "jsonb_set" && (n < 3 || n > 4) || name == "jsonb_insert" && (n < 3 || n > 4) || name == "jsonb_set_lax" && (n < 3 || n > 5) {
			return argErr()
		}
		if args[0].IsNull() || args[1].IsNull() {
			return Null, true, nil
		}
		if args[2].IsNull() && name != "jsonb_set_lax" {
			return Null, true, nil
		}
		a, _, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		path, err := pathOf(args[1])
		if err != nil {
			return Null, true, err
		}
		flag := name != "jsonb_insert"
		if n >= 4 && !args[3].IsNull() {
			flag = args[3].Bool()
		} else if n >= 4 {
			return Null, true, nil
		}
		var nv *jn
		if args[2].IsNull() { // jsonb_set_lax: null_value_treatment
			mode := "use_json_null"
			if n == 5 {
				mode = args[4].String()
			}
			switch mode {
			case "raise_exception":
				return Null, true, errf("22004", "JSON value must not be null")
			case "return_target":
				return args[0], true, nil
			case "delete_key":
				r, err := jdelPath(a, path)
				if err != nil {
					return Null, true, err
				}
				d, err := jsonbOf(r)
				return d, true, err
			}
			nv = &jn{t: 'n'}
		} else if nv, _, err = jsonIn(args[2]); err != nil {
			return Null, true, err
		}
		var r *jn
		if name == "jsonb_insert" {
			r, err = jset(a, path, nv, true, true, flag)
		} else {
			r, err = jset(a, path, nv, flag, false, false)
		}
		if err != nil {
			return Null, true, err
		}
		d, err := jsonbOf(r)
		return d, true, err
	case "jsonb_typeof", "json_typeof":
		if n != 1 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		a, _, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		return TextD(jtype(a)), true, nil
	case "jsonb_array_length", "json_array_length":
		if n != 1 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		a, _, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		switch a.t {
		case 'a':
			return IntD(int64(len(a.kids))), true, nil
		case 'o':
			return Null, true, errf("22023", "cannot get array length of a non-array")
		}
		return Null, true, errf("22023", "cannot get array length of a scalar")
	case "jsonb_strip_nulls", "json_strip_nulls":
		if n != 1 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		a, k, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		r := jstrip(a)
		if k == KJSON {
			return Datum{K: KJSON, S: r.compact()}, true, nil
		}
		d, err := jsonbOf(r)
		return d, true, err
	case "jsonb_pretty":
		if n != 1 {
			return argErr()
		}
		if args[0].IsNull() {
			return Null, true, nil
		}
		a, _, err := jsonIn(args[0])
		if err != nil {
			return Null, true, err
		}
		var b strings.Builder
		jpretty(a, &b, 0)
		return TextD(b.String()), true, nil
	}
	return Null, false, nil
}

// outJSONFresh prints a constructed tree: json as PostgreSQL's to_json
// does (no whitespace), jsonb normalized.
func outJSONFresh(n *jn, k Kind) (Datum, error) {
	if k == KJSON {
		return Datum{K: KJSON, S: n.compactRaw()}, nil
	}
	return jsonbOf(n)
}

// compactRaw is compact, but nodes that came from json input keep their
// own text, as PostgreSQL embeds json values verbatim.
func (n *jn) compactRaw() string {
	if n.raw != "" && (n.t == 'o' || n.t == 'a') {
		return n.raw
	}
	switch n.t {
	case 'a':
		parts := make([]string, len(n.kids))
		for i, k := range n.kids {
			parts[i] = k.compactRaw()
		}
		return "[" + strings.Join(parts, ",") + "]"
	case 'o':
		parts := make([]string, len(n.kids))
		for i, k := range n.kids {
			parts[i] = jsonQuote(n.keys[i]) + ":" + k.compactRaw()
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return n.compact()
}

func jsonBuild(name string, args []Datum) (Datum, bool, error) {
	k := KJSONB
	if strings.HasPrefix(name, "json_") {
		k = KJSON
	}
	obj := strings.HasSuffix(name, "object")
	if obj && len(args)%2 != 0 {
		return Null, true, errf("22023", "argument list must have even number of elements")
	}
	if k == KJSONB {
		n := &jn{t: 'a'}
		if obj {
			n.t = 'o'
		}
		for i := 0; i < len(args); i++ {
			if obj {
				if args[i].IsNull() {
					return Null, true, errf("22004", "argument %d cannot be null", i+1)
				}
				n.keys = append(n.keys, args[i].String())
				i++
			}
			n.kids = append(n.kids, toJSONNode(args[i]))
		}
		d, err := jsonbOf(n)
		return d, true, err
	}
	var b strings.Builder
	piece := func(a Datum) string {
		if a.K == KJSON {
			return a.S
		}
		return toJSONNode(a).compactRaw()
	}
	if obj {
		b.WriteString("{")
		for i := 0; i < len(args); i += 2 {
			if args[i].IsNull() {
				return Null, true, errf("22004", "argument %d cannot be null", i+1)
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(jsonQuote(args[i].String()) + " : " + piece(args[i+1]))
		}
		b.WriteString("}")
	} else {
		b.WriteString("[")
		for i, a := range args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(piece(a))
		}
		b.WriteString("]")
	}
	return Datum{K: KJSON, S: b.String()}, true, nil
}

// jsonArith is jsonb - text|int|text[].
func jsonArith(l, r Datum) (Datum, bool, error) {
	if l.K != KJSONB && l.K != KJSON {
		return Null, false, nil
	}
	a, _, err := jsonIn(l)
	if err != nil {
		return Null, true, err
	}
	var res *jn
	switch {
	case r.K == KInt:
		res, err = jdelIndex(a, int(r.I))
	case r.K == KArray:
		keys, perr := arrOf(r, KText)
		if perr != nil {
			return Null, true, perr
		}
		res = a
		for _, e := range keys.el {
			if !e.IsNull() {
				if res, err = jdelKey(res, e.String()); err != nil {
					break
				}
			}
		}
	default:
		if l.K == KJSON {
			return Null, true, errf(codeUndefinedFunction, "operator does not exist: json - unknown")
		}
		res, err = jdelKey(a, r.String())
	}
	if err != nil {
		return Null, true, err
	}
	d, err := jsonbOf(res)
	return d, true, err
}

// jsonConcat is jsonb || jsonb (text operands take the jsonb side's type).
func jsonConcat(l, r Datum) (Datum, bool, error) {
	if l.K != KJSONB && r.K != KJSONB {
		return Null, false, nil
	}
	a, _, err := jsonIn(l)
	if err != nil {
		return Null, true, err
	}
	b, _, err := jsonIn(r)
	if err != nil {
		return Null, true, err
	}
	d, err := jsonbOf(jconcat(a, b))
	return d, true, err
}
