package sql

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"stask/internal/core"
	"stask/internal/schema"
)

// The user function catalog with overloading by argument types, OUT / INOUT / VARIADIC / DEFAULT
// parameters, RETURNS TABLE and SETOF (docs/14 appendix Z). One catalog object (kind "func") per
// function NAME holds every overload: a single overload is stored as the bare funcDef (the
// format before overloading existed), several as {"ovl":[funcDef, ...]}. Existing databases
// therefore read unchanged; the new funcDef / funcArg fields are all omitempty.

type funcRecord struct {
	Ovl []funcDef `json:"ovl"`
}

var (
	funcDecoded    sync.Map // object data -> []funcDef
	funcDecodedLen atomic.Int64
)

// decodeFuncs parses the data of a catalog object into its overloads (cached by text).
func decodeFuncs(data string) []funcDef {
	if v, ok := funcDecoded.Load(data); ok {
		return v.([]funcDef)
	}
	var out []funcDef
	var m funcRecord
	if json.Unmarshal([]byte(data), &m) == nil && len(m.Ovl) > 0 {
		out = m.Ovl
	} else {
		var d funcDef
		if json.Unmarshal([]byte(data), &d) == nil && d.Name != "" {
			out = []funcDef{d}
		}
	}
	if funcDecodedLen.Add(1) > 2000 {
		funcDecoded.Clear()
		funcDecodedLen.Store(1)
	}
	funcDecoded.Store(data, out)
	return out
}

func encodeFuncs(defs []funcDef) string {
	if len(defs) == 1 {
		b, _ := json.Marshal(defs[0])
		return string(b)
	}
	b, _ := json.Marshal(funcRecord{Ovl: defs})
	return string(b)
}

// funcsNamed lists the overloads of the function name.
func (s *Session) funcsNamed(name string) []funcDef {
	o, ok := s.E.Obj("func", name)
	if !ok {
		return nil
	}
	return decodeFuncs(o.Data)
}

// allFuncs lists every user function (all overloads), by name.
func allFuncs(e *core.Engine) []funcDef {
	var out []funcDef
	for _, o := range e.Objs("func") {
		out = append(out, decodeFuncs(o.Data)...)
	}
	return out
}

// isIn / isOut: the parameter modes. "" is IN, "o" OUT, "b" INOUT, "v" VARIADIC, "t" a column of
// RETURNS TABLE.
func (a funcArg) isIn() bool  { return a.Mode == "" || a.Mode == "b" || a.Mode == "v" }
func (a funcArg) isOut() bool { return a.Mode == "o" || a.Mode == "b" || a.Mode == "t" }

func (d *funcDef) ins() []funcArg {
	var out []funcArg
	for _, a := range d.Args {
		if a.isIn() {
			out = append(out, a)
		}
	}
	return out
}

func (d *funcDef) outs() []funcArg {
	var out []funcArg
	for _, a := range d.Args {
		if a.isOut() {
			out = append(out, a)
		}
	}
	return out
}

// sig is the identity of an overload: the PostgreSQL names of its input argument types.
func (d *funcDef) sig() string {
	var parts []string
	for _, a := range d.ins() {
		parts = append(parts, funcTypeName(a.Type, a.PG))
	}
	return strings.Join(parts, ",")
}

// retRow reports whether the function returns a row (several output columns or a composite
// type) rather than a scalar.
func (s *Session) retComposite(d *funcDef) []Column {
	outs := d.outs()
	switch {
	case len(outs) > 1:
		return outColumns(outs)
	case len(outs) == 1 && d.Args != nil && outs[0].Mode == "t":
		return outColumns(outs)
	case len(outs) == 0 && d.Ret != "" && d.Ret != "void" && d.Ret != "trigger" && d.Ret != "event_trigger":
		if t, err := s.E.UserTable(d.Ret); err == nil {
			return tableColumns(t.Schema())
		}
		if cols := s.compositeCols(d.Ret); cols != nil {
			return cols
		}
	}
	return nil
}

func outColumns(outs []funcArg) []Column {
	cols := make([]Column, len(outs))
	for i, a := range outs {
		name := a.Name
		if name == "" {
			name = fmt.Sprintf("column%d", i+1)
		}
		cols[i] = Column{Name: name, Kind: kindOfType(a.Type)}.withTy(specTy(a.PG))
	}
	return cols
}

