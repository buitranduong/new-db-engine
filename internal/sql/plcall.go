package sql

import (
	"fmt"
	"strings"
)

// Calling user functions: from expressions (scalar context), from FROM (set-returning), and the
// shared invoke that runs the body in the caller's transaction.

// fnResult is what a call produced.
type fnResult struct {
	cols []Column
	rows [][]Datum // a set; or the one row of a row-returning function
	set  bool
	row  bool  // a composite value (several OUT parameters, a table type)
	val  Datum // the scalar
}

// beginFnTx makes sure the statement that calls a function runs in a transaction, so the function's
// writes commit or roll back with it: an autocommit statement gets an implicit one that Exec ends.
func (s *Session) beginFnTx() error {
	if s.explicit || s.tx != nil || s.fnTx {
		return nil
	}
	if _, err := s.exec(&Begin{}, nil); err != nil {
		return err
	}
	s.fnTx = true
	return nil
}

// endFnTx ends the implicit transaction of beginFnTx: commit if the statement succeeded.
func (s *Session) endFnTx(err error) error {
	if !s.fnTx {
		return err
	}
	s.fnTx = false
	if err != nil {
		s.exec(&Rollback{}, nil)
		return err
	}
	if s.failed {
		s.exec(&Rollback{}, nil)
		return nil
	}
	_, cerr := s.exec(&Commit{}, nil)
	return cerr
}

// invoke runs d with its bound input values.
func (s *Session) invoke(d *funcDef, args []Datum) (*fnResult, error) {
	if d.Ret == "trigger" || d.Ret == "event_trigger" {
		return nil, errf(codeFeature, "trigger functions cannot be called directly")
	}
	if s.plDepth >= maxPLDepth {
		return nil, errf("54001", "stack depth limit exceeded")
	}
	s.plDepth++
	defer func() { s.plDepth-- }()
	if err := s.beginFnTx(); err != nil {
		return nil, err
	}
	vars := map[string]Datum{}
	kinds := map[string]Kind{}
	ins := d.ins()
	for i, a := range ins {
		v := args[i]
		k := kindOfType(a.Type)
		if !v.IsNull() && v.K != k {
			var err error
			if v, err = coerce(v, k); err != nil {
				return nil, err
			}
		}
		vars[fmt.Sprintf("$%d", i+1)] = v
		if a.Name != "" {
			n := strings.ToLower(a.Name)
			vars[n] = v
			kinds[n] = k
		}
	}
	outs := d.outs()
	for _, a := range outs {
		if a.Name == "" {
			continue
		}
		n := strings.ToLower(a.Name)
		if _, ok := vars[n]; !ok {
			vars[n] = Null
			kinds[n] = kindOfType(a.Type)
		}
	}
	res := &fnResult{set: d.Set}
	comp := s.retComposite(d)
	res.cols = s.resultCols(d)
	res.row = comp != nil && !d.Set
	if d.Lang == "sql" {
		r, err := s.runSQLBody(d.Body, vars)
		if err != nil {
			return nil, err
		}
		switch {
		case d.Set:
			res.rows = r.Rows
		case res.row:
			if len(r.Rows) > 0 {
				res.rows = [][]Datum{r.Rows[0]}
			}
		default:
			if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
				res.val = r.Rows[0][0]
			}
		}
	} else {
		blk, err := cachedPlParse(d.Body, d.argNames())
		if err != nil {
			return nil, err
		}
		run := &plRun{s: s, vars: vars, kinds: kinds, fn: d, cols: res.cols}
		for _, a := range outs {
			if a.Name != "" {
				run.outNames = append(run.outNames, strings.ToLower(a.Name))
			}
		}
		defer run.closeCursors()
		if _, err := run.block(blk, nil); err != nil {
			return nil, err
		}
		if !run.returned && !d.Set && len(outs) == 0 && d.Ret != "void" && !d.Procedure {
			return nil, errf("2F005", "control reached end of function without RETURN")
		}
		switch {
		case d.Set:
			res.rows = run.rows
		case run.retRow != nil:
			res.rows = [][]Datum{run.retRow}
			res.row = true
		case len(outs) > 0 && len(run.outNames) == len(outs):
			row := make([]Datum, len(outs))
			for i, n := range run.outNames {
				row[i] = run.vars[n]
			}
			if len(outs) == 1 && !res.row {
				res.val = row[0]
			} else {
				res.rows, res.row = [][]Datum{row}, true
			}
		default:
			res.val = run.ret
			if res.row && !res.val.IsNull() && len(res.cols) > 0 { // RETURN ROW(..) / a record text
				if row, err := recordRow(res.val, res.cols); err != nil {
					return nil, err
				} else {
					res.rows = [][]Datum{row}
				}
			}
		}
	}
	if d.Procedure && len(outs) > 0 {
		pr := *res
		if !pr.row {
			pr.rows = [][]Datum{{pr.val}}
		}
		pr.cols = outColumns(outs)
		s.lastProc = &pr
	}
	if d.Ret == "void" {
		res.val = TextD("")
		return res, nil
	}
	if !res.row && !res.set {
		if k := userFuncKind0(*d); !res.val.IsNull() && res.val.K != k {
			var err error
			if res.val, err = coerce(res.val, k); err != nil {
				return nil, err
			}
		}
	}
	// coerce the values of a set / row to the column kinds
	for _, row := range res.rows {
		for i := range row {
			if i < len(res.cols) && !row[i].IsNull() && row[i].K != res.cols[i].Kind && res.cols[i].Kind != KNull {
				if v, err := coerce(row[i], res.cols[i].Kind); err == nil {
					row[i] = v
				}
			}
		}
	}
	return res, nil
}

