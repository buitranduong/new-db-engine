package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Counts is what a run did to the table.
type Counts struct {
	Rows       int64 `json:"rows"`       // data rows read
	Invalid    int64 `json:"invalid"`    // rejected by validation (see the error report)
	Duplicates int64 `json:"duplicates"` // repeated key within one batch: the last row wins
	Inserted   int64 `json:"inserted"`
	Updated    int64 `json:"updated"`
	Unchanged  int64 `json:"unchanged"` // matched the table: nothing written
	Failed     int64 `json:"failed"`    // valid rows the database refused (see the error report)
	History    int64 `json:"history_rows"`
	Batches    int64 `json:"batches"`
	Retries    int64 `json:"retries"`
}

func (c *Counts) add(o Counts) {
	c.Rows += o.Rows
	c.Invalid += o.Invalid
	c.Duplicates += o.Duplicates
	c.Inserted += o.Inserted
	c.Updated += o.Updated
	c.Unchanged += o.Unchanged
	c.Failed += o.Failed
	c.History += o.History
	c.Batches += o.Batches
	c.Retries += o.Retries
}

// vrow is a validated row: vals follow Spec.Columns.
type vrow struct {
	n    int64
	line int64
	key  any
	vals []any
}

// errRec is one line of the error report.
type errRec struct {
	Batch  int64
	Line   int64
	Column string
	Value  string
	Reason string
}

type applier struct {
	sp      *Spec
	url     string
	conn    *pgx.Conn
	dry     bool
	keyType string
	cols    []string // non-key spec columns, in spec order
	sel     string   // SELECT .. WHERE key IN (
	selTail string
	ins     string // INSERT ... prefix and ON CONFLICT suffix pieces
	retries int
	now     func() time.Time
}

func newApplier(url string, sp *Spec, dry bool) *applier {
	a := &applier{sp: sp, url: url, dry: dry, now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }, retries: 8}
	a.keyType = sp.col(sp.Key).Type
	names := []string{sp.Key}
	for _, c := range sp.Columns {
		if c.Name != sp.Key {
			a.cols = append(a.cols, c.Name)
			names = append(names, c.Name)
		}
	}
	a.sel = fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (", strings.Join(names, ", "), sp.Table, sp.Key)
	a.selTail = " ORDER BY " + sp.Key
	if !dry {
		a.selTail += " FOR UPDATE"
	}
	return a
}

func (a *applier) connect(ctx context.Context) error {
	if a.conn != nil && !a.conn.IsClosed() {
		return nil
	}
	c, err := pgx.Connect(ctx, a.url)
	if err != nil {
		return err
	}
	a.conn = c
	return nil
}

func (a *applier) close(ctx context.Context) {
	if a.conn != nil {
		a.conn.Close(ctx)
	}
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "40001", "40P01", "55P03", "53300", "57014", "08006", "08003", "57P01":
			return true
		}
		return false
	}
	// not a database answer: a broken or timed-out connection
	return true
}

// norm makes a value read from the table comparable with a parsed cell.
func norm(v any) any {
	switch x := v.(type) {
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float32:
		return float64(x)
	case time.Time:
		return x.UTC().Truncate(time.Microsecond)
	case []byte:
		return string(x)
	}
	return v
}

func same(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if x, ok := a.(time.Time); ok {
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	}
	return a == b
}

const lookupChunk = 250

type change struct {
	row vrow
	old []any // nil: the row did not exist
}

// apply classifies the rows against the table and, unless dry, writes the
// inserted and changed rows plus their history rows in ONE short transaction
// (rows locked in key order). rows have distinct keys.
func (a *applier) apply(ctx context.Context, rows []vrow) (res Counts, err error) {
	sort.Slice(rows, func(i, j int) bool { return lessKey(rows[i].key, rows[j].key) })
	for attempt := 0; ; attempt++ {
		res, err = a.applyOnce(ctx, rows)
		res.Retries = int64(attempt)
		if err == nil || !retryable(err) || attempt >= a.retries || ctx.Err() != nil {
			return res, err
		}
		d := time.Duration(20<<min(attempt, 6)) * time.Millisecond
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(d/2 + time.Duration(rand.Int64N(int64(d)))):
		}
	}
}

func lessKey(x, y any) bool {
	switch a := x.(type) {
	case int64:
		return a < y.(int64)
	case float64:
		return a < y.(float64)
	}
	return fmt.Sprint(x) < fmt.Sprint(y)
}