func tableColumns(t *schema.Table) []Column {
	var cols []Column
	for _, ci := range t.VisibleCols() {
		c := &t.Columns[ci]
		cols = append(cols, Column{Name: c.Name, Kind: kindOfType(c.Type)}.withTy(colTy(c)))
	}
	return cols
}

// resultCols are the columns a set-returning (or row-returning) function produces.
func (s *Session) resultCols(d *funcDef) []Column {
	if cols := s.retComposite(d); cols != nil {
		return cols
	}
	if d.Ret == "void" || d.Ret == "" {
		return nil
	}
	name := d.Name
	if outs := d.outs(); len(outs) == 1 && outs[0].Name != "" {
		name = outs[0].Name
	}
	return []Column{Column{Name: name, Kind: retKind(d.Ret)}.withTy(specTy(d.RetPG))}
}

// ---- overload resolution ----

// argScore says how well a value of kind k fits a parameter of type t (0: not at all).
func argScore(v Datum, a funcArg) int {
	pk := kindOfType(a.Type)
	switch {
	case v.IsNull():
		return 2
	case v.K == pk:
		return 4
	case v.K == KText: // an untyped literal takes the parameter's type
		return 1
	case v.K == KInt && (pk == KNumeric || pk == KFloat):
		return 2
	case v.K == KNumeric && pk == KFloat:
		return 2
	case v.K == KDate && (pk == KTime || pk == KTimestamp):
		return 2
	case v.K == KTimestamp && pk == KTime:
		return 2
	case v.K == KJSON && pk == KJSONB, v.K == KJSONB && pk == KJSON:
		return 1
	case pk == KText && a.Type != "text" && (v.K == KUUID):
		return 1
	}
	return 0
}

// bindArgs lines the call's values up with d's input parameters: positional values first, then the
// named ones, then DEFAULTs. ok=false if d cannot take the call.
func (s *Session) bindArgs(d *funcDef, args []Datum, names []string) ([]Datum, int, bool) {
	ins := d.ins()
	out := make([]Datum, len(ins))
	set := make([]bool, len(ins))
	score := 0
	variadic := len(ins) > 0 && ins[len(ins)-1].Mode == "v"
	pos := 0
	var vrest []Datum
	for i, v := range args {
		if i < len(names) && names[i] != "" {
			found := false
			for j, a := range ins {
				if strings.EqualFold(a.Name, names[i]) && a.Name != "" {
					if set[j] {
						return nil, 0, false
					}
					out[j], set[j], found = v, true, true
					score += argScore(v, a)
				}
			}
			if !found {
				return nil, 0, false
			}
			continue
		}
		if pos >= len(ins) || (variadic && pos == len(ins)-1) {
			if variadic && pos >= len(ins)-1 {
				a := ins[len(ins)-1]
				ea := funcArg{Type: strings.TrimSuffix(a.Type, "[]")}
				if sc := argScore(v, ea); sc > 0 || v.IsNull() {
					score += sc
				} else {
					return nil, 0, false
				}
				vrest = append(vrest, v)
				pos = len(ins) - 1
				continue
			}
			return nil, 0, false
		}
		out[pos], set[pos] = v, true
		score += argScore(v, ins[pos])
		if argScore(v, ins[pos]) == 0 {
			score -= 100
		}
		pos++
	}
	if variadic {
		last := len(ins) - 1
		if len(vrest) > 0 {
			out[last], set[last] = arrayD(vrest), true
		} else if !set[last] {
			out[last], set[last] = arrayD(nil), true
		}
	}
	for j, a := range ins {
		if set[j] {
			continue
		}
		if a.Def == "" {
			return nil, 0, false
		}
		v, err := s.evalConstText(a.Def)
		if err != nil {
			return nil, 0, false
		}
		out[j] = v
	}
	return out, score, true
}

// evalConstText evaluates a constant expression (a parameter DEFAULT).
func (s *Session) evalConstText(text string) (Datum, error) {
	sts, err := parseWith("SELECT "+text, s)
	if err != nil {
		return Null, err
	}
	r, err := s.exec(sts[0], nil)
	if err != nil {
		return Null, err
	}
	if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
		return Null, nil
	}
	return r.Rows[0][0], nil
}

