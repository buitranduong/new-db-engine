package sql

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"stask/internal/core"
	"stask/internal/pgnum"
	"stask/internal/schema"
)

// VersionString is reported as server_version and by version().
const VersionString = "PostgreSQL 16.0 (STASK prototype)"

// Column describes one result column.
type Column struct {
	Name string
	Kind Kind
	// OID and Mod refine Kind to the PostgreSQL type (int4 for KInt,
	// varchar(n) for KText...); OID 0 means the Kind's default type.
	OID uint32
	Mod int32
	// TableOID and AttNum: the column is read straight from that table's
	// column (RowDescription fields JDBC ResultSetMetaData uses); 0 if not.
	TableOID uint32
	AttNum   int16
}

// withTy returns c typed t when t describes values of c's Kind.
func (c Column) withTy(t pgTy) Column {
	if t = t.forKind(c.Kind); t.oid != 0 {
		c.OID, c.Mod = t.oid, t.mod
	}
	return c
}

// Result of one statement.
type Result struct {
	Cols    []Column
	Rows    [][]Datum
	Tag     string // CommandComplete tag
	HasRows bool   // statement returns a row set (even if empty)
	Notice  string
}

// Session holds per-connection state: the open transaction and settings.
type Session struct {
	E        *core.Engine
	tx       *core.Tx
	explicit bool       // inside BEGIN ... COMMIT
	spilled  bool       // the last query ran in spill mode (EXPLAIN ANALYZE)
	rep      *repairRec // occ_repair.go: the autocommit statement being run
	failed   bool       // an error occurred inside the explicit transaction
	vars     map[string]string
	Views    Views // nil: aggregates always scan rows

	// PID identifies the session to listeners (the wire protocol's
	// backend process id).
	PID       int32
	listener  *Listener
	notes     []Notification   // NOTIFY of the current transaction
	currvals  map[string]int64 // currval: the last nextval of each sequence
	prepared  map[string]Stmt  // SQL-level PREPARE (prepare.go)
	lastSeq   string           // lastval's sequence
	listenOps []listenOp       // LISTEN/UNLISTEN of the current transaction

	ctl stmtCtl // cancel and timeouts of the running statement (cancel.go)

	trigDepth int                  // nesting of row triggers being run (trigger.go)
	role      string               // role the session runs as; "" = bootstrap superuser (roles.go)
	needCache map[Stmt]cachedNeeds // privileges each statement needs
	pendingQ  string               // text of the statement about to run (SetQuery)
	act       *Activity            // pg_stat_activity entry (activity.go)

	// compat_stmts.go
	savepoints []savepointState
	readOnly   bool       // BEGIN READ ONLY / SET TRANSACTION READ ONLY
	localUndo  []localVar // SET LOCAL values to restore at the end of the transaction

	// cursor.go, locks.go, setrole.go
	curs       *cursorSet
	lk         *sessLocks
	rc         roleCtx
	tmp        *tempState // temporary tables (temptable.go)
	refreshing string     // materialized view being refreshed (ctas_exec.go)
}

// clickHouse reports whether the session uses ClickHouse's rules where
// they conflict with PostgreSQL's (SET sql_dialect = 'clickhouse', as
// Apache Doris's sql_dialect): names in ORDER BY then mean input columns
// unless they are explicit aliases.
func (s *Session) clickHouse() bool { return s.vars["sql_dialect"] == "clickhouse" }

func NewSession(e *core.Engine) *Session {
	return &Session{E: e, vars: sessionDefaults()}
}

// TxStatus is the ReadyForQuery indicator: 'I' idle, 'T' in transaction, 'E' failed.
func (s *Session) TxStatus() byte {
	switch {
	case !s.explicit:
		return 'I'
	case s.failed:
		return 'E'
	}
	return 'T'
}

// Close aborts any open transaction.
func (s *Session) Close() {
	if s.tx != nil {
		s.tx.Abort()
		s.tx = nil
	}
	s.explicit, s.failed = false, false
	s.closeCursors()
	s.releaseAllLocks()
	s.discardPending()
	s.closeListener()
	s.dropAllTemps()
	s.untrack()
}

// A single autocommit statement that loses a write-write race is retried
// on a fresh snapshot (as READ COMMITTED would re-evaluate it); nothing has
// been returned to the client yet, so the retry is invisible.
const autocommitRetries = 1000

