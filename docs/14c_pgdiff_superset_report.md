# pgdiff nhóm g (Superset/SQLAlchemy), nhánh superq, 2026-10-30

Toàn văn 88 câu khác biệt (65 DIFF + 23 ERR-ONLY-ONE-SIDE) và 2 PASS-MSG của `scripts/pgdiff/g_superset.sql` (341 câu). PostgreSQL 16.15 (`timezone=UTC`) so với STASK `main` @ bcb74f9. Phân loại và mức ảnh hưởng: `docs/14` Phụ lục E.

| group | statements | PASS | PASS-MSG | DIFF | ERR-ONLY-ONE-SIDE |
|---|---|---|---|---|---|
| g | 341 | 251 | 2 | 65 | 23 |

### ERR-ONLY-ONE-SIDE — g/ss-setup:22 (feature)

```sql
CREATE INDEX orders_created_idx ON orders (created)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE INDEX`
- stask: `ERROR 0A000: columns of type timestamptz cannot be indexed`

### DIFF — g/ss-connect:31 (value)

```sql
select pg_catalog.version()
```
- why: row 1 col version: pg="PostgreSQL 16.15 (Debian 16.15-1.pgdg13+2) on x86_64-pc-linux-gnu, compiled by g…" stask="PostgreSQL 16.0 (STASK prototype)"
- pg: `cols(version:25) 1 rows ["PostgreSQL 16.15 (Debian 16.15-1.pgdg13+2) on x86_64-pc-linux-gnu, compiled by g…"] tag=SELECT 1`
- stask: `cols(version:25) 1 rows ["PostgreSQL 16.0 (STASK prototype)"] tag=SELECT 1`

### DIFF — g/ss-connect:32 (value)

```sql
SELECT version()
```
- why: row 1 col version: pg="PostgreSQL 16.15 (Debian 16.15-1.pgdg13+2) on x86_64-pc-linux-gnu, compiled by g…" stask="PostgreSQL 16.0 (STASK prototype)"
- pg: `cols(version:25) 1 rows ["PostgreSQL 16.15 (Debian 16.15-1.pgdg13+2) on x86_64-pc-linux-gnu, compiled by g…"] tag=SELECT 1`
- stask: `cols(version:25) 1 rows ["PostgreSQL 16.0 (STASK prototype)"] tag=SELECT 1`

### DIFF — g/ss-connect:34 (type)

```sql
select current_schema()
```
- why: col 1 (current_schema) type OID pg=19 stask=25
- pg: `cols(current_schema:19) 1 rows ["public"] tag=SELECT 1`
- stask: `cols(current_schema:25) 1 rows ["public"] tag=SELECT 1`

### DIFF — g/ss-connect:35 (type)

```sql
select pg_catalog.current_schema()
```
- why: col 1 (current_schema) type OID pg=19 stask=25
- pg: `cols(current_schema:19) 1 rows ["public"] tag=SELECT 1`
- stask: `cols(current_schema:25) 1 rows ["public"] tag=SELECT 1`

### DIFF — g/ss-connect:36 (value)

```sql
show transaction isolation level
```
- why: row 1 col transaction_isolation: pg="read committed" stask="repeatable read"
- pg: `cols(transaction_isolation:25) 1 rows ["read committed"] tag=SHOW`
- stask: `cols(transaction_isolation:25) 1 rows ["repeatable read"] tag=SHOW`

### DIFF — g/ss-connect:37 (value)

```sql
SHOW server_version
```
- why: row 1 col server_version: pg="16.15 (Debian 16.15-1.pgdg13+2)" stask="16.0"
- pg: `cols(server_version:25) 1 rows ["16.15 (Debian 16.15-1.pgdg13+2)"] tag=SHOW`
- stask: `cols(server_version:25) 1 rows ["16.0"] tag=SHOW`

### DIFF — g/ss-connect:38 (value)

```sql
SHOW server_version_num
```
- why: row 1 col server_version_num: pg="160015" stask="160000"
- pg: `cols(server_version_num:25) 1 rows ["160015"] tag=SHOW`
- stask: `cols(server_version_num:25) 1 rows ["160000"] tag=SHOW`

### DIFF — g/ss-connect:39 (type+value)

```sql
SELECT current_database()
```
- why: col 1 (current_database) type OID pg=19 stask=25; row 1 col current_database: pg="postgres" stask="stask"
- pg: `cols(current_database:19) 1 rows ["postgres"] tag=SELECT 1`
- stask: `cols(current_database:25) 1 rows ["stask"] tag=SELECT 1`

### DIFF — g/ss-connect:40 (type+value)

```sql
SELECT current_user
```
- why: col 1 (current_user) type OID pg=19 stask=25; row 1 col current_user: pg="postgres" stask="stask"
- pg: `cols(current_user:19) 1 rows ["postgres"] tag=SELECT 1`
- stask: `cols(current_user:25) 1 rows ["stask"] tag=SELECT 1`

### DIFF — g/ss-connect:57 (type)

