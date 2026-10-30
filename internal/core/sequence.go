package core

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"stask/internal/schema"
)

// Sequences (CREATE SEQUENCE, nextval, SERIAL and IDENTITY columns), as
// PostgreSQL implements them (https://www.postgresql.org/docs/16/sql-createsequence.html):
//
//   - nextval is not transactional: a value is never handed out twice, even
//     if the transaction that took it rolls back;
//   - the state is logged ahead: each logged write reserves the next
//     seqLogVals values (PostgreSQL's SEQ_LOG_VALS), so only one nextval
//     in 32 commits. After a crash the sequence resumes after the last
//     reserved value, skipping the ones not handed out, as in PostgreSQL.
//
// Each sequence is one row of a system table, written like any row, so
// sequences are durable, replicated, checkpointed and recovered with no
// separate mechanism. The table is hidden from clients (Schema.System).

const (
	seqTableName = "__stask_sequences"
	seqLogVals   = 32
	// the sequence behind a hidden __rowid column (a table without PRIMARY
	// KEY) reserves far more, as every row of a bulk insert takes a value:
	// one log write per 32 rows would make such a table 5x slower to load.
	// After a crash the gap is at most this many ids, which nobody sees.
	rowIDLogVals = 1 << 16
)

var (
	ErrNoSequence     = errors.New("sequence does not exist")
	ErrSequenceExists = errors.New("sequence already exists")
	ErrSequenceLimit  = errors.New("sequence reached its limit")
	ErrSequenceBounds = errors.New("value out of the sequence's bounds")
)

// Sequence is a sequence's definition.
type Sequence struct {
	Name     string
	Type     string // smallint, integer or bigint
	Start    int64
	Inc      int64
	Min, Max int64
	Cycle    bool
	// Owner is "table.column" for the sequence of a SERIAL or IDENTITY
	// column: dropped with the table, reset by TRUNCATE RESTART IDENTITY.
	Owner string
}

type seqState struct {
	Sequence
	cur    int64 // the last value handed out (or the next one if !called)
	called bool
	logged int64 // values up to this one are reserved in the log
}

func seqSchema() schema.Table {
	return schema.Table{Name: seqTableName, Key: "name", System: true, Columns: []schema.Column{
		{Name: "name", Type: "string"}, {Name: "start", Type: "int"}, {Name: "inc", Type: "int"},
		{Name: "min", Type: "int"}, {Name: "max", Type: "int"}, {Name: "cycle", Type: "bool"},
		{Name: "owner", Type: "string"}, {Name: "last", Type: "int"}, {Name: "called", Type: "bool"},
		{Name: "type", Type: "string"}}}
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func seqRow(s *Sequence, last int64, called bool) schema.Row {
	return schema.Row{{S: s.Name}, {I: s.Start}, {I: s.Inc}, {I: s.Min}, {I: s.Max}, {I: b2i(s.Cycle)},
		{S: s.Owner, Null: s.Owner == ""}, {I: last}, {I: b2i(called)}, {S: s.Type}}
}

func seqOfRow(r schema.Row) *seqState {
	st := &seqState{Sequence: Sequence{Name: r[0].S, Start: r[1].I, Inc: r[2].I, Min: r[3].I, Max: r[4].I,
		Cycle: r[5].I != 0, Owner: r[6].S, Type: r[9].S}, cur: r[7].I, called: r[8].I != 0}
	st.logged = st.cur
	return st
}

// seqTable returns the sequences' system table, creating it on first use.
func (e *Engine) seqTable() error {
	if _, err := e.Table(seqTableName); err == nil {
		return nil
	}
	return e.createSystemTable(seqSchema())
}

func (e *Engine) tableExists(name string) bool {
	_, err := e.Table(name)
	return err == nil
}

// loadSeq returns a sequence's state; seqMu is held.
func (e *Engine) loadSeq(name string) (*seqState, error) {
	if st := e.seqs[name]; st != nil && !e.ro.Load() { // a replica reads the leader's writes
		return st, nil
	}
	if !e.tableExists(seqTableName) {
		return nil, fmt.Errorf("%w: %s", ErrNoSequence, name)
	}
	r, _, err := e.Get(seqTableName, name)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSequence, name)
	}
	if e.seqs == nil {
		e.seqs = map[string]*seqState{}
	}
	st := seqOfRow(r)
	e.seqs[name] = st
	return st, nil
}

// SequenceDefaults fills in PostgreSQL's defaults for a sequence of a type
// with the given bounds: increment 1; ascending sequences run from 1 to
// the type's maximum, descending ones from -1 down to its minimum; the
// start is the minimum (ascending) or the maximum (descending).
func SequenceDefaults(s *Sequence, typeMin, typeMax int64, hasMin, hasMax, hasStart bool) error {
	if s.Inc == 0 {
		return errors.New("INCREMENT must not be zero")
	}
	if !hasMin {
		s.Min = 1
		if s.Inc < 0 {
			s.Min = typeMin
		}
	}
	if !hasMax {
		s.Max = typeMax
		if s.Inc < 0 {
			s.Max = -1
		}
	}
	if s.Min < typeMin || s.Max > typeMax {
		return fmt.Errorf("%w: MINVALUE or MAXVALUE out of range for the sequence's type", ErrSequenceBounds)
	}
	if s.Min >= s.Max {
		return fmt.Errorf("%w: MINVALUE (%d) must be less than MAXVALUE (%d)", ErrSequenceBounds, s.Min, s.Max)
	}
	if !hasStart {
		s.Start = s.Min
		if s.Inc < 0 {
			s.Start = s.Max
		}
	}
	if s.Start < s.Min || s.Start > s.Max {
		return fmt.Errorf("%w: START value (%d) must be between MINVALUE (%d) and MAXVALUE (%d)", ErrSequenceBounds, s.Start, s.Min, s.Max)
	}
	return nil
}

