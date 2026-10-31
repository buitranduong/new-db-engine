package shadow

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Config configures Run.
type Config struct {
	SourceURL, StaskURL string
	Tables              []string // public tables, in the publication and in STASK
	Slot, Publication   string

	CreateTables       bool // initial snapshot: DROP and CREATE the tables in STASK from the source catalog
	SetReplicaIdentity bool // ALTER TABLE .. REPLICA IDENTITY FULL on source tables without a primary key
	Upsert             bool // apply INSERT as INSERT .. ON CONFLICT DO UPDATE (repair mode)
	Strict             bool // an UPDATE/DELETE that finds no row stops the tool
	Reset              bool // drop the slot and the control row, snapshot again
	// SchemaChanges: "apply" (default) carries ADD/DROP/RENAME COLUMN and
	// ALTER COLUMN TYPE of the source to STASK; "stop" stops with the DDL to run.
	SchemaChanges string

	RowsPerSec  float64       // apply rate limit (rows per second, 0 = unlimited)
	Chunk       int           // statements per pipeline flush inside a transaction
	PauseFile   string        // while this file exists, no new transaction is applied
	TxLog       string        // append one line per applied transaction (commit_us,applied_us,rows,end_lsn)
	StatusEvery time.Duration // standby status / idle poll interval
	LogEvery    time.Duration // progress line interval
	Logf        func(string, ...any)
	Stats       *Stats
}

