package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"stask/internal/chsync"
)

// state of a query shape (docs/35 section 6)
const (
	shapeNew       = iota
	shapeVerifying // cost says ClickHouse; answered by PostgreSQL while results are compared
	shapeCH        // every comparison identical: answered by ClickHouse
	shapePG        // cheap, untranslatable, or once different: PostgreSQL for good
)

type shape struct {
	mu        sync.Mutex
	state     int
	reason    string
	cost      float64
	matches   int
	fails     int
	checking  int                         // comparisons running (at most maxChecks)
	fields    []pgproto3.FieldDescription // PostgreSQL's description of the result
	runs      atomic.Int64
	chRuns    atomic.Int64
	retries   int // comparisons without a verdict, and the last reason
	lastRetry string
	chMs      []float64 // gateway-side time of ClickHouse answers (ms), the last 10000
}

// Router holds what all sessions share: the shapes, the sync it runs
// (chsync, in process: docs/35 section 7) and the connections used to
// verify shapes.
type Router struct {
	cfg    Config
	ch     *chsync.CH
	shapes sync.Map      // shape text -> *shape
	gate   chsync.Gate   // held for writing by chsync while it applies a batch
	pin    atomic.Uint64 // chsync stops before the first transaction after it (comparisons)
	pinMu  sync.Mutex    // one pinned comparison at a time
	sync   chsync.Stats
	verify chan *pgconn.PgConn
	Stats  Stats
}

type Stats struct {
	PG, CH, Fresh, Stale, VerifyOK, VerifyDiff atomic.Int64
}

func (r *Router) shapeOf(s string) *shape {
	v, _ := r.shapes.LoadOrStore(s, &shape{})
	return v.(*shape)
}

