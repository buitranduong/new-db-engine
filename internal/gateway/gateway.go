// Package gateway is the single endpoint of docs/35 section 5: clients
// speak the PostgreSQL protocol to it; transactions, writes and cheap
// reads go to PostgreSQL unchanged, expensive reads whose results were
// proven identical go to ClickHouse at a watermark that includes the
// session's own writes.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"stask/internal/chsync"
)

type Config struct {
	Listen        string
	PGURL         string // PostgreSQL as the gateway's backend user; client sessions run as this user
	CHURL         string
	Tables        []string            // tables chsync keeps in ClickHouse
	Slot          string              // chsync slot (key of the watermark)
	CostThreshold float64             // PostgreSQL estimated cost from which a read is a ClickHouse candidate
	VerifyRuns    int                 // identical comparisons before a shape is answered by ClickHouse
	VerifyConns   int                 // PostgreSQL connections for comparisons (at most this many at once)
	VerifyTimeout time.Duration       // one comparison, PostgreSQL query included
	Resample      float64             // fraction of ClickHouse answers compared again
	MaxStale      time.Duration       // W older than this: PostgreSQL
	WaitOwnWrite  time.Duration       // how long a read waits for ClickHouse to include the session's writes
	MaxWait       time.Duration       // the longest wait for ClickHouse, for shapes slow on PostgreSQL
	PollEvery     time.Duration       // how often a waiting read looks at W again
	Reset         bool                // chsync: snapshot the tables again
	Layout        map[string][]string // chsync: base sort columns per table (docs/35 section 8)
	Partitions    int
	DeltaRows     int
	Logf          func(string, ...any)
}

func New(ctx context.Context, cfg Config) (*Router, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	r := &Router{cfg: cfg, ch: chsync.NewCH(cfg.CHURL), verify: make(chan *pgconn.PgConn, max(cfg.VerifyConns, 1))}
	for i := 0; i < cap(r.verify); i++ {
		c, err := pgconn.Connect(ctx, cfg.PGURL)
		if err != nil {
			return nil, err
		}
		r.verify <- c
	}
	go r.runSync(ctx)
	return r, nil
}

// Serve accepts clients until ctx ends.
func (r *Router) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.cfg.Listen)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	r.cfg.Logf("listening on %s", r.cfg.Listen)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer c.Close()
			if err := r.session(ctx, c); err != nil && !errors.Is(err, io.EOF) {
				r.cfg.Logf("session: %v", err)
			}
		}()
	}
}

type session struct {
	r       *Router
	cli     *pgproto3.Backend
	be      *pgproto3.Frontend
	beConn  net.Conn
	tx      byte   // transaction status of the backend: I, T, E
	dirty   bool   // a write committed since ownLSN was read
	ownLSN  uint64 // ClickHouse must have W >= ownLSN before answering this session
	sentRow int
}

func (r *Router) session(ctx context.Context, c net.Conn) error {
	cli := pgproto3.NewBackend(c, c)
	var params map[string]string
	for params == nil {
		m, err := cli.ReceiveStartupMessage()
		if err != nil {
			return err
		}
		switch m := m.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := c.Write([]byte("N")); err != nil {
				return err
			}
		case *pgproto3.StartupMessage:
			params = m.Parameters
		case *pgproto3.CancelRequest:
			return nil // not forwarded yet
		default:
			return fmt.Errorf("unexpected startup message %T", m)
		}
	}
	pc, err := pgconn.Connect(ctx, r.cfg.PGURL)
	if err != nil {
		cli.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "08006", Message: "gateway: PostgreSQL: " + err.Error()})
		cli.Flush()
		return err
	}
	hc, err := pc.Hijack()
	if err != nil {
		return err
	}
	defer hc.Conn.Close()
	s := &session{r: r, cli: cli, be: hc.Frontend, beConn: hc.Conn, tx: 'I'}
	cli.Send(&pgproto3.AuthenticationOk{})
	for k, v := range hc.ParameterStatuses {
		cli.Send(&pgproto3.ParameterStatus{Name: k, Value: v})
	}
	cli.Send(&pgproto3.BackendKeyData{ProcessID: hc.PID, SecretKey: hc.SecretKey})
	cli.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := cli.Flush(); err != nil {
		return err
	}
	// The session runs in UTC like chsync; a client asking for another time zone is served by PostgreSQL only.
	utc := params["TimeZone"] == "" || strings.EqualFold(params["TimeZone"], "UTC") || strings.EqualFold(params["TimeZone"], "Etc/UTC")
	if tz := params["TimeZone"]; tz != "" {
		if err := s.backendExec("SET TimeZone = '" + strings.ReplaceAll(tz, "'", "''") + "'"); err != nil {
			return err
		}
	}
	for {
		m, err := cli.Receive()
		if err != nil {
			return err
		}
		switch m := m.(type) {
		case *pgproto3.Terminate:
			return nil
		case *pgproto3.Query:
			if utc && s.tx == 'I' {
				if done, err := s.tryCH(ctx, m.String); err != nil {
					return err
				} else if done {
					continue
				}
			}
			r.Stats.PG.Add(1)
			s.be.Send(m)
			if err := s.relay(); err != nil {
				return err
			}
		case *pgproto3.Sync:
			s.be.Send(m)
			if err := s.relay(); err != nil {
				return err
			}
		case *pgproto3.Flush:
			s.be.Send(m)
			if err := s.relayAvailable(); err != nil {
				return err
			}
		default: // Parse, Bind, Describe, Execute, Close: the extended protocol goes to PostgreSQL
			s.be.Send(m)
		}
	}
}

