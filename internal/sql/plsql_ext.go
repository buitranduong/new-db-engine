package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// PL/pgSQL beyond the first subset (docs/14 appendix Z): GET DIAGNOSTICS, FOREACH, cursors,
// RETURN NEXT / RETURN QUERY, ASSERT, records, RAISE with all its options.

type plGetDiag struct {
	stacked bool
	items   [][2]string // target variable, item
}
type plForeach struct {
	label string
	v     string
	slice int
	expr  string
	body  []plStmt
}
type plOpen struct {
	cur   string
	args  []string
	query string
	exec  bool
	using []string
	bound bool // OPEN c [(args)] of a declared cursor
}
type plFetch struct {
	cur  string
	dir  string
	into []string
	move bool
}
type plClose struct{ cur string }
type plReturnNext struct{ expr string }
type plReturnQuery struct {
	query string
	exec  bool
	using []string
}
type plAssert struct{ cond, msg string }

// plCurDecl is a cursor declared in DECLARE.
type plCurDecl struct {
	params []plDecl
	query  string
}

func (q *plp) identTok() (string, error) {
	t := q.cur()
	if t.k != tIdent && t.k != tQuoted {
		return "", q.syntax()
	}
	q.i++
	return strings.ToLower(t.s), nil
}

func (q *plp) semi() func(t token) bool {
	return func(t token) bool { return t.k == tOp && t.s == ";" }
}

// ext parses the statements of this file; ok=false if the current token starts none of them.
func (q *plp) ext() (plStmt, bool, error) {
	t := q.cur()
	if t.k != tIdent {
		return nil, false, nil
	}
	switch t.s {
	case "get":
		st, err := q.getDiag()
		return st, true, err
	case "foreach":
		st, err := q.foreachStmt("")
		return st, true, err
	case "open":
		st, err := q.openStmt()
		return st, true, err
	case "fetch", "move":
		st, err := q.fetchStmt()
		return st, true, err
	case "close":
		q.i++
		n, err := q.identTok()
		if err != nil {
			return nil, true, err
		}
		return &plClose{n}, true, q.endStmt()
	case "assert":
		q.i++
		e, err := q.textUntil(func(t token) bool { return t.k == tOp && (t.s == "," || t.s == ";") })
		if err != nil {
			return nil, true, err
		}
		a := &plAssert{cond: e}
		if q.opIs(",") {
			q.i++
			if a.msg, err = q.textUntil(q.semi()); err != nil {
				return nil, true, err
			}
		}
		return a, true, q.endStmt()
	}
	return nil, false, nil
}

func (q *plp) getDiag() (plStmt, error) {
	q.i++ // get
	g := &plGetDiag{}
	if q.acceptKw("stacked") {
		g.stacked = true
	} else {
		q.acceptKw("current")
	}
	if !q.acceptKw("diagnostics") {
		return nil, q.syntax()
	}
	for {
		target, err := q.identTok()
		if err != nil {
			return nil, err
		}
		if q.opIs(":") {
			q.i++
		}
		if !q.opIs("=") {
			return nil, q.syntax()
		}
		q.i++
		item, err := q.identTok()
		if err != nil {
			return nil, err
		}
		g.items = append(g.items, [2]string{target, item})
		if !q.opIs(",") {
			break
		}
		q.i++
	}
	return g, q.endStmt()
}

func (q *plp) foreachStmt(label string) (plStmt, error) {
	q.i++ // foreach
	f := &plForeach{label: label}
	var err error
	if f.v, err = q.identTok(); err != nil {
		return nil, err
	}
	for q.opIs(",") { // FOREACH a, b IN ARRAY: not supported with several targets
		return nil, plErr("FOREACH with several targets is not supported")
	}
	if q.acceptKw("slice") {
		n := q.cur()
		q.i++
		f.slice, _ = strconv.Atoi(n.s)
	}
	if !q.acceptKw("in") || !q.acceptKw("array") {
		return nil, q.syntax()
	}
	if f.expr, err = q.textUntil(func(t token) bool { return t.k == tIdent && t.s == "loop" }); err != nil {
		return nil, err
	}
	q.i++
	q.scope[f.v] = true
	if f.body, err = q.stmts("end"); err != nil {
		return nil, err
	}
	if !q.acceptKw("end") || !q.acceptKw("loop") {
		return nil, q.syntax()
	}
	if t := q.cur(); t.k == tIdent && !q.opIs(";") {
		q.i++
	}
	return f, q.endStmt()
}

