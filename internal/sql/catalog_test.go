package sql

import (
	"strings"
	"testing"
)

// catalogFixture creates the tables the catalog tests read: a table with a
// serial key, defaults, a CHECK, a dropped and added columns; a columnar
// table; an identity table; a free sequence.
func catalogFixture(t *testing.T) *Session {
	t.Helper()
	s := NewSession(newEngine(t))
	for _, q := range []string{
		`CREATE TABLE items (id serial PRIMARY KEY, name text NOT NULL DEFAULT 'x', qty bigint DEFAULT 0 CHECK (qty >= 0), created timestamptz DEFAULT now(), tmp varchar(20), price double precision)`,
		`INSERT INTO items (name, qty) VALUES ('a', 1), ('b', 2)`,
		`ALTER TABLE items DROP COLUMN tmp`,
		`ALTER TABLE items ADD COLUMN extra integer`,
		`ALTER TABLE items ADD COLUMN note text DEFAULT 'n'`,
		`CREATE TABLE ev (id bigint PRIMARY KEY, ts timestamptz NOT NULL, v double precision, ok boolean) WITH (columnar = true)`,
		`CREATE TABLE ident (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, label text)`,
		`CREATE SEQUENCE s1 START 10 INCREMENT BY 5`,
	} {
		run(t, s, q)
	}
	return s
}

func oidOf(t *testing.T, s *Session, rel string) string {
	t.Helper()
	r := run(t, s, `SELECT '`+rel+`'::regclass::oid`)
	return r.Rows[0][0].String()
}

// rows runs q and renders its rows, booleans as true and false.
func rows(t *testing.T, s *Session, q string) string {
	t.Helper()
	var out []string
	for _, r := range run(t, s, q).Rows {
		var vs []string
		for _, v := range r {
			switch {
			case v.IsNull():
				vs = append(vs, "NULL")
			case v.K == KBool:
				vs = append(vs, map[bool]string{true: "true", false: "false"}[v.Bool()])
			default:
				vs = append(vs, v.String())
			}
		}
		out = append(out, strings.Join(vs, ","))
	}
	return strings.Join(out, ";")
}

func expectRows(t *testing.T, s *Session, q, want string) {
	t.Helper()
	if got := rows(t, s, q); got != want {
		t.Errorf("%s\n got: %s\nwant: %s", q, got, want)
	}
}

// TestCatalogClientQueries runs every query psql and the JDBC driver send
// (catalog_queries_test.go): none may fail.
func TestCatalogClientQueries(t *testing.T) {
	s := catalogFixture(t)
	for i, q := range catalogClientQueries(oidOf(t, s, "items"), oidOf(t, s, "items_id_seq")) {
		if _, err := tryRun(s, q); err != nil {
			t.Errorf("[%d] %v\n  %.200s", i, err, q)
		}
	}
}