// Exec runs one statement.
// exec executes one statement (Exec in cancel.go wraps it).
func (s *Session) exec(st Stmt, params []Datum) (*Result, error) {
	if err := s.waitMinLSN(st); err != nil {
		return nil, err
	}
	if s.failed {
		switch st.(type) {
		case *Commit, *Rollback, *RollbackToStmt:
		default:
			return nil, errf(codeInFailedTx, "current transaction is aborted, commands ignored until end of transaction block")
		}
	}
	if s.role != "" {
		if err := s.authorize(st); err != nil {
			s.markFailed()
			return nil, err
		}
	}
	if w, what := writesStatement(st); w && s.isReadOnly() {
		s.markFailed()
		return nil, errf("25006", "cannot execute %s in a read-only transaction", what)
	}
	switch st := st.(type) {
	case *WithDML:
		return s.withDML(st, params)
	case *PrepareStmt, *ExecuteStmt, *DeallocateStmt, *MaintStmt:
		return s.execPrepare(st, params)
	case *SavepointStmt, *ReleaseStmt, *RollbackToStmt, *DiscardStmt, *ResetStmt, *CommentStmt:
		return s.execCompat(st)
	case *DeclareCursor, *FetchStmt, *CloseCursor, *LockTable, *ResetRole:
		return s.execCursor(st, params)
	case *Begin:
		if s.explicit {
			return &Result{Tag: "BEGIN", Notice: "there is already a transaction in progress"}, nil
		}
		iso := core.SnapshotIsolation
		if st.Iso == "serializable" || st.Iso == "" && s.vars["default_transaction_isolation"] == "serializable" {
			iso = core.Serializable
		}
		s.tx, s.explicit, s.failed = s.begin(iso), true, false
		s.readOnly = st.ReadOnly || s.vars["default_transaction_read_only"] == "on"
		return &Result{Tag: "BEGIN"}, nil
	case *Commit:
		if !s.explicit {
			return &Result{Tag: "COMMIT", Notice: "there is no transaction in progress"}, nil
		}
		defer s.releaseTxLocks()
		tx, failed := s.tx, s.failed
		s.tx, s.explicit, s.failed = nil, false, false
		s.endTx()
		if failed {
			tx.Abort()
			s.cursorsOnAbort()
			s.discardPending()
			s.afterTxEnd(false)
			return &Result{Tag: "ROLLBACK"}, nil
		}
		if err := s.cursorsOnCommit(tx); err != nil {
			tx.Abort()
			s.discardPending()
			return nil, err
		}
		ops := s.beforeCommit(tx)
		lsn, err := tx.Commit()
		if err != nil {
			return nil, AsError(err)
		}
		s.noteCommit(lsn)
		for _, op := range ops {
			s.applyListen(op)
		}
		s.afterTxEnd(true)
		return &Result{Tag: "COMMIT"}, nil
	case *Rollback:
		if !s.explicit {
			return &Result{Tag: "ROLLBACK", Notice: "there is no transaction in progress"}, nil
		}
		defer s.releaseTxLocks()
		s.tx.Abort()
		s.tx, s.explicit, s.failed = nil, false, false
		s.endTx()
		s.cursorsOnAbort()
		s.discardPending()
		s.afterTxEnd(false)
		return &Result{Tag: "ROLLBACK"}, nil
	case *SetVar:
		return s.set(st)
	case *Show:
		if isShowAll(st.Name) {
			return s.showAll(), nil
		}
		name := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(st.Name), " . ", "."), " ", "_")
		if name == "transaction_isolation_level" {
			name = "transaction_isolation"
		}
		v, ok := s.vars[name]
		if !ok {
			v, ok = settingDefaults[name]
		}
		if name == "transaction_read_only" && s.explicit && s.readOnly {
			v = "on"
		}
		if name == "transaction_isolation" && s.explicit && s.tx.Iso() == core.Serializable {
			v = "serializable"
		}
		if !ok {
			return nil, errf("42704", "unrecognized configuration parameter %q", st.Name)
		}
		col := name
		if c, ok := showNames[name]; ok {
			col = c
		}
		return &Result{Cols: []Column{{Name: col, Kind: KText}}, Rows: [][]Datum{{TextD(v)}}, Tag: "SHOW", HasRows: true}, nil
	case *ObjDDL, *CommentOn:
		// applied at once, like CREATE TABLE (ddlobjs_exec.go)
		res, err := s.objDDL(st)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *CreateTable:
		res, err := s.createTable(st)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *CreatePublication, *DropPublication, *AlterReplicaIdentity:
		// applied at once, not at commit (unlike PostgreSQL, where these
		// are transactional catalog changes)
		res, err := s.cdcDDL(st)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *AlterTable:
		// applied at once, like CREATE TABLE
		res, err := s.alterTable(st)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *CreateSequence, *DropSequence, *AlterSequence:
		// not transactional: applied at once (PostgreSQL's are, except
		// for nextval and setval)
		res, err := s.seqDDL(st)
		if err != nil {
			s.markFailed()
		} else {
			s.afterDrop(st)
		}
		return res, err
	case *CreateRole, *AlterRole, *DropRole, *Grant:
		// applied at once, not at commit
		res, err := s.roleDDL(st)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *CreateIndex:
		res, err := s.createIndex(st)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *DropIndex:
		res, err := s.dropIndex(st)
		if err != nil {
			s.markFailed()
		} else {
			s.afterDrop(st)
		}
		return res, err
	case *CreateView, *DropView:
		// applied at once, like CREATE TABLE
		res, err := s.viewDDL(st)
		if err != nil {
			s.markFailed()
		} else {
			s.afterDrop(st)
		}
		return res, err
	case *CreateTrigger, *DropTrigger:
		// applied at once, like CREATE TABLE
		res, err := s.triggerDDL(st)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *Explain:
		res, err := s.explainAny(st, params)
		if err != nil {
			s.markFailed()
		}
		return res, err
	case *CreateSubstrIndex, *DropSubstrIndex:
		return s.execSubstr(st)
	case *DropTable, *Truncate:
		// applied at once, not at commit, like CREATE TABLE (PostgreSQL
		// runs both inside the transaction)
		res, err := s.dropOrTruncate(st)
		if err != nil {
			s.markFailed()
		} else {
			s.afterDrop(st)
		}
		return res, err
	case *Listen:
		op := listenOp{listen: true, ch: st.Channel}
		if s.explicit {
			s.listenOps = append(s.listenOps, op)
		} else {
			s.applyListen(op)
		}
		return &Result{Tag: "LISTEN"}, nil
	case *Unlisten:
		op := listenOp{ch: st.Channel, all: st.All}
		if s.explicit {
			s.listenOps = append(s.listenOps, op)
		} else {
			s.applyListen(op)
		}
		return &Result{Tag: "UNLISTEN"}, nil
	case *Notify:
		if err := s.notify(st.Channel, st.Payload); err != nil {
			s.markFailed()
			return nil, err
		}
		if !s.explicit {
			hubOf(s.E).publish(s.notes)
			s.notes = nil
		}
		return &Result{Tag: "NOTIFY"}, nil
	}

	// DML and queries run in the explicit transaction, or in their own
	// snapshot-isolated transaction (retried on write conflicts, which is
	// invisible to the client because nothing was returned yet).
	if s.explicit {
		res, err := s.run(s.tx, st, params)
		if err != nil {
			s.markFailed()
			return nil, AsError(err)
		}
		return res, nil
	}
	defer s.releaseTxLocks()
	for attempt := 0; ; attempt++ {
		s.releaseTxLocks()
		s.notes = nil // pg_notify() calls of an attempt that was retried
		tx := s.begin(core.SnapshotIsolation)
		tx.SetImplicit()
		s.rep = s.repairFor(st, params) // occ_repair.go
		res, err := s.run(tx, st, params)
		rec := s.rep
		s.rep = nil
		if err != nil {
			tx.Abort()
			s.discardPending()
			if errors.Is(err, core.ErrConflict) && attempt < s.retryLimit(rec, tx) {
				// rows built for a schema an ALTER TABLE replaced
				backoff(attempt)
				continue
			}
			return nil, AsError(err)
		}
		s.beforeCommit(tx)
		lsn, err := s.commitAuto(tx, rec, res)
		if err != nil {
			if errors.Is(err, core.ErrConflict) && attempt < s.retryLimit(rec, tx) {
				backoff(attempt)
				continue
			}
			return nil, AsError(err)
		}
		s.noteCommit(lsn)
		s.afterTxEnd(true)
		return res, nil
	}
}

func backoff(attempt int) {
	d := time.Duration(min(attempt, 20)) * 100 * time.Microsecond
	time.Sleep(d + time.Duration(rand.Int64N(int64(d)+1)))
}

func (s *Session) markFailed() {
	if s.explicit {
		s.failed = true
	}
}

func (s *Session) set(st *SetVar) (*Result, error) {
	if r, ok, err := s.setRoleVar(st); ok {
		return r, err
	}
	name := strings.ToLower(st.Name)
	val := strings.Trim(strings.ToLower(st.Value), "'\"")
	if st.Local && name != "transaction" {
		if !s.explicit { // PostgreSQL warns: there is no transaction block, so nothing is set
			return &Result{Tag: "SET", Notice: "SET LOCAL can only be used in transaction blocks"}, nil
		}
		s.noteLocal(name)
		local := *st
		local.Local = false
		return s.set(&local)
	}
	switch name {
	case "transaction", "session characteristics":
		if strings.Contains(val, "read only") || strings.Contains(val, "read write") {
			ro := strings.Contains(val, "read only")
			if name == "transaction" {
				if !s.explicit {
					return &Result{Tag: "SET"}, nil
				}
				s.readOnly = ro
			} else if ro {
				s.vars["default_transaction_read_only"] = "on"
			} else {
				s.vars["default_transaction_read_only"] = "off"
			}
			if !strings.Contains(val, "isolation") {
				return &Result{Tag: "SET"}, nil
			}
		}
		// SET TRANSACTION ISOLATION LEVEL x / SET SESSION CHARACTERISTICS AS TRANSACTION ...
		val = strings.TrimPrefix(strings.TrimPrefix(val, "as transaction "), "isolation level ")
		if name == "session characteristics" {
			s.vars["default_transaction_isolation"] = val
			return &Result{Tag: "SET"}, nil
		}
		if s.explicit && val == "serializable" && s.tx.Iso() != core.Serializable {
			s.tx.Abort()
			s.tx = s.begin(core.Serializable)
		}
		return &Result{Tag: "SET"}, nil
	case "time zone":
		name = "timezone"
	case "statement_timeout", "idle_in_transaction_session_timeout", "idle_session_timeout":
		return s.setTimeout(name, st.Value)
	case MinLSNVar:
		if _, err := strconv.ParseUint(strings.Trim(st.Value, "'\""), 10, 64); err != nil {
			return nil, errf(codeInvalidText, "invalid value for %s: %q (want an LSN, a number)", MinLSNVar, st.Value)
		}
	case MinLSNTimeoutVar:
		if _, err := parseStaleness(strings.Trim(st.Value, "'\"")); err != nil {
			return nil, err
		}
	case SearchConsistencyVar, SearchMaxLagVar:
		if err := checkSearchVar(name, strings.Trim(st.Value, "'\"")); err != nil {
			return nil, err
		}
	case LastLSNVar:
		return nil, errf("55000", "parameter %q cannot be changed", LastLSNVar)
	case ReadStalenessVar:
		if _, err := parseStaleness(strings.Trim(st.Value, "'\"")); err != nil {
			return nil, err
		}
	case "search_path":
		s.vars[name] = normSearchPath(st.Value)
		return &Result{Tag: "SET"}, nil
	case "sql_dialect":
		if val != "postgres" && val != "clickhouse" {
			return nil, errf(codeInvalidParameter, "invalid value for parameter \"sql_dialect\": %q (postgres or clickhouse)", val)
		}
		st.Value = val
	}
	s.vars[name] = strings.Trim(st.Value, "'\"")
	return &Result{Tag: "SET"}, nil
}