func (q *plp) openStmt() (plStmt, error) {
	q.i++ // open
	o := &plOpen{}
	var err error
	if o.cur, err = q.identTok(); err != nil {
		return nil, err
	}
	if q.opIs("(") { // bound cursor with arguments
		q.i++
		o.bound = true
		for !q.opIs(")") {
			a, err := q.textUntil(func(t token) bool { return t.k == tOp && (t.s == "," || t.s == ")") })
			if err != nil {
				return nil, err
			}
			o.args = append(o.args, a)
			if q.opIs(",") {
				q.i++
			}
		}
		q.i++
		return o, q.endStmt()
	}
	q.acceptKw("no")
	q.acceptKw("scroll")
	if !q.acceptKw("for") {
		o.bound = true
		return o, q.endStmt()
	}
	if q.acceptKw("execute") {
		o.exec = true
		if o.query, err = q.textUntil(func(t token) bool { return t.k == tOp && t.s == ";" || t.k == tIdent && t.s == "using" }); err != nil {
			return nil, err
		}
		if q.acceptKw("using") {
			if o.using, err = q.usingList(); err != nil {
				return nil, err
			}
		}
		return o, q.endStmt()
	}
	if o.query, err = q.textUntil(q.semi()); err != nil {
		return nil, err
	}
	return o, q.endStmt()
}

func (q *plp) usingList() ([]string, error) {
	var out []string
	for {
		a, err := q.textUntil(func(t token) bool { return t.k == tOp && (t.s == "," || t.s == ";") || t.k == tIdent && t.s == "loop" })
		if err != nil {
			return nil, err
		}
		out = append(out, a)
		if !q.opIs(",") {
			return out, nil
		}
		q.i++
	}
}

func (q *plp) fetchStmt() (plStmt, error) {
	f := &plFetch{move: q.cur().s == "move"}
	q.i++
	start := q.cur().pos
	var dirToks []string
	sawFrom := false
	for q.cur().k != tEOF && !q.opIs(";") {
		t := q.cur()
		if t.k == tIdent && (t.s == "from" || t.s == "in") {
			q.i++
			sawFrom = true
			break
		}
		if t.k == tIdent && t.s == "into" {
			break
		}
		dirToks = append(dirToks, q.src[t.pos:t.pos+tokenWidth(q.src, t)])
		q.i++
	}
	_ = start
	// the cursor is the identifier after FROM / IN; if there was no FROM, the last word is the cursor
	if sawFrom && (q.cur().k == tIdent || q.cur().k == tQuoted) {
		f.cur = strings.ToLower(q.cur().s)
		q.i++
		f.dir = strings.Join(dirToks, " ")
	} else if len(dirToks) > 0 {
		f.cur = strings.ToLower(dirToks[len(dirToks)-1])
		f.dir = strings.Join(dirToks[:len(dirToks)-1], " ")
	} else {
		return nil, q.syntax()
	}
	if q.acceptKw("into") {
		for {
			n, err := q.identTok()
			if err != nil {
				return nil, err
			}
			f.into = append(f.into, n)
			if !q.opIs(",") {
				break
			}
			q.i++
		}
	}
	return f, q.endStmt()
}

// tokenWidth is the source width of t.
func tokenWidth(src string, t token) int {
	switch t.k {
	case tIdent, tNumber:
		return len(t.s)
	case tOp:
		return len(t.s)
	}
	end := t.pos + 1
	for end < len(src) && src[end] != ' ' && src[end] != ';' && src[end] != '\n' {
		end++
	}
	return end - t.pos
}

// ---- declarations ----

