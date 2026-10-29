package sql

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/schema"
)

// Per-row repair of a long autocommit write (docs/PROCESS.md D24).
//
// A statement that writes many rows computes them all on one snapshot and
// commits once. Under first-committer-wins any row changed by a small
// concurrent transaction in the meantime fails the commit, and rerunning the
// whole statement fails again for the same reason. For INSERT .. ON CONFLICT
// DO UPDATE / DO NOTHING, UPDATE (also .. FROM) and DELETE without RETURNING
// we instead do what PostgreSQL's READ COMMITTED does per row: the commit
// reports the rows that changed (core.Tx.CommitRepairable), the statement is
// evaluated again for those rows only, against their newest versions (on a
// fresh snapshot, restricted to those keys), and the new writes replace the
// old ones (core.Tx.RepairFrom). Rows that no longer match are dropped.
//
// Not covered (they keep the whole-statement retry, now capped for big
// statements): tables with triggers, foreign keys, or referenced by one;
// RETURNING; statements that change the key; non int/text keys.

// repairMinRows is the number of writes from which a statement is repaired
// instead of rerun (smaller ones are cheap to rerun).
var repairMinRows = 256

const (
	repairMaxRounds   = 64               // rounds of one statement
	repairStallRounds = 8                // rounds in a row without fewer conflicts
	repairChunk       = 4096             // keys recomputed by one fresh snapshot
	bigStmtRetries    = 8                // whole reruns of a statement that is repaired
	repairGateRound   = 3                // from this round on the table's other writers wait (core.WriteGate)
	repairGateAfter   = 3 * time.Second  // ... or when the statement has been repairing this long
	repairGateHold    = 20 * time.Second // until the statement commits, at most this long
)

// occTestHook, in tests, runs before every commit attempt of a repaired statement.
var occTestHook func(round int)

var repairOff = os.Getenv("STASK_OCCREPAIR") == "off" // measurement: report conflicts, rerun the statement

// repairRec follows one autocommit statement while it runs.
type repairRec struct {
	st     Stmt
	params []Datum
	ins    *Insert            // the INSERT as executed (rows evaluated)
	first  map[string]int32   // key -> index in ins.Rows
	more   map[string][]int32 // further rows with the same key
	// UPDATE .. FROM / DELETE .. USING: the references to the other
	// relations in SET and WHERE, and their values per target key
	refs    []*ColRef
	foreign map[string][]Datum
}

// foreignOf registers the references of exprs to relations other than the
// target t (qualified by qual) and returns them, to be selected with the
// target rows.
func (r *repairRec) foreignOf(t *schema.Table, qual string, exprs ...Expr) []*ColRef {
	var items []SelItem
	for _, x := range exprs {
		if x != nil {
			items = append(items, SelItem{X: x})
		}
	}
	r.refs = foreignRefs(items, t, qual)
	if len(r.refs) > 0 {
		r.foreign = map[string][]Datum{}
	}
	return r.refs
}

// keep records the values of the foreign references for the target row key.
func (r *repairRec) keep(key string, vals []Datum) {
	if r.foreign != nil {
		r.foreign[key] = append([]Datum(nil), vals...)
	}
}

// repairFor starts a record for st, or returns nil when st is not of a kind
// that is repaired.
func (s *Session) repairFor(st Stmt, params []Datum) *repairRec {
	switch x := st.(type) {
	case *Insert:
		if x.OnConflict == nil || x.Returning != nil {
			return nil
		}
	case *Update:
		if x.Returning != nil {
			return nil
		}
	case *Delete:
		if x.Returning != nil {
			return nil
		}
	default:
		return nil
	}
	return &repairRec{st: st, params: params}
}

// note records that row i of the INSERT being executed has key.
func (r *repairRec) note(ins *Insert, key string, i int) {
	if r.ins != ins {
		r.ins, r.first, r.more = ins, make(map[string]int32, len(ins.Rows)), nil
	}
	if _, dup := r.first[key]; dup {
		if r.more == nil {
			r.more = map[string][]int32{}
		}
		r.more[key] = append(r.more[key], int32(i))
		return
	}
	r.first[key] = int32(i)
}