// CreateSequence creates a sequence (with its fields already defaulted).
func (e *Engine) CreateSequence(s Sequence) error {
	if err := e.seqTable(); err != nil {
		return err
	}
	_, err := e.Write([]WriteOp{{Table: seqTableName, Row: seqRow(&s, s.Start, false), IfAbsent: true}})
	if errors.As(err, new(*KeyExistsError)) {
		return fmt.Errorf("%w: %s", ErrSequenceExists, s.Name)
	}
	return err
}

// DropSequence drops a sequence.
func (e *Engine) DropSequence(name string) error {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	if _, err := e.loadSeq(name); err != nil {
		return err
	}
	delete(e.seqs, name)
	_, err := e.Write([]WriteOp{{Table: seqTableName, Key: name}})
	return err
}

// Sequences lists the sequences by name.
func (e *Engine) Sequences() []Sequence {
	if !e.tableExists(seqTableName) {
		return nil
	}
	var out []Sequence
	e.ScanSnapshot(seqTableName, e.DurableLSN(), func(_ string, r schema.Row, _ *Version) bool {
		out = append(out, seqOfRow(r).Sequence)
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SequenceState is a sequence's current value as pg_sequences shows it:
// the last value handed out, if any.
func (e *Engine) SequenceState(name string) (last int64, called bool, err error) {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	st, err := e.loadSeq(name)
	if err != nil {
		return 0, false, err
	}
	return st.cur, st.called, nil
}

// NextVal hands out a sequence's next value.
func (e *Engine) NextVal(name string) (int64, error) {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	st, err := e.loadSeq(name)
	if err != nil {
		return 0, err
	}
	v, cycled := st.cur, false
	if st.called {
		next, ok := addInt(st.cur, st.Inc)
		if !ok || next > st.Max || next < st.Min {
			if !st.Cycle {
				lim := st.Max
				if st.Inc < 0 {
					lim = st.Min
				}
				return 0, fmt.Errorf("%w: nextval: reached %s value of sequence %q (%d)", ErrSequenceLimit,
					map[bool]string{true: "maximum", false: "minimum"}[st.Inc > 0], name, lim)
			}
			next, cycled = st.Min, true
			if st.Inc < 0 {
				next = st.Max
			}
		}
		v = next
	}
	beyond := st.Inc > 0 && v > st.logged || st.Inc < 0 && v < st.logged
	if !st.called || beyond || cycled {
		// reserve up to seqLogVals values, not past the bound
		var hw int64
		ahead := int64(seqLogVals)
		if strings.HasSuffix(st.Owner, "."+schema.RowIDCol) {
			ahead = rowIDLogVals
		}
		ok := st.Inc <= math.MaxInt64/ahead && st.Inc >= math.MinInt64/ahead
		if ok {
			hw, ok = addInt(v, st.Inc*(ahead-1))
		}
		if !ok || hw > st.Max || hw < st.Min {
			hw = st.Max
			if st.Inc < 0 {
				hw = st.Min
			}
		}
		if _, err := e.Write([]WriteOp{{Table: seqTableName, Row: seqRow(&st.Sequence, hw, true)}}); err != nil {
			return 0, err
		}
		st.logged = hw
	}
	st.cur, st.called = v, true
	return v, nil
}

// SetVal sets a sequence's value: the next nextval returns v if !called,
// else the value after v.
func (e *Engine) SetVal(name string, v int64, called bool) error {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	st, err := e.loadSeq(name)
	if err != nil {
		return err
	}
	if v < st.Min || v > st.Max {
		return fmt.Errorf("%w: setval: value %d is out of bounds for sequence %q (%d..%d)", ErrSequenceBounds, v, name, st.Min, st.Max)
	}
	if _, err := e.Write([]WriteOp{{Table: seqTableName, Row: seqRow(&st.Sequence, v, called)}}); err != nil {
		return err
	}
	st.cur, st.called, st.logged = v, called, v
	return nil
}

// AlterSequence changes a sequence's definition with f and, if f returns
// a restart value, restarts it there (the next nextval returns it).
// Otherwise the current value is kept and must be within the new bounds.
func (e *Engine) AlterSequence(name string, f func(*Sequence) (restart *int64, err error)) error {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	st, err := e.loadSeq(name)
	if err != nil {
		return err
	}
	def := st.Sequence
	restart, err := f(&def)
	if err != nil {
		return err
	}
	cur, called := st.cur, st.called
	if restart != nil {
		cur, called = *restart, false
	}
	if cur < def.Min || cur > def.Max {
		return fmt.Errorf("%w: value %d of sequence %q is out of its bounds (%d..%d)", ErrSequenceBounds, cur, name, def.Min, def.Max)
	}
	if _, err := e.Write([]WriteOp{{Table: seqTableName, Row: seqRow(&def, cur, called)}}); err != nil {
		return err
	}
	st.Sequence, st.cur, st.called, st.logged = def, cur, called, cur
	return nil
}

func addInt(a, b int64) (int64, bool) {
	c := a + b
	if b > 0 && c < a || b < 0 && c > a {
		return 0, false
	}
	return c, true
}
