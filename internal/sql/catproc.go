package sql

import (
	"fmt"
	"strings"

	"stask/internal/core"
	"stask/internal/schema"
)

// pg_proc, pg_get_functiondef and friends for user functions (CREATE FUNCTION, plsql.go) and for the
// functions of the installed extensions (docs/14 appendix W). The built-in functions of PostgreSQL
// are not listed: an application that reads pg_proc asks for its own functions (Liquibase and
// Hibernate diffs, \df, pgAdmin).

const (
	funcOIDBase    = 0x5c000000
	extFuncOIDBase = 0x5e000000
	oidLangSQL     = 14
	oidLangPlpgsql = 13632
	oidLangC       = 13
	oidTrigger     = 2279
	oidVoid        = 2278
	oidInternal    = 2281
)

// funcOID is the OID of the overload of name that takes no arguments (what a trigger names).
func funcOID(name string) int64 { return funcOIDSig(name, "") }

// funcOIDSig is the OID of the overload of name with these input argument types.
func funcOIDSig(name, sig string) int64 {
	if sig == "" {
		return funcOIDBase + int64(hash32("f:"+name)&0x03ffffff)
	}
	return funcOIDBase + int64(hash32("f:"+name+"("+sig+")")&0x03ffffff)
}

func (d *funcDef) oid() int64 { return funcOIDSig(d.Name, d.sig()) }

func init() {
	// pg_language: what \df+ joins to name the language of a function
	defCat("pg_language", "oid:i lanname:t lanowner:i lanispl:b lanpltrusted:b lanplcallfoid:i laninline:i lanvalidator:i lanacl:t",
		func(c *catSnap) [][]Datum {
			return [][]Datum{
				{IntD(12), TextD("internal"), IntD(ownerOID), BoolD(false), BoolD(false), IntD(0), IntD(0), IntD(2246), Null},
				{IntD(13), TextD("c"), IntD(ownerOID), BoolD(false), BoolD(false), IntD(0), IntD(0), IntD(2247), Null},
				{IntD(14), TextD("sql"), IntD(ownerOID), BoolD(false), BoolD(true), IntD(0), IntD(0), IntD(2248), Null},
				{IntD(oidLangPlpgsql), TextD("plpgsql"), IntD(ownerOID), BoolD(true), BoolD(true), IntD(13641), IntD(13642), IntD(13643), Null},
			}
		})
	catalogTables["pg_language"].cols[1].ty = pgTy{oid: tyName, mod: -1}
	wrapCat("pg_proc", func(c *catSnap, rows [][]Datum) [][]Datum {
		for _, d := range allFuncs(c.s.E) {
			rows = append(rows, c.s.userProcRow(d))
		}
		for _, o := range c.s.E.Objs("ext") {
			for _, f := range extProcs[o.Name] {
				rows = append(rows, f.row(extData(o.Data).Schema))
			}
		}
		return rows
	})
}

// funcTypeOID is the OID of a declared type of a function (void and trigger included).
func funcTypeOID(typ, pg string) int64 {
	switch typ {
	case "void":
		return oidVoid
	case "trigger":
		return oidTrigger
	case "event_trigger":
		return 3838
	}
	return int64(PgTypeOID(funcCol(typ, pg)))
}

func funcCol(typ, pg string) schema.Column {
	ct, _ := schema.ParseType(typ)
	return schema.Column{Type: typ, PG: pg, T: ct}
}

func funcTypeName(typ, pg string) string {
	switch typ {
	case "void", "trigger", "event_trigger":
		return typ
	}
	return pgTypeName(funcCol(typ, pg))
}

func oidVector(oids []int64) string {
	parts := make([]string, len(oids))
	for i, o := range oids {
		parts[i] = fmt.Sprint(o)
	}
	return strings.Join(parts, " ")
}

