package proj

// Walkers for the reconciliation tool (internal/reconcile, docs/33): visit
// the live rows of a derived view in bounded chunks and give, per row, a
// hash of its key and a hash of the columns the view holds. The core side
// computes the same hashes from its rows (RecRowHash), so equal sums over
// equal key sets prove the view equals the core for the columns it keeps.
//
// A walk holds the view's table lock only for one chunk. If the view is
// renumbered between two chunks (compaction, ALTER) the walk returns
// ErrRecRestart and the caller starts again.

import (
	"errors"
	"hash/maphash"
	"math"
	"strconv"

	"stask/internal/schema"
)

// ErrRecRestart: the view changed shape under a walk; start it again.
var ErrRecRestart = errors.New("view changed under the walk")

// ErrRecUnavailable: the view has no such table (yet).
var ErrRecUnavailable = errors.New("view does not hold the table")

// RecCol is a column a view holds in a form both sides can hash. Kind is
// 'i' (integer, boolean, time: Value.I), 'f' (float), 's' (string)
// or 'a' (all three fields).
type RecCol struct {
	Num  int
	Name string
	Kind byte
}

var recSeed = maphash.MakeSeed()

// RecKeyHash is the hash of a primary key used for buckets and sampling.
func RecKeyHash(key string) uint64 { return maphash.String(recSeed, key) }

func recMix(h uint64, num int, kind byte, null bool, i int64, f float64, s string) uint64 {
	var x uint64
	switch {
	case null:
		x = 0x6e756c6c6e756c6c
	case kind == 'i':
		x = uint64(i)*2 + 1
	case kind == 'f':
		x = math.Float64bits(f)
	default:
		x = maphash.String(recSeed, s)
	}
	return mix64(h+uint64(num)*0x9E3779B97F4A7C15, x^uint64(kind))
}

// RecRowHash hashes the columns cols of a core row (idx[k] is the position
// of cols[k] in r).
func RecRowHash(cols []RecCol, idx []int, r schema.Row) uint64 {
	h := uint64(len(cols))
	for k, c := range cols {
		v := r[idx[k]]
		h = recMix(h, c.Num, c.Kind, v.Null, v.I, v.F, v.S)
	}
	return h
}