// declCursor parses "[SCROLL | NO SCROLL] CURSOR [(params)] FOR | IS query" after the name; ok=false if the
// declaration is not a cursor.
func (q *plp) declCursor(d *plDecl) (bool, error) {
	save := q.i
	q.acceptKw("no")
	q.acceptKw("scroll")
	if !q.acceptKw("cursor") {
		q.i = save
		return false, nil
	}
	cd := &plCurDecl{}
	if q.opIs("(") {
		q.i++
		for !q.opIs(")") {
			n, err := q.identTok()
			if err != nil {
				return true, err
			}
			ty, err := q.textUntil(func(t token) bool { return t.k == tOp && (t.s == "," || t.s == ")") })
			if err != nil {
				return true, err
			}
			cd.params = append(cd.params, plDecl{name: n, typ: ty})
			if q.opIs(",") {
				q.i++
			}
		}
		q.i++
	}
	if !q.acceptKw("for") && !q.acceptKw("is") {
		return true, q.syntax()
	}
	var err error
	if cd.query, err = q.textUntil(q.semi()); err != nil {
		return true, err
	}
	d.cursor = cd
	q.skipSemis()
	return true, nil
}

// ---- RAISE ----

// plConditions maps condition names to SQLSTATE codes (PostgreSQL appendix A).
var plConditions = map[string]string{
	"unique_violation": "23505", "foreign_key_violation": "23503", "not_null_violation": "23502",
	"check_violation": "23514", "exclusion_violation": "23P01", "integrity_constraint_violation": "23000",
	"restrict_violation": "23001", "duplicate_object": "42710", "duplicate_table": "42P07",
	"duplicate_column": "42701", "duplicate_function": "42723", "duplicate_schema": "42P06",
	"duplicate_alias": "42712", "duplicate_database": "42P04", "duplicate_cursor": "42P03",
	"duplicate_prepared_statement": "42P05", "undefined_table": "42P01", "undefined_column": "42703",
	"undefined_object": "42704", "undefined_function": "42883", "undefined_parameter": "42P02",
	"invalid_text_representation": "22P02", "division_by_zero": "22012", "raise_exception": "P0001",
	"no_data_found": "P0002", "too_many_rows": "P0003", "assert_failure": "P0004",
	"syntax_error": "42601", "insufficient_privilege": "42501", "feature_not_supported": "0A000",
	"invalid_schema_name": "3F000", "dependent_objects_still_exist": "2BP01",
	"string_data_right_truncation": "22001", "numeric_value_out_of_range": "22003",
	"serialization_failure": "40001", "deadlock_detected": "40P01", "datatype_mismatch": "42804",
	"invalid_parameter_value": "22023", "invalid_datetime_format": "22007", "datetime_field_overflow": "22008",
	"null_value_not_allowed": "22004", "invalid_cursor_name": "34000", "invalid_cursor_state": "24000",
	"invalid_transaction_state": "25000", "active_sql_transaction": "25001", "no_active_sql_transaction": "25P01",
	"in_failed_sql_transaction": "25P02", "read_only_sql_transaction": "25006", "query_canceled": "57014",
	"lock_not_available": "55P03", "object_not_in_prerequisite_state": "55000", "object_in_use": "55006",
	"ambiguous_column": "42702", "ambiguous_function": "42725", "invalid_foreign_key": "42830",
	"cannot_coerce": "42846", "grouping_error": "42803", "wrong_object_type": "42809",
	"invalid_column_reference": "42P10", "invalid_object_definition": "42P17", "invalid_function_definition": "42P13",
	"too_many_arguments": "54023", "statement_too_complex": "54001", "program_limit_exceeded": "54000",
	"cardinality_violation": "21000", "data_exception": "22000", "invalid_regular_expression": "2201B",
	"invalid_escape_sequence": "22025", "invalid_character_value_for_cast": "22018", "array_subscript_error": "2202E",
	"sql_routine_exception": "2F000", "function_executed_no_return_statement": "2F005",
	"plpgsql_error": "P0000", "internal_error": "XX000", "data_corrupted": "XX001", "index_corrupted": "XX002",
	"connection_exception": "08000", "warning": "01000", "invalid_authorization_specification": "28000",
	"invalid_catalog_name": "3D000", "undefined_file": "58P01", "io_error": "58030", "out_of_memory": "53200",
	"disk_full": "53100", "too_many_connections": "53300", "admin_shutdown": "57P01", "name_too_long": "42622",
	"reserved_name": "42939", "invalid_name": "42602", "invalid_table_definition": "42P16",
	"invalid_column_definition": "42611", "invalid_role_specification": "0P000", "no_additional_dynamic_result_sets_returned": "02001",
	"successful_completion": "00000",
}

