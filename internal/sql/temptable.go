package sql

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"stask/internal/core"
	"stask/internal/schema"
)

// Temporary tables (docs/05, last section).
//
// A temporary table is an engine table of the reserved name
// "pg_temp_<session>#<name>" flagged System: the flag hides it from every
// client lookup (UserTable), from the catalogs of other sessions, from CDC
// and from publications. Only the creating session maps its plain name to
// it (Session.table), so it shadows a table of the same name in public, is
// invisible to other sessions and is dropped when the session closes (or
// when the process starts again: sweepTemps). Its rows ARE written to the
// log, like any table: the engine has no log-free table (a change would be
// a change of the commit path); they replicate to followers, which never
// show them.

const tempPrefix = "pg_temp_"

// isTempName reports an engine name made by tempReal.
func isTempName(n string) bool {
	return strings.HasPrefix(n, tempPrefix) && strings.Contains(n, "#")
}

var (
	tempSeq   atomic.Int64
	liveTemps sync.Map // session id -> struct{}
	swept     sync.Map // *core.Engine -> struct{}
)

type tempRel struct {
	real     string
	onCommit string // "", "preserve", "delete", "drop"
	inTx     bool   // created by the open explicit transaction
}

// tempState is the temporary tables of one session.
type tempState struct {
	id   int64
	rels map[string]*tempRel
	act  bool // some table has ON COMMIT DELETE ROWS / DROP
}

func (s *Session) tempSt() *tempState {
	if s.tmp == nil {
		s.tmp = &tempState{id: tempSeq.Add(1), rels: map[string]*tempRel{}}
	}
	return s.tmp
}

// tempSchema is the name of the session's temporary schema.
func (s *Session) tempSchema() string { return tempPrefix + strconv.FormatInt(s.tempSt().id, 10) }

// tempReal is the engine name of the session's temporary table name, if any.
func (s *Session) tempReal(name string) (string, bool) {
	if s.tmp == nil {
		return name, false
	}
	if r, ok := s.tmp.rels[name]; ok {
		return r.real, true
	}
	return name, false
}

// mapTemp maps a table name to its temporary table's engine name, if any.
func (s *Session) mapTemp(name string) string {
	r, _ := s.tempReal(name)
	return r
}

func (s *Session) mapTemps(names []string) []string {
	if s.tmp == nil || len(s.tmp.rels) == 0 {
		return names
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = s.mapTemp(n)
	}
	return out
}

// userTable is Engine.UserTable that sees the session's temporary tables.
func (s *Session) userTable(name string) (*core.TableState, error) {
	if r, ok := s.tempReal(name); ok {
		return s.E.Table(r)
	}
	if s.tmp != nil && strings.HasPrefix(name, s.tempSchema()+"#") { // an engine name of this session
		return s.E.Table(name)
	}
	return s.E.UserTable(name)
}

// sweepTemps drops the temporary tables left by sessions of an earlier run
// (nobody owns them any more), once per engine and process.
func (s *Session) sweepTemps() {
	if _, done := swept.LoadOrStore(s.E, struct{}{}); done {
		return
	}
	for _, ts := range s.E.Tables() {
		n := ts.Schema().Name
		if !isTempName(n) {
			continue
		}
		if id, _, ok := strings.Cut(strings.TrimPrefix(n, tempPrefix), "#"); ok {
			if i, err := strconv.ParseInt(id, 10, 64); err == nil {
				if _, live := liveTemps.Load(i); live {
					continue
				}
			}
		}
		s.dropReal(n)
	}
}

// dropReal drops the engine table with its sequences and triggers.
func (s *Session) dropReal(real string) error {
	seqs := s.tableSequences(real)
	err := s.E.DropTable(real)
	if err != nil {
		return err
	}
	s.E.DropTableTriggers(real)
	for _, q := range seqs {
		s.E.DropSequence(q.Name)
	}
	return nil
}

// newTempName reserves the engine name of a temporary table to be created.
func (s *Session) newTempName(name string) string {
	s.sweepTemps()
	st := s.tempSt()
	liveTemps.Store(st.id, struct{}{})
	return tempPrefix + strconv.FormatInt(st.id, 10) + "#" + name
}

// registerTemp makes name the session's temporary table real.
func (s *Session) registerTemp(name, real, onCommit string) {
	st := s.tempSt()
	st.rels[name] = &tempRel{real: real, onCommit: onCommit, inTx: s.explicit}
	if onCommit == "delete" || onCommit == "drop" {
		st.act = true
	}
}

