package shadow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---- lag and progress as Prometheus text ----

// NewStats returns a Stats to pass in Config.Stats when the caller wants to
// read it (metrics endpoint) while Run is running.
func NewStats() *Stats { return newStats() }

// WritePrometheus writes the replication state in the Prometheus text format
// (docs/21 section 2.5): lag in bytes (source WAL end minus applied LSN) and
// in seconds (age of the newest applied commit while behind), throughput,
// reconnects and schema changes.
func (s *Stats) WritePrometheus(w io.Writer) {
	s.mu.Lock()
	txs, rows, anom := s.Txs, s.Rows, s.Anomalies
	s.mu.Unlock()
	src, app := s.sourceLSN.Load(), s.appliedLSN.Load()
	lag := int64(src) - int64(app)
	if lag < 0 {
		lag = 0
	}
	age := 0.0
	if us := s.lastCommitUs.Load(); us > 0 && lag > 0 {
		age = float64(time.Now().UnixMicro()-us) / 1e6
	}
	g := func(name, typ, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, v)
	}
	b := 0
	if s.streaming.Load() {
		b = 1
	}
	g("stashadow_streaming", "gauge", "1 while the change stream is connected and applying.", b)
	g("stashadow_source_lsn", "gauge", "Newest WAL position the source reported.", src)
	g("stashadow_applied_lsn", "gauge", "WAL position of the newest source transaction committed in the target.", app)
	g("stashadow_lag_bytes", "gauge", "Source WAL not yet applied in the target.", lag)
	g("stashadow_lag_seconds", "gauge", "Age of the newest applied source commit while the target is behind (0 when caught up).", age)
	g("stashadow_transactions_total", "counter", "Source transactions applied.", txs)
	g("stashadow_rows_total", "counter", "Changed rows applied.", rows)
	g("stashadow_anomalies_total", "counter", "Updates or deletes that found no row in the target.", anom)
	g("stashadow_reconnects_total", "counter", "Reconnects after a connection or apply error.", s.reconnects.Load())
	g("stashadow_schema_changes_total", "counter", "Column changes of the source applied to the target.", s.schemaChanges.Load())
}

// ---- verify ----

// VerifyConfig configures Verify.
type VerifyConfig struct {
	AURL, BURL string   // the reference (usually the source of record) and the system to check
	Tables     []string // empty = every base table of schema public in A
	Retries    int      // re-check a differing table this many times (live systems)
	RetryWait  time.Duration
	Logf       func(string, ...any)
}

// VerifyTable is the result for one table.
type VerifyTable struct {
	Table    string  `json:"table"`
	RowsA    int64   `json:"rows_a"`
	RowsB    int64   `json:"rows_b"`
	HashA    string  `json:"hash_a"`
	HashB    string  `json:"hash_b"`
	OK       bool    `json:"ok"`
	Error    string  `json:"error,omitempty"`
	SecondsA float64 `json:"seconds_a"`
	SecondsB float64 `json:"seconds_b"`
}

// VerifyReport is the result of Verify.
type VerifyReport struct {
	OK     bool          `json:"ok"`
	Tables []VerifyTable `json:"tables"`
	Diff   int           `json:"tables_different"`
}

