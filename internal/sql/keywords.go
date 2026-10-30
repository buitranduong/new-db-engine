package sql

import "strings"

// pg_get_keywords(): Liquibase reads PostgreSQL's reserved words with it
// when it connects. The list is the words PostgreSQL 16 reserves (catcode
// R) or restricts as function or type names (T); a driver only needs it to
// know which identifiers to quote.

const reservedKeywords = "all analyse analyze and any array as asc asymmetric authorization binary both case cast check " +
	"collate collation column concurrently constraint create cross current_catalog current_date current_role " +
	"current_schema current_time current_timestamp current_user default deferrable desc distinct do else end except " +
	"false fetch for foreign freeze from full grant group having ilike in initially inner intersect into is isnull " +
	"join lateral leading left like limit localtime localtimestamp natural not notnull null offset on only or order " +
	"outer overlaps placing primary references returning right select session_user similar some symmetric " +
	"system_user table tablesample then to trailing true union unique user using variadic verbose when where window with"

func init() {
	sessionFuncs["pg_get_keywords"] = true
	tableFuncCols["pg_get_keywords"] = []relCol{{name: "word", kind: KText}, {name: "catcode", kind: KText},
		{name: "barelabel", kind: KBool}, {name: "catdesc", kind: KText}, {name: "baredesc", kind: KText}}
}

func keywordRows() [][]Datum {
	var rows [][]Datum
	for _, w := range strings.Fields(reservedKeywords) {
		rows = append(rows, []Datum{TextD(w), TextD("R"), BoolD(false), TextD("reserved"), TextD("can't be bare label")})
	}
	return rows
}
