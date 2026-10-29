package sql

import (
	"encoding/json"
	"sort"
	"strings"

	"stask/internal/schema"
)

// pg_catalog and information_schema rows for schemas, enum types, domains,
// extensions and comments (branch ddlobjs). The relations are defined in
// catalog_rels.go and catalog_info.go; the wrappers below, registered after
// them (init order follows the file names), add to or adjust their rows.

func wrapCat(name string, f func(c *catSnap, rows [][]Datum) [][]Datum) {
	r := catalogTables[name]
	if r == nil {
		panic("ddlobjs: no catalog relation " + name)
	}
	old := r.rows
	r.rows = func(c *catSnap) [][]Datum {
		var rows [][]Datum
		if old != nil {
			rows = old(c)
		}
		return f(c, rows)
	}
}

// typeByOID is pgTypeByOID that knows the user types and never returns nil.
func typeByOID(oid int64) *pgType {
	if t, ok := pgTypeByOID[oid]; ok {
		return t
	}
	if ut := userTypeByOID(uint32(oid)); ut != nil {
		if ut.Kind == "enum" {
			return &pgType{oid: oid, name: bareName(ut.Name), len: 4, cat: "E", sqlName: ut.Name, infoName: "USER-DEFINED"}
		}
		if ut.tmp != nil {
			b := typeByOID(int64(PgTypeOID(ut.tmp.Columns[1])))
			return &pgType{oid: oid, name: bareName(ut.Name), len: b.len, cat: b.cat, sqlName: ut.Name, infoName: b.infoName}
		}
	}
	return pgTypeByOID[25]
}

// nsInfo knows the user schemas of a catalog query.
type nsInfo map[string]bool

func (c *catSnap) nsInfo() nsInfo {
	m := nsInfo{}
	for _, o := range c.s.E.Objs("schema") {
		m[o.Name] = true
	}
	return m
}

// split gives a stored name's schema OID, schema name and bare name.
func (n nsInfo) split(name string) (int64, string, string) {
	if sch, b, ok := splitSchema(name); ok && n[sch] {
		return schemaOID(sch), sch, b
	}
	return nsPublic, "public", name
}