// Verify compares two databases table by table with the technique of
// accept/clients/crm/data/parity.py: each side computes, in its own engine,
// the row count and an order-independent checksum, the md5 of the sorted
// list of per-row md5s of the columns written as text (NULL as '<N>'). Only
// counts and hashes cross the wire, never rows. The columns are those of
// table A; B must have them. It works in both directions (PostgreSQL to STASK
// while shadowing, STASK to PostgreSQL after the switch).
func Verify(ctx context.Context, cfg VerifyConfig) (*VerifyReport, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.RetryWait <= 0 {
		cfg.RetryWait = 2 * time.Second
	}
	a, err := connect(ctx, cfg.AURL, false)
	if err != nil {
		return nil, fmt.Errorf("connect A: %w", err)
	}
	defer a.Close(context.Background())
	b, err := connect(ctx, cfg.BURL, false)
	if err != nil {
		return nil, fmt.Errorf("connect B: %w", err)
	}
	defer b.Close(context.Background())
	rows, err := query(ctx, a, `select table_name, column_name from information_schema.columns c
		where table_schema = 'public' and table_name in (select table_name from information_schema.tables where table_schema = 'public' and table_type = 'BASE TABLE')
		order by table_name, ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("read the columns of A: %w", err)
	}
	cols := map[string][]string{}
	for _, r := range rows.Data {
		cols[*r[0]] = append(cols[*r[0]], *r[1])
	}
	var names []string
	if len(cfg.Tables) > 0 {
		for _, t := range cfg.Tables {
			if _, ok := cols[t]; !ok {
				return nil, fmt.Errorf("A has no table public.%s", t)
			}
			names = append(names, t)
		}
	} else {
		for t := range cols {
			if t == ctlTable || t == schemaTable {
				continue
			}
			names = append(names, t)
		}
		sort.Strings(names)
	}
	rep := &VerifyReport{OK: true}
	for _, t := range names {
		q := hashQuery(t, cols[t])
		var vt VerifyTable
		for try := 0; ; try++ {
			vt = VerifyTable{Table: t}
			var wg sync.WaitGroup
			wg.Add(2)
			var ea, eb error
			go func() {
				defer wg.Done()
				vt.RowsA, vt.HashA, vt.SecondsA, ea = runHash(ctx, a, q)
			}()
			go func() {
				defer wg.Done()
				vt.RowsB, vt.HashB, vt.SecondsB, eb = runHash(ctx, b, q)
			}()
			wg.Wait()
			switch {
			case ea != nil:
				vt.Error = "A: " + ea.Error()
			case eb != nil:
				vt.Error = "B: " + eb.Error()
			}
			vt.OK = vt.Error == "" && vt.RowsA == vt.RowsB && vt.HashA == vt.HashB
			if vt.OK || try >= cfg.Retries || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(cfg.RetryWait):
			}
		}
		if !vt.OK {
			rep.OK = false
			rep.Diff++
		}
		rep.Tables = append(rep.Tables, vt)
		cfg.Logf("verify %-30s rows %d/%d %s", t, vt.RowsA, vt.RowsB, map[bool]string{true: "OK", false: "DIFF"}[vt.OK])
	}
	return rep, nil
}

func hashQuery(table string, cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "coalesce(" + qi(c) + "::text,'<N>')"
	}
	expr := "md5(concat_ws('|'," + strings.Join(parts, ",") + "))"
	return "select count(*), coalesce(md5(string_agg(h,',' order by h)),'') from (select " + expr + " as h from " + qi(table) + ") s"
}

func runHash(ctx context.Context, c *pgconn.PgConn, q string) (int64, string, float64, error) {
	t0 := time.Now()
	r, err := query(ctx, c, q)
	if err != nil {
		return 0, "", 0, err
	}
	if len(r.Data) != 1 {
		return 0, "", 0, fmt.Errorf("unexpected result")
	}
	n, _ := strconv.ParseInt(*r.Data[0][0], 10, 64)
	return n, *r.Data[0][1], time.Since(t0).Seconds(), nil
}

// WriteTable prints the report.
func (r *VerifyReport) WriteTable(w io.Writer) {
	for _, t := range r.Tables {
		st := "OK"
		if !t.OK {
			st = "DIFF"
		}
		extra := ""
		if t.Error != "" {
			extra = " " + t.Error
		} else if !t.OK {
			extra = fmt.Sprintf(" hash %s / %s", t.HashA[:min(8, len(t.HashA))], t.HashB[:min(8, len(t.HashB))])
		}
		fmt.Fprintf(w, "%-40s %-4s rows=%d/%d%s\n", t.Table, st, t.RowsA, t.RowsB, extra)
	}
	fmt.Fprintf(w, "tables compared: %d  different: %d\n", len(r.Tables), r.Diff)
}

// WriteJSON writes the report as JSON.
func (r *VerifyReport) WriteJSON(w io.Writer) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(r)
}

// ---- drain ----

// Drain waits until everything the source had committed when Drain was
// called is in STASK: it reads the source's WAL position (call it after the
// source is frozen for writes), then waits until the slot's confirmed flush
// position, which the tool only advances after STASK committed, has reached
// it. Returns the position it waited for.
func Drain(ctx context.Context, sourceURL, slot string, timeout time.Duration, logf func(string, ...any)) (uint64, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	src, err := connect(ctx, sourceURL, false)
	if err != nil {
		return 0, err
	}
	defer src.Close(context.Background())
	r, err := query(ctx, src, "SELECT pg_current_wal_lsn()::text")
	if err != nil || len(r.Data) != 1 {
		return 0, fmt.Errorf("read the source WAL position: %v", err)
	}
	target, err := pglogrepl.ParseLSN(*r.Data[0][0])
	if err != nil {
		return 0, err
	}
	end := time.Now().Add(timeout)
	var conf pglogrepl.LSN
	for {
		r, err := query(ctx, src, "SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name = $1", &slot)
		if err != nil {
			return uint64(target), err
		}
		if len(r.Data) == 0 || r.Data[0][0] == nil {
			return uint64(target), fmt.Errorf("slot %q has no confirmed position (is stashadow run running?)", slot)
		}
		conf, _ = pglogrepl.ParseLSN(*r.Data[0][0])
		if conf >= target {
			logf("drained: slot confirmed %s >= source %s", conf, target)
			return uint64(target), nil
		}
		if time.Now().After(end) {
			return uint64(target), fmt.Errorf("not drained within %v: slot confirmed %s, source %s (%d bytes behind)", timeout, conf, target, int64(target)-int64(conf))
		}
		select {
		case <-ctx.Done():
			return uint64(target), ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---- sequences ----

// SeqResult is what SyncSequences did for one sequence.
type SeqResult struct {
	Name   string `json:"name"`
	From   int64  `json:"from_value"`
	To     int64  `json:"to_value_before"`
	Set    bool   `json:"set"`
	Reason string `json:"reason,omitempty"`
}

// SyncSequences copies the position of every sequence of schema public from
// one system to the other (logical replication does not carry sequences):
// the target's sequence is set to the source's last value when that is
// larger, plus margin. Run it with writes to the source frozen, before the
// applications are pointed at the target.
func SyncSequences(ctx context.Context, fromURL, toURL string, margin int64, logf func(string, ...any)) ([]SeqResult, error) {
	from, err := connect(ctx, fromURL, false)
	if err != nil {
		return nil, err
	}
	defer from.Close(context.Background())
	to, err := connect(ctx, toURL, false)
	if err != nil {
		return nil, err
	}
	defer to.Close(context.Background())
	rows, err := query(ctx, from, "SELECT sequencename, last_value::text FROM pg_sequences WHERE schemaname = 'public' AND last_value IS NOT NULL ORDER BY 1")
	if err != nil {
		return nil, fmt.Errorf("read sequences of the source: %w", err)
	}
	var out []SeqResult
	for _, r := range rows.Data {
		v, _ := strconv.ParseInt(*r[1], 10, 64)
		res := SeqResult{Name: *r[0], From: v}
		cur, err := query(ctx, to, "SELECT last_value::text FROM pg_sequences WHERE schemaname = 'public' AND sequencename = $1", r[0])
		if err != nil || len(cur.Data) == 0 {
			res.Reason = "no such sequence in the target"
			out = append(out, res)
			continue
		}
		if cur.Data[0][0] != nil {
			res.To, _ = strconv.ParseInt(*cur.Data[0][0], 10, 64)
		}
		want := v + margin
		if cur.Data[0][0] != nil && res.To >= want {
			res.Reason = "target already ahead"
			out = append(out, res)
			continue
		}
		if _, err := query(ctx, to, "SELECT setval('"+strings.ReplaceAll(qi(*r[0]), "'", "''")+"', "+strconv.FormatInt(want, 10)+", true)"); err != nil {
			return out, fmt.Errorf("setval %s: %w", *r[0], err)
		}
		res.Set = true
		logf("sequence %s: %d -> %d", res.Name, res.To, want)
		out = append(out, res)
	}
	return out, nil
}