func (r *repairRec) table() string {
	switch x := r.st.(type) {
	case *Insert:
		return x.Table
	case *Update:
		return x.Table
	case *Delete:
		return x.Table
	}
	return ""
}

// usable reports whether the statement, as it ran, can be repaired.
func (s *Session) repairUsable(r *repairRec, tx *core.Tx) bool {
	if r == nil || tx.NumWrites() < repairMinRows {
		return false
	}
	t, err := s.table(r.table())
	if err != nil || len(t.ForeignKeys) > 0 || s.E.Triggers(t.Name) != nil || len(s.E.FKRefs(t.Name)) > 0 {
		return false
	}
	if kt := t.Columns[t.KeyIndex()].T; kt != schema.TInt && kt != schema.TString && kt != schema.TText {
		return false
	}
	switch x := r.st.(type) {
	case *Insert:
		return r.ins != nil
	case *Update:
		for _, a := range x.Set {
			if strings.EqualFold(a.Col, t.Key) {
				return false
			}
		}
	}
	return true
}

// retryLimit is how many times the whole statement may be rerun.
func (s *Session) retryLimit(r *repairRec, tx *core.Tx) int {
	if r != nil && tx.NumWrites() >= repairMinRows {
		return bigStmtRetries
	}
	return autocommitRetries
}

func keyLits(t *schema.Table, keys []string) ([]Expr, error) {
	out := make([]Expr, len(keys))
	for i, k := range keys {
		if t.Columns[t.KeyIndex()].T == schema.TInt {
			n, err := strconv.ParseInt(k, 10, 64)
			if err != nil {
				return nil, err
			}
			out[i] = &Lit{V: IntD(n)}
		} else {
			out[i] = &Lit{V: TextD(k)}
		}
	}
	return out, nil
}

func tagCount(tag string) int {
	n, _ := strconv.Atoi(tag[strings.LastIndexByte(tag, ' ')+1:])
	return n
}

func withTagCount(tag string, n int) string {
	return tag[:strings.LastIndexByte(tag, ' ')+1] + strconv.Itoa(n)
}

// redo evaluates the statement again for keys on tx (a fresh snapshot) and
// returns how many rows it affected.
func (s *Session) redo(r *repairRec, tx *core.Tx, keys []string) (int, error) {
	t, err := s.table(r.table())
	if err != nil {
		return 0, err
	}
	var res *Result
	switch r.st.(type) {
	case *Insert:
		var idx []int32
		for _, k := range keys {
			if i, ok := r.first[k]; ok {
				idx = append(idx, i)
				idx = append(idx, r.more[k]...)
			}
		}
		slices.Sort(idx)
		sub := *r.ins
		sub.Rows = make([][]Expr, len(idx))
		for j, i := range idx {
			sub.Rows[j] = r.ins.Rows[i]
		}
		res, err = s.insert(tx, &sub, r.params)
	case *Update, *Delete:
		return s.redoWrite(r, tx, t, keys)
	}
	if err != nil {
		return 0, err
	}
	return tagCount(res.Tag), nil
}