// TestCatalogPsql checks what psql prints for \dt, \d, \di, \ds, \dn and
// \d items.
func TestCatalogPsql(t *testing.T) {
	s := catalogFixture(t)
	tbl, seq := oidOf(t, s, "items"), oidOf(t, s, "items_id_seq")
	qs := catalogClientQueries(tbl, seq)
	expectRows(t, s, qs[0], "public,ev,table,stask;public,ident,table,stask;public,items,table,stask") // \dt
	expectRows(t, s, qs[1], "public,ev,table,stask;public,ident,table,stask;public,ident_id_seq,sequence,stask;"+
		"public,items,table,stask;public,items_id_seq,sequence,stask;public,s1,sequence,stask") // \d
	expectRows(t, s, qs[2], "public,ev_pkey,index,stask,ev;public,ident_pkey,index,stask,ident;public,items_pkey,index,stask,items") // \di
	expectRows(t, s, qs[3], "public,ident_id_seq,sequence,stask;public,items_id_seq,sequence,stask;public,s1,sequence,stask")        // \ds
	expectRows(t, s, qs[4], "public,stask")                                                                                          // \dn
	// \d items
	expectRows(t, s, qs[5], tbl+",public,items")
	expectRows(t, s, qs[6], "1,r,true,false,false,false,false,false,false,,0,,p,d,heap")
	expectRows(t, s, qs[7], "id,bigint,nextval('items_id_seq'::regclass),true,NULL,,;name,text,'x',true,NULL,,;qty,bigint,0,false,NULL,,;"+
		"created,timestamp with time zone,now(),false,NULL,,;price,double precision,NULL,false,NULL,,;extra,bigint,NULL,false,NULL,,;"+
		"note,text,'n',false,NULL,,")
	expectRows(t, s, qs[8], "items_pkey,true,true,false,true,CREATE UNIQUE INDEX items_pkey ON public.items USING btree (id),PRIMARY KEY (id),p,false,false,false,0")
	expectRows(t, s, qs[9], "items_qty_check,CHECK (qty >= 0)")
	for _, i := range []int{10, 11, 12, 13, 14, 15, 16} { // foreign keys, statistics, publications, triggers, inheritance
		expectRows(t, s, qs[i], "")
	}
	// \d items_id_seq
	expectRows(t, s, qs[17], "integer,1,1,2147483647,1,no,1")
	expectRows(t, s, qs[18], "public.items.id,a")
	// \l, \dT, \du
	expectRows(t, s, qs[19], "stask,stask,UTF8,C.UTF-8,C.UTF-8,NULL,libc,NULL")
	if r := run(t, s, qs[20]); len(r.Rows) == 0 || r.Rows[0][1].IsNull() {
		t.Errorf("\\dT: %s", render(r))
	}
	expectRows(t, s, qs[21], "stask,true,true,true,true,true,-1,NULL,{},NULL,true,true")
	// the other tables' sequence owners: identity is 'i'
	expectRows(t, s, strings.ReplaceAll(qs[18], "'"+seq+"'", "'"+oidOf(t, s, "ident_id_seq")+"'"), "public.ident.id,i")
	// \d ident: identity column shows no default
	expectRows(t, s, strings.ReplaceAll(qs[7], "'"+tbl+"'", "'"+oidOf(t, s, "ident")+"'"),
		"id,bigint,NULL,true,NULL,a,;label,text,NULL,false,NULL,,")
	// \d ev (columnar): the key is NOT NULL, ts too
	expectRows(t, s, strings.ReplaceAll(qs[7], "'"+tbl+"'", "'"+oidOf(t, s, "ev")+"'"),
		"id,bigint,NULL,true,NULL,,;ts,timestamp with time zone,NULL,true,NULL,,;v,double precision,NULL,false,NULL,,;ok,boolean,NULL,false,NULL,,")
	// \d with a schema-qualified pattern, \dt it*
	expectRows(t, s, qs[48], tbl+",public,items")
	expectRows(t, s, qs[49], "public,items,table,stask")
}

// TestCatalogJDBC checks DatabaseMetaData: getTables, getColumns,
// getPrimaryKeys, getIndexInfo, getSchemas, getCatalogs.
func TestCatalogJDBC(t *testing.T) {
	s := catalogFixture(t)
	qs := catalogClientQueries(oidOf(t, s, "items"), oidOf(t, s, "items_id_seq"))
	expectRows(t, s, qs[22], "NULL,public,ident_id_seq,SEQUENCE,NULL,,,,,;NULL,public,items_id_seq,SEQUENCE,NULL,,,,,;"+
		"NULL,public,s1,SEQUENCE,NULL,,,,,;NULL,public,ev,TABLE,NULL,,,,,;NULL,public,ident,TABLE,NULL,,,,,;NULL,public,items,TABLE,NULL,,,,,")
	// getColumns: the dropped column is skipped and attnum is the row number
	expectRows(t, s, qs[23], "public,items,id,20,true,-1,8,-1,1,NULL,NULL,nextval('items_id_seq'::regclass),NULL,0,b;"+
		"public,items,name,25,true,-1,-1,-1,2,NULL,NULL,'x',NULL,0,b;"+
		"public,items,qty,20,false,-1,8,-1,3,NULL,NULL,0,NULL,0,b;"+
		"public,items,created,1184,false,-1,8,-1,4,NULL,NULL,now(),NULL,0,b;"+
		"public,items,price,701,false,-1,8,-1,5,NULL,NULL,NULL,NULL,0,b;"+
		"public,items,extra,20,false,-1,8,-1,6,NULL,NULL,NULL,NULL,0,b;"+
		"public,items,note,25,false,-1,-1,-1,7,NULL,NULL,'n',NULL,0,b")
	expectRows(t, s, qs[24], "NULL,public,items,id,1,items_pkey")
	expectRows(t, s, qs[25], "NULL,public,items,false,NULL,items_pkey,3,1,id,A,-1,0,NULL")
	expectRows(t, s, qs[26], "information_schema,NULL;pg_catalog,NULL;public,NULL")
	expectRows(t, s, qs[27], "stask")
	expectRows(t, s, qs[29], "true,pg_catalog,text")
	expectRows(t, s, qs[30], "1007,_int4")
	// getPrimaryKeys of a table named in a LIKE pattern or another table
	expectRows(t, s, strings.ReplaceAll(qs[24], "E'items'", "E'ev'"), "NULL,public,ev,id,1,ev_pkey")
	expectRows(t, s, qs[33], "") // getImportedKeys: no foreign keys
}