func (s *Session) createTable(c *CreateTable) (*Result, error) {
	if err := s.resolveUserTypes(c.Cols); err != nil {
		return nil, err
	}
	if c.Ext != nil && !c.Ext.done {
		return s.createTableExt(c, false)
	}
	if _, err := s.E.UserTable(c.Name); err == nil || s.isView(c.Name) {
		if c.IfNotExists {
			return &Result{Tag: "CREATE TABLE", Notice: fmt.Sprintf("relation %q already exists, skipping", c.Name)}, nil
		}
		return nil, errf(codeDuplicateTable, "relation %q already exists", c.Name)
	}
	t := schema.Table{Name: c.Name, Key: c.Key, KeyCols: c.KeyCols, PKName: c.PKName}
	s.extTable(c, &t)
	seen := map[string]bool{}
	for _, cd := range c.Cols {
		if cd.Name == schema.RowIDCol || cd.Name == schema.KeyCol {
			return nil, errf("42939", "column name %q is reserved", cd.Name)
		}
		if seen[cd.Name] {
			return nil, errf("42701", "column %q specified more than once", cd.Name)
		}
		seen[cd.Name] = true
		ty := cd.Type
		if cd.Name == c.Key && ty == "text" {
			ty = "string" // keys are exact-match strings
		}
		t.Columns = append(t.Columns, schema.Column{Name: cd.Name, Type: ty, PG: cd.PG, Domain: cd.Domain, NotNull: cd.NotNull || cd.Primary})
	}
	for k, v := range c.Options {
		switch k {
		case "columnar":
			t.Columnar = v == "true" || v == "on"
		case "append_only":
			t.AppendOnly = v == "true" || v == "on"
		case "search":
			for _, f := range strings.Split(v, ",") {
				if f = strings.TrimSpace(f); f != "" {
					t.Search = append(t.Search, f)
				}
			}
		case "trigram":
			for _, f := range strings.Split(v, ",") {
				if f = strings.TrimSpace(f); f != "" {
					t.Substr = append(t.Substr, schema.SubstrIndex{Name: c.Name + "_" + f + "_trgm_idx", Col: f})
				}
			}
		case "time_column":
			t.TimeColumn = v
		case "series_column":
			t.SeriesColumn = v
		case "history_retention":
			if err := setTableOptions(&t, map[string]string{k: v}); err != nil {
				return nil, err
			}
		default:
			return nil, errf("22023", "unrecognized table option %q", k)
		}
	}
	seqs, err := s.columnsWithSequences(c, &t)
	if err != nil {
		return nil, err
	}
	if q, err := s.hiddenKey(c, &t); err != nil {
		return nil, err
	} else if q != nil {
		seqs = append(seqs, *q)
	}
	if t.Checks, err = checksOf(c, &t); err != nil {
		return nil, err
	}
	if err := s.constraintsOf(c, &t); err != nil {
		return nil, err
	}
	if _, err := s.E.CreateTable(t); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			if c.IfNotExists {
				return &Result{Tag: "CREATE TABLE"}, nil
			}
			return nil, errf(codeDuplicateTable, "relation %q already exists", c.Name)
		}
		return nil, errf("42P16", "%v", err)
	}
	for _, q := range seqs {
		if err := s.E.CreateSequence(q); err != nil {
			return nil, AsError(err)
		}
	}
	return &Result{Tag: "CREATE TABLE"}, nil
}

func (s *Session) table(name string) (*schema.Table, error) {
	ts, err := s.userTable(name)
	if err == nil && ts.Schema().MatUnpopulated {
		if err := s.matGuard(ts.Schema()); err != nil {
			return nil, err
		}
	}
	if err != nil {
		if s.isView(name) {
			return nil, errf(codeObjectNotInState, "cannot change view %q", name)
		}
		return nil, errf(codeUndefinedTable, "relation %q does not exist", name)
	}
	return s.withTypes(ts.Schema()), nil
}

func (s *Session) run(tx *core.Tx, st Stmt, params []Datum) (*Result, error) {
	if heavyQ != nil && s.heavy(st, params) {
		var res *Result
		var err error
		defer admit(ClassHeavy)()
		onHeavyLane(func() { res, err = s.runHere(tx, st, params) })
		return res, err
	}
	// the slot covers execution only: an autocommit's fsync wait happens
	// after run returns
	defer admit(classOf(st))()
	return s.runHere(tx, st, params)
}

func (s *Session) runHere(tx *core.Tx, st Stmt, params []Datum) (*Result, error) {
	switch st := st.(type) {
	case *Select:
		if needsGeneral(st) {
			return s.runQuery(tx, st, params)
		}
		res, err := s.selectRows(tx, st, params)
		if err != nil && retryGeneral(err) {
			if r2, err2 := s.runQuery(tx, st, params); err2 == nil {
				return r2, nil
			}
		}
		if err != nil && (s.isSequence(st.From) || s.isView(st.From)) { // SELECT last_value FROM seq, or FROM a view
			return s.runQuery(tx, st, params)
		}
		return res, err
	case *SetOp:
		return s.runQuery(tx, st, params)
	case *Insert:
		if err := s.rejectMatWrite(st.Table); err != nil {
			return nil, err
		}
		return s.insert(tx, st, params)
	case *Update:
		if err := s.rejectMatWrite(st.Table); err != nil {
			return nil, err
		}
		return s.update(tx, st, params)
	case *Delete:
		if err := s.rejectMatWrite(st.Table); err != nil {
			return nil, err
		}
		return s.delete(tx, st, params)
	}
	return s.runExt(tx, st, params)
}

// ---- access paths ----

// maxKeyRange is the widest integer key range that is read as point
// lookups; wider ranges scan.
const maxKeyRange = 4096

// keyLookups returns the primary-key values that WHERE pins (key = c, or
// key IN (c, ...), as a top-level conjunct, or a narrow range of an integer
// key: c1 <= key < c2, key BETWEEN c1 AND c2), or ok=false for a full scan.
// The keys may be a superset of the matches: callers still apply WHERE.
func keyLookups(t *schema.Table, where Expr, ctx *evalCtx) ([]string, bool, error) {
	if where == nil {
		return nil, false, nil
	}
	var conj []Expr
	var split func(x Expr)
	split = func(x Expr) {
		if b, ok := x.(*Binary); ok && b.Op == "and" {
			split(b.L)
			split(b.R)
			return
		}
		conj = append(conj, x)
	}
	split(where)
	isKey := func(x Expr) bool {
		c, ok := x.(*ColRef)
		return ok && c.Name == t.Key && c.Table != "excluded"
	}
	constant := func(x Expr) bool {
		ok := true
		walk(x, func(e Expr) {
			switch e.(type) {
			case *ColRef:
				ok = false
			case *Func:
				if aggFuncs[e.(*Func).Name] {
					ok = false
				}
			}
		})
		return ok
	}
	keyKind := kindOf(t.Columns[t.KeyIndex()].T)
	toKey := func(x Expr) (string, bool, error) {
		v, err := ctx.eval(x)
		if err != nil || v.IsNull() {
			return "", false, err
		}
		if v, err = coerce(v, keyKind); err != nil {
			return "", false, err
		}
		if keyKind == KInt {
			return strconv.FormatInt(v.I, 10), true, nil
		}
		return v.S, true, nil
	}
	for _, c := range conj {
		switch e := c.(type) {
		case *Binary:
			if e.Op != "=" {
				continue
			}
			var other Expr
			switch {
			case isKey(e.L) && constant(e.R):
				other = e.R
			case isKey(e.R) && constant(e.L):
				other = e.L
			default:
				continue
			}
			k, ok, err := toKey(other)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				return nil, true, nil // key = NULL matches nothing
			}
			return []string{k}, true, nil
		case *InList:
			if e.Not || !isKey(e.X) {
				continue
			}
			all := true
			for _, it := range e.List {
				all = all && constant(it)
			}
			if !all {
				continue
			}
			var keys []string
			seen := map[string]bool{}
			for _, it := range e.List {
				k, ok, err := toKey(it)
				if err != nil {
					return nil, false, err
				}
				if ok && !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
			return keys, true, nil
		}
	}
	if keyKind != KInt {
		return nil, false, nil
	}
	// integer keys are stored as decimal strings, whose order is not numeric:
	// a narrow range becomes one lookup per value
	lo, hi := int64(math.MinInt64), int64(math.MaxInt64) // inclusive
	none := false
	bound := func(x Expr, op string) error {
		v, err := ctx.eval(x)
		if err != nil {
			return err
		}
		var l, h float64
		switch v.K {
		case KNull:
			none = true // comparisons with NULL match nothing
			return nil
		case KInt:
			l, h = float64(v.I), float64(v.I)
			if v.I > 1<<52 || v.I < -(1<<52) {
				return nil // not exact as float64: no bound
			}
		case KFloat:
			l, h = math.Floor(v.F), math.Ceil(v.F)
		default:
			return nil
		}
		if math.IsNaN(l) || math.Abs(l) > 1<<52 || math.Abs(h) > 1<<52 {
			return nil
		}
		switch op {
		case ">=", ">":
			lo = max(lo, int64(l))
		case "<=", "<":
			hi = min(hi, int64(h))
		}
		return nil
	}
	flip := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<="}
	for _, c := range conj {
		var err error
		switch e := c.(type) {
		case *Binary:
			if _, ok := flip[e.Op]; !ok {
				continue
			}
			switch {
			case isKey(e.L) && constant(e.R):
				err = bound(e.R, e.Op)
			case isKey(e.R) && constant(e.L):
				err = bound(e.L, flip[e.Op])
			}
		case *Between:
			if !e.Not && isKey(e.X) && constant(e.Lo) && constant(e.Hi) {
				if err = bound(e.Lo, ">="); err == nil {
					err = bound(e.Hi, "<=")
				}
			}
		}
		if err != nil {
			return nil, false, err
		}
	}
	switch {
	case none || lo > hi:
		return nil, true, nil
	case lo == math.MinInt64 || hi == math.MaxInt64 || uint64(hi-lo) >= maxKeyRange:
		return nil, false, nil
	}
	keys := make([]string, 0, hi-lo+1)
	for k := lo; k <= hi; k++ {
		keys = append(keys, strconv.FormatInt(k, 10))
	}
	return keys, true, nil
}

