// Package chsync keeps ClickHouse in step with PostgreSQL, one source
// transaction at a time: every change becomes a row version stamped with
// the commit LSN of its transaction, and readers see the tables as of a
// watermark W below which every transaction is complete (docs/35).
package chsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"stask/internal/shadow"
)

type Config struct {
	SourceURL   string
	CHURL       string
	Tables      []string
	Slot        string
	Publication string
	BatchRows   int           // flush once this many row versions are pending (default 50000)
	BatchWait   time.Duration // or once the oldest pending transaction is this old (default 200 ms)
	Reset       bool          // drop the slot and the ClickHouse tables, snapshot again
	Logf        func(string, ...any)
	Stats       *Stats
	// Gate, when set, is held for writing while a batch is applied (and
	// while the initial snapshot is taken), so that a reader holding it for
	// reading sees ClickHouse exactly as of W: whole transactions, no
	// version above W (docs/35 section 7).
	Gate *Gate
	// Pin, when non-zero, stops the stream before the first transaction
	// committed after it (for at most HoldMax, default 5 s), so a reader can
	// see ClickHouse exactly as of a PostgreSQL snapshot taken at Pin.
	Pin     *atomic.Uint64
	HoldMax time.Duration
	// Layout: per table, the columns the base is sorted by before the key
	// (docs/35 section 8); Partitions: hash partitions of tables of a
	// million rows or more (default 16); DeltaRows: the delta size from
	// which a table is consolidated (default 20000).
	Layout     map[string][]string
	Partitions int
	DeltaRows  int
}

func (c *Config) defaults() {
	if c.Slot == "" {
		c.Slot = "chsync"
	}
	if c.Publication == "" {
		c.Publication = "chsync_pub"
	}
	if c.BatchRows <= 0 {
		c.BatchRows = 50000
	}
	if c.BatchWait <= 0 {
		c.BatchWait = 200 * time.Millisecond
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Stats == nil {
		c.Stats = &Stats{}
	}
	if c.Gate == nil {
		c.Gate = &Gate{}
	}
	if c.Pin == nil {
		c.Pin = &atomic.Uint64{}
	}
	if c.HoldMax <= 0 {
		c.HoldMax = 5 * time.Second
	}
	if c.Partitions <= 0 {
		c.Partitions = 16
	}
	if c.DeltaRows <= 0 {
		c.DeltaRows = 20000
	}
}

// Stats are read by the progress log (and later by the router).
type Stats struct {
	W, Received    atomic.Uint64 // watermark; end of the last transaction received
	Applied        atomic.Uint64 // commit LSN of the last transaction applied
	Held           atomic.Uint64 // commit LSN of the transaction the stream waits before (Pin); 0 = not waiting
	CaughtUp       atomic.Int64  // unix nanos when everything received was last known applied
	Txs, Versions  atomic.Int64
	Consolidations atomic.Int64
	Flushes        atomic.Int64
	FlushNanos     atomic.Int64
}

// FatalError stops the tool: running again would fail the same way.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

func IsFatal(err error) bool { var f *FatalError; return errors.As(err, &f) }

func fatalf(f string, a ...any) error { return &FatalError{fmt.Errorf(f, a...)} }

func connect(ctx context.Context, url string, replication bool) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["application_name"] = "chsync"
	cfg.RuntimeParams["timezone"] = "UTC"
	cfg.RuntimeParams["datestyle"] = "ISO, YMD"
	if replication {
		cfg.RuntimeParams["replication"] = "database"
	}
	cfg.ConnectTimeout = 10 * time.Second
	return pgconn.ConnectConfig(ctx, cfg)
}

func pgExec(ctx context.Context, c *pgconn.PgConn, sql string) error {
	_, err := c.Exec(ctx, sql).ReadAll()
	return err
}

