package shadow

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
)

// CompareConfig configures Compare.
type CompareConfig struct {
	SourceURL, StaskURL string
	Tables              []string
	Slot                string
	Buckets             int           // key blocks for the checksum (default 256)
	Sample              int           // random rows per table checked by primary-key lookup on both sides
	QueriesFile         string        // SQL file run on both systems (see ParseQueries)
	Retries             int           // re-check a differing table/query this many times (live source)
	RetryWait           time.Duration // pause between retries
	WaitCatchUp         time.Duration // wait up to this long for STASK to apply everything the source had flushed
	Seed                int64
	Logf                func(string, ...any)
}

// Report is the result of one comparison.
type Report struct {
	Time       time.Time     `json:"time"`
	Lag        Lag           `json:"lag"`
	CaughtUp   bool          `json:"caught_up"`
	Tables     []TableReport `json:"tables"`
	Queries    []QueryReport `json:"queries,omitempty"`
	Mismatches int           `json:"mismatches"`
	OK         bool          `json:"ok"`
	Seconds    float64       `json:"seconds"`
}

// Lag measures how far STASK is behind the source.
type Lag struct {
	SourceFlushLSN  string  `json:"source_flush_lsn"`
	SlotConfirmed   string  `json:"slot_confirmed_flush_lsn"`
	AppliedLSN      string  `json:"stask_applied_lsn"`
	LagBytesApplied int64   `json:"lag_bytes_to_applied"`
	LagBytesAcked   int64   `json:"lag_bytes_to_acked"`
	AppliedTxs      int64   `json:"applied_txs"`
	AppliedRows     int64   `json:"applied_rows"`
	LastCommitAgeMs float64 `json:"last_applied_commit_age_ms"`
}

// TableReport is the comparison of one table.
type TableReport struct {
	Table       string   `json:"table"`
	SourceRows  int64    `json:"source_rows"`
	StaskRows   int64    `json:"stask_rows"`
	CountOK     bool     `json:"count_ok"`
	Buckets     int      `json:"buckets"`
	BadBuckets  int      `json:"bad_buckets"`
	OnlyInSrc   int      `json:"rows_only_in_source"`
	OnlyInStask int      `json:"rows_only_in_stask"`
	Changed     int      `json:"rows_changed"`
	Examples    []string `json:"examples,omitempty"`
	SampleN     int      `json:"sample_rows"`
	SampleBad   int      `json:"sample_mismatches"`
	Attempts    int      `json:"attempts"`
	OK          bool     `json:"ok"`
	Err         string   `json:"error,omitempty"`
}

// QueryReport is the comparison of one query run on both systems.
type QueryReport struct {
	Name     string  `json:"name"`
	SQL      string  `json:"sql"`
	OK       bool    `json:"ok"`
	Rows     int     `json:"rows"`
	Detail   string  `json:"detail,omitempty"`
	Attempts int     `json:"attempts"`
	SrcMs    float64 `json:"source_ms"`
	StaskMs  float64 `json:"stask_ms"`
}

type colInfo struct {
	name string
	kind Kind
}

// Compare compares source and STASK: row counts, order-independent checksums
// by key block (with a drill-down listing the differing rows), random rows by
// primary-key lookup, and a query set.
func Compare(ctx context.Context, cfg CompareConfig) (*Report, error) {
	if cfg.Buckets <= 0 {
		cfg.Buckets = 256
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.RetryWait <= 0 {
		cfg.RetryWait = 2 * time.Second
	}
	t0 := time.Now()
	src, err := connect(ctx, cfg.SourceURL, false)
	if err != nil {
		return nil, err
	}
	defer src.Close(context.Background())
	st, err := connect(ctx, cfg.StaskURL, false)
	if err != nil {
		return nil, err
	}
	defer st.Close(context.Background())
	tables, err := LoadSourceTables(ctx, src, cfg.Tables)
	if err != nil {
		return nil, err
	}
	rep := &Report{Time: time.Now()}
	if cfg.WaitCatchUp > 0 {
		rep.CaughtUp = waitCatchUp(ctx, src, cfg.Slot, cfg.WaitCatchUp, cfg.Logf)
	}
	rep.Lag = measureLag(ctx, src, st, cfg.Slot)
	rng := rand.New(rand.NewSource(cfg.Seed + 1))
	for _, t := range tables {
		var tr TableReport
		for attempt := 1; ; attempt++ {
			tr = compareTable(ctx, cfg, src, st, t, rng)
			tr.Attempts = attempt
			if tr.OK || attempt > cfg.Retries || ctx.Err() != nil {
				break
			}
			cfg.Logf("table %s differs (attempt %d), waiting %v and checking again", t.Name, attempt, cfg.RetryWait)
			time.Sleep(cfg.RetryWait)
			if cfg.WaitCatchUp > 0 {
				waitCatchUp(ctx, src, cfg.Slot, cfg.WaitCatchUp, cfg.Logf)
			}
		}
		rep.Tables = append(rep.Tables, tr)
		if !tr.OK {
			rep.Mismatches++
		}
	}
	if cfg.QueriesFile != "" {
		qs, err := ParseQueriesFile(cfg.QueriesFile)
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			var qr QueryReport
			for attempt := 1; ; attempt++ {
				qr = compareQuery(ctx, src, st, q)
				qr.Attempts = attempt
				if qr.OK || attempt > cfg.Retries || ctx.Err() != nil {
					break
				}
				time.Sleep(cfg.RetryWait)
			}
			rep.Queries = append(rep.Queries, qr)
			if !qr.OK {
				rep.Mismatches++
			}
		}
	}
	rep.OK = rep.Mismatches == 0
	rep.Seconds = time.Since(t0).Seconds()
	return rep, nil
}