func init() {
	wrapCat("pg_namespace", func(c *catSnap, rows [][]Datum) [][]Datum {
		for _, o := range c.s.E.Objs("schema") {
			rows = append(rows, []Datum{IntD(schemaOID(o.Name)), TextD(o.Name), IntD(ownerOID), Null})
		}
		return rows
	})

	wrapCat("pg_class", func(c *catSnap, rows [][]Datum) [][]Datum {
		ns := c.nsInfo()
		if len(ns) == 0 {
			return rows
		}
		idxNS := map[int64]int64{}
		secIndexRows(c, func(t *schema.Table, ix schema.IndexDef) {
			o, _, _ := ns.split(t.Name)
			idxNS[secIndexOID(ix.Name)] = o
		})
		for _, r := range rows {
			if o, ok := idxNS[r[0].I]; ok {
				r[2] = IntD(o)
				continue
			}
			o, _, b := ns.split(r[1].S)
			r[1], r[2] = TextD(b), IntD(o)
		}
		return rows
	})

	// schemaname/tablename pairs
	fixPair := func(name string, schCol, nameCol int, extra func(r []Datum, sch, bare string)) {
		wrapCat(name, func(c *catSnap, rows [][]Datum) [][]Datum {
			ns := c.nsInfo()
			if len(ns) == 0 {
				return rows
			}
			for _, r := range rows {
				full := r[nameCol].S
				if _, sch, b := ns.split(full); sch != "public" {
					r[schCol], r[nameCol] = TextD(sch), TextD(b)
					if extra != nil {
						extra(r, sch, b)
					}
				}
			}
			return rows
		})
	}
	fixPair("pg_tables", 0, 1, nil)
	fixPair("pg_views", 0, 1, nil)
	wrapCat("pg_indexes", func(c *catSnap, rows [][]Datum) [][]Datum {
		ns := c.nsInfo()
		if len(ns) == 0 {
			return rows
		}
		for _, r := range rows {
			if _, sch, b := ns.split(r[1].S); sch != "public" {
				full := r[1].S
				r[0], r[1] = TextD(sch), TextD(b)
				if _, ib, ok := splitSchema(r[2].S); ok {
					r[2] = TextD(ib)
				}
				d := r[4].S
				d = strings.ReplaceAll(d, "public."+quoteIdent(full), quoteIdent(sch)+"."+quoteIdent(b))
				d = strings.ReplaceAll(d, quoteIdent(full+"_pkey"), quoteIdent(b+"_pkey"))
				r[4] = TextD(d)
			}
		}
		return rows
	})

	// information_schema: table_schema, table_name (and sequence_*)
	for _, v := range []string{"tables", "columns", "sequences", "views"} {
		fixPair(infoPrefix+v, 1, 2, nil)
	}
	wrapCat(infoPrefix+"schemata", func(c *catSnap, rows [][]Datum) [][]Datum {
		for _, o := range c.s.E.Objs("schema") {
			rows = append(rows, []Datum{TextD("stask"), TextD(o.Name), TextD(roleName), Null, Null, Null, Null})
		}
		return rows
	})
	wrapCat(infoPrefix+"columns", func(c *catSnap, rows [][]Datum) [][]Datum {
		// enum columns say USER-DEFINED, domain columns name their domain
		i := 0
		for _, t := range c.tables {
			for _, col := range t.Columns {
				if i >= len(rows) {
					return rows
				}
				r := rows[i]
				i++
				switch {
				case strings.HasPrefix(col.PG, "enum:"):
					ut := lookupUserType(col.PG[len("enum:"):])
					if ut != nil {
						r[7] = TextD("USER-DEFINED")
						r[26], r[27] = TextD(schemaOfName(ut.Name)), TextD(bareName(ut.Name))
					}
				case col.Domain != "":
					r[22], r[23], r[24] = TextD("stask"), TextD(schemaOfName(col.Domain)), TextD(bareName(col.Domain))
				}
			}
		}
		return rows
	})
	wrapCat(infoPrefix+"domains", func(c *catSnap, rows [][]Datum) [][]Datum {
		loadUserTypes(c.s.E)
		for _, o := range c.s.E.Objs("domain") {
			ut := lookupUserType(o.Name)
			if ut == nil || ut.tmp == nil {
				continue
			}
			rows = append(rows, []Datum{TextD("stask"), TextD(schemaOfName(ut.Name)), TextD(bareName(ut.Name)),
				TextD(typeByOID(int64(PgTypeOID(ut.tmp.Columns[1]))).infoName)})
		}
		return rows
	})

	// attributes of domain columns have the domain as their type
	wrapCat("pg_attribute", func(c *catSnap, rows [][]Datum) [][]Datum {
		dom := map[[2]int64]int64{}
		for _, t := range c.tables {
			for _, col := range t.Columns {
				if col.Domain != "" {
					dom[[2]int64{tableOID(t), int64(col.Num)}] = int64(userTypeOID(col.Domain))
				}
			}
		}
		if len(dom) == 0 {
			return rows
		}
		for _, r := range rows {
			if o, ok := dom[[2]int64{r[0].I, r[5].I}]; ok {
				r[2] = IntD(o)
			}
		}
		return rows
	})

	wrapCat("pg_type", func(c *catSnap, rows [][]Datum) [][]Datum {
		loadUserTypes(c.s.E)
		ns := c.nsInfo()
		for _, kind := range []string{"enum", "domain"} {
			for _, o := range c.s.E.Objs(kind) {
				ut := lookupUserType(o.Name)
				if ut == nil {
					continue
				}
				nsoid, _, bare := ns.split(ut.Name)
				if ut.Kind == "enum" {
					rows = append(rows, []Datum{IntD(int64(ut.OID)), TextD(bare), IntD(nsoid), IntD(ownerOID), IntD(4),
						BoolD(true), TextD("e"), TextD("E"), BoolD(false), BoolD(true), TextD(","), IntD(0), IntD(0),
						IntD(0), IntD(0), TextD("enum_in"), TextD("enum_out"), TextD("enum_recv"), TextD("enum_send"),
						TextD("-"), TextD("-"), TextD("-"), TextD("i"), TextD("p"), BoolD(false), IntD(0), IntD(-1),
						IntD(0), IntD(0), Null, Null, Null})
					continue
				}
				if ut.tmp == nil {
					continue
				}
				boid := int64(PgTypeOID(ut.tmp.Columns[1]))
				b := typeByOID(boid)
				def := Null
				if ut.Default != "" {
					def = TextD(ut.Default)
				}
				rows = append(rows, []Datum{IntD(int64(ut.OID)), TextD(bare), IntD(nsoid), IntD(ownerOID), IntD(b.len),
					BoolD(b.len > 0 && b.len <= 8), TextD("d"), TextD(b.cat), BoolD(false), BoolD(true), TextD(","), IntD(0),
					IntD(0), IntD(0), IntD(0), TextD("domain_in"), TextD(b.name + "out"), TextD("domain_recv"),
					TextD(b.name + "send"), TextD("-"), TextD("-"), TextD("-"), TextD("i"), TextD("p"), BoolD(ut.NotNull),
					IntD(boid), IntD(typmodOf(ut.tmp.Columns[1])), IntD(0), IntD(0), Null, def, Null})
			}
		}
		return rows
	})

	wrapCat("pg_enum", func(c *catSnap, rows [][]Datum) [][]Datum {
		loadUserTypes(c.s.E)
		for _, o := range c.s.E.Objs("enum") {
			ut := lookupUserType(o.Name)
			if ut == nil {
				continue
			}
			for _, l := range ut.Labels {
				rows = append(rows, []Datum{IntD(enumLabelOID(ut.Name, l.L)), IntD(int64(ut.OID)), FloatD(l.O), TextD(l.L)})
			}
		}
		return rows
	})

	wrapCat("pg_constraint", func(c *catSnap, rows [][]Datum) [][]Datum {
		loadUserTypes(c.s.E)
		ns := c.nsInfo()
		for _, o := range c.s.E.Objs("domain") {
			ut := lookupUserType(o.Name)
			if ut == nil {
				continue
			}
			nsoid, _, _ := ns.split(ut.Name)
			for _, ck := range ut.Checks {
				rows = append(rows, []Datum{IntD(domConOID(ut.Name, ck.Name)), TextD(ck.Name), IntD(nsoid), TextD("c"),
					BoolD(false), BoolD(false), BoolD(true), IntD(0), IntD(int64(ut.OID)), IntD(0), IntD(0), IntD(0),
					TextD(" "), TextD(" "), TextD(" "), BoolD(true), IntD(0), BoolD(false), Null, Null, Null, Null,
					Null, Null, Null, TextD(ck.Expr)})
			}
		}
		return rows
	})

	wrapCat("pg_extension", func(c *catSnap, rows [][]Datum) [][]Datum {
		rows = append(rows, []Datum{IntD(extensionOID("plpgsql")), TextD("plpgsql"), IntD(ownerOID), IntD(nsPgCatalog),
			BoolD(false), TextD("1.0")})
		ns := c.nsInfo()
		for _, o := range c.s.E.Objs("ext") {
			d := extData(o.Data)
			nsoid, _, _ := ns.split(d.Schema + "." + o.Name)
			if d.Schema == "public" || d.Schema == "" {
				nsoid = nsPublic
			}
			rows = append(rows, []Datum{IntD(extensionOID(o.Name)), TextD(o.Name), IntD(ownerOID), IntD(nsoid),
				BoolD(true), TextD(d.Version)})
		}
		return rows
	})

	wrapCat("pg_description", func(c *catSnap, rows [][]Datum) [][]Datum {
		return append(rows, c.descriptions()...)
	})
}