// tryCH answers a simple query from ClickHouse when the docs/35 rules allow it.
func (s *session) tryCH(ctx context.Context, sql string) (bool, error) {
	r := s.r
	q, ok := readOnlyCandidate(sql)
	if !ok {
		return false, nil
	}
	sh := r.shapeOf(Shape(q))
	sh.runs.Add(1)
	r.classify(ctx, s, sh, q) // no-op unless the shape is new
	sh.mu.Lock()
	state, fields, pgMs := sh.state, sh.fields, sh.pgMs
	sh.mu.Unlock()
	// How long ClickHouse may be waited for: a shape PostgreSQL answers
	// slowly is better waited for than sent there, up to as long as
	// PostgreSQL took for it (at most MaxWait).
	wait := r.cfg.WaitOwnWrite
	if d := time.Duration(pgMs * float64(time.Millisecond)); d > wait {
		wait = min(d, r.cfg.MaxWait)
	}
	switch state {
	case shapePG:
		return false, nil
	case shapeVerifying:
		r.startVerify(sh, q)
		return false, nil
	}
	if !r.waitFresh(wait - r.cfg.WaitOwnWrite) {
		r.Stats.Stale.Add(1)
		return false, nil
	}
	if s.dirty { // read your writes: the session's last commit must be in ClickHouse
		rows, err := s.backendQuery("SELECT pg_current_wal_lsn()::text")
		if err != nil {
			return false, err
		}
		l, _ := pglogrepl.ParseLSN(string(rows[0][0]))
		s.ownLSN, s.dirty = uint64(l), false
	}
	if !r.waitW(s.ownLSN, wait) {
		r.Stats.Stale.Add(1)
		return false, nil
	}
	t0 := time.Now()
	rows, err := r.runCH(ctx, q, fields, nil)
	ms := float64(time.Since(t0).Microseconds()) / 1000
	if err != nil {
		sh.mu.Lock()
		sh.state, sh.reason = shapePG, "ClickHouse error: "+clip(err.Error())
		sh.mu.Unlock()
		r.cfg.Logf("shape -> PostgreSQL (ClickHouse error %v): %s", err, clip(Shape(q)))
		return false, nil
	}
	r.Stats.CH.Add(1)
	sh.chRuns.Add(1)
	sh.mu.Lock()
	if sh.chMs = append(sh.chMs, ms); len(sh.chMs) > 10000 {
		sh.chMs = sh.chMs[1:]
	}
	sh.mu.Unlock()
	s.cli.Send(&pgproto3.RowDescription{Fields: fields})
	for _, row := range rows {
		s.cli.Send(&pgproto3.DataRow{Values: row})
	}
	s.cli.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT " + strconv.Itoa(len(rows)))})
	s.cli.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if r.sample() {
		r.startVerify(sh, q)
	}
	return true, s.cli.Flush()
}

