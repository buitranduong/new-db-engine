package sql

import (
	"encoding/json"
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

func funcOID(name string) int64 { return funcOIDBase + int64(hash32("f:"+name)&0x03ffffff) }

func init() {
	wrapCat("pg_proc", func(c *catSnap, rows [][]Datum) [][]Datum {
		for _, o := range c.s.E.Objs("func") {
			var d funcDef
			if json.Unmarshal([]byte(o.Data), &d) != nil {
				continue
			}
			rows = append(rows, userProcRow(d))
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
	}
	return int64(PgTypeOID(funcCol(typ, pg)))
}

func funcCol(typ, pg string) schema.Column {
	ct, _ := schema.ParseType(typ)
	return schema.Column{Type: typ, PG: pg, T: ct}
}

func funcTypeName(typ, pg string) string {
	switch typ {
	case "void", "trigger":
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

func userProcRow(d funcDef) []Datum {
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
	args := make([]int64, len(d.Args))
	var names []string
	named := false
	for i, a := range d.Args {
		args[i] = funcTypeOID(a.Type, a.PG)
		names = append(names, a.Name)
		named = named || a.Name != ""
	}
	nm := Null
	if named {
		nm = Datum{K: KArray, S: "{" + strings.Join(names, ",") + "}", I: int64(KText)}
	}
	ret := int64(oidVoid)
	if !d.Procedure {
		ret = funcTypeOID(d.Ret, d.RetPG)
	}
	return []Datum{IntD(funcOID(d.Name)), TextD(d.Name), IntD(nsPublic), IntD(ownerOID), IntD(lang), FloatD(100), FloatD(0), IntD(0),
		TextD("-"), TextD(kind), BoolD(false), BoolD(false), BoolD(d.Strict), BoolD(false), TextD(vol), TextD("u"),
		IntD(int64(len(d.Args))), IntD(0), IntD(ret), TextD(oidVector(args)), Null, Null, nm, Null, Null,
		TextD(d.Body), Null, Null, Null, Null}
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

func (f extProc) row(sch string) []Datum {
	return []Datum{IntD(extFuncOIDBase + int64(hash32("e:"+f.name+"("+oidVector(f.args)+")")&0x03ffffff)), TextD(f.name),
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
	for _, o := range s.E.Objs("func") {
		var d funcDef
		if json.Unmarshal([]byte(o.Data), &d) == nil && funcOID(d.Name) == oid {
			return &d, true
		}
	}
	return nil, false
}

func (d *funcDef) argText() string {
	parts := make([]string, len(d.Args))
	for i, a := range d.Args {
		p := ""
		if d.Procedure {
			p = "IN "
		}
		if a.Name != "" {
			p += quoteIdent(a.Name) + " "
		}
		parts[i] = p + funcTypeName(a.Type, a.PG)
	}
	return strings.Join(parts, ", ")
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
		fmt.Fprintf(&b, " RETURNS %s\n", funcTypeName(d.Ret, d.RetPG))
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
	case "pg_get_functiondef", "pg_get_function_arguments", "pg_get_function_result":
		d, ok := s.funcByOID(oid)
		if !ok {
			return Null, true
		}
		switch name {
		case "pg_get_function_arguments":
			return TextD(d.argText()), true
		case "pg_get_function_result":
			if d.Procedure {
				return Null, true
			}
			return TextD(funcTypeName(d.Ret, d.RetPG)), true
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
