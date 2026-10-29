package sql

// Queries beyond one table: FROM trees, CTEs, set operations, subqueries,
// window functions, grouping sets. They run on the general executor
// (query.go); single-table SELECTs keep the fast path in exec.go.

// FromNode is one item of a FROM clause.
type FromNode interface{ fromNode() }

type FromTable struct {
	Name, Alias string
	ColAliases  []string
}
type FromSubquery struct {
	Q          Stmt // *Select or *SetOp
	Alias      string
	ColAliases []string
	Lateral    bool
}
type FromValues struct {
	Rows       [][]Expr
	Alias      string
	ColAliases []string
}
type FromFunc struct { // generate_series, numbers, ...
	F          *Func
	Alias      string
	ColAliases []string
}
type FromJoin struct {
	Kind    string // inner, left, right, full, cross
	Any     bool   // ClickHouse ANY strictness: at most one match per left row
	L, R    FromNode
	On      Expr
	Using   []string
	Natural bool
}

func (*FromTable) fromNode()    {}
func (*FromSubquery) fromNode() {}
func (*FromValues) fromNode()   {}
func (*FromFunc) fromNode()     {}
func (*FromJoin) fromNode()     {}

// CTE is one WITH entry; Scalar is ClickHouse's WITH expr AS name.
type CTE struct {
	Name      string
	Cols      []string
	Q         Stmt
	Recursive bool
	Scalar    Expr
}

// SetOp is UNION / INTERSECT / EXCEPT, with the ORDER BY and LIMIT that
// apply to the combined result.
type SetOp struct {
	Op      string // union, intersect, except
	All     bool
	L, R    Stmt
	OrderBy []OrderItem
	Limit   Expr
	Offset  Expr
	With    []CTE
}

func (*SetOp) stmt() {}

type WindowSpec struct {
	Ref         string // named window this one extends
	PartitionBy []Expr
	OrderBy     []OrderItem
	Frame       *Frame
}

type Frame struct {
	Mode       string // rows, range, groups
	Start, End FrameBound
}

type FrameBound struct {
	Kind string // unbounded preceding, preceding, current row, following, unbounded following
	N    Expr
}

type LimitBy struct {
	N, Offset Expr
	By        []Expr
}

// Subquery expressions.
type Subquery struct {
	Q     Stmt
	Array bool // ARRAY(subquery): the rows of the one column as an array
} // scalar
type Exists struct{ Q Stmt }
type InSub struct {
	X   Expr
	Q   Stmt
	Not bool
}
type AnyAll struct { // x op ANY|ALL (subquery)
	X   Expr
	Op  string
	All bool
	Q   Stmt
}
type RowExpr struct{ Items []Expr } // (a, b)

// Bound references, produced by the binder of the general executor.
type Slot struct{ I int }            // column I of the current row
type OuterRef struct{ Depth, I int } // column I of the row Depth queries out

func (*Subquery) expr() {}
func (*Exists) expr()   {}
func (*InSub) expr()    {}
func (*AnyAll) expr()   {}
func (*RowExpr) expr()  {}
func (*Slot) expr()     {}
func (*OuterRef) expr() {}
