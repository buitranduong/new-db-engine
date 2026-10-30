package sql

import (
	"fmt"
	"strings"
	"sync"
)

// The system information functions the catalog queries call
// (https://www.postgresql.org/docs/16/functions-info.html). They are
// session functions, so a statement calling them runs on the general
// executor and the fast path never sees them.

// catalogFuncKinds are the result kinds of the catalog functions.
var catalogFuncKinds = map[string]Kind{
	"pg_table_is_visible": KBool, "pg_type_is_visible": KBool, "pg_function_is_visible": KBool,
	"has_table_privilege": KBool, "has_schema_privilege": KBool, "has_database_privilege": KBool,
	"has_sequence_privilege": KBool, "has_column_privilege": KBool, "has_any_column_privilege": KBool,
	"pg_get_userbyid": KText, "pg_get_expr": KText, "pg_get_indexdef": KText, "pg_get_constraintdef": KText,
	"format_type": KText, "obj_description": KText, "col_description": KText, "shobj_description": KText,
	"pg_get_serial_sequence": KText, "pg_encoding_to_char": KText, "quote_ident": KText,
	"current_schemas": KArray, "current_user": KText, "session_user": KText, "current_role": KText,
	"current_catalog": KText, "pg_get_viewdef": KText, "pg_relation_size": KInt, "pg_total_relation_size": KInt,
	"pg_table_size": KInt, "pg_indexes_size": KInt, "array_lower": KInt, "array_upper": KInt,
	"stask_subscript": KNull, "stask_field": KInt, "_pg_expandarray": KText, "stask_anyarr": KBool,
	"current_setting": KText, "set_config": KText, "to_regclass": KInt, "to_regtype": KInt, "pg_relation_is_publishable": KBool,
	"pg_get_statisticsobjdef_columns": KText, "pg_get_triggerdef": KText, "pg_get_ruledef": KText,
	"pg_get_partkeydef": KText, "pg_backend_pid": KInt, "pg_database_size": KInt, "pg_my_temp_schema": KInt,
	"pg_is_other_temp_schema": KBool, "pg_is_in_recovery": KBool, "pg_get_function_arguments": KText, "pg_get_function_result": KText,
}

// vecElems are the elements of an array value, or of an int2vector or
// oidvector, which pg_index prints as text ("1 2") and which PostgreSQL
// subscripts from 0 (zero is true for those).
func vecElems(d Datum) (el []Datum, zero bool) {
	if d.K == KArray {
		return parseArrayText(d), false
	}
	if t := strings.TrimSpace(d.String()); strings.HasPrefix(t, "{") { // an array literal or parameter in text form
		return parseArrayText(Datum{K: KArray, S: t, I: int64(KText)}), false
	}
	for _, f := range strings.Fields(d.String()) {
		v, err := coerce(TextD(f), KInt)
		if err != nil {
			return nil, true
		}
		el = append(el, v)
	}
	return el, true
}

// settingDefaults are the settings current_setting answers that a session
// does not hold as variables.
var settingDefaults = map[string]string{"server_version_num": "160000", "max_index_keys": "32",
	"max_identifier_length": "63", "block_size": "8192", "lc_collate": "C.UTF-8", "lc_ctype": "C.UTF-8",
	"is_superuser": "on", "session_authorization": roleName, "in_hot_standby": "off", "wal_level": "logical",
	"default_with_oids": "off", "transaction_read_only": "off", "extra_float_digits": "1"}

func init() {
	for n := range catalogFuncKinds {
		sessionFuncs[n] = true
	}
}

// seqOIDNames maps the OIDs of sequences resolved by ::regclass back to
// their names, for nextval('s'::regclass).
var seqOIDNames sync.Map // int64 -> string

// regclass resolves a relation name to its OID, as text::regclass does.
func (s *Session) regclass(name string) (Datum, error) {
	n := strings.TrimSpace(name)
	for _, p := range []string{"pg_catalog.", "public."} {
		n = strings.TrimPrefix(n, p)
	}
	n = seqName(n) // quotes and case, as for nextval's argument
	if strings.HasPrefix(name, infoPrefix) {
		n = name
	}
	if oid, ok := catalogRels[n]; ok {
		return IntD(oid), nil
	}
	if ts, err := s.E.UserTable(n); err == nil {
		return IntD(tableOID(ts.Schema())), nil
	}
	for _, v := range s.E.Views() {
		if v.Name == n {
			return IntD(viewOID(n)), nil
		}
	}
	if _, _, err := s.E.SequenceState(n); err == nil {
		oid := seqOID(n)
		seqOIDNames.Store(oid, n)
		return IntD(oid), nil
	}
	if t, ok := strings.CutSuffix(n, "_pkey"); ok {
		if ts, err := s.E.UserTable(t); err == nil {
			return IntD(indexOID(ts.Schema())), nil
		}
	}
	if _, taken := s.E.IndexNameTaken(n); taken {
		return IntD(secIndexOID(n)), nil
	}
	if virtualTables[n] {
		return IntD(12100), nil // a view of pg_catalog without an OID of its own here
	}
	return Null, errf(codeUndefinedTable, "relation %q does not exist", name)
}