func pgOne(ctx context.Context, c *pgconn.PgConn, sql string, args ...string) (string, bool, error) {
	pv := make([][]byte, len(args))
	for i, a := range args {
		pv[i] = []byte(a)
	}
	r := c.ExecParams(ctx, sql, pv, nil, nil, nil).Read()
	if r.Err != nil {
		return "", false, r.Err
	}
	if len(r.Rows) == 0 || r.Rows[0][0] == nil {
		return "", false, nil
	}
	return string(r.Rows[0][0]), true, nil
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

// ReadW returns the watermark of slot (ok = false before the initial snapshot finished).
func ReadW(ctx context.Context, ch *CH, slot string) (uint64, bool, error) {
	out, err := ch.Query(ctx, "SELECT w FROM "+StateTable+" FINAL WHERE slot = "+sqlString(slot))
	if err != nil {
		return 0, false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return 0, false, nil
	}
	w, err := strconv.ParseUint(out, 10, 64)
	return w, err == nil, err
}

func writeW(ctx context.Context, ch *CH, slot string, w uint64) error {
	return ch.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%s, %d, now64(3))", StateTable, sqlString(slot), w))
}

// Run snapshots the tables if needed, then applies the source changes until
// ctx ends or an error occurs.
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	src, err := connect(ctx, cfg.SourceURL, false)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer src.Close(context.Background())
	tables, err := shadow.LoadSourceTables(ctx, src, cfg.Tables)
	if err != nil {
		return fatalf("%v", err)
	}
	for _, t := range tables {
		if _, err := colDefs(t, Layout{}); err != nil {
			return fatalf("%v", err)
		}
	}
	ch := NewCH(cfg.CHURL)
	if err := ch.Wait(ctx, 30*time.Second); err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	if err := ch.Exec(ctx, stateDDL); err != nil {
		return err
	}
	w, ok, err := ReadW(ctx, ch, cfg.Slot)
	if err != nil {
		return err
	}
	if cfg.Reset || !ok {
		cfg.Gate.Lock() // readers wait for the snapshot (no W yet means none read anyway)
		w, err = initialize(ctx, &cfg, src, ch, tables)
		cfg.Gate.Unlock()
		if err != nil {
			return err
		}
		cfg.Reset = false
	}
	for _, t := range tables {
		if err := ch.Exec(ctx, viewDDL(t)); err != nil {
			return fatalf("view %s: %v", t.Name, err)
		}
	}
	cfg.Stats.W.Store(w)
	cfg.Stats.Applied.Store(w)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go consolidate(cctx, &cfg, ch, tables)
	return stream(ctx, &cfg, ch, tables, w)
}