var plCondNames = func() map[string]string {
	m := map[string]string{}
	for n, c := range plConditions {
		if old, ok := m[c]; !ok || n < old {
			m[c] = n
		}
	}
	return m
}()

// raiseLevelRank orders client_min_messages.
var raiseLevelRank = map[string]int{"debug5": 1, "debug4": 2, "debug3": 3, "debug2": 4, "debug1": 5, "log": 6,
	"notice": 7, "warning": 8, "error": 9}

func (r *plRun) queueNotice(level, code, msg, detail, hint string) {
	sev := strings.ToUpper(level)
	switch level {
	case "info":
	case "debug", "debug1", "debug2", "debug3", "debug4", "debug5", "log":
		min := r.s.vars["client_min_messages"]
		if min == "" {
			min = "notice"
		}
		lv := level
		if lv == "debug" {
			lv = "debug1"
		}
		if raiseLevelRank[lv] < raiseLevelRank[strings.ToLower(min)] {
			return
		}
		if sev == "DEBUG" {
			sev = "DEBUG"
		}
	case "warning":
		if code == "" {
			code = "01000"
		}
	}
	if code == "" {
		code = "00000"
	}
	r.s.notices = append(r.s.notices, Notice{Sev: sev, Code: code, Msg: msg, Detail: detail, Hint: hint})
}

// runRaise executes RAISE.
func (r *plRun) runRaise(st *plRaise, cur *Error) error {
	if st.rethrow {
		if cur == nil {
			return errf("0Z002", "RAISE without parameters cannot be used outside an exception handler")
		}
		return cur
	}
	var args []string
	for _, a := range st.args {
		v, err := r.eval(a)
		if err != nil {
			return err
		}
		if v.IsNull() {
			args = append(args, "<NULL>")
		} else {
			args = append(args, v.String())
		}
	}
	opt := func(k string) (string, bool, error) {
		e, ok := st.opts[k]
		if !ok {
			return "", false, nil
		}
		v, err := r.eval(e)
		if err != nil {
			return "", true, err
		}
		return v.String(), true, nil
	}
	msg := raiseFormat(st.format, args)
	if v, ok, err := opt("message"); err != nil {
		return err
	} else if ok {
		msg = v
	}
	detail, _, err := opt("detail")
	if err != nil {
		return err
	}
	hint, _, err := opt("hint")
	if err != nil {
		return err
	}
	code := st.errcode
	if v, ok, err := opt("errcode"); err != nil {
		return err
	} else if ok {
		code = v
	}
	if c, ok := plConditions[strings.ToLower(code)]; ok {
		if msg == "" && st.format == "" {
			msg = strings.ToLower(code)
		}
		code = c
	} else if code != "" && len(code) != 5 {
		return errf("22023", "invalid SQLSTATE code %q", code)
	}
	if msg == "" && st.format == "" && st.opts["message"] == "" {
		if n, ok := plCondNames[code]; ok {
			msg = n
		} else if code != "" {
			msg = "unrecognized exception condition \"" + code + "\""
		}
	}
	if st.level != "exception" {
		r.queueNotice(st.level, code, msg, detail, hint)
		return nil
	}
	if code == "" {
		code = "P0001"
	}
	return &Error{Code: code, Msg: msg, Detail: detail, Hint: hint}
}

// ---- GET DIAGNOSTICS ----

func (r *plRun) runGetDiag(g *plGetDiag, cur *Error) error {
	if g.stacked && cur == nil {
		return errf("0Z002", "GET STACKED DIAGNOSTICS cannot be used outside an exception handler")
	}
	for _, it := range g.items {
		var v Datum
		switch it[1] {
		case "row_count":
			v = IntD(r.rowCount)
		case "result_oid":
			v = IntD(0)
		case "pg_context", "pg_exception_context":
			v = TextD("PL/pgSQL function")
		case "message_text":
			v = TextD("")
			if cur != nil {
				v = TextD(cur.Msg)
			}
		case "pg_exception_detail":
			v = TextD("")
			if cur != nil {
				v = TextD(cur.Detail)
			}
		case "pg_exception_hint":
			v = TextD("")
			if cur != nil {
				v = TextD(cur.Hint)
			}
		case "returned_sqlstate":
			v = TextD("")
			if cur != nil {
				v = TextD(cur.Code)
			}
		case "column_name", "constraint_name", "pg_datatype_name", "table_name", "schema_name":
			v = TextD("")
		default:
			return errf("42601", "unrecognized GET DIAGNOSTICS item %q", strings.ToUpper(it[1]))
		}
		if err := r.set(it[0], v); err != nil {
			return err
		}
	}
	return nil
}

