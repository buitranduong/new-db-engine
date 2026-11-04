package gateway

import (
	"bytes"
	"context"
	"encoding/json"
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
	mu       sync.Mutex
	state    int
	reason   string
	cost     float64
	matches  int
	fails    int
	checking bool
	fields   []pgproto3.FieldDescription // PostgreSQL's description of the result
	runs     atomic.Int64
	chRuns   atomic.Int64
}

// Router holds what all sessions share: the shapes, the watermark and the
// connections used to verify shapes.
type Router struct {
	cfg    Config
	ch     *chsync.CH
	tables map[string]bool
	shapes sync.Map // shape text -> *shape
	w      atomic.Uint64
	wAt    atomic.Int64 // unix nanos of the last W read
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

// pollW keeps the watermark of chsync current (every PollEvery).
func (r *Router) pollW(ctx context.Context) {
	for ctx.Err() == nil {
		if w, ok, err := chsync.ReadW(ctx, r.ch, r.cfg.Slot); err == nil && ok {
			r.w.Store(w)
			r.wAt.Store(time.Now().UnixNano())
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.cfg.PollEvery):
		}
	}
}

// fresh: the watermark is known and recent (chsync advances it at least once
// a second even when the source is idle).
func (r *Router) fresh() bool {
	at := r.wAt.Load()
	return at != 0 && time.Since(time.Unix(0, at)) < r.cfg.MaxStale
}

// waitW waits until W >= lsn, at most d.
func (r *Router) waitW(lsn uint64, d time.Duration) bool {
	end := time.Now().Add(d)
	for r.w.Load() < lsn {
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
	if _, err := Translate(sql, r.tables, 0); err != nil {
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

// startVerify compares one execution in the background: PostgreSQL in a
// REPEATABLE READ snapshot at LSN L, ClickHouse at w = L once W >= L.
func (r *Router) startVerify(sh *shape, sql string) {
	sh.mu.Lock()
	if sh.checking {
		sh.mu.Unlock()
		return
	}
	sh.checking = true
	sh.mu.Unlock()
	go func() {
		same, why := r.compare(sh, sql)
		sh.mu.Lock()
		defer sh.mu.Unlock()
		sh.checking = false
		switch {
		case same:
			r.Stats.VerifyOK.Add(1)
			sh.matches++
			if sh.state == shapeVerifying && sh.matches >= r.cfg.VerifyRuns {
				sh.state, sh.reason = shapeCH, fmt.Sprintf("%d identical comparisons (cost %.0f)", sh.matches, sh.cost)
				r.cfg.Logf("shape -> ClickHouse: %s", clip(Shape(sql)))
			}
		case why == "retry":
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var c *pgconn.PgConn
	select {
	case c = <-r.verify:
	case <-ctx.Done():
		return false, "retry"
	}
	defer func() { r.verify <- c }()
	res, err := c.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SELECT pg_current_wal_lsn()::text; "+sql+"; COMMIT").ReadAll()
	if err != nil || len(res) < 3 {
		c.Exec(ctx, "ROLLBACK").ReadAll()
		return false, fmt.Sprintf("PostgreSQL: %v", err)
	}
	lsn, _ := pglogrepl.ParseLSN(string(res[1].Rows[0][0]))
	pgRes := res[2]
	sh.mu.Lock()
	if sh.fields == nil {
		for _, f := range pgRes.FieldDescriptions {
			sh.fields = append(sh.fields, pgproto3.FieldDescription{Name: []byte(f.Name), TableOID: f.TableOID,
				TableAttributeNumber: f.TableAttributeNumber, DataTypeOID: f.DataTypeOID, DataTypeSize: f.DataTypeSize,
				TypeModifier: f.TypeModifier, Format: 0})
		}
	}
	fields := sh.fields
	sh.mu.Unlock()
	if !r.waitW(uint64(lsn), 30*time.Second) {
		return false, "retry"
	}
	chRows, err := r.runCH(ctx, sql, uint64(lsn), fields)
	if err != nil {
		return false, "ClickHouse: " + clip(err.Error())
	}
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

func (r *Router) runCH(ctx context.Context, sql string, w uint64, fields []pgproto3.FieldDescription) ([][][]byte, error) {
	q, err := Translate(sql, r.tables, w)
	if err != nil {
		return nil, err
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