// descriptions are the rows of pg_description: the stored comments, and
// what PostgreSQL itself says about the public schema and the extensions.
func (c *catSnap) descriptions() [][]Datum {
	const clsClass, clsType, clsNS, clsExt = pgClassOID, pgTypeRelOID, pgNspRelOID, 3079
	cm := c.s.comments()
	var rows [][]Datum
	add := func(oid, cls, sub int64, text string) {
		rows = append(rows, []Datum{IntD(oid), IntD(cls), IntD(sub), TextD(text)})
	}
	if _, ok := cm["schema:public"]; !ok {
		add(nsPublic, clsNS, 0, "standard public schema")
	}
	if _, ok := cm["ext:plpgsql"]; !ok {
		add(extensionOID("plpgsql"), clsExt, 0, "PL/pgSQL procedural language")
	}
	for _, o := range c.s.E.Objs("ext") {
		if _, ok := cm["ext:"+o.Name]; !ok {
			add(extensionOID(o.Name), clsExt, 0, extensions[o.Name].comment)
		}
	}
	keys := make([]string, 0, len(cm))
	for k := range cm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kind, name, _ := strings.Cut(k, ":")
		text := cm[k]
		switch kind {
		case "rel":
			if t := c.tableNamed(name); t != nil {
				add(tableOID(t), clsClass, 0, text)
			} else if c.viewNamed(name) {
				add(viewOID(name), clsClass, 0, text)
			}
		case "col":
			tn, cn, _ := strings.Cut(name, "#")
			if t := c.tableNamed(tn); t != nil {
				if i, ok := t.Col(cn); ok {
					add(tableOID(t), clsClass, int64(t.Columns[i].Num), text)
				}
			} else if c.viewNamed(tn) {
				for _, v := range c.views {
					for i, vc := range v.Cols {
						if v.Name == tn && vc == cn {
							add(viewOID(tn), clsClass, int64(i+1), text)
						}
					}
				}
			}
		case "seq":
			add(seqOID(name), clsClass, 0, text)
		case "index":
			if t := c.tableNamed(strings.TrimSuffix(name, "_pkey")); t != nil && strings.HasSuffix(name, "_pkey") && t.HasPK() {
				add(indexOID(t), clsClass, 0, text)
			} else {
				add(secIndexOID(name), clsClass, 0, text)
			}
		case "type":
			add(int64(userTypeOID(name)), clsType, 0, text)
		case "schema":
			if name == "public" {
				add(nsPublic, clsNS, 0, text)
			} else {
				add(schemaOID(name), clsNS, 0, text)
			}
		case "ext":
			add(extensionOID(name), clsExt, 0, text)
		}
	}
	return rows
}