// runSync keeps ClickHouse in step with PostgreSQL for as long as the
// gateway runs (reconnecting after errors, stopping on a fatal one).
func (r *Router) runSync(ctx context.Context) {
	cfg := chsync.Config{SourceURL: r.cfg.PGURL, CHURL: r.cfg.CHURL, Tables: r.cfg.Tables, Slot: r.cfg.Slot, Reset: r.cfg.Reset,
		Gate: &r.gate, Pin: &r.pin, Stats: &r.sync, Logf: func(f string, a ...any) { r.cfg.Logf("chsync: "+f, a...) }}
	for backoff := time.Second; ctx.Err() == nil; {
		err := chsync.Run(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if chsync.IsFatal(err) {
			r.cfg.Logf("chsync stopped: %v (every read now goes to PostgreSQL)", err)
			return
		}
		cfg.Reset = false
		r.cfg.Logf("chsync: %v; again in %s", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// fresh: chsync was caught up with the source recently.
func (r *Router) fresh() bool {
	at := r.sync.CaughtUp.Load()
	return at != 0 && time.Since(time.Unix(0, at)) < r.cfg.MaxStale
}

// waitW waits until W >= lsn, at most d.
func (r *Router) waitW(lsn uint64, d time.Duration) bool {
	end := time.Now().Add(d)
	for r.sync.W.Load() < lsn {
		if time.Now().After(end) {
			return false
		}
		time.Sleep(r.cfg.PollEvery / 2)
	}
	return true
}

type explainOut []struct {
	Plan struct {
		TotalCost float64 `json:"Total Cost"`
	} `json:"Plan"`
}

// classify decides a new shape from PostgreSQL's own estimate (EXPLAIN does
// not run the query) and from whether it translates.
func (r *Router) classify(ctx context.Context, sess *session, sh *shape, sql string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.state != shapeNew {
		return
	}
	if _, err := Translate(sql); err != nil {
		sh.state, sh.reason = shapePG, "translate: "+err.Error()
		return
	}
	rows, err := sess.backendQuery("EXPLAIN (FORMAT JSON) " + sql)
	if err != nil || len(rows) == 0 {
		sh.state, sh.reason = shapePG, fmt.Sprintf("explain: %v", err)
		return
	}
	var ex explainOut
	if err := json.Unmarshal(rows[0][0], &ex); err != nil || len(ex) == 0 {
		sh.state, sh.reason = shapePG, "explain output"
		return
	}
	sh.cost = ex[0].Plan.TotalCost
	if sh.cost < r.cfg.CostThreshold {
		sh.state, sh.reason = shapePG, fmt.Sprintf("cheap on PostgreSQL (cost %.0f)", sh.cost)
		return
	}
	sh.state, sh.reason = shapeVerifying, fmt.Sprintf("cost %.0f", sh.cost)
}

// maxChecks comparisons of one shape may run at once: an expensive shape
// needs VerifyRuns of them before ClickHouse may answer it, each running the
// query on PostgreSQL.
const maxChecks = 2

// startVerify compares one execution in the background: PostgreSQL in a
// REPEATABLE READ snapshot at LSN L, ClickHouse with exactly the
// transactions committed up to L applied (chsync pinned at L).
func (r *Router) startVerify(sh *shape, sql string) {
	sh.mu.Lock()
	if sh.checking >= maxChecks {
		sh.mu.Unlock()
		return
	}
	sh.checking++
	sh.mu.Unlock()
	go func() {
		same, why := r.compare(sh, sql)
		sh.mu.Lock()
		defer sh.mu.Unlock()
		sh.checking--
		switch {
		case same:
			r.Stats.VerifyOK.Add(1)
			sh.matches++
			if sh.state == shapeVerifying && sh.matches >= r.cfg.VerifyRuns {
				sh.state, sh.reason = shapeCH, fmt.Sprintf("%d identical comparisons (cost %.0f)", sh.matches, sh.cost)
				r.cfg.Logf("shape -> ClickHouse: %s", clip(Shape(sql)))
			}
		case strings.HasPrefix(why, "retry"):
			sh.retries++
			sh.lastRetry = why
		default:
			r.Stats.VerifyDiff.Add(1)
			sh.fails++
			if sh.fails >= 2 || sh.state == shapeCH { // one retry for the race between snapshot and LSN
				sh.state, sh.reason = shapePG, "result differs: "+why
				r.cfg.Logf("shape -> PostgreSQL (%s): %s", why, clip(Shape(sql)))
			}
		}
	}()
}

func (r *Router) compare(sh *shape, sql string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.VerifyTimeout)
	defer cancel()
	var c *pgconn.PgConn
	select {
	case c = <-r.verify:
	case <-ctx.Done():
		return false, "retry: no comparison connection free"
	}
	defer func() { r.verify <- c }()
	defer func() { c.Exec(context.Background(), "ROLLBACK").ReadAll() }()
	// The snapshot is taken by the first statement, which also reads L; the
	// query itself runs on that snapshot after ClickHouse answered, so chsync
	// is held only for the ClickHouse query.
	res, err := c.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SELECT pg_current_wal_lsn()::text").ReadAll()
	if err != nil || len(res) < 2 {
		return false, "retry: PostgreSQL snapshot: " + clip(fmt.Sprint(err)) // PostgreSQL could not answer: no verdict on ClickHouse
	}
	l, _ := pglogrepl.ParseLSN(string(res[1].Rows[0][0]))
	lsn := uint64(l)
	sh.mu.Lock()
	fields := sh.fields
	sh.mu.Unlock()
	if fields == nil { // the description of the result, from PostgreSQL itself
		d, err := c.Prepare(ctx, "", sql, nil)
		if err != nil {
			return false, "retry: PostgreSQL describe: " + clip(fmt.Sprint(err))
		}
		for _, f := range d.Fields {
			fields = append(fields, pgproto3.FieldDescription{Name: []byte(f.Name), TableOID: f.TableOID,
				TableAttributeNumber: f.TableAttributeNumber, DataTypeOID: f.DataTypeOID, DataTypeSize: f.DataTypeSize,
				TypeModifier: f.TypeModifier, Format: 0})
		}
		sh.mu.Lock()
		sh.fields = fields
		sh.mu.Unlock()
	}
	chRows, why := r.chAt(ctx, sql, lsn, fields)
	if why != "" {
		return false, why
	}
	res, err = c.Exec(ctx, sql).ReadAll()
	if err != nil || len(res) < 1 {
		return false, "retry: PostgreSQL query: " + clip(fmt.Sprint(err))
	}
	pgRes := res[0]
	a, b := pgRes.Rows, chRows
	if !orderedQuery(sql) {
		a, b = sortRows(a), sortRows(b)
	}
	if len(a) != len(b) {
		return false, fmt.Sprintf("%d rows on PostgreSQL, %d on ClickHouse", len(a), len(b))
	}
	for i := range a {
		for j := range a[i] {
			if (a[i][j] == nil) != (b[i][j] == nil) || !bytes.Equal(a[i][j], b[i][j]) {
				return false, fmt.Sprintf("row %d column %s: PostgreSQL %q, ClickHouse %q", i, fields[j].Name, a[i][j], b[i][j])
			}
		}
	}
	return true, ""
}

// chAt runs sql on ClickHouse holding exactly the transactions committed up
// to lsn: chsync is pinned at lsn, applies what comes before it and stops;
// "retry: .." when that state cannot be had (chsync was already past lsn).
func (r *Router) chAt(ctx context.Context, sql string, lsn uint64, fields []pgproto3.FieldDescription) ([][][]byte, string) {
	r.pinMu.Lock()
	defer r.pinMu.Unlock()
	r.pin.Store(lsn)
	defer r.pin.Store(0)
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
		if r.sync.Applied.Load() > lsn || time.Now().After(end) {
			return nil, "retry: chsync already past the snapshot"
		}
		if r.sync.W.Load() >= lsn || r.sync.Held.Load() > lsn {
			break
		}
	}
	rows, err := r.runCH(ctx, sql, fields, func() bool {
		return r.sync.Applied.Load() <= lsn && (r.sync.W.Load() >= lsn || r.sync.Held.Load() > lsn)
	})
	if err == errMoved {
		return nil, "retry: ClickHouse moved past the snapshot"
	}
	if err != nil {
		return nil, "ClickHouse: " + clip(err.Error())
	}
	return rows, ""
}

var errMoved = errors.New("ClickHouse moved past the snapshot")

// runCH answers sql from ClickHouse while holding the gate for reading:
// no batch is half applied while it runs. ok, if given, must still hold
// once the gate is held (else errMoved).
func (r *Router) runCH(ctx context.Context, sql string, fields []pgproto3.FieldDescription, ok func() bool) ([][][]byte, error) {
	q, err := Translate(sql)
	if err != nil {
		return nil, err
	}
	r.gate.RLock()
	defer r.gate.RUnlock()
	if ok != nil && !ok() {
		return nil, errMoved
	}
	rc, err := r.ch.Stream(ctx, q+" "+chSettings+" FORMAT TabSeparated")
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readCH(rc, fields)
}

func orderedQuery(sql string) bool {
	return strings.Contains(strings.ToLower(Shape(sql)), "order by")
}

func sortRows(rows [][][]byte) [][][]byte {
	out := slices.Clone(rows)
	key := func(r [][]byte) string {
		var b strings.Builder
		for _, v := range r {
			if v == nil {
				b.WriteString("\x00N")
			} else {
				b.Write(v)
			}
			b.WriteByte(0xff)
		}
		return b.String()
	}
	slices.SortFunc(out, func(x, y [][]byte) int { return strings.Compare(key(x), key(y)) })
	return out
}

// sample: an answered-from-ClickHouse shape is compared again now and then.
func (r *Router) sample() bool { return rand.Float64() < r.cfg.Resample }

func clip(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}