// regtype resolves a type name to its OID (text::regtype).
func regtype(name string) (Datum, error) {
	n := strings.TrimPrefix(strings.TrimSpace(strings.ToLower(name)), "pg_catalog.")
	for _, t := range pgTypes {
		if t.name == n || t.sqlName == n {
			return IntD(t.oid), nil
		}
	}
	return Null, errf(codeUndefinedObject, "type %q does not exist", name)
}

// castCatalog evaluates casts to the OID types; ok is false for others.
func (c *evalCtx) castCatalog(v Datum, typ string) (Datum, bool, error) {
	switch typ {
	case "regclass", "regtype", "oid", "regproc", "regnamespace":
	default:
		return Null, false, nil
	}
	if v.IsNull() || v.K != KText {
		d, err := coerce(v, KInt)
		return d, true, err
	}
	if _, err := coerce(v, KInt); err == nil { // '16384'::regclass
		d, err := coerce(v, KInt)
		return d, true, err
	}
	switch typ {
	case "regclass":
		s := c.session()
		if s == nil {
			return v, true, nil // no catalog here: keep the name (nextval('s'::regclass))
		}
		d, err := s.regclass(v.S)
		return d, true, err
	case "regtype":
		d, err := regtype(v.S)
		return d, true, err
	case "regnamespace":
		switch strings.Trim(v.S, `"`) {
		case "pg_catalog":
			return IntD(nsPgCatalog), true, nil
		case "public":
			return IntD(nsPublic), true, nil
		case "information_schema":
			return IntD(nsInfoSchema), true, nil
		}
		return Null, true, errf(codeUndefinedObject, "schema %q does not exist", v.S)
	}
	d, err := coerce(v, KInt)
	return d, true, err
}

// seqNameByOID is the name of the sequence with an OID (nextval(oid)).
func (s *Session) seqNameByOID(oid int64) string {
	if n, ok := seqOIDNames.Load(oid); ok {
		return n.(string)
	}
	for _, q := range s.E.Sequences() {
		if seqOID(q.Name) == oid {
			seqOIDNames.Store(oid, q.Name)
			return q.Name
		}
	}
	return itoa(oid)
}

// catalogFunc runs a catalog relation or function; ok is false if name
// is neither.
func (s *Session) catalogFunc(name string, args []Datum) ([][]Datum, bool, error) {
	if r, ok := catalogTables[name]; ok {
		if r.rows == nil {
			return nil, true, nil
		}
		return r.rows(s.catSnap()), true, nil
	}
	if _, ok := catalogFuncKinds[name]; !ok {
		return nil, false, nil
	}
	d, err := s.catalogScalar(name, args)
	return [][]Datum{{d}}, true, err
}