func (s *Session) userProcRow(d funcDef) []Datum {
	kind, lang := "f", int64(oidLangSQL)
	if d.Procedure {
		kind = "p"
	}
	if d.Lang == "plpgsql" {
		lang = oidLangPlpgsql
	}
	vol := d.Vol
	if vol == "" {
		vol = "v"
	}
	var ins []int64
	var allTypes []int64
	var modes, names []string
	named, special, defaults := false, false, 0
	variadic := int64(0)
	for _, a := range d.Args {
		oid := funcTypeOID(a.Type, a.PG)
		allTypes = append(allTypes, oid)
		mode := map[string]string{"": "i", "o": "o", "b": "b", "v": "v", "t": "t"}[a.Mode]
		modes = append(modes, mode)
		names = append(names, a.Name)
		named = named || a.Name != ""
		special = special || a.Mode != ""
		if a.isIn() {
			ins = append(ins, oid)
			if a.Def != "" {
				defaults++
			}
		}
		if a.Mode == "v" {
			variadic = oid
		}
	}
	nm, all, md := Null, Null, Null
	if named {
		nm = Datum{K: KArray, S: "{" + strings.Join(names, ",") + "}", I: int64(KText)}
	}
	if special {
		all = Datum{K: KArray, S: "{" + strings.ReplaceAll(oidVector(allTypes), " ", ",") + "}", I: int64(KInt)}
		md = Datum{K: KArray, S: "{" + strings.Join(modes, ",") + "}", I: int64(KText)}
	}
	ret := int64(oidVoid)
	if !d.Procedure {
		ret = s.funcRetOID(&d)
	}
	var defTree Datum
	if defaults > 0 {
		defTree = TextD("(defaults)")
	}
	return []Datum{IntD(d.oid()), TextD(d.Name), IntD(nsPublic), IntD(ownerOID), IntD(lang), FloatD(100), FloatD(map[bool]float64{true: 1000, false: 0}[d.Set]), IntD(variadic),
		TextD("-"), TextD(kind), BoolD(false), BoolD(false), BoolD(d.Strict), BoolD(d.Set), TextD(vol), TextD("u"),
		IntD(int64(len(ins))), IntD(int64(defaults)), IntD(ret), TextD(oidVector(ins)), all, md, nm, defTree, Null,
		TextD(d.Body), Null, Null, Null, Null}
}

// funcRetOID is prorettype: record for several output columns, the row type of a table.
func (s *Session) funcRetOID(d *funcDef) int64 {
	outs := d.outs()
	switch {
	case len(outs) > 1 || d.Ret == "record":
		return 2249
	case len(outs) == 1:
		return funcTypeOID(outs[0].Type, outs[0].PG)
	}
	if _, err := s.E.UserTable(d.Ret); err == nil {
		return 2249 // a table row type has no pg_type row here
	}
	return funcTypeOID(d.Ret, d.RetPG)
}

// extProc is a function of an extension.
type extProc struct {
	name   string
	args   []int64
	ret    int64
	vol    string // i, s, v
	par    string // parallel safety: s, u
	strict bool
	src    string
}

func (f extProc) oid() int64 {
	return extFuncOIDBase + int64(hash32("e:"+f.name+"("+oidVector(f.args)+")")&0x03ffffff)
}

func (f extProc) row(sch string) []Datum {
	return []Datum{IntD(f.oid()), TextD(f.name),
		IntD(nsPublic), IntD(ownerOID), IntD(oidLangC), FloatD(1), FloatD(0), IntD(0), TextD("-"), TextD("f"), BoolD(false), BoolD(false),
		BoolD(f.strict), BoolD(false), TextD(f.vol), TextD(f.par), IntD(int64(len(f.args))), IntD(0), IntD(f.ret), TextD(oidVector(f.args)),
		Null, Null, Null, Null, Null, TextD(f.src), Null, Null, Null, Null}
}

const (
	oText, oBytea, oInt4, oUUID, oFloat4 = 25, 17, 23, 2950, 700
	oRegdict                             = 3769
)

