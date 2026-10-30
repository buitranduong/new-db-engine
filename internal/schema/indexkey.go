package schema

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
)

// Secondary index definitions and the order-preserving key encoding.
//
// An index entry is keyed by Enc(col1) || Enc(col2) ... || primary key, so
// entries with equal indexed values are ordered by primary key and every
// entry is unique, as in PostgreSQL's btree (which adds the heap TID as a
// tie-breaker). The encoding of one value is prefix-free (self-delimiting),
// which makes the concatenation decodable and lets a bound on the first k
// columns be a plain byte prefix. Comparing two encodings bytewise gives the
// order of the values, with NULL after every value (PostgreSQL's default
// for ASC; DESC scans read the index backwards, giving NULLS FIRST).
//
// This file is the only place that knows how a column type is encoded. A
// new column type must add a case to AppendKeyValue and KeyValueLen (and
// IndexableType), and nothing else changes.

// IndexDef is a secondary index.
type IndexDef struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	// Constraint: the index backs a UNIQUE constraint of this name
	// (usually the same as Name); empty for CREATE UNIQUE INDEX.
	Constraint string `json:"constraint,omitempty"`
	// Internal: created for a foreign key (the referencing columns), so
	// that deleting a parent finds its children; it goes with the table.
	Internal bool `json:"internal,omitempty"`
}

// ForeignKey is a REFERENCES constraint of the table that holds it.
type ForeignKey struct {
	Name     string   `json:"name"`
	Columns  []string `json:"columns"`
	RefTable string   `json:"ref_table"`
	RefCols  []string `json:"ref_columns"`
	// OnDelete is "no action", "restrict", "cascade" or "set null".
	OnDelete string `json:"on_delete,omitempty"`
}

// IndexableType reports whether columns of type ct can be indexed.
func IndexableType(ct ColType) bool {
	switch ct {
	case TInt, TFloat, TString, TText, TTime, TTimestamp, TTimestamptz, TBool:
		return true
	}
	return false
}

func (t *Table) initIndexes() error {
	seen := map[string]bool{}
	t.ixCols = make([][]int, len(t.Indexes))
	for n, ix := range t.Indexes {
		if ix.Name == "" || len(ix.Columns) == 0 {
			return fmt.Errorf("index needs a name and columns")
		}
		if seen[ix.Name] {
			return fmt.Errorf("duplicate index %q", ix.Name)
		}
		seen[ix.Name] = true
		for _, c := range ix.Columns {
			i, ok := t.idx[c]
			if !ok {
				return fmt.Errorf("index %q: column %q not found", ix.Name, c)
			}
			if !IndexableType(t.Columns[i].T) {
				return fmt.Errorf("index %q: column %q has a type that cannot be indexed", ix.Name, c)
			}
			t.ixCols[n] = append(t.ixCols[n], i)
		}
	}
	for _, fk := range t.ForeignKeys {
		if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefCols) {
			return fmt.Errorf("foreign key %q: column lists differ", fk.Name)
		}
		for _, c := range fk.Columns {
			if _, ok := t.idx[c]; !ok {
				return fmt.Errorf("foreign key %q: column %q not found", fk.Name, c)
			}
		}
	}
	return nil
}

// IndexColsAt returns the column positions of t.Indexes[i] in t's rows.
func (t *Table) IndexColsAt(i int) []int { return t.ixCols[i] }

// Index returns the index called name, or nil.
func (t *Table) Index(name string) *IndexDef {
	for i := range t.Indexes {
		if t.Indexes[i].Name == name {
			return &t.Indexes[i]
		}
	}
	return nil
}

// CloneConstraints makes the table's index and foreign key lists its own,
// so that they can be edited (ALTER TABLE works on a copy).
func (t *Table) CloneConstraints() {
	t.Indexes = slices.Clone(t.Indexes)
	t.ForeignKeys = slices.Clone(t.ForeignKeys)
}

const (
	keyNull    = 0xff // NULL: after every value
	keyPresent = 0x01
)

// AppendKeyValue appends the order-preserving encoding of v, a value of
// column type ct, to dst.
func AppendKeyValue(dst []byte, ct ColType, v Value) []byte {
	if v.Null {
		return append(dst, keyNull)
	}
	dst = append(dst, keyPresent)
	switch ct {
	case TInt, TTime, TTimestamp, TTimestamptz, TBool:
		return binary.BigEndian.AppendUint64(dst, uint64(v.I)^1<<63)
	case TFloat:
		f := v.F
		if f != f { // NaN: above +Inf, as in PostgreSQL
			return binary.BigEndian.AppendUint64(dst, math.MaxUint64)
		}
		if f == 0 {
			f = 0 // -0 = 0
		}
		u := math.Float64bits(f)
		if u>>63 != 0 {
			u = ^u
		} else {
			u |= 1 << 63
		}
		if u == math.MaxUint64 { // a NaN payload would collide with NaN
			u--
		}
		return binary.BigEndian.AppendUint64(dst, u)
	case TString, TText:
		for i := 0; i < len(v.S); i++ {
			c := v.S[i]
			if c == 0 {
				dst = append(dst, 0, 0xff)
			} else {
				dst = append(dst, c)
			}
		}
		return append(dst, 0, 1)
	}
	panic(fmt.Sprintf("schema: column type %d cannot be indexed", ct))
}

// KeyValueLen returns the length of the encoded value at the start of b
// for a column of type ct, or -1 if b is malformed.
func KeyValueLen(b []byte, ct ColType) int {
	if len(b) == 0 {
		return -1
	}
	if b[0] == keyNull {
		return 1
	}
	switch ct {
	case TInt, TTime, TTimestamp, TTimestamptz, TBool, TFloat:
		if len(b) < 9 {
			return -1
		}
		return 9
	case TString, TText:
		for i := 1; i < len(b); i++ {
			if b[i] == 0 {
				if i+1 < len(b) && b[i+1] == 1 {
					return i + 2
				}
				i++ // 0 0xff: an escaped zero
			}
		}
	}
	return -1
}

// IndexKey appends the encoded indexed columns of row r to dst. hasNull
// reports a NULL in any column (such entries never conflict in a unique
// index).
func (t *Table) IndexKey(dst []byte, cols []int, r Row) (out []byte, hasNull bool) {
	for _, c := range cols {
		if r[c].Null {
			hasNull = true
		}
		dst = AppendKeyValue(dst, t.Columns[c].T, r[c])
	}
	return dst, hasNull
}

// IndexKeyLen is the length of the encoded indexed columns at the start of
// an entry key (what precedes the primary key), or -1.
func (t *Table) IndexKeyLen(cols []int, b []byte) int {
	n := 0
	for _, c := range cols {
		l := KeyValueLen(b[n:], t.Columns[c].T)
		if l < 0 {
			return -1
		}
		n += l
	}
	return n
}