func waitCatchUp(ctx context.Context, src *pgconn.PgConn, slot string, d time.Duration, logf func(string, ...any)) bool {
	rows, err := query(ctx, src, "SELECT pg_current_wal_flush_lsn()::text")
	if err != nil || len(rows.Data) == 0 {
		return false
	}
	target, err := pglogrepl.ParseLSN(*rows.Data[0][0])
	if err != nil {
		return false
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		r, err := query(ctx, src, "SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name = $1", &slot)
		if err == nil && len(r.Data) > 0 && r.Data[0][0] != nil {
			if c, err := pglogrepl.ParseLSN(*r.Data[0][0]); err == nil && c >= target {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	logf("not caught up to source flush LSN %s within %v", target, d)
	return false
}

func measureLag(ctx context.Context, src, st *pgconn.PgConn, slot string) Lag {
	var l Lag
	cur := uint64(0)
	if r, err := query(ctx, src, "SELECT pg_current_wal_flush_lsn()::text"); err == nil && len(r.Data) > 0 {
		l.SourceFlushLSN = *r.Data[0][0]
		v, _ := pglogrepl.ParseLSN(l.SourceFlushLSN)
		cur = uint64(v)
	}
	var conf uint64
	if r, err := query(ctx, src, "SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name = $1", &slot); err == nil && len(r.Data) > 0 && r.Data[0][0] != nil {
		l.SlotConfirmed = *r.Data[0][0]
		v, _ := pglogrepl.ParseLSN(l.SlotConfirmed)
		conf = uint64(v)
	}
	if cs, err := readCtl(ctx, st, slot); err == nil && cs != nil {
		l.AppliedLSN = pglogrepl.LSN(cs.applied).String()
		l.AppliedTxs, l.AppliedRows = cs.txs, cs.rows
		l.LagBytesApplied = int64(cur) - int64(cs.applied)
	}
	l.LagBytesAcked = int64(cur) - int64(conf)
	if r, err := query(ctx, st, "SELECT commit_us FROM "+ctlTable+" WHERE slot = $1 AND commit_us > 0", &slot); err == nil && len(r.Data) > 0 {
		us, _ := strconv.ParseInt(*r.Data[0][0], 10, 64)
		l.LastCommitAgeMs = float64(time.Now().UnixMicro()-us) / 1000
	}
	return l
}

// rowKey builds the canonical key text (primary key, or the whole row when
// there is none) and the hash of the whole row.
type scanner struct {
	cols []colInfo
	pk   []int
}

func (sc *scanner) selectSQL(table string) string {
	names := make([]string, len(sc.cols))
	for i, c := range sc.cols {
		names[i] = c.name
	}
	return "SELECT " + qiList(names) + " FROM " + qi(table)
}

func (sc *scanner) canon(vals [][]byte) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		if v == nil {
			out[i] = "\x00NULL"
		} else {
			out[i] = Normalize(sc.cols[i].kind, string(v))
		}
	}
	return out
}

func hashStrings(parts []string) uint64 {
	h := fnv.New64a()
	var lenb [4]byte
	for _, p := range parts {
		n := len(p)
		lenb[0], lenb[1], lenb[2], lenb[3] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
		h.Write(lenb[:])
		io.WriteString(h, p)
	}
	return h.Sum64()
}

func (sc *scanner) keyOf(canon []string) (key string, kh uint64, rh uint64) {
	rh = hashStrings(canon)
	if len(sc.pk) == 0 {
		return "", rh, rh
	}
	ks := make([]string, len(sc.pk))
	for i, p := range sc.pk {
		ks[i] = canon[p]
	}
	return strings.Join(ks, "\x1f"), hashStrings(ks), rh
}

type bucketSums struct {
	sum []uint64
	cnt []int64
}

// scan streams all rows of a table from c and calls fn for each.
func (sc *scanner) scan(ctx context.Context, c *pgconn.PgConn, table string, fn func(canon []string)) error {
	rr := c.ExecParams(ctx, sc.selectSQL(table), nil, nil, nil, nil)
	for rr.NextRow() {
		fn(sc.canon(rr.Values()))
	}
	_, err := rr.Close()
	return err
}

func (sc *scanner) checksum(ctx context.Context, c *pgconn.PgConn, table string, nb int) (*bucketSums, error) {
	b := &bucketSums{sum: make([]uint64, nb), cnt: make([]int64, nb)}
	err := sc.scan(ctx, c, table, func(canon []string) {
		_, kh, rh := sc.keyOf(canon)
		i := int(kh % uint64(nb))
		b.sum[i] += rh
		b.cnt[i]++
	})
	return b, err
}

type rowSet struct {
	byKey map[string]rowEntry   // tables with a primary key
	multi map[uint64]*multiRows // tables without
}
type rowEntry struct {
	hash uint64
	text []string
}
type multiRows struct {
	n    int
	text []string
}

func (sc *scanner) collect(ctx context.Context, c *pgconn.PgConn, table string, bad map[int]bool, nb int) (*rowSet, error) {
	rs := &rowSet{byKey: map[string]rowEntry{}, multi: map[uint64]*multiRows{}}
	err := sc.scan(ctx, c, table, func(canon []string) {
		key, kh, rh := sc.keyOf(canon)
		if !bad[int(kh%uint64(nb))] {
			return
		}
		if len(sc.pk) > 0 {
			rs.byKey[key] = rowEntry{hash: rh, text: canon}
			return
		}
		m := rs.multi[rh]
		if m == nil {
			m = &multiRows{text: canon}
			rs.multi[rh] = m
		}
		m.n++
	})
	return rs, err
}

func fmtRow(canon []string) string {
	p := make([]string, len(canon))
	for i, v := range canon {
		if v == "\x00NULL" {
			p[i] = "NULL"
		} else {
			p[i] = v
		}
	}
	return strings.Join(p, " | ")
}

func compareTable(ctx context.Context, cfg CompareConfig, src, st *pgconn.PgConn, t *Table, rng *rand.Rand) (tr TableReport) {
	tr = TableReport{Table: t.Name, Buckets: cfg.Buckets}
	fail := func(err error) TableReport { tr.Err = err.Error(); return tr }
	sc := &scanner{pk: t.PK}
	for _, c := range t.Cols {
		sc.cols = append(sc.cols, colInfo{c.Name, c.Kind})
	}
	// counts
	cnt := func(c *pgconn.PgConn) (int64, error) {
		r, err := query(ctx, c, "SELECT count(*) FROM "+qi(t.Name))
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(*r.Data[0][0], 10, 64)
	}
	var err1, err2 error
	tr.SourceRows, err1 = cnt(src)
	tr.StaskRows, err2 = cnt(st)
	if err1 != nil || err2 != nil {
		return fail(fmt.Errorf("count: source=%v stask=%v", err1, err2))
	}
	tr.CountOK = tr.SourceRows == tr.StaskRows
	// checksums, both sides at once on separate connections (src, st are in use by caller, so open a second STASK conn)
	var bs, bt *bucketSums
	var e1, e2 error
	st2, err := connect(ctx, cfg.StaskURL, false)
	if err != nil {
		return fail(err)
	}
	defer st2.Close(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); bs, e1 = sc.checksum(ctx, src, t.Name, cfg.Buckets) }()
	go func() { defer wg.Done(); bt, e2 = sc.checksum(ctx, st2, t.Name, cfg.Buckets) }()
	wg.Wait()
	if e1 != nil || e2 != nil {
		return fail(fmt.Errorf("checksum scan: source=%v stask=%v", e1, e2))
	}
	bad := map[int]bool{}
	for i := 0; i < cfg.Buckets; i++ {
		if bs.sum[i] != bt.sum[i] || bs.cnt[i] != bt.cnt[i] {
			bad[i] = true
		}
	}
	tr.BadBuckets = len(bad)
	if len(bad) > 0 {
		var rs, rt *rowSet
		wg.Add(2)
		go func() { defer wg.Done(); rs, e1 = sc.collect(ctx, src, t.Name, bad, cfg.Buckets) }()
		go func() { defer wg.Done(); rt, e2 = sc.collect(ctx, st2, t.Name, bad, cfg.Buckets) }()
		wg.Wait()
		if e1 != nil || e2 != nil {
			return fail(fmt.Errorf("drill-down scan: source=%v stask=%v", e1, e2))
		}
		diffSets(&tr, sc, rs, rt)
	}
	// sample by primary key lookup (different access path from the scan)
	if cfg.Sample > 0 && len(t.PK) > 0 {
		sampleRows(ctx, &tr, cfg, src, st, t, sc)
	}
	tr.OK = tr.CountOK && tr.BadBuckets == 0 && tr.SampleBad == 0 && tr.Err == ""
	return tr
}

