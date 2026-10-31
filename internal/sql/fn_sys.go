package sql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// System information and administration functions (PostgreSQL 16 section 9.27): pg_sleep_for/until,
// pg_size_bytes, txid snapshot functions, pg_input_is_valid/pg_input_error_info, num_nulls, ...
// Precedent: PostgreSQL docs tables 9.70-9.80.

var serverStart = time.Now()

func sessSleep(c *evalCtx, d time.Duration) error {
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return nil
		}
		if s := c.session(); s != nil && s.interrupted() {
			return errf(codeQueryCanceled, "canceling statement due to user request")
		}
		time.Sleep(min(left, 50*time.Millisecond))
	}
}

func snapshotText(c *evalCtx) string {
	x := currentXid(c)
	return fmt.Sprintf("%d:%d:", x, x)
}

func currentXid(c *evalCtx) int64 {
	if d, ok, err := c.callSession(&Func{Name: "txid_current"}, nil); ok && err == nil && d.K == KInt {
		return d.I
	}
	return 1
}

func parseSnapshot(s string) (xmin, xmax int64, xip []int64, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, nil, errf(codeInvalidText, "invalid input syntax for type pg_snapshot: \"%s\"", s)
	}
	if xmin, err = strconv.ParseInt(parts[0], 10, 64); err == nil {
		xmax, err = strconv.ParseInt(parts[1], 10, 64)
	}
	if err != nil {
		return 0, 0, nil, errf(codeInvalidText, "invalid input syntax for type pg_snapshot: \"%s\"", s)
	}
	for _, p := range strings.Split(parts[2], ",") {
		if p == "" {
			continue
		}
		if v, e := strconv.ParseInt(p, 10, 64); e == nil {
			xip = append(xip, v)
		}
	}
	return
}

