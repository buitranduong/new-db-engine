package sql

import (
	"slices"

	"stask/internal/core"
	"stask/internal/schema"
)

// Trigger timing as PostgreSQL has it (docs/14 Appendix X):
//
//   - AFTER ROW triggers do not run at the row: they are queued and run, in
//     row order, when the statement has written all its rows, before the
//     AFTER STATEMENT triggers. A statement therefore never sees its own
//     AFTER triggers' effects half way, and the row triggers of an INSERT ..
//     SELECT run after the last row is written.
//   - statement-level triggers fire for every statement that writes the
//     table: INSERT, UPDATE, DELETE, COPY FROM, MERGE (one set per kind of
//     WHEN action), data-modifying WITH queries (each one is a statement) and
//     TRUNCATE (BEFORE/AFTER, statement level only).

type afterEvent struct {
	t       *schema.Table
	tgs     []*core.Trigger
	event   string
	old, nw schema.Row
}

// afterQueue holds the AFTER ROW trigger firings of the running statement.
type afterQueue struct{ ev []afterEvent }

// queueAfter is called by fireTriggers for an AFTER timing: it queues the
// firing if a statement scope is open and reports whether it did.
func (s *Session) queueAfter(t *schema.Table, tgs []*core.Trigger, event string, old, nw schema.Row) bool {
	if s.afterQ == nil {
		return false
	}
	for _, tg := range tgs {
		if !tg.Statement() && tg.Fires("after", event) {
			s.afterQ.ev = append(s.afterQ.ev, afterEvent{t, tgs, event, old, nw})
			return true
		}
	}
	return true // nothing to run
}

// flushAfter runs the queued AFTER ROW triggers. The queue is detached while
// they run, so statements they execute open scopes of their own.
func (s *Session) flushAfter(tx *core.Tx, q *afterQueue) error {
	prev := s.afterQ
	s.afterQ = nil
	defer func() { s.afterQ = prev }()
	for i := 0; i < len(q.ev); i++ { // triggers cannot append to q: no scope points at it
		e := q.ev[i]
		if _, _, err := s.fireTriggers(tx, e.t, e.tgs, "after", e.event, e.old, e.nw); err != nil {
			return err
		}
	}
	q.ev = nil
	return nil
}

// hasStmtTrigger reports a statement-level trigger of table for event.
func hasStmtTrigger(tgs []*core.Trigger, event string) bool {
	for _, tg := range tgs {
		if tg.Statement() && slices.Contains(tg.Events, event) {
			return true
		}
	}
	return false
}

// withStmtEvents runs f, a statement that writes table with the given kinds
// of change, between the BEFORE and the AFTER statement-level triggers of
// those events, and runs the AFTER ROW triggers it queued at its end.
func (s *Session) withStmtEvents(tx *core.Tx, table string, events []string, f func() (*Result, error)) (*Result, error) {
	tgs := s.E.Triggers(table)
	if tgs == nil || s.noTrig {
		return f()
	}
	var fire []string
	for _, ev := range events {
		if hasStmtTrigger(tgs, ev) && !slices.Contains(fire, ev) {
			fire = append(fire, ev)
		}
	}
	for _, ev := range fire {
		if err := s.fireStmtTriggers(tx, table, tgs, "before", ev); err != nil {
			return nil, err
		}
	}
	prev := s.afterQ
	q := &afterQueue{}
	s.afterQ = q
	res, err := f()
	s.afterQ = prev
	if err != nil {
		return nil, err
	}
	if err := s.flushAfter(tx, q); err != nil {
		return nil, err
	}
	for _, ev := range fire {
		if err := s.fireStmtTriggers(tx, table, tgs, "after", ev); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// mergeEvents are the kinds of change the WHEN actions of a MERGE make.
func mergeEvents(st *Merge) []string {
	var out []string
	for _, w := range st.Whens {
		if w.Action == "nothing" || slices.Contains(out, w.Action) {
			continue
		}
		out = append(out, w.Action)
	}
	return out
}

// truncateTriggered runs TRUNCATE of tables that have TRUNCATE triggers:
// BEFORE triggers, the truncation (as deletes of the transaction, so that
// the whole statement is atomic), AFTER triggers. handled is false when no
// table has one.
func (s *Session) truncateTriggered(st *Truncate, params []Datum) (*Result, bool, error) {
	if s.noTrig || s.inTruncTrig {
		return nil, false, nil
	}
	names := s.mapTemps(st.Names)
	if st.Cascade {
		names, _ = s.truncateClosure(names)
	}
	var with []string
	for _, n := range names {
		if hasStmtTrigger(s.E.Triggers(n), "truncate") {
			with = append(with, n)
		}
	}
	if len(with) == 0 {
		return nil, false, nil
	}
	own := !s.explicit
	if own {
		if _, err := s.exec(&Begin{}, nil); err != nil {
			return nil, true, err
		}
	}
	fail := func(err error) (*Result, bool, error) {
		if own {
			s.exec(&Rollback{}, nil)
		}
		return nil, true, err
	}
	for _, n := range with {
		if err := s.fireStmtTriggers(s.tx, n, s.E.Triggers(n), "before", "truncate"); err != nil {
			return fail(err)
		}
	}
	s.inTruncTrig = true
	res, err := s.exec(st, params)
	s.inTruncTrig = false
	if err != nil {
		return fail(err)
	}
	for _, n := range with {
		if err := s.fireStmtTriggers(s.tx, n, s.E.Triggers(n), "after", "truncate"); err != nil {
			return fail(err)
		}
	}
	if own {
		if _, err := s.exec(&Commit{}, nil); err != nil {
			return nil, true, err
		}
	}
	return res, true, nil
}