// relay forwards backend messages to the client until ReadyForQuery,
// including COPY FROM STDIN data from the client.
func (s *session) relay() error {
	if err := s.be.Flush(); err != nil {
		return err
	}
	wrote := false
	n := 0
	for {
		m, err := s.be.Receive()
		if err != nil {
			return err
		}
		switch m := m.(type) {
		case *pgproto3.CommandComplete:
			if isWrite(m.CommandTag) {
				wrote = true
			}
		case *pgproto3.CopyInResponse:
			s.cli.Send(m)
			if err := s.cli.Flush(); err != nil {
				return err
			}
			if err := s.copyIn(); err != nil {
				return err
			}
			continue
		case *pgproto3.ReadyForQuery:
			s.tx = m.TxStatus
			if wrote {
				s.dirty = true
			}
			s.cli.Send(m)
			return s.cli.Flush()
		}
		s.cli.Send(m)
		if n++; n%256 == 0 {
			if err := s.cli.Flush(); err != nil {
				return err
			}
		}
	}
}

func (s *session) copyIn() error {
	for {
		m, err := s.cli.Receive()
		if err != nil {
			return err
		}
		s.be.Send(m)
		switch m.(type) {
		case *pgproto3.CopyDone, *pgproto3.CopyFail:
			return s.be.Flush()
		}
	}
}

// relayAvailable forwards what the backend sends in answer to a Flush.
func (s *session) relayAvailable() error {
	if err := s.be.Flush(); err != nil {
		return err
	}
	for {
		s.beConn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		m, err := s.be.Receive()
		s.beConn.SetReadDeadline(time.Time{})
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return s.cli.Flush()
			}
			return err
		}
		s.cli.Send(m)
	}
}

func isWrite(tag []byte) bool {
	w, _, _ := strings.Cut(string(tag), " ")
	switch w {
	case "SELECT", "SHOW", "BEGIN", "COMMIT", "ROLLBACK", "SET", "RESET", "FETCH", "DECLARE", "CLOSE", "DISCARD", "DEALLOCATE", "PREPARE", "LISTEN", "UNLISTEN", "SAVEPOINT", "RELEASE", "EXPLAIN":
		return false
	}
	return true
}

// backendQuery runs a statement of the gateway's own on the session's
// backend between client statements; nothing reaches the client.
func (s *session) backendQuery(sql string) ([][][]byte, error) {
	s.be.Send(&pgproto3.Query{String: sql})
	if err := s.be.Flush(); err != nil {
		return nil, err
	}
	var rows [][][]byte
	var qerr error
	for {
		m, err := s.be.Receive()
		if err != nil {
			return nil, err
		}
		switch m := m.(type) {
		case *pgproto3.DataRow:
			row := make([][]byte, len(m.Values))
			for i, v := range m.Values {
				if v != nil {
					row[i] = append([]byte{}, v...)
				}
			}
			rows = append(rows, row)
		case *pgproto3.ErrorResponse:
			qerr = errors.New(m.Message)
		case *pgproto3.ReadyForQuery:
			s.tx = m.TxStatus
			return rows, qerr
		}
	}
}

func (s *session) backendExec(sql string) error { _, err := s.backendQuery(sql); return err }

// Report lists the shapes and where they run (for the metrics endpoint).
func (r *Router) Report(w io.Writer) {
	fmt.Fprintf(w, "W=%s fresh=%v pg=%d ch=%d stale_to_pg=%d verify_ok=%d verify_diff=%d\n", pglogrepl.LSN(r.sync.W.Load()), r.fresh(),
		r.Stats.PG.Load(), r.Stats.CH.Load(), r.Stats.Stale.Load(), r.Stats.VerifyOK.Load(), r.Stats.VerifyDiff.Load())
	r.shapes.Range(func(k, v any) bool {
		sh := v.(*shape)
		sh.mu.Lock()
		st := [...]string{"new", "verifying", "clickhouse", "postgresql"}[sh.state]
		times := ""
		if n := len(sh.chMs); n > 0 {
			v := slices.Sorted(slices.Values(sh.chMs))
			times = fmt.Sprintf(" ch_ms p50=%.0f p99=%.0f max=%.0f", v[n/2], v[min(n-1, n*99/100)], v[n-1])
		}
		retry := ""
		if sh.retries > 0 {
			retry = fmt.Sprintf(" retries=%d (%s)", sh.retries, sh.lastRetry)
		}
		fmt.Fprintf(w, "%-11s runs=%d ch=%d%s %s%s | %s\n", st, sh.runs.Load(), sh.chRuns.Load(), times, sh.reason, retry, clip(k.(string)))
		sh.mu.Unlock()
		return true
	})
}
