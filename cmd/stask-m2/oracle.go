package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type pgxTx = pgx.Tx

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// QueryText runs sql and returns every value as its text form ("NULL" for null).
func QueryText(ctx context.Context, c *pgx.Conn, sql string) ([][]string, error) {
	rows, err := c.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		raw := rows.RawValues()
		row := make([]string, len(raw))
		for i, v := range raw {
			if v == nil {
				row[i] = "NULL"
			} else {
				row[i] = string(v)
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- query files

type Config struct {
	SUT, RefDSN, Label, Out, QueriesDir, SchemaFile string
	N                                               int
	Seed                                            string
	HistPerSim                                      int
	Duration                                        time.Duration
	T, P, R                                         int
	ThinkT, ThinkR                                  int
	ImportEvery, ImportBatch                        int
	ImportPause, Warmup                             time.Duration
	BarrierQueries                                  string
	ImportMode, PhaseFirst                          string
	Probes                                          int
	HotFrac                                         float64
	Hot                                             int
	RefDocker, KeepRef, SkipLoad, LoadOnly, NoRef   bool
	RefSkipLoad                                     bool
	RefVolume                                       string
	CrashAt                                         time.Duration
	KillCmd, RestartCmd                             string
	RestartWait                                     time.Duration
	OracleEvery                                     time.Duration
	Force                                           bool
	GateLog, DiskPath                               string
	MinFreeGB                                       float64
	LoadMode, Exec, Iso, RefName                    string
	Columnar                                        bool
	Tear                                            int
	ProbeWorkers                                    int
	ProbeGap                                        time.Duration
	MinT, MinP, MinR2, MinImport, MinProbesPct      int    // load-happened guard (guard.go); 0 disables a minimum
	PidCmd                                          string // prints the server pid; the crash drill compares it before and after
}

func (c *Config) Query(name string, page int) string {
	b, err := os.ReadFile(filepath.Join(c.QueriesDir, name+".sql"))
	if err != nil {
		panic(err)
	}
	return strings.ReplaceAll(string(b), ":page", strconv.Itoa(page))
}

var allQueries = []string{"q01_point", "q02_list_keyset", "q03_r2_quota", "q04_facets", "q05_revenue_by_carrier", "q06_top_customers",
	"q07_stock_by_region", "q08_top3_per_store", "q09_price_churn", "q10_never_ordered", "q11_order_list_total", "q12_daily_orders",
	"q13_stock_invariant", "q14_moving_avg", "q15_search_page"}

// ---------------------------------------------------------------- periodic oracles (strong reads, one statement = one snapshot)

const (
	sqlOrphanItems = `SELECT count(*) FROM order_items i LEFT JOIN orders o ON o.id = i.order_id WHERE o.id IS NULL`
	sqlEmptyOrders = `SELECT count(*) FROM orders o WHERE o.id >= 1000000000000 AND NOT EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = o.id)`
	sqlTotals      = `SELECT count(*) FROM orders o JOIN (SELECT order_id, sum(price) AS t FROM order_items WHERE order_id >= 1000000000000 GROUP BY order_id) x
ON x.order_id = o.id WHERE o.total <> x.t`
	sqlReservedSum = `SELECT (SELECT sum(reserved) FROM stock), (SELECT count(*) FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.status = 'open' AND o.id >= 1000000000000)`
)

func one(ctx context.Context, c *pgx.Conn, sql string) ([]string, error) {
	rows, err := QueryText(ctx, c, sql)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("expected 1 row, got %d", len(rows))
	}
	return rows[0], nil
}

func (r *Run) oraclePass(ctx context.Context, s *Sess, baseline int64, phase string) {
	c, err := s.Conn(ctx)
	if err != nil {
		return
	}
	fail := func(name string, err error) {
		if ctx.Err() != nil {
			return
		}
		if ErrCode(err) == "conn" {
			s.Drop()
		}
		r.Count("oracle_query_errors", 1)
		r.Count("oracle_query_err:"+name+": "+trunc(err.Error(), 90), 1)
	}
	rows, err := QueryText(ctx, c, r.cfg.Query("q13_stock_invariant", 0))
	if err != nil {
		fail("q13", err)
	} else {
		r.Check("stock_invariant_q13", len(rows) == 0, "%d (store,category) rows differ, first %v (%s)", len(rows), first(rows), phase)
	}
	for _, q := range []struct{ name, sql string }{{"no_orphan_order_items", sqlOrphanItems}, {"no_order_without_items", sqlEmptyOrders}, {"order_total_equals_items", sqlTotals}} {
		v, err := one(ctx, c, q.sql)
		if err != nil {
			fail(q.name, err)
			continue
		}
		r.Check(q.name, v[0] == "0", "count=%s (%s)", v[0], phase)
	}
	if v, err := one(ctx, c, sqlReservedSum); err != nil {
		fail("reserved_sum", err)
	} else {
		a, _ := strconv.ParseInt(strings.Split(v[0], ".")[0], 10, 64)
		b, _ := strconv.ParseInt(v[1], 10, 64)
		r.Check("reserved_stock_equals_open_items", a-baseline == b, "sum(stock.reserved)-baseline=%d but open T order items=%d (%s)", a-baseline, b, phase)
	}
	r.paginationCheck(ctx, c, s, phase)
}

func first(rows [][]string) any {
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// paginationCheck: on the frozen sub-population no write touches the rows, so pages 1..3 of the quota query must be disjoint
// and their concatenation must equal one query of triple page size (the prefix of the same total order).
func (r *Run) paginationCheck(ctx context.Context, c *pgx.Conn, s *Sess, phase string) {
	lo := r.g.FrozenLo()
	var pages [3][][]string
	for p := 1; p <= 3; p++ {
		rows, err := QueryText(ctx, c, R2Query(p, 1, lo))
		if err != nil {
			if ctx.Err() == nil {
				r.Count("oracle_query_errors", 1)
				if ErrCode(err) == "conn" {
					s.Drop()
				}
			}
			return
		}
		pages[p-1] = rows
	}
	seen := map[string]int{}
	var cat [][]string
	for i, pg := range pages {
		for _, row := range pg {
			if j, dup := seen[row[0]]; dup {
				r.Check("r2_pages_disjoint_frozen", false, "sim %s on page %d and page %d (%s)", row[0], j+1, i+1, phase)
			}
			seen[row[0]] = i
			cat = append(cat, row)
		}
	}
	r.Check("r2_pages_disjoint_frozen", true, "")
	whole, err := QueryText(ctx, c, R2Query(1, 3, lo))
	if err != nil {
		return
	}
	key := func(rows [][]string) string {
		ks := make([]string, len(rows))
		for i, x := range rows {
			ks[i] = strings.Join(x, "|")
		}
		return strings.Join(ks, "\n")
	}
	// the triple-size query orders each carrier's rows 1..3q; concatenated pages are the same set per carrier, in a different
	// overall order (sorted by the same keys), so compare as sorted multisets
	a, b := strings.Split(key(cat), "\n"), strings.Split(key(whole), "\n")
	sort.Strings(a)
	sort.Strings(b)
	r.Check("r2_pages_prefix_consistent_frozen", strings.Join(a, "\n") == strings.Join(b, "\n"),
		"pages 1..3 (%d rows) differ from the single query of triple size (%d rows) (%s)", len(cat), len(whole), phase)
}

func (r *Run) workerOracle(ctx context.Context, baseline int64) {
	s := &Sess{dsn: r.sut}
	defer s.Drop()
	for sleepCtx(ctx, r.cfg.OracleEvery) {
		r.oraclePass(ctx, s, baseline, r.Phase())
		r.Count("oracle_passes", 1)
	}
}

// ---------------------------------------------------------------- read-your-writes probe

func (r *Run) workerProbe(ctx context.Context, probes []Sim, wid, nw int) {
	// worker wid of nw takes the probe sims with index = wid (mod nw): no two writers touch the same sim
	var mine []Sim
	for i, p := range probes {
		if i%nw == wid {
			mine = append(mine, p)
		}
	}
	probes = mine
	if len(probes) == 0 || r.cfg.Probes == 0 {
		return
	}
	w, rd := &Sess{dsn: r.sut}, &Sess{dsn: r.sut} // the read goes through ANOTHER session
	defer w.Drop()
	defer rd.Drop()
	rc := r.NewRecorder()
	nprobes := r.cfg.Probes / nw
	gap := r.cfg.Duration * 9 / 10 / time.Duration(max(1, nprobes))
	if r.cfg.ProbeGap >= 0 {
		gap = r.cfg.ProbeGap
	}
	for i := 0; i < nprobes && ctx.Err() == nil; i++ {
		sim := probes[i%len(probes)]
		val := 11 + (i*nw+wid)%990
		txid := r.txid.Add(1)
		p := &Payload{Kind: "probe", TS: fmtTS(time.Now()), Nums: []string{sim.Number}, Val: val}
		r.gate.RLock()
		t0 := time.Now()
		txc, txcancel := txContext(ctx)
		res := w.RunTx(txc, 30, func(ctx context.Context, tx pgxTx) (bool, error) { return Apply(ctx, tx, p, true, txid) })
		txcancel()
		r.gate.RUnlock()
		if res.Err != nil || res.Noop {
			if res.Unknown {
				r.unknown.Store(txid, "probe")
			}
			if ctx.Err() == nil {
				rc.AddE("probe_write", r.Phase(), time.Since(t0), ErrCode(res.Err), res.Err)
			}
			continue
		}
		r.acked.Store(txid, time.Now())
		r.ackedKind.Store(txid, "probe")
		rc.Add("probe_write", r.Phase(), time.Since(t0), "")
		c, err := rd.Conn(ctx)
		if err != nil {
			continue
		}
		// R1: point read
		rows, err := QueryText(ctx, c, fmt.Sprintf(`SELECT s.store_rate, c.code FROM sims s JOIN carriers c ON c.id = s.carrier_id WHERE s.number = '%s'`, sim.Number))
		if err == nil {
			r.Check("read_your_writes_r1", len(rows) == 1 && rows[0][0] == strconv.Itoa(val), "probe %d sim %s wrote store_rate=%d, R1 read %v", i, sim.Number, val, rows)
		} else if ctx.Err() == nil {
			rd.Drop()
		}
		// R2 shape: window over the quota partition restricted to the probe sim (filters identical to R2)
		rows, err = QueryText(ctx, c, fmt.Sprintf(`SELECT number, store_rate FROM (
  SELECT number, store_rate, row_number() OVER (PARTITION BY carrier_id ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3) AND number = '%s') t WHERE rn <= 1`, sim.Number))
		if err == nil {
			r.Check("read_your_writes_r2", len(rows) == 1 && rows[0][1] == strconv.Itoa(val), "probe %d sim %s wrote store_rate=%d, R2 read %v", i, sim.Number, val, rows)
		} else if ctx.Err() == nil {
			rd.Drop()
		}
		// R2 page shape (quota pages over the carriers, filters identical to R2 plus store_rate = val): the form the
		// hybrid path recognises; the probe sim must be among the rows with the value it just wrote
		rows, err = QueryText(ctx, c, fmt.Sprintf(`SELECT t.number, t.store_rate FROM (
  SELECT number, carrier_id, store_rate, row_number() OVER (PARTITION BY carrier_id ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND status = 'available' AND carrier_id IN (1,2,3) AND store_rate = %d) t
WHERE (t.carrier_id = 1 AND t.rn BETWEEN 1 AND 100) OR (t.carrier_id = 2 AND t.rn BETWEEN 1 AND 100) OR (t.carrier_id = 3 AND t.rn BETWEEN 1 AND 100)
ORDER BY t.store_rate DESC, md5(t.number || '42'), t.number`, val))
		if err == nil {
			seen := false
			for _, row := range rows {
				seen = seen || (row[0] == sim.Number && row[1] == strconv.Itoa(val))
			}
			r.Check("read_your_writes_r2_page", seen, "probe %d sim %s wrote store_rate=%d, the R2 page with store_rate = %d has %d rows without it", i, sim.Number, val, val, len(rows))
		} else if ctx.Err() == nil {
			rd.Drop()
		}
		r.Count("probes_done", 1)
		sleepCtx(ctx, gap)
	}
}

// ---------------------------------------------------------------- journal checks (after the run and after recovery)

type JRow struct {
	Seq, TxID int64
	Kind      string
	P         Payload
}

func readJournal(ctx context.Context, dsn string) ([]JRow, error) {
	c, err := Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	rows, err := QueryText(ctx, c, "SELECT seq, txid, kind, payload FROM m2_journal ORDER BY seq")
	if err != nil {
		return nil, err
	}
	out := make([]JRow, 0, len(rows))
	for _, x := range rows {
		var j JRow
		j.Seq, _ = strconv.ParseInt(x[0], 10, 64)
		j.TxID, _ = strconv.ParseInt(x[1], 10, 64)
		j.Kind = x[2]
		if err := json.Unmarshal([]byte(x[3]), &j.P); err != nil {
			return nil, fmt.Errorf("journal %d: %w", j.Seq, err)
		}
		out = append(out, j)
	}
	return out, nil
}

// JournalChecks: atomicity and durability of the transactions, from the journal that the transactions wrote themselves.
func (r *Run) JournalChecks(ctx context.Context, jr []JRow, tag string, ackedBefore map[int64]string) {
	c, err := Connect(ctx, r.sut)
	if err != nil {
		r.Check("journal_readable_"+tag, false, "%v", err)
		return
	}
	defer c.Close(ctx)
	inJ := map[int64]bool{}
	gapless := true
	for i, j := range jr {
		inJ[j.TxID] = true
		if j.Seq != int64(i)+1 {
			gapless = false
		}
	}
	r.Check("journal_seq_gapless_"+tag, gapless, "journal sequence has a gap or duplicate (n=%d)", len(jr))
	missing := 0
	var ex int64
	for id, k := range ackedBefore {
		if !inJ[id] {
			missing++
			ex = id
			_ = k
		}
	}
	r.Check("acked_txns_present_"+tag, missing == 0, "%d of %d acknowledged transactions missing from the journal, e.g. txid %d", missing, len(ackedBefore), ex)

	exp := map[int64]string{} // T order id -> expected status
	items := map[int64]int{}  // T order id -> items
	var histExp, impExp, impRows int64
	for _, j := range jr {
		switch j.Kind {
		case "order":
			exp[j.P.OrderID] = "open"
			items[j.P.OrderID] = len(j.P.Got)
			histExp += int64(len(j.P.Got))
		case "pay":
			exp[j.P.OrderID] = "paid"
			histExp += int64(len(j.P.Got))
		case "cancel":
			exp[j.P.OrderID] = "cancelled"
			histExp += int64(len(j.P.Got))
		case "import":
			impRows += j.P.Rows
			impExp++
		}
	}
	rows, err := QueryText(ctx, c, "SELECT id, status FROM orders WHERE id >= 1000000000000")
	if err != nil {
		r.Check("orders_readable_"+tag, false, "%v", err)
		return
	}
	bad, extra := 0, 0
	have := map[int64]bool{}
	var exs string
	for _, x := range rows {
		id, _ := strconv.ParseInt(x[0], 10, 64)
		have[id] = true
		e, ok := exp[id]
		if !ok {
			extra++
			sp, _ := r.suspect.Load(id)
			exs = fmt.Sprintf("order %d has no journal row; attempts on it: %v", id, sp)
		} else if e != x[1] {
			bad++
			sp, _ := r.suspect.Load(id)
			exs = fmt.Sprintf("order %d status %s, journal says %s; failed attempts on it: %v", id, x[1], e, sp)
		}
	}
	lost := 0
	for id := range exp {
		if !have[id] {
			lost++
			exs = fmt.Sprintf("journaled order %d absent", id)
		}
	}
	r.Check("orders_match_journal_"+tag, bad+extra+lost == 0, "wrong status %d, unjournaled %d, lost %d, e.g. %s", bad, extra, lost, exs)
	rows, err = QueryText(ctx, c, "SELECT order_id, count(*) FROM order_items WHERE order_id >= 1000000000000 GROUP BY order_id")
	if err == nil {
		bad = 0
		cnt := map[int64]int{}
		for _, x := range rows {
			id, _ := strconv.ParseInt(x[0], 10, 64)
			n, _ := strconv.Atoi(x[1])
			cnt[id] = n
		}
		for id, n := range items {
			if cnt[id] != n {
				bad++
				exs = fmt.Sprintf("order %d has %d items, journal says %d", id, cnt[id], n)
			}
		}
		r.Check("order_items_match_journal_"+tag, bad == 0 && len(cnt) == len(items), "%d orders with a partial/extra item set (journal %d orders, db %d), e.g. %s", bad, len(items), len(cnt), exs)
	}
	if v, err := one(ctx, c, "SELECT count(*) FROM history WHERE id >= 20000000000000 AND id < 30000000000000"); err == nil {
		r.Check("history_match_journal_"+tag, v[0] == strconv.FormatInt(histExp, 10), "history rows of T txns=%s, journal says %d", v[0], histExp)
	}
	if v, err := one(ctx, c, "SELECT count(*) FROM history WHERE id >= 30000000000000"); err == nil {
		r.Check("import_history_match_journal_"+tag, v[0] == strconv.FormatInt(impRows, 10), "import history rows=%s, journal says %d rows in %d imports", v[0], impRows, impExp)
	}
}

// ---------------------------------------------------------------- replay on the reference and the barrier comparison

func Replay(ctx context.Context, refDSN string, jr []JRow) (applied, diverged int, first string, err error) {
	s := &Sess{dsn: refDSN}
	defer s.Drop()
	for _, j := range jr {
		p := j.P
		want := append([]string(nil), p.Got...)
		if p.Kind == "order" {
			// replay applies the recorded effect: the sims the SUT reserved MUST all be available here (a double
			// reservation or a lost update shows up as a divergence); sims it skipped are not re-derived (read committed
			// skip-reads are not serialisable against the commit order, and are not what the oracle is about)
			p.Nums = want
		}
		wantRows, wantTotal := p.Rows, p.Total
		res := s.RunTx(ctx, 30, func(ctx context.Context, tx pgxTx) (bool, error) { return Apply(ctx, tx, &p, false, j.TxID) })
		if res.Err != nil {
			return applied, diverged, first, fmt.Errorf("replay seq %d (%s): %w", j.Seq, j.Kind, res.Err)
		}
		applied++
		if res.Noop || strings.Join(want, ",") != strings.Join(p.Got, ",") || wantRows != p.Rows || wantTotal != p.Total {
			diverged++
			if first == "" {
				first = fmt.Sprintf("seq %d %s: journal outcome %v/%d/%d, reference got %v/%d/%d (noop=%v)", j.Seq, j.Kind, want, wantRows, wantTotal, p.Got, p.Rows, p.Total, res.Noop)
			}
		}
	}
	return
}

var numRe = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][-+]?\d+)?$`)

// Norm normalises a value for row-for-row comparison across engines.
func Norm(v string) string {
	if numRe.MatchString(v) {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			if f == math.Trunc(f) && math.Abs(f) < 1e15 {
				return strconv.FormatInt(int64(f), 10)
			}
			return strconv.FormatFloat(f, 'g', 12, 64)
		}
	}
	for _, l := range []string{"2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", time.RFC3339Nano} {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.999999Z")
		}
	}
	return v
}

type QCompare struct {
	Query   string `json:"query"`
	RowsSUT int    `json:"rows_sut"`
	RowsRef int    `json:"rows_ref"`
	Equal   bool   `json:"equal"`
	SameSet bool   `json:"same_as_multiset,omitempty"`
	MsSUT   int64  `json:"ms_sut"`
	MsRef   int64  `json:"ms_ref"`
	Diff    string `json:"first_diff,omitempty"`
	ErrSUT  string `json:"error_sut,omitempty"`
	ErrRef  string `json:"error_ref,omitempty"`
}

func normRows(rows [][]string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		n := make([]string, len(r))
		for j, v := range r {
			n[j] = Norm(v)
		}
		out[i] = strings.Join(n, "|")
	}
	return out
}

func Barrier(ctx context.Context, cfg *Config, sutDSN, refDSN string) []QCompare {
	a, errA := Connect(ctx, sutDSN)
	b, errB := Connect(ctx, refDSN)
	var out []QCompare
	for _, q := range allQueries {
		if cfg.BarrierQueries != "" && !strings.Contains(","+cfg.BarrierQueries+",", ","+q+",") {
			continue
		}
		pages := []int{0}
		if q == "q03_r2_quota" {
			pages = []int{1, 2, 3}
		}
		for _, pg := range pages {
			name := q
			if pg > 0 {
				name = fmt.Sprintf("%s_p%d", q, pg)
			}
			qc := QCompare{Query: name}
			sql := cfg.Query(q, pg)
			var ra, rb [][]string
			if errA == nil {
				t0 := time.Now()
				ra, errA = QueryText(ctx, a, sql)
				qc.MsSUT = time.Since(t0).Milliseconds()
				if errA != nil {
					qc.ErrSUT = errA.Error()
					if ErrCode(errA) == "conn" {
						a, errA = Connect(ctx, sutDSN)
					} else {
						errA = nil
					}
				}
			} else {
				qc.ErrSUT = errA.Error()
			}
			if errB == nil {
				t0 := time.Now()
				rb, errB = QueryText(ctx, b, sql)
				qc.MsRef = time.Since(t0).Milliseconds()
				if errB != nil {
					qc.ErrRef = errB.Error()
					errB = nil
				}
			}
			qc.RowsSUT, qc.RowsRef = len(ra), len(rb)
			na, nb := normRows(ra), normRows(rb)
			qc.Equal = qc.ErrSUT == "" && qc.ErrRef == "" && len(na) == len(nb)
			if qc.Equal {
				for i := range na {
					if na[i] != nb[i] {
						qc.Equal = false
						qc.Diff = fmt.Sprintf("row %d: sut=%s ref=%s", i, trunc(na[i], 160), trunc(nb[i], 160))
						break
					}
				}
			} else if qc.ErrSUT == "" && qc.ErrRef == "" {
				qc.Diff = fmt.Sprintf("row count sut=%d ref=%d", len(na), len(nb))
			}
			if !qc.Equal && qc.ErrSUT == "" && qc.ErrRef == "" {
				sa, sb := append([]string(nil), na...), append([]string(nil), nb...)
				sort.Strings(sa)
				sort.Strings(sb)
				qc.SameSet = strings.Join(sa, "\n") == strings.Join(sb, "\n")
			}
			out = append(out, qc)
		}
	}
	if a != nil {
		a.Close(ctx)
	}
	if b != nil {
		b.Close(ctx)
	}
	return out
}

// ---------------------------------------------------------------- tear test: one snapshot, several tables
//
// A read-only REPEATABLE READ transaction reads, at ONE snapshot: an R2 page (hybrid path on STASK -hybrid), then the
// invariants that span orders + order_items + stock (q13, orphans, totals, reserved = open items), then the same R2
// page again. The writers (T) change those tables together in one transaction each; a read that saw one table
// before and another after a commit would violate an invariant, and the second R2 page must equal the first.
func (r *Run) workerTear(ctx context.Context, id int, baseline int64) {
	s := &Sess{dsn: r.sut}
	defer s.Drop()
	rc := r.NewRecorder()
	page := 1 + id%3
	for ctx.Err() == nil {
		sleepCtx(ctx, 100*time.Millisecond)
		c, err := s.Conn(ctx)
		if err != nil {
			continue
		}
		t0 := time.Now()
		bad := func(err error) bool {
			if err == nil || ctx.Err() != nil {
				return err != nil
			}
			r.Count("tear_query_errors", 1)
			r.Count("tear_query_err: "+trunc(err.Error(), 90), 1)
			c.Exec(context.Background(), "ROLLBACK")
			if ErrCode(err) == "conn" {
				s.Drop()
			}
			return true
		}
		if _, err := c.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); bad(err) {
			continue
		}
		q := R2Query(page, 1, "")
		first, err := QueryText(ctx, c, q)
		if bad(err) {
			continue
		}
		rows13, err := QueryText(ctx, c, r.cfg.Query("q13_stock_invariant", 0))
		if bad(err) {
			continue
		}
		okAll := true
		r.Check("tear_q13_at_one_snapshot", len(rows13) == 0, "%d rows differ, first %v", len(rows13), first_(rows13))
		for _, x := range []struct{ name, sql string }{{"tear_no_orphan_items", sqlOrphanItems}, {"tear_no_order_without_items", sqlEmptyOrders}, {"tear_order_total_equals_items", sqlTotals}} {
			v, err := one(ctx, c, x.sql)
			if bad(err) {
				okAll = false
				break
			}
			r.Check(x.name, v[0] == "0", "count=%s", v[0])
		}
		if !okAll {
			continue
		}
		v, err := one(ctx, c, sqlReservedSum)
		if bad(err) {
			continue
		}
		a, _ := strconv.ParseInt(strings.Split(v[0], ".")[0], 10, 64)
		b, _ := strconv.ParseInt(v[1], 10, 64)
		r.Check("tear_reserved_equals_open_items", a-baseline == b, "sum(stock.reserved)-baseline=%d but open T order items=%d", a-baseline, b)
		second, err := QueryText(ctx, c, q)
		if bad(err) {
			continue
		}
		same := len(first) == len(second)
		for i := 0; same && i < len(first); i++ {
			same = strings.Join(first[i], "|") == strings.Join(second[i], "|")
		}
		r.Check("tear_r2_page_repeatable_in_snapshot", same, "R2 page %d read twice in one REPEATABLE READ transaction differs (%d vs %d rows)", page, len(first), len(second))
		_, err = c.Exec(ctx, "COMMIT")
		bad(err)
		rc.Add("tear_tx", r.Phase(), time.Since(t0), "")
		r.Count("tear_snapshots", 1)
	}
}

func first_(rows [][]string) any { return first(rows) }
