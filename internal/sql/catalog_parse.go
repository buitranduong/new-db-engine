package sql

// catalogTokens rewrites the spellings that catalog queries of psql and
// JDBC drivers use into the forms the parser knows:
//
//   - OPERATOR(pg_catalog.~) becomes the operator ~ (psql's patterns);
//   - COLLATE name (COLLATE pg_catalog.default, COLLATE "C") is dropped:
//     there is one collation;
//   - pg_catalog. after :: and information_schema. before a function
//     call are dropped (::pg_catalog.regclass,
//     information_schema._pg_expandarray(...)), and ::"char" is the type
//     char;
//   - current_user, session_user, current_role and current_catalog
//     without parentheses are calls.
//
// Statements without these words come back unchanged, without copying.
func catalogTokens(toks []token) []token {
	hit := false
	for _, t := range toks {
		if t.k == tIdent {
			switch t.s {
			case "operator", "collate", "pg_catalog", "information_schema", "current_user", "session_user",
				"current_role", "current_catalog":
				hit = true
			}
		} else if t.k == tQuoted && t.s == "char" {
			hit = true
		}
	}
	if !hit {
		return toks
	}
	out := make([]token, 0, len(toks))
	is := func(i int, k tokKind, s string) bool { return i < len(toks) && toks[i].k == k && toks[i].s == s }
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		prevDot := len(out) > 0 && out[len(out)-1].k == tOp && out[len(out)-1].s == "."
		switch {
		case t.k == tIdent && t.s == "operator" && is(i+1, tOp, "("):
			// OPERATOR ( [schema .] op )
			j := i + 2
			if is(j, tIdent, "pg_catalog") && is(j+1, tOp, ".") {
				j += 2
			}
			if j+1 < len(toks) && toks[j].k == tOp && is(j+1, tOp, ")") {
				out = append(out, token{tOp, toks[j].s, t.pos})
				i = j + 1
				continue
			}
		case t.k == tIdent && t.s == "collate" && !prevDot:
			j := i + 1
			if j < len(toks) && (toks[j].k == tIdent || toks[j].k == tQuoted) {
				if is(j+1, tOp, ".") && j+2 < len(toks) && (toks[j+2].k == tIdent || toks[j+2].k == tQuoted) {
					j += 2
				}
				i = j
				continue
			}
		case t.k == tIdent && t.s == "pg_catalog" && is(i+1, tOp, ".") && len(out) > 0 &&
			out[len(out)-1].k == tOp && out[len(out)-1].s == "::":
			i++ // ::pg_catalog.type
			continue
		case t.k == tIdent && t.s == "information_schema" && is(i+1, tOp, ".") && i+3 < len(toks) &&
			toks[i+2].k == tIdent && is(i+3, tOp, "("):
			i++ // information_schema.func(
			continue
		case t.k == tQuoted && t.s == "char" && len(out) > 0 && out[len(out)-1].k == tOp && out[len(out)-1].s == "::":
			t.k = tIdent
		case t.k == tIdent && !prevDot && !is(i+1, tOp, "(") && (t.s == "current_user" || t.s == "session_user" ||
			t.s == "current_role" || t.s == "current_catalog"):
			out = append(out, t, token{tOp, "(", t.pos}, token{tOp, ")", t.pos})
			continue
		}
		out = append(out, t)
	}
	return out
}