func addExample(tr *TableReport, s string) {
	if len(tr.Examples) < 12 {
		tr.Examples = append(tr.Examples, s)
	}
}

func diffSets(tr *TableReport, sc *scanner, rs, rt *rowSet) {
	for k, a := range rs.byKey {
		b, ok := rt.byKey[k]
		switch {
		case !ok:
			tr.OnlyInSrc++
			addExample(tr, "only in source: "+fmtRow(a.text))
		case a.hash != b.hash:
			tr.Changed++
			var d []string
			for i := range a.text {
				if a.text[i] != b.text[i] {
					d = append(d, fmt.Sprintf("%s: source=%s stask=%s", sc.cols[i].name, show(a.text[i]), show(b.text[i])))
				}
			}
			addExample(tr, fmt.Sprintf("changed key=%s: %s", strings.ReplaceAll(k, "\x1f", ","), strings.Join(d, "; ")))
		}
	}
	for k, b := range rt.byKey {
		if _, ok := rs.byKey[k]; !ok {
			tr.OnlyInStask++
			addExample(tr, "only in STASK: "+fmtRow(b.text))
		}
	}
	for h, a := range rs.multi {
		b := rt.multi[h]
		n := 0
		if b != nil {
			n = b.n
		}
		if a.n > n {
			tr.OnlyInSrc += a.n - n
			addExample(tr, fmt.Sprintf("only in source (x%d more): %s", a.n-n, fmtRow(a.text)))
		}
	}
	for h, b := range rt.multi {
		n := 0
		if a := rs.multi[h]; a != nil {
			n = a.n
		}
		if b.n > n {
			tr.OnlyInStask += b.n - n
			addExample(tr, fmt.Sprintf("only in STASK (x%d more): %s", b.n-n, fmtRow(b.text)))
		}
	}
}

