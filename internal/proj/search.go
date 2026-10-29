package proj

import (
	"container/heap"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"stask/internal/core"
	"stask/internal/schema"
	"stask/internal/wal"
)

// Search is an inverted index (BM25) over the `search` columns of a table,
// with fuzzy term expansion through a trigram index of the term dictionary.
// Tokens are lower-cased and Vietnamese diacritics are folded, so
// "Hà Nội" matches "ha noi".
type Search struct {
	mu      sync.RWMutex
	tables  map[uint32]*searchTable
	pending []searchChange
}

type searchChange struct {
	table  uint32
	key    string
	lsn    uint64
	del    bool
	tf     map[string]uint32
	length uint32
}

type posting struct {
	doc uint32
	tf  uint32
}

type searchTable struct {
	mu       sync.RWMutex
	schema   *schema.Table
	cols     []int
	docKey   []string
	docLen   []uint32
	docLSN   []uint64
	live     []uint64
	keyDoc   map[string]uint32
	post     map[string][]posting
	trig     map[string][]string
	totalLen int64
	nlive    int
}

func NewSearch() *Search { return &Search{tables: map[uint32]*searchTable{}} }

func (s *Search) Name() string { return "search" }

func (s *Search) table(id uint32) *searchTable {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tables[id]
}

func (s *Search) Has(id uint32) bool { return s.table(id) != nil }

func (s *Search) ensure(t *schema.Table) *searchTable {
	if len(t.Search) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.tables[t.ID]
	if st == nil {
		st = newSearchTable(t)
		s.tables[t.ID] = st
	}
	return st
}

func newSearchTable(t *schema.Table) *searchTable {
	st := &searchTable{keyDoc: map[string]uint32{}, post: map[string][]posting{}, trig: map[string][]string{}}
	st.follow(t)
	return st
}

// follow resolves the searched columns in schema t (ALTER TABLE moves
// them: DROP COLUMN of an earlier column, RENAME). Only the goroutine that
// analyzes rows calls it.
func (st *searchTable) follow(t *schema.Table) {
	if st.schema == t {
		return
	}
	st.schema, st.cols = t, st.cols[:0]
	for _, c := range t.Search {
		i, _ := t.Col(c)
		st.cols = append(st.cols, i)
	}
}

// analyze tokenizes row r, decoded with schema sch.
func (st *searchTable) analyze(r schema.Row, sch *schema.Table) (map[string]uint32, uint32) {
	st.follow(sch)
	tf := map[string]uint32{}
	var n uint32
	for _, c := range st.cols {
		if r[c].Null {
			continue
		}
		TokenizeExact(r[c].S, func(exact, folded string) {
			tf[exact]++
			if folded != exact {
				tf[foldedKey+folded]++
			}
			n++
		})
	}
	return tf, n
}

// Bootstrap fills each table before publishing it, as Columnar.Bootstrap.
func (s *Search) Bootstrap(e *core.Engine, snap uint64) error {
	for _, ts := range e.Tables() {
		if len(ts.Schema().Search) == 0 {
			continue
		}
		st := newSearchTable(ts.Schema())
		err := e.ScanSnapshot(ts.Schema().Name, snap, func(key string, r schema.Row, v *core.Version) bool {
			tf, n := st.analyze(r, ts.Schema())
			st.add(key, v.LSN, tf, n)
			return true
		})
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.tables[ts.Schema().ID] = st
		s.mu.Unlock()
	}
	return nil
}

func (s *Search) Apply(e *core.Engine, rec *wal.Record) {
	if rec.Kind == wal.KindDDL {
		// changes so far are of whole transactions: apply them before a
		// dropped (or truncated) table's view is released
		s.Flush(rec.LSN)
		live := map[uint32]bool{}
		for _, ts := range e.Tables() {
			live[ts.Schema().ID] = true
			if s.table(ts.Schema().ID) == nil {
				s.ensure(ts.Schema())
			}
		}
		s.mu.Lock()
		for id := range s.tables {
			if !live[id] {
				delete(s.tables, id)
			}
		}
		s.mu.Unlock()
		return
	}
	for _, op := range rec.Ops {
		st := s.table(op.Table)
		if st == nil {
			continue
		}
		ch := searchChange{table: op.Table, key: string(op.Key), lsn: rec.LSN, del: op.Kind == wal.OpDel}
		if !ch.del {
			r, sch, err := rowOf(e, &op, rec)
			if err != nil {
				continue
			}
			ch.tf, ch.length = st.analyze(r, sch) // tokenization happens outside the index lock
		}
		s.pending = append(s.pending, ch)
	}
}

func (s *Search) Flush(uint64) {
	if len(s.pending) == 0 {
		return
	}
	// apply per table under one lock so a multi-op transaction becomes
	// visible atomically within each table view
	sort.SliceStable(s.pending, func(i, j int) bool { return s.pending[i].table < s.pending[j].table })
	var cur *searchTable
	var curID uint32
	for _, ch := range s.pending {
		if cur == nil || ch.table != curID {
			if cur != nil {
				cur.mu.Unlock()
			}
			cur, curID = s.table(ch.table), ch.table
			cur.mu.Lock()
		}
		cur.remove(ch.key)
		if !ch.del {
			cur.add(ch.key, ch.lsn, ch.tf, ch.length)
		}
	}
	cur.mu.Unlock()
	clear(s.pending)
	s.pending = s.pending[:0]
}