// RecColIdx maps cols to positions in sch (by column number); ok is false
// if one is not in sch.
func RecColIdx(sch *schema.Table, cols []RecCol) (idx []int, ok bool) {
	idx = make([]int, len(cols))
	for k, c := range cols {
		found := false
		for j := range sch.Columns {
			if sch.Columns[j].Num == c.Num {
				idx[k], found = j, true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return idx, true
}

// RecCursor is the position of a walk.
type RecCursor struct {
	Pos     int
	Gen     uint32
	Started bool
}

func (cur *RecCursor) check(gen uint32) error {
	if !cur.Started {
		cur.Started, cur.Gen = true, gen
		return nil
	}
	if cur.Gen != gen {
		return ErrRecRestart
	}
	return nil
}

// ---- columnar ----

func colKind(cv *colVec) byte {
	if !cv.stored || cv.num {
		return 0
	}
	switch cv.t {
	case schema.TInt, schema.TTime, schema.TBool:
		return 'i'
	case schema.TFloat:
		return 'f'
	case schema.TString:
		return 's'
	}
	return 0
}

// RecCols lists the columns the columnar view holds for a table, whether it
// can tell the primary key of a row (integer keys), and its live row count.
func (c *Columnar) RecCols(tableID uint32) (cols []RecCol, keyed bool, live int, ok bool) {
	ct := c.table(tableID)
	if ct == nil {
		return nil, false, 0, false
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	for i := range ct.cols {
		if k := colKind(&ct.cols[i]); k != 0 {
			cols = append(cols, RecCol{Num: ct.schema.Columns[i].Num, Name: ct.schema.Columns[i].Name, Kind: k})
		}
	}
	return cols, ct.kidx != nil, ct.n - ct.dead, true
}

// RecWalk visits up to max live rows from cur. With div > 1 only rows whose
// key hash is a multiple of div are hashed (a table without a visible key is
// then still visited whole: keyed is false). fn gets the decimal key (valid
// during the call; nil when the view cannot tell it), its hash and the row
// hash.
func (c *Columnar) RecWalk(tableID uint32, cols []RecCol, cur *RecCursor, max int, div uint64,
	fn func(key []byte, kh, rh uint64)) (done bool, err error) {
	ct := c.table(tableID)
	if ct == nil {
		return false, ErrRecUnavailable
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if err := cur.check(ct.gen); err != nil {
		return false, err
	}
	vi := make([]int, len(cols))
	for k, rc := range cols {
		vi[k] = -1
		for i := range ct.cols {
			if ct.schema.Columns[i].Num == rc.Num && colKind(&ct.cols[i]) == rc.Kind {
				vi[k] = i
			}
		}
		if vi[k] < 0 {
			return false, ErrRecRestart // a column left the view
		}
	}
	var buf [24]byte
	visited := 0
	for cur.Pos < ct.n && visited < max {
		p := cur.Pos
		cur.Pos++
		if ct.live[p>>6]&(1<<(p&63)) == 0 {
			continue
		}
		visited++
		var key []byte
		var kh uint64
		if ct.kidx != nil {
			key = strconv.AppendInt(buf[:0], ct.cols[ct.kcol].ints.at(p), 10)
			kh = maphash.Bytes(recSeed, key)
			if div > 1 && kh%div != 0 {
				continue
			}
		}
		h := uint64(len(cols))
		for k, rc := range cols {
			cv := &ct.cols[vi[k]]
			null := len(cv.nulls) > p>>6 && cv.nulls[p>>6]&(1<<(p&63)) != 0
			var iv int64
			var fv float64
			var sv string
			if !null {
				switch rc.Kind {
				case 'i':
					iv = cv.ints.at(p)
				case 'f':
					fv = cv.floats.at(p)
				default:
					sv = cv.dict.vals[cv.codes.at(p)]
				}
			}
			h = recMix(h, rc.Num, rc.Kind, null, iv, fv, sv)
		}
		fn(key, kh, h)
	}
	return cur.Pos >= ct.n, nil
}

// ---- trigram (substring) view ----

// RecCols lists the indexed columns of a table in the trigram view and its
// live document count.
func (s *Substr) RecCols(tableID uint32) (cols []RecCol, live int, ok bool) {
	st := s.table(tableID)
	if st == nil || st.failed.Load() != nil {
		return nil, 0, false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.building {
		return nil, 0, false
	}
	for _, c := range st.cols {
		if c.ci < len(st.sch.Columns) {
			cols = append(cols, RecCol{Num: st.sch.Columns[c.ci].Num, Name: c.name, Kind: 's'})
		}
	}
	return cols, st.nlive, true
}

// RecWalk visits up to max live documents from cur (see Columnar.RecWalk).
// The view keeps the indexed value of every document, so the hash covers
// the value, not only its trigrams.
func (s *Substr) RecWalk(tableID uint32, cols []RecCol, cur *RecCursor, max int, div uint64,
	fn func(key []byte, kh, rh uint64)) (done bool, err error) {
	st := s.table(tableID)
	if st == nil {
		return false, ErrRecUnavailable
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if err := cur.check(st.epoch); err != nil {
		return false, err
	}
	sc := make([]*subCol, len(cols))
	for k, rc := range cols {
		for _, c := range st.cols {
			if c.ci < len(st.sch.Columns) && st.sch.Columns[c.ci].Num == rc.Num {
				sc[k] = c
			}
		}
		if sc[k] == nil {
			return false, ErrRecRestart
		}
	}
	visited := 0
	for cur.Pos < len(st.docKey) && visited < max {
		d := cur.Pos
		cur.Pos++
		if st.live[d>>6]&(1<<(d&63)) == 0 {
			continue
		}
		visited++
		key := st.docKey[d]
		kh := RecKeyHash(key)
		if div > 1 && kh%div != 0 {
			continue
		}
		h := uint64(len(cols))
		for k, rc := range cols {
			c := sc[k]
			null := c.flags[d]&nullBit != 0
			sv := ""
			if !null {
				sv = c.vals[d]
			}
			h = recMix(h, rc.Num, 's', null, 0, 0, sv)
		}
		fn([]byte(key), kh, h)
	}
	return cur.Pos >= len(st.docKey), nil
}
