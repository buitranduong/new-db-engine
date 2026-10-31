package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var execMode = pgx.QueryExecModeSimpleProtocol // simple protocol: the least a PostgreSQL-wire target must support
var isoLevel = pgx.TxIsoLevel("")

func Connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.DefaultQueryExecMode = execMode
	cfg.RuntimeParams["TimeZone"] = "UTC"
	cfg.ConnectTimeout = 5 * time.Second
	return pgx.ConnectConfig(ctx, cfg)
}

// Sess is one reconnecting session.
type Sess struct {
	dsn  string
	conn *pgx.Conn
}

func (s *Sess) Conn(ctx context.Context) (*pgx.Conn, error) {
	if s.conn != nil && !s.conn.IsClosed() {
		return s.conn, nil
	}
	c, err := Connect(ctx, s.dsn)
	if err != nil {
		return nil, err
	}
	s.conn = c
	return c, nil
}

func (s *Sess) Drop() {
	if s.conn != nil {
		s.conn.Close(context.Background())
		s.conn = nil
	}
}

// ErrCode returns the SQLSTATE, or "conn" for connection-level failures, or "other".
func ErrCode(err error) string {
	if err == nil {
		return ""
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	var ne net.Error
	if errors.As(err, &ne) || pgconn.SafeToRetry(err) || strings.Contains(err.Error(), "EOF") ||
		strings.Contains(err.Error(), "conn closed") || strings.Contains(err.Error(), "connection") ||
		errors.Is(err, context.DeadlineExceeded) {
		return "conn"
	}
	return "other"
}

func Retryable(err error) bool { c := ErrCode(err); return c == "40001" || c == "40P01" }

// errUnknown: the COMMIT was sent but its outcome is unknown (connection lost).
var errUnknown = errors.New("commit outcome unknown")

type TxResult struct {
	Attempts int
	Retries  int
	Noop     bool
	Err      error
	Unknown  bool
	Codes    map[string]int // sqlstates seen on failed attempts
}

// txContext detaches a live transaction from the end of the run: a transaction that was sent to the server must be
// allowed to finish (commit or fail) before the final checks read the journal and the tables, otherwise a COMMIT that
// the client abandoned at the deadline lands on the server between those reads (B2: an order that "has no journal row").
// A crash is not affected: the connection error ends the attempt.
func txContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
}

// RunTx runs fn in a transaction with retry on 40001/40P01 (and on connection loss before COMMIT, after reconnecting).
// fn returns commit=false for a no-op (rolled back).
func (s *Sess) RunTx(ctx context.Context, maxAttempts int, fn func(ctx context.Context, tx pgx.Tx) (bool, error)) TxResult {
	r := TxResult{Codes: map[string]int{}}
	for r.Attempts < maxAttempts {
		if ctx.Err() != nil {
			r.Err = ctx.Err()
			return r
		}
		r.Attempts++
		c, err := s.Conn(ctx)
		if err != nil {
			r.Codes["conn"]++
			r.Err = err
			time.Sleep(200 * time.Millisecond)
			if r.Attempts >= 3 && r.Attempts < maxAttempts {
				r.Attempts-- // connection retries do not consume the conflict budget (bounded by ctx)
			}
			continue
		}
		tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: isoLevel})
		if err != nil {
			r.Codes[ErrCode(err)]++
			r.Err = err
			s.Drop()
			continue
		}
		commit, err := fn(ctx, tx)
		if err == nil && !commit {
			tx.Rollback(ctx)
			r.Noop, r.Err = true, nil
			return r
		}
		if err == nil {
			err = tx.Commit(ctx)
			if err != nil {
				if ErrCode(err) == "conn" {
					s.Drop()
					r.Unknown, r.Err = true, err
					return r
				}
			} else {
				r.Err = nil
				return r
			}
		} else {
			rbctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			tx.Rollback(rbctx)
			cancel()
		}
		r.Err = err
		if ctx.Err() != nil {
			return r
		}
		code := ErrCode(err)
		r.Codes[code]++
		if code == "conn" {
			s.Drop()
		}
		if code == "40001" || code == "40P01" || code == "conn" {
			r.Retries++
			time.Sleep(time.Duration(1+r.Retries) * 2 * time.Millisecond)
			continue
		}
		return r
	}
	return r
}

// ---------------------------------------------------------------------------------------------------------------
// Transaction kinds. Every transaction is a pure function of its Payload and the database state, so that the committed
// journal can be replayed on another PostgreSQL-wire server (the barrier comparison).

type Payload struct {
	Kind    string   `json:"k"`
	TS      string   `json:"ts"`
	OrderID int64    `json:"o,omitempty"`
	Cust    int      `json:"c,omitempty"`
	Store   int      `json:"s,omitempty"`
	Nums    []string `json:"n,omitempty"` // candidate sims (order) / probe sim / import range [a,b)
	Slice   int      `json:"sl,omitempty"`
	Val     int      `json:"v,omitempty"`
	// outcome
	Got   []string `json:"g,omitempty"`
	Total int64    `json:"t,omitempty"`
	Rows  int64    `json:"r,omitempty"`
}

type grp struct {
	store int
	cat   string
	d     int
}

func addGrp(gs []grp, store int, cat string) []grp {
	for i := range gs {
		if gs[i].store == store && gs[i].cat == cat {
			gs[i].d++
			return gs
		}
	}
	return append(gs, grp{store, cat, 1})
}

var seqNo atomic.Int64

func parseTS(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
func fmtTS(t time.Time) string   { return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) }

