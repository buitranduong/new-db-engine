package sql

import (
	"fmt"
	"sort"

	"stask/internal/schema"
)

// The pg_catalog relations (https://www.postgresql.org/docs/16/catalogs.html):
// the columns the target clients read, in PostgreSQL's order.

func init() {
	defCat("pg_namespace", "oid:i nspname:t nspowner:i nspacl:a", func(c *catSnap) [][]Datum {
		return [][]Datum{
			{IntD(nsPgCatalog), TextD("pg_catalog"), IntD(ownerOID), Null},
			{IntD(nsPublic), TextD("public"), IntD(ownerOID), Null},
			{IntD(nsInfoSchema), TextD("information_schema"), IntD(ownerOID), Null},
		}
	})

	defCat("pg_class", "oid:i relname:t relnamespace:i reltype:i reloftype:i relowner:i relam:i relfilenode:i "+
		"reltablespace:i relpages:i reltuples:f relallvisible:i reltoastrelid:i relhasindex:b relisshared:b "+
		"relpersistence:t relkind:t relnatts:i relchecks:i relhasrules:b relhastriggers:b relhassubclass:b "+
		"relrowsecurity:b relforcerowsecurity:b relispopulated:b relreplident:t relispartition:b relrewrite:i "+
		"relfrozenxid:i relminmxid:i relacl:a reloptions:a relpartbound:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		rel := func(oid int64, name string, am int64, kind string, natts, checks int, hasIndex bool, ident string, opts Datum) {
			rows = append(rows, []Datum{IntD(oid), TextD(name), IntD(nsPublic), IntD(0), IntD(0), IntD(ownerOID),
				IntD(am), IntD(oid), IntD(0), IntD(0), FloatD(-1), IntD(0), IntD(0), BoolD(hasIndex), BoolD(false),
				TextD("p"), TextD(kind), IntD(int64(natts)), IntD(int64(checks)), BoolD(false), BoolD(kind == "r" && c.s.E.Triggers(name) != nil), BoolD(false),
				BoolD(false), BoolD(false), BoolD(true), TextD(ident), BoolD(false), IntD(0),
				IntD(0), IntD(0), Null, opts, Null})
		}
		for _, t := range c.tables {
			ident := "d"
			if c.s.E.ReplicaIdentityFull(t.Name) {
				ident = "f"
			}
			rel(tableOID(t), t.Name, heapAMOID, "r", maxNum(t), len(t.Checks), true, ident, relOptions(t))
			supfixRelStats(c, t, rows[len(rows)-1])
			if t.HasPK() {
				rel(indexOID(t), pkeyName(t), btreeAMOID, "i", len(t.PKCols()), 0, false, "n", Null)
			}
		}
		secIndexRows(c, func(t *schema.Table, ix schema.IndexDef) {
			rel(secIndexOID(ix.Name), ix.Name, btreeAMOID, "i", len(ix.Columns), 0, false, "n", Null)
		})
		for _, q := range c.seqs {
			rel(seqOID(q.Name), q.Name, 0, "S", 3, 0, false, "n", Null)
		}
		for _, v := range c.views {
			rel(viewOID(v.Name), v.Name, 0, "v", len(v.Cols), 0, false, "n", Null)
		}
		return rows
	})

	defCat("pg_attribute", "attrelid:i attname:t atttypid:i attstattarget:i attlen:i attnum:i attndims:i "+
		"attcacheoff:i atttypmod:i attbyval:b attalign:t attstorage:t attcompression:t attnotnull:b atthasdef:b "+
		"atthasmissing:b attidentity:t attgenerated:t attisdropped:b attislocal:b attinhcount:i attcollation:i "+
		"attacl:a attoptions:a attfdwoptions:a attmissingval:a", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		mod := int64(-1) // atttypmod of the column being added
		att := func(rel int64, name string, typ int64, num int, notNull, def bool, ident string, missing, dropped bool) {
			ty := pgTypeByOID[typ]
			coll := int64(0)
			if ty.cat == "S" {
				coll = 100
			}
			if dropped { // PostgreSQL keeps the slot: no type, a placeholder name
				name, typ, notNull, def, ident, coll = fmt.Sprintf("........pg.dropped.%d........", num), 0, false, false, "", 0
			}
			rows = append(rows, []Datum{IntD(rel), TextD(name), IntD(typ), IntD(-1), IntD(ty.len), IntD(int64(num)),
				IntD(0), IntD(-1), IntD(mod), BoolD(ty.len > 0 && ty.len <= 8), TextD("d"), TextD("p"), TextD(""),
				BoolD(notNull), BoolD(def), BoolD(missing), TextD(ident), TextD(""), BoolD(dropped), BoolD(true), IntD(0),
				IntD(coll), Null, Null, Null, Null})
		}
		for _, t := range c.tables {
			// live and dropped columns in attnum order
			type ac struct {
				col     schema.Column
				dropped bool
			}
			var all []ac
			for _, col := range t.Columns {
				all = append(all, ac{col, false})
			}
			for _, col := range t.Dropped {
				all = append(all, ac{col, true})
			}
			sort.SliceStable(all, func(i, j int) bool { return all[i].col.Num < all[j].col.Num })
			for _, a := range all {
				col := a.col
				mod = typmodOf(col)
				att(tableOID(t), col.Name, colTypeOID(col), col.Num, col.NotNull || col.Name == t.Key, hasDef(col),
					identityChar(col), col.Missing != nil, a.dropped)
			}
			mod = -1
			for i, name := range t.PKCols() {
				j, _ := t.Col(name)
				att(indexOID(t), name, colTypeOID(t.Columns[j]), i+1, false, false, "", false, false)
			}
		}
		secIndexRows(c, func(t *schema.Table, ix schema.IndexDef) {
			for i, name := range ix.Columns {
				j, _ := t.Col(name)
				att(secIndexOID(ix.Name), name, colTypeOID(t.Columns[j]), i+1, false, false, "", false, false)
			}
		})
		for _, q := range c.seqs {
			att(seqOID(q.Name), "last_value", 20, 1, true, false, "", false, false)
			att(seqOID(q.Name), "log_cnt", 20, 2, true, false, "", false, false)
			att(seqOID(q.Name), "is_called", 16, 3, true, false, "", false, false)
		}
		for _, v := range c.views { // the output types are not recorded: text
			for i, name := range v.Cols {
				att(viewOID(v.Name), name, 25, i+1, false, false, "", false, false)
			}
		}
		return rows
	})

	defCat("pg_type", "oid:i typname:t typnamespace:i typowner:i typlen:i typbyval:b typtype:t typcategory:t "+
		"typispreferred:b typisdefined:b typdelim:t typrelid:i typsubscript:i typelem:i typarray:i typinput:t "+
		"typoutput:t typreceive:t typsend:t typmodin:t typmodout:t typanalyze:t typalign:t typstorage:t "+
		"typnotnull:b typbasetype:i typtypmod:i typndims:i typcollation:i typdefaultbin:t typdefault:t typacl:a",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, t := range pgTypes {
				coll := int64(0)
				if t.cat == "S" {
					coll = 100
				}
				align := "i"
				if t.len == 8 {
					align = "d"
				}
				rows = append(rows, []Datum{IntD(t.oid), TextD(t.name), IntD(nsPgCatalog), IntD(ownerOID), IntD(t.len),
					BoolD(t.len > 0 && t.len <= 8), TextD("b"), TextD(t.cat), BoolD(false), BoolD(true), TextD(","), IntD(0),
					IntD(0), IntD(t.elem), IntD(t.array), TextD(t.name + "in"), TextD(t.name + "out"), TextD(t.name + "recv"),
					TextD(t.name + "send"), TextD("-"), TextD("-"), TextD("-"), TextD(align), TextD("p"), BoolD(false),
					IntD(0), IntD(-1), IntD(0), IntD(coll), Null, Null, Null})
			}
			return rows
		})

	defCat("pg_index", "indexrelid:i indrelid:i indnatts:i indnkeyatts:i indisunique:b indnullsnotdistinct:b "+
		"indisprimary:b indisexclusion:b indimmediate:b indisclustered:b indisvalid:b indcheckxmin:b indisready:b "+
		"indislive:b indisreplident:b indkey:t indcollation:t indclass:t indoption:t indexprs:t indpred:t",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, t := range c.tables {
				if !t.HasPK() {
					continue
				}
				n := len(t.PKCols())
				zeros := make([]int, n)
				rows = append(rows, []Datum{IntD(indexOID(t)), IntD(tableOID(t)), IntD(int64(n)), IntD(int64(n)), BoolD(true), BoolD(false),
					BoolD(true), BoolD(false), BoolD(true), BoolD(false), BoolD(true), BoolD(false), BoolD(true),
					BoolD(true), BoolD(false), vecText(pkNums(t)...), vecText(zeros...), vecText(zeros...), vecText(zeros...), Null, Null})
			}
			secIndexRows(c, func(t *schema.Table, ix schema.IndexDef) {
				zeros := make([]int, len(ix.Columns))
				rows = append(rows, []Datum{IntD(secIndexOID(ix.Name)), IntD(tableOID(t)), IntD(int64(len(ix.Columns))),
					IntD(int64(len(ix.Columns))), BoolD(ix.Unique), BoolD(false), BoolD(false), BoolD(false), BoolD(true),
					BoolD(false), BoolD(true), BoolD(false), BoolD(true), BoolD(true), BoolD(false),
					vecText(attnums(t, ix.Columns)...), vecText(zeros...), vecText(zeros...), vecText(zeros...), Null, Null})
			})
			return rows
		})

	defCat("pg_constraint", "oid:i conname:t connamespace:i contype:t condeferrable:b condeferred:b convalidated:b "+
		"conrelid:i contypid:i conindid:i conparentid:i confrelid:i confupdtype:t confdeltype:t confmatchtype:t "+
		"conislocal:b coninhcount:i connoinherit:b conkey:a confkey:a conpfeqop:a conppeqop:a conffeqop:a "+
		"confdelsetcols:a conexclop:a conbin:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		con := func(oid int64, name, typ string, rel, ind int64, key Datum, bin Datum) {
			rows = append(rows, []Datum{IntD(oid), TextD(name), IntD(nsPublic), TextD(typ), BoolD(false), BoolD(false),
				BoolD(true), IntD(rel), IntD(0), IntD(ind), IntD(0), IntD(0), TextD(" "), TextD(" "), TextD(" "),
				BoolD(true), IntD(0), BoolD(false), key, Null, Null, Null, Null, Null, Null, bin})
		}
		for _, t := range c.tables {
			if t.HasPK() {
				con(pkeyConOID(t), pkeyName(t), "p", tableOID(t), indexOID(t), i2vec(pkNums(t)...), Null)
			}
			for i, ck := range t.Checks {
				con(checkOID(t, i), ck.Name, "c", tableOID(t), 0, checkKeys(t, ck.Expr), TextD(ck.Expr))
			}
		}
		secConstraintRows(c, &rows)
		return rows
	})

	defCat("pg_attrdef", "oid:i adrelid:i adnum:i adbin:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			for _, col := range t.Columns {
				if hasDef(col) {
					rows = append(rows, []Datum{IntD(attrdefOID(t, col.Num)), IntD(tableOID(t)), IntD(int64(col.Num)),
						TextD(defaultText(col))})
				}
			}
		}
		return rows
	})

	defCat("pg_sequence", "seqrelid:i seqtypid:i seqstart:i seqincrement:i seqmax:i seqmin:i seqcache:i seqcycle:b",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, q := range c.seqs {
				rows = append(rows, []Datum{IntD(seqOID(q.Name)), IntD(seqTypeOID(q.Type)), IntD(q.Start), IntD(q.Inc),
					IntD(q.Max), IntD(q.Min), IntD(1), BoolD(q.Cycle)})
			}
			return rows
		})

	// pg_depend: a SERIAL or IDENTITY column owns its sequence ('a' auto,
	// 'i' internal for identity), the primary-key index belongs to its
	// constraint, which belongs to the table.
	defCat("pg_depend", "classid:i objid:i objsubid:i refclassid:i refobjid:i refobjsubid:i deptype:t",
		func(c *catSnap) [][]Datum {
			var rows [][]Datum
			for _, q := range c.seqs {
				if t, col := c.seqOwner(q); t != nil {
					typ := "a"
					if col.Identity != "" {
						typ = "i"
					}
					rows = append(rows, []Datum{IntD(pgClassOID), IntD(seqOID(q.Name)), IntD(0), IntD(pgClassOID),
						IntD(tableOID(t)), IntD(int64(col.Num)), TextD(typ)})
				}
			}
			for _, t := range c.tables {
				if t.HasPK() {
					rows = append(rows, []Datum{IntD(pgClassOID), IntD(indexOID(t)), IntD(0), IntD(pgConstrOID),
						IntD(pkeyConOID(t)), IntD(0), TextD("i")})
				}
			}
			return rows
		})

	// the relations psql's \d and the drivers join but that hold nothing here
	defCat("pg_auth_members", "roleid:i member:i grantor:i admin_option:b inherit_option:b set_option:b", nil)
	defCat("pg_policy", "oid:i polname:t polrelid:i polcmd:t polpermissive:b polroles:a polqual:t polwithcheck:t", nil)
	defCat("pg_statistic_ext", "oid:i stxrelid:i stxname:t stxnamespace:i stxowner:i stxstattarget:i stxkeys:a "+
		"stxkind:a", nil)
	defCat("pg_publication_namespace", "oid:i pnpubid:i pnnspid:i", nil)
	defCat("pg_rewrite", "oid:i rulename:t ev_class:i ev_type:t ev_enabled:t is_instead:b ev_qual:t ev_action:t", nil)
	defCat("pg_enum", "oid:i enumtypid:i enumsortorder:f enumlabel:t", nil)
	defCat("pg_range", "rngtypid:i rngsubtype:i rngmultitypid:i rngcollation:i rngsubopc:i rngcanonical:t rngsubdiff:t", nil)
	defCat("pg_publication_rel", "oid:i prpubid:i prrelid:i prqual:t prattrs:a", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for i, p := range c.s.E.Publications() {
			if p.AllTables {
				continue
			}
			for _, t := range c.tables {
				if p.Has(t.Name) {
					rows = append(rows, []Datum{IntD(int64(0x50000000) + tableOID(t)), IntD(int64(16384 + i)), IntD(tableOID(t)), Null, Null})
				}
			}
		}
		return rows
	})
	defCat("pg_description", "objoid:i classoid:i objsubid:i description:t", nil)
	defCat("pg_inherits", "inhrelid:i inhparent:i inhseqno:i inhdetachpending:b", nil)
	defCat("pg_trigger", "oid:i tgrelid:i tgparentid:i tgname:t tgfoid:i tgtype:i tgenabled:t tgisinternal:b "+
		"tgconstrrelid:i tgconstrindid:i tgconstraint:i tgdeferrable:b tginitdeferred:b tgnargs:i tgattr:a "+
		"tgargs:t tgqual:t tgoldtable:t tgnewtable:t", pgTriggerRows)
	defCat("pg_proc", "oid:i proname:t pronamespace:i proowner:i prolang:i procost:f prorows:f provariadic:i "+
		"prosupport:t prokind:t prosecdef:b proleakproof:b proisstrict:b proretset:b provolatile:t proparallel:t "+
		"pronargs:i pronargdefaults:i prorettype:i proargtypes:t proallargtypes:a proargmodes:a proargnames:a "+
		"proargdefaults:t protrftypes:a prosrc:t probin:t prosqlbody:t proconfig:a proacl:a", nil)
	defCat("pg_extension", "oid:i extname:t extowner:i extnamespace:i extrelocatable:b extversion:t", nil)

	defCat("pg_am", "oid:i amname:t amhandler:t amtype:t", func(c *catSnap) [][]Datum {
		return [][]Datum{
			{IntD(heapAMOID), TextD("heap"), TextD("heap_tableam_handler"), TextD("t")},
			{IntD(btreeAMOID), TextD("btree"), TextD("bthandler"), TextD("i")},
		}
	})

	defCat("pg_collation", "oid:i collname:t collnamespace:i collowner:i collprovider:t collisdeterministic:b "+
		"collencoding:i collcollate:t collctype:t", func(c *catSnap) [][]Datum {
		return [][]Datum{{IntD(100), TextD("default"), IntD(nsPgCatalog), IntD(ownerOID), TextD("d"), BoolD(true),
			IntD(-1), Null, Null}}
	})

	defCat("pg_tablespace", "oid:i spcname:t spcowner:i spcacl:a spcoptions:a", func(c *catSnap) [][]Datum {
		return [][]Datum{{IntD(1663), TextD("pg_default"), IntD(ownerOID), Null, Null}}
	})

	defCat("pg_database", "oid:i datname:t datdba:i encoding:i datlocprovider:t datistemplate:b datallowconn:b "+
		"datconnlimit:i datfrozenxid:i datminmxid:i dattablespace:i datcollate:t datctype:t daticulocale:t "+
		"daticurules:t datcollversion:t datacl:a", func(c *catSnap) [][]Datum {
		return [][]Datum{{IntD(databaseOID), TextD("stask"), IntD(ownerOID), IntD(6), TextD("c"), BoolD(false), BoolD(true),
			IntD(-1), IntD(0), IntD(0), IntD(1663), TextD("C.UTF-8"), TextD("C.UTF-8"), Null, Null, Null, Null}}
	})

	roleCols := "rolname:t rolsuper:b rolinherit:b rolcreaterole:b rolcreatedb:b rolcanlogin:b rolreplication:b " +
		"rolconnlimit:i rolpassword:t rolvaliduntil:t rolbypassrls:b rolconfig:a oid:i"
	// roles: the bootstrap superuser (the server's shared password) and
	// the stored roles (core/roles.go); no password is ever shown
	roleRow := func(name string, super, login, repl bool, oid int64) []Datum {
		return []Datum{TextD(name), BoolD(super), BoolD(true), BoolD(super), BoolD(super), BoolD(login), BoolD(repl),
			IntD(-1), TextD("********"), Null, BoolD(super), Null, IntD(oid)}
	}
	roleRows := func(c *catSnap) [][]Datum {
		rows := [][]Datum{roleRow(roleName, true, true, true, ownerOID)}
		for i, r := range c.s.E.Roles() {
			rows = append(rows, roleRow(r.Name, r.Super, r.Login, r.Replication, int64(16400+i)))
		}
		return rows
	}
	defCat("pg_roles", roleCols, roleRows)
	defCat("pg_authid", roleCols, roleRows)
	defCat("pg_user", "usename:t usesysid:i usecreatedb:b usesuper:b userepl:b usebypassrls:b passwd:t "+
		"valuntil:t useconfig:a", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, r := range roleRows(c) {
			if r[5].Bool() { // rolcanlogin
				rows = append(rows, []Datum{r[0], r[12], r[4], r[1], r[6], r[10], TextD("********"), Null, Null})
			}
		}
		return rows
	})

	defCat("pg_settings", "name:t setting:t unit:t category:t short_desc:t extra_desc:t context:t vartype:t "+
		"source:t min_val:t max_val:t enumvals:a boot_val:t reset_val:t sourcefile:t sourceline:i "+
		"pending_restart:b", func(c *catSnap) [][]Datum {
		set := map[string]string{"max_index_keys": "32", "max_identifier_length": "63", "block_size": "8192",
			"max_connections": "100000", "lc_collate": "C.UTF-8", "lc_ctype": "C.UTF-8"}
		for k, v := range c.s.vars {
			set[k] = v
		}
		names := make([]string, 0, len(set))
		for k := range set {
			names = append(names, k)
		}
		sort.Strings(names)
		var rows [][]Datum
		for _, n := range names {
			rows = append(rows, []Datum{TextD(n), TextD(set[n]), Null, TextD(""), TextD(""), Null, TextD("user"),
				TextD("string"), TextD("default"), Null, Null, Null, TextD(set[n]), TextD(set[n]), Null, Null, BoolD(false)})
		}
		return rows
	})

	defCat("pg_tables", "schemaname:t tablename:t tableowner:t tablespace:t hasindexes:b hasrules:b "+
		"hastriggers:b rowsecurity:b", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			rows = append(rows, []Datum{TextD("public"), TextD(t.Name), TextD(roleName), Null, BoolD(true), BoolD(false),
				BoolD(c.s.E.Triggers(t.Name) != nil), BoolD(false)})
		}
		return rows
	})
	defCat("pg_indexes", "schemaname:t tablename:t indexname:t tablespace:t indexdef:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, t := range c.tables {
			if t.HasPK() {
				rows = append(rows, []Datum{TextD("public"), TextD(t.Name), TextD(pkeyName(t)), Null, TextD(indexDef(t))})
			}
			for _, ix := range listedIndexes(t) {
				rows = append(rows, []Datum{TextD("public"), TextD(t.Name), TextD(ix.Name), Null, TextD(secIndexDef(t, ix))})
			}
		}
		return rows
	})
	defCat("pg_views", "schemaname:t viewname:t viewowner:t definition:t", func(c *catSnap) [][]Datum {
		var rows [][]Datum
		for _, v := range c.views {
			rows = append(rows, []Datum{TextD("public"), TextD(v.Name), TextD(roleName), TextD(c.s.viewDefText(v.Query, false))})
		}
		return rows
	})
	defCat("pg_matviews", "schemaname:t matviewname:t matviewowner:t tablespace:t hasindexes:b ispopulated:b "+
		"definition:t", nil)
}

// roleName is the one role (owner of everything; pg_sequences says the same).
const roleName = "stask"

// indexDef is pg_get_indexdef of the primary-key index.
func indexDef(t *schema.Table) string {
	return "CREATE UNIQUE INDEX " + quoteIdent(pkeyName(t)) + " ON public." + quoteIdent(t.Name) +
		" USING btree (" + quoteList(t.PKCols()) + ")"
}

// checkKeys are the columns a CHECK expression names (pg_constraint.conkey).
func checkKeys(t *schema.Table, expr string) Datum {
	x, err := parseCheck(expr)
	if err != nil {
		return Null
	}
	seen := map[int]bool{}
	walk(x, func(e Expr) {
		if cr, ok := e.(*ColRef); ok {
			if i, ok := t.Col(cr.Name); ok {
				seen[t.Columns[i].Num] = true
			}
		}
	})
	if len(seen) == 0 {
		return Null
	}
	var nums []int
	for n := range seen {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return i2vec(nums...)
}