// parseSize reads "1.5 GB" as bytes (pg_size_bytes).
func parseSize(s string) (int64, error) {
	bad := func() (int64, error) { return 0, errf(codeInvalidParameter, "invalid size: \"%s\"", s) }
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && (t[i] == '-' || t[i] == '+' || t[i] == '.' || t[i] >= '0' && t[i] <= '9' || t[i] == 'e' || t[i] == 'E' && i > 0) {
		if (t[i] == 'e' || t[i] == 'E') && (i+1 >= len(t) || !(t[i+1] >= '0' && t[i+1] <= '9' || t[i+1] == '-' || t[i+1] == '+')) {
			break
		}
		i++
	}
	if i == 0 {
		return bad()
	}
	num, err := strconv.ParseFloat(t[:i], 64)
	if err != nil {
		return bad()
	}
	unit := strings.TrimSpace(t[i:])
	mult := map[string]float64{"": 1, "b": 1, "kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30, "tb": 1 << 40, "pb": 1 << 50}
	m, ok := mult[strings.ToLower(unit)]
	if !ok {
		return 0, errf(codeInvalidParameter, "invalid size: \"%s\"", s)
	}
	v := math.Round(num * m)
	if v > math.MaxInt64 || v < math.MinInt64 {
		return 0, errf(codeNumericOutOfRange, "bigint out of range")
	}
	return int64(v), nil
}

func init() {
	voidFn := func(c *evalCtx, a []Datum) (Datum, error) { return TextD(""), nil }
	regFn("pg_sleep_for", &fnSig{min: 1, max: 1, kind: KText, ty: tyOf(tyVoid), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		iv, err := coerce(a[0], KInterval)
		if err != nil {
			return Null, err
		}
		if err := sessSleep(c, time.Duration(intervalMs(iv))*time.Microsecond); err != nil {
			return Null, err
		}
		return voidFn(c, a)
	}})
	regFn("pg_sleep_until", &fnSig{min: 1, max: 1, kind: KText, ty: tyOf(tyVoid), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		t, err := coerce(a[0], KTime)
		if err != nil {
			return Null, err
		}
		if err := sessSleep(c, time.Until(goTime(t.I))); err != nil {
			return Null, err
		}
		return voidFn(c, a)
	}})
	regFn("pg_postmaster_start_time pg_conf_load_time", &fnSig{min: 0, max: 0, kind: KTime, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		return TimeD(serverStart.UnixMicro()), nil
	}})
	regFn("pg_size_bytes", &fnSig{min: 1, max: 1, kind: KInt, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		v, err := parseSize(a[0].String())
		return IntD(v), err
	}})
	regFn("has_function_privilege pg_has_role has_type_privilege has_language_privilege has_tablespace_privilege has_foreign_data_wrapper_privilege has_server_privilege has_any_column_privilege", &fnSig{min: 2, max: 3, kind: KBool,
		impl: func(c *evalCtx, a []Datum) (Datum, error) { return BoolD(true), nil }})
	regFn("pg_char_to_encoding", &fnSig{min: 1, max: 1, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		enc := map[string]int64{"SQL_ASCII": 0, "UTF8": 6, "LATIN1": 8, "LATIN2": 9, "LATIN9": 16, "WIN1252": 40, "WIN1251": 39}[strings.ToUpper(a[0].String())]
		if enc == 0 && strings.ToUpper(a[0].String()) != "SQL_ASCII" {
			return IntD(-1), nil
		}
		return IntD(enc), nil
	}})
	regFn("pg_client_encoding", &fnSig{min: 0, max: 0, kind: KText, ty: tyOf(tyName), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		return TextD("UTF8"), nil
	}})
	regFn("pg_notification_queue_usage", &fnSig{min: 0, max: 0, kind: KFloat, impl: func(c *evalCtx, a []Datum) (Datum, error) { return FloatD(0), nil }})
	regFn("pg_trigger_depth", &fnSig{min: 0, max: 0, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) { return IntD(0), nil }})
	regFn("pg_blocking_pids pg_safe_snapshot_blocking_pids", &fnSig{min: 1, max: 1, kind: KArray, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		return Datum{K: KArray, S: "{}", I: int64(KInt)}, nil
	}})
	regFn("pg_current_logfile", &fnSig{min: 0, max: 1, nonStrict: true, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) { return Null, nil }})

	// transaction ids and snapshots (txid_snapshot / pg_snapshot are carried as their text form)
	regFn("txid_current_if_assigned pg_current_xact_id_if_assigned", &fnSig{min: 0, max: 0, kind: KInt, impl: func(c *evalCtx, a []Datum) (Datum, error) { return IntD(currentXid(c)), nil }})
	regFn("pg_current_xact_id", &fnSig{min: 0, max: 0, kind: KInt, impl: func(c *evalCtx, a []Datum) (Datum, error) { return IntD(currentXid(c)), nil }})
	regFn("txid_current_snapshot pg_current_snapshot", &fnSig{min: 0, max: 0, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) { return TextD(snapshotText(c)), nil }})
	regFn("txid_snapshot_xmin pg_snapshot_xmin", &fnSig{min: 1, max: 1, kind: KInt, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		x, _, _, err := parseSnapshot(a[0].String())
		return IntD(x), err
	}})
	regFn("txid_snapshot_xmax pg_snapshot_xmax", &fnSig{min: 1, max: 1, kind: KInt, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		_, x, _, err := parseSnapshot(a[0].String())
		return IntD(x), err
	}})
	regFn("txid_visible_in_snapshot pg_visible_in_snapshot", &fnSig{min: 2, max: 2, kind: KBool, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		xid, err := fnInt(a[0])
		if err != nil {
			return Null, err
		}
		xmin, xmax, xip, err := parseSnapshot(a[1].String())
		if err != nil {
			return Null, err
		}
		if xid < xmin {
			return BoolD(true), nil
		}
		if xid >= xmax {
			return BoolD(false), nil
		}
		for _, x := range xip {
			if x == xid {
				return BoolD(false), nil
			}
		}
		return BoolD(true), nil
	}})
	regFn("txid_status pg_xact_status", &fnSig{min: 1, max: 1, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		xid, err := fnInt(a[0])
		if err != nil {
			return Null, err
		}
		switch cur := currentXid(c); {
		case xid == cur:
			return TextD("in progress"), nil
		case xid < cur:
			return TextD("committed"), nil
		}
		return Null, nil
	}})

	regFn("num_nulls", &fnSig{min: 1, max: -1, nonStrict: true, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n := 0
		for _, d := range a {
			if d.IsNull() {
				n++
			}
		}
		return IntD(int64(n)), nil
	}})
	regFn("num_nonnulls", &fnSig{min: 1, max: -1, nonStrict: true, kind: KInt, ty: tyOf(tyInt4), impl: func(c *evalCtx, a []Datum) (Datum, error) {
		n := 0
		for _, d := range a {
			if !d.IsNull() {
				n++
			}
		}
		return IntD(int64(n)), nil
	}})
	regFn("pg_collation_for", &fnSig{min: 1, max: 1, nonStrict: true, kind: KText, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		switch a[0].K {
		case KNull:
			return Null, nil
		case KText:
			return TextD(`"default"`), nil
		}
		return Null, errf(codeDatatypeMismatch, "collations are not supported by type %s", pgKindName(a[0].K))
	}})

	// pg_input_is_valid / pg_input_error_info: try the cast and report
	regFn("pg_input_is_valid", &fnSig{min: 2, max: 2, kind: KBool, impl: func(c *evalCtx, a []Datum) (Datum, error) {
		_, err := tryInputCast(a[0].String(), a[1].String())
		if err == nil {
			return BoolD(true), nil
		}
		if e, ok := err.(*Error); ok && e.Code == codeUndefinedObject {
			return Null, err
		}
		return BoolD(false), nil
	}})
	srfDefs["pg_input_error_info"] = srfDef{[]string{"message", "detail", "hint", "sql_error_code"}, []Kind{KText, KText, KText, KText}}
}