// callUser calls the user function named by f with evaluated args in scalar context; ok=false if
// there is none of that name.
func (c *evalCtx) callUser(f *Func, args []Datum) (Datum, bool, error) {
	s := c.session()
	if s == nil {
		return Null, false, nil
	}
	if _, known := userFuncKinds.Load(f.Name); !known {
		return Null, false, nil
	}
	d, full, found, err := s.resolveFunc(f.Name, args, f.ArgNames)
	if !found {
		return Null, false, nil
	}
	if err != nil {
		return Null, true, err
	}
	if d.Strict {
		for _, a := range full {
			if a.IsNull() {
				return Null, true, nil
			}
		}
	}
	r, err := s.invoke(d, full)
	if err != nil {
		return Null, true, err
	}
	switch {
	case r.set:
		if len(r.rows) == 0 {
			return Null, true, nil
		}
		if len(r.rows[0]) == 1 && len(r.cols) == 1 {
			return r.rows[0][0], true, nil
		}
		return TextD(recordText(r.rows[0])), true, nil
	case r.row:
		if len(r.rows) == 0 {
			return Null, true, nil
		}
		return TextD(recordText(r.rows[0])), true, nil
	}
	return r.val, true, nil
}

// runSQLBody runs a LANGUAGE sql body: the result is that of the last statement that returns rows.
func (s *Session) runSQLBody(body string, vars map[string]Datum) (*Result, error) {
	text, params, err := sqlWithVars(body, vars)
	if err != nil {
		return nil, err
	}
	sts, err := parseWith(text, s)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, st := range sts {
		r, err := s.exec(st, params)
		if err != nil {
			return nil, err
		}
		if r.HasRows {
			res = r
		}
	}
	return res, nil
}

// userSRF finds the set-returning (or row-returning) user function a FROM clause names, by name and
// argument count; its columns are what the FROM item exposes.
func (s *Session) userSRF(name string, nargs int, names []string) (*funcDef, bool) {
	defs := s.funcsNamed(name)
	for i := range defs {
		if defs[i].Procedure || defs[i].Ret == "trigger" {
			continue
		}
		ins := defs[i].ins()
		req := 0
		for _, a := range ins {
			if a.Def == "" && a.Mode != "v" {
				req++
			}
		}
		if nargs >= req && (nargs <= len(ins) || len(ins) > 0 && ins[len(ins)-1].Mode == "v") {
			return &defs[i], true
		}
	}
	return nil, false
}

// callSRF runs a user function in a FROM clause and returns its rows.
func (s *Session) callSRF(name string, args []Datum, names []string) ([][]Datum, error) {
	d, full, found, err := s.resolveFunc(name, args, names)
	if !found {
		return nil, errf(codeUndefinedFunction, "function %s does not exist", name)
	}
	if err != nil {
		return nil, err
	}
	if d.Strict {
		for _, a := range full {
			if a.IsNull() {
				return nil, nil
			}
		}
	}
	r, err := s.invoke(d, full)
	if err != nil {
		return nil, err
	}
	if r.set || r.row {
		return r.rows, nil
	}
	if d.Ret == "void" {
		return [][]Datum{{TextD("")}}, nil
	}
	return [][]Datum{{r.val}}, nil
}

// describeCall types CALL: a procedure with OUT / INOUT parameters returns one row of them.
func (s *Session) describeCall(st *CallStmt) (*Description, error) {
	d := &Description{}
	sts, err := parseWith(st.Text, s)
	if err != nil || len(sts) != 1 {
		return d, nil
	}
	sel, ok := sts[0].(*Select)
	if !ok || len(sel.Items) != 1 {
		return d, nil
	}
	f, ok := sel.Items[0].X.(*Func)
	if !ok {
		return d, nil
	}
	d.Params = make([]Kind, maxParamAny(sel))
	for i := range d.Params {
		d.Params[i] = KText
	}
	var def *funcDef
	for _, c := range s.funcsNamed(f.Name) {
		c := c
		if c.Procedure && len(c.ins()) >= len(f.Args) {
			def = &c
			break
		}
	}
	if def != nil {
		for i, a := range def.ins() {
			if p, isP := f.Args[min(i, len(f.Args)-1)].(*Param); isP && i < len(f.Args) && p.N <= len(d.Params) {
				d.Params[p.N-1] = kindOfType(a.Type)
			}
		}
		if outs := def.outs(); len(outs) > 0 {
			d.HasRows, d.Cols = true, outColumns(outs)
		}
	}
	return d, nil
}