// initialize: publication, slot with an exported snapshot, ClickHouse tables
// and a copy of every table in that snapshot, stamped with the slot's
// consistent point; W is written last (a crash before it means: start over).
func initialize(ctx context.Context, cfg *Config, src *pgconn.PgConn, ch *CH, tables []*shadow.Table) (uint64, error) {
	cfg.Logf("initial snapshot of %d tables", len(tables))
	if _, ok, err := pgOne(ctx, src, "SELECT 1 FROM pg_publication WHERE pubname = $1", cfg.Publication); err != nil {
		return 0, err
	} else if !ok {
		names := make([]string, len(tables))
		for i, t := range tables {
			names[i] = `"` + strings.ReplaceAll(t.Name, `"`, `""`) + `"`
		}
		if err := pgExec(ctx, src, `CREATE PUBLICATION "`+cfg.Publication+`" FOR TABLE `+strings.Join(names, ", ")); err != nil {
			return 0, fatalf("create publication: %v", err)
		}
	}
	for _, t := range tables {
		if _, ok, err := pgOne(ctx, src, "SELECT 1 FROM pg_publication_tables WHERE pubname = $1 AND schemaname = 'public' AND tablename = $2", cfg.Publication, t.Name); err != nil {
			return 0, err
		} else if !ok {
			return 0, fatalf("publication %s does not contain table %s", cfg.Publication, t.Name)
		}
	}
	rc, err := connect(ctx, cfg.SourceURL, true)
	if err != nil {
		return 0, fmt.Errorf("replication connect: %w", err)
	}
	defer rc.Close(context.Background())
	if _, ok, err := pgOne(ctx, src, "SELECT 1 FROM pg_replication_slots WHERE slot_name = $1", cfg.Slot); err != nil {
		return 0, err
	} else if ok {
		if err := pglogrepl.DropReplicationSlot(ctx, rc, cfg.Slot, pglogrepl.DropReplicationSlotOptions{Wait: true}); err != nil {
			return 0, fmt.Errorf("drop old slot: %w", err)
		}
	}
	res, err := pglogrepl.CreateReplicationSlot(ctx, rc, cfg.Slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{
		Mode: pglogrepl.LogicalReplication, SnapshotAction: "EXPORT_SNAPSHOT"})
	if err != nil {
		return 0, fatalf("create slot: %v", err)
	}
	cp, err := pglogrepl.ParseLSN(res.ConsistentPoint)
	if err != nil {
		return 0, err
	}
	for _, t := range tables {
		lay, err := layoutOf(ctx, cfg, src, t)
		if err != nil {
			return 0, err
		}
		b, err := baseDDL(t, lay)
		if err != nil {
			return 0, fatalf("%v", err)
		}
		d, _ := deltaDDL(t, lay)
		for _, name := range []string{t.Name, t.Name + "_at", baseName(t.Name), deltaName(t.Name), stageName(t.Name), dtmpName(t.Name)} {
			if err := ch.Exec(ctx, "DROP TABLE IF EXISTS "+qc(name)); err != nil {
				return 0, err
			}
		}
		for _, ddl := range []string{b, d, viewDDL(t)} {
			if err := ch.Exec(ctx, ddl); err != nil {
				return 0, fatalf("create %s in ClickHouse: %v", t.Name, err)
			}
		}
		cfg.Logf("%s: base sorted by %v before the key, %d partitions, LowCardinality %v", t.Name, lay.Order, lay.Partitions, keysOf(lay.LowCard))
	}
	cc, err := connect(ctx, cfg.SourceURL, false)
	if err != nil {
		return 0, err
	}
	defer cc.Close(context.Background())
	// the exported snapshot lives while rc is open and idle
	if err := pgExec(ctx, cc, "BEGIN ISOLATION LEVEL REPEATABLE READ; SET TRANSACTION SNAPSHOT '"+res.SnapshotName+"'"); err != nil {
		return 0, fmt.Errorf("import snapshot: %w", err)
	}
	for _, t := range tables {
		t0 := time.Now()
		pr, pw := io.Pipe()
		var n int64
		go func() {
			tag, err := cc.CopyTo(ctx, pw, copySelect(t, uint64(cp)))
			n = tag.RowsAffected()
			pw.CloseWithError(err)
		}()
		if err := ch.Insert(ctx, insertAll(baseName(t.Name), t), pr); err != nil {
			pr.CloseWithError(err)
			return 0, fmt.Errorf("copy %s: %w", t.Name, err)
		}
		cfg.Logf("copied %s: %d rows in %.1fs", t.Name, n, time.Since(t0).Seconds())
	}
	if err := pgExec(ctx, cc, "COMMIT"); err != nil {
		return 0, err
	}
	if err := writeW(ctx, ch, cfg.Slot, uint64(cp)); err != nil {
		return 0, err
	}
	cfg.Logf("snapshot done at %s", cp)
	return uint64(cp), nil
}

// layoutOf: the configured sort columns; LowCardinality for the text
// columns with at most 10000 distinct values in the source statistics
// (ClickHouse's guidance for LowCardinality); hash partitions for tables of
// a million rows or more.
func layoutOf(ctx context.Context, cfg *Config, src *pgconn.PgConn, t *shadow.Table) (Layout, error) {
	lay := Layout{Order: cfg.Layout[t.Name], LowCard: map[string]bool{}, Partitions: 1}
	res, err := src.Exec(ctx, "SELECT s.attname, s.n_distinct, c.reltuples FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "+
		"LEFT JOIN pg_stats s ON s.schemaname = n.nspname AND s.tablename = c.relname WHERE n.nspname = 'public' AND c.relname = "+sqlString(t.Name)).ReadAll()
	if err != nil {
		return lay, err
	}
	for _, r := range res {
		for _, row := range r.Rows {
			tuples, _ := strconv.ParseFloat(string(row[2]), 64)
			if tuples >= 1e6 {
				lay.Partitions = cfg.Partitions
			}
			if row[0] == nil {
				continue
			}
			nd, _ := strconv.ParseFloat(string(row[1]), 64)
			if nd < 0 {
				nd = -nd * tuples
			}
			if nd > 0 && nd <= 10000 {
				lay.LowCard[string(row[0])] = true
			}
		}
	}
	return lay, nil
}