func (s *Session) catalogScalar(name string, args []Datum) (Datum, error) {
	switch name {
	case "has_table_privilege", "has_schema_privilege", "has_database_privilege", "has_sequence_privilege",
		"has_column_privilege", "has_any_column_privilege":
		return s.hasPrivilege(name, args), nil
	case "current_user", "session_user", "current_role":
		return TextD(s.currentUser()), nil
	case "current_catalog":
		return TextD("stask"), nil
	case "current_schemas":
		if len(args) == 1 && args[0].Bool() {
			return arrayD([]Datum{TextD("pg_catalog"), TextD("public")}), nil
		}
		return arrayD([]Datum{TextD("public")}), nil
	case "pg_backend_pid":
		if s.PID > 0 {
			return IntD(int64(s.PID)), nil // the pid pg_stat_activity shows for this session
		}
		return IntD(1), nil
	case "pg_sleep":
		return s.supfixSleep(args)
	case "pg_my_temp_schema":
		return IntD(0), nil
	case "set_config": // set_config(name, value, is_local)
		if len(args) < 3 || args[0].IsNull() {
			return Null, errf(codeInvalidParameter, "set_config needs a name, a value and is_local")
		}
		val := ""
		if !args[1].IsNull() {
			val = args[1].String()
		}
		if _, err := s.set(&SetVar{Name: args[0].String(), Value: val, Local: args[2].Bool()}); err != nil {
			return Null, err
		}
		if cur, ok := s.vars[strings.ToLower(args[0].String())]; ok {
			return TextD(cur), nil // the setting after the change, e.g. 5s for statement_timeout
		}
		return TextD(val), nil
	case "current_setting": // current_setting(name [, missing_ok])
		if len(args) == 0 || args[0].IsNull() {
			return Null, nil
		}
		n := strings.ToLower(args[0].String())
		if v, ok := s.vars[n]; ok {
			return TextD(v), nil
		}
		if v, ok := settingDefaults[n]; ok {
			return TextD(v), nil
		}
		if len(args) > 1 && args[1].Bool() {
			return Null, nil
		}
		return Null, errf(codeUndefinedObject, "unrecognized configuration parameter %q", args[0].String())
	case "format_type": // the type modifier may be NULL
		if len(args) == 0 || args[0].IsNull() {
			return Null, nil
		}
		oid, _ := coerce(args[0], KInt)
		if oid.I == 0 {
			return TextD("-"), nil
		}
		if t, ok := pgTypeByOID[oid.I]; ok {
			if oid.I == 1700 && len(args) > 1 && !args[1].IsNull() {
				if m, err := coerce(args[1], KInt); err == nil && m.I >= 4 {
					return TextD(fmt.Sprintf("numeric(%d,%d)", (m.I-4)>>16&0xffff, (m.I-4)&0xffff)), nil
				}
			}
			if len(args) > 1 && !args[1].IsNull() && (oid.I == tyVarchar || oid.I == tyBpchar) {
				if m, err := coerce(args[1], KInt); err == nil && m.I >= 4 {
					return TextD(formatType(uint32(oid.I), int32(m.I))), nil
				}
			}
			if oid.I == tyTimestamptz && len(args) > 1 && !args[1].IsNull() {
				if m, err := coerce(args[1], KInt); err == nil && m.I >= 0 {
					return TextD(formatType(uint32(oid.I), int32(m.I))), nil
				}
			}
			if oid.I == tyBpchar {
				return TextD("bpchar"), nil // PostgreSQL prints char without a length as bpchar
			}
			return TextD(t.sqlName), nil
		}
		return TextD("???"), nil
	case "to_regclass", "to_regtype":
		if len(args) == 0 || args[0].IsNull() {
			return Null, nil
		}
		var d Datum
		var err error
		if name == "to_regclass" {
			d, err = s.regclass(args[0].String())
		} else {
			d, err = regtype(args[0].String())
		}
		if err != nil {
			return Null, nil
		}
		return d, nil
	}
	for _, a := range args {
		if a.IsNull() {
			return Null, nil // the rest are strict
		}
	}
	arg := func(i int) Datum {
		if i < len(args) {
			return args[i]
		}
		return Null
	}
	argInt := func(i int) int64 {
		d, _ := coerce(arg(i), KInt)
		return d.I
	}
	switch name {
	case "stask_anyarr": // x op ANY|ALL (array)
		el, _ := vecElems(arg(1))
		op, all := arg(2).String(), arg(3).Bool()
		x := arg(0)
		res, sawNull := all, false
		for _, e := range el {
			if e.IsNull() {
				sawNull = true
				continue
			}
			cmp, err := compare(x, e)
			if err != nil {
				return Null, err
			}
			var ok bool
			switch op {
			case "=":
				ok = cmp == 0
			case "<>":
				ok = cmp != 0
			case "<":
				ok = cmp < 0
			case "<=":
				ok = cmp <= 0
			case ">":
				ok = cmp > 0
			default:
				ok = cmp >= 0
			}
			if all && !ok {
				return BoolD(false), nil
			}
			if !all && ok {
				return BoolD(true), nil
			}
		}
		if sawNull {
			return Null, nil
		}
		return BoolD(res), nil
	case "pg_relation_is_publishable":
		return BoolD(true), nil
	case "pg_get_statisticsobjdef_columns", "pg_get_triggerdef", "pg_get_ruledef", "pg_get_partkeydef",
		"pg_get_function_arguments", "pg_get_function_result":
		return Null, nil
	case "pg_database_size":
		return IntD(0), nil
	case "pg_is_other_temp_schema", "pg_is_in_recovery":
		return BoolD(false), nil
	case "pg_table_is_visible", "pg_type_is_visible", "pg_function_is_visible":
		if name == "pg_type_is_visible" && len(args) == 1 && args[0].K == KInt && isInfoDomain(args[0].I) {
			return BoolD(false), nil
		}
		return BoolD(true), nil // everything is in pg_catalog or public, both on the search path
	case "pg_get_userbyid":
		return TextD(roleName), nil
	case "pg_get_expr":
		return arg(0), nil // adbin and conbin hold the expression's text
	case "pg_get_viewdef":
		for _, v := range s.E.Views() {
			if o := arg(0); o.K == KInt && o.I == viewOID(v.Name) || o.K == KText && o.S == v.Name {
				return TextD(s.viewDefText(v.Query, len(args) > 1 && args[1].Bool())), nil
			}
		}
		return Null, nil
	case "obj_description", "col_description", "shobj_description":
		return Null, nil
	case "pg_encoding_to_char":
		if argInt(0) == 6 {
			return TextD("UTF8"), nil
		}
		return TextD(""), nil
	case "quote_ident":
		return TextD(quoteIdent(arg(0).String())), nil
	case "pg_relation_size", "pg_total_relation_size", "pg_table_size", "pg_indexes_size":
		return s.supfixRelSize(arg(0), name != "pg_relation_size" && name != "pg_table_size"), nil
	case "pg_get_indexdef":
		oid := argInt(0)
		for _, t := range s.catSnap().tables {
			if t.HasPK() && indexOID(t) == oid {
				if len(args) > 1 && argInt(1) > 0 {
					if n, pk := int(argInt(1)), t.PKCols(); n <= len(pk) {
						return TextD(quoteIdent(pk[n-1])), nil
					}
					return TextD(""), nil
				}
				return TextD(indexDef(t)), nil
			}
		}
		if t, ix := s.catSnap().secIndexByOID(oid); ix != nil {
			if len(args) > 1 && argInt(1) > 0 {
				if n := int(argInt(1)); n <= len(ix.Columns) {
					return TextD(quoteIdent(ix.Columns[n-1])), nil
				}
				return TextD(""), nil
			}
			return TextD(secIndexDef(t, *ix)), nil
		}
		return Null, nil
	case "pg_get_constraintdef":
		oid := argInt(0)
		for _, t := range s.catSnap().tables {
			if t.HasPK() && pkeyConOID(t) == oid {
				return TextD("PRIMARY KEY (" + quoteList(t.PKCols()) + ")"), nil
			}
			for i, ck := range t.Checks {
				if checkOID(t, i) == oid {
					return TextD("CHECK (" + ck.Expr + ")"), nil
				}
			}
		}
		if d, ok := s.catSnap().secConstraintDef(oid); ok {
			return TextD(d), nil
		}
		return Null, nil
	case "pg_get_serial_sequence":
		tn := seqName(arg(0).String())
		cn := arg(1).String()
		for _, q := range s.E.Sequences() {
			if q.Owner == tn+"."+cn {
				return TextD("public." + quoteIdent(q.Name)), nil
			}
		}
		return Null, nil
	case "array_lower", "array_upper":
		el, _ := vecElems(arg(0))
		n := len(el)
		if n == 0 || argInt(1) != 1 {
			return Null, nil
		}
		if name == "array_lower" {
			return IntD(1), nil
		}
		return IntD(int64(n)), nil
	case "stask_subscript": // a[i]
		el, zero := vecElems(arg(0))
		i := argInt(1)
		if zero { // int2vector and oidvector count from 0
			i++
		}
		if i < 1 || int(i) > len(el) {
			return Null, nil
		}
		return el[i-1], nil
	case "_pg_expandarray":
		// information_schema._pg_expandarray(a) is a set of (x, n) records;
		// STASK's arrays here (an index's or constraint's columns) have one
		// element, so this is its one row.
		el, _ := vecElems(arg(0))
		switch len(el) {
		case 0:
			return Null, nil
		case 1:
			return TextD("(" + el[0].String() + ",1)"), nil
		}
		return Null, errf(codeFeature, "_pg_expandarray over more than one element is not supported")
	case "stask_field": // (record).field of an _pg_expandarray record
		rec := strings.Trim(arg(0).String(), "()")
		x, n, _ := strings.Cut(rec, ",")
		switch arg(1).String() {
		case "x":
			return coerce(TextD(x), KInt)
		case "n":
			return coerce(TextD(n), KInt)
		}
		return Null, errf(codeUndefinedColumn, "record has no field %q", arg(1).String())
	}
	return Null, errf(codeUndefinedFunction, "function %s does not exist", name)
}
