package schema

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Expression indexes: an index element may be an immutable single-column
// function of a column, lower(email), instead of the column itself
// (CREATE INDEX ON t (lower(email))). IndexDef.Exprs holds, parallel to
// Columns, the canonical function name of each element ("" for a plain
// column). Only the function name is stored, never expression text, so the
// definition cannot drift from the evaluator; the column names are renamed
// with the table's columns as before. The functions are a closed set,
// evaluated here with the same semantics as the SQL layer's (eval.go):
// the index key of a row is computed from the row alone and is immutable.

type exprFunc struct {
	// out is the result type for an argument of type in, ok=false if the
	// function does not accept in.
	out func(in ColType) (ColType, bool)
	fn  func(v Value) Value // v is not NULL
}

func textIn(in ColType) bool { return in == TString || in == TText }

var exprFuncs = map[string]exprFunc{
	"lower": textFn(func(s string) string { return strings.ToLower(s) }),
	"upper": textFn(func(s string) string { return strings.ToUpper(s) }),
	"btrim": textFn(func(s string) string { return strings.TrimSpace(s) }),
	"ltrim": textFn(func(s string) string { return strings.TrimLeft(s, " \t\n") }),
	"rtrim": textFn(func(s string) string { return strings.TrimRight(s, " \t\n") }),
	"length": {
		out: func(in ColType) (ColType, bool) { return TInt, textIn(in) },
		fn:  func(v Value) Value { return Value{I: int64(utf8.RuneCountInString(v.S))} },
	},
	"abs": {
		out: func(in ColType) (ColType, bool) { return in, in == TInt || in == TFloat },
		fn: func(v Value) Value {
			if v.F < 0 {
				v.F = -v.F
			}
			if v.I < 0 {
				v.I = -v.I
			}
			return v
		},
	},
}

func textFn(f func(string) string) exprFunc {
	return exprFunc{
		out: func(in ColType) (ColType, bool) { return TText, textIn(in) },
		fn:  func(v Value) Value { return Value{S: f(v.S)} },
	}
}

// CanonExprFunc maps a function name as written to the name stored in
// IndexDef.Exprs, ok=false if it cannot be indexed.
func CanonExprFunc(name string) (string, bool) {
	switch name = strings.ToLower(name); name {
	case "trim":
		name = "btrim"
	case "char_length":
		name = "length"
	}
	_, ok := exprFuncs[name]
	return name, ok
}

// ExprResultType is the type of fn applied to a column of type in.
func ExprResultType(fn string, in ColType) (ColType, bool) {
	f, ok := exprFuncs[fn]
	if !ok {
		return 0, false
	}
	return f.out(in)
}

// ApplyExpr evaluates the index function fn ("": the value itself); NULL gives NULL.
func ApplyExpr(fn string, v Value) Value {
	if v.Null || fn == "" {
		return v
	}
	return exprFuncs[fn].fn(v)
}

// ElemExpr is the function of element k ("" for a plain column).
func (ix IndexDef) ElemExpr(k int) string {
	if k < len(ix.Exprs) {
		return ix.Exprs[k]
	}
	return ""
}

// HasExpr reports whether any element is an expression.
func (ix IndexDef) HasExpr() bool {
	for _, e := range ix.Exprs {
		if e != "" {
			return true
		}
	}
	return false
}

// ElemKey names element k as the planner matches it: "email" or
// "lower(email)".
func (ix IndexDef) ElemKey(k int) string {
	if e := ix.ElemExpr(k); e != "" {
		return e + "(" + ix.Columns[k] + ")"
	}
	return ix.Columns[k]
}

// ElemKeys is ElemKey of every element.
func (ix IndexDef) ElemKeys() []string {
	out := make([]string, len(ix.Columns))
	for k := range out {
		out[k] = ix.ElemKey(k)
	}
	return out
}

// ElemType is the type of the values element k indexes.
func (t *Table) ElemType(ix IndexDef, k int) (ColType, bool) {
	i, ok := t.idx[ix.Columns[k]]
	if !ok {
		return 0, false
	}
	if e := ix.ElemExpr(k); e != "" {
		return ExprResultType(e, t.Columns[i].T)
	}
	return t.Columns[i].T, true
}

// ElemValue is the indexed value of element k in row r.
func (t *Table) ElemValue(ix IndexDef, k int, r Row) Value {
	i := t.idx[ix.Columns[k]]
	return ApplyExpr(ix.ElemExpr(k), r[i])
}

// Indexer computes the entry keys of one index.
type Indexer struct {
	cols  []int
	fns   []string
	types []ColType
	pred  []predTerm // partial index: rows outside it get a NULL-like key
}

// NewIndexer resolves def against t.
func (t *Table) NewIndexer(def IndexDef) (*Indexer, error) {
	x := &Indexer{}
	var err error
	if x.pred, err = t.compilePred(def); err != nil {
		return nil, err
	}
	for k, c := range def.Columns {
		i, ok := t.idx[c]
		if !ok {
			return nil, fmt.Errorf("index %q: column %q not found", def.Name, c)
		}
		if !IndexableType(t.Columns[i].T) {
			return nil, fmt.Errorf("index %q: column %q has a type that cannot be indexed", def.Name, c)
		}
		ct, ok := t.ElemType(def, k)
		if !ok {
			return nil, fmt.Errorf("index %q: function %s cannot be applied to column %q", def.Name, def.ElemExpr(k), c)
		}
		x.cols = append(x.cols, i)
		x.fns = append(x.fns, def.ElemExpr(k))
		x.types = append(x.types, ct)
	}
	return x, nil
}

// Key appends the encoded index elements of row r to dst; hasNull reports a
// NULL element (such entries never conflict in a unique index).
func (x *Indexer) Key(dst []byte, r Row) (out []byte, hasNull bool) {
	for k, c := range x.cols {
		v := r[c]
		if x.fns[k] != "" {
			v = ApplyExpr(x.fns[k], v)
		}
		if v.Null {
			hasNull = true
		}
		dst = AppendKeyValue(dst, x.types[k], v)
	}
	if x.pred != nil && !matchPred(x.pred, r) {
		hasNull = true
	}
	return dst, hasNull
}

// KeyLen is the length of the encoded elements at the start of an entry key.
func (x *Indexer) KeyLen(b []byte) int {
	n := 0
	for _, ct := range x.types {
		l := KeyValueLen(b[n:], ct)
		if l < 0 {
			return -1
		}
		n += l
	}
	return n
}

// IndexKeyAt is IndexKey for t.Indexes[i], expressions included.
func (t *Table) IndexKeyAt(dst []byte, i int, r Row) ([]byte, bool) { return t.ixr[i].Key(dst, r) }

// IndexKeyLenAt is IndexKeyLen for t.Indexes[i].
func (t *Table) IndexKeyLenAt(i int, b []byte) int { return t.ixr[i].KeyLen(b) }