// resolveFunc picks the overload of name that the call (argument values and names) selects. found
// is false if there is no function of that name at all.
func (s *Session) resolveFunc(name string, args []Datum, names []string) (d *funcDef, full []Datum, found bool, err error) {
	defs := s.funcsNamed(name)
	if len(defs) == 0 {
		return nil, nil, false, nil
	}
	best, bestScore := -1, -1<<30
	var bestArgs []Datum
	for i := range defs {
		if defs[i].Ret == "trigger" && len(args) > 0 {
			continue
		}
		bound, sc, ok := s.bindArgs(&defs[i], args, names)
		if !ok {
			continue
		}
		if len(defs) > 1 { // overloads: a value that does not fit rules the candidate out
			bad := false
			ins := defs[i].ins()
			for j := 0; j < len(args) && j < len(ins) && len(names) == 0; j++ {
				if argScore(args[j], ins[j]) == 0 && ins[j].Mode != "v" {
					bad = true
				}
			}
			if bad {
				continue
			}
		}
		// among equal fits prefer the canonical text type, so that a varchar overload that calls
		// the text one does not call itself
		if sc > bestScore || sc == bestScore && best >= 0 && defs[best].sig() != "text" && defs[i].sig() == "text" {
			best, bestScore, bestArgs = i, sc, bound
		}
	}
	if best < 0 {
		return nil, nil, true, errf(codeUndefinedFunction, "function %s(%s) does not exist", name, argKindsText(args))
	}
	return &defs[best], bestArgs, true, nil
}

func argKindsText(args []Datum) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = "unknown"
		if !a.IsNull() {
			parts[i] = a.K.String()
		}
	}
	return strings.Join(parts, ", ")
}

// loadFunc returns the overload of name that takes no arguments, else the first (used by triggers,
// whose functions take none).
func (s *Session) loadFunc(name string) (*funcDef, bool) {
	defs := s.funcsNamed(name)
	if len(defs) == 0 {
		return nil, false
	}
	for i := range defs {
		if len(defs[i].ins()) == 0 {
			return &defs[i], true
		}
	}
	return &defs[0], true
}

// noteFuncTypes records the static result types of the overloads of name (for expressions typed
// before they run).
func noteFuncTypes(name string, defs []funcDef) {
	if len(defs) == 0 {
		userFuncKinds.Delete(name)
		userFuncTys.Delete(name)
		userSRFs.Delete(name)
		return
	}
	userSRFs.Delete(name)
	for _, d := range defs {
		if d.Set {
			userSRFs.Store(name, true)
		}
	}
	userFuncKinds.Store(name, userFuncKind0(defs[len(defs)-1]))
	t := userFuncTy0(defs[0])
	for _, d := range defs[1:] {
		if userFuncTy0(d) != t {
			t = pgTy{}
		}
	}
	if t.oid != 0 {
		userFuncTys.Store(name, t)
	} else {
		userFuncTys.Delete(name)
	}
}

// userSRFs: the names of the user functions that return sets (expanded in a select list).
var userSRFs sync.Map

func isUserSRF(name string) bool {
	_, ok := userSRFs.Load(name)
	return ok
}

// userFuncTys: the PostgreSQL type of a function's result when all its overloads agree.
var userFuncTys sync.Map // name -> pgTy

func userFuncTy(name string) (pgTy, bool) {
	t, ok := userFuncTys.Load(name)
	if !ok {
		return pgTy{}, false
	}
	return t.(pgTy), true
}

func userFuncKind0(d funcDef) Kind {
	if len(d.outs()) > 1 || d.Set && len(d.outs()) > 0 {
		return KText
	}
	if outs := d.outs(); len(outs) == 1 {
		return kindOfType(outs[0].Type)
	}
	return retKind(d.Ret)
}

func userFuncTy0(d funcDef) pgTy {
	if len(d.outs()) > 1 && !d.Set {
		return pgTy{oid: 2249, mod: -1}
	}
	if d.Ret == "void" || d.Ret == "trigger" || d.Ret == "record" || len(d.outs()) > 1 || d.Set && len(d.outs()) > 0 {
		return pgTy{}
	}
	pg, typ := d.RetPG, d.Ret
	if outs := d.outs(); len(outs) == 1 {
		pg, typ = outs[0].PG, outs[0].Type
	}
	if pg == "" {
		pg = typ
	}
	t := specTy(pg)
	if t.oid == 0 {
		return pgTy{}
	}
	return t
}

// ---- CREATE / DROP ----