func (st *searchTable) remove(key string) {
	if d, ok := st.keyDoc[key]; ok {
		st.live[d/64] &^= 1 << (d % 64)
		st.totalLen -= int64(st.docLen[d])
		st.nlive--
		delete(st.keyDoc, key)
	}
}

// add indexes a new document id. Postings of removed documents stay in the
// lists and are skipped via the live bitmap (a merge step would purge them).
func (st *searchTable) add(key string, lsn uint64, tf map[string]uint32, n uint32) {
	d := uint32(len(st.docKey))
	st.docKey = append(st.docKey, key)
	st.docLen = append(st.docLen, n)
	st.docLSN = append(st.docLSN, lsn)
	st.live = setBit(st.live, int(d))
	st.keyDoc[key] = d
	st.totalLen += int64(n)
	st.nlive++
	for term, f := range tf {
		pl, ok := st.post[term]
		if !ok && inFoldedSpace(term) {
			for _, g := range trigrams(foldedForm(term)) {
				st.trig[g] = append(st.trig[g], term)
			}
		}
		st.post[term] = append(pl, posting{d, f})
	}
}

// ---- query ----

type SearchQuery struct {
	Table string `json:"table"`
	Query string `json:"query"`
	Fuzzy bool   `json:"fuzzy,omitempty"`
	All   bool   `json:"all,omitempty"` // require every query term (AND)
	Limit int    `json:"limit,omitempty"`
}

type Hit struct {
	Key   string  `json:"key"`
	Score float64 `json:"score"`
	LSN   uint64  `json:"indexed_lsn"`
}

type SearchResult struct {
	Hits     []Hit             `json:"hits"`
	Expanded map[string]string `json:"expanded,omitempty"`
	Matched  int               `json:"matched"`
}

func (s *Search) Query(tableID uint32, q *SearchQuery) (*SearchResult, error) {
	st := s.table(tableID)
	if st == nil {
		return nil, fmt.Errorf("table has no search columns")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	type qterm struct{ exact, folded string }
	var terms []qterm
	seen := map[string]bool{}
	TokenizeExact(q.Query, func(exact, folded string) {
		if !seen[exact] {
			seen[exact] = true
			terms = append(terms, qterm{exact, folded})
		}
	})
	res := &SearchResult{}
	if len(terms) == 0 {
		return res, nil
	}

	st.mu.RLock()
	defer st.mu.RUnlock()
	N := float64(st.nlive)
	avgdl := 1.0
	if st.nlive > 0 {
		avgdl = float64(st.totalLen) / N
	}
	const k1, b = 1.2, 0.75
	sc := scratchPool.Get().(*scratch)
	defer scratchPool.Put(sc)
	sc.reset(len(st.docKey))
	for ti, qt := range terms {
		type cand struct {
			term   string
			weight float64
		}
		cands := []cand{}
		// a query word with diacritics matches exactly that word; a word
		// typed without them matches every accented variant
		if qt.exact != qt.folded {
			cands = append(cands, cand{qt.exact, 1})
		} else {
			cands = append(cands, cand{qt.exact, 1}, cand{foldedKey + qt.exact, 1})
		}
		if q.Fuzzy {
			for _, ft := range st.fuzzy(qt.folded, maxEdits(qt.folded)) {
				if foldedForm(ft.term) != qt.folded {
					cands = append(cands, cand{ft.term, 1 / float64(1+ft.dist)})
					if res.Expanded == nil {
						res.Expanded = map[string]string{}
					}
					res.Expanded[foldedForm(ft.term)] = qt.exact
				}
			}
		}
		for _, c := range cands {
			pl := st.post[c.term]
			df := float64(len(pl))
			idf := math.Log(1 + (N-df+0.5)/(df+0.5))
			for _, p := range pl {
				if st.live[p.doc/64]&(1<<(p.doc%64)) == 0 {
					continue
				}
				f := float64(p.tf)
				dl := float64(st.docLen[p.doc])
				if sc.hits[p.doc] == 0 && sc.scores[p.doc] == 0 {
					sc.touched = append(sc.touched, p.doc)
				}
				sc.scores[p.doc] += float32(c.weight * idf * f * (k1 + 1) / (f + k1*(1-b+b*dl/avgdl)))
				sc.hits[p.doc] |= 1 << (ti % 16)
			}
		}
	}
	need := uint16(1<<min(len(terms), 16)) - 1
	h := &hitHeap{}
	for _, d := range sc.touched {
		score := float64(sc.scores[d])
		if q.All && sc.hits[d] != need {
			continue
		}
		res.Matched++
		if h.Len() < limit {
			heap.Push(h, Hit{st.docKey[d], score, st.docLSN[d]})
		} else if score > (*h)[0].Score {
			(*h)[0] = Hit{st.docKey[d], score, st.docLSN[d]}
			heap.Fix(h, 0)
		}
	}
	res.Hits = make([]Hit, h.Len())
	for i := len(res.Hits) - 1; i >= 0; i-- {
		res.Hits[i] = heap.Pop(h).(Hit)
	}
	return res, nil
}

func (s *Search) Stats(id uint32) (docs, terms int) {
	st := s.table(id)
	if st == nil {
		return 0, 0
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.nlive, len(st.post)
}

// scratch is per-query dense scoring state, reused across queries: a map
// keyed by doc id is several times slower for long posting lists.
type scratch struct {
	scores  []float32
	hits    []uint16
	touched []uint32
}

var scratchPool = sync.Pool{New: func() any { return &scratch{} }}

func (s *scratch) reset(n int) {
	for _, d := range s.touched {
		s.scores[d], s.hits[d] = 0, 0
	}
	s.touched = s.touched[:0]
	if len(s.scores) < n {
		s.scores = append(s.scores, make([]float32, n-len(s.scores))...)
		s.hits = append(s.hits, make([]uint16, n-len(s.hits))...)
	}
}

// foldedKey prefixes index terms that hold the diacritic-free form of an
// accented word; plain words are indexed once under their own spelling.
const foldedKey = "\x01"

func inFoldedSpace(term string) bool {
	return strings.HasPrefix(term, foldedKey) || foldString(term) == term
}

func foldedForm(term string) string {
	if strings.HasPrefix(term, foldedKey) {
		return term[len(foldedKey):]
	}
	return foldString(term)
}

func foldString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if f, ok := fold[r]; ok {
			r = f
		}
		b.WriteRune(r)
	}
	return b.String()
}