// dropTempNamed drops the session's temporary table name.
func (s *Session) dropTempNamed(name string) {
	if s.tmp == nil {
		return
	}
	if r, ok := s.tmp.rels[name]; ok {
		s.dropReal(r.real)
		delete(s.tmp.rels, name)
	}
}

// dropAllTemps runs when the session closes.
func (s *Session) dropAllTemps() {
	if s.tmp == nil {
		return
	}
	for n := range s.tmp.rels {
		s.dropTempNamed(n)
	}
	liveTemps.Delete(s.tmp.id)
}

// forgetDropped removes from the session the temporary tables DROP TABLE removed.
func (s *Session) forgetDropped() {
	if s.tmp == nil {
		return
	}
	for n, r := range s.tmp.rels {
		if _, err := s.E.Table(r.real); err != nil {
			delete(s.tmp.rels, n)
		}
	}
}

// afterTxEnd applies ON COMMIT actions (committed) and removes the temporary
// tables a rolled back transaction created.
func (s *Session) afterTxEnd(committed bool) {
	if s.tmp == nil || len(s.tmp.rels) == 0 {
		return
	}
	for n, r := range s.tmp.rels {
		switch {
		case !committed && r.inTx:
			s.dropTempNamed(n)
		case committed && r.onCommit == "drop":
			s.dropTempNamed(n)
		case committed && r.onCommit == "delete":
			if ts, err := s.E.Table(r.real); err == nil {
				if !s.tempEmpty(ts) {
					s.E.TruncateTable(r.real)
				}
			}
		}
		if r2, ok := s.tmp.rels[n]; ok {
			r2.inTx = false
		}
	}
}

// tempEmpty reports whether a table holds no row (a fresh snapshot).
func (s *Session) tempEmpty(ts *core.TableState) bool {
	tx := s.begin(core.SnapshotIsolation)
	defer tx.Abort()
	empty := true
	tx.ScanAll(ts.Schema().Name, func(string, schema.Row) bool { empty = false; return false })
	return empty
}

// tempCatalog lists the session's temporary tables as the catalogs show them:
// the schema under their plain names (the second result maps each to its
// plain name).
func (s *Session) tempCatalog() []*schema.Table {
	if s.tmp == nil {
		return nil
	}
	var out []*schema.Table
	for n, r := range s.tmp.rels {
		ts, err := s.E.Table(r.real)
		if err != nil {
			continue
		}
		t := ts.Schema().ClientView()
		cp := *t
		cp.Name, cp.System = n, false
		out = append(out, &cp)
	}
	return out
}


// ---- catalogs ----

const nsTempBase = 99000

func (s *Session) tempNS() int64 { return nsTempBase + s.tempSt().id }

// nsName is the schema a catalog shows for the table.
func (c *catSnap) nsName(t *schema.Table) string {
	if c.temps[t] {
		return c.s.tempSchema()
	}
	return "public"
}

func (c *catSnap) tblType(t *schema.Table) string {
	if c.temps[t] {
		return "LOCAL TEMPORARY"
	}
	return "BASE TABLE"
}

// nsExt adds the session's temporary schema to pg_namespace.
func (c *catSnap) nsExt(rows [][]Datum) [][]Datum {
	if c.s.tmp != nil {
		rows = append(rows, []Datum{IntD(c.s.tempNS()), TextD(c.s.tempSchema()), IntD(ownerOID), Null})
	}
	return rows
}

// relExt patches the pg_class row of table t: temporary schema and
// persistence, materialized views not populated.
func (c *catSnap) relExt(row []Datum, t *schema.Table) {
	switch {
	case c.temps[t]:
		row[2], row[15] = IntD(c.s.tempNS()), TextD("t")
	case t.Unlogged:
		row[15] = TextD("u")
	}
	if t.MatUnpopulated {
		row[24] = BoolD(false)
	}
}

func init() {
	defCat("pg_matviews", "schemaname:t matviewname:t matviewowner:t tablespace:t hasindexes:b ispopulated:b "+
		"definition:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			if t.MatView == "" {
				continue
			}
			rows = append(rows, []Datum{TextD("public"), TextD(t.Name), TextD(roleName), Null, BoolD(len(t.Indexes) > 0),
				BoolD(!t.MatUnpopulated), TextD(" " + strings.TrimSuffix(strings.TrimSpace(t.MatView), ";") + ";")})
		}
		return rows
	})
}
