package sql

import "strings"

// catalogClientQueries are the catalog queries psql 16 (\dt, \d, \di, \d
// tbl, \dn, \ds, \l) and pgjdbc 42.7 (DatabaseMetaData) send, as those
// clients spell them. 'TBL' is the OID of items, 'SEQ' that of items_id_seq.
func catalogClientQueries(tbl, seq string) []string {
	qs := []string{
		// 0: psql \dt
		`SELECT n.nspname as "Schema",
  c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' WHEN 's' THEN 'special' WHEN 't' THEN 'TOAST table' WHEN 'f' THEN 'foreign table' WHEN 'p' THEN 'partitioned table' WHEN 'I' THEN 'partitioned index' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner"
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam
WHERE c.relkind IN ('r','p','')
      AND n.nspname <> 'pg_catalog'
      AND n.nspname !~ '^pg_toast'
      AND n.nspname <> 'information_schema'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2;`,
		// 1: psql \d
		`SELECT n.nspname as "Schema",
  c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' WHEN 't' THEN 'TOAST table' WHEN 'f' THEN 'foreign table' WHEN 'p' THEN 'partitioned table' WHEN 'I' THEN 'partitioned index' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner"
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam
WHERE c.relkind IN ('r','p','v','m','S','f','')
      AND n.nspname <> 'pg_catalog'
      AND n.nspname !~ '^pg_toast'
      AND n.nspname <> 'information_schema'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2;`,
		// 2: psql \di
		`SELECT n.nspname as "Schema",
  c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'i' THEN 'index' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner",
  c2.relname as "Table"
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     LEFT JOIN pg_catalog.pg_index i ON i.indexrelid = c.oid
     LEFT JOIN pg_catalog.pg_class c2 ON i.indrelid = c2.oid
WHERE c.relkind IN ('i','I','')
      AND n.nspname <> 'pg_catalog'
      AND n.nspname !~ '^pg_toast'
      AND n.nspname <> 'information_schema'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2;`,
		// 3: psql \ds
		`SELECT n.nspname as "Schema", c.relname as "Name", CASE c.relkind WHEN 'S' THEN 'sequence' END as "Type", pg_catalog.pg_get_userbyid(c.relowner) as "Owner" FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('S','') AND n.nspname <> 'pg_catalog' AND n.nspname !~ '^pg_toast' AND n.nspname <> 'information_schema' AND pg_catalog.pg_table_is_visible(c.oid) ORDER BY 1,2;`,
		// 4: psql \dn
		`SELECT n.nspname AS "Name", pg_catalog.pg_get_userbyid(n.nspowner) AS "Owner" FROM pg_catalog.pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' ORDER BY 1;`,
		// 5: psql \d items: resolve the name
		`SELECT c.oid,
  n.nspname,
  c.relname
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname OPERATOR(pg_catalog.~) '^(items)$' COLLATE pg_catalog.default
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 2, 3;`,
		// 6
		`SELECT c.relchecks, c.relkind, c.relhasindex, c.relhasrules, c.relhastriggers, c.relrowsecurity, c.relforcerowsecurity, false AS relhasoids, c.relispartition, pg_catalog.array_to_string(c.reloptions || array(select 'toast.' || x from pg_catalog.unnest(tc.reloptions) x), ', ')
, c.reltablespace, CASE WHEN c.reloftype = 0 THEN '' ELSE c.reloftype::pg_catalog.regtype::pg_catalog.text END, c.relpersistence, c.relreplident, am.amname
FROM pg_catalog.pg_class c
 LEFT JOIN pg_catalog.pg_class tc ON (c.reltoastrelid = tc.oid)
LEFT JOIN pg_catalog.pg_am am ON (c.relam = am.oid)
WHERE c.oid = 'TBL';`,
		// 7
		`SELECT a.attname,
  pg_catalog.format_type(a.atttypid, a.atttypmod),
  (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid, true)
   FROM pg_catalog.pg_attrdef d
   WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef),
  a.attnotnull,
  (SELECT c.collname FROM pg_catalog.pg_collation c, pg_catalog.pg_type t
   WHERE c.oid = a.attcollation AND t.oid = a.atttypid AND a.attcollation <> t.typcollation) AS attcollation,
  a.attidentity,
  a.attgenerated
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = 'TBL' AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum;`,
		// 8
		`SELECT c2.relname, i.indisprimary, i.indisunique, i.indisclustered, i.indisvalid, pg_catalog.pg_get_indexdef(i.indexrelid, 0, true),
  pg_catalog.pg_get_constraintdef(con.oid, true), contype, condeferrable, condeferred, i.indisreplident, c2.reltablespace
FROM pg_catalog.pg_class c, pg_catalog.pg_class c2, pg_catalog.pg_index i
  LEFT JOIN pg_catalog.pg_constraint con ON (conrelid = i.indrelid AND conindid = i.indexrelid AND contype IN ('p','u','x'))
WHERE c.oid = 'TBL' AND c.oid = i.indrelid AND i.indexrelid = c2.oid
ORDER BY i.indisprimary DESC, c2.relname;`,
		// 9
		`SELECT r.conname, pg_catalog.pg_get_constraintdef(r.oid, true)
FROM pg_catalog.pg_constraint r
WHERE r.conrelid = 'TBL' AND r.contype = 'c'
ORDER BY 1;`,
		// 10
		`SELECT conname, conrelid::pg_catalog.regclass AS ontable,
       pg_catalog.pg_get_constraintdef(oid, true) AS condef
FROM pg_catalog.pg_constraint r
WHERE r.conrelid = 'TBL' AND r.contype = 'f' AND conparentid = 0
ORDER BY conname;`,
		// 11
		`SELECT conname, conrelid::pg_catalog.regclass AS ontable,
       pg_catalog.pg_get_constraintdef(oid, true) AS condef
FROM pg_catalog.pg_constraint c
WHERE confrelid = 'TBL' AND contype = 'f' AND conparentid = 0
ORDER BY conname;`,
		// 12
		`SELECT oid, stxrelid::pg_catalog.regclass, stxnamespace::pg_catalog.regnamespace::pg_catalog.text AS nsp, stxname,
pg_catalog.pg_get_statisticsobjdef_columns(oid) AS columns,
  'd' = any(stxkind) AS ndist_enabled,
  'f' = any(stxkind) AS deps_enabled,
  'm' = any(stxkind) AS mcv_enabled,
stxstattarget
FROM pg_catalog.pg_statistic_ext stat
WHERE stxrelid = 'TBL'
ORDER BY 1;`,
		// 13
		`SELECT pubname
     , NULL
     , NULL
FROM pg_catalog.pg_publication p
     JOIN pg_catalog.pg_publication_namespace pn ON p.oid = pn.pnpubid
     JOIN pg_catalog.pg_class pc ON pc.relnamespace = pn.pnnspid
WHERE pc.oid ='TBL' and pg_catalog.pg_relation_is_publishable('TBL')
UNION
SELECT pubname
     , pg_get_expr(pr.prqual, c.oid)
     , (CASE WHEN pr.prattrs IS NOT NULL THEN
         (SELECT string_agg(attname, ', ')
           FROM pg_catalog.generate_series(0, pg_catalog.array_upper(pr.prattrs::pg_catalog.int2[], 1)) s,
                pg_catalog.pg_attribute
          WHERE attrelid = pr.prrelid AND attnum = prattrs[s])
        ELSE NULL END) FROM pg_catalog.pg_publication p
     JOIN pg_catalog.pg_publication_rel pr ON p.oid = pr.prpubid
     JOIN pg_catalog.pg_class c ON c.oid = pr.prrelid
WHERE pr.prrelid = 'TBL'
UNION
SELECT pubname
     , NULL
     , NULL
FROM pg_catalog.pg_publication p
WHERE p.puballtables AND pg_catalog.pg_relation_is_publishable('TBL')
ORDER BY 1;`,
		// 14
		`SELECT t.tgname, pg_catalog.pg_get_triggerdef(t.oid, true), t.tgenabled, t.tgisinternal
FROM pg_catalog.pg_trigger t
WHERE t.tgrelid = 'TBL' AND (NOT t.tgisinternal OR (t.tgisinternal AND t.tgenabled = 'D')
	OR EXISTS (SELECT 1 FROM pg_catalog.pg_depend WHERE objid = t.oid
		AND refclassid = 'pg_catalog.pg_trigger'::pg_catalog.regclass))
ORDER BY 1;`,
		// 15
		`SELECT c.oid::pg_catalog.regclass FROM pg_catalog.pg_class c, pg_catalog.pg_inherits i WHERE c.oid = i.inhparent AND i.inhrelid = 'TBL' AND c.relkind != 'p' AND c.relkind != 'I' ORDER BY inhseqno;`,
		// 16
		`SELECT c.oid::pg_catalog.regclass, c.relkind, inhdetachpending, pg_catalog.pg_get_expr(c.relpartbound, c.oid) FROM pg_catalog.pg_class c, pg_catalog.pg_inherits i WHERE c.oid = i.inhrelid AND i.inhparent = 'TBL' ORDER BY pg_catalog.pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT', c.oid::pg_catalog.regclass::pg_catalog.text;`,
		// 17: psql \d sequence
		`SELECT pg_catalog.format_type(seqtypid, NULL) AS "Type", seqstart AS "Start", seqmin AS "Minimum", seqmax AS "Maximum", seqincrement AS "Increment", CASE WHEN seqcycle THEN 'yes' ELSE 'no' END AS "Cycles?", seqcache AS "Cache" FROM pg_catalog.pg_sequence WHERE seqrelid = 'SEQ';`,
		// 18
		`SELECT pg_catalog.quote_ident(nspname) || '.' || pg_catalog.quote_ident(relname) || '.' || pg_catalog.quote_ident(attname), d.deptype FROM pg_catalog.pg_class c INNER JOIN pg_catalog.pg_depend d ON c.oid=d.refobjid INNER JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace INNER JOIN pg_catalog.pg_attribute a ON ( a.attrelid=c.oid AND a.attnum=d.refobjsubid) WHERE d.classid='pg_catalog.pg_class'::pg_catalog.regclass AND d.refclassid='pg_catalog.pg_class'::pg_catalog.regclass AND d.objid='SEQ' AND d.deptype IN ('a', 'i');`,
		// 19: psql \l
		`SELECT d.datname as "Name", pg_catalog.pg_get_userbyid(d.datdba) as "Owner", pg_catalog.pg_encoding_to_char(d.encoding) as "Encoding", d.datcollate as "Collate", d.datctype as "Ctype", d.daticulocale as "ICU Locale", CASE d.datlocprovider WHEN 'c' THEN 'libc' WHEN 'i' THEN 'icu' END AS "Locale Provider", pg_catalog.array_to_string(d.datacl, E'\n') AS "Access privileges" FROM pg_catalog.pg_database d ORDER BY 1;`,
		// 20: psql \dT
		`SELECT n.nspname as "Schema", pg_catalog.format_type(t.oid, NULL) AS "Name", pg_catalog.obj_description(t.oid, 'pg_type') as "Description" FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace WHERE (t.typrelid = 0 OR (SELECT c.relkind = 'c' FROM pg_catalog.pg_class c WHERE c.oid = t.typrelid)) AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_type el WHERE el.oid = t.typelem AND el.typarray = t.oid) AND pg_catalog.pg_type_is_visible(t.oid) ORDER BY 1, 2;`,
		// 21: psql \du
		`SELECT r.rolname, r.rolsuper, r.rolinherit, r.rolcreaterole, r.rolcreatedb, r.rolcanlogin, r.rolconnlimit, r.rolvaliduntil, ARRAY(SELECT b.rolname FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles b ON (m.roleid = b.oid) WHERE m.member = r.oid) as memberof, pg_catalog.shobj_description(r.oid, 'pg_authid') AS description, r.rolreplication, r.rolbypassrls FROM pg_catalog.pg_roles r WHERE r.rolname !~ '^pg_' ORDER BY 1;`,

		// 22: pgjdbc getTables
		`SELECT NULL AS TABLE_CAT, n.nspname AS TABLE_SCHEM, c.relname AS TABLE_NAME,  CASE n.nspname ~ '^pg_' OR n.nspname = 'information_schema'  WHEN true THEN CASE  WHEN n.nspname = 'pg_catalog' OR n.nspname = 'information_schema' THEN CASE c.relkind   WHEN 'r' THEN 'SYSTEM TABLE'   WHEN 'v' THEN 'SYSTEM VIEW'   WHEN 'i' THEN 'SYSTEM INDEX'   ELSE NULL   END  WHEN n.nspname = 'pg_toast' THEN CASE c.relkind   WHEN 'r' THEN 'SYSTEM TOAST TABLE'   WHEN 'i' THEN 'SYSTEM TOAST INDEX'   ELSE NULL   END  ELSE CASE c.relkind   WHEN 'r' THEN 'TEMPORARY TABLE'   WHEN 'p' THEN 'TEMPORARY TABLE'   WHEN 'i' THEN 'TEMPORARY INDEX'   WHEN 'S' THEN 'TEMPORARY SEQUENCE'   WHEN 'v' THEN 'TEMPORARY VIEW'   ELSE NULL   END  END  WHEN false THEN CASE c.relkind  WHEN 'r' THEN 'TABLE'  WHEN 'p' THEN 'PARTITIONED TABLE'  WHEN 'i' THEN 'INDEX'  WHEN 'P' THEN 'PARTITIONED INDEX'  WHEN 'S' THEN 'SEQUENCE'  WHEN 'v' THEN 'VIEW'  WHEN 'c' THEN 'TYPE'  WHEN 'f' THEN 'FOREIGN TABLE'  WHEN 'm' THEN 'MATERIALIZED VIEW'  ELSE NULL  END  ELSE NULL  END  AS TABLE_TYPE, d.description AS REMARKS, '' as TYPE_CAT, '' as TYPE_SCHEM, '' as TYPE_NAME, '' AS SELF_REFERENCING_COL_NAME, '' AS REF_GENERATION  FROM pg_catalog.pg_namespace n, pg_catalog.pg_class c  LEFT JOIN pg_catalog.pg_description d ON (c.oid = d.objoid AND d.objsubid = 0  and d.classoid = 'pg_class'::regclass) WHERE c.relnamespace = n.oid  AND (false  OR ( c.relkind = 'r' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' ) OR ( c.relkind = 'S' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' ) ) ORDER BY TABLE_TYPE,TABLE_SCHEM,TABLE_NAME`,
		// 23: getColumns
		`SELECT * FROM (SELECT n.nspname,c.relname,a.attname,a.atttypid,a.attnotnull OR (t.typtype = 'd' AND t.typnotnull) AS attnotnull,a.atttypmod,a.attlen,t.typtypmod,row_number() OVER (PARTITION BY a.attrelid ORDER BY a.attnum) AS attnum, nullif(a.attidentity, '') as attidentity,nullif(a.attgenerated, '') as attgenerated,pg_catalog.pg_get_expr(def.adbin, def.adrelid) AS adsrc,dsc.description,t.typbasetype,t.typtype  FROM pg_catalog.pg_namespace n  JOIN pg_catalog.pg_class c ON (c.relnamespace = n.oid)  JOIN pg_catalog.pg_attribute a ON (a.attrelid=c.oid)  JOIN pg_catalog.pg_type t ON (a.atttypid = t.oid)  LEFT JOIN pg_catalog.pg_attrdef def ON (a.attrelid=def.adrelid AND a.attnum = def.adnum)  LEFT JOIN pg_catalog.pg_description dsc ON (c.oid=dsc.objoid AND a.attnum = dsc.objsubid)  LEFT JOIN pg_catalog.pg_class dc ON (dc.oid=dsc.classoid AND dc.relname='pg_class')  LEFT JOIN pg_catalog.pg_namespace dn ON (dc.relnamespace=dn.oid AND dn.nspname='pg_catalog')  WHERE c.relkind in ('r','p','v','f','m') and a.attnum > 0 AND NOT a.attisdropped  AND c.relname LIKE E'items') c WHERE true  ORDER BY nspname,c.relname,attnum`,
		// 24: getPrimaryKeys
		`SELECT result.TABLE_CAT, result.TABLE_SCHEM, result.TABLE_NAME, result.COLUMN_NAME, result.KEY_SEQ, result.PK_NAME FROM (SELECT NULL AS TABLE_CAT, n.nspname AS TABLE_SCHEM,   ct.relname AS TABLE_NAME, a.attname AS COLUMN_NAME,   (information_schema._pg_expandarray(i.indkey)).n AS KEY_SEQ, ci.relname AS PK_NAME,   information_schema._pg_expandarray(i.indkey) AS KEYS, a.attnum AS A_ATTNUM FROM pg_catalog.pg_class ct   JOIN pg_catalog.pg_attribute a ON (ct.oid = a.attrelid)   JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid)   JOIN pg_catalog.pg_index i ON ( a.attrelid = i.indrelid)   JOIN pg_catalog.pg_class ci ON (ci.oid = i.indexrelid) WHERE true  AND ct.relname = E'items' AND i.indisprimary  ) result where  result.A_ATTNUM = (result.KEYS).x  ORDER BY result.table_name, result.pk_name, result.key_seq`,
		// 25: getIndexInfo
		`SELECT tmp.TABLE_CAT, tmp.TABLE_SCHEM, tmp.TABLE_NAME, tmp.NON_UNIQUE, tmp.INDEX_QUALIFIER, tmp.INDEX_NAME, tmp.TYPE, tmp.ORDINAL_POSITION, trim(both '"' from pg_catalog.pg_get_indexdef(tmp.CI_OID, tmp.ORDINAL_POSITION, false)) AS COLUMN_NAME, CASE tmp.AM_NAME WHEN 'btree' THEN CASE tmp.I_INDOPTION[tmp.ORDINAL_POSITION - 1] & 1 WHEN 1 THEN 'D' ELSE 'A' END ELSE NULL END AS ASC_OR_DESC, tmp.CARDINALITY, tmp.PAGES, tmp.FILTER_CONDITION FROM (SELECT NULL AS TABLE_CAT, n.nspname AS TABLE_SCHEM,   ct.relname AS TABLE_NAME, NOT i.indisunique AS NON_UNIQUE,   NULL AS INDEX_QUALIFIER, ci.relname AS INDEX_NAME,   CASE i.indisclustered     WHEN true THEN 1    ELSE CASE am.amname       WHEN 'hash' THEN 2      ELSE 3    END   END AS TYPE,   (information_schema._pg_expandarray(i.indkey)).n AS ORDINAL_POSITION,   ci.reltuples AS CARDINALITY,   ci.relpages AS PAGES,   pg_catalog.pg_get_expr(i.indpred, i.indrelid) AS FILTER_CONDITION,   ci.oid AS CI_OID,   i.indoption AS I_INDOPTION,   am.amname AS AM_NAME FROM pg_catalog.pg_class ct   JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid)   JOIN pg_catalog.pg_index i ON (ct.oid = i.indrelid)   JOIN pg_catalog.pg_class ci ON (ci.oid = i.indexrelid)   JOIN pg_catalog.pg_am am ON (ci.relam = am.oid) WHERE true  AND n.nspname = E'public' AND ct.relname = E'items') AS tmp ORDER BY NON_UNIQUE, TYPE, INDEX_NAME, ORDINAL_POSITION`,
		// 26: getSchemas
		`SELECT nspname AS TABLE_SCHEM, NULL AS TABLE_CATALOG FROM pg_catalog.pg_namespace WHERE nspname <> 'pg_toast' AND (nspname !~ '^pg_temp_'  OR nspname = (pg_catalog.current_schemas(true))[pg_catalog.array_lower(pg_catalog.current_schemas(true),1)]) AND (nspname !~ '^pg_toast_temp_'  OR nspname = replace((pg_catalog.current_schemas(true))[pg_catalog.array_lower(pg_catalog.current_schemas(true),1)], 'pg_temp_', 'pg_toast_temp_')) ORDER BY TABLE_SCHEM`,
		// 27: getCatalogs
		`SELECT datname AS TABLE_CAT FROM pg_catalog.pg_database WHERE datallowconn = true ORDER BY datname`,
		// 28: getTypeInfo
		`SELECT t.typname,t.oid FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON (t.typnamespace = n.oid) WHERE n.nspname != 'pg_toast' AND (t.typrelid = 0 OR (SELECT c.relkind = 'c' FROM pg_catalog.pg_class c WHERE c.oid = t.typrelid))`,
		// 29: TypeInfoCache by OID
		`SELECT n.nspname = ANY(current_schemas(true)), n.nspname, t.typname FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON t.typnamespace = n.oid WHERE t.oid = 25`,
		// 30: TypeInfoCache by name
		`SELECT t.typarray, arr.typname FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON t.typnamespace = n.oid LEFT JOIN pg_catalog.pg_type arr ON arr.oid = t.typarray WHERE t.typname = 'int4' AND (n.nspname = 'pg_catalog' OR true AND n.nspname = ANY (current_schemas(true))) ORDER BY t.oid DESC LIMIT 1`,
		// 31
		`SELECT oid, typbasetype FROM pg_type WHERE typname = 'int4'`,
		// 32
		`SELECT current_schema(), version(), current_database(), current_setting('server_version_num')`,
		// 33: getImportedKeys (no foreign keys here)
		`SELECT NULL::text AS PKTABLE_CAT, pkn.nspname AS PKTABLE_SCHEM, pkc.relname AS PKTABLE_NAME, pka.attname AS PKCOLUMN_NAME, NULL::text AS FKTABLE_CAT, fkn.nspname AS FKTABLE_SCHEM, fkc.relname AS FKTABLE_NAME, fka.attname AS FKCOLUMN_NAME, pos.n AS KEY_SEQ, CASE con.confupdtype  WHEN 'c' THEN 0 WHEN 'n' THEN 2 WHEN 'd' THEN 4 WHEN 'r' THEN 1 WHEN 'a' THEN 3 ELSE NULL END AS UPDATE_RULE, con.conname AS FK_NAME, pkic.relname AS PK_NAME, con.condeferrable AS DEFERRABILITY FROM pg_catalog.pg_namespace pkn, pg_catalog.pg_class pkc, pg_catalog.pg_attribute pka, pg_catalog.pg_namespace fkn, pg_catalog.pg_class fkc, pg_catalog.pg_attribute fka, pg_catalog.pg_constraint con, pg_catalog.generate_series(1, 32) pos(n), pg_catalog.pg_class pkic WHERE pkn.oid = pkc.relnamespace AND pkc.oid = pka.attrelid AND pka.attnum = con.confkey[pos.n] AND con.confrelid = pkc.oid AND fkn.oid = fkc.relnamespace AND fkc.oid = fka.attrelid AND fka.attnum = con.conkey[pos.n] AND con.conrelid = fkc.oid AND con.contype = 'f' AND pkic.oid = con.conindid AND fkc.relname = 'items' ORDER BY pkn.nspname,pkc.relname,con.conname,pos.n`,

		// 34..: information_schema, as Liquibase, DBeaver and ORMs read it
		`SELECT table_catalog, table_schema, table_name, table_type FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name`,
		`SELECT column_name, data_type, is_nullable, column_default, ordinal_position, character_maximum_length, numeric_precision, udt_name, is_identity FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'items' ORDER BY ordinal_position`,
		`SELECT tc.constraint_name, tc.constraint_type, kcu.column_name, kcu.ordinal_position FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema AND tc.table_name = kcu.table_name WHERE tc.table_name = 'items' AND tc.constraint_type = 'PRIMARY KEY'`,
		`SELECT schema_name FROM information_schema.schemata ORDER BY 1`,

		// 38..: DBeaver (its PostgreSQL plugin) loading the schema tree
		`SELECT n.oid,n.*,d.description FROM pg_catalog.pg_namespace n LEFT OUTER JOIN pg_catalog.pg_description d ON d.objoid=n.oid AND d.objsubid=0 AND d.classoid='pg_namespace'::regclass ORDER BY nspname`,
		`SELECT c.oid,c.*,d.description,pg_catalog.pg_get_expr(c.relpartbound, c.oid) as partition_expr,  pg_catalog.pg_get_partkeydef(c.oid) as partition_key FROM pg_catalog.pg_class c LEFT OUTER JOIN pg_catalog.pg_description d ON d.objoid=c.oid AND d.objsubid=0 AND d.classoid='pg_class'::regclass WHERE c.relnamespace=2200 AND c.relkind not in ('i','I','c')`,
		`SELECT a.*,pg_catalog.pg_get_expr(ad.adbin, ad.adrelid, true) as def_value,dsc.description,dep.objid AS serial_seq FROM pg_catalog.pg_attribute a LEFT OUTER JOIN pg_catalog.pg_attrdef ad ON (a.attrelid=ad.adrelid AND a.attnum = ad.adnum) LEFT OUTER JOIN pg_catalog.pg_description dsc ON (a.attrelid=dsc.objoid AND a.attnum = dsc.objsubid) LEFT OUTER JOIN pg_depend dep ON dep.refobjid = a.attrelid AND dep.deptype = 'a' AND dep.refobjsubid = a.attnum AND dep.classid = (SELECT oid FROM pg_class WHERE relname = 'pg_class') AND dep.objid IN (SELECT oid FROM pg_class WHERE relkind = 'S') WHERE a.attnum > 0 AND NOT a.attisdropped AND a.attrelid='TBL' ORDER BY a.attnum`,
		`SELECT c.oid,c.*,t.relname as tabrelname,rt.relnamespace as refnamespace,d.description, case when c.contype='c' then "substring"(pg_get_constraintdef(c.oid), 7) else null end consrc_copy FROM pg_catalog.pg_constraint c INNER JOIN pg_catalog.pg_class t ON t.oid=c.conrelid LEFT OUTER JOIN pg_catalog.pg_class rt ON rt.oid=c.confrelid LEFT OUTER JOIN pg_catalog.pg_description d ON d.objoid=c.oid AND d.objsubid=0 AND d.classoid='pg_constraint'::regclass WHERE t.relnamespace=2200 ORDER BY c.oid`,
		`SELECT i.*,i.indkey as keys,c.relname,c.relnamespace,c.relam,c.reltablespace,tc.relname as tabrelname,dsc.description,pg_catalog.pg_get_expr(i.indpred, i.indrelid) as pred_expr,pg_catalog.pg_get_expr(i.indexprs, i.indrelid, true) as expr,pg_catalog.pg_get_indexdef(i.indexrelid, 0, true) as def FROM pg_catalog.pg_index i INNER JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid INNER JOIN pg_catalog.pg_class tc ON tc.oid=i.indrelid LEFT OUTER JOIN pg_catalog.pg_description dsc ON i.indexrelid=dsc.objoid WHERE  tc.relnamespace=2200 ORDER BY c.relname`,
		`SELECT t.oid,t.*,c.relkind,format_type(nullif(t.typbasetype, 0), t.typtypmod) as base_type_name, d.description FROM pg_catalog.pg_type t LEFT OUTER JOIN pg_catalog.pg_class c ON c.oid=t.typrelid LEFT OUTER JOIN pg_catalog.pg_description d ON t.oid=d.objoid WHERE typnamespace=11 ORDER BY t.oid`,
		`SELECT db.oid,db.*,pg_catalog.pg_encoding_to_char(db.encoding) as encoding_name,t.spcname FROM pg_catalog.pg_database db LEFT OUTER JOIN pg_catalog.pg_tablespace t ON db.dattablespace=t.oid WHERE datallowconn AND NOT datistemplate`,
		`SELECT p.oid,p.*,pg_catalog.pg_get_expr(p.proargdefaults, 0) as arg_defaults,d.description FROM pg_catalog.pg_proc p LEFT OUTER JOIN pg_catalog.pg_description d ON d.objoid=p.oid WHERE p.pronamespace=2200 AND p.prokind <> 'a' ORDER BY p.proname`,
		`SELECT current_schema(),session_user`,
		`SELECT * FROM pg_catalog.pg_roles ORDER BY rolname`,
		// psql with a schema-qualified pattern, \dt it*
		`SELECT c.oid, n.nspname, c.relname FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname OPERATOR(pg_catalog.~) '^(public)$' COLLATE pg_catalog.default AND c.relname OPERATOR(pg_catalog.~) '^(items)$' COLLATE pg_catalog.default ORDER BY 2, 3;`,
		`SELECT n.nspname as "Schema", c.relname as "Name", CASE c.relkind WHEN 'r' THEN 'table' END as "Type", pg_catalog.pg_get_userbyid(c.relowner) as "Owner" FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p','') AND n.nspname <> 'pg_catalog' AND n.nspname !~ '^pg_toast' AND n.nspname <> 'information_schema' AND c.relname OPERATOR(pg_catalog.~) '^(it.*)$' COLLATE pg_catalog.default AND pg_catalog.pg_table_is_visible(c.oid) ORDER BY 1,2;`,
		// Hibernate / Liquibase
		`select sequence_name, start_value, increment from information_schema.sequences`,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`,
		`SELECT NULL AS PROCEDURE_CAT, n.nspname AS PROCEDURE_SCHEM, p.proname AS PROCEDURE_NAME, NULL, NULL, NULL, d.description AS REMARKS, 2 AS PROCEDURE_TYPE, p.proname || '_' || p.oid AS SPECIFIC_NAME FROM pg_catalog.pg_proc p LEFT JOIN pg_catalog.pg_namespace n ON p.pronamespace=n.oid LEFT JOIN pg_catalog.pg_description d ON (p.oid=d.objoid) LEFT JOIN pg_catalog.pg_class c ON (d.classoid=c.oid AND c.relname='pg_proc') LEFT JOIN pg_catalog.pg_namespace pn ON (c.relnamespace=pn.oid AND pn.nspname='pg_catalog') WHERE p.prokind='p' AND true  ORDER BY PROCEDURE_SCHEM, PROCEDURE_NAME, p.oid::text`,
		`SELECT pg_catalog.set_config('search_path', '', false)`,
		`SELECT obj_description('TBL'::oid, 'pg_class'), col_description('TBL'::oid, 1), to_regclass('items'), to_regclass('nope'), to_regclass('public.items_pkey'), 'items'::regclass, '"items"'::regclass, 'public.items'::regclass::oid, 'pg_catalog.pg_class'::regclass::oid, 'int8'::regtype::oid, pg_get_serial_sequence('items', 'id'), pg_table_is_visible('TBL'::oid), has_table_privilege('items', 'SELECT')`,
	}
	for i, q := range qs {
		q = strings.ReplaceAll(q, "'TBL'", "'"+tbl+"'")
		qs[i] = strings.ReplaceAll(q, "'SEQ'", "'"+seq+"'")
	}
	return qs
}