// scanRows calls fn for every row of t visible to tx that satisfies where.
func (s *Session) scanRows(tx *core.Tx, t *schema.Table, ctx *evalCtx, where Expr, fn func(schema.Row) (bool, error)) error {
	match := func(r schema.Row) (bool, error) {
		if where == nil {
			return true, nil
		}
		ctx.row = r
		v, err := ctx.evalBool(where)
		if err != nil {
			return false, err
		}
		return v.Bool(), nil
	}
	keys, point, err := keyLookups(t, where, ctx)
	if err != nil {
		return err
	}
	if point {
		for _, k := range keys {
			r, ok, err := tx.Get(t.Name, k)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if m, err := match(r); err != nil {
				return err
			} else if m {
				if cont, err := fn(r); err != nil || !cont {
					return err
				}
			}
		}
		return nil
	}
	if canUseIndex(tx, t) {
		alias := ""
		if ctx.sc != nil {
			alias = ctx.sc.alias
		}
		if c := chooseIndex(t, alias, where, ctx, nil); c != nil {
			if ok, err := s.scanIndex(tx, t, c, false, match, fn); err != nil || ok {
				return err
			}
		}
	}
	var ierr error
	err = tx.ScanAll(t.Name, func(_ string, r schema.Row) bool {
		m, err := match(r)
		if err != nil {
			ierr = err
			return false
		}
		if !m {
			return true
		}
		cont, err := fn(r)
		if err != nil {
			ierr = err
			return false
		}
		return cont
	})
	if ierr != nil {
		return ierr
	}
	return err
}

// ---- SELECT ----

type aggState struct {
	f        *Func
	count    int64
	sumI     int64
	sumF     float64
	sumN     pgnum.Dec // the numeric values' sum
	isNum    bool
	isFloat  bool
	best     Datum
	distinct map[string]bool
	pre      *Datum // the result, computed elsewhere (a view)
}

func (a *aggState) add(v Datum) error {
	if a.f.Star {
		a.count++
		return nil
	}
	if v.IsNull() {
		return nil
	}
	if a.distinct != nil {
		k := groupKey([]Datum{v})
		if a.distinct[k] {
			return nil
		}
		a.distinct[k] = true
	}
	a.count++
	switch a.f.Name {
	case "sum", "avg":
		switch v.K {
		case KInt:
			a.sumI += v.I
		case KFloat:
			a.isFloat = true
			a.sumF += v.F
		case KNumeric:
			a.isNum = true
			a.sumN = pgnum.Add(a.sumN, numOf(v))
		default:
			f, err := coerce(v, KFloat)
			if err != nil {
				return errf(codeUndefinedFunction, "function %s(%s) does not exist", a.f.Name, v.K)
			}
			a.isFloat = true
			a.sumF += f.F
		}
	case "min", "max":
		if a.best.IsNull() {
			a.best = v
			return nil
		}
		cmp, err := compare(v, a.best)
		if err != nil {
			return err
		}
		if a.f.Name == "min" && cmp < 0 || a.f.Name == "max" && cmp > 0 {
			a.best = v
		}
	}
	return nil
}

func (a *aggState) result() Datum {
	if a.pre != nil {
		return *a.pre
	}
	switch a.f.Name {
	case "count":
		return IntD(a.count)
	case "sum":
		if a.count == 0 {
			return Null
		}
		if a.isFloat {
			return FloatD(a.sumF + a.sumN.Float64() + float64(a.sumI))
		}
		if a.isNum {
			return NumericD(pgnum.Add(a.sumN, pgnum.FromInt64(a.sumI)))
		}
		return IntD(a.sumI)
	case "avg":
		if a.count == 0 {
			return Null
		}
		if !a.isFloat {
			x, _ := pgnum.Div(pgnum.Add(a.sumN, pgnum.FromInt64(a.sumI)), pgnum.FromInt64(a.count))
			return NumericD(x)
		}
		return FloatD((a.sumF + a.sumN.Float64() + float64(a.sumI)) / float64(a.count))
	}
	return a.best
}

// merge adds b, the same aggregate over other rows, into a.
func (a *aggState) merge(b *aggState) error {
	a.count += b.count
	a.sumI += b.sumI
	a.sumF += b.sumF
	a.sumN = pgnum.Add(a.sumN, b.sumN)
	a.isNum = a.isNum || b.isNum
	a.isFloat = a.isFloat || b.isFloat
	if b.best.IsNull() {
		return nil
	}
	if a.best.IsNull() {
		a.best = b.best
		return nil
	}
	cmp, err := compare(b.best, a.best)
	if err != nil {
		return err
	}
	if a.f.Name == "min" && cmp < 0 || a.f.Name == "max" && cmp > 0 {
		a.best = b.best
	}
	return nil
}

// groupParallel computes the groups of a full-scan GROUP BY on several
// cores: each worker aggregates the chunks it scans, then the partial
// groups merge. Groups come out in the order a sequential scan would first
// meet them. done=false when the query does not qualify (point lookups,
// DISTINCT aggregates, volatile functions, own writes to the table);
// the caller then aggregates sequentially. Float sums may differ in the
// last bits from a sequential sum, as with PostgreSQL's parallel aggregate.
func groupParallel(tx *core.Tx, st *Select, t *schema.Table, ctx *evalCtx, aggs []*Func) (map[string]*group, []string, bool, error) {
	helpers := min(ScanWorkers-1, int(scanHelpers.Load()))
	if helpers < 1 {
		return nil, nil, false, nil
	}
	for _, f := range aggs {
		switch f.Name {
		case "count", "sum", "avg", "min", "max":
		default:
			return nil, nil, false, nil
		}
		if f.Distinct || !f.Star && len(f.Args) != 1 {
			return nil, nil, false, nil
		}
	}
	volatile := false
	exprs := append([]Expr{st.Where}, st.GroupBy...)
	for _, f := range aggs {
		exprs = append(exprs, f.Args...)
	}
	for _, x := range exprs {
		if x != nil {
			walk(x, func(e Expr) {
				if f, ok := e.(*Func); ok && f.Name == "random" {
					volatile = true
				}
			})
		}
	}
	if volatile {
		return nil, nil, false, nil
	}
	if _, point, err := keyLookups(t, st.Where, ctx); err != nil || point {
		return nil, nil, false, err
	}

	// decode only the columns the statement mentions
	need := make([]bool, len(t.Columns))
	mark := func(x Expr) {
		walk(x, func(e Expr) {
			if c, ok := e.(*ColRef); ok {
				for i, col := range t.Columns {
					if col.Name == c.Name {
						need[i] = true
					}
				}
			}
		})
	}
	for _, it := range st.Items {
		if it.Star || it.X == nil {
			return nil, nil, false, nil
		}
		mark(it.X)
	}
	mark(st.Where)
	mark(st.Having)
	for _, g := range st.GroupBy {
		mark(g)
	}
	for _, o := range st.OrderBy {
		mark(o.X)
	}

	type pgroup struct {
		g     *group
		first int64 // seq<<32 | idx of the group's first row
	}
	n := helpers + 1
	local := make([]map[string]*pgroup, n)
	ctxs := make([]evalCtx, n)
	errs := make([]error, n)
	keyBuf := make([][]Datum, n) // per worker, reused for every row
	kb := make([][]byte, n)
	for w := range local {
		local[w] = map[string]*pgroup{}
		ctxs[w] = evalCtx{sc: ctx.sc, params: ctx.params, now: ctx.now, sess: ctx.sess}
	}
	ok, err := tx.ScanAllParallel(t.Name, need, helpers, spawnHelper, keepHelper, func(w, seq, idx int, r schema.Row) bool {
		c := &ctxs[w]
		c.row = r
		if st.Where != nil {
			v, err := c.evalBool(st.Where)
			if err != nil {
				errs[w] = err
				return false
			}
			if !v.Bool() {
				return true
			}
		}
		keys := keyBuf[w][:0]
		for _, g := range st.GroupBy {
			v, err := c.eval(g)
			if err != nil {
				errs[w] = err
				return false
			}
			keys = append(keys, v)
		}
		keyBuf[w] = keys
		kb[w] = appendGroupKey(kb[w][:0], keys)
		pg := local[w][string(kb[w])]
		if pg == nil {
			k := string(kb[w])
			// r and keys are reused for the next row: keep copies
			pg = &pgroup{g: &group{rep: append(schema.Row(nil), r...), keys: append([]Datum(nil), keys...)}, first: int64(seq)<<32 | int64(idx)}
			for _, f := range aggs {
				pg.g.aggs = append(pg.g.aggs, &aggState{f: f})
			}
			local[w][k] = pg
		}
		for _, a := range pg.g.aggs {
			var v Datum
			if !a.f.Star {
				var err error
				if v, err = c.eval(a.f.Args[0]); err != nil {
					errs[w] = err
					return false
				}
			}
			if err := a.add(v); err != nil {
				errs[w] = err
				return false
			}
		}
		return true
	})
	if err != nil || !ok {
		return nil, nil, false, err
	}
	for _, err := range errs {
		if err != nil {
			return nil, nil, false, err
		}
	}
	all := local[0]
	for _, m := range local[1:] {
		for k, pg := range m {
			cur := all[k]
			if cur == nil {
				all[k] = pg
				continue
			}
			for i, a := range cur.g.aggs {
				if err := a.merge(pg.g.aggs[i]); err != nil {
					return nil, nil, false, err
				}
			}
			if pg.first < cur.first {
				cur.first, cur.g.rep = pg.first, pg.g.rep
			}
		}
	}
	groups := make(map[string]*group, len(all))
	order := make([]string, 0, len(all))
	for k, pg := range all {
		groups[k] = pg.g
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool { return all[order[i]].first < all[order[j]].first })
	return groups, order, true, nil
}