func (s *Session) createFunc(c *CreateFunc) (*Result, error) {
	d := c.Def
	if d.Lang == "plpgsql" && d.Ret != "trigger" {
		if _, err := plParse(d.Body, d.argNames()); err != nil {
			return nil, err
		}
	}
	defs := append([]funcDef(nil), s.funcsNamed(d.Name)...)
	idx := -1
	for i := range defs {
		if defs[i].sig() == d.sig() {
			idx = i
		}
	}
	switch {
	case idx >= 0 && !c.OrReplace:
		return nil, errf("42723", "function %q already exists with same argument types", d.Name)
	case idx >= 0:
		if defs[idx].Procedure != d.Procedure {
			return nil, errf(codeWrongObjectType, "%q is not a %s", d.Name, map[bool]string{true: "function", false: "procedure"}[d.Procedure])
		}
		defs[idx] = d
	default:
		defs = append(defs, d)
	}
	if err := s.E.PutObj(core.Obj{Kind: "func", Name: d.Name, Data: encodeFuncs(defs)}, true); err != nil {
		return nil, AsError(err)
	}
	noteFuncTypes(d.Name, defs)
	if d.Procedure {
		return &Result{Tag: "CREATE PROCEDURE"}, nil
	}
	return &Result{Tag: "CREATE FUNCTION"}, nil
}

func (d funcDef) argNames() []string {
	var out []string
	for _, a := range d.Args {
		if a.isIn() {
			out = append(out, a.Name)
		}
	}
	return out
}

func (s *Session) dropFunc(d *DropFunc) (*Result, error) {
	kind := "function"
	tag := "DROP FUNCTION"
	if d.Procedure {
		kind, tag = "procedure", "DROP PROCEDURE"
	}
	defs := s.funcsNamed(d.Name)
	var keep []funcDef
	var dropped *funcDef
	for i := range defs {
		match := !d.HasTypes && len(defs) == 1
		if d.HasTypes {
			match = defs[i].sig() == d.sigText()
		} else if len(defs) > 1 && len(defs[i].ins()) == 0 {
			match = true
		}
		if match && dropped == nil {
			dropped = &defs[i]
			continue
		}
		keep = append(keep, defs[i])
	}
	if dropped == nil {
		if len(defs) > 1 && !d.HasTypes && !d.IfExists {
			return nil, errf("42725", "function name %q is not unique", d.Name)
		}
		if d.IfExists {
			return &Result{Tag: tag, Notice: fmt.Sprintf("%s %s(%s) does not exist, skipping", kind, d.Name, strings.Join(d.Types, ","))}, nil
		}
		return nil, errf(codeUndefinedFunction, "%s %s(%s) does not exist", kind, d.Name, strings.Join(d.Types, ","))
	}
	for _, tg := range s.E.AllTriggers() { // trigger definitions name their function
		if tg.Func != d.Name || len(dropped.ins()) != 0 {
			continue
		}
		if !d.Cascade {
			return nil, errf("2BP01", "cannot drop function %s because other objects depend on it (trigger %s on table %s)", d.Name, tg.Name, tg.Table)
		}
		if err := s.E.DropTrigger(tg.Table, tg.Name); err != nil {
			return nil, AsError(err)
		}
	}
	var op core.ObjOp
	if len(keep) == 0 {
		op = core.ObjOp{O: core.Obj{Kind: "func", Name: d.Name}, Delete: true}
	} else {
		op = core.ObjOp{O: core.Obj{Kind: "func", Name: d.Name, Data: encodeFuncs(keep)}, Replace: true}
	}
	if err := s.E.WriteObjs([]core.ObjOp{op}); err != nil {
		return nil, AsError(err)
	}
	noteFuncTypes(d.Name, keep)
	return &Result{Tag: tag}, nil
}

func (d *DropFunc) sigText() string { return strings.Join(d.Types, ",") }

var funcsLoaded sync.Map // *core.Engine -> true

// loadUserFuncs records the result types of the functions stored in e, once per engine, so that the
// first query after a restart already types them.
func loadUserFuncs(e *core.Engine) {
	if _, done := funcsLoaded.LoadOrStore(e, true); done {
		return
	}
	for _, o := range e.Objs("func") {
		if defs := decodeFuncs(o.Data); len(defs) > 0 {
			noteFuncTypes(o.Name, defs)
		}
	}
}