// extProcs are the functions an extension adds, as PostgreSQL 16 lists them.
var extProcs = map[string][]extProc{
	"unaccent": {
		{"unaccent", []int64{oText}, oText, "s", "s", true, "unaccent_dict"},
		{"unaccent", []int64{oRegdict, oText}, oText, "s", "s", true, "unaccent_dict"},
		{"unaccent_init", []int64{oidInternal}, oidInternal, "v", "s", false, "unaccent_init"},
		{"unaccent_lexize", []int64{oidInternal, oidInternal, oidInternal, oidInternal}, oidInternal, "v", "s", false, "unaccent_lexize"},
	},
	"pgcrypto": {
		{"digest", []int64{oText, oText}, oBytea, "i", "s", true, "pg_digest"},
		{"digest", []int64{oBytea, oText}, oBytea, "i", "s", true, "pg_digest"},
		{"hmac", []int64{oText, oText, oText}, oBytea, "i", "s", true, "pg_hmac"},
		{"hmac", []int64{oBytea, oBytea, oText}, oBytea, "i", "s", true, "pg_hmac"},
		{"gen_random_bytes", []int64{oInt4}, oBytea, "v", "s", true, "pg_random_bytes"},
		{"gen_random_uuid", nil, oUUID, "v", "s", false, "pg_random_uuid"},
	},
	"uuid-ossp": {
		{"uuid_nil", nil, oUUID, "i", "s", true, "uuid_nil"},
		{"uuid_generate_v1", nil, oUUID, "v", "s", true, "uuid_generate_v1"},
		{"uuid_generate_v1mc", nil, oUUID, "v", "s", true, "uuid_generate_v1mc"},
		{"uuid_generate_v3", []int64{oUUID, oText}, oUUID, "i", "s", true, "uuid_generate_v3"},
		{"uuid_generate_v4", nil, oUUID, "v", "s", true, "uuid_generate_v4"},
		{"uuid_generate_v5", []int64{oUUID, oText}, oUUID, "i", "s", true, "uuid_generate_v5"},
		{"uuid_ns_dns", nil, oUUID, "i", "s", true, "uuid_ns_dns"},
		{"uuid_ns_url", nil, oUUID, "i", "s", true, "uuid_ns_url"},
		{"uuid_ns_oid", nil, oUUID, "i", "s", true, "uuid_ns_oid"},
		{"uuid_ns_x500", nil, oUUID, "i", "s", true, "uuid_ns_x500"},
	},
	"pg_trgm": {
		{"similarity", []int64{oText, oText}, oFloat4, "i", "s", true, "similarity"},
	},
}

// funcByOID finds a user function by its pg_proc OID.
func (s *Session) funcByOID(oid int64) (*funcDef, bool) {
	for _, d := range allFuncs(s.E) {
		if d.oid() == oid {
			d := d
			return &d, true
		}
	}
	return nil, false
}

// deparseDefault prints a parameter DEFAULT the way pg_get_function_arguments does.
func deparseDefault(def, typ string) string {
	toks, err := lex(def)
	if err != nil || len(toks) != 2 { // one token and EOF
		return def
	}
	switch t := toks[0]; t.k {
	case tString:
		return "'" + strings.ReplaceAll(t.s, "'", "''") + "'::" + typ
	case tNumber:
		return t.s
	case tIdent:
		if t.s == "null" {
			return "NULL::" + typ
		}
		return strings.ToUpper(t.s) // true, false
	}
	return def
}

