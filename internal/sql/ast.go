package sql

import "sync/atomic"

// ---- statements ----

type Stmt interface{ stmt() }

type ColDef struct {
	Collation string // COLLATE name (collddl.go)
	Name      string
	Type      string // schema type: int, float, string, text, time, bool
	PG        string // declared PostgreSQL type when Type cannot tell (pgtype.go)
	Domain    string // the domain of the column (ddlobjs_types.go)
	Primary   bool
	PKName    string // CONSTRAINT name PRIMARY KEY (column constraint)
	NotNull   bool
	Default   string // SQL text of DEFAULT
	Checks    []CheckDef
	// a SERIAL type or an IDENTITY column owns a sequence of IntType
	Serial   bool
	Identity string   // "always" or "by default"
	IntType  string   // smallint, integer or bigint
	SeqOpts  *SeqOpts // IDENTITY (sequence options)
	// column constraints UNIQUE and REFERENCES (parse_index.go)
	Constraints []*ConstraintDef
}

// SeqOpts are the options of CREATE/ALTER SEQUENCE; nil fields are not set.
type SeqOpts struct {
	Type                 string
	Inc, Min, Max, Start *int64
	NoMin, NoMax         bool
	Cycle                *bool
	Restart              bool   // ALTER SEQUENCE ... RESTART
	RestartWith          *int64 // RESTART WITH n
}

type CreateSequence struct {
	Name        string
	IfNotExists bool
	Opts        SeqOpts
}

type DropSequence struct {
	Names    []string
	IfExists bool
}

// AlterIndex is ALTER INDEX name SET (index_storage = 'disk'|'memory').
type AlterIndex struct {
	Name     string
	IfExists bool
	Opts     map[string]string
}

type AlterSequence struct {
	Name     string
	IfExists bool
	Opts     SeqOpts
}

// AlterTable is ALTER TABLE with one or more actions.
type AlterTable struct {
	Name     string
	IfExists bool
	Actions  []AlterAction
}

// AlterAction is one action of ALTER TABLE. Kind is one of: add_column,
// drop_column, rename_column, rename_table, set_default, drop_default,
// set_not_null, drop_not_null, set_type, add_check, drop_constraint, owner.
type AlterAction struct {
	Kind       string
	Col        ColDef   // add_column
	Column     string   // the column acted on
	KeyCols    []string // add_pk: the PRIMARY KEY columns
	NewName    string   // rename_column, rename_table
	Expr       string   // set_default
	Type       string   // set_type: the new type (schema type name)
	PG         string   // set_type: its PostgreSQL spelling (pgtype.go)
	Using      bool     // set_type: USING col::type was given
	Collation  string   // set_type: COLLATE name
	Check      CheckDef
	Constraint *ConstraintDef    // add_constraint (UNIQUE, FOREIGN KEY)
	IfExists   bool              // ADD COLUMN IF NOT EXISTS, DROP ... IF EXISTS
	Opts       map[string]string // set_options: SET (name = value, ...)
}

// DefaultExpr is DEFAULT in INSERT ... VALUES or UPDATE ... SET.
type DefaultExpr struct{}

// CheckDef is a CHECK constraint: Expr is its SQL text, Name may be empty.
type CheckDef struct{ Name, Expr string }

type CreateTable struct {
	Name        string
	Cols        []ColDef
	Checks      []CheckDef       // table constraints
	Constraints []*ConstraintDef // UNIQUE and FOREIGN KEY, table and column constraints
	Key         string
	KeyCols     []string // composite PRIMARY KEY (two columns or more); Key is then empty
	PKName      string   // CONSTRAINT name PRIMARY KEY
	IfNotExists bool
	Options     map[string]string // WITH (columnar = true, search = 'a,b', ...)
	Ext         *tableExt         // TEMP, UNLOGGED, ON COMMIT, LIKE (ctas.go)
	Part        *PartSpec         // PARTITION BY (partition.go)
}

type Assign struct {
	Col string
	X   Expr
}