func show(v string) string {
	if v == "\x00NULL" {
		return "NULL"
	}
	return v
}

func sampleRows(ctx context.Context, tr *TableReport, cfg CompareConfig, src, st *pgconn.PgConn, t *Table, sc *scanner) {
	pkn := t.pkNames()
	ks, err := query(ctx, src, "SELECT "+qiList(pkn)+" FROM "+qi(t.Name)+" ORDER BY random() LIMIT "+strconv.Itoa(cfg.Sample))
	if err != nil {
		tr.Err = "sample keys: " + err.Error()
		return
	}
	var w []string
	for i, n := range pkn {
		w = append(w, fmt.Sprintf("%s = $%d", qi(n), i+1))
	}
	sql := sc.selectSQL(t.Name) + " WHERE " + strings.Join(w, " AND ")
	for _, k := range ks.Data {
		tr.SampleN++
		a, e1 := query(ctx, src, sql, k...)
		b, e2 := query(ctx, st, sql, k...)
		if e1 != nil || e2 != nil {
			tr.SampleBad++
			addExample(tr, fmt.Sprintf("sample lookup error: source=%v stask=%v", e1, e2))
			continue
		}
		ra, rb := canonRows(sc, a), canonRows(sc, b)
		if strings.Join(ra, "\n") != strings.Join(rb, "\n") {
			tr.SampleBad++
			addExample(tr, fmt.Sprintf("sample key=%s: source=%v stask=%v", derefJoin(k), ra, rb))
		}
	}
}