// argText is pg_get_function_arguments: every argument but the columns of RETURNS TABLE, with
// its mode and DEFAULT.
func (d *funcDef) argText() string {
	var parts []string
	for _, a := range d.Args {
		if a.Mode == "t" {
			continue
		}
		p := ""
		switch a.Mode {
		case "o":
			p = "OUT "
		case "b":
			p = "INOUT "
		case "v":
			p = "VARIADIC "
		default:
			if d.Procedure {
				p = "IN "
			}
		}
		if a.Name != "" {
			p += quoteIdent(a.Name) + " "
		}
		p += funcTypeName(a.Type, a.PG)
		if a.Def != "" {
			p += " DEFAULT " + deparseDefault(a.Def, funcTypeName(a.Type, a.PG))
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// identityArgs is pg_get_function_identity_arguments: the input arguments without DEFAULTs.
func (d *funcDef) identityArgs() string {
	var parts []string
	for _, a := range d.Args {
		if !a.isIn() {
			if a.Mode == "o" {
				parts = append(parts, "OUT "+quoteIdentOpt(a.Name)+funcTypeName(a.Type, a.PG))
			}
			continue
		}
		p := ""
		if a.Mode == "b" {
			p = "INOUT "
		} else if a.Mode == "v" {
			p = "VARIADIC "
		}
		p += quoteIdentOpt(a.Name) + funcTypeName(a.Type, a.PG)
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

func quoteIdentOpt(n string) string {
	if n == "" {
		return ""
	}
	return quoteIdent(n) + " "
}

// resultText is pg_get_function_result.
func (d *funcDef) resultText() string {
	var tcols []string
	for _, a := range d.Args {
		if a.Mode == "t" {
			tcols = append(tcols, quoteIdent(a.Name)+" "+funcTypeName(a.Type, a.PG))
		}
	}
	if len(tcols) > 0 {
		return "TABLE(" + strings.Join(tcols, ", ") + ")"
	}
	outs := d.outs()
	t := funcTypeName(d.Ret, d.RetPG)
	switch {
	case len(outs) > 1:
		t = "record"
	case len(outs) == 1:
		t = funcTypeName(outs[0].Type, outs[0].PG)
	}
	if d.Set {
		return "SETOF " + t
	}
	return t
}

// functionDef is pg_get_functiondef.
func (d *funcDef) functionDef() string {
	var b strings.Builder
	kind, tag := "FUNCTION", "function"
	if d.Procedure {
		kind, tag = "PROCEDURE", "procedure"
	}
	fmt.Fprintf(&b, "CREATE OR REPLACE %s public.%s(%s)\n", kind, quoteIdent(d.Name), d.argText())
	if !d.Procedure {
		fmt.Fprintf(&b, " RETURNS %s\n", d.resultText())
	}
	fmt.Fprintf(&b, " LANGUAGE %s\n", d.Lang)
	switch d.Vol {
	case "i":
		b.WriteString(" IMMUTABLE\n")
	case "s":
		b.WriteString(" STABLE\n")
	}
	if d.Strict {
		b.WriteString(" STRICT\n")
	}
	q := "$" + tag + "$"
	for strings.Contains(d.Body, q) {
		q = q[:len(q)-1] + "x$"
	}
	fmt.Fprintf(&b, "AS %s%s%s\n", q, d.Body, q)
	return b.String()
}

// funcCatalogFn implements the catalog functions that describe functions and triggers; ok is
// false for any other name.
func (s *Session) funcCatalogFn(name string, args []Datum) (Datum, bool) {
	if len(args) == 0 {
		return Null, false
	}
	oid := int64(-1)
	if args[0].K == KInt {
		oid = args[0].I
	}
	switch name {
	case "pg_get_functiondef", "pg_get_function_arguments", "pg_get_function_result", "pg_get_function_identity_arguments":
		d, ok := s.funcByOID(oid)
		if !ok {
			return Null, true
		}
		switch name {
		case "pg_get_function_arguments":
			return TextD(d.argText()), true
		case "pg_get_function_identity_arguments":
			return TextD(d.identityArgs()), true
		case "pg_get_function_result":
			if d.Procedure {
				return Null, true
			}
			return TextD(d.resultText()), true
		}
		return TextD(d.functionDef()), true
	case "pg_get_triggerdef":
		for _, tg := range s.E.AllTriggers() {
			if trigOID(tg.Table, tg.Name) == oid {
				return TextD(triggerDef(tg)), true
			}
		}
		return Null, true
	}
	return Null, false
}

var _ = core.Obj{}