// tryInputCast converts text to the named type with the engine's cast rules.
func tryInputCast(text, typ string) (d Datum, err error) {
	if strings.ContainsAny(typ, ";'\"\\") || strings.TrimSpace(typ) == "" {
		return Null, errf(codeUndefinedObject, "type \"%s\" does not exist", typ)
	}
	for _, r := range typ {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_ ()[],.", r)) {
			return Null, errf(codeUndefinedObject, "type \"%s\" does not exist", typ)
		}
	}
	st, perr := ParseOne("SELECT NULL::" + typ)
	if perr != nil {
		return Null, errf(codeUndefinedObject, "type \"%s\" does not exist", typ)
	}
	sel, ok := st.(*Select)
	if !ok || len(sel.Items) != 1 {
		return Null, errf(codeUndefinedObject, "type \"%s\" does not exist", typ)
	}
	cast, ok := sel.Items[0].X.(*Cast)
	if !ok {
		return Null, errf(codeUndefinedObject, "type \"%s\" does not exist", typ)
	}
	defer func() {
		if r := recover(); r != nil {
			err = errf(codeInvalidText, "invalid input")
		}
	}()
	return (&evalCtx{}).eval(&Cast{X: &Lit{TextD(text)}, Type: cast.Type, PG: cast.PG})
}

func pgInputErrorRows(a []Datum) [][]Datum {
	if a[0].IsNull() || a[1].IsNull() {
		return [][]Datum{{Null, Null, Null, Null}}
	}
	_, err := tryInputCast(a[0].String(), a[1].String())
	if err == nil {
		return [][]Datum{{Null, Null, Null, Null}}
	}
	e, _ := err.(*Error)
	if e == nil {
		return [][]Datum{{TextD(err.Error()), Null, Null, TextD("22P02")}}
	}
	return [][]Datum{{TextD(e.Msg), Null, Null, TextD(e.Code)}}
}

func init() {
	// result types only; the session functions run in catalog_funcs.go
	fall := func(c *evalCtx, a []Datum) (Datum, error) { return Null, errFnFall }
	regFn("pg_sleep", &fnSig{min: 1, max: 1, kind: KText, ty: tyOf(tyVoid), impl: fall})
	regFn("pg_backend_pid", &fnSig{min: 0, max: 0, kind: KInt, ty: tyOf(tyInt4), impl: fall})
}