func derefJoin(k []*string) string {
	p := make([]string, len(k))
	for i, v := range k {
		if v != nil {
			p[i] = *v
		}
	}
	return strings.Join(p, ",")
}

func canonRows(sc *scanner, r *Rows) []string {
	out := make([]string, 0, len(r.Data))
	for _, row := range r.Data {
		raw := make([][]byte, len(row))
		for i, v := range row {
			if v != nil {
				raw[i] = []byte(*v)
			}
		}
		out = append(out, fmtRow(sc.canon(raw)))
	}
	sort.Strings(out)
	return out
}

// ---- query set ----

// Query is one statement of a query file.
type Query struct {
	Name    string
	SQL     string
	Ordered bool
}

// ParseQueriesFile reads a query file. Statements end with ";" at the end of a
// line. "-- name: x" names the next statement. Rows are compared as a set
// unless the statement contains ORDER BY or is preceded by "-- ordered".
func ParseQueriesFile(path string) ([]Query, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseQueries(string(b)), nil
}

func ParseQueries(text string) []Query {
	var out []Query
	var cur []string
	name, ordered := "", false
	for _, line := range strings.Split(text, "\n") {
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "--") {
			c := strings.TrimSpace(strings.TrimPrefix(tl, "--"))
			switch {
			case strings.HasPrefix(c, "name:"):
				name = strings.TrimSpace(strings.TrimPrefix(c, "name:"))
			case c == "ordered":
				ordered = true
			}
			continue
		}
		if tl == "" && len(cur) == 0 {
			continue
		}
		cur = append(cur, line)
		if strings.HasSuffix(tl, ";") {
			sql := strings.TrimSpace(strings.Join(cur, "\n"))
			sql = strings.TrimSuffix(sql, ";")
			if name == "" {
				name = fmt.Sprintf("q%d", len(out)+1)
			}
			out = append(out, Query{Name: name, SQL: sql, Ordered: ordered || strings.Contains(strings.ToLower(sql), "order by")})
			cur, name, ordered = nil, "", false
		}
	}
	return out
}

func runQuery(ctx context.Context, c *pgconn.PgConn, sql string) (*Rows, time.Duration, error) {
	t0 := time.Now()
	r, err := query(ctx, c, sql)
	return r, time.Since(t0), err
}

func compareQuery(ctx context.Context, src, st *pgconn.PgConn, q Query) QueryReport {
	qr := QueryReport{Name: q.Name, SQL: q.SQL}
	a, da, e1 := runQuery(ctx, src, q.SQL)
	b, db, e2 := runQuery(ctx, st, q.SQL)
	qr.SrcMs, qr.StaskMs = float64(da.Microseconds())/1000, float64(db.Microseconds())/1000
	if e1 != nil || e2 != nil {
		if e1 != nil && e2 != nil && sqlstate(e1) == sqlstate(e2) {
			qr.OK, qr.Detail = true, "both fail with SQLSTATE "+sqlstate(e1)
			return qr
		}
		qr.Detail = fmt.Sprintf("error: source=%v stask=%v", e1, e2)
		return qr
	}
	qr.Rows = len(a.Data)
	qr.OK, qr.Detail = compareResults(a, b, q.Ordered)
	return qr
}

func sqlstate(err error) string {
	var pe *pgconn.PgError
	if asPgErr(err, &pe) {
		return pe.Code
	}
	return "?"
}

// compareResults compares two result sets cell by cell, tolerating the
// textual differences of equal values (numeric scale, float digits, bool, timestamps).
func compareResults(a, b *Rows, ordered bool) (bool, string) {
	if len(a.Cols) != len(b.Cols) {
		return false, fmt.Sprintf("column count: source=%d stask=%d", len(a.Cols), len(b.Cols))
	}
	if len(a.Data) != len(b.Data) {
		return false, fmt.Sprintf("row count: source=%d stask=%d", len(a.Data), len(b.Data))
	}
	kinds := make([]Kind, len(a.Cols))
	for i := range kinds {
		kinds[i] = kindOfOID(a.OIDs[i])
	}
	ra, rb := a.Data, b.Data
	if !ordered {
		ra, rb = sortedRows(a.Data, kinds), sortedRows(b.Data, kinds)
	}
	for i := range ra {
		for j := range ra[i] {
			if !cellEq(ra[i][j], rb[i][j], kinds[j], b.OIDs[j]) {
				return false, fmt.Sprintf("row %d col %s: source=%s stask=%s", i, a.Cols[j], showPtr(ra[i][j]), showPtr(rb[i][j]))
			}
		}
	}
	return true, ""
}