// ---- records ----

// isRecord reports whether name is a record / row variable.
func (r *plRun) isRecord(name string) bool {
	_, ok := r.vars[name+".*"]
	return ok
}

func (r *plRun) recFields(name string) []string {
	v, ok := r.vars[name+".*"]
	if !ok || v.S == "" {
		return nil
	}
	return strings.Split(v.S, ",")
}

// setRecord assigns the columns of one result row (nil: no row, every field NULL) to record name.
func (r *plRun) setRecord(name string, cols []Column, row []Datum) {
	var names []string
	if row == nil && r.isRecord(name) && len(r.recFields(name)) > 0 {
		names = r.recFields(name) // a typed row keeps its fields, now NULL
	} else {
		for _, c := range cols {
			names = append(names, strings.ToLower(c.Name))
		}
	}
	for _, f := range r.recFields(name) {
		delete(r.vars, name+"."+f)
	}
	for i, n := range names {
		v := Null
		if row != nil && i < len(row) {
			v = row[i]
		}
		r.vars[name+"."+n] = v
	}
	r.vars[name+".*"] = TextD(strings.Join(names, ","))
}

// recordValues are the fields of record name, in order.
func (r *plRun) recordValues(name string) []Datum {
	fs := r.recFields(name)
	out := make([]Datum, len(fs))
	for i, f := range fs {
		out[i] = r.vars[name+"."+f]
	}
	return out
}

// intoTargets assigns the first row of res to the INTO targets.
func (r *plRun) intoTargets(into []string, res *Result) error {
	var row []Datum
	if res != nil && len(res.Rows) > 0 {
		row = res.Rows[0]
	}
	if len(into) == 1 && r.isRecord(into[0]) {
		var cols []Column
		if res != nil {
			cols = res.Cols
		}
		r.setRecord(into[0], cols, row)
		return nil
	}
	for i, name := range into {
		v := Null
		if i < len(row) {
			v = row[i]
		}
		if err := r.set(name, v); err != nil {
			return err
		}
	}
	return nil
}

// rowText evaluates the items of "ROW(a, b)" (or "(a, b)") as a select list and returns the
// values; ok=false if text is not a row constructor.
func (r *plRun) rowConstructor(text string) ([]Column, []Datum, bool, error) {
	t := strings.TrimSpace(text)
	lt := strings.ToLower(t)
	if strings.HasPrefix(lt, "row") {
		rest := strings.TrimSpace(t[3:])
		if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
			t = rest[1 : len(rest)-1]
		} else {
			return nil, nil, false, nil
		}
	} else {
		return nil, nil, false, nil
	}
	res, err := r.sql("SELECT " + t)
	if err != nil {
		return nil, nil, true, err
	}
	cols := make([]Column, len(res.Cols))
	for i, c := range res.Cols {
		c.Name = fmt.Sprintf("f%d", i+1)
		cols[i] = c
	}
	if len(res.Rows) == 0 {
		return cols, nil, true, nil
	}
	return cols, res.Rows[0], true, nil
}

// ---- dynamic SQL ----

// runDynamic runs the text of an EXECUTE with its USING values.
func (r *plRun) runDynamic(qexpr string, using []string) (*Result, error) {
	q, err := r.eval(qexpr)
	if err != nil {
		return nil, err
	}
	if q.IsNull() {
		return nil, errf("22004", "query string argument of EXECUTE is null")
	}
	var params []Datum
	for _, a := range using {
		v, err := r.eval(a)
		if err != nil {
			return nil, err
		}
		params = append(params, v)
	}
	sts, err := parseWith(q.String(), r.s)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, x := range sts {
		if res, err = r.exec(x, params); err != nil {
			return nil, err
		}
	}
	r.noteCount(res)
	return res, nil
}