// commitAuto commits the single-statement transaction tx; with rec it
// repairs lost rows instead of failing the statement.
func (s *Session) commitAuto(tx *core.Tx, rec *repairRec, res *Result) (uint64, error) {
	if rec == nil || !s.repairUsable(rec, tx) {
		if occTestHook != nil {
			occTestHook(1)
		}
		return tx.Commit()
	}
	t0 := time.Now()
	nOK, prev, stalled, total := tagCount(res.Tag), -1, 0, 0
	gated := false
	for round := 1; ; round++ {
		if occTestHook != nil {
			occTestHook(round)
		}
		lsn, err := tx.CommitRepairable()
		var re *core.RepairableError
		if !errors.As(err, &re) {
			if core.RepairDebug && round > 1 {
				fmt.Fprintf(os.Stderr, "occ: repaired %d rows in %d rounds, %v\n", total, round-1, time.Since(t0))
			}
			if err == nil {
				res.Tag = withTagCount(res.Tag, nOK)
			}
			return lsn, err
		}
		if repairOff {
			tx.Abort()
			return 0, core.ErrConflict
		}
		n := len(re.Keys)
		total += n
		if prev >= 0 && n >= prev {
			stalled++
		} else {
			stalled = 0
		}
		prev = n
		if round > repairMaxRounds || stalled >= repairStallRounds {
			tx.Abort()
			return 0, errf(codeSerialization, "could not serialize access due to concurrent update: "+
				"%d of the rows written by this statement were changed again by concurrent transactions "+
				"after %d repair rounds (%d rows repaired in %v); rerun the statement, or write in smaller batches",
				n, round, total, time.Since(t0).Round(time.Millisecond))
		}
		if !gated && (round >= repairGateRound || time.Since(t0) > repairGateAfter) {
			gated = true
			// overtaken again and again: hold the other writers of the table back
			// for the few milliseconds the last rounds take
			defer tx.CloseGate(repairGateHold)()
		}
		if round >= 3 {
			time.Sleep(time.Duration(min(round, 10)) * 300 * time.Microsecond) // let the versions that beat us become durable
		}
		for lo := 0; lo < n; lo += repairChunk {
			part := re.Keys[lo:min(lo+repairChunk, n)]
			keys := make([]string, len(part))
			for i, k := range part {
				keys[i] = k.Key
			}
			fresh := s.begin(core.SnapshotIsolation)
			fresh.SetImplicit()
			got, err := s.redo(rec, fresh, keys)
			if err == nil {
				err = tx.RepairFrom(fresh, part)
			}
			fresh.Abort()
			if err != nil {
				tx.Abort()
				if errors.Is(err, core.ErrConflict) || strings.HasPrefix(err.Error(), "occ: ") {
					return 0, core.ErrConflict // rerun the statement
				}
				return 0, err
			}
			nOK += got - len(part) // the part was counted once already
		}
	}
}

// redoWrite applies an UPDATE or DELETE again to the newest versions of the
// rows with keys, as READ COMMITTED does: the row is read again, the WHERE
// condition is evaluated on it (a row that no longer matches, or was
// deleted, is skipped), and SET is evaluated on it. Values from FROM / USING
// relations are those the statement's join found for the row.
func (s *Session) redoWrite(r *repairRec, tx *core.Tx, t *schema.Table, keys []string) (int, error) {
	var where Expr
	var set []Assign
	var alias string
	del := false
	switch x := r.st.(type) {
	case *Update:
		where, set, alias = x.Where, x.Set, x.Alias
	case *Delete:
		where, alias, del = x.Where, x.Alias, true
	}
	ctx := &evalCtx{sc: &scope{t: t, alias: alias}, params: r.params, now: nowMicros(), sess: s}
	bind := func(x Expr, vals []Datum) Expr {
		if x == nil || len(r.refs) == 0 {
			return x
		}
		return bindForeign([]SelItem{{X: x}}, r.refs, vals)[0].X
	}
	n := 0
	for _, key := range keys {
		old, ok, err := tx.Get(t.Name, key)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		vals := r.foreign[key]
		if len(r.refs) > 0 && vals == nil {
			return 0, errors.New("occ: join values of a row were not recorded")
		}
		ctx.row = old
		if w := bind(where, vals); w != nil {
			v, err := ctx.evalBool(w)
			if err != nil {
				return 0, err
			}
			if !v.Bool() {
				continue
			}
		}
		if del {
			if err := s.deleteRow(tx, t, nil, old); err != nil {
				return 0, err
			}
			n++
			continue
		}
		bound := make([]Assign, len(set))
		for i, a := range set {
			bound[i] = Assign{Col: a.Col, X: bind(a.X, vals)}
		}
		upd, err := s.applySet(t, ctx, old, bound)
		if err != nil {
			return 0, err
		}
		if t.KeyOf(upd) != key {
			return 0, errors.New("occ: the statement changes the key")
		}
		if _, err := s.updateRow(tx, t, nil, old, upd); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

func setExprs(set []Assign) Expr {
	var out Expr
	for _, a := range set {
		if out == nil {
			out = a.X
		} else {
			out = &Binary{Op: "and", L: out, R: a.X} // only walked, never evaluated
		}
	}
	return out
}