type hitHeap []Hit

func (h hitHeap) Len() int           { return len(h) }
func (h hitHeap) Less(i, j int) bool { return h[i].Score < h[j].Score }
func (h hitHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)        { *h = append(*h, x.(Hit)) }
func (h *hitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func maxEdits(t string) int {
	n := utf8.RuneCountInString(t)
	switch {
	case n <= 3:
		return 0
	case n <= 6:
		return 1
	}
	return 2
}

type fuzzyTerm struct {
	term string
	dist int
}

// fuzzy returns dictionary terms within k edits of t, using the trigram
// index to avoid scanning the whole dictionary.
func (st *searchTable) fuzzy(t string, k int) []fuzzyTerm {
	if k == 0 {
		return nil
	}
	grams := trigrams(t)
	counts := map[string]int{}
	for _, g := range grams {
		for _, term := range st.trig[g] {
			counts[term]++
		}
	}
	// a term within k edits shares at least len(grams) - 3k trigrams
	minShared := max(1, len(grams)-3*k)
	tl := utf8.RuneCountInString(t)
	var out []fuzzyTerm
	for term, c := range counts {
		if c < minShared {
			continue
		}
		ff := foldedForm(term)
		if d := utf8.RuneCountInString(ff) - tl; d > k || d < -k {
			continue
		}
		if d := levenshtein(t, ff, k); d <= k {
			out = append(out, fuzzyTerm{term, d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		return len(st.post[out[i].term]) > len(st.post[out[j].term])
	})
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

func trigrams(t string) []string {
	r := []rune("$" + t + "$")
	if len(r) < 3 {
		return []string{string(r)}
	}
	out := make([]string, 0, len(r)-2)
	for i := 0; i+3 <= len(r); i++ {
		out = append(out, string(r[i:i+3]))
	}
	return out
}

// levenshtein with early exit once every cell of a row exceeds k.
func levenshtein(a, b string, k int) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			best = min(best, cur[j])
		}
		if best > k {
			return k + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// TokenizeExact lower-cases and splits like Tokenize, emitting each word
// both as written and with diacritics folded.
func TokenizeExact(s string, emit func(exact, folded string)) {
	var b, f strings.Builder
	flush := func() {
		if b.Len() > 0 {
			emit(b.String(), f.String())
			b.Reset()
			f.Reset()
		}
	}
	for _, r := range s {
		r = unicode.ToLower(r)
		fr := r
		if x, ok := fold[r]; ok {
			fr = x
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			f.WriteRune(fr)
		} else {
			flush()
		}
	}
	flush()
}

// Tokenize lower-cases, folds Vietnamese diacritics and splits on anything
// that is not a letter or digit.
func Tokenize(s string, emit func(string)) {
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			emit(b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		r = unicode.ToLower(r)
		if f, ok := fold[r]; ok {
			r = f
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
}

var fold = func() map[rune]rune {
	m := map[rune]rune{'đ': 'd'}
	groups := map[rune]string{
		'a': "àáảãạăằắẳẵặâầấẩẫậ",
		'e': "èéẻẽẹêềếểễệ",
		'i': "ìíỉĩị",
		'o': "òóỏõọôồốổỗộơờớởỡợ",
		'u': "ùúủũụưừứửữự",
		'y': "ỳýỷỹỵ",
	}
	for base, vs := range groups {
		for _, v := range vs {
			m[v] = base
		}
	}
	return m
}()