// noteCount records the rows a statement affected, for GET DIAGNOSTICS.
func (r *plRun) noteCount(res *Result) {
	n := len(res.Rows)
	if !res.HasRows {
		n = 0
		if f := strings.Fields(res.Tag); len(f) > 0 {
			n, _ = strconv.Atoi(f[len(f)-1])
		}
	}
	r.rowCount = int64(n)
}

// ---- cursors ----

func (r *plRun) curName(v string, open bool) (string, error) {
	d, ok := r.vars[v]
	if !ok {
		return "", errf("42703", "variable %q does not exist", v)
	}
	if d.IsNull() && !open {
		return "", errf("22004", "cursor variable %q is null", v)
	}
	if d.IsNull() && r.curDefs[v] != nil { // a bound cursor is named after its variable
		r.vars[v] = TextD(v)
		return v, nil
	}
	if d.IsNull() {
		r.curSeq++
		name := fmt.Sprintf("<unnamed portal %d>", r.curSeq)
		r.vars[v] = TextD(name)
		return name, nil
	}
	return d.String(), nil
}

func (r *plRun) openCursor(o *plOpen) error {
	name, err := r.curName(o.cur, true)
	if err != nil {
		return err
	}
	var query string
	var params []Datum
	switch {
	case o.exec:
		q, err := r.eval(o.query)
		if err != nil {
			return err
		}
		query = q.String()
		for _, a := range o.using {
			v, err := r.eval(a)
			if err != nil {
				return err
			}
			params = append(params, v)
		}
	case o.bound:
		def, ok := r.curDefs[o.cur]
		if !ok {
			return errf("42601", "cursor variable %q must have a bound query", o.cur)
		}
		saved := map[string]Datum{}
		had := map[string]bool{}
		for i, p := range def.params {
			v := Null
			if i < len(o.args) {
				if v, err = r.eval(o.args[i]); err != nil {
					return err
				}
			}
			saved[p.name], had[p.name] = r.vars[p.name]
			r.vars[p.name] = v
		}
		text, ps, err := sqlWithVars(def.query, r.vars)
		for _, p := range def.params {
			if had[p.name] {
				r.vars[p.name] = saved[p.name]
			} else {
				delete(r.vars, p.name)
			}
		}
		if err != nil {
			return err
		}
		query, params = text, ps
	default:
		text, ps, err := sqlWithVars(o.query, r.vars)
		if err != nil {
			return err
		}
		query, params = text, ps
	}
	sts, err := parseWith(query, r.s)
	if err != nil {
		return err
	}
	if len(sts) != 1 {
		return errf("42601", "cursor query must be a single statement")
	}
	if err := r.s.beginFnTx(); err != nil {
		return err
	}
	if _, err := r.s.exec(&DeclareCursor{Name: name, Query: sts[0], Text: query, Scroll: 1}, params); err != nil {
		return err
	}
	r.opened = append(r.opened, name)
	return nil
}

func (r *plRun) closeCursors() {
	keep := ""
	if !r.ret.IsNull() && r.ret.K == KText {
		keep = r.ret.S
	}
	for _, n := range r.opened {
		if n != keep {
			r.s.exec(&CloseCursor{Name: n}, nil)
		}
	}
	r.opened = nil
}

func (r *plRun) fetchCursor(f *plFetch) error {
	name, err := r.curName(f.cur, false)
	if err != nil {
		return err
	}
	dir := strings.TrimSpace(f.dir)
	text := fmt.Sprintf("%s %s FROM %s", map[bool]string{true: "MOVE", false: "FETCH"}[f.move], dir, quoteIdent(name))
	sts, err := parseWith(text, r.s)
	if err != nil {
		return err
	}
	res, err := r.s.exec(sts[0], nil)
	if err != nil {
		return err
	}
	if f.move {
		n := 0
		if fs := strings.Fields(res.Tag); len(fs) > 0 {
			n, _ = strconv.Atoi(fs[len(fs)-1])
		}
		r.vars["found"] = BoolD(n > 0)
		r.rowCount = int64(n)
		return nil
	}
	r.vars["found"] = BoolD(len(res.Rows) > 0)
	r.rowCount = int64(len(res.Rows))
	return r.intoTargets(f.into, res)
}