// Apply runs one transaction body. live=true also takes a sequence number and writes the journal row (last statements before
// COMMIT, so the hot row is held only for the commit). It fills the outcome fields of p. commit=false means no-op.
func Apply(ctx context.Context, tx pgx.Tx, p *Payload, live bool, txid int64) (bool, error) {
	t := parseTS(p.TS)
	p.Got, p.Total, p.Rows = nil, 0, 0
	var gs []grp
	switch p.Kind {
	case "order":
		var prices []int
		for _, n := range p.Nums {
			var st, pr int
			var cat string
			err := tx.QueryRow(ctx, "UPDATE sims SET status = 'reserved', updated_at = $1 WHERE number = $2 AND status = 'available' RETURNING store_id, category, price", t, n).Scan(&st, &cat, &pr)
			if err == pgx.ErrNoRows {
				continue
			}
			if err != nil {
				return false, err
			}
			p.Got = append(p.Got, n)
			prices = append(prices, pr)
			p.Total += int64(pr)
			gs = addGrp(gs, st, cat)
		}
		if len(p.Got) == 0 {
			return false, nil
		}
		if err := bumpStock(ctx, tx, gs, +1); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO orders (id, customer_id, store_id, status, created_at, total) VALUES ($1, $2, $3, 'open', $4, $5)", p.OrderID, p.Cust, p.Store, t, p.Total); err != nil {
			return false, err
		}
		for i, n := range p.Got {
			if _, err := tx.Exec(ctx, "INSERT INTO order_items (id, order_id, sim_number, price) VALUES ($1, $2, $3, $4)", p.OrderID*10+int64(i), p.OrderID, n, prices[i]); err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO history (id, sim_number, changed_at, field, old_value, new_value) VALUES ($1, $2, $3, 'status', 'available', 'reserved')", tHistBase+(p.OrderID-tOrderBase)*10+int64(i), n, t); err != nil {
				return false, err
			}
		}
	case "pay", "cancel":
		newO, newS, hv := "paid", "sold", "sold"
		if p.Kind == "cancel" {
			newO, newS, hv = "cancelled", "available", "available"
		}
		ct, err := tx.Exec(ctx, "UPDATE orders SET status = '"+newO+"' WHERE id = $1 AND status = 'open'", p.OrderID)
		if err != nil {
			return false, err
		}
		if ct.RowsAffected() == 0 {
			return false, nil
		}
		rows, err := tx.Query(ctx, "SELECT sim_number FROM order_items WHERE order_id = $1 ORDER BY id", p.OrderID)
		if err != nil {
			return false, err
		}
		var items []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return false, err
			}
			items = append(items, n)
		}
		rows.Close()
		if rows.Err() != nil {
			return false, rows.Err()
		}
		for i, n := range items {
			var st int
			var cat string
			err := tx.QueryRow(ctx, "UPDATE sims SET status = '"+newS+"', updated_at = $1 WHERE number = $2 AND status = 'reserved' RETURNING store_id, category", t, n).Scan(&st, &cat)
			if err == pgx.ErrNoRows {
				return false, fmt.Errorf("anomaly: sim %s of open order %d is not reserved", n, p.OrderID)
			}
			if err != nil {
				return false, err
			}
			p.Got = append(p.Got, n)
			gs = addGrp(gs, st, cat)
			if _, err := tx.Exec(ctx, "INSERT INTO history (id, sim_number, changed_at, field, old_value, new_value) VALUES ($1, $2, $3, 'status', 'reserved', '"+hv+"')", tHistBase+txid*10+int64(i), n, t); err != nil {
				return false, err
			}
		}
		if err := bumpStock(ctx, tx, gs, -1); err != nil {
			return false, err
		}
	case "probe":
		ct, err := tx.Exec(ctx, "UPDATE sims SET store_rate = $1 WHERE number = $2", p.Val, p.Nums[0])
		if err != nil {
			return false, err
		}
		p.Rows = ct.RowsAffected()
	case "import":
		// re-price a 1% slice of the sims in ONE transaction and write history for it (docs/30 §7)
		ct, err := tx.Exec(ctx, "UPDATE sims SET price = 500000 + ((price * 7 + $1) % 4000000), updated_at = $2 WHERE number >= $3 AND number < $4", p.Slice, t, p.Nums[0], p.Nums[1])
		if err != nil {
			return false, err
		}
		p.Rows = ct.RowsAffected()
		if _, err := tx.Exec(ctx, "INSERT INTO history (id, sim_number, changed_at, field, old_value, new_value) SELECT $1 + CAST(substr(number, 3) AS bigint), number, $2, 'price', NULL, CAST(price AS text) FROM sims WHERE number >= $3 AND number < $4", p.OrderID, t, p.Nums[0], p.Nums[1]); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unknown kind %s", p.Kind)
	}
	if live {
		var seq int64
		if err := tx.QueryRow(ctx, "UPDATE m2_seq SET v = v + 1 WHERE id = 1 RETURNING v").Scan(&seq); err != nil {
			return false, err
		}
		js := mustJSON(p)
		if _, err := tx.Exec(ctx, "INSERT INTO m2_journal (seq, txid, kind, payload) VALUES ($1, $2, $3, $4)", seq, txid, p.Kind, js); err != nil {
			return false, err
		}
	}
	return true, nil
}

func bumpStock(ctx context.Context, tx pgx.Tx, gs []grp, sign int) error {
	for _, g := range gs {
		ct, err := tx.Exec(ctx, "UPDATE stock SET reserved = reserved + $1 WHERE store_id = $2 AND category = $3", sign*g.d, g.store, g.cat)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return fmt.Errorf("stock row (%d,%s) missing", g.store, g.cat)
		}
	}
	return nil
}

func mustJSON(v any) string {
	b, err := jsonMarshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