// TestCatalogDBeaver runs the queries DBeaver's schema tree sends.
func TestCatalogDBeaver(t *testing.T) {
	s := catalogFixture(t)
	qs := catalogClientQueries(oidOf(t, s, "items"), oidOf(t, s, "items_id_seq"))
	if r := run(t, s, qs[39]); len(r.Rows) != 6 { // tables and sequences of public, not indexes
		t.Errorf("pg_class of public: %s", render(r))
	}
	// attributes of items: dropped ones are filtered by DBeaver, and the
	// serial column's sequence is found through pg_depend
	r := run(t, s, qs[40])
	if len(r.Rows) != 7 {
		t.Fatalf("attributes: %s", render(r))
	}
	if got := rows(t, s, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid = '`+oidOf(t, s, "items")+`' AND a.attnum > 0 AND a.attisdropped`); got != "........pg.dropped.5........" {
		t.Errorf("dropped attribute: %q", got)
	}
	r = run(t, s, qs[41]) // constraints: consrc_copy is the CHECK expression
	if len(r.Rows) != 4 || r.Rows[0][len(r.Rows[0])-1].String() != "(qty >= 0)" || !r.Rows[1][len(r.Rows[1])-1].IsNull() {
		t.Errorf("constraints: %s", render(r))
	}
}

// TestCatalogRelations checks the pg_catalog relations row by row.
func TestCatalogRelations(t *testing.T) {
	s := catalogFixture(t)
	tbl := oidOf(t, s, "items")
	expectRows(t, s, `SELECT oid, nspname FROM pg_namespace ORDER BY oid`, "11,pg_catalog;2200,public;13000,information_schema")
	expectRows(t, s, `SELECT 'pg_catalog.pg_class'::regclass::oid, 'pg_namespace'::regclass::oid, to_regclass('nope') IS NULL`, "1259,2615,true")

	// pg_class: kinds, natts counts the dropped slot, options of the columnar table
	expectRows(t, s, `SELECT relkind, count(*) FROM pg_class GROUP BY relkind ORDER BY relkind`, "S,3;i,3;r,3")
	expectRows(t, s, `SELECT relname, relkind, relnatts, relchecks, relhasindex, relam, relnamespace, relpersistence, reloptions, relreplident
		FROM pg_class WHERE relname IN ('items','ev','items_pkey','s1') ORDER BY relname`,
		"ev,r,4,0,true,2,2200,p,{columnar=true},d;items,r,8,1,true,2,2200,p,NULL,d;items_pkey,i,1,0,false,403,2200,p,NULL,n;s1,S,3,0,false,0,2200,p,NULL,n")
	expectRows(t, s, `SELECT c.oid = `+tbl+`, c.relkind FROM pg_class c WHERE c.oid = 'items'::regclass`, "true,r")

	// pg_attribute: dropped slot between created and price
	expectRows(t, s, `SELECT attnum, attname, atttypid, attnotnull, atthasdef, atthasmissing, attidentity, attisdropped, attlen
		FROM pg_attribute WHERE attrelid = 'items'::regclass AND attnum > 0 ORDER BY attnum`,
		"1,id,20,true,true,false,,false,8;2,name,25,true,true,false,,false,-1;3,qty,20,false,true,false,,false,8;"+
			"4,created,1184,false,true,false,,false,8;5,........pg.dropped.5........,0,false,false,false,,true,-1;"+
			"6,price,701,false,false,false,,false,8;7,extra,20,false,false,false,,false,8;8,note,25,false,true,true,,false,-1")
	expectRows(t, s, `SELECT attname, attidentity, atthasdef FROM pg_attribute WHERE attrelid = 'ident'::regclass AND attnum > 0 ORDER BY attnum`,
		"id,a,false;label,,false")
	expectRows(t, s, `SELECT attname, atttypid FROM pg_attribute WHERE attrelid = 'items_pkey'::regclass`, "id,20")
	expectRows(t, s, `SELECT attname, atttypid, attnum FROM pg_attribute WHERE attrelid = 's1'::regclass ORDER BY attnum`,
		"last_value,20,1;log_cnt,20,2;is_called,16,3")
	expectRows(t, s, `SELECT count(*) FROM pg_attribute WHERE attrelid = 'ev'::regclass AND attnum > 0 AND NOT attisdropped`, "4")

	// pg_type
	expectRows(t, s, `SELECT oid, typname, typlen, typcategory, typelem, typarray, typnamespace FROM pg_type WHERE typname IN ('int8','text','_int4','timestamptz') ORDER BY oid`,
		"20,int8,8,N,0,1016,11;25,text,-1,S,0,1009,11;1007,_int4,-1,A,23,0,11;1184,timestamptz,8,D,0,1185,11")
	expectRows(t, s, `SELECT typname FROM pg_type WHERE oid IN (16, 701)`, "bool;float8")

	// pg_index, pg_constraint, pg_attrdef
	expectRows(t, s, `SELECT indrelid = 'items'::regclass, indexrelid = 'items_pkey'::regclass, indnatts, indisunique, indisprimary, indisvalid, indkey, indoption
		FROM pg_index WHERE indrelid = 'items'::regclass`, "true,true,1,true,true,true,1,0")
	expectRows(t, s, `SELECT conname, contype, conkey, conindid <> 0, condeferrable, convalidated, conbin FROM pg_constraint WHERE conrelid = 'items'::regclass ORDER BY contype`,
		"items_qty_check,c,{3},false,false,true,qty >= 0;items_pkey,p,{1},true,false,true,NULL")
	expectRows(t, s, `SELECT adnum, adbin FROM pg_attrdef WHERE adrelid = 'items'::regclass ORDER BY adnum`,
		"1,nextval('items_id_seq'::regclass);2,'x';3,0;4,now();8,'n'")
	expectRows(t, s, `SELECT count(*) FROM pg_attrdef WHERE adrelid = 'ident'::regclass`, "0") // identity has no default row
	expectRows(t, s, `SELECT seqtypid, seqstart, seqincrement, seqmax, seqmin, seqcycle FROM pg_sequence WHERE seqrelid = 's1'::regclass`,
		"20,10,5,9223372036854775807,1,false")
	expectRows(t, s, `SELECT count(*) FROM pg_description`, "0")
	expectRows(t, s, `SELECT amname FROM pg_am ORDER BY oid`, "heap;btree")
	expectRows(t, s, `SELECT d.refobjsubid, d.deptype FROM pg_depend d WHERE d.objid = 'items_id_seq'::regclass`, "1,a")
	expectRows(t, s, `SELECT tablename, hasindexes FROM pg_tables ORDER BY tablename`, "ev,true;ident,true;items,true")
	expectRows(t, s, `SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'ev'`,
		"ev_pkey,CREATE UNIQUE INDEX ev_pkey ON public.ev USING btree (id)")
	expectRows(t, s, `SELECT datname, pg_encoding_to_char(encoding) FROM pg_database`, "stask,UTF8")
	expectRows(t, s, `SELECT count(*) FROM pg_settings WHERE name = 'max_index_keys'`, "1")

	// the catalog follows DDL: drop, create, alter, truncate (which keeps the OID)
	run(t, s, `CREATE TABLE tmp1 (a bigint PRIMARY KEY, b text)`)
	oidBefore := oidOf(t, s, "tmp1")
	run(t, s, `ALTER TABLE tmp1 ADD COLUMN c bigint`)
	run(t, s, `ALTER TABLE tmp1 DROP COLUMN b`)
	expectRows(t, s, `SELECT attnum, attname, attisdropped FROM pg_attribute WHERE attrelid = 'tmp1'::regclass AND attnum > 0 ORDER BY attnum`,
		"1,a,false;2,........pg.dropped.2........,true;3,c,false")
	run(t, s, `TRUNCATE tmp1`)
	if oidOf(t, s, "tmp1") != oidBefore {
		t.Errorf("TRUNCATE changed the OID of tmp1")
	}
	run(t, s, `DROP TABLE tmp1`)
	expectRows(t, s, `SELECT count(*) FROM pg_class WHERE relname LIKE 'tmp1%'`, "0")
	expectRows(t, s, `SELECT count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = 'tmp1'`, "0")
	if _, err := tryRun(s, `SELECT 'tmp1'::regclass`); code(err) != codeUndefinedTable {
		t.Errorf("regclass of a dropped table: %v", err)
	}
	run(t, s, `DROP SEQUENCE s1`)
	expectRows(t, s, `SELECT count(*) FROM pg_class WHERE relkind = 'S'`, "2")
}

// TestCatalogInformationSchema checks the information_schema views.
func TestCatalogInformationSchema(t *testing.T) {
	s := catalogFixture(t)
	expectRows(t, s, `SELECT table_catalog, table_schema, table_name, table_type, is_insertable_into FROM information_schema.tables ORDER BY table_name`,
		"stask,public,ev,BASE TABLE,YES;stask,public,ident,BASE TABLE,YES;stask,public,items,BASE TABLE,YES")
	expectRows(t, s, `SELECT column_name, ordinal_position, column_default, is_nullable, data_type, udt_name, numeric_precision, is_identity, identity_generation
		FROM information_schema.columns WHERE table_name = 'items' ORDER BY ordinal_position`,
		"id,1,nextval('items_id_seq'::regclass),NO,bigint,int8,64,NO,NULL;"+
			"name,2,'x',NO,text,text,NULL,NO,NULL;"+
			"qty,3,0,YES,bigint,int8,64,NO,NULL;"+
			"created,4,now(),YES,timestamp with time zone,timestamptz,NULL,NO,NULL;"+
			"price,6,NULL,YES,double precision,float8,53,NO,NULL;"+
			"extra,7,NULL,YES,bigint,int8,64,NO,NULL;"+
			"note,8,'n',YES,text,text,NULL,NO,NULL")
	expectRows(t, s, `SELECT column_name, column_default, is_identity, identity_generation, identity_start, identity_increment
		FROM information_schema.columns WHERE table_name = 'ident' ORDER BY ordinal_position`,
		"id,NULL,YES,ALWAYS,1,1;label,NULL,NO,NULL,NULL,NULL")
	expectRows(t, s, `SELECT column_name, data_type, is_nullable FROM information_schema.columns WHERE table_name = 'ev' ORDER BY ordinal_position`,
		"id,bigint,NO;ts,timestamp with time zone,NO;v,double precision,YES;ok,boolean,YES")
	expectRows(t, s, `SELECT constraint_name, table_name, constraint_type FROM information_schema.table_constraints WHERE table_name = 'items' ORDER BY constraint_type DESC`,
		"items_pkey,items,PRIMARY KEY;items_qty_check,items,CHECK")
	expectRows(t, s, `SELECT constraint_name, table_name, column_name, ordinal_position FROM information_schema.key_column_usage ORDER BY table_name`,
		"ev_pkey,ev,id,1;ident_pkey,ident,id,1;items_pkey,items,id,1")
	expectRows(t, s, `SELECT constraint_name, check_clause FROM information_schema.check_constraints`, "items_qty_check,qty >= 0")
	expectRows(t, s, `SELECT table_name, column_name, constraint_name FROM information_schema.constraint_column_usage WHERE table_name = 'items' ORDER BY constraint_name`,
		"items,id,items_pkey;items,qty,items_qty_check")
	expectRows(t, s, `SELECT schema_name, schema_owner FROM information_schema.schemata ORDER BY schema_name`,
		"information_schema,stask;pg_catalog,stask;public,stask")
	expectRows(t, s, `SELECT sequence_name, data_type, start_value, increment FROM information_schema.sequences ORDER BY sequence_name`,
		"ident_id_seq,bigint,1,1;items_id_seq,integer,1,1;s1,bigint,10,5")
	expectRows(t, s, `SELECT count(*) FROM information_schema.table_privileges WHERE table_name = 'items'`, "7")
	// the join Liquibase, Hibernate and ORMs use for primary keys
	expectRows(t, s, `SELECT kcu.column_name FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu ON tc.constraint_name = kcu.constraint_name AND tc.table_name = kcu.table_name
		WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_name = 'ev'`, "id")
}

// TestCatalogFunctions checks the system information functions, the casts
// to the OID types and the spellings catalog queries use.
func TestCatalogFunctions(t *testing.T) {
	s := catalogFixture(t)
	tbl := oidOf(t, s, "items")
	for _, c := range []struct{ q, want string }{
		{`SELECT current_schema()`, "public"},
		{`SELECT current_schemas(false)`, "{public}"},
		{`SELECT current_schemas(true)`, "{pg_catalog,public}"},
		{`SELECT current_database(), current_catalog, current_user, session_user, pg_catalog.current_user()`, "stask,stask,stask,stask,stask"},
		{`SELECT version() LIKE 'PostgreSQL 16%'`, "true"},
		{`SELECT current_setting('server_version_num'), current_setting('server_version')`, "160000,16.0"},
		{`SELECT current_setting('nope', true) IS NULL, current_setting('max_identifier_length')`, "true,63"},
		{`SELECT format_type(20, NULL), format_type(25, -1), format_type(1184, NULL), format_type(1007, NULL), format_type(0, NULL), format_type(701, NULL)`,
			"bigint,text,timestamp with time zone,integer[],-,double precision"},
		{`SELECT format_type(NULL, NULL) IS NULL`, "true"},
		{`SELECT pg_table_is_visible('items'::regclass), pg_type_is_visible(25)`, "true,true"},
		{`SELECT pg_get_expr('nextval(''s''::regclass)', 0), pg_get_expr('x', 0, true)`, "nextval('s'::regclass),x"},
		{`SELECT pg_get_userbyid(10), pg_encoding_to_char(6)`, "stask,UTF8"},
		{`SELECT obj_description(` + tbl + `, 'pg_class') IS NULL, col_description(` + tbl + `, 1) IS NULL`, "true,true"},
		{`SELECT 'items'::regclass::oid = ` + tbl + `, 'items'::regclass = ` + tbl, "true,true"},
		{`SELECT '` + tbl + `'::regclass::oid, 'public.items'::regclass::oid = ` + tbl + `, '"items"'::regclass::oid = ` + tbl, tbl + ",true,true"},
		{`SELECT 'pg_class'::regclass::oid, 'pg_catalog.pg_attribute'::regclass::oid, 'pg_type'::regclass::oid`, "1259,1249,1247"},
		{`SELECT to_regclass('items') IS NOT NULL, to_regclass('nope') IS NULL, to_regclass('pg_class')`, "true,true,1259"},
		{`SELECT to_regtype('int8'), to_regtype('bigint'), to_regtype('nope') IS NULL, 'text'::regtype::oid`, "20,20,true,25"},
		{`SELECT 'public'::regnamespace::oid, 'pg_catalog'::regnamespace::oid`, "2200,11"},
		{`SELECT pg_get_indexdef('items_pkey'::regclass), pg_get_indexdef('items_pkey'::regclass, 1, true), pg_get_indexdef('items_pkey'::regclass, 0, true)`,
			"CREATE UNIQUE INDEX items_pkey ON public.items USING btree (id),id,CREATE UNIQUE INDEX items_pkey ON public.items USING btree (id)"},
		{`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'items_pkey'`, "PRIMARY KEY (id)"},
		{`SELECT pg_get_constraintdef(oid, true) FROM pg_constraint WHERE conname = 'items_qty_check'`, "CHECK (qty >= 0)"},
		{`SELECT pg_get_serial_sequence('items', 'id'), pg_get_serial_sequence('items', 'name') IS NULL, pg_get_serial_sequence('public.ident', 'id')`,
			"public.items_id_seq,true,public.ident_id_seq"},
		{`SELECT quote_ident('items'), quote_ident('Mixed'), quote_ident('a b'), quote_ident('select')`, `items,"Mixed","a b","select"`},
		{`SELECT has_table_privilege('items', 'SELECT'), has_schema_privilege('public', 'USAGE'), has_database_privilege('stask', 'CONNECT')`, "true,true,true"},
		{`SELECT pg_relation_size('items'::regclass), pg_total_relation_size('items'::regclass)`, "0,0"},
		{`SELECT pg_backend_pid() > 0, pg_is_in_recovery()`, "true,false"},
		// spellings psql and JDBC use
		{`SELECT 'items' OPERATOR(pg_catalog.~) '^(it)'`, "true"},
		{`SELECT relname FROM pg_class WHERE relname OPERATOR(pg_catalog.~) '^(ev)$' COLLATE pg_catalog.default`, "ev"},
		{`SELECT relname FROM pg_class WHERE relname = 'ev' COLLATE "C"`, "ev"},
		{`SELECT relname::pg_catalog.text, relkind::"char" FROM pg_class WHERE relname = 'ev'`, "ev,r"},
		{`SELECT '{1}'::pg_catalog.int2[]`, "{1}"},
		{`SELECT (information_schema._pg_expandarray(indkey)).n, (information_schema._pg_expandarray(indkey)).x FROM pg_index WHERE indrelid = 'ev'::regclass`, "1,1"},
		{`SELECT 3 = ANY(ARRAY[1,2,3]), 4 = ANY(ARRAY[1,2,3]), 4 <> ALL(ARRAY[1,2,3]), 2 <> ALL(ARRAY[1,2,3]), 'b' = ANY('{a,b}')`, "true,false,true,false,true"},
		{`SELECT relname FROM pg_class WHERE relkind = ANY (ARRAY['r','p']) AND relname = ANY (ARRAY['ev','nope']) `, "ev"},
		{`SELECT n.nspname = ANY(current_schemas(true)) FROM pg_namespace n WHERE n.oid = 2200`, "true"},
		{`SELECT 7 & 3, 6 & 1`, "3,0"},
		{`SELECT (ARRAY[10,20,30])[2], array_upper(ARRAY[10,20,30], 1), array_lower(ARRAY[10,20,30], 1)`, "20,3,1"},
		{`SELECT i.indoption[0], i.indkey[0] FROM pg_index i WHERE indrelid = 'ev'::regclass`, "0,1"},
		{`SELECT ARRAY(SELECT relname FROM pg_class WHERE relkind = 'r' ORDER BY relname)`, "{ev,ident,items}"},
		{`SELECT "substring"('abcdef', 3)`, "cdef"},
		{`SELECT trim(both '"' from '"id"')`, "id"},
		{`SELECT n FROM pg_catalog.generate_series(1, 3) pos(n)`, "1;2;3"},
		{`SELECT count(*) FROM pg_class c, pg_namespace n WHERE c.relnamespace = n.oid AND n.nspname = 'public'`, "9"},
	} {
		expectRows(t, s, c.q, c.want)
	}
	for _, c := range []struct{ q, code string }{
		{`SELECT 'nope'::regclass`, codeUndefinedTable},
		{`SELECT current_setting('nope')`, codeUndefinedObject},
		{`SELECT 'nope'::regtype`, codeUndefinedObject},
	} {
		if _, err := tryRun(s, c.q); code(err) != c.code {
			t.Errorf("%s: got %v, want %s", c.q, err, c.code)
		}
	}
}

// TestCatalogPrepared runs catalog queries with parameters, as drivers do.
func TestCatalogPrepared(t *testing.T) {
	s := catalogFixture(t)
	r := run(t, s, `SELECT a.attname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		WHERE c.relname = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, TextD("ev"))
	if got := render(r); got != "id;ts;v;ok" {
		t.Errorf("attributes by parameter: %s", got)
	}
	r = run(t, s, `SELECT c.relname FROM pg_class c WHERE c.relnamespace = $1 AND c.relkind = ANY ($2)`, IntD(2200), TextD("{r}"))
	if got := render(r); got != "ev;ident;items" {
		t.Errorf("ANY with an array parameter: %s", got)
	}
}