```sql
SELECT pg_catalog.pg_encoding_to_char(encoding) FROM pg_catalog.pg_database WHERE datname = current_database()
```
- why: col 1 (pg_encoding_to_char) type OID pg=19 stask=25
- pg: `cols(pg_encoding_to_char:19) 1 rows ["UTF8"] tag=SELECT 1`
- stask: `cols(pg_encoding_to_char:25) 1 rows ["UTF8"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-tables:61 (feature)

```sql
SELECT pg_catalog.pg_namespace.nspname FROM pg_catalog.pg_namespace WHERE pg_catalog.pg_namespace.nspname != 'information_schema' AND pg_catalog.pg_namespace.nspname NOT LIKE 'pg_%' ORDER BY pg_catalog.pg_namespace.nspname
```
- why: STASK rejects: 42601
- pg: `cols(nspname:19) 1 rows ["public"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "."`

### DIFF — g/ss-reflect-tables:62 (type)

```sql
SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' ORDER BY nspname
```
- why: col 1 (nspname) type OID pg=19 stask=25
- pg: `cols(nspname:19) 2 rows ["information_schema"] ["public"] tag=SELECT 2`
- stask: `cols(nspname:25) 2 rows ["information_schema"] ["public"] tag=SELECT 2`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-tables:63 (feature)

```sql
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' ORDER BY pg_catalog.pg_class.relname
```
- why: STASK rejects: 42601
- pg: `cols(relname:19) 3 rows ["order_items"] ["orders"] ["sims"] tag=SELECT 3`
- stask: `ERROR 42601: syntax error at or near "."`

### DIFF — g/ss-reflect-tables:64 (type)

```sql
SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY c.relname
```
- why: col 1 (relname) type OID pg=19 stask=25
- pg: `cols(relname:19) 3 rows ["order_items"] ["orders"] ["sims"] tag=SELECT 3`
- stask: `cols(relname:25) 3 rows ["order_items"] ["orders"] ["sims"] tag=SELECT 3`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-tables:67 (feature)

```sql
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relname = 'sims' AND pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p', 'f', 'v', 'm']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog'
```
- why: STASK rejects: 42601
- pg: `cols(relname:19) 1 rows ["sims"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "."`

### DIFF — g/ss-reflect-tables:68 (type)

```sql
select relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where pg_catalog.pg_table_is_visible(c.oid) and relname='sims'
```
- why: col 1 (relname) type OID pg=19 stask=25
- pg: `cols(relname:19) 1 rows ["sims"] tag=SELECT 1`
- stask: `cols(relname:25) 1 rows ["sims"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:71 (type)

```sql
select relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where pg_catalog.pg_table_is_visible(c.oid) and relname=$1
```
args: "sims"

- why: col 1 (relname) type OID pg=19 stask=25
- pg: `cols(relname:19) 1 rows ["sims"] tag=SELECT 1`
- stask: `cols(relname:25) 1 rows ["sims"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:73 (type+value)

```sql
SELECT c.oid FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE (pg_catalog.pg_table_is_visible(c.oid)) AND c.relname = 'sims' AND c.relkind in ('r', 'v', 'm', 'f', 'p')
```
note: raw oid values differ between servers by design; only the shape matters

- why: col 1 (oid) type OID pg=26 stask=20; row 1 col oid: pg="16753" stask="16430"
- pg: `cols(oid:26) 1 rows ["16753"] tag=SELECT 1`
- stask: `cols(oid:20) 1 rows ["16430"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:74 (type)

```sql
SELECT c.oid IS NOT NULL AS has_oid, c.relkind, n.nspname FROM pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE (pg_catalog.pg_table_is_visible(c.oid)) AND c.relname = 'sims' AND c.relkind in ('r', 'v', 'm', 'f', 'p')
```
- why: col 2 (relkind) type OID pg=18 stask=25; col 3 (nspname) type OID pg=19 stask=25
- pg: `cols(has_oid:16, relkind:18, nspname:19) 1 rows ["t", "r", "public"] tag=SELECT 1`
- stask: `cols(has_oid:16, relkind:25, nspname:25) 1 rows ["t", "r", "public"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:75 (type)

```sql
SELECT c.relname, c.relkind, c.relispartition FROM pg_catalog.pg_class c WHERE c.relname = 'sims'
```
- why: col 1 (relname) type OID pg=19 stask=25; col 2 (relkind) type OID pg=18 stask=25
- pg: `cols(relname:19, relkind:18, relispartition:16) 1 rows ["sims", "r", "f"] tag=SELECT 1`
- stask: `cols(relname:25, relkind:25, relispartition:16) 1 rows ["sims", "r", "f"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:76 (type)

```sql
SELECT c.relname, c.relkind FROM pg_catalog.pg_class c WHERE c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = 'public') AND c.relkind IN ('r', 'v', 'm', 'f', 'p') AND c.relname IN ('sims', 'orders', 'order_items') ORDER BY 1
```
- why: col 1 (relname) type OID pg=19 stask=25; col 2 (relkind) type OID pg=18 stask=25
- pg: `cols(relname:19, relkind:18) 3 rows ["order_items", "r"] ["orders", "r"] ["sims", "r"] tag=SELECT 3`
- stask: `cols(relname:25, relkind:25) 3 rows ["order_items", "r"] ["orders", "r"] ["sims", "r"] tag=SELECT 3`

### DIFF — g/ss-reflect-tables:77 (type)

```sql
SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name
```
- why: col 1 (table_name) type OID pg=19 stask=25
- pg: `cols(table_name:19) 3 rows ["order_items"] ["orders"] ["sims"] tag=SELECT 3`
- stask: `cols(table_name:25) 3 rows ["order_items"] ["orders"] ["sims"] tag=SELECT 3`

### DIFF — g/ss-reflect-tables:78 (type)

```sql
SELECT table_schema, table_name, table_type FROM information_schema.tables WHERE table_name = 'sims'
```
- why: col 1 (table_schema) type OID pg=19 stask=25; col 2 (table_name) type OID pg=19 stask=25; col 3 (table_type) type OID pg=1043 stask=25
- pg: `cols(table_schema:19, table_name:19, table_type:1043) 1 rows ["public", "sims", "BASE TABLE"] tag=SELECT 1`
- stask: `cols(table_schema:25, table_name:25, table_type:25) 1 rows ["public", "sims", "BASE TABLE"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:79 (type)

```sql
SELECT schema_name FROM information_schema.schemata ORDER BY schema_name
```
- why: col 1 (schema_name) type OID pg=19 stask=25; command tag pg="SELECT 6" stask="SELECT 3"; row count pg=6 stask=3
- pg: `cols(schema_name:19) 6 rows ["information_schema"] ["pg_catalog"] ["pg_temp_3"] ... tag=SELECT 6`
- stask: `cols(schema_name:25) 3 rows ["information_schema"] ["pg_catalog"] ["public"] tag=SELECT 3`

### DIFF — g/ss-reflect-tables:82 (type+value)

```sql
SELECT pg_catalog.pg_get_userbyid(c.relowner) FROM pg_catalog.pg_class c WHERE c.relname = 'sims'
```
- why: col 1 (pg_get_userbyid) type OID pg=19 stask=25; row 1 col pg_get_userbyid: pg="postgres" stask="stask"
- pg: `cols(pg_get_userbyid:19) 1 rows ["postgres"] tag=SELECT 1`
- stask: `cols(pg_get_userbyid:25) 1 rows ["stask"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:85 (value)

```sql
SELECT 'public.sims'::regclass::text
```
- why: row 1 col text: pg="sims" stask="16430"
- pg: `cols(text:25) 1 rows ["sims"] tag=SELECT 1`
- stask: `cols(text:25) 1 rows ["16430"] tag=SELECT 1`

### DIFF — g/ss-reflect-tables:86 (value)

```sql
SELECT to_regclass('sims')::text
```
- why: row 1 col to_regclass: pg="sims" stask="16430"
- pg: `cols(to_regclass:25) 1 rows ["sims"] tag=SELECT 1`
- stask: `cols(to_regclass:25) 1 rows ["16430"] tag=SELECT 1`

### DIFF — g/ss-reflect-columns:91 (type+value)

```sql
SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid) FROM pg_catalog.pg_attrdef d WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef) AS DEFAULT, a.attnotnull, a.attrelid AS table_oid, pgd.description AS comment FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_description pgd ON (pgd.objoid = a.attrelid AND pgd.objsubid = a.attnum) WHERE a.attrelid = (SELECT c.oid FROM pg_catalog.pg_class c WHERE c.relname = 'sims' AND pg_catalog.pg_table_is_visible(c.oid)) AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum
```
- why: col 1 (attname) type OID pg=19 stask=25; col 5 (table_oid) type OID pg=26 stask=20; row 1 col table_oid: pg="16753" stask="16430"
- pg: `cols(attname:19, format_type:25, default:25, attnotnull:16, table_oid:26, comment:25) 8 rows ["number", "text", NULL, "t", "16753", NULL] ["carrier", "text", NULL, "t", "16753", NULL] ["status", "text", NULL, "t", "16753", NULL] ... tag=SELECT 8`
- stask: `cols(attname:25, format_type:25, default:25, attnotnull:16, table_oid:20, comment:25) 8 rows ["number", "text", NULL, "t", "16430", NULL] ["carrier", "text", NULL, "t", "16430", NULL] ["status", "text", NULL, "t", "16430", NULL] ... tag=SELECT 8`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-columns:92 (feature)

```sql
SELECT pg_catalog.pg_class.relname, pg_catalog.pg_attribute.attname, pg_catalog.format_type(pg_catalog.pg_attribute.atttypid, pg_catalog.pg_attribute.atttypmod) AS format_type, (SELECT pg_catalog.pg_get_expr(pg_catalog.pg_attrdef.adbin, pg_catalog.pg_attrdef.adrelid) AS pg_get_expr_1 FROM pg_catalog.pg_attrdef WHERE pg_catalog.pg_attrdef.adrelid = pg_catalog.pg_attribute.attrelid AND pg_catalog.pg_attrdef.adnum = pg_catalog.pg_attribute.attnum AND pg_catalog.pg_attribute.atthasdef) AS "default", pg_catalog.pg_attribute.attnotnull AS not_null, pg_catalog.pg_class.relname AS table_name, pg_catalog.pg_description.description AS comment, pg_catalog.pg_attribute.attgenerated AS generated FROM pg_catalog.pg_class LEFT OUTER JOIN pg_catalog.pg_attribute ON pg_catalog.pg_class.oid = pg_catalog.pg_attribute.attrelid AND pg_catalog.pg_attribute.attnum > 0 AND NOT pg_catalog.pg_attribute.attisdropped LEFT OUTER JOIN pg_catalog.pg_description ON pg_catalog.pg_description.objoid = pg_catalog.pg_attribute.attrelid AND pg_catalog.pg_description.objsubid = pg_catalog.pg_attribute.attnum JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p', 'f', 'v', 'm']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' AND pg_catalog.pg_class.relname IN ('sims', 'orders', 'order_items') ORDER BY pg_catalog.pg_class.relname, pg_catalog.pg_attribute.attnum
```
- why: STASK rejects: 42601
- pg: `cols(relname:19, attname:19, format_type:25, default:25, not_null:16, table_name:19, comment:25, generated:18) 19 rows ["order_items", "id", "bigint", NULL, "t", "order_items", NULL, ""] ["order_items", "order_id", "bigint", NULL, "t", "order_items", NULL, ""] ["order_items", "sim_number", "text", NULL, "t", "order_items", NULL, ""] ... tag=SELECT 19`
- stask: `ERROR 42601: syntax error at or near "."`

### DIFF — g/ss-reflect-columns:93 (type)

```sql
SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod) AS ftype, a.attnotnull, a.atthasdef, a.attidentity, a.attgenerated FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'order_items'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum
```
- why: col 1 (attname) type OID pg=19 stask=25; col 5 (attidentity) type OID pg=18 stask=25; col 6 (attgenerated) type OID pg=18 stask=25
- pg: `cols(attname:19, ftype:25, attnotnull:16, atthasdef:16, attidentity:18, attgenerated:18) 5 rows ["id", "bigint", "t", "f", "", ""] ["order_id", "bigint", "t", "f", "", ""] ["sim_number", "text", "t", "f", "", ""] ... tag=SELECT 5`
- stask: `cols(attname:25, ftype:25, attnotnull:16, atthasdef:16, attidentity:25, attgenerated:25) 5 rows ["id", "bigint", "t", "f", "", ""] ["order_id", "bigint", "t", "f", "", ""] ["sim_number", "text", "t", "f", "", ""] ... tag=SELECT 5`

### DIFF — g/ss-reflect-columns:94 (type)

```sql
SELECT a.attname, a.atttypid, a.atttypmod, a.attlen, a.attnum, a.attndims FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'sims'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum
```
- why: col 1 (attname) type OID pg=19 stask=25; col 2 (atttypid) type OID pg=26 stask=20; col 3 (atttypmod) type OID pg=23 stask=20; col 4 (attlen) type OID pg=21 stask=20; col 5 (attnum) type OID pg=21 stask=20; col 6 (attndims) type OID pg=21 stask=20
- pg: `cols(attname:19, atttypid:26, atttypmod:23, attlen:21, attnum:21, attndims:21) 8 rows ["number", "25", "-1", "-1", "1", "0"] ["carrier", "25", "-1", "-1", "2", "0"] ["status", "25", "-1", "-1", "3", "0"] ... tag=SELECT 8`
- stask: `cols(attname:25, atttypid:20, atttypmod:20, attlen:20, attnum:20, attndims:20) 8 rows ["number", "25", "-1", "-1", "1", "0"] ["carrier", "25", "-1", "-1", "2", "0"] ["status", "25", "-1", "-1", "3", "0"] ... tag=SELECT 8`

### DIFF — g/ss-reflect-columns:95 (type)

```sql
SELECT t.typname, t.typcategory, t.typlen, t.typtype FROM pg_catalog.pg_type t WHERE t.oid IN (SELECT a.atttypid FROM pg_attribute a WHERE a.attrelid = 'sims'::regclass AND a.attnum > 0) ORDER BY t.typname
```
- why: col 1 (typname) type OID pg=19 stask=25; col 2 (typcategory) type OID pg=18 stask=25; col 3 (typlen) type OID pg=21 stask=20; col 4 (typtype) type OID pg=18 stask=25
- pg: `cols(typname:19, typcategory:18, typlen:21, typtype:18) 3 rows ["numeric", "N", "-1", "b"] ["text", "S", "-1", "b"] ["timestamptz", "D", "8", "b"] tag=SELECT 3`
- stask: `cols(typname:25, typcategory:25, typlen:20, typtype:25) 3 rows ["numeric", "N", "-1", "b"] ["text", "S", "-1", "b"] ["timestamptz", "D", "8", "b"] tag=SELECT 3`

### DIFF — g/ss-reflect-columns:97 (columns)

```sql
SELECT t.typname AS "name", pg_catalog.format_type(t.typbasetype, t.typtypmod) AS "attype", NOT t.typnotnull AS "nullable", t.typdefault AS "default", pg_catalog.pg_type_is_visible(t.oid) AS "visible", n.nspname AS "schema" FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace WHERE t.typtype = 'd' ORDER BY n.nspname, t.typname
```
- why: column count pg=6 stask=0; command tag pg="SELECT 5" stask="SELECT 0"
- pg: `cols(name:19, attype:25, nullable:16, default:25, visible:16, schema:19) 5 rows ["cardinal_number", "integer", "t", NULL, "f", "information_schema"] ["character_data", "character varying", "t", NULL, "f", "information_schema"] ["sql_identifier", "name", "t", NULL, "f", "information_schema"] ... tag=SELECT 5`
- stask: `tag=SELECT 0`

### DIFF — g/ss-reflect-columns:99 (type)

```sql
SELECT column_name, data_type, is_nullable, column_default, character_maximum_length, numeric_precision, numeric_scale FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'sims' ORDER BY ordinal_position
```
- why: col 1 (column_name) type OID pg=19 stask=25; col 2 (data_type) type OID pg=1043 stask=25; col 3 (is_nullable) type OID pg=1043 stask=25; col 4 (column_default) type OID pg=1043 stask=25; col 5 (character_maximum_length) type OID pg=23 stask=20; col 6 (numeric_precision) type OID pg=23 stask=20; col 7 (numeric_scale) type OID pg=23 stask=20
- pg: `cols(column_name:19, data_type:1043, is_nullable:1043, column_default:1043, character_maximum_length:23, numeric_precision:23, numeric_scale:23) 8 rows ["number", "text", "NO", NULL, NULL, NULL, NULL] ["carrier", "text", "NO", NULL, NULL, NULL, NULL] ["status", "text", "NO", NULL, NULL, NULL, NULL] ... tag=SELECT 8`
- stask: `cols(column_name:25, data_type:25, is_nullable:25, column_default:25, character_maximum_length:20, numeric_precision:20, numeric_scale:20) 8 rows ["number", "text", "NO", NULL, NULL, NULL, NULL] ["carrier", "text", "NO", NULL, NULL, NULL, NULL] ["status", "text", "NO", NULL, NULL, NULL, NULL] ... tag=SELECT 8`

### DIFF — g/ss-reflect-columns:100 (type)

```sql
SELECT column_name, data_type, udt_name FROM information_schema.columns WHERE table_name = 'orders' ORDER BY ordinal_position
```
- why: col 1 (column_name) type OID pg=19 stask=25; col 2 (data_type) type OID pg=1043 stask=25; col 3 (udt_name) type OID pg=19 stask=25
- pg: `cols(column_name:19, data_type:1043, udt_name:19) 6 rows ["id", "bigint", "int8"] ["customer", "text", "text"] ["channel", "text", "text"] ... tag=SELECT 6`
- stask: `cols(column_name:25, data_type:25, udt_name:25) 6 rows ["id", "bigint", "int8"] ["customer", "text", "text"] ["channel", "text", "text"] ... tag=SELECT 6`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-columns:101 (feature)

```sql
SELECT pg_catalog.pg_description.description FROM pg_catalog.pg_description WHERE pg_catalog.pg_description.objoid = 'sims'::regclass AND pg_catalog.pg_description.objsubid = 0
```
- why: STASK rejects: 42601
- pg: `cols(description:25) 1 rows ["SIM warehouse"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "."`

### DIFF — g/ss-reflect-columns:102 (type)

```sql
SELECT n.nspname, c.relname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod) FROM pg_attribute a JOIN pg_class c ON a.attrelid = c.oid JOIN pg_namespace n ON c.relnamespace = n.oid WHERE c.relname = 'sims' AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum
```
- why: col 1 (nspname) type OID pg=19 stask=25; col 2 (relname) type OID pg=19 stask=25; col 3 (attname) type OID pg=19 stask=25
- pg: `cols(nspname:19, relname:19, attname:19, format_type:25) 8 rows ["public", "sims", "number", "text"] ["public", "sims", "carrier", "text"] ["public", "sims", "status", "text"] ... tag=SELECT 8`
- stask: `cols(nspname:25, relname:25, attname:25, format_type:25) 8 rows ["public", "sims", "number", "text"] ["public", "sims", "carrier", "text"] ["public", "sims", "status", "text"] ... tag=SELECT 8`

### DIFF — g/ss-reflect-keys:108 (type)

```sql
SELECT a.attname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_index i ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey) WHERE i.indrelid = 'sims'::regclass AND i.indisprimary ORDER BY a.attnum
```
- why: col 1 (attname) type OID pg=19 stask=25
- pg: `cols(attname:19) 1 rows ["number"] tag=SELECT 1`
- stask: `cols(attname:25) 1 rows ["number"] tag=SELECT 1`

### DIFF — g/ss-reflect-keys:109 (type)

```sql
SELECT a.attname, a.attnum, c.conname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_constraint c ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey) WHERE c.contype = 'p' AND a.attrelid = 'sims'::regclass ORDER BY a.attnum
```
- why: col 1 (attname) type OID pg=19 stask=25; col 2 (attnum) type OID pg=21 stask=20; col 3 (conname) type OID pg=19 stask=25
- pg: `cols(attname:19, attnum:21, conname:19) 1 rows ["number", "1", "sims_pkey"] tag=SELECT 1`
- stask: `cols(attname:25, attnum:20, conname:25) 1 rows ["number", "1", "sims_pkey"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-keys:110 (feature)

```sql
SELECT pg_catalog.pg_attribute.attname AS cols, pg_catalog.pg_constraint.conname AS name, pg_catalog.pg_constraint.conkey AS colnums FROM pg_catalog.pg_attribute JOIN pg_catalog.pg_constraint ON pg_catalog.pg_attribute.attrelid = pg_catalog.pg_constraint.conrelid WHERE pg_catalog.pg_attribute.attrelid = 'orders'::regclass AND pg_catalog.pg_constraint.contype = 'p' AND pg_catalog.pg_attribute.attnum = ANY (pg_catalog.pg_constraint.conkey) ORDER BY pg_catalog.pg_attribute.attnum
```
- why: STASK rejects: 42601
- pg: `cols(cols:19, name:19, colnums:1005) 1 rows ["id", "orders_pkey", "{1}"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "."`

### DIFF — g/ss-reflect-keys:111 (type)

```sql
SELECT conname, pg_catalog.pg_get_constraintdef(oid, true) FROM pg_catalog.pg_constraint WHERE conrelid = 'sims'::regclass AND contype = 'p'
```
- why: col 1 (conname) type OID pg=19 stask=25
- pg: `cols(conname:19, pg_get_constraintdef:25) 1 rows ["sims_pkey", "PRIMARY KEY (number)"] tag=SELECT 1`
- stask: `cols(conname:25, pg_get_constraintdef:25) 1 rows ["sims_pkey", "PRIMARY KEY (number)"] tag=SELECT 1`

### DIFF — g/ss-reflect-keys:112 (type)

```sql
SELECT r.conname, pg_catalog.pg_get_constraintdef(r.oid, true) AS condef, n.nspname AS conschema FROM pg_catalog.pg_constraint r, pg_namespace n, pg_class c WHERE r.conrelid = 'order_items'::regclass AND r.contype = 'f' AND c.oid = confrelid AND n.oid = c.relnamespace ORDER BY 1
```
- why: col 1 (conname) type OID pg=19 stask=25; col 3 (conschema) type OID pg=19 stask=25
- pg: `cols(conname:19, condef:25, conschema:19) 1 rows ["order_items_order_fk", "FOREIGN KEY (order_id) REFERENCES orders(id)", "public"] tag=SELECT 1`
- stask: `cols(conname:25, condef:25, conschema:25) 1 rows ["order_items_order_fk", "FOREIGN KEY (order_id) REFERENCES orders(id)", "public"] tag=SELECT 1`

### DIFF — g/ss-reflect-keys:113 (type+value)

```sql
SELECT con.conname, con.confupdtype, con.confdeltype, con.confmatchtype, con.condeferrable, con.condeferred FROM pg_catalog.pg_constraint con WHERE con.conrelid = 'order_items'::regclass AND con.contype = 'f'
```
- why: col 1 (conname) type OID pg=19 stask=25; col 2 (confupdtype) type OID pg=18 stask=25; col 3 (confdeltype) type OID pg=18 stask=25; col 4 (confmatchtype) type OID pg=18 stask=25; row 1 col confmatchtype: pg="s" stask=" "
- pg: `cols(conname:19, confupdtype:18, confdeltype:18, confmatchtype:18, condeferrable:16, condeferred:16) 1 rows ["order_items_order_fk", "a", "a", "s", "f", "f"] tag=SELECT 1`
- stask: `cols(conname:25, confupdtype:25, confdeltype:25, confmatchtype:25, condeferrable:16, condeferred:16) 1 rows ["order_items_order_fk", "a", "a", " ", "f", "f"] tag=SELECT 1`

### DIFF — g/ss-reflect-keys:114 (type+value)

```sql
SELECT c.conname, c.contype, c.conrelid::regclass::text, c.confrelid::regclass::text FROM pg_catalog.pg_constraint c WHERE c.conrelid IN ('sims'::regclass, 'orders'::regclass, 'order_items'::regclass) ORDER BY c.conname
```
- why: col 1 (conname) type OID pg=19 stask=25; col 2 (contype) type OID pg=18 stask=25; row 1 col conrelid: pg="order_items" stask="16432"
- pg: `cols(conname:19, contype:18, conrelid:25, confrelid:25) 5 rows ["order_items_order_fk", "f", "order_items", "orders"] ["order_items_pkey", "p", "order_items", "-"] ["orders_pkey", "p", "orders", "-"] ... tag=SELECT 5`
- stask: `cols(conname:25, contype:25, conrelid:25, confrelid:25) 5 rows ["order_items_order_fk", "f", "16432", "16431"] ["order_items_pkey", "p", "16432", "0"] ["orders_pkey", "p", "16431", "0"] ... tag=SELECT 5`

### DIFF — g/ss-reflect-keys:115 (type)

```sql
SELECT i.relname AS relname, ix.indisunique, ix.indexprs, a.attname, a.attnum, c.conrelid IS NOT NULL AS has_con, ix.indkey::varchar, ix.indoption::varchar, i.reloptions, am.amname, pg_get_expr(ix.indpred, ix.indrelid), ix.indnkeyatts AS indnkeyatts FROM pg_class t JOIN pg_index ix ON t.oid = ix.indrelid JOIN pg_class i ON i.oid = ix.indexrelid LEFT OUTER JOIN pg_attribute a ON t.oid = a.attrelid AND a.attnum = ANY (ix.indkey) LEFT OUTER JOIN pg_constraint c ON (ix.indrelid = c.conrelid AND ix.indexrelid = c.conindid AND c.contype IN ('p', 'u', 'x')) LEFT OUTER JOIN pg_am am ON i.relam = am.oid WHERE t.relkind IN ('r', 'v', 'f', 'm', 'p') AND t.oid = 'sims'::regclass AND ix.indisprimary = 'f' ORDER BY t.relname, i.relname, a.attnum
```
- why: col 1 (relname) type OID pg=19 stask=25; col 3 (indexprs) type OID pg=194 stask=25; col 4 (attname) type OID pg=19 stask=25; col 5 (attnum) type OID pg=21 stask=20; col 10 (amname) type OID pg=19 stask=25; col 12 (indnkeyatts) type OID pg=21 stask=20
- pg: `cols(relname:19, indisunique:16, indexprs:194, attname:19, attnum:21, has_con:16, indkey:1043, indoption:1043, reloptions:1009, amname:19, pg_get_expr:25, indnkeyatts:21) 2 rows ["sims_carrier_status_idx", "f", NULL, "carrier", "2", "f", "2 3", "0 0", NULL, "btree", NULL, "2"] ["sims_carrier_status_idx", "f", NULL, "status", "3", "f", "2 3", "0 0", NULL, "btree", NULL, "2"] tag=SELECT 2`
- stask: `cols(relname:25, indisunique:16, indexprs:25, attname:25, attnum:20, has_con:16, indkey:1043, indoption:1043, reloptions:1009, amname:25, pg_get_expr:25, indnkeyatts:20) 2 rows ["sims_carrier_status_idx", "f", NULL, "carrier", "2", "f", "2 3", "0 0", NULL, "btree", NULL, "2"] ["sims_carrier_status_idx", "f", NULL, "status", "3", "f", "2 3", "0 0", NULL, "btree", NULL, "2"] tag=SELECT 2`

### DIFF — g/ss-reflect-keys:116 (type)

```sql
SELECT i.relname, ix.indisunique, ix.indisprimary, pg_catalog.pg_get_indexdef(ix.indexrelid) FROM pg_catalog.pg_index ix JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid WHERE ix.indrelid = 'sims'::regclass ORDER BY i.relname
```
- why: col 1 (relname) type OID pg=19 stask=25
- pg: `cols(relname:19, indisunique:16, indisprimary:16, pg_get_indexdef:25) 2 rows ["sims_carrier_status_idx", "f", "f", "CREATE INDEX sims_carrier_status_idx ON public.sims USING btree (carrier, status…"] ["sims_pkey", "t", "t", "CREATE UNIQUE INDEX sims_pkey ON public.sims USING btree (number)"] tag=SELECT 2`
- stask: `cols(relname:25, indisunique:16, indisprimary:16, pg_get_indexdef:25) 2 rows ["sims_carrier_status_idx", "f", "f", "CREATE INDEX sims_carrier_status_idx ON public.sims USING btree (carrier, status…"] ["sims_pkey", "t", "t", "CREATE UNIQUE INDEX sims_pkey ON public.sims USING btree (number)"] tag=SELECT 2`

### DIFF — g/ss-reflect-keys:117 (type)

```sql
SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'sims' ORDER BY indexname
```
- why: col 1 (indexname) type OID pg=19 stask=25
- pg: `cols(indexname:19, indexdef:25) 2 rows ["sims_carrier_status_idx", "CREATE INDEX sims_carrier_status_idx ON public.sims USING btree (carrier, status…"] ["sims_pkey", "CREATE UNIQUE INDEX sims_pkey ON public.sims USING btree (number)"] tag=SELECT 2`
- stask: `cols(indexname:25, indexdef:25) 2 rows ["sims_carrier_status_idx", "CREATE INDEX sims_carrier_status_idx ON public.sims USING btree (carrier, status…"] ["sims_pkey", "CREATE UNIQUE INDEX sims_pkey ON public.sims USING btree (number)"] tag=SELECT 2`

### DIFF — g/ss-reflect-keys:118 (type)

```sql
SELECT schemaname, tablename, indexname FROM pg_catalog.pg_indexes WHERE schemaname = 'public' ORDER BY indexname
```
- why: col 1 (schemaname) type OID pg=19 stask=25; col 2 (tablename) type OID pg=19 stask=25; col 3 (indexname) type OID pg=19 stask=25; command tag pg="SELECT 6" stask="SELECT 5"; row count pg=6 stask=5
- pg: `cols(schemaname:19, tablename:19, indexname:19) 6 rows ["public", "order_items", "order_items_pkey"] ["public", "orders", "orders_created_idx"] ["public", "orders", "orders_customer_id_uq"] ... tag=SELECT 6`
- stask: `cols(schemaname:25, tablename:25, indexname:25) 5 rows ["public", "order_items", "order_items_pkey"] ["public", "orders", "orders_customer_id_uq"] ["public", "orders", "orders_pkey"] ... tag=SELECT 5`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-keys:120 (feature)

```sql
SELECT pg_catalog.pg_constraint.conname AS name, pg_catalog.pg_get_constraintdef(pg_catalog.pg_constraint.oid, true) AS src FROM pg_catalog.pg_constraint WHERE pg_catalog.pg_constraint.conrelid = 'sims'::regclass AND pg_catalog.pg_constraint.contype = 'c' ORDER BY pg_catalog.pg_constraint.conname
```
- why: STASK rejects: 42601
- pg: `cols(name:19, src:25) 1 rows ["sims_price_ck", "CHECK (price >= 0::numeric)"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "."`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-keys:121 (semantics)

```sql
SELECT * FROM pg_catalog.pg_stats WHERE tablename = 'sims' AND attname = 'carrier'
```
- why: STASK rejects: 42P01
- pg: `cols(schemaname:19, tablename:19, attname:19, inherited:16, null_frac:700, avg_width:23, n_distinct:700, most_common_vals:2277, most_common_freqs:1021, histogram_bounds:2277, correlation:700, most_common_elems:2277, most_common_elem_freqs:1021, elem_count_histogram:1021) 1 rows ["public", "sims", "carrier", "f", "0", "10", "4", "{mobifone,vietnamobile,viettel,vinaphone}", "{0.25,0.25,0.25,0.25}", NULL, "0.25049958", NULL, NULL, NULL] tag=SELECT 1`
- stask: `ERROR 42P01: relation "pg_stats" does not exist`

### DIFF — g/ss-reflect-keys:122 (type+value)

```sql
SELECT relname, reltuples::bigint, relpages FROM pg_class WHERE relname = 'sims'
```
- why: col 1 (relname) type OID pg=19 stask=25; col 3 (relpages) type OID pg=23 stask=20; row 1 col reltuples: pg="3000" stask="-1"
- pg: `cols(relname:19, reltuples:20, relpages:23) 1 rows ["sims", "3000", "35"] tag=SELECT 1`
- stask: `cols(relname:25, reltuples:20, relpages:20) 1 rows ["sims", "-1", "0"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — g/ss-reflect-keys:123 (semantics)

```sql
SELECT n_live_tup FROM pg_stat_user_tables WHERE relname = 'sims'
```
- why: STASK rejects: 42P01
- pg: `cols(n_live_tup:20) 1 rows ["6000"] tag=SELECT 1`
- stask: `ERROR 42P01: relation "pg_stat_user_tables" does not exist`

### DIFF — g/ss-reflect-keys:124 (value)

```sql
SELECT pg_total_relation_size('sims') > 0
```
- why: row 1 col ?column?: pg="t" stask="f"
- pg: `cols(?column?:16) 1 rows ["t"] tag=SELECT 1`
- stask: `cols(?column?:16) 1 rows ["f"] tag=SELECT 1`

### DIFF — g/ss-reflect-keys:125 (value)

```sql
SELECT pg_relation_size('sims') > 0
```
- why: row 1 col ?column?: pg="t" stask="f"
- pg: `cols(?column?:16) 1 rows ["t"] tag=SELECT 1`
- stask: `cols(?column?:16) 1 rows ["f"] tag=SELECT 1`

### DIFF — g/ss-chart-basic:147 (type)

```sql
SELECT carrier, ROUND(AVG(price), 2) AS "ROUND(AVG(price))", ROUND(STDDEV_SAMP(price), 2) AS sd FROM sims GROUP BY carrier ORDER BY carrier
```
- why: col 3 (sd) type OID pg=1700 stask=701
- pg: `cols(carrier:25, ROUND(AVG(price)):1700, sd:1700) 4 rows ["mobifone", "2483024.50", "1450938.01"] ["vietnamobile", "2483692.17", "1450029.89"] ["viettel", "2480690.67", "1449477.19"] ... tag=SELECT 4`
- stask: `cols(carrier:25, ROUND(AVG(price)):1700, sd:701) 4 rows ["mobifone", "2483024.50", "1450938.01"] ["vietnamobile", "2483692.17", "1450029.89"] ["viettel", "2480690.67", "1449477.19"] ... tag=SELECT 4`

### ERR-ONLY-ONE-SIDE — g/ss-chart-basic:153 (semantics)

```sql
SELECT CASE WHEN price < 1000000 THEN 'low' WHEN price < 3000000 THEN 'mid' ELSE 'high' END AS bucket, COUNT(*) AS cnt FROM sims GROUP BY CASE WHEN price < 1000000 THEN 'low' WHEN price < 3000000 THEN 'mid' ELSE 'high' END ORDER BY bucket
```
- why: STASK rejects: 42803
- pg: `cols(bucket:25, cnt:20) 3 rows ["high", "1189"] ["low", "621"] ["mid", "1190"] tag=SELECT 3`
- stask: `ERROR 42803: column "price" must appear in the GROUP BY clause or be used in an aggregate function`

### ERR-ONLY-ONE-SIDE — g/ss-chart-basic:165 (feature)

```sql
SELECT carrier AS "carrier ""x""", COUNT(*) AS "my metric" FROM sims GROUP BY 1 ORDER BY 1 LIMIT 2
```
- why: STASK rejects: 42601
- pg: `cols(carrier "x":25, my metric:20) 2 rows ["mobifone", "750"] ["vietnamobile", "750"] tag=SELECT 2`
- stask: `ERROR 42601: syntax error at or near "x"`

### DIFF — g/ss-chart-time:184 (tag)

```sql
SELECT TO_CHAR(created, 'YYYY-"W"IW') AS yw, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 4
```
- why: command tag pg="SELECT 4" stask="SELECT 1"; row count pg=4 stask=1
- pg: `cols(yw:25, count:20) 4 rows ["2026-W01", "60"] ["2026-W02", "105"] ["2026-W03", "105"] ... tag=SELECT 4`
- stask: `cols(yw:25, count:20) 1 rows ["2026-WIW", "3000"] tag=SELECT 1`

### DIFF — g/ss-chart-time:190 (type+value)

```sql
SELECT extract(epoch from DATE_TRUNC('day', created)) * 1000 AS __timestamp, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1 LIMIT 3
```
- why: col 1 (__timestamp) type OID pg=1700 stask=701; row 1 col __timestamp: pg="1767225600000.000000" stask="1767225600000"
- pg: `cols(__timestamp:1700, count:20) 3 rows ["1767225600000.000000", "15"] ["1767312000000.000000", "15"] ["1767398400000.000000", "15"] tag=SELECT 3`
- stask: `cols(__timestamp:701, count:20) 3 rows ["1767225600000", "15"] ["1767312000000", "15"] ["1767398400000", "15"] tag=SELECT 3`

### DIFF — g/ss-virtual:281 (type+value)

```sql
SELECT o.channel, s.carrier, SUM(oi.qty * oi.unit_price) AS revenue FROM orders o JOIN order_items oi ON oi.order_id = o.id JOIN sims s ON s.number = oi.sim_number GROUP BY o.channel, s.carrier ORDER BY o.channel, s.carrier
```
- why: col 3 (revenue) type OID pg=1700 stask=701; row 1 col revenue: pg="208796188.50" stask="208796188.5"
- pg: `cols(channel:25, carrier:25, revenue:1700) 12 rows ["crm", "mobifone", "208796188.50"] ["crm", "vietnamobile", "204502187.50"] ["crm", "viettel", "203188186.00"] ... tag=SELECT 12`
- stask: `cols(channel:25, carrier:25, revenue:701) 12 rows ["crm", "mobifone", "208796188.5"] ["crm", "vietnamobile", "204502187.5"] ["crm", "viettel", "203188186"] ... tag=SELECT 12`

### DIFF — g/ss-analytic:297 (type)

```sql
SELECT m, c, SUM(c) OVER (ORDER BY m) AS running FROM (SELECT DATE_TRUNC('month', created) AS m, COUNT(*) AS c FROM sims GROUP BY 1) t ORDER BY m
```
- why: col 3 (running) type OID pg=1700 stask=20
- pg: `cols(m:1184, c:20, running:1700) 7 rows ["2026-01-01 00:00:00+00", "465", "465"] ["2026-02-01 00:00:00+00", "420", "885"] ["2026-03-01 00:00:00+00", "465", "1350"] ... tag=SELECT 7`
- stask: `cols(m:1184, c:20, running:20) 7 rows ["2026-01-01 00:00:00+00", "465", "465"] ["2026-02-01 00:00:00+00", "420", "885"] ["2026-03-01 00:00:00+00", "465", "1350"] ... tag=SELECT 7`

### DIFF — g/ss-analytic:298 (type)

```sql
SELECT carrier, m, SUM(c) OVER (PARTITION BY carrier ORDER BY m) AS running FROM (SELECT carrier, DATE_TRUNC('month', created) AS m, COUNT(*) AS c FROM sims GROUP BY 1, 2) t ORDER BY carrier, m
```
- why: col 3 (running) type OID pg=1700 stask=20
- pg: `cols(carrier:25, m:1184, running:1700) 28 rows ["mobifone", "2026-01-01 00:00:00+00", "120"] ["mobifone", "2026-02-01 00:00:00+00", "225"] ["mobifone", "2026-03-01 00:00:00+00", "345"] ... tag=SELECT 28`
- stask: `cols(carrier:25, m:1184, running:20) 28 rows ["mobifone", "2026-01-01 00:00:00+00", "120"] ["mobifone", "2026-02-01 00:00:00+00", "225"] ["mobifone", "2026-03-01 00:00:00+00", "345"] ... tag=SELECT 28`

### DIFF — g/ss-analytic:301 (type+value)

```sql
SELECT carrier, SUM(price) * 100.0 / SUM(SUM(price)) OVER () AS pct FROM sims GROUP BY carrier ORDER BY carrier
```
- why: col 2 (pct) type OID pg=1700 stask=701; row 1 col pct: pg="25.0117494295352706" stask="25.01174942953527"
- pg: `cols(carrier:25, pct:1700) 4 rows ["mobifone", "25.0117494295352706"] ["vietnamobile", "25.0184749013818511"] ["viettel", "24.9882404973666067"] ... tag=SELECT 4`
- stask: `cols(carrier:25, pct:701) 4 rows ["mobifone", "25.01174942953527"] ["vietnamobile", "25.01847490138185"] ["viettel", "24.988240497366608"] ... tag=SELECT 4`

### DIFF — g/ss-analytic:310 (type+value)

```sql
SELECT carrier, STDDEV(price) AS sd, VARIANCE(price) AS var, STDDEV_POP(price) AS sdp FROM sims GROUP BY carrier ORDER BY carrier
```
- why: col 2 (sd) type OID pg=1700 stask=701; col 3 (var) type OID pg=1700 stask=701; col 4 (sdp) type OID pg=1700 stask=701; row 1 col sd: pg="1450938.0089" stask="1450938.0089039786"
- pg: `cols(carrier:25, sd:1700, var:1700, sdp:1700) 4 rows ["mobifone", "1450938.0089", "2105221105682.2430", "1449970.3943"] ["vietnamobile", "1450029.8854", "2102586668655.0957", "1449062.8764"] ["viettel", "1449477.1889", "2100984121258.5670", "1448510.5485"] ... tag=SELECT 4`
- stask: `cols(carrier:25, sd:701, var:701, sdp:701) 4 rows ["mobifone", "1450938.0089039786", "2105221105682.2417", "1449970.394252241"] ["vietnamobile", "1450029.885435157", "2102586668655.0942", "1449062.8764010048"] ["viettel", "1449477.1889404005", "2100984121258.5652", "1448510.5484934817"] ... tag=SELECT 4`

### ERR-ONLY-ONE-SIDE — g/ss-analytic:311 (semantics)

```sql
SELECT carrier, CORR(price, EXTRACT(epoch FROM created)) IS NOT NULL AS has_corr FROM sims GROUP BY carrier ORDER BY carrier
```
- why: STASK rejects: 42803
- pg: `cols(carrier:25, has_corr:16) 4 rows ["mobifone", "t"] ["vietnamobile", "t"] ["viettel", "t"] ... tag=SELECT 4`
- stask: `ERROR 42803: column "price" must appear in the GROUP BY clause or be used in an aggregate function`

### PASS-MSG — g/ss-analytic:312 (message)

```sql
SELECT NTILE(4) OVER (ORDER BY price, number) AS q, COUNT(*) FROM sims GROUP BY 1 ORDER BY 1
```
- why: same SQLSTATE 42P20, message differs
- pg: `ERROR 42P20: window functions are not allowed in GROUP BY`
- stask: `ERROR 42P20: window functions are not allowed here`

### ERR-ONLY-ONE-SIDE — g/ss-analytic:320 (feature)

```sql
SELECT carrier, json_agg(status ORDER BY status) IS NOT NULL AS j FROM sims GROUP BY carrier ORDER BY carrier
```
- why: STASK rejects: 42883
- pg: `cols(carrier:25, j:16) 4 rows ["mobifone", "t"] ["vietnamobile", "t"] ["viettel", "t"] ... tag=SELECT 4`
- stask: `ERROR 42883: function json_agg does not exist`

### DIFF — g/ss-sqllab:328 (value)

```sql
EXPLAIN SELECT carrier, COUNT(*) FROM sims GROUP BY carrier
```
- why: row count pg=3 stask=2
- pg: `cols(QUERY PLAN:25) 3 rows ["HashAggregate (cost=80.00..80.04 rows=4 width=18)"] [" Group Key: carrier"] [" -> Seq Scan on sims (cost=0.00..65.00 rows=3000 width=10)"] tag=EXPLAIN`
- stask: `cols(QUERY PLAN:25) 2 rows ["Seq Scan on sims"] [" Group: facet over the rows found"] tag=EXPLAIN`

### DIFF — g/ss-sqllab:329 (value)

```sql
EXPLAIN SELECT * FROM sims WHERE number = '0900000001'
```
- why: row count pg=2 stask=1
- pg: `cols(QUERY PLAN:25) 2 rows ["Index Scan using sims_pkey on sims (cost=0.28..8.30 rows=1 width=59)"] [" Index Cond: (number = '0900000001'::text)"] tag=EXPLAIN`
- stask: `cols(QUERY PLAN:25) 1 rows ["Seq Scan on sims"] tag=EXPLAIN`

### DIFF — g/ss-sqllab:330 (type+value)

```sql
EXPLAIN (FORMAT JSON) SELECT COUNT(*) FROM sims
```
- why: col 1 (QUERY PLAN) type OID pg=114 stask=25; row 1 col QUERY PLAN: pg="[\n  {\n    \"Plan\": {\n      \"Node Type\": \"Aggregate\",\n      \"Strategy\": \"Plain\",\n …" stask="Seq Scan on sims"
- pg: `cols(QUERY PLAN:114) 1 rows ["[\n {\n \"Plan\": {\n \"Node Type\": \"Aggregate\",\n \"Strategy\": \"Plain\",\n …"] tag=EXPLAIN`
- stask: `cols(QUERY PLAN:25) 1 rows ["Seq Scan on sims"] tag=EXPLAIN`

### DIFF — g/ss-sqllab:331 (value)

```sql
EXPLAIN ANALYZE SELECT COUNT(*) FROM sims
```
- why: row count pg=4 stask=1
- pg: `cols(QUERY PLAN:25) 4 rows ["Aggregate (cost=72.50..72.51 rows=1 width=8) (actual time=1.088..1.089 rows=1 l…"] [" -> Seq Scan on sims (cost=0.00..65.00 rows=3000 width=0) (actual time=0.010.…"] ["Planning Time: 0.040 ms"] ... tag=EXPLAIN`
- stask: `cols(QUERY PLAN:25) 1 rows ["Seq Scan on sims"] tag=EXPLAIN`

### DIFF — g/ss-sqllab:338 (format)

```sql
SHOW search_path
```
- why: row 1 col search_path: pg="public, pg_catalog" stask="public , pg_catalog"
- pg: `cols(search_path:25) 1 rows ["public, pg_catalog"] tag=SHOW`
- stask: `cols(search_path:25) 1 rows ["public , pg_catalog"] tag=SHOW`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:340 (feature)

```sql
SHOW max_connections
```
- why: STASK rejects: 42704
- pg: `cols(max_connections:25) 1 rows ["100"] tag=SHOW`
- stask: `ERROR 42704: unrecognized configuration parameter "max_connections"`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:344 (feature)

```sql
WITH c AS MATERIALIZED (SELECT * FROM sims WHERE status = 'sold') SELECT COUNT(*) FROM c
```
- why: STASK rejects: 42601
- pg: `cols(count:20) 1 rows ["600"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "select"`

### PASS-MSG — g/ss-sqllab:351 (message)

```sql
SELECT carrier, COUNT(*) FROM sims
```
- why: same SQLSTATE 42803, message differs
- pg: `ERROR 42803: column "sims.carrier" must appear in the GROUP BY clause or be used in an aggregate function`
- stask: `ERROR 42803: column "carrier" must appear in the GROUP BY clause or be used in an aggregate function`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:362 (feature)

```sql
CREATE TEMP TABLE tmp_report AS SELECT carrier, COUNT(*) AS n FROM sims GROUP BY carrier
```
- why: STASK rejects: 0A000
- pg: `tag=SELECT 4`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:363 (semantics)

```sql
SELECT * FROM tmp_report ORDER BY carrier
```
- why: STASK rejects: 42P01
- pg: `cols(carrier:25, n:20) 4 rows ["mobifone", "750"] ["vietnamobile", "750"] ["viettel", "750"] ... tag=SELECT 4`
- stask: `ERROR 42P01: relation "tmp_report" does not exist`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:364 (feature)

```sql
CREATE TABLE report_snapshot AS SELECT carrier, COUNT(*) AS n FROM sims GROUP BY carrier
```
- why: STASK rejects: 42601
- pg: `tag=SELECT 4`
- stask: `ERROR 42601: syntax error at or near "as" (expected "(")`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:365 (semantics)

```sql
SELECT * FROM report_snapshot ORDER BY carrier
```
- why: STASK rejects: 42P01
- pg: `cols(carrier:25, n:20) 4 rows ["mobifone", "750"] ["vietnamobile", "750"] ["viettel", "750"] ... tag=SELECT 4`
- stask: `ERROR 42P01: relation "report_snapshot" does not exist`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:368 (feature)

```sql
SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind IN ('v', 'm') AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog' ORDER BY 1
```
- why: STASK rejects: 42601
- pg: `cols(relname:19) 1 rows ["v_sold"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "."`

### DIFF — g/ss-sqllab:369 (value)

```sql
SELECT definition FROM pg_catalog.pg_views WHERE viewname = 'v_sold'
```
- why: row 1 col definition: pg=" SELECT number,\n    carrier,\n    price\n   FROM sims\n  WHERE (status = 'sold'::te…" stask=" SELECT number, carrier, price FROM sims WHERE status = 'sold';"
- pg: `cols(definition:25) 1 rows [" SELECT number,\n carrier,\n price\n FROM sims\n WHERE (status = 'sold'::te…"] tag=SELECT 1`
- stask: `cols(definition:25) 1 rows [" SELECT number, carrier, price FROM sims WHERE status = 'sold';"] tag=SELECT 1`

### DIFF — g/ss-sqllab:370 (value)

```sql
SELECT pg_get_viewdef('v_sold'::regclass, true)
```
- why: row 1 col pg_get_viewdef: pg=" SELECT number,\n    carrier,\n    price\n   FROM sims\n  WHERE status = 'sold'::tex…" stask=" SELECT number, carrier, price FROM sims WHERE status = 'sold';"
- pg: `cols(pg_get_viewdef:25) 1 rows [" SELECT number,\n carrier,\n price\n FROM sims\n WHERE status = 'sold'::tex…"] tag=SELECT 1`
- stask: `cols(pg_get_viewdef:25) 1 rows [" SELECT number, carrier, price FROM sims WHERE status = 'sold';"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:372 (semantics)

```sql
DROP TABLE report_snapshot
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "report_snapshot" does not exist`

### ERR-ONLY-ONE-SIDE — g/ss-sqllab:374 (feature)

```sql
SELECT pg_sleep(0.05) IS NULL
```
- why: STASK rejects: 42883
- pg: `cols(?column?:16) 1 rows ["f"] tag=SELECT 1`
- stask: `ERROR 42883: function pg_sleep does not exist`

### DIFF — g/ss-sqllab:375 (value)

```sql
SELECT set_config('statement_timeout', '5000', false)
```
- why: row 1 col set_config: pg="5s" stask="5000"
- pg: `cols(set_config:25) 1 rows ["5s"] tag=SELECT 1`
- stask: `cols(set_config:25) 1 rows ["5000"] tag=SELECT 1`

### DIFF — g/ss-sqllab:377 (columns)

```sql
SELECT pid, state, query FROM pg_stat_activity WHERE pid = pg_backend_pid()
```
- why: column count pg=3 stask=0; command tag pg="SELECT 1" stask="SELECT 0"
- pg: `cols(pid:23, state:25, query:25) 1 rows ["140", "active", "SELECT pid, state, query FROM pg_stat_activity WHERE pid = pg_backend_pid()"] tag=SELECT 1`
- stask: `tag=SELECT 0`

### DIFF — g/ss-sqllab:378 (type)

```sql
SELECT datname FROM pg_database ORDER BY datname
```
- why: col 1 (datname) type OID pg=19 stask=25; command tag pg="SELECT 3" stask="SELECT 1"; row count pg=3 stask=1
- pg: `cols(datname:19) 3 rows ["postgres"] ["template0"] ["template1"] tag=SELECT 3`
- stask: `cols(datname:25) 1 rows ["stask"] tag=SELECT 1`

### DIFF — g/ss-sqllab:379 (type+value)

```sql
SELECT rolname FROM pg_roles WHERE rolname = current_user
```
- why: col 1 (rolname) type OID pg=19 stask=25; row 1 col rolname: pg="postgres" stask="stask"
- pg: `cols(rolname:19) 1 rows ["postgres"] tag=SELECT 1`
- stask: `cols(rolname:25) 1 rows ["stask"] tag=SELECT 1`

### DIFF — g/ss-sqllab:380 (type+value)

```sql
SELECT usename FROM pg_user WHERE usename = current_user
```
- why: col 1 (usename) type OID pg=19 stask=25; row 1 col usename: pg="postgres" stask="stask"
- pg: `cols(usename:19) 1 rows ["postgres"] tag=SELECT 1`
- stask: `cols(usename:25) 1 rows ["stask"] tag=SELECT 1`