func (c *catSnap) viewNamed(name string) bool {
	for _, v := range c.views {
		if v.Name == name {
			return true
		}
	}
	return false
}

func extData(data string) objData {
	var d objData
	_ = json.Unmarshal([]byte(data), &d)
	return d
}

// objDescription is obj_description(oid[, catalog]) and col_description(oid,
// n).
func (s *Session) objDescription(fn string, args []Datum) Datum {
	if fn == "shobj_description" || len(args) == 0 || args[0].IsNull() {
		return Null
	}
	oid, err := coerce(args[0], KInt)
	if err != nil {
		return Null
	}
	sub := int64(0)
	cls := int64(0)
	switch {
	case fn == "col_description" && len(args) > 1:
		n, _ := coerce(args[1], KInt)
		sub, cls = n.I, pgClassOID
	case len(args) > 1 && !args[1].IsNull():
		cls = catalogRels[strings.TrimPrefix(args[1].String(), "pg_catalog.")]
	}
	for _, r := range s.catSnap().descriptions() {
		if r[0].I == oid.I && r[2].I == sub && (cls == 0 || r[1].I == cls) {
			return r[3]
		}
	}
	return Null
}

// domainConDef is pg_get_constraintdef of a domain CHECK.
func (s *Session) domainConDef(oid int64) (string, bool) {
	loadUserTypes(s.E)
	for _, o := range s.E.Objs("domain") {
		if ut := lookupUserType(o.Name); ut != nil {
			for _, ck := range ut.Checks {
				if domConOID(ut.Name, ck.Name) == oid {
					return "CHECK (" + ck.Expr + ")", true
				}
			}
		}
	}
	return "", false
}

// currentSchema is current_schema(): the first schema of the search path.
func (c *evalCtx) currentSchema() string {
	if s := c.session(); s != nil {
		if p := s.pathList(); len(p) > 0 {
			return p[0]
		}
	}
	return "public"
}

// currentSchemas is current_schemas(includeImplicit).
func (s *Session) currentSchemas(implicit bool) Datum {
	var out []Datum
	if implicit {
		out = append(out, TextD("pg_catalog"))
	}
	for _, p := range s.pathList() {
		out = append(out, TextD(p))
	}
	return arrayD(out)
}

// visible is pg_table_is_visible / pg_type_is_visible: the object's schema
// is on the search path.
func (s *Session) visible(args []Datum) bool {
	if len(args) == 0 || args[0].IsNull() {
		return true
	}
	oid, err := coerce(args[0], KInt)
	if err != nil || !s.customPath() {
		return true
	}
	onPath := map[string]bool{"pg_catalog": true}
	for _, p := range s.pathList() {
		onPath[p] = true
	}
	c := s.catSnap()
	ns := c.nsInfo()
	if ut := userTypeByOID(uint32(oid.I)); ut != nil {
		return onPath[schemaOfName(ut.Name)]
	}
	for _, t := range c.tables {
		if tableOID(t) == oid.I || indexOID(t) == oid.I {
			_, sch, _ := ns.split(t.Name)
			return onPath[sch]
		}
	}
	for _, q := range c.seqs {
		if seqOID(q.Name) == oid.I {
			_, sch, _ := ns.split(q.Name)
			return onPath[sch]
		}
	}
	for _, v := range c.views {
		if viewOID(v.Name) == oid.I {
			_, sch, _ := ns.split(v.Name)
			return onPath[sch]
		}
	}
	return true
}