func showPtr(p *string) string {
	if p == nil {
		return "NULL"
	}
	return *p
}

func sortedRows(rows [][]*string, kinds []Kind) [][]*string {
	type kr struct {
		k string
		r []*string
	}
	tmp := make([]kr, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, c := range r {
			if c == nil {
				parts[j] = "\x00NULL"
			} else {
				k := kinds[j]
				if k == KFloat4 || k == KFloat8 || k == KNumeric {
					// round hard so that float noise cannot reorder rows
					if f, err := strconv.ParseFloat(*c, 64); err == nil {
						parts[j] = strconv.FormatFloat(f, 'g', 8, 64)
						continue
					}
				}
				parts[j] = Normalize(k, *c)
			}
		}
		tmp[i] = kr{strings.Join(parts, "\x1f"), r}
	}
	sort.SliceStable(tmp, func(i, j int) bool { return tmp[i].k < tmp[j].k })
	out := make([][]*string, len(rows))
	for i := range tmp {
		out[i] = tmp[i].r
	}
	return out
}

func cellEq(a, b *string, k Kind, otherOID uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if *a == *b {
		return true
	}
	if Normalize(k, *a) == Normalize(k, *b) {
		return true
	}
	if k2 := kindOfOID(otherOID); k2 != k && Normalize(k2, *a) == Normalize(k2, *b) {
		return true
	}
	fa, e1 := strconv.ParseFloat(*a, 64)
	fb, e2 := strconv.ParseFloat(*b, 64)
	if e1 == nil && e2 == nil {
		d := math.Abs(fa - fb)
		return d <= 1e-9*math.Max(math.Abs(fa), math.Abs(fb)) || d < 1e-12
	}
	return false
}

// ---- output ----

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(r)
}

// WriteTable writes a human-readable summary.
func (r *Report) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "stashadow compare  %s  (%.1fs)\n", r.Time.Format(time.RFC3339), r.Seconds)
	fmt.Fprintf(w, "lag: source flush %s, slot confirmed %s, STASK applied %s; %d bytes behind applied, %d behind acked; applied %d txs / %d rows; caught_up=%v\n\n",
		r.Lag.SourceFlushLSN, r.Lag.SlotConfirmed, r.Lag.AppliedLSN, r.Lag.LagBytesApplied, r.Lag.LagBytesAcked, r.Lag.AppliedTxs, r.Lag.AppliedRows, r.CaughtUp)
	fmt.Fprintf(w, "%-16s %12s %12s %7s %9s %9s %9s %8s %11s  %s\n", "table", "src_rows", "stask_rows", "counts", "bad_blks", "only_src", "only_st", "changed", "sample_bad", "verdict")
	for _, t := range r.Tables {
		v := "OK"
		if !t.OK {
			v = "DIFF"
		}
		if t.Err != "" {
			v = "ERROR"
		}
		fmt.Fprintf(w, "%-16s %12d %12d %7v %4d/%-4d %9d %9d %8d %5d/%-5d  %s\n", t.Table, t.SourceRows, t.StaskRows, t.CountOK, t.BadBuckets, t.Buckets, t.OnlyInSrc, t.OnlyInStask, t.Changed, t.SampleBad, t.SampleN, v)
		for _, e := range t.Examples {
			fmt.Fprintf(w, "    %s\n", e)
		}
		if t.Err != "" {
			fmt.Fprintf(w, "    error: %s\n", t.Err)
		}
	}
	if len(r.Queries) > 0 {
		fmt.Fprintf(w, "\n%-24s %7s %10s %10s  %s\n", "query", "rows", "src_ms", "stask_ms", "verdict")
		for _, q := range r.Queries {
			v := "OK"
			if !q.OK {
				v = "DIFF: " + q.Detail
			}
			fmt.Fprintf(w, "%-24s %7d %10.2f %10.2f  %s\n", q.Name, q.Rows, q.SrcMs, q.StaskMs, v)
		}
	}
	fmt.Fprintf(w, "\nmismatches: %d  => %s\n", r.Mismatches, map[bool]string{true: "NO DIFFERENCE", false: "DIFFERENCES FOUND"}[r.OK])
}