func keysOf(m map[string]bool) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	slices.Sort(k)
	return k
}

// version is the newest change of one key in the current batch (nil value
// = NULL); del versions carry only the key.
type version struct {
	vals [][]byte
	del  bool
	lsn  uint64
}

type batch struct {
	rows  map[*shadow.Table]map[string]*version
	n     int
	start time.Time
	end   uint64 // end LSN of the last transaction in the batch
	last  uint64 // commit LSN of the last transaction in the batch
}

func (b *batch) put(t *shadow.Table, key string, v *version) {
	m := b.rows[t]
	if m == nil {
		m = map[string]*version{}
		b.rows[t] = m
	}
	if _, ok := m[key]; !ok {
		b.n++
	}
	m[key] = v // the newest version of a key is the only one a reader at the end of the batch can see
}

func keyOf(t *shadow.Table, vals [][]byte) string {
	var b strings.Builder
	for i, p := range t.PK {
		if i > 0 {
			b.WriteByte(0)
		}
		b.Write(vals[p])
	}
	return b.String()
}

func tupleVals(t *shadow.Table, td *pglogrepl.TupleData) ([][]byte, error) {
	if td == nil || len(td.Columns) != len(t.Cols) {
		return nil, fatalf("table %s: tuple has a different number of columns than the catalog (schema change; not handled yet, docs/35)", t.Name)
	}
	vals := make([][]byte, len(td.Columns))
	for i, c := range td.Columns {
		switch c.DataType {
		case 'n':
		case 't':
			vals[i] = append([]byte{}, c.Data...)
		case 'u':
			return nil, fatalf("table %s column %s: unchanged TOAST value in a change (set REPLICA IDENTITY FULL on the source)", t.Name, t.Cols[i].Name)
		default:
			return nil, fatalf("table %s: tuple data type %q", t.Name, c.DataType)
		}
	}
	return vals, nil
}

func (cfg *Config) flush(ctx context.Context, ch *CH, b *batch) error {
	if b.n == 0 {
		return nil
	}
	t0 := time.Now()
	cfg.Gate.Lock()
	defer cfg.Gate.Unlock()
	for t, m := range b.rows {
		var ups, dels strings.Builder
		for _, v := range m {
			out := &ups
			if v.del {
				out = &dels
				for i, p := range t.PK {
					if i > 0 {
						out.WriteByte('\t')
					}
					tsvValue(out, &t.Cols[p], v.vals[p], false)
				}
			} else {
				for i := range t.Cols {
					if i > 0 {
						out.WriteByte('\t')
					}
					tsvValue(out, &t.Cols[i], v.vals[i], v.vals[i] == nil)
				}
			}
			fmt.Fprintf(out, "\t%d\t%d\n", v.lsn, map[bool]int{false: 0, true: 1}[v.del])
		}
		if ups.Len() > 0 {
			if err := ch.Insert(ctx, insertAll(deltaName(t.Name), t), strings.NewReader(ups.String())); err != nil {
				return fmt.Errorf("apply %s: %w", t.Name, err)
			}
		}
		if dels.Len() > 0 {
			if err := ch.Insert(ctx, insertDeletes(deltaName(t.Name), t), strings.NewReader(dels.String())); err != nil {
				return fmt.Errorf("apply deletes of %s: %w", t.Name, err)
			}
		}
	}
	// only now may readers see the batch: every table of every transaction up to b.end is in
	if err := writeW(ctx, ch, cfg.Slot, b.end); err != nil {
		return err
	}
	cfg.Stats.W.Store(b.end)
	cfg.Stats.Applied.Store(b.last)
	cfg.Stats.CaughtUp.Store(time.Now().UnixNano())
	cfg.Stats.Versions.Add(int64(b.n))
	cfg.Stats.Flushes.Add(1)
	cfg.Stats.FlushNanos.Add(int64(time.Since(t0)))
	b.rows, b.n = map[*shadow.Table]map[string]*version{}, 0
	return nil
}