// ---- RETURN NEXT / QUERY ----

func (r *plRun) returnRow(expr string) ([]Datum, error) {
	if strings.TrimSpace(expr) == "" {
		row := make([]Datum, len(r.outNames))
		for i, n := range r.outNames {
			row[i] = r.vars[n]
		}
		return row, nil
	}
	name := strings.ToLower(strings.TrimSpace(expr))
	if r.isRecord(name) {
		return r.recordValues(name), nil
	}
	if len(r.cols) > 1 {
		if _, vals, ok, err := r.rowConstructor(expr); ok || err != nil {
			return vals, err
		}
	}
	v, err := r.eval(expr)
	return []Datum{v}, err
}

func (r *plRun) addRows(rows [][]Datum) {
	for _, row := range rows {
		r.rows = append(r.rows, append([]Datum(nil), row...))
	}
}

// ---- FOREACH ----

func (r *plRun) foreach(f *plForeach, cur *Error) (plFlow, error) {
	a, err := r.eval(f.expr)
	if err != nil {
		return flowNext, err
	}
	if a.IsNull() {
		return flowNext, errf("22004", "FOREACH expression must not be null")
	}
	if a.K != KArray {
		return flowNext, errf("42804", "FOREACH expression must yield an array, not type %s", a.K)
	}
	var items []Datum
	if f.slice == 0 {
		items = flattenArray(a)
	} else {
		items = sliceArray(a, f.slice)
	}
	saved, had := r.vars[f.v]
	defer func() {
		if had {
			r.vars[f.v] = saved
		} else {
			delete(r.vars, f.v)
		}
	}()
	for _, it := range items {
		if k, ok := r.kinds[f.v]; ok && !it.IsNull() && it.K != k && k != KArray {
			if c, err := coerce(it, k); err == nil {
				it = c
			}
		}
		r.vars[f.v] = it
		if r.iters++; r.iters > maxPLIterations {
			return flowNext, errf("54000", "PL/pgSQL loop exceeded %d iterations", maxPLIterations)
		}
		flow, err := r.list(f.body, cur)
		if err != nil {
			return flowNext, err
		}
		switch flow {
		case flowReturn:
			return flow, nil
		case flowExit, flowContinue:
			if r.flowLabel != "" && r.flowLabel != strings.ToLower(f.label) {
				return flow, nil
			}
			r.flowLabel = ""
			if flow == flowExit {
				return flowNext, nil
			}
		}
	}
	return flowNext, nil
}

// flattenArray lists the elements of a (possibly multi-dimensional) array text.
func flattenArray(a Datum) []Datum {
	var out []Datum
	for _, e := range parseArrayText(a) {
		if e.K == KArray {
			out = append(out, flattenArray(e)...)
		} else if e.K == KText && strings.HasPrefix(e.S, "{") {
			out = append(out, flattenArray(Datum{K: KArray, S: e.S, I: a.I})...)
		} else {
			out = append(out, e)
		}
	}
	return out
}

// sliceArray cuts an array into slices of the given number of dimensions.
func sliceArray(a Datum, dims int) []Datum {
	depth := 0
	for _, c := range strings.TrimSpace(a.S) {
		if c != '{' {
			break
		}
		depth++
	}
	var walk func(s string, level int) []Datum
	walk = func(s string, level int) []Datum {
		if depth-level <= dims {
			return []Datum{{K: KArray, S: s, I: a.I}}
		}
		var out []Datum
		for _, e := range splitBraceElems(s) {
			out = append(out, walk(e, level+1)...)
		}
		return out
	}
	return walk(strings.TrimSpace(a.S), 0)
}

// splitBraceElems splits "{x,y,z}" into its elements at depth one.
func splitBraceElems(s string) []string {
	if len(s) < 2 {
		return nil
	}
	s = s[1 : len(s)-1]
	var out []string
	depth, start := 0, 0
	inq := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inq = !inq
		case inq && c == '\\':
			i++
		case inq:
		case c == '{':
			depth++
		case c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if s != "" {
		out = append(out, s[start:])
	}
	return out
}