func (a *applier) applyOnce(ctx context.Context, rows []vrow) (res Counts, err error) {
	if err = a.connect(ctx); err != nil {
		return res, err
	}
	tx, err := a.conn.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			rb, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			tx.Rollback(rb)
			cancel()
			if a.conn.IsClosed() {
				a.conn = nil
			}
		}
	}()
	existing := make(map[string][]any, len(rows)) // key -> values in a.cols order
	// ANY($1) would scan the table and re-parse the array per row (measured), so look the
	// keys up with IN lists, lookupChunk at a time (IN cost grows faster than linearly).
	for lo := 0; lo < len(rows); lo += lookupChunk {
		chunk := rows[lo:min(lo+lookupChunk, len(rows))]
		var sb strings.Builder
		sb.WriteString(a.sel)
		args := make([]any, len(chunk))
		for i, r := range chunk {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "$%d", i+1)
			args[i] = r.key
		}
		sb.WriteByte(')')
		sb.WriteString(a.selTail)
		rs, qerr := tx.Query(ctx, sb.String(), args...)
		if qerr != nil {
			return res, qerr
		}
		for rs.Next() {
			vs, verr := rs.Values()
			if verr != nil {
				rs.Close()
				return res, verr
			}
			for i := range vs {
				vs[i] = norm(vs[i])
			}
			existing[fmt.Sprint(vs[0])] = vs[1:]
		}
		rs.Close()
		if err = rs.Err(); err != nil {
			return res, err
		}
	}
	// position of each non-key spec column in vrow.vals
	var changes []change
	for _, r := range rows {
		old, found := existing[fmt.Sprint(r.key)]
		final := make([]any, len(a.cols))
		k := 0
		for i, c := range a.sp.Columns {
			if c.Name == a.sp.Key {
				continue
			}
			v := r.vals[i]
			if found {
				if v == nil && c.KeepOldNull || containsFold(c.KeepOldWhen, old[k]) {
					v = old[k]
				}
			}
			final[k] = v
			k++
		}
		r.vals = append(r.vals[:0:0], final...) // vals now: the non-key columns after keep rules
		if !found {
			res.Inserted++
			changes = append(changes, change{row: r})
			continue
		}
		diff := false
		for i := range final {
			if !same(final[i], old[i]) {
				diff = true
				break
			}
		}
		if !diff {
			res.Unchanged++
			continue
		}
		res.Updated++
		changes = append(changes, change{row: r, old: old})
	}
	if a.dry || len(changes) == 0 {
		return res, tx.Rollback(ctx)
	}
	now := a.now()
	if err = a.upsert(ctx, tx, changes, now); err != nil {
		return res, err
	}
	if a.sp.History != nil {
		if err = a.writeHistory(ctx, tx, changes, now); err != nil {
			return res, err
		}
		res.History = int64(len(changes))
	}
	if err = tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}

func containsFold(list []string, v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func (a *applier) upsert(ctx context.Context, tx pgx.Tx, ch []change, now time.Time) error {
	names := append([]string{a.sp.Key}, a.cols...)
	var touch []string
	for c := range a.sp.Touch {
		touch = append(touch, c)
	}
	sort.Strings(touch)
	names = append(names, touch...)
	var sb strings.Builder
	fmt.Fprintf(&sb, "INSERT INTO %s (%s) VALUES ", a.sp.Table, strings.Join(names, ", "))
	args := make([]any, 0, len(ch)*len(names))
	for i, c := range ch {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		args = append(args, c.row.key)
		args = append(args, c.row.vals...)
		for range touch {
			args = append(args, now)
		}
		for j := range names {
			if j > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "$%d", i*len(names)+j+1)
		}
		sb.WriteByte(')')
	}
	fmt.Fprintf(&sb, " ON CONFLICT (%s) DO UPDATE SET ", a.sp.Key)
	for i, n := range names[1:] {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s = excluded.%s", n, n)
	}
	if len(names) == 1 {
		return nil // key-only table: nothing to update
	}
	_, err := tx.Exec(ctx, sb.String(), args...)
	return err
}

func (a *applier) writeHistory(ctx context.Context, tx pgx.Tx, ch []change, now time.Time) error {
	h := a.sp.History
	names := []string{h.KeyColumn, h.AtColumn}
	var extra []string
	for c := range h.Extra {
		extra = append(extra, c)
	}
	sort.Strings(extra)
	names = append(names, extra...)
	pos := map[string]int{}
	for i, c := range a.cols {
		pos[c] = i
	}
	for _, c := range h.Columns {
		names = append(names, h.OldPrefix+c, h.NewPrefix+c)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "INSERT INTO %s (%s) VALUES ", h.Table, strings.Join(names, ", "))
	args := make([]any, 0, len(ch)*len(names))
	for i, c := range ch {
		if i > 0 {
			sb.WriteByte(',')
		}
		args = append(args, c.row.key, now)
		for _, e := range extra {
			args = append(args, h.Extra[e])
		}
		for _, col := range h.Columns {
			var o any
			if c.old != nil {
				o = c.old[pos[col]]
			}
			args = append(args, o, c.row.vals[pos[col]])
		}
		sb.WriteByte('(')
		for j := range names {
			if j > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "$%d", i*len(names)+j+1)
		}
		sb.WriteByte(')')
	}
	_, err := tx.Exec(ctx, sb.String(), args...)
	return err
}