type OnConflict struct {
	Target    []string // ON CONFLICT (cols); nil: none given
	DoNothing bool
	Set       []Assign
	Where     Expr
}

type Insert struct {
	Table      string
	Cols       []string
	Rows       [][]Expr
	Query      Stmt // INSERT ... SELECT
	OnConflict *OnConflict
	Returning  []SelItem
	Overriding string // OVERRIDING SYSTEM|USER VALUE
}

type Update struct {
	Table     string
	Alias     string
	From      FromNode // UPDATE ... FROM
	Set       []Assign
	Where     Expr
	Returning []SelItem
}

type Delete struct {
	Table     string
	Alias     string
	Using     FromNode // DELETE ... USING
	Where     Expr
	Returning []SelItem
}

type SelItem struct {
	X     Expr // nil for *
	Alias string
	Star  bool
}

type OrderItem struct {
	X     Expr
	Desc  bool
	Nulls int        // 0 default (last ascending, first descending), 1 first, 2 last
	Coll  *Collation // ORDER BY x COLLATE "name"; nil: the default (or the column's)
}

type Select struct {
	Distinct bool
	Items    []SelItem
	From     string // "" = no FROM clause
	Alias    string
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr
	ForUpd   bool
	Lock     []*LockClause // FOR UPDATE / SHARE ... (rowlock.go)
	Into     *IntoClause   // SELECT ... INTO (ctas.go)

	// general executor (query.go)
	Rel        FromNode // FROM beyond one table; nil: From/Alias
	With       []CTE
	Sets       [][]Expr // GROUPING SETS / ROLLUP / CUBE expanded; nil: GroupBy
	DistinctOn []Expr
	LimitBy    *LimitBy
	Windows    map[string]*WindowSpec
	ZeroRollup bool // ClickHouse WITH ROLLUP / CUBE: rolled-up keys are type defaults, not NULL

	cached *selectPlan // see Select.plan
}

type Begin struct {
	Iso      string
	ReadOnly bool
	Start    bool // START TRANSACTION (the command tag says so)
}
type Commit struct{}
type Rollback struct{}
type SetVar struct {
	Name, Value string
	Local       bool // SET LOCAL: until the end of the transaction
}
type Show struct{ Name string }
type DropTable struct {
	Names    []string
	IfExists bool
	Cascade  bool // also drop the views that read the tables
	Mat      bool // DROP MATERIALIZED VIEW (ctas.go)
}

// Truncate is TRUNCATE [TABLE] name [, ...].
type Truncate struct {
	Names   []string
	Restart bool // RESTART IDENTITY
	Cascade bool // CASCADE: also the tables with a foreign key to these
}
type Listen struct{ Channel string }
type Unlisten struct {
	Channel string
	All     bool // UNLISTEN *
}
type Notify struct{ Channel, Payload string }

// CreatePublication: CREATE PUBLICATION name [FOR ALL TABLES | FOR TABLE
// t, ...] [WITH (publish = 'insert, update, delete')].
type CreatePublication struct {
	Name      string
	AllTables bool
	Tables    []string
	Publish   string // "" = all operations
}
type DropPublication struct {
	Name     string
	IfExists bool
}

// AlterReplicaIdentity: ALTER TABLE t REPLICA IDENTITY {DEFAULT | FULL}.
type AlterReplicaIdentity struct {
	Table string
	Full  bool
}

func (*CreatePublication) stmt()    {}
func (*CreateSequence) stmt()       {}
func (*AlterTable) stmt()           {}
func (*DropSequence) stmt()         {}
func (*AlterSequence) stmt()        {}
func (*AlterIndex) stmt()           {}
func (*DropPublication) stmt()      {}
func (*AlterReplicaIdentity) stmt() {}

func (*CreateTable) stmt() {}
func (*Insert) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
func (*Select) stmt()      {}
func (*Begin) stmt()       {}
func (*Commit) stmt()      {}
func (*Rollback) stmt()    {}
func (*SetVar) stmt()      {}
func (*Show) stmt()        {}
func (*DropTable) stmt()   {}
func (*Truncate) stmt()    {}
func (*Listen) stmt()      {}
func (*Unlisten) stmt()    {}
func (*Notify) stmt()      {}

