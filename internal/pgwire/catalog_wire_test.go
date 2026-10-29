package pgwire

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestCatalogOverWire runs the catalog queries of psql (\dt, \d tbl) and of
// pgjdbc (getTables, getColumns, getPrimaryKeys) through pgx, in the
// extended protocol (parameters, Describe) and the simple one.
func TestCatalogOverWire(t *testing.T) {
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeSimpleProtocol} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			ctx := context.Background()
			cfg, err := pgx.ParseConfig(startServer(t, ""))
			if err != nil {
				t.Fatal(err)
			}
			cfg.DefaultQueryExecMode = mode
			conn, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			for _, q := range []string{
				`CREATE TABLE items (id serial PRIMARY KEY, name text NOT NULL DEFAULT 'x', tmp text, qty bigint)`,
				`ALTER TABLE items DROP COLUMN tmp`,
				`CREATE TABLE ev (id bigint PRIMARY KEY, v double precision) WITH (columnar = true)`,
			} {
				if _, err := conn.Exec(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			all := func(q string, args ...any) string {
				t.Helper()
				rows, err := conn.Query(ctx, q, args...)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				defer rows.Close()
				var out []string
				for rows.Next() {
					vals, err := rows.Values()
					if err != nil {
						t.Fatalf("%s: %v", q, err)
					}
					var cells []string
					for _, v := range vals {
						if v == nil {
							cells = append(cells, "NULL")
						} else {
							cells = append(cells, fmt.Sprint(v))
						}
					}
					out = append(out, strings.Join(cells, ","))
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return strings.Join(out, ";")
			}
			check := func(got, want string) {
				t.Helper()
				if got != want {
					t.Errorf("got  %s\nwant %s", got, want)
				}
			}
			// psql \dt
			check(all(`SELECT n.nspname as "Schema", c.relname as "Name", CASE c.relkind WHEN 'r' THEN 'table' WHEN 'S' THEN 'sequence' END as "Type", pg_catalog.pg_get_userbyid(c.relowner) as "Owner" FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p','') AND n.nspname <> 'pg_catalog' AND n.nspname !~ '^pg_toast' AND n.nspname <> 'information_schema' AND pg_catalog.pg_table_is_visible(c.oid) ORDER BY 1,2`),
				"public,ev,table,stask;public,items,table,stask")
			// psql \d items
			var oid int64
			if err := conn.QueryRow(ctx, `SELECT c.oid FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relname OPERATOR(pg_catalog.~) '^(items)$' COLLATE pg_catalog.default AND pg_catalog.pg_table_is_visible(c.oid) ORDER BY 1`).Scan(&oid); err != nil {
				t.Fatal(err)
			}
			check(all(fmt.Sprintf(`SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid, true) FROM pg_catalog.pg_attrdef d WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef), a.attnotnull, a.attidentity FROM pg_catalog.pg_attribute a WHERE a.attrelid = '%d' AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)),
				"id,bigint,nextval('items_id_seq'::regclass),true,;name,text,'x',true,;qty,bigint,NULL,false,")
			// pgjdbc getTables, getColumns, getPrimaryKeys with bound parameters
			check(all(`SELECT n.nspname AS TABLE_SCHEM, c.relname AS TABLE_NAME, CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'S' THEN 'SEQUENCE' END AS TABLE_TYPE FROM pg_catalog.pg_namespace n, pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_description d ON (c.oid = d.objoid AND d.objsubid = 0 and d.classoid = 'pg_class'::regclass) WHERE c.relnamespace = n.oid AND (false OR ( c.relkind = 'r' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' ) ) AND c.relname LIKE $1 ORDER BY TABLE_TYPE,TABLE_SCHEM,TABLE_NAME`, "it%"),
				"public,items,TABLE")
			check(all(`SELECT * FROM (SELECT n.nspname,c.relname,a.attname,a.atttypid,a.attnotnull OR (t.typtype = 'd' AND t.typnotnull) AS attnotnull,a.atttypmod,a.attlen,t.typtypmod,row_number() OVER (PARTITION BY a.attrelid ORDER BY a.attnum) AS attnum, nullif(a.attidentity, '') as attidentity,pg_catalog.pg_get_expr(def.adbin, def.adrelid) AS adsrc,dsc.description,t.typbasetype,t.typtype FROM pg_catalog.pg_namespace n JOIN pg_catalog.pg_class c ON (c.relnamespace = n.oid) JOIN pg_catalog.pg_attribute a ON (a.attrelid=c.oid) JOIN pg_catalog.pg_type t ON (a.atttypid = t.oid) LEFT JOIN pg_catalog.pg_attrdef def ON (a.attrelid=def.adrelid AND a.attnum = def.adnum) LEFT JOIN pg_catalog.pg_description dsc ON (c.oid=dsc.objoid AND a.attnum = dsc.objsubid) WHERE c.relkind in ('r','p','v','f','m') and a.attnum > 0 AND NOT a.attisdropped AND c.relname LIKE $1) c WHERE true ORDER BY nspname,c.relname,attnum`, "items"),
				"public,items,id,20,true,-1,8,-1,1,NULL,nextval('items_id_seq'::regclass),NULL,0,b;"+
					"public,items,name,25,true,-1,-1,-1,2,NULL,'x',NULL,0,b;public,items,qty,20,false,-1,8,-1,3,NULL,NULL,NULL,0,b")
			check(all(`SELECT result.TABLE_SCHEM, result.TABLE_NAME, result.COLUMN_NAME, result.KEY_SEQ, result.PK_NAME FROM (SELECT n.nspname AS TABLE_SCHEM, ct.relname AS TABLE_NAME, a.attname AS COLUMN_NAME, (information_schema._pg_expandarray(i.indkey)).n AS KEY_SEQ, ci.relname AS PK_NAME, information_schema._pg_expandarray(i.indkey) AS KEYS, a.attnum AS A_ATTNUM FROM pg_catalog.pg_class ct JOIN pg_catalog.pg_attribute a ON (ct.oid = a.attrelid) JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid) JOIN pg_catalog.pg_index i ON ( a.attrelid = i.indrelid) JOIN pg_catalog.pg_class ci ON (ci.oid = i.indexrelid) WHERE true AND ct.relname = $1 AND i.indisprimary) result where result.A_ATTNUM = (result.KEYS).x ORDER BY result.table_name, result.pk_name, result.key_seq`, "items"),
				"public,items,id,1,items_pkey")
			check(all(`SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`, "public", "ev"),
				"ev,id,bigint;ev,v,double precision")
		})
	}
}