type group struct {
	rep  schema.Row // a row of the group, for grouped column references
	keys []Datum
	aggs []*aggState
}

type outRow struct {
	vals []Datum
	sort []Datum
	lkey string // row key, for FOR UPDATE after ORDER BY (rowlock.go)
}

func (s *Session) selectRows(tx *core.Tx, st *Select, params []Datum) (*Result, error) {
	sc := &scope{}
	if st.From != "" {
		t, err := s.table(st.From)
		if err != nil {
			return nil, err
		}
		sc.t, sc.alias = t, st.Alias
	}
	ctx := &evalCtx{sc: sc, params: params, now: nowMicros(), sess: s}

	plan, err := st.plan(sc)
	if err != nil {
		return nil, err
	}
	items, cols, aggs := plan.items, plan.cols, plan.aggs
	grouped := len(aggs) > 0 || len(st.GroupBy) > 0
	if st.Where != nil && hasAgg(st.Where) {
		return nil, errf(codeGrouping, "aggregate functions are not allowed in WHERE")
	}
	if grouped {
		if err := checkGrouping(items, st, aggs); err != nil {
			return nil, err
		}
	}

	// ORDER BY items that name an output column (alias or ordinal)
	orderOut := make([]int, len(st.OrderBy))
	for i, o := range st.OrderBy {
		orderOut[i] = -1
		if l, ok := o.X.(*Lit); ok && l.V.K == KInt {
			if l.V.I < 1 || int(l.V.I) > len(items) {
				return nil, errf("42P10", "ORDER BY position %d is not in select list", l.V.I)
			}
			orderOut[i] = int(l.V.I) - 1
		} else if c, ok := o.X.(*ColRef); ok && c.Table == "" {
			// an output column name wins over an input column (PostgreSQL)
			for j, it := range items {
				if itemName(it) == c.Name && (it.Alias != "" || !s.clickHouse()) {
					orderOut[i] = j
					break
				}
			}
		}
	}

	// an unknown column is an error even when no row is read (42703)
	if sc.t != nil {
		var cerr error
		check := func(x Expr) {
			walk(x, func(e Expr) {
				if c, ok := e.(*ColRef); ok && cerr == nil {
					_, cerr = sc.resolve(c)
				}
			})
		}
		for _, it := range items {
			check(it.X)
		}
		check(st.Where)
		check(st.Having)
		for i, o := range st.OrderBy {
			if orderOut[i] < 0 {
				check(o.X)
			}
		}
		if cerr != nil {
			return nil, cerr
		}
	}

	limit, offset := int64(-1), int64(0)
	if st.Limit != nil {
		v, err := ctx.eval(st.Limit)
		if err != nil {
			return nil, err
		}
		if !v.IsNull() {
			if v, err = coerce(v, KInt); err != nil {
				return nil, err
			}
			limit = v.I
			if limit < 0 && !s.clickHouse() {
				return nil, errf("2201W", "LIMIT must not be negative")
			}
		}
	}
	if st.Offset != nil {
		v, err := ctx.eval(st.Offset)
		if err != nil {
			return nil, err
		}
		if v, err = coerce(v, KInt); err != nil {
			return nil, err
		}
		offset = v.I
	}

	lk, err := s.fastRowLocker(tx, st, sc.t, sc.alias)
	if err != nil {
		return nil, err
	}
	var out []outRow
	project := func() error {
		r := outRow{vals: make([]Datum, len(items))}
		if lk != nil && !lk.inVisit {
			r.lkey = sc.t.KeyOf(ctx.row)
		}
		for i, it := range items {
			v, err := ctx.eval(it.X)
			if err != nil {
				return err
			}
			r.vals[i] = v
		}
		for i, o := range st.OrderBy {
			if orderOut[i] >= 0 {
				r.sort = append(r.sort, r.vals[orderOut[i]])
				continue
			}
			v, err := ctx.eval(o.X)
			if err != nil {
				return err
			}
			r.sort = append(r.sort, v)
		}
		out = append(out, r)
		return nil
	}

	// stream rows, stopping early when the order of rows does not matter
	early := !grouped && len(st.OrderBy) == 0 && !st.Distinct && limit >= 0
	// ... or when an index delivers them in the order asked for
	ordStop := false
	var ordIx *indexChoice
	ordDesc := false
	if !grouped && !st.Distinct && limit > 0 && len(st.OrderBy) > 0 && sc.t != nil && len(sc.t.Indexes) > 0 && canUseIndex(tx, sc.t) {
		if need, desc := orderColumns(st, items, orderOut, sc.t, sc.alias); need != nil {
			ordIx, ordDesc = chooseIndex(sc.t, sc.alias, st.Where, ctx, need), desc
			if plain := chooseIndex(sc.t, sc.alias, st.Where, ctx, nil); plain != nil && (ordIx == nil || plain.score > ordIx.score) {
				ordIx = nil // WHERE has a better index; the rows are sorted afterwards
			}
			ordStop = ordIx != nil && lk == nil
		}
	}
	var groups map[string]*group
	var order []string
	if grouped {
		groups = map[string]*group{}
	}
	visit := func(r schema.Row) (bool, error) {
		ctx.row = r
		if !grouped {
			if lk != nil && lk.inVisit {
				if ok, err := lk.lockRowOf(r); err != nil || !ok {
					return err == nil, err
				}
			}
			if err := project(); err != nil {
				return false, err
			}
			return !(early || ordStop) || int64(len(out)) < offset+limit, nil
		}
		keys := make([]Datum, len(st.GroupBy))
		for i, g := range st.GroupBy {
			v, err := ctx.eval(g)
			if err != nil {
				return false, err
			}
			keys[i] = v
		}
		k := groupKey(keys)
		g := groups[k]
		if g == nil {
			g = &group{rep: r, keys: keys}
			for _, f := range aggs {
				a := &aggState{f: f}
				if f.Distinct {
					a.distinct = map[string]bool{}
				}
				g.aggs = append(g.aggs, a)
			}
			groups[k] = g
			order = append(order, k)
		}
		for _, a := range g.aggs {
			var v Datum
			if !a.f.Star {
				if len(a.f.Args) != 1 {
					return false, errf(codeUndefinedFunction, "function %s requires one argument", a.f.Name)
				}
				var err error
				if v, err = ctx.eval(a.f.Args[0]); err != nil {
					return false, err
				}
			}
			if err := a.add(v); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if sc.t == nil {
		if st.Where != nil {
			v, err := ctx.evalBool(st.Where)
			if err != nil {
				return nil, err
			}
			if v.Bool() {
				if _, err := visit(nil); err != nil {
					return nil, err
				}
			}
		} else if _, err := visit(nil); err != nil {
			return nil, err
		}
	} else if limit != 0 || grouped {
		done := false
		if grouped {
			g, o, ok, err := s.viewGroups(tx, st, sc.t, ctx, aggs, items, orderOut, limit, offset)
			if err != nil {
				return nil, err
			}
			if ok {
				groups, order, done = g, o, true
			}
		}
		cand := s.substrPrepare(tx, sc.t, sc, ctx, st.Where)
		// an equality or IN on an index beats the parallel scan of a whole
		// table (a range may match most of it, which the scan reads faster)
		selective := false
		if grouped && !done && cand == nil && len(sc.t.Indexes) > 0 && canUseIndex(tx, sc.t) {
			c := chooseIndex(sc.t, sc.alias, st.Where, ctx, nil)
			selective = c != nil && c.score >= 2
		}
		if grouped && !done && cand == nil && !selective {
			g, o, ok, err := groupParallel(tx, st, sc.t, ctx, aggs)
			if err != nil {
				return nil, err
			}
			if ok {
				groups, order, done = g, o, true
			}
		}
		if !done && cand == nil && ordIx != nil {
			ok, err := s.scanIndex(tx, sc.t, ordIx, ordDesc, matcher(ctx, st.Where), visit)
			if err != nil {
				return nil, err
			}
			if done = ok; !ok {
				ordStop, out = false, nil // the index cannot serve this snapshot
			}
		}
		if !done && cand != nil {
			hint := int64(0)
			if !grouped && len(st.OrderBy) == 0 && !st.Distinct && limit > 0 {
				hint = limit + offset
			}
			if err := s.scanCandidates(tx, cand, ctx, st.Where, hint, visit); err != nil {
				return nil, err
			}
		} else if !done {
			if err := s.scanRows(tx, sc.t, ctx, st.Where, visit); err != nil {
				return nil, err
			}
		}
	}

	if grouped {
		if len(order) == 0 && len(st.GroupBy) == 0 {
			// aggregate over no rows still yields one row
			g := &group{}
			for _, f := range aggs {
				g.aggs = append(g.aggs, &aggState{f: f})
			}
			groups = map[string]*group{"": g}
			order = []string{""}
		}
		for _, k := range order {
			g := groups[k]
			ctx.row = g.rep
			ctx.aggs = map[*Func]Datum{}
			for _, a := range g.aggs {
				ctx.aggs[a.f] = a.result()
			}
			if st.Having != nil {
				v, err := ctx.evalBool(st.Having)
				if err != nil {
					return nil, err
				}
				if !v.Bool() {
					continue
				}
			}
			if err := project(); err != nil {
				return nil, err
			}
		}
	}

	if st.Distinct {
		seen := map[string]bool{}
		dst := out[:0]
		for _, r := range out {
			k := groupKey(r.vals)
			if !seen[k] {
				seen[k] = true
				dst = append(dst, r)
			}
		}
		out = dst
	}
	if len(st.OrderBy) > 0 {
		var serr error
		sort.SliceStable(out, func(i, j int) bool {
			for k, o := range st.OrderBy {
				a, b := out[i].sort[k], out[j].sort[k]
				var c int
				switch {
				case a.IsNull() && b.IsNull():
					c = 0
				case a.IsNull():
					c = 1 // NULLS LAST for ASC, FIRST for DESC (PostgreSQL default)
				case b.IsNull():
					c = -1
				default:
					var err error
					if c, err = compare(a, b); err != nil && serr == nil {
						serr = err
					}
				}
				if o.Desc {
					c = -c
				}
				if c != 0 {
					return c < 0
				}
			}
			return false
		})
		if serr != nil {
			return nil, serr
		}
	}
	if lk != nil && !lk.inVisit { // ORDER BY: lock in output order, until OFFSET+LIMIT rows are locked
		kept := out[:0]
		for _, o := range out {
			if limit >= 0 && int64(len(kept)) >= offset+limit {
				break
			}
			if ok, err := lk.lockKeys([]string{o.lkey}, []bool{true}); err != nil {
				return nil, err
			} else if ok {
				kept = append(kept, o)
			}
		}
		out = kept
	}
	if offset > 0 {
		out = out[min(int(offset), len(out)):]
	}
	if limit >= 0 && int64(len(out)) > limit {
		out = out[:limit]
	}
	res := &Result{Cols: cols, HasRows: true, Tag: fmt.Sprintf("SELECT %d", len(out))}
	res.Rows = make([][]Datum, len(out))
	for i, r := range out {
		res.Rows[i] = conform(r.vals, cols)
	}
	return res, nil
}

// selectPlan is the statement-level analysis of a SELECT: the expanded
// output items, their types, and the aggregate calls. It depends only on
// the statement and the table schema, so prepared statements compute it
// once instead of on every execution.
type selectPlan struct {
	t     *schema.Table
	items []SelItem
	cols  []Column
	aggs  []*Func
}

func (st *Select) plan(sc *scope) (*selectPlan, error) {
	if p := st.cached; p != nil && p.t == sc.t {
		return p, nil
	}
	p := &selectPlan{t: sc.t}
	for _, it := range st.Items {
		if it.Star {
			if sc.t == nil {
				return nil, errf(codeSyntax, "SELECT * with no tables specified is not valid")
			}
			for _, c := range sc.t.Columns {
				if !c.Hidden {
					p.items = append(p.items, SelItem{X: &ColRef{Name: c.Name}, Alias: c.Name})
				}
			}
			continue
		}
		p.items = append(p.items, it)
	}
	p.cols = make([]Column, len(p.items))
	pk := paramKinds(st, sc)
	for _, it := range p.items {
		markWidths(it.X, sc.leafTy)
	}
	for i, it := range p.items {
		p.cols[i] = itemCol(it, sc, pk)
	}
	// aggregates anywhere in the select list, HAVING or ORDER BY
	collect := func(x Expr) {
		walk(x, func(e Expr) {
			if f, ok := e.(*Func); ok && aggFuncs[f.Name] {
				p.aggs = append(p.aggs, f)
			}
		})
	}
	for _, it := range p.items {
		collect(it.X)
	}
	collect(st.Having)
	for _, o := range st.OrderBy {
		collect(o.X)
	}
	st.cached = p
	return p, nil
}

func (sc *scope) colIndex(name string) (int, bool) {
	if sc.t == nil {
		return 0, false
	}
	return sc.t.Col(name)
}

// conform converts values to the declared column kinds where they differ
// (e.g. an untyped text parameter returned as a bigint column).
func conform(vals []Datum, cols []Column) []Datum {
	for i := range vals {
		if !vals[i].IsNull() && vals[i].K != cols[i].Kind {
			if v, err := coerce(vals[i], cols[i].Kind); err == nil {
				vals[i] = v
			}
		}
	}
	return vals
}

func itemName(it SelItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	switch e := it.X.(type) {
	case *ColRef:
		return e.Name
	case *Func:
		if e.Disp != "" {
			return e.Disp
		}
		if (e.Name == "stask_subscript" || e.Name == "stask_slice") && len(e.Args) > 0 { // (ARRAY[1,2])[3] is named by the array
			return itemName(SelItem{X: e.Args[0]})
		}
		return e.Name
	case *Cast:
		// the operand's name when it has one, else the type's
		if n := castOperandName(e.X); n != "?column?" {
			return n
		}
		return castName(e)
	case *Case:
		return "case"
	case *Subquery:
		if s, ok := e.Q.(*Select); ok && !e.Array && len(s.Items) == 1 {
			return itemName(s.Items[0])
		}
		if e.Array {
			return "array"
		}
	}
	return "?column?"
}

// checkGrouping rejects column references outside aggregates that are not
// GROUP BY expressions.
func checkGrouping(items []SelItem, st *Select, aggs []*Func) error {
	grouped := map[string]bool{}
	for _, g := range st.GroupBy {
		grouped[exprKey(g)] = true
		if c, ok := g.(*ColRef); ok {
			grouped["col:"+c.Name] = true
		}
	}
	var check func(x Expr) error
	check = func(x Expr) error {
		if x == nil || grouped[exprKey(x)] {
			return nil
		}
		switch e := x.(type) {
		case *ColRef:
			if !grouped["col:"+e.Name] {
				return errf(codeGrouping, "column %q must appear in the GROUP BY clause or be used in an aggregate function", e.Name)
			}
		case *Func:
			if aggFuncs[e.Name] {
				return nil
			}
			for _, a := range e.Args {
				if err := check(a); err != nil {
					return err
				}
			}
		case *Unary:
			return check(e.X)
		case *Binary:
			if err := check(e.L); err != nil {
				return err
			}
			return check(e.R)
		case *IsNull:
			return check(e.X)
		case *InList:
			if err := check(e.X); err != nil {
				return err
			}
			for _, a := range e.List {
				if err := check(a); err != nil {
					return err
				}
			}
		case *Between:
			for _, a := range []Expr{e.X, e.Lo, e.Hi} {
				if err := check(a); err != nil {
					return err
				}
			}
		case *Like:
			if err := check(e.X); err != nil {
				return err
			}
			return check(e.Pat)
		case *Cast:
			return check(e.X)
		case *Case:
			if err := check(e.Operand); err != nil {
				return err
			}
			for _, w := range e.Whens {
				if err := check(w.Cond); err != nil {
					return err
				}
				if err := check(w.Then); err != nil {
					return err
				}
			}
			return check(e.Else)
		}
		return nil
	}
	for _, it := range items {
		if err := check(it.X); err != nil {
			return err
		}
	}
	if err := check(st.Having); err != nil {
		return err
	}
	for _, o := range st.OrderBy {
		if _, isLit := o.X.(*Lit); isLit {
			continue
		}
		if c, ok := o.X.(*ColRef); ok {
			aliased := false
			for _, it := range items {
				aliased = aliased || it.Alias == c.Name
			}
			if aliased {
				continue
			}
		}
		if err := check(o.X); err != nil {
			return err
		}
	}
	return nil
}

// exprKey is a structural identity for expressions (GROUP BY matching).
func exprKey(x Expr) string { return fmt.Sprintf("%#v", stripPtrs(x)) }

func stripPtrs(x Expr) any {
	switch e := x.(type) {
	case *ColRef:
		return "col:" + e.Name
	case *Lit:
		return e.V
	case *Param:
		return fmt.Sprintf("$%d", e.N)
	case *Unary:
		return []any{e.Op, stripPtrs(e.X)}
	case *Binary:
		return []any{e.Op, stripPtrs(e.L), stripPtrs(e.R)}
	case *Func:
		args := []any{e.Name, e.Star, e.Distinct}
		for _, a := range e.Args {
			args = append(args, stripPtrs(a))
		}
		return args
	case *Cast:
		return []any{"cast", e.Type, stripPtrs(e.X)}
	}
	if k, ok := stripPtrsMore(x); ok {
		return k
	}
	return fmt.Sprintf("%p", x)
}

// ---- INSERT / UPDATE / DELETE ----

func (s *Session) buildRow(t *schema.Table, cols []int, vals []Datum) (schema.Row, error) {
	r := make(schema.Row, len(t.Columns))
	for i := range r {
		r[i].Null = true
	}
	for i, ci := range cols {
		v, err := toValueCol(&t.Columns[ci], vals[i])
		if err != nil {
			return nil, err
		}
		r[ci] = v
	}
	return r, checkRow(t, r)
}

func checkRow(t *schema.Table, r schema.Row) error {
	if err := checkUserCols(t, r); err != nil {
		return err
	}
	for i, c := range t.Columns {
		if r[i].Null && (c.NotNull || i == t.KeyIndex()) && !(c.Hidden && t.Composite()) {
			return errf(codeNotNullViolation, "null value in column %q of relation %q violates not-null constraint", c.Name, t.Name)
		}
	}
	if len(t.Checks) > 0 {
		return checkConstraints(t, r)
	}
	return nil
}

func (s *Session) returning(sc *scope, ctx *evalCtx, items []SelItem, r schema.Row, res *Result) error {
	if items == nil {
		return nil
	}
	ctx.row = r
	var vals []Datum
	for _, it := range items {
		if it.Star {
			for i, c := range sc.t.Columns {
				if !c.Hidden {
					vals = append(vals, fromCol(&c, r[i]))
				}
			}
			continue
		}
		v, err := ctx.eval(it.X)
		if err != nil {
			return err
		}
		vals = append(vals, v)
	}
	res.Rows = append(res.Rows, conform(vals, res.Cols))
	return nil
}

func returningCols(sc *scope, items []SelItem, pk map[int]Kind) []Column {
	var cols []Column
	for _, it := range items {
		if it.Star {
			for i := range sc.t.Columns {
				if !sc.t.Columns[i].Hidden {
					cols = append(cols, tableCol(&sc.t.Columns[i]).withTable(sc.t))
				}
			}
			continue
		}
		cols = append(cols, itemCol(it, sc, pk))
	}
	return cols
}

func (s *Session) insert(tx *core.Tx, st *Insert, params []Datum) (*Result, error) {
	if st.Query != nil {
		return s.insertQuery(tx, st, params)
	}
	t, err := s.table(st.Table)
	if err != nil {
		return nil, err
	}
	sc := &scope{t: t}
	ctx := &evalCtx{sc: sc, params: params, now: nowMicros(), sess: s}
	tgs := s.E.Triggers(t.Name)
	var cols []int
	if st.Cols == nil {
		cols = t.VisibleCols()
	} else {
		seen := map[int]bool{}
		for _, c := range st.Cols {
			i, ok := t.Col(c)
			if !ok {
				return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", c, t.Name)
			}
			if seen[i] {
				return nil, errf("42701", "column %q specified more than once", c)
			}
			seen[i] = true
			cols = append(cols, i)
		}
	}
	res := &Result{HasRows: st.Returning != nil}
	if st.Returning != nil {
		res.Cols = returningCols(sc, st.Returning, paramKinds(st, sc))
	}
	if st.OnConflict != nil {
		if err := checkConflictTarget(t, st.OnConflict.Target); err != nil {
			return nil, err
		}
	}
	n := 0
	inserted := make(map[string]bool, len(st.Rows))
	// A plain INSERT that is its own transaction needs no lookup here: the
	// commit rejects existing keys (as 23505) and nothing is visible before.
	// In an explicit transaction the error must come from this statement.
	commitChecks := st.OnConflict == nil && !s.explicit
	defaults := hasDefaults(t)
	if commitChecks {
		tx.Grow(len(st.Rows))
	}
	rec := s.rep
	if rec != nil && len(st.Rows) < repairMinRows {
		rec = nil
	}
	for ri, exprs := range st.Rows {
		if len(exprs) > len(cols) {
			return nil, errf(codeSyntax, "INSERT has more expressions than target columns")
		}
		if len(exprs) < len(cols) && st.Cols != nil {
			return nil, errf(codeSyntax, "INSERT has more target columns than expressions")
		}
		rc := cols[:len(exprs)]
		var vals []Datum
		slow := defaults
		for _, x := range exprs {
			if _, ok := x.(*DefaultExpr); ok {
				slow = true
			}
		}
		if slow {
			if rc, vals, err = s.rowValues(t, cols, exprs, ctx, st.Overriding); err != nil {
				return nil, err
			}
		} else {
			vals = make([]Datum, len(exprs))
			for i, x := range exprs {
				if vals[i], err = ctx.eval(x); err != nil {
					return nil, err
				}
			}
		}
		row, err := s.buildRow(t, rc, vals)
		if err != nil {
			return nil, err
		}
		if tgs != nil {
			if row, err = s.fireTriggers(tx, t, tgs, "before", "insert", nil, row); err != nil {
				return nil, err
			}
		}
		key := t.KeyOf(row)
		if rec != nil {
			rec.note(st, key, ri)
		}
		if commitChecks {
			if inserted[key] {
				return nil, dupKeyErr(t, key)
			}
			if err := tx.InsertUniqueAs(t.Name, t, row); err != nil {
				return nil, err
			}
			if tgs != nil {
				if _, err := s.fireTriggers(tx, t, tgs, "after", "insert", nil, row); err != nil {
					return nil, err
				}
			}
			inserted[key] = true
			n++
			if err := s.returning(sc, ctx, st.Returning, row, res); err != nil {
				return nil, err
			}
			continue
		}
		existing, exists, err := tx.Get(t.Name, key)
		if err != nil {
			return nil, err
		}
		if exists {
			switch {
			case st.OnConflict == nil || inserted[key] && st.OnConflict.Set != nil:
				if inserted[key] && st.OnConflict != nil {
					return nil, errf("21000", "ON CONFLICT DO UPDATE command cannot affect row a second time")
				}
				return nil, dupKeyErr(t, key)
			case st.OnConflict.DoNothing:
				continue
			}
			ctx.row, ctx.excluded = existing, row
			if st.OnConflict.Where != nil {
				v, err := ctx.evalBool(st.OnConflict.Where)
				if err != nil {
					return nil, err
				}
				if !v.Bool() {
					continue
				}
			}
			upd, err := s.applySet(t, ctx, existing, st.OnConflict.Set)
			if err != nil {
				return nil, err
			}
			if t.KeyOf(upd) != key {
				return nil, errf(codeFeature, "ON CONFLICT DO UPDATE cannot change the primary key")
			}
			if upd, err = s.updateRow(tx, t, tgs, existing, upd); err != nil {
				return nil, err
			}
			row = upd
		} else if err := tx.InsertAs(t, row); err != nil {
			return nil, err
		} else if tgs != nil {
			if _, err := s.fireTriggers(tx, t, tgs, "after", "insert", nil, row); err != nil {
				return nil, err
			}
		}
		inserted[key] = true
		n++
		ctx.excluded = nil
		if err := s.returning(sc, ctx, st.Returning, row, res); err != nil {
			return nil, err
		}
	}
	res.Tag = fmt.Sprintf("INSERT 0 %d", n)
	return res, nil
}

func (s *Session) applySet(t *schema.Table, ctx *evalCtx, old schema.Row, set []Assign) (schema.Row, error) {
	ctx.row = old
	upd := append(schema.Row(nil), old...)
	for _, a := range set {
		i, ok := t.Col(a.Col)
		if !ok {
			return nil, errf(codeUndefinedColumn, "column %q of relation %q does not exist", a.Col, t.Name)
		}
		var v Datum
		var err error
		if _, ok := a.X.(*DefaultExpr); ok {
			v, err = s.columnDefault(t, i, ctx.now)
		} else if t.Columns[i].Identity == "always" {
			return nil, errf("428C9", "column %q can only be updated to DEFAULT", a.Col)
		} else {
			v, err = ctx.eval(a.X)
		}
		if err != nil {
			return nil, err
		}
		if upd[i], err = toValueCol(&t.Columns[i], v); err != nil {
			return nil, err
		}
	}
	return upd, checkRow(t, upd)
}

// matching materializes the rows to modify before writing, so that a
// statement never sees its own changes.
func (s *Session) matching(tx *core.Tx, t *schema.Table, ctx *evalCtx, where Expr) ([]schema.Row, error) {
	var rows []schema.Row
	err := s.scanRows(tx, t, ctx, where, func(r schema.Row) (bool, error) {
		rows = append(rows, r)
		return true, nil
	})
	return rows, err
}

func (s *Session) update(tx *core.Tx, st *Update, params []Datum) (*Result, error) {
	t, err := s.table(st.Table)
	if err != nil {
		return nil, err
	}
	if st.From != nil || hasSubquery(st.Where) || setHasSubquery(st.Set) {
		return s.updateGeneral(tx, t, st, params)
	}
	sc := &scope{t: t}
	ctx := &evalCtx{sc: sc, params: params, now: nowMicros(), sess: s}
	rows, err := s.matching(tx, t, ctx, st.Where)
	if err != nil {
		return nil, err
	}
	tgs := s.E.Triggers(t.Name)
	res := &Result{HasRows: st.Returning != nil}
	if st.Returning != nil {
		res.Cols = returningCols(sc, st.Returning, paramKinds(st, sc))
	}
	for _, old := range rows {
		upd, err := s.applySet(t, ctx, old, st.Set)
		if err != nil {
			return nil, err
		}
		if upd, err = s.updateRow(tx, t, tgs, old, upd); err != nil {
			return nil, err
		}
		if err := s.returning(sc, ctx, st.Returning, upd, res); err != nil {
			return nil, err
		}
	}
	res.Tag = fmt.Sprintf("UPDATE %d", len(rows))
	return res, nil
}

func (s *Session) delete(tx *core.Tx, st *Delete, params []Datum) (*Result, error) {
	t, err := s.table(st.Table)
	if err != nil {
		return nil, err
	}
	if st.Using != nil || hasSubquery(st.Where) {
		return s.deleteGeneral(tx, t, st, params)
	}
	sc := &scope{t: t}
	ctx := &evalCtx{sc: sc, params: params, now: nowMicros(), sess: s}
	rows, err := s.matching(tx, t, ctx, st.Where)
	if err != nil {
		return nil, err
	}
	tgs := s.E.Triggers(t.Name)
	res := &Result{HasRows: st.Returning != nil}
	if st.Returning != nil {
		res.Cols = returningCols(sc, st.Returning, paramKinds(st, sc))
	}
	for _, r := range rows {
		if err := s.deleteRow(tx, t, tgs, r); err != nil {
			return nil, err
		}
		if err := s.returning(sc, ctx, st.Returning, r, res); err != nil {
			return nil, err
		}
	}
	res.Tag = fmt.Sprintf("DELETE %d", len(rows))
	return res, nil
}

// dropOrTruncate runs DROP TABLE and TRUNCATE. As in PostgreSQL, every
// table must exist (or IF EXISTS skips it) before any is changed; the
// tables are then changed one at a time.
func (s *Session) dropOrTruncate(st Stmt) (*Result, error) {
	var names []string
	ifExists, tag, restart, cascade := false, "TRUNCATE TABLE", false, false
	switch st := st.(type) {
	case *DropTable:
		names, ifExists, tag, cascade = st.Names, st.IfExists, "DROP TABLE", st.Cascade
	case *Truncate:
		names, restart = st.Names, st.Restart
	}
	names = s.mapTemps(names)
	res := &Result{Tag: tag}
	if tr, ok := st.(*Truncate); ok && tr.Cascade {
		var added []string
		if names, added = s.truncateClosure(names); len(added) > 0 {
			res.Notice = "truncate cascades to table " + strings.Join(added, ", ")
		}
	}
	var todo []string
	for _, n := range names {
		if ts, err := s.userTable(n); err == nil && ts.Schema().MatView != "" && !s.dropsMat(st) {
			return nil, errf(codeWrongObject, "%q is not a table", n)
		}
		if _, err := s.userTable(n); err != nil {
			if s.isView(n) {
				return nil, errf(codeWrongObjectType, "%q is not a table", n)
			}
			if !ifExists {
				return nil, errf(codeUndefinedTable, "table %q does not exist", n)
			}
			res.Notice = fmt.Sprintf("table %q does not exist, skipping", n)
			continue
		}
		todo = append(todo, n)
	}
	for _, n := range todo {
		for _, r := range s.E.FKRefs(n) {
			if r.Child == n || slices.Contains(todo, r.Child) {
				continue
			}
			switch {
			case tag != "DROP TABLE":
				return nil, errf(codeFeature, "cannot truncate a table referenced in a foreign key constraint: table %q references %q via constraint %q", r.Child, n, r.FK.Name)
			case !cascade:
				return nil, errf(codeDependentObjects, "cannot drop table %s because other objects depend on it: constraint %s on table %s depends on table %s", n, r.FK.Name, r.Child, n)
			}
			if _, err := s.dropConstraint(r.Child, r.FK.Name); err != nil {
				return nil, AsError(err)
			}
			res.Notice = strings.Trim(res.Notice+"; drop cascades to constraint "+r.FK.Name+" on table "+r.Child, "; ")
		}
	}
	if tag == "DROP TABLE" {
		note, err := s.dropTableViews(todo, cascade) // 2BP01 unless CASCADE
		if err != nil {
			return nil, err
		}
		if note != "" {
			res.Notice = strings.Trim(res.Notice+"; "+note, "; ")
		}
	}
	for _, n := range todo {
		var err error
		if tag == "DROP TABLE" {
			if err = s.E.DropTable(n); err == nil {
				err = s.E.DropTableTriggers(n)
				if err == nil {
					err = s.E.MoveGrants(n, "") // privileges do not outlive the table
				}
			}
		} else {
			err = s.E.TruncateTable(n)
		}
		if err != nil {
			return nil, AsError(err)
		}
		// the sequences of SERIAL/IDENTITY columns go with the table
		for _, q := range s.tableSequences(n) {
			switch {
			case tag == "DROP TABLE":
				err = s.E.DropSequence(q.Name)
			case restart:
				err = s.E.AlterSequence(q.Name, func(d *core.Sequence) (*int64, error) { return &d.Start, nil })
			}
			if err != nil {
				return nil, AsError(err)
			}
		}
	}
	s.forgetDropped()
	return res, nil
}