func (c *Config) defaults() {
	if c.Slot == "" {
		c.Slot = "stashadow"
	}
	if c.SchemaChanges == "" {
		c.SchemaChanges = "apply"
	}
	if c.Publication == "" {
		c.Publication = "stashadow_pub"
	}
	if c.StatusEvery <= 0 {
		c.StatusEvery = time.Second
	}
	if c.LogEvery <= 0 {
		c.LogEvery = 5 * time.Second
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Stats == nil {
		c.Stats = newStats()
	}
}

// Run applies the source's changes to STASK until ctx is cancelled (returns
// nil) or a fatal error occurs. Connection errors are retried with backoff and
// the stream resumes from the applied LSN stored in STASK.
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	r := &runner{cfg: cfg, reset: cfg.Reset}
	if cfg.TxLog != "" {
		f, err := os.OpenFile(cfg.TxLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		r.txlog = bufio.NewWriterSize(f, 1<<16)
		defer func() { r.txlog.Flush(); f.Close() }()
	}
	backoff := time.Second
	for {
		err := r.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if IsFatal(err) {
			return err
		}
		cfg.Logf("connection or apply error, will reconnect and resume from the applied LSN: %v", err)
		cfg.Stats.reconnects.Add(1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

type runner struct {
	cfg   Config
	reset bool
	txlog *bufio.Writer
	mu    sync.Mutex

	srcConn, stConn *pgconn.PgConn        // the source (catalog reads) and a second STASK connection (DDL), per runOnce
	shapes          map[string][]shapeCol // schema.go: the shape of each table as last applied to STASK
}

type ctlState struct {
	phase   string
	applied uint64
	txs     int64
	rows    int64
}

func readCtl(ctx context.Context, st *pgconn.PgConn, slot string) (*ctlState, error) {
	cols, ok, err := stColumns(ctx, st, ctlTable)
	if err != nil {
		return nil, err
	}
	if !ok || len(cols) == 0 {
		return nil, nil
	}
	rows, err := query(ctx, st, "SELECT phase, applied_lsn, ntxs, nrows FROM "+ctlTable+" WHERE slot = $1", &slot)
	if err != nil {
		return nil, err
	}
	if len(rows.Data) == 0 {
		return nil, nil
	}
	d := rows.Data[0]
	a, _ := strconv.ParseInt(*d[1], 10, 64)
	t, _ := strconv.ParseInt(*d[2], 10, 64)
	n, _ := strconv.ParseInt(*d[3], 10, 64)
	return &ctlState{phase: *d[0], applied: uint64(a), txs: t, rows: n}, nil
}

func (r *runner) runOnce(ctx context.Context) error {
	cfg := &r.cfg
	st, err := connect(ctx, cfg.StaskURL, false)
	if err != nil {
		return fmt.Errorf("connect to STASK: %w", err)
	}
	defer st.Close(context.Background())
	src, err := connect(ctx, cfg.SourceURL, false)
	if err != nil {
		return fmt.Errorf("connect to source: %w", err)
	}
	defer src.Close(context.Background())
	r.srcConn, r.stConn, r.shapes = src, nil, map[string][]shapeCol{}
	defer func() {
		if r.stConn != nil {
			r.stConn.Close(context.Background())
			r.stConn = nil
		}
	}()

	tables, err := LoadSourceTables(ctx, src, cfg.Tables)
	if err != nil {
		return err
	}
	snapShapes := map[string][]shapeCol{} // the shapes at the time of the source catalog read above
	for _, t := range tables {
		now, err := sourceNow(ctx, src, t.Name)
		if err != nil {
			return err
		}
		snapShapes[t.Name] = shapeOf(t, attnumsOf(now))
	}
	for _, t := range tables {
		if len(t.PK) == 0 && t.ReplIdent != 'f' && cfg.SetReplicaIdentity {
			cfg.Logf("table %s has no primary key: setting REPLICA IDENTITY FULL on the source (-set-replica-identity)", t.Name)
			if err := exec(ctx, src, "ALTER TABLE "+qi(t.Name)+" REPLICA IDENTITY FULL"); err != nil {
				return fatalf("set replica identity: %v", err)
			}
			t.ReplIdent = 'f'
		}
		if err := t.Check(); err != nil {
			return &FatalError{err}
		}
	}
	state, err := readCtl(ctx, st, cfg.Slot)
	if err != nil {
		return fmt.Errorf("read control table: %w", err)
	}
	didInit := false
	if r.reset || state == nil || state.phase != "streaming" {
		didInit = true
		if state != nil && state.phase == "snapshot" {
			cfg.Logf("previous run stopped during the initial snapshot: starting it again from scratch")
		}
		if err := r.initialize(ctx, st, src, tables); err != nil {
			return err
		}
		r.reset = false
		if state, err = readCtl(ctx, st, cfg.Slot); err != nil || state == nil {
			return fmt.Errorf("read control row after snapshot: %v", err)
		}
	}
	if err := ensureSchemaTable(ctx, st); err != nil {
		return fatalf("create %s in STASK: %v", schemaTable, err)
	}
	for _, t := range tables {
		known, ok, err := loadShape(ctx, st, cfg.Slot, t.Name)
		if err != nil {
			return err
		}
		if !ok || didInit {
			known = snapShapes[t.Name]
			if err := saveShape(ctx, st, cfg.Slot, t.Name, known); err != nil {
				return err
			}
		} else {
			// the shape applied so far, not the source's current one: the
			// stream brings the difference as Relation messages
			pk := t.pkNames()
			t.Cols = t.Cols[:0]
			for _, c := range known {
				t.Cols = append(t.Cols, Column{Name: c.Name, OID: c.OID, Type: c.Type, Kind: kindOfOID(c.OID)})
			}
			t.PK = t.PK[:0]
			for _, n := range pk {
				if j := t.colIndex(n); j >= 0 {
					t.PK = append(t.PK, j)
				}
			}
		}
		r.shapes[t.Name] = known
	}
	stcols := map[string]map[string]bool{}
	for _, t := range tables {
		cols, ok, err := stColumns(ctx, st, t.Name)
		if err != nil {
			return err
		}
		if !ok {
			return fatalf("STASK has no table %s (run with -create-tables -reset, or create it)", t.Name)
		}
		m := map[string]bool{}
		for _, c := range cols {
			m[c] = true
		}
		stcols[t.Name] = m
	}
	cfg.Logf("streaming from slot %q at applied LSN %s (txs=%d rows=%d so far)", cfg.Slot, pglogrepl.LSN(state.applied), state.txs, state.rows)
	cfg.Stats.appliedLSN.Store(state.applied)
	cfg.Stats.streaming.Store(true)
	defer cfg.Stats.streaming.Store(false)
	return r.stream(ctx, st, tables, stcols, state.applied)
}

// initialize creates the publication and the slot with an exported snapshot,
// copies every table in that snapshot into STASK and records the slot's
// consistent point as the applied LSN.
func (r *runner) initialize(ctx context.Context, st, src *pgconn.PgConn, tables []*Table) error {
	cfg := &r.cfg
	cfg.Logf("initial snapshot of %d tables", len(tables))
	if cols, ok, err := stColumns(ctx, st, ctlTable); err != nil {
		return err
	} else if !ok || len(cols) == 0 {
		if err := exec(ctx, st, "CREATE TABLE "+ctlTable+" (slot text PRIMARY KEY, phase text NOT NULL, applied_lsn bigint NOT NULL, ntxs bigint NOT NULL, nrows bigint NOT NULL, commit_us bigint NOT NULL)"); err != nil {
			return fatalf("create %s in STASK: %v", ctlTable, err)
		}
	}
	if _, err := query(ctx, st, "DELETE FROM "+ctlTable+" WHERE slot = $1", &cfg.Slot); err != nil {
		return err
	}
	if _, err := query(ctx, st, "INSERT INTO "+ctlTable+" (slot, phase, applied_lsn, ntxs, nrows, commit_us) VALUES ($1, 'snapshot', 0, 0, 0, 0)", &cfg.Slot); err != nil {
		return err
	}

	// publication
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = qi(t.Name)
	}
	pub, err := query(ctx, src, "SELECT 1 FROM pg_publication WHERE pubname = $1", &cfg.Publication)
	if err != nil {
		return err
	}
	if len(pub.Data) == 0 {
		if err := exec(ctx, src, "CREATE PUBLICATION "+qi(cfg.Publication)+" FOR TABLE "+strings.Join(names, ", ")); err != nil {
			return fatalf("create publication: %v", err)
		}
	} else {
		have, err := query(ctx, src, "SELECT tablename FROM pg_publication_tables WHERE pubname = $1 AND schemaname = 'public'", &cfg.Publication)
		if err != nil {
			return err
		}
		in := map[string]bool{}
		for _, d := range have.Data {
			in[*d[0]] = true
		}
		for _, t := range tables {
			if !in[t.Name] {
				return fatalf("publication %s exists but does not contain table %s; run on the source: ALTER PUBLICATION %s ADD TABLE %s", cfg.Publication, t.Name, qi(cfg.Publication), qi(t.Name))
			}
		}
	}

	// STASK tables
	var toTruncate []string
	for _, t := range tables {
		if cfg.CreateTables {
			if err := exec(ctx, st, "DROP TABLE IF EXISTS "+qi(t.Name)); err != nil {
				return fatalf("drop STASK table %s: %v", t.Name, err)
			}
			if err := exec(ctx, st, t.CreateDDL()); err != nil {
				return fatalf("create STASK table %s: %v\n  DDL: %s", t.Name, err, t.CreateDDL())
			}
		} else {
			cols, ok, err := stColumns(ctx, st, t.Name)
			if err != nil {
				return err
			}
			if !ok {
				return fatalf("STASK has no table %s; create it (suggested DDL: %s) or use -create-tables", t.Name, t.CreateDDL())
			}
			have := map[string]bool{}
			for _, c := range cols {
				have[c] = true
			}
			for _, c := range t.Cols {
				if !have[c.Name] {
					return fatalf("STASK table %s lacks column %s; run: ALTER TABLE %s ADD COLUMN %s %s", t.Name, c.Name, qi(t.Name), qi(c.Name), ddlType(c.Kind, c.Type))
				}
			}
			toTruncate = append(toTruncate, qi(t.Name))
		}
	}
	// One statement for all tables: PostgreSQL (and STASK) refuse to truncate a
	// table that another table references by foreign key unless that table is
	// truncated in the same statement.
	if len(toTruncate) > 0 {
		if err := exec(ctx, st, "TRUNCATE TABLE "+strings.Join(toTruncate, ", ")); err != nil {
			return fatalf("truncate STASK tables: %v", err)
		}
	}

	// slot with exported snapshot
	rc, err := connect(ctx, cfg.SourceURL, true)
	if err != nil {
		return fmt.Errorf("replication connect: %w", err)
	}
	defer rc.Close(context.Background())
	ex, err := query(ctx, src, "SELECT 1 FROM pg_replication_slots WHERE slot_name = $1", &cfg.Slot)
	if err != nil {
		return err
	}
	if len(ex.Data) > 0 {
		var derr error
		for i := 0; i < 10; i++ {
			if derr = pglogrepl.DropReplicationSlot(ctx, rc, cfg.Slot, pglogrepl.DropReplicationSlotOptions{Wait: true}); derr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if derr != nil {
			return fatalf("drop old slot %s: %v", cfg.Slot, derr)
		}
	}
	res, err := pglogrepl.CreateReplicationSlot(ctx, rc, cfg.Slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{SnapshotAction: "EXPORT_SNAPSHOT", Mode: pglogrepl.LogicalReplication})
	if err != nil {
		return fatalf("create replication slot: %v", err)
	}
	cp, err := pglogrepl.ParseLSN(res.ConsistentPoint)
	if err != nil {
		return err
	}
	cfg.Logf("slot %q created, consistent point %s, snapshot %s", cfg.Slot, cp, res.SnapshotName)
	if err := exec(ctx, src, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return err
	}
	if err := exec(ctx, src, "SET TRANSACTION SNAPSHOT "+ql(res.SnapshotName)); err != nil {
		return fatalf("import snapshot: %v", err)
	}
	t0 := time.Now()
	var total int64
	ordered, cyclic, err := copyOrder(ctx, src, tables)
	if err != nil {
		return fatalf("read source foreign keys: %v", err)
	}
	if len(cyclic) > 0 {
		cfg.Logf("warning: foreign key cycle between %v: loaded last, in listed order", cyclic)
	}
	for _, t := range ordered {
		n, err := copyTable(ctx, src, st, t)
		if err != nil {
			exec(context.Background(), src, "ROLLBACK")
			return fmt.Errorf("copy table %s: %w", t.Name, err)
		}
		total += n
		cfg.Logf("snapshot: %s %d rows", t.Name, n)
	}
	if err := exec(ctx, src, "COMMIT"); err != nil {
		return err
	}
	lsn := strconv.FormatInt(int64(cp), 10)
	if _, err := query(ctx, st, "UPDATE "+ctlTable+" SET phase = 'streaming', applied_lsn = $1, nrows = $2 WHERE slot = $3", &lsn, s(strconv.FormatInt(total, 10)), &cfg.Slot); err != nil {
		return err
	}
	cfg.Logf("snapshot done: %d rows in %v", total, time.Since(t0).Round(time.Millisecond))
	return nil
}

// copyTable pipes COPY .. TO STDOUT (CSV) of the source, read inside the
// exported snapshot, into COPY .. FROM STDIN of STASK, in one STASK transaction.
func copyTable(ctx context.Context, src, st *pgconn.PgConn, t *Table) (int64, error) {
	names := t.colNames()
	if err := exec(ctx, st, "BEGIN"); err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := src.CopyTo(ctx, pw, "COPY (SELECT "+qiList(names)+" FROM "+qi(t.Name)+") TO STDOUT WITH (FORMAT csv)")
		pw.CloseWithError(err)
		done <- err
	}()
	tag, err := st.CopyFrom(ctx, pr, "COPY "+qi(t.Name)+" ("+qiList(names)+") FROM STDIN WITH (FORMAT csv)")
	pr.CloseWithError(io.ErrClosedPipe)
	serr := <-done
	if err == nil {
		err = serr
	}
	if err != nil {
		exec(context.Background(), st, "ROLLBACK")
		return 0, err
	}
	if err := exec(ctx, st, "COMMIT"); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *runner) pause(ctx context.Context, rc *pgconn.PgConn, ack uint64) {
	if r.cfg.PauseFile == "" {
		return
	}
	first := true
	for ctx.Err() == nil {
		if _, err := os.Stat(r.cfg.PauseFile); err != nil {
			if !first {
				r.cfg.Logf("resumed")
			}
			return
		}
		if first {
			r.cfg.Logf("paused: %s exists", r.cfg.PauseFile)
			first = false
		}
		pglogrepl.SendStandbyStatusUpdate(ctx, rc, pglogrepl.StandbyStatusUpdate{WALWritePosition: pglogrepl.LSN(ack)})
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (r *runner) stream(ctx context.Context, st *pgconn.PgConn, tables []*Table, stcols map[string]map[string]bool, applied uint64) error {
	cfg := &r.cfg
	rc, err := connect(ctx, cfg.SourceURL, true)
	if err != nil {
		return fmt.Errorf("replication connect: %w", err)
	}
	defer rc.Close(context.Background())
	err = pglogrepl.StartReplication(ctx, rc, cfg.Slot, pglogrepl.LSN(applied), pglogrepl.StartReplicationOptions{
		Mode:       pglogrepl.LogicalReplication,
		PluginArgs: []string{"proto_version '1'", "publication_names '" + strings.ReplaceAll(cfg.Publication, "'", "''") + "'"},
	})
	if err != nil {
		if isPgCode(err, "55006") { // slot active (old walsender not gone yet)
			return fmt.Errorf("slot busy: %w", err)
		}
		if isPgCode(err, "42704") {
			return fatalf("replication slot %q does not exist on the source but STASK says streaming; run with -reset", cfg.Slot)
		}
		return fmt.Errorf("start replication: %w", err)
	}
	byName := map[string]*Table{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	rels := map[uint32]*rel{}
	ap := newApplier(st, cfg.Slot, cfg.Chunk, cfg.Strict, cfg.Logf)
	rl := &rateLimiter{rps: cfg.RowsPerSec}

	var (
		inTx, skip   bool
		idleAck      uint64
		lastStatus   = time.Now()
		lastLog      = time.Now()
		lastAnomaly  int64
		pendingReply bool
	)
	ack := func() uint64 {
		if idleAck > applied {
			return idleAck
		}
		return applied
	}
	status := func(reply bool) error {
		a := pglogrepl.LSN(ack())
		lastStatus = time.Now()
		return pglogrepl.SendStandbyStatusUpdate(ctx, rc, pglogrepl.StandbyStatusUpdate{WALWritePosition: a, WALFlushPosition: a, WALApplyPosition: a, ReplyRequested: reply})
	}
	for {
		if ctx.Err() != nil {
			status(false)
			return nil
		}
		if time.Since(lastLog) >= cfg.LogEvery {
			rps, p50, p99, n := cfg.Stats.Window()
			cfg.Logf("applied=%s txs=%d rows/s=%.0f lat_p50=%.1fms lat_p99=%.1fms anomalies=%d", pglogrepl.LSN(applied), n, rps, p50, p99, ap.anomalies)
			lastLog = time.Now()
		}
		rctx, cancel := context.WithTimeout(ctx, cfg.StatusEvery)
		raw, err := rc.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) && ctx.Err() == nil {
				if !inTx {
					if err := status(true); err != nil {
						return err
					}
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
			cfg.Stats.sourceLSN.Store(uint64(k.ServerWALEnd))
			if !inTx && uint64(k.ServerWALEnd) > idleAck {
				idleAck = uint64(k.ServerWALEnd)
			}
			if !inTx {
				// idle and everything before this keepalive is applied: WAL of
				// other tables and checkpoints is not lag
				cfg.Stats.appliedLSN.Store(ack())
			}
			if k.ReplyRequested {
				pendingReply = true
			}
			if pendingReply && !inTx {
				pendingReply = false
				if err := status(false); err != nil {
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
				return fatalf("decode pgoutput message: %v", err)
			}
			switch m := msg.(type) {
			case *pglogrepl.BeginMessage:
				r.pause(ctx, rc, ack())
				inTx = true
				skip = uint64(m.FinalLSN) < applied
				if !skip {
					ap.begin()
				}
			case *pglogrepl.RelationMessage:
				rr, err := r.onRelation(ctx, m, byName, stcols)
				if err != nil {
					return err
				}
				rels[m.RelationID] = rr
			case *pglogrepl.InsertMessage:
				if skip || rels[m.RelationID] == nil {
					break
				}
				nv, err := tupleVals(m.Tuple)
				if err != nil {
					return err
				}
				s, err := rels[m.RelationID].insert(nv, cfg.Upsert)
				if err != nil {
					return err
				}
				if err := ap.add(ctx, s); err != nil {
					return err
				}
			case *pglogrepl.UpdateMessage:
				if skip || rels[m.RelationID] == nil {
					break
				}
				nv, err := tupleVals(m.NewTuple)
				if err != nil {
					return err
				}
				ov, err := tupleVals(m.OldTuple)
				if err != nil {
					return err
				}
				ss, err := rels[m.RelationID].update(nv, ov)
				if err != nil {
					return err
				}
				if err := ap.add(ctx, ss...); err != nil {
					return err
				}
			case *pglogrepl.DeleteMessage:
				if skip || rels[m.RelationID] == nil {
					break
				}
				ov, err := tupleVals(m.OldTuple)
				if err != nil {
					return err
				}
				ss, err := rels[m.RelationID].delete(ov)
				if err != nil {
					return err
				}
				if err := ap.add(ctx, ss...); err != nil {
					return err
				}
			case *pglogrepl.TruncateMessage:
				if skip {
					break
				}
				for _, id := range m.RelationIDs {
					if rr := rels[id]; rr != nil {
						if err := ap.add(ctx, stmt{sql: "TRUNCATE TABLE " + rr.table()}); err != nil {
							return err
						}
					}
				}
			case *pglogrepl.CommitMessage:
				if !inTx {
					return fatalf("protocol error: Commit without Begin")
				}
				inTx = false
				if skip {
					break
				}
				end := uint64(m.TransactionEndLSN)
				if err := ap.commit(ctx, end, m.CommitTime.UnixMicro()); err != nil {
					if isPgCode(err, "22000", "23000", "23502", "23503", "23505", "22001", "22003", "22007", "22008", "22012", "22P02", "42P01", "42703", "42804", "42601", "42883") {
						return fatalf("STASK rejected a change (divergence or unsupported value): %v", err)
					}
					return err
				}
				applied = end
				cfg.Stats.appliedLSN.Store(end)
				cfg.Stats.lastCommitUs.Store(m.CommitTime.UnixMicro())
				idleAck = 0
				now := time.Now()
				lat := now.UnixMicro() - m.CommitTime.UnixMicro()
				cfg.Stats.record(ap.txRows, lat)
				cfg.Stats.mu.Lock()
				cfg.Stats.Anomalies = ap.anomalies
				cfg.Stats.mu.Unlock()
				if r.txlog != nil {
					fmt.Fprintf(r.txlog, "%d,%d,%d,%d\n", m.CommitTime.UnixMicro(), now.UnixMicro(), ap.txRows, end)
				}
				if ap.anomalies != lastAnomaly {
					lastAnomaly = ap.anomalies
				}
				if time.Since(lastStatus) >= cfg.StatusEvery {
					if err := status(false); err != nil {
						return err
					}
					if r.txlog != nil {
						r.txlog.Flush()
					}
				}
				if d := rl.delay(ap.txRows, now); d > 0 {
					select {
					case <-ctx.Done():
					case <-time.After(d):
					}
				}
			}
		}
	}
}

// onRelation checks a Relation message against the source catalog and STASK.
// A table whose shape does not fit stops the tool with the exact DDL to run:
// the tool never applies DDL by itself.
func (r *runner) onRelation(ctx context.Context, m *pglogrepl.RelationMessage, byName map[string]*Table, stcols map[string]map[string]bool) (*rel, error) {
	t := byName[m.RelationName]
	if m.Namespace != "public" || t == nil {
		r.cfg.Logf("ignoring relation %s.%s (not in -tables)", m.Namespace, m.RelationName)
		return nil, nil
	}
	var rm relMsg
	for _, c := range m.Columns {
		rm.cols = append(rm.cols, msgCol{Name: c.Name, OID: c.DataType})
	}
	if err := r.evolve(ctx, rm, t, stcols); err != nil {
		return nil, err
	}
	rr := &rel{tbl: t}
	for i, c := range m.Columns {
		k := kindOfOID(c.DataType)
		if k == KUnsupported {
			return nil, &FatalError{&DDLError{Table: t.Name, Msg: fmt.Sprintf("column %s has unsupported type oid %d", c.Name, c.DataType)}}
		}
		if !stcols[t.Name][c.Name] {
			return nil, &FatalError{&DDLError{Table: t.Name, Msg: fmt.Sprintf("source column %s does not exist in STASK; run on STASK: ALTER TABLE %s ADD COLUMN %s <type> (then restart; the tool does not apply DDL)", c.Name, qi(t.Name), qi(c.Name))}}
		}
		if j := t.colIndex(c.Name); j >= 0 && t.Cols[j].OID != c.DataType {
			return nil, &FatalError{&DDLError{Table: t.Name, Msg: fmt.Sprintf("column %s changed type (oid %d -> %d) on the source; recreate the STASK column and run with -reset", c.Name, t.Cols[j].OID, c.DataType)}}
		}
		rr.cols = append(rr.cols, relCol{Name: c.Name, Kind: k})
		_ = i
	}
	for _, p := range t.PK {
		pos := -1
		for i, c := range rr.cols {
			if c.Name == t.Cols[p].Name {
				pos = i
			}
		}
		if pos < 0 {
			return nil, &FatalError{&DDLError{Table: t.Name, Msg: fmt.Sprintf("primary key column %s is no longer in the table; run with -reset after fixing STASK", t.Cols[p].Name)}}
		}
		rr.pk = append(rr.pk, pos)
	}
	if len(t.PK) == 0 && m.ReplicaIdentity != 'f' {
		return nil, &FatalError{&DDLError{Table: t.Name, Msg: "table has no primary key and replica identity is not FULL"}}
	}
	return rr, nil
}

// DDLError reports a schema change the tool will not apply by itself.
type DDLError struct{ Table, Msg string }

func (e *DDLError) Error() string { return "schema change on table " + e.Table + ": " + e.Msg }

var _ = errors.New