func stream(ctx context.Context, cfg *Config, ch *CH, tables []*shadow.Table, w uint64) error {
	rc, err := connect(ctx, cfg.SourceURL, true)
	if err != nil {
		return fmt.Errorf("replication connect: %w", err)
	}
	defer rc.Close(context.Background())
	err = pglogrepl.StartReplication(ctx, rc, cfg.Slot, pglogrepl.LSN(w), pglogrepl.StartReplicationOptions{
		Mode:       pglogrepl.LogicalReplication,
		PluginArgs: []string{"proto_version '1'", "publication_names '" + strings.ReplaceAll(cfg.Publication, "'", "''") + "'"},
	})
	if err != nil {
		return fmt.Errorf("start replication: %w", err)
	}
	byName := map[string]*shadow.Table{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	rels := map[uint32]*shadow.Table{}
	b := &batch{rows: map[*shadow.Table]map[string]*version{}, end: w}
	var (
		inTx, skip bool
		txLSN      uint64
		lastStatus time.Time
		lastLog    = time.Now()
		lastIdleW  time.Time
	)
	status := func() error {
		lastStatus = time.Now()
		a := pglogrepl.LSN(cfg.Stats.W.Load())
		return pglogrepl.SendStandbyStatusUpdate(ctx, rc, pglogrepl.StandbyStatusUpdate{WALWritePosition: a, WALFlushPosition: a, WALApplyPosition: a})
	}
	flush := func() error {
		if err := cfg.flush(ctx, ch, b); err != nil {
			return err
		}
		return status()
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(lastLog) >= 5*time.Second {
			s := cfg.Stats
			fl := s.Flushes.Load()
			ms := 0.0
			if fl > 0 {
				ms = float64(s.FlushNanos.Load()) / float64(fl) / 1e6
			}
			cfg.Logf("W=%s received=%s txs=%d versions=%d flushes=%d avg_flush=%.1fms",
				pglogrepl.LSN(s.W.Load()), pglogrepl.LSN(s.Received.Load()), s.Txs.Load(), s.Versions.Load(), fl, ms)
			lastLog = time.Now()
		}
		if !inTx && b.n > 0 && time.Since(b.start) >= cfg.BatchWait {
			if err := flush(); err != nil {
				return err
			}
		}
		if time.Since(lastStatus) >= 10*time.Second {
			if err := status(); err != nil {
				return err
			}
		}
		wait := cfg.BatchWait
		if b.n == 0 {
			wait = time.Second
		}
		rctx, cancel := context.WithTimeout(ctx, wait)
		raw, err := rc.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) && ctx.Err() == nil {
				if !inTx && b.n == 0 { // nothing arrived for a while and nothing is pending
					cfg.Stats.CaughtUp.Store(time.Now().UnixNano())
				}
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive: %w", err)
		}
		cd, ok := raw.(*pgproto3.CopyData)
		if !ok {
			if ee, ok := raw.(*pgproto3.ErrorResponse); ok {
				return fmt.Errorf("source: %w", pgconn.ErrorResponseToPgError(ee))
			}
			continue
		}
		switch cd.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			k, err := pglogrepl.ParsePrimaryKeepaliveMessage(cd.Data[1:])
			if err != nil {
				return err
			}
			// Between transactions the walsender has sent everything before
			// ServerWALEnd: with nothing pending, W may advance to it, so an
			// idle source does not look stale (persisted at most once a second).
			if !inTx && b.n == 0 && uint64(k.ServerWALEnd) > cfg.Stats.W.Load() && time.Since(lastIdleW) >= time.Second {
				cfg.Gate.Lock()
				err := writeW(ctx, ch, cfg.Slot, uint64(k.ServerWALEnd))
				if err == nil {
					cfg.Stats.W.Store(uint64(k.ServerWALEnd))
					cfg.Stats.CaughtUp.Store(time.Now().UnixNano())
				}
				cfg.Gate.Unlock()
				if err != nil {
					return err
				}
				b.end = uint64(k.ServerWALEnd)
				lastIdleW = time.Now()
			}
			if k.ReplyRequested {
				if err := status(); err != nil {
					return err
				}
			}
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(cd.Data[1:])
			if err != nil {
				return err
			}
			msg, err := pglogrepl.Parse(xld.WALData)
			if err != nil {
				return fmt.Errorf("parse pgoutput: %w", err)
			}
			switch m := msg.(type) {
			case *pglogrepl.RelationMessage:
				t := byName[m.RelationName]
				if t == nil || m.Namespace != "public" {
					continue
				}
				if len(m.Columns) != len(t.Cols) {
					return fatalf("table %s changed its columns on the source (not handled yet, docs/35); run with -reset", t.Name)
				}
				for i, c := range m.Columns {
					if c.Name != t.Cols[i].Name || c.DataType != t.Cols[i].OID {
						return fatalf("table %s: column %d is %s (oid %d) on the source, %s (oid %d) at the snapshot (not handled yet, docs/35); run with -reset",
							t.Name, i+1, c.Name, c.DataType, t.Cols[i].Name, t.Cols[i].OID)
					}
				}
				rels[m.RelationID] = t
			case *pglogrepl.BeginMessage:
				inTx, txLSN = true, uint64(m.FinalLSN)
				skip = txLSN < cfg.Stats.W.Load() // applied before a restart
				if pin := cfg.Pin.Load(); !skip && pin != 0 && txLSN > pin {
					// a reader wants ClickHouse as of pin: apply what is before it, then wait
					if err := flush(); err != nil {
						return err
					}
					cfg.Stats.Held.Store(txLSN)
					for end := time.Now().Add(cfg.HoldMax); ctx.Err() == nil && time.Now().Before(end); {
						if p := cfg.Pin.Load(); p == 0 || p >= txLSN {
							break
						}
						time.Sleep(time.Millisecond)
					}
					cfg.Stats.Held.Store(0)
				}
				if !skip && b.n == 0 {
					b.start = time.Now()
				}
			case *pglogrepl.CommitMessage:
				inTx = false
				cfg.Stats.Received.Store(uint64(m.TransactionEndLSN))
				if skip {
					continue
				}
				cfg.Stats.Txs.Add(1)
				b.end, b.last = uint64(m.TransactionEndLSN), txLSN
				if b.n >= cfg.BatchRows || time.Since(b.start) >= cfg.BatchWait {
					if err := flush(); err != nil {
						return err
					}
				}
			case *pglogrepl.InsertMessage:
				if skip {
					continue
				}
				t := rels[m.RelationID]
				if t == nil {
					return fatalf("insert into unknown relation %d", m.RelationID)
				}
				vals, err := tupleVals(t, m.Tuple)
				if err != nil {
					return err
				}
				b.put(t, keyOf(t, vals), &version{vals: vals, lsn: txLSN})
			case *pglogrepl.UpdateMessage:
				if skip {
					continue
				}
				t := rels[m.RelationID]
				if t == nil {
					return fatalf("update of unknown relation %d", m.RelationID)
				}
				vals, err := tupleVals(t, m.NewTuple)
				if err != nil {
					return err
				}
				key := keyOf(t, vals)
				if m.OldTuple != nil { // the key changed: the old key is gone
					old := make([][]byte, len(t.Cols))
					for _, p := range t.PK {
						if c := m.OldTuple.Columns[p]; c.DataType == 't' {
							old[p] = append([]byte{}, c.Data...)
						}
					}
					if oldKey := keyOf(t, old); oldKey != key {
						b.put(t, oldKey, &version{vals: old, del: true, lsn: txLSN})
					}
				}
				b.put(t, key, &version{vals: vals, lsn: txLSN})
			case *pglogrepl.DeleteMessage:
				if skip {
					continue
				}
				t := rels[m.RelationID]
				if t == nil {
					return fatalf("delete from unknown relation %d", m.RelationID)
				}
				if m.OldTuple == nil || len(m.OldTuple.Columns) != len(t.Cols) {
					return fatalf("table %s: delete without the old key", t.Name)
				}
				old := make([][]byte, len(t.Cols))
				for _, p := range t.PK {
					if c := m.OldTuple.Columns[p]; c.DataType == 't' {
						old[p] = append([]byte{}, c.Data...)
					}
				}
				b.put(t, keyOf(t, old), &version{vals: old, del: true, lsn: txLSN})
			case *pglogrepl.TruncateMessage:
				if skip {
					continue
				}
				return fatalf("TRUNCATE on the source is not handled yet (docs/35); run with -reset")
			}
		}
	}
}