// ---- expressions ----

type Expr interface{ expr() }

type Lit struct{ V Datum }
type Param struct{ N int } // 1-based
type ColRef struct{ Table, Name string }
type Unary struct {
	Op string // "-", "not"
	X  Expr
}
type Binary struct {
	Op   string // = <> < <= > >= + - * / % || and or
	L, R Expr
	W    uint8      // 2 or 4: an int2/int4 result that must fit (22003), see markWidths
	Coll *Collation // < <= > >= with COLLATE "name" on an operand; nil: the default
}
type Func struct {
	Name     string
	Disp     string // column name when it differs from Name ("substring" for substr)
	Args     []Expr
	Star     bool // count(*)
	Distinct bool
	Order    []OrderItem // string_agg(x, ',' ORDER BY y)
	Filter   Expr        // agg(x) FILTER (WHERE ...)
	Within   []OrderItem // percentile_cont(0.5) WITHIN GROUP (ORDER BY x)
	Params   []Expr      // ClickHouse parametric: quantileExact(0.5)(x)
	ArgNames []string    // name => value arguments of a user function ("" for positional ones)
	Over     *WindowSpec // window function
}
type IsNull struct {
	X   Expr
	Not bool
}
type InList struct {
	X    Expr
	List []Expr
	Not  bool
	memo atomic.Pointer[inMemo] // anyparam.go: sorted constant items of a long list
}
type Between struct {
	X, Lo, Hi Expr
	Not       bool
	Coll      *Collation // COLLATE "name" on an operand; nil: the default
}
type Like struct {
	X, Pat Expr
	Not    bool
	ICase  bool
}
type Cast struct {
	X    Expr
	Type string
	PG   string // PostgreSQL type spelling (int2, int4, varchar(5)...), see pgtype.go
}
type Case struct {
	Operand Expr
	Whens   []When
	Else    Expr
}
type When struct{ Cond, Then Expr }

func (*Lit) expr()         {}
func (*DefaultExpr) expr() {}
func (*Param) expr()       {}
func (*ColRef) expr()      {}
func (*Unary) expr()       {}
func (*Binary) expr()      {}
func (*Func) expr()        {}
func (*IsNull) expr()      {}
func (*InList) expr()      {}
func (*Between) expr()     {}
func (*Like) expr()        {}
func (*Cast) expr()        {}
func (*Case) expr()        {}

var aggFuncs = map[string]bool{"count": true, "sum": true, "avg": true, "min": true, "max": true}

// hasAgg reports whether x contains an aggregate call.
func hasAgg(x Expr) bool {
	found := false
	walk(x, func(e Expr) {
		if f, ok := e.(*Func); ok && aggFuncs[f.Name] {
			found = true
		}
	})
	return found
}

// walk visits x and every sub-expression (not descending into aggregates' args twice).
func walk(x Expr, fn func(Expr)) {
	if x == nil {
		return
	}
	fn(x)
	switch e := x.(type) {
	case *Unary:
		walk(e.X, fn)
	case *Binary:
		walk(e.L, fn)
		walk(e.R, fn)
	case *Func:
		for _, a := range e.Args {
			walk(a, fn)
		}
	case *IsNull:
		walk(e.X, fn)
	case *InList:
		walk(e.X, fn)
		for _, a := range e.List {
			walk(a, fn)
		}
	case *Between:
		walk(e.X, fn)
		walk(e.Lo, fn)
		walk(e.Hi, fn)
	case *Like:
		walk(e.X, fn)
		walk(e.Pat, fn)
	case *Cast:
		walk(e.X, fn)
	case *Case:
		walk(e.Operand, fn)
		for _, w := range e.Whens {
			walk(w.Cond, fn)
			walk(w.Then, fn)
		}
		walk(e.Else, fn)
	}
}
