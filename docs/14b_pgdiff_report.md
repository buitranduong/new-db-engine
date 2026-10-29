# pgdiff report

859 statements in total: PASS 391, PASS-MSG 51 (same SQLSTATE, different message), DIFF 254, ERR-ONLY-ONE-SIDE 163

| group | statements | PASS | PASS-MSG | DIFF | ERR-ONLY-ONE-SIDE |
|---|---|---|---|---|---|
| a | 166 | 111 | 5 | 31 | 19 |
| b | 287 | 88 | 6 | 141 | 52 |
| c | 266 | 93 | 37 | 49 | 87 |
| d | 140 | 99 | 3 | 33 | 5 |

| verdict/class | count |
|---|---|
| DIFF/columns | 19 |
| DIFF/format | 1 |
| DIFF/sqlstate | 43 |
| DIFF/tag | 3 |
| DIFF/type | 105 |
| DIFF/type+value | 68 |
| DIFF/value | 15 |
| ERR-ONLY-ONE-SIDE/feature | 79 |
| ERR-ONLY-ONE-SIDE/semantics | 84 |
| PASS-MSG/message | 51 |

## Statements

### DIFF — a/liquibase-bootstrap:11 (type)

```sql
SELECT LOCKED, LOCKEDBY FROM databasechangeloglock WHERE ID = 1
```
- why: col 2 (lockedby) type OID pg=1043 stask=25
- pg: `cols(locked:16, lockedby:1043) 1 rows ["t", "host (10.0.0.1)"] tag=SELECT 1`
- stask: `cols(locked:16, lockedby:25) 1 rows ["t", "host (10.0.0.1)"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:12 (feature)

```sql
CREATE TABLE databasechangelog (ID VARCHAR(255) NOT NULL, AUTHOR VARCHAR(255) NOT NULL, FILENAME VARCHAR(255) NOT NULL, DATEEXECUTED TIMESTAMP WITHOUT TIME ZONE NOT NULL, ORDEREXECUTED INTEGER NOT NULL, EXECTYPE VARCHAR(10) NOT NULL, MD5SUM VARCHAR(35), DESCRIPTION VARCHAR(255), COMMENTS VARCHAR(255), TAG VARCHAR(255), LIQUIBASE VARCHAR(20), CONTEXTS VARCHAR(255), LABELS VARCHAR(255), DEPLOYMENT_ID VARCHAR(10))
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: table "databasechangelog" needs a single-column PRIMARY KEY`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:13 (semantics)

```sql
SELECT COUNT(*) FROM databasechangelog
```
- why: STASK rejects: 42P01
- pg: `cols(count:20) 1 rows ["0"] tag=SELECT 1`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:14 (semantics)

```sql
SELECT MAX(ORDEREXECUTED) FROM databasechangelog
```
- why: STASK rejects: 42P01
- pg: `cols(max:23) 1 rows [NULL] tag=SELECT 1`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:15 (semantics)

```sql
INSERT INTO databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('1', 'admin', 'db/changelog.xml', NOW(), 1, '9:abc', 'createTable tableName=sims', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890')
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 1`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:16 (semantics)

```sql
INSERT INTO databasechangelog (ID, AUTHOR, FILENAME, DATEEXECUTED, ORDEREXECUTED, MD5SUM, DESCRIPTION, COMMENTS, EXECTYPE, CONTEXTS, LABELS, LIQUIBASE, DEPLOYMENT_ID) VALUES ('2', 'admin', 'db/changelog.xml', NOW(), 2, '9:def', 'addColumn', '', 'EXECUTED', NULL, NULL, '4.27.0', '1234567890')
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 1`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:17 (semantics)

```sql
SELECT ID, AUTHOR, FILENAME, EXECTYPE, ORDEREXECUTED FROM databasechangelog ORDER BY ORDEREXECUTED ASC
```
- why: STASK rejects: 42P01
- pg: `cols(id:1043, author:1043, filename:1043, exectype:1043, orderexecuted:23) 2 rows ["1", "admin", "db/changelog.xml", "EXECUTED", "1"] ["2", "admin", "db/changelog.xml", "EXECUTED", "2"] tag=SELECT 2`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:18 (semantics)

```sql
SELECT MD5SUM, ID, AUTHOR, FILENAME FROM databasechangelog ORDER BY DATEEXECUTED ASC, ORDEREXECUTED ASC
```
- why: STASK rejects: 42P01
- pg: `cols(md5sum:1043, id:1043, author:1043, filename:1043) 2 rows ["9:abc", "1", "admin", "db/changelog.xml"] ["9:def", "2", "admin", "db/changelog.xml"] tag=SELECT 2`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:19 (semantics)

```sql
UPDATE databasechangelog SET MD5SUM = '9:new' WHERE ID = '1' AND AUTHOR = 'admin' AND FILENAME = 'db/changelog.xml'
```
- why: STASK rejects: 42P01
- pg: `tag=UPDATE 1`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### ERR-ONLY-ONE-SIDE — a/liquibase-bootstrap:20 (semantics)

```sql
DELETE FROM databasechangelog WHERE ID = '2' AND AUTHOR = 'admin' AND FILENAME = 'db/changelog.xml'
```
- why: STASK rejects: 42P01
- pg: `tag=DELETE 1`
- stask: `ERROR 42P01: relation "databasechangelog" does not exist`

### DIFF — a/create-table-constraints:28 (type+value)

```sql
SELECT id, name, qty, price, active, created IS NOT NULL AS has_created FROM pd_item
```
- why: col 2 (name) type OID pg=1043 stask=25; col 3 (qty) type OID pg=23 stask=20; col 4 (price) type OID pg=1700 stask=701; row 1 col price: pg="0.00" stask="0"
- pg: `cols(id:20, name:1043, qty:23, price:1700, active:16, has_created:16) 1 rows ["1", "a", "0", "0.00", "t", "t"] tag=SELECT 1`
- stask: `cols(id:20, name:25, qty:20, price:701, active:16, has_created:16) 1 rows ["1", "a", "0", "0", "t", "t"] tag=SELECT 1`

### DIFF — a/create-table-constraints:30 (type+value)

```sql
SELECT id, qty, price, active FROM pd_item ORDER BY id
```
- why: col 2 (qty) type OID pg=23 stask=20; col 3 (price) type OID pg=1700 stask=701; row 1 col price: pg="0.00" stask="0"
- pg: `cols(id:20, qty:23, price:1700, active:16) 2 rows ["1", "0", "0.00", "t"] ["2", "5", "9.50", "f"] tag=SELECT 2`
- stask: `cols(id:20, qty:20, price:701, active:16) 2 rows ["1", "0", "0", "t"] ["2", "5", "9.5", "f"] tag=SELECT 2`

### DIFF — a/create-table-constraints:31 (type+value)

```sql
SELECT price FROM pd_item WHERE id = 2
```
- why: col 1 (price) type OID pg=1700 stask=701; row 1 col price: pg="9.50" stask="9.5"
- pg: `cols(price:1700) 1 rows ["9.50"] tag=SELECT 1`
- stask: `cols(price:701) 1 rows ["9.5"] tag=SELECT 1`

### DIFF — a/create-table-constraints:32 (value)

```sql
SELECT pg_typeof(price)::text, pg_typeof(created)::text, pg_typeof(active)::text, pg_typeof(id)::text FROM pd_item WHERE id = 1
```
- why: row 1 col pg_typeof: pg="numeric" stask="double precision"
- pg: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["numeric", "timestamp without time zone", "boolean", "bigint"] tag=SELECT 1`
- stask: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["double precision", "timestamptz", "boolean", "bigint"] tag=SELECT 1`

### PASS-MSG — a/create-table-constraints:34 (message)

```sql
INSERT INTO pd_item (id, name) VALUES (1, 'dup')
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "pk_pd_item"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "pd_item_pkey": Key (id)=(1) already exists`

### ERR-ONLY-ONE-SIDE — a/create-table-constraints:37 (semantics)

```sql
INSERT INTO pd_item (id, name, price) VALUES (6, 'f', 12345678901.99)
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: numeric field overflow`
- stask: `tag=INSERT 0 1`

### DIFF — a/create-table-constraints:38 (columns)

```sql
SELECT price FROM pd_item WHERE id = 6
```
- why: column count pg=0 stask=1; command tag pg="SELECT 0" stask="SELECT 1"
- pg: `tag=SELECT 0`
- stask: `cols(price:701) 1 rows ["12345678901.99"] tag=SELECT 1`

### DIFF — a/create-table-constraints:40 (type+value)

```sql
SELECT price FROM pd_item WHERE id = 7
```
- why: col 1 (price) type OID pg=1700 stask=701; row 1 col price: pg="1.01" stask="1.005"
- pg: `cols(price:1700) 1 rows ["1.01"] tag=SELECT 1`
- stask: `cols(price:701) 1 rows ["1.005"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — a/create-table-constraints:41 (semantics)

```sql
INSERT INTO pd_item (id, name, price) VALUES (8, 'h', 99999999999.99)
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: numeric field overflow`
- stask: `tag=INSERT 0 1`

### ERR-ONLY-ONE-SIDE — a/create-table-constraints:42 (semantics)

```sql
INSERT INTO pd_item (id, name) VALUES (9, repeat('x', 101))
```
- why: STASK accepts, PostgreSQL rejects: 22001
- pg: `ERROR 22001: value too long for type character varying(100)`
- stask: `tag=INSERT 0 1`

### DIFF — a/create-table-constraints:44 (type)

```sql
SELECT length(name) FROM pd_item WHERE id = 10
```
- why: col 1 (length) type OID pg=23 stask=20
- pg: `cols(length:23) 1 rows ["100"] tag=SELECT 1`
- stask: `cols(length:20) 1 rows ["100"] tag=SELECT 1`

### DIFF — a/alter-table:51 (type)

```sql
SELECT id, w FROM pd_alt ORDER BY id
```
- why: col 2 (w) type OID pg=23 stask=20
- pg: `cols(id:20, w:23) 2 rows ["1", "7"] ["2", "7"] tag=SELECT 2`
- stask: `cols(id:20, w:20) 2 rows ["1", "7"] ["2", "7"] tag=SELECT 2`

### DIFF — a/alter-table:59 (type)

```sql
SELECT id, w FROM pd_alt WHERE id = 4
```
- why: col 2 (w) type OID pg=23 stask=20
- pg: `cols(id:20, w:23) 1 rows ["4", "9"] tag=SELECT 1`
- stask: `cols(id:20, w:20) 1 rows ["4", "9"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — a/alter-table:67 (feature)

```sql
ALTER TABLE pd_alt ALTER COLUMN w TYPE BIGINT
```
- why: STASK rejects: 0A000
- pg: `tag=ALTER TABLE`
- stask: `ERROR 0A000: ALTER TABLE ... SET TYPE is not supported yet`

### ERR-ONLY-ONE-SIDE — a/alter-table:68 (feature)

```sql
ALTER TABLE pd_alt ALTER COLUMN w TYPE TEXT USING w::text
```
- why: STASK rejects: 0A000
- pg: `tag=ALTER TABLE`
- stask: `ERROR 0A000: ALTER TABLE ... SET TYPE is not supported yet`

### DIFF — a/insert-returning:81 (type+value)

```sql
INSERT INTO pd_ret (name, score) VALUES ('a', 1.5) RETURNING id, name, score
```
- why: col 3 (score) type OID pg=1700 stask=701; row 1 col score: pg="1.50" stask="1.5"
- pg: `cols(id:20, name:25, score:1700) 1 rows ["1", "a", "1.50"] tag=INSERT 0 1`
- stask: `cols(id:20, name:25, score:701) 1 rows ["1", "a", "1.5"] tag=INSERT 0 1`

### DIFF — a/insert-returning:84 (type+value)

```sql
INSERT INTO pd_ret (name) VALUES ('e') RETURNING *
```
- why: col 3 (score) type OID pg=1700 stask=701; row 1 col created: pg="2026-10-29 14:17:57.314196+00" stask="2026-10-29 14:17:57.315+00"
- pg: `cols(id:20, name:25, score:1700, created:1184) 1 rows ["5", "e", NULL, "2026-10-29 14:17:57.314196+00"] tag=INSERT 0 1`
- stask: `cols(id:20, name:25, score:701, created:1184) 1 rows ["5", "e", NULL, "2026-10-29 14:17:57.315+00"] tag=INSERT 0 1`

### DIFF — a/insert-returning:90 (type)

```sql
INSERT INTO pd_ret (name, score) VALUES ($1, $2) RETURNING id, name, score
```
args: "z", "3.25"

- why: col 3 (score) type OID pg=1700 stask=701
- pg: `cols(id:20, name:25, score:1700) 1 rows ["9", "z", "3.25"] tag=INSERT 0 1`
- stask: `cols(id:20, name:25, score:701) 1 rows ["9", "z", "3.25"] tag=INSERT 0 1`

### DIFF — a/insert-returning:92 (type)

```sql
INSERT INTO pd_ret (name, score) VALUES ($1, $2::numeric) RETURNING name, score
```
args: "y", NULL

- why: col 2 (score) type OID pg=1700 stask=701
- pg: `cols(name:25, score:1700) 1 rows ["y", NULL] tag=INSERT 0 1`
- stask: `cols(name:25, score:701) 1 rows ["y", NULL] tag=INSERT 0 1`

### DIFF — a/on-conflict:101 (type)

```sql
SELECT * FROM pd_oc
```
- why: col 1 (id) type OID pg=23 stask=20; col 3 (n) type OID pg=23 stask=20
- pg: `cols(id:23, v:25, n:23) 1 rows ["1", "a", "1"] tag=SELECT 1`
- stask: `cols(id:20, v:25, n:20) 1 rows ["1", "a", "1"] tag=SELECT 1`

### DIFF — a/on-conflict:103 (type)

```sql
SELECT * FROM pd_oc
```
- why: col 1 (id) type OID pg=23 stask=20; col 3 (n) type OID pg=23 stask=20
- pg: `cols(id:23, v:25, n:23) 1 rows ["1", "b", "3"] tag=SELECT 1`
- stask: `cols(id:20, v:25, n:20) 1 rows ["1", "b", "3"] tag=SELECT 1`

### DIFF — a/on-conflict:104 (type)

```sql
INSERT INTO pd_oc VALUES (1, 'c', 3) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v RETURNING id, v, n
```
- why: col 1 (id) type OID pg=23 stask=20; col 3 (n) type OID pg=23 stask=20
- pg: `cols(id:23, v:25, n:23) 1 rows ["1", "c", "3"] tag=INSERT 0 1`
- stask: `cols(id:20, v:25, n:20) 1 rows ["1", "c", "3"] tag=INSERT 0 1`

### DIFF — a/on-conflict:105 (type)

```sql
INSERT INTO pd_oc VALUES (2, 'x', 1) ON CONFLICT (id) DO UPDATE SET v = EXCLUDED.v RETURNING id, v, n
```
- why: col 1 (id) type OID pg=23 stask=20; col 3 (n) type OID pg=23 stask=20
- pg: `cols(id:23, v:25, n:23) 1 rows ["2", "x", "1"] tag=INSERT 0 1`
- stask: `cols(id:20, v:25, n:20) 1 rows ["2", "x", "1"] tag=INSERT 0 1`

### DIFF — a/on-conflict:112 (type)

```sql
SELECT id, v FROM pd_oc ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, v:25) 4 rows ["1", "c"] ["2", "x"] ["4", "m"] ... tag=SELECT 4`
- stask: `cols(id:20, v:25) 4 rows ["1", "c"] ["2", "x"] ["4", "m"] ... tag=SELECT 4`

### ERR-ONLY-ONE-SIDE — a/on-conflict:113 (semantics)

```sql
INSERT INTO pd_oc VALUES (1, 'q', 1) ON CONFLICT (v) DO NOTHING
```
- why: STASK accepts, PostgreSQL rejects: 42P10
- pg: `ERROR 42P10: there is no unique or exclusion constraint matching the ON CONFLICT specification`
- stask: `tag=INSERT 0 0`

### PASS-MSG — a/on-conflict:114 (message)

```sql
INSERT INTO pd_oc VALUES (1, 'q', 1)
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_oc_pkey"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "pd_oc_pkey": Key (id)=(1) already exists`

### DIFF — a/update-from:123 (type)

```sql
SELECT * FROM pd_u1 ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20; col 3 (n) type OID pg=23 stask=20
- pg: `cols(id:23, v:25, n:23) 3 rows ["1", "x", "10"] ["2", "b", "20"] ["3", "z", "30"] tag=SELECT 3`
- stask: `cols(id:20, v:25, n:20) 3 rows ["1", "x", "10"] ["2", "b", "20"] ["3", "z", "30"] tag=SELECT 3`

### ERR-ONLY-ONE-SIDE — a/update-from:124 (semantics)

```sql
UPDATE pd_u1 t SET n = t.n + 1 FROM pd_u2 s WHERE t.id = s.id RETURNING t.id, t.n, s.w
```
- why: STASK rejects: 42P01
- pg: `cols(id:23, n:23, w:25) 2 rows ["1", "11", "x"] ["3", "31", "z"] tag=UPDATE 2`
- stask: `ERROR 42P01: missing FROM-clause entry for table "t"`

### DIFF — a/update-from:125 (type+value)

```sql
UPDATE pd_u1 SET n = n * 2 WHERE id IN (SELECT id FROM pd_u2) RETURNING id, n
```
- why: col 1 (id) type OID pg=23 stask=20; col 2 (n) type OID pg=23 stask=20; row 1 col n: pg="22" stask="20"
- pg: `cols(id:23, n:23) 2 rows ["1", "22"] ["3", "62"] tag=UPDATE 2`
- stask: `cols(id:20, n:20) 2 rows ["1", "20"] ["3", "60"] tag=UPDATE 2`

### ERR-ONLY-ONE-SIDE — a/update-from:128 (feature)

```sql
UPDATE pd_u1 SET (v, n) = ('p', 7) WHERE id = 1
```
- why: STASK rejects: 42601
- pg: `tag=UPDATE 1`
- stask: `ERROR 42601: syntax error at or near "(" (expected an identifier)`

### DIFF — a/update-from:129 (type+value)

```sql
SELECT * FROM pd_u1 WHERE id = 1
```
- why: col 1 (id) type OID pg=23 stask=20; col 3 (n) type OID pg=23 stask=20; row 1 col v: pg="p" stask="x"
- pg: `cols(id:23, v:25, n:23) 1 rows ["1", "p", "7"] tag=SELECT 1`
- stask: `cols(id:20, v:25, n:20) 1 rows ["1", "x", "20"] tag=SELECT 1`

### PASS-MSG — a/update-from:131 (message)

```sql
UPDATE pd_u1 SET n = 'abc'
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "abc"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "abc"`

### PASS-MSG — a/update-from:132 (message)

```sql
UPDATE pd_u1 SET id = 2 WHERE id = 1
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_u1_pkey"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "pd_u1_pkey": Key (id)=(2) already exists`

### DIFF — a/delete-using:140 (type)

```sql
DELETE FROM pd_d1 USING pd_d2 WHERE pd_d1.grp = pd_d2.grp AND pd_d2.bad RETURNING pd_d1.id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 3 rows ["1"] ["2"] ["4"] tag=DELETE 3`
- stask: `cols(id:20) 3 rows ["1"] ["2"] ["4"] tag=DELETE 3`

### DIFF — a/delete-using:141 (type)

```sql
SELECT id FROM pd_d1 ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 1 rows ["3"] tag=SELECT 1`
- stask: `cols(id:20) 1 rows ["3"] tag=SELECT 1`

### DIFF — a/delete-using:142 (type)

```sql
DELETE FROM pd_d1 WHERE id = 3 RETURNING *
```
- why: col 1 (id) type OID pg=23 stask=20; col 2 (grp) type OID pg=23 stask=20
- pg: `cols(id:23, grp:23) 1 rows ["3", "2"] tag=DELETE 1`
- stask: `cols(id:20, grp:20) 1 rows ["3", "2"] tag=DELETE 1`

### DIFF — a/serial-and-identity:154 (type)

```sql
SELECT id, v FROM pd_ser ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, v:25) 2 rows ["1", "a"] ["2", "b"] tag=SELECT 2`
- stask: `cols(id:20, v:25) 2 rows ["1", "a"] ["2", "b"] tag=SELECT 2`

### DIFF — a/serial-and-identity:156 (type)

```sql
INSERT INTO pd_ser (v) VALUES ('d') RETURNING id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 1 rows ["3"] tag=INSERT 0 1`
- stask: `cols(id:20) 1 rows ["3"] tag=INSERT 0 1`

### PASS-MSG — a/transaction-basics:172 (message)

```sql
INSERT INTO pd_tx VALUES (1, 2)
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_tx_pkey"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "pd_tx_pkey": Key (id)=(1) already exists`

### DIFF — a/transaction-basics:184 (sqlstate)

```sql
SAVEPOINT s1
```
- why: SQLSTATE pg=25P01 stask=0A000
- pg: `ERROR 25P01: SAVEPOINT can only be used in transaction blocks`
- stask: `ERROR 0A000: statement "SAVEPOINT" is not supported`

### ERR-ONLY-ONE-SIDE — a/transaction-basics:187 (feature)

```sql
SAVEPOINT s1
```
- why: STASK rejects: 0A000
- pg: `tag=SAVEPOINT`
- stask: `ERROR 0A000: statement "SAVEPOINT" is not supported`

### ERR-ONLY-ONE-SIDE — a/transaction-basics:189 (feature)

```sql
ROLLBACK TO SAVEPOINT s1
```
- why: STASK rejects: 42601
- pg: `tag=ROLLBACK`
- stask: `ERROR 42601: syntax error at or near "to"`

### DIFF — a/transaction-basics:191 (type)

```sql
SELECT id FROM pd_tx ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20; command tag pg="SELECT 2" stask="SELECT 3"; row count pg=2 stask=3
- pg: `cols(id:23) 2 rows ["2"] ["3"] tag=SELECT 2`
- stask: `cols(id:20) 3 rows ["2"] ["3"] ["4"] tag=SELECT 3`

### DIFF — a/transaction-basics:194 (type)

```sql
SELECT 1
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### DIFF — a/transaction-basics:196 (value)

```sql
SHOW transaction_isolation
```
- why: row 1 col transaction_isolation: pg="read committed" stask="repeatable read"
- pg: `cols(transaction_isolation:25) 1 rows ["read committed"] tag=SHOW`
- stask: `cols(transaction_isolation:25) 1 rows ["repeatable read"] tag=SHOW`

### ERR-ONLY-ONE-SIDE — b/arithmetic:4 (feature)

```sql
SELECT 1 + 2, 5 - 7, 3 * 4, 7 / 2, 7 % 3, -7 % 3, 2 ^ 10
```
- why: STASK rejects: 42601
- pg: `cols(?column?:23, ?column?:23, ?column?:23, ?column?:23, ?column?:23, ?column?:23, ?column?:701) 1 rows ["3", "-2", "12", "3", "1", "-1", "1024"] tag=SELECT 1`
- stask: `ERROR 42601: unexpected character '^'`

### DIFF — b/arithmetic:5 (type+value)

```sql
SELECT 7 / 2.0, 7.0 / 2, 1 / 3.0
```
- why: col 1 (?column?) type OID pg=1700 stask=701; col 2 (?column?) type OID pg=1700 stask=701; col 3 (?column?) type OID pg=1700 stask=701; row 1 col ?column?: pg="3.5000000000000000" stask="3.5"
- pg: `cols(?column?:1700, ?column?:1700, ?column?:1700) 1 rows ["3.5000000000000000", "3.5000000000000000", "0.33333333333333333333"] tag=SELECT 1`
- stask: `cols(?column?:701, ?column?:701, ?column?:701) 1 rows ["3.5", "3.5", "0.3333333333333333"] tag=SELECT 1`

### DIFF — b/arithmetic:6 (type+value)

```sql
SELECT 0.1 + 0.2, 0.1::float8 + 0.2::float8
```
- why: col 1 (?column?) type OID pg=1700 stask=701; row 1 col ?column?: pg="0.3" stask="0.30000000000000004"
- pg: `cols(?column?:1700, ?column?:701) 1 rows ["0.3", "0.30000000000000004"] tag=SELECT 1`
- stask: `cols(?column?:701, ?column?:701) 1 rows ["0.30000000000000004", "0.30000000000000004"] tag=SELECT 1`

### PASS-MSG — b/arithmetic:7 (message)

```sql
SELECT 9223372036854775807::bigint, -9223372036854775808::bigint
```
- why: same SQLSTATE 22003, message differs
- pg: `ERROR 22003: bigint out of range`
- stask: `ERROR 22003: value 9.223372036854776e+18 is not an integer`

### ERR-ONLY-ONE-SIDE — b/arithmetic:8 (semantics)

```sql
SELECT 2147483647 + 1
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: integer out of range`
- stask: `cols(?column?:20) 1 rows ["2147483648"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/arithmetic:9 (semantics)

```sql
SELECT 9223372036854775807 + 1
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: bigint out of range`
- stask: `cols(?column?:20) 1 rows ["-9223372036854775808"] tag=SELECT 1`

### DIFF — b/arithmetic:10 (value)

```sql
SELECT pg_typeof(1)::text, pg_typeof(1.5)::text, pg_typeof(1::bigint)::text, pg_typeof(1::smallint + 1)::text
```
- why: row 1 col pg_typeof: pg="integer" stask="bigint"
- pg: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["integer", "numeric", "bigint", "integer"] tag=SELECT 1`
- stask: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["bigint", "double precision", "bigint", "bigint"] tag=SELECT 1`

### DIFF — b/arithmetic:11 (type)

```sql
SELECT abs(-5), abs(-5.5), sign(-3), ceil(1.2), floor(-1.2), round(2.5), round(3.5), round(2.567, 2), trunc(2.99), trunc(-2.99)
```
- why: col 1 (abs) type OID pg=23 stask=20; col 2 (abs) type OID pg=1700 stask=701; col 3 (sign) type OID pg=701 stask=20; col 4 (ceil) type OID pg=1700 stask=701; col 5 (floor) type OID pg=1700 stask=701; col 6 (round) type OID pg=1700 stask=701; col 7 (round) type OID pg=1700 stask=701; col 8 (round) type OID pg=1700 stask=701; col 9 (trunc) type OID pg=1700 stask=701; col 10 (trunc) type OID pg=1700 stask=701
- pg: `cols(abs:23, abs:1700, sign:701, ceil:1700, floor:1700, round:1700, round:1700, round:1700, trunc:1700, trunc:1700) 1 rows ["5", "5.5", "-1", "2", "-2", "3", "4", "2.57", "2", "-2"] tag=SELECT 1`
- stask: `cols(abs:20, abs:701, sign:20, ceil:701, floor:701, round:701, round:701, round:701, trunc:701, trunc:701) 1 rows ["5", "5.5", "-1", "2", "-2", "3", "4", "2.57", "2", "-2"] tag=SELECT 1`

### DIFF — b/arithmetic:12 (type+value)

```sql
SELECT sqrt(16), sqrt(2), power(2, 8), power(2.0, 0.5), exp(0), ln(1), log(100), mod(10, 3), div(7, 2)
```
- why: col 4 (power) type OID pg=1700 stask=701; col 8 (mod) type OID pg=23 stask=20; col 9 (div) type OID pg=1700 stask=20; row 1 col power: pg="1.4142135623730950" stask="1.4142135623730951"
- pg: `cols(sqrt:701, sqrt:701, power:701, power:1700, exp:701, ln:701, log:701, mod:23, div:1700) 1 rows ["4", "1.4142135623730951", "256", "1.4142135623730950", "1", "0", "2", "1", "3"] tag=SELECT 1`
- stask: `cols(sqrt:701, sqrt:701, power:701, power:701, exp:701, ln:701, log:701, mod:20, div:20) 1 rows ["4", "1.4142135623730951", "256", "1.4142135623730951", "1", "0", "2", "1", "3"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/arithmetic:13 (feature)

```sql
SELECT 5 & 3, 5 | 3, 5 # 3, ~5, 1 << 4, 256 >> 2
```
- why: STASK rejects: 42601
- pg: `cols(?column?:23, ?column?:23, ?column?:23, ?column?:23, ?column?:23, ?column?:23) 1 rows ["1", "7", "6", "-6", "16", "64"] tag=SELECT 1`
- stask: `ERROR 42601: unexpected character '|'`

### DIFF — b/arithmetic:14 (type)

```sql
SELECT greatest(1, 5, 3), least(4, 2, 9), greatest(1, NULL, 3), least('b', 'a')
```
- why: col 1 (greatest) type OID pg=23 stask=20; col 2 (least) type OID pg=23 stask=20; col 3 (greatest) type OID pg=23 stask=20
- pg: `cols(greatest:23, least:23, greatest:23, least:25) 1 rows ["5", "2", "3", "a"] tag=SELECT 1`
- stask: `cols(greatest:20, least:20, greatest:20, least:25) 1 rows ["5", "2", "3", "a"] tag=SELECT 1`

### DIFF — b/arithmetic:15 (type+value)

```sql
SELECT 1.0::numeric, 1.50::numeric, 100::numeric(5,2), 1e3::numeric, 1.23e-4::numeric
```
- why: col 1 name pg="numeric" stask="?column?"; col 1 (numeric) type OID pg=1700 stask=701; col 2 name pg="numeric" stask="?column?"; col 2 (numeric) type OID pg=1700 stask=701; col 3 name pg="numeric" stask="?column?"; col 3 (numeric) type OID pg=1700 stask=701; col 4 name pg="numeric" stask="?column?"; col 4 (numeric) type OID pg=1700 stask=701; col 5 name pg="numeric" stask="?column?"; col 5 (numeric) type OID pg=1700 stask=701; row 1 col numeric: pg="1.0" stask="1"
- pg: `cols(numeric:1700, numeric:1700, numeric:1700, numeric:1700, numeric:1700) 1 rows ["1.0", "1.50", "100.00", "1000", "0.000123"] tag=SELECT 1`
- stask: `cols(?column?:701, ?column?:701, ?column?:701, ?column?:701, ?column?:701) 1 rows ["1", "1.5", "100", "1000", "0.000123"] tag=SELECT 1`

### DIFF — b/arithmetic:16 (type+value)

```sql
SELECT 12345678901234567890.123456789::numeric
```
- why: col 1 name pg="numeric" stask="?column?"; col 1 (numeric) type OID pg=1700 stask=701; row 1 col numeric: pg="12345678901234567890.123456789" stask="1.2345678901234567e+19"
- pg: `cols(numeric:1700) 1 rows ["12345678901234567890.123456789"] tag=SELECT 1`
- stask: `cols(?column?:701) 1 rows ["1.2345678901234567e+19"] tag=SELECT 1`

### DIFF — b/arithmetic:17 (type+value)

```sql
SELECT (0.1::numeric + 0.2::numeric), 1.1::numeric * 1.1::numeric, 10::numeric / 3
```
- why: col 1 (?column?) type OID pg=1700 stask=701; col 2 (?column?) type OID pg=1700 stask=701; col 3 (?column?) type OID pg=1700 stask=701; row 1 col ?column?: pg="0.3" stask="0.30000000000000004"
- pg: `cols(?column?:1700, ?column?:1700, ?column?:1700) 1 rows ["0.3", "1.21", "3.3333333333333333"] tag=SELECT 1`
- stask: `cols(?column?:701, ?column?:701, ?column?:701) 1 rows ["0.30000000000000004", "1.2100000000000002", "3.3333333333333335"] tag=SELECT 1`

### DIFF — b/arithmetic:18 (type+value)

```sql
SELECT 123456789012345678901234567890::numeric * 10
```
- why: col 1 (?column?) type OID pg=1700 stask=701; row 1 col ?column?: pg="1234567890123456789012345678900" stask="1.2345678901234568e+30"
- pg: `cols(?column?:1700) 1 rows ["1234567890123456789012345678900"] tag=SELECT 1`
- stask: `cols(?column?:701) 1 rows ["1.2345678901234568e+30"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/arithmetic:19 (semantics)

```sql
SELECT 1e308::float8 * 10, -1e308::float8 * 10, 'NaN'::float8, 'Infinity'::float8
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: value out of range: overflow`
- stask: `cols(?column?:701, ?column?:701, ?column?:701, ?column?:701) 1 rows ["Infinity", "-Infinity", "NaN", "Infinity"] tag=SELECT 1`

### DIFF — b/arithmetic:20 (type)

```sql
SELECT 1.5::float4, 0.1::float4, 3.14159274::float4::float8
```
- why: col 1 name pg="float4" stask="?column?"; col 1 (float4) type OID pg=700 stask=701; col 2 name pg="float4" stask="?column?"; col 2 (float4) type OID pg=700 stask=701; col 3 name pg="float8" stask="?column?"
- pg: `cols(float4:700, float4:700, float8:701) 1 rows ["1.5", "0.1", "3.1415927410125732"] tag=SELECT 1`
- stask: `cols(?column?:701, ?column?:701, ?column?:701) 1 rows ["1.5", "0.1", "3.14159274"] tag=SELECT 1`

### DIFF — b/arithmetic:21 (columns)

```sql
SELECT 1.0::float8, 100000000000000000000::float8, 1e-7::float8, 123456789.123456789::float8
```
- why: col 1 name pg="float8" stask="?column?"; col 2 name pg="float8" stask="?column?"; col 3 name pg="float8" stask="?column?"; col 4 name pg="float8" stask="?column?"
- pg: `cols(float8:701, float8:701, float8:701, float8:701) 1 rows ["1", "1e+20", "1e-07", "123456789.12345679"] tag=SELECT 1`
- stask: `cols(?column?:701, ?column?:701, ?column?:701, ?column?:701) 1 rows ["1", "1e+20", "1e-07", "123456789.12345679"] tag=SELECT 1`

### DIFF — b/arithmetic:22 (type+value)

```sql
SELECT sum(x), avg(x) FROM (VALUES (1), (2), (4)) AS t(x)
```
- why: col 2 (avg) type OID pg=1700 stask=701; row 1 col avg: pg="2.3333333333333333" stask="2.3333333333333335"
- pg: `cols(sum:20, avg:1700) 1 rows ["7", "2.3333333333333333"] tag=SELECT 1`
- stask: `cols(sum:20, avg:701) 1 rows ["7", "2.3333333333333335"] tag=SELECT 1`

### DIFF — b/arithmetic:23 (type+value)

```sql
SELECT sum(x::numeric), avg(x::numeric) FROM (VALUES (1), (2), (4)) AS t(x)
```
- why: col 1 (sum) type OID pg=1700 stask=701; col 2 (avg) type OID pg=1700 stask=701; row 1 col avg: pg="2.3333333333333333" stask="2.3333333333333335"
- pg: `cols(sum:1700, avg:1700) 1 rows ["7", "2.3333333333333333"] tag=SELECT 1`
- stask: `cols(sum:701, avg:701) 1 rows ["7", "2.3333333333333335"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/strings:26 (semantics)

```sql
SELECT 'abc' || 'def', 'abc' || 1, 1 || 2
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: operator does not exist: integer || integer`
- stask: `cols(?column?:25, ?column?:25, ?column?:25) 1 rows ["abcdef", "abc1", "12"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/strings:27 (feature)

```sql
SELECT length('héllo'), char_length('héllo'), octet_length('héllo'), bit_length('a')
```
- why: STASK rejects: 42883
- pg: `cols(length:23, char_length:23, octet_length:23, bit_length:23) 1 rows ["5", "5", "6", "8"] tag=SELECT 1`
- stask: `ERROR 42883: function bit_length does not exist`

### DIFF — b/strings:29 (columns)

```sql
SELECT substring('hello' FROM 2 FOR 3), substr('hello', 2), substr('hello', 0, 3), substring('hello', -1, 3)
```
- why: col 1 name pg="substring" stask="substr"; col 4 name pg="substring" stask="substr"
- pg: `cols(substring:25, substr:25, substr:25, substring:25) 1 rows ["ell", "ello", "he", "h"] tag=SELECT 1`
- stask: `cols(substr:25, substr:25, substr:25, substr:25) 1 rows ["ell", "ello", "he", "h"] tag=SELECT 1`

### DIFF — b/strings:30 (type)

```sql
SELECT position('lo' IN 'hello'), strpos('hello', 'l'), strpos('hello', 'z')
```
- why: col 1 name pg="position" stask="strpos"; col 1 (position) type OID pg=23 stask=20; col 2 (strpos) type OID pg=23 stask=20; col 3 (strpos) type OID pg=23 stask=20
- pg: `cols(position:23, strpos:23, strpos:23) 1 rows ["4", "3", "0"] tag=SELECT 1`
- stask: `cols(strpos:20, strpos:20, strpos:20) 1 rows ["4", "3", "0"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/strings:34 (feature)

```sql
SELECT replace('banana', 'an', 'AN'), translate('12345', '143', 'ax'), overlay('Txxxxas' placing 'hom' from 2 for 4)
```
- why: STASK rejects: 42601
- pg: `cols(replace:25, translate:25, overlay:25) 1 rows ["bANANa", "a2x5", "Thomas"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "placing" (expected ")")`

### DIFF — b/strings:38 (type)

```sql
SELECT ascii('A'), chr(66), chr(8364), md5('abc')
```
- why: col 1 (ascii) type OID pg=23 stask=20
- pg: `cols(ascii:23, chr:25, chr:25, md5:25) 1 rows ["65", "B", "€", "900150983cd24fb0d6963f7d28e17f72"] tag=SELECT 1`
- stask: `cols(ascii:20, chr:25, chr:25, md5:25) 1 rows ["65", "B", "€", "900150983cd24fb0d6963f7d28e17f72"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/strings:39 (semantics)

```sql
SELECT format('%s-%s', 'a', 1), format('%I', 'Hello World'), format('%L', 'it''s'), format('%5s|%-5s|', 'a', 'b')
```
- why: STASK rejects: 22023
- pg: `cols(format:25, format:25, format:25, format:25) 1 rows ["a-1", "\"Hello World\"", "'it''s'", " a|b |"] tag=SELECT 1`
- stask: `ERROR 22023: unrecognized format() type specifier '5'`

### ERR-ONLY-ONE-SIDE — b/strings:40 (feature)

```sql
SELECT quote_ident('a b'), quote_literal('a''b'), quote_nullable(NULL)
```
- why: STASK rejects: 42883
- pg: `cols(quote_ident:25, quote_literal:25, quote_nullable:25) 1 rows ["\"a b\"", "'a''b'", "NULL"] tag=SELECT 1`
- stask: `ERROR 42883: function quote_literal does not exist`

### DIFF — b/strings:41 (format)

```sql
SELECT to_char(1234.5, 'FM9999.00'), to_char(0.5, '0.00'), to_char(1234567, 'FM9,999,999'), to_char(-5, 'FM000')
```
- why: row 1 col to_char: pg="1234.50" stask="1234.5"
- pg: `cols(to_char:25, to_char:25, to_char:25, to_char:25) 1 rows ["1234.50", " 0.50", "1,234,567", "-005"] tag=SELECT 1`
- stask: `cols(to_char:25, to_char:25, to_char:25, to_char:25) 1 rows ["1234.5", " 0.50", "1,234,567", "-005"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/strings:42 (feature)

```sql
SELECT starts_with('hello', 'he'), 'hello' ^@ 'he'
```
- why: STASK rejects: 42601
- pg: `cols(starts_with:16, ?column?:16) 1 rows ["t", "t"] tag=SELECT 1`
- stask: `ERROR 42601: unexpected character '^'`

### DIFF — b/strings:43 (columns)

```sql
SELECT regexp_replace('abc123', '[0-9]+', '#'), regexp_replace('aaa', 'a', 'b', 'g'), 'abc' ~ '^a', 'abc' ~* 'B', 'abc' !~ 'z'
```
- why: col 3 name pg="?column?" stask="regexp_like"; col 4 name pg="?column?" stask="regexp_like"
- pg: `cols(regexp_replace:25, regexp_replace:25, ?column?:16, ?column?:16, ?column?:16) 1 rows ["abc#", "bbb", "t", "t", "t"] tag=SELECT 1`
- stask: `cols(regexp_replace:25, regexp_replace:25, regexp_like:16, regexp_like:16, ?column?:16) 1 rows ["abc#", "bbb", "t", "t", "t"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/strings:44 (feature)

```sql
SELECT (regexp_match('foo123bar', '([0-9]+)'))[1], regexp_split_to_array('a1b2c', '[0-9]')
```
- why: STASK rejects: 42883
- pg: `cols(regexp_match:25, regexp_split_to_array:1009) 1 rows ["123", "{a,b,c}"] tag=SELECT 1`
- stask: `ERROR 42883: function regexp_match does not exist`

### DIFF — b/strings:45 (value)

```sql
SELECT 'abc' < 'abd', 'a' < 'B', 'Z' < 'a', 'abc' = 'ABC', 'é' > 'e'
```
- why: row 1 col ?column?: pg="t" stask="f"
- pg: `cols(?column?:16, ?column?:16, ?column?:16, ?column?:16, ?column?:16) 1 rows ["t", "t", "f", "f", "t"] tag=SELECT 1`
- stask: `cols(?column?:16, ?column?:16, ?column?:16, ?column?:16, ?column?:16) 1 rows ["t", "f", "t", "f", "t"] tag=SELECT 1`

### DIFF — b/strings:47 (type+value)

```sql
SELECT 'abc'::char(5) || 'x', length('abc'::char(5)), 'abc'::varchar(2)
```
- why: col 2 (length) type OID pg=23 stask=20; col 3 name pg="varchar" stask="?column?"; col 3 (varchar) type OID pg=1043 stask=25; row 1 col varchar: pg="ab" stask="abc"
- pg: `cols(?column?:25, length:23, varchar:1043) 1 rows ["abcx", "3", "ab"] tag=SELECT 1`
- stask: `cols(?column?:25, length:20, ?column?:25) 1 rows ["abcx", "3", "abc"] tag=SELECT 1`

### DIFF — b/strings:48 (type+value)

```sql
SELECT 'x'::text, 'x'::name, 'x'::"char", pg_typeof('x')::text, pg_typeof('x'::varchar)::text, pg_typeof('x'::char(3))::text
```
- why: col 1 name pg="text" stask="?column?"; col 2 name pg="name" stask="?column?"; col 2 (name) type OID pg=19 stask=25; col 3 name pg="char" stask="?column?"; col 3 (char) type OID pg=18 stask=25; row 1 col pg_typeof: pg="unknown" stask="text"
- pg: `cols(text:25, name:19, char:18, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["x", "x", "x", "unknown", "character varying", "character"] tag=SELECT 1`
- stask: `cols(?column?:25, ?column?:25, ?column?:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["x", "x", "x", "text", "text", "text"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/strings:49 (feature)

```sql
SELECT E'a\tb', E'line1\nline2', 'a\tb', $$dollar 'quoted'$$, U&'\0041'
```
- why: STASK rejects: 42601
- pg: `cols(?column?:25, ?column?:25, ?column?:25, ?column?:25, ?column?:25) 1 rows ["a\tb", "line1\nline2", "a\\tb", "dollar 'quoted'", "A"] tag=SELECT 1`
- stask: `ERROR 42601: unexpected character '$'`

### DIFF — b/null-logic:55 (columns)

```sql
SELECT NULL IS DISTINCT FROM NULL, 1 IS DISTINCT FROM NULL, 1 IS NOT DISTINCT FROM 1
```
- why: col 1 name pg="?column?" stask="is_distinct_from"; col 2 name pg="?column?" stask="is_distinct_from"; col 3 name pg="?column?" stask="is_not_distinct_from"
- pg: `cols(?column?:16, ?column?:16, ?column?:16) 1 rows ["f", "t", "t"] tag=SELECT 1`
- stask: `cols(is_distinct_from:16, is_distinct_from:16, is_not_distinct_from:16) 1 rows ["f", "t", "t"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/null-logic:56 (feature)

```sql
SELECT TRUE IS TRUE, NULL::boolean IS TRUE, NULL::boolean IS NOT TRUE, NULL::boolean IS UNKNOWN, FALSE IS FALSE
```
- why: STASK rejects: 42601
- pg: `cols(?column?:16, ?column?:16, ?column?:16, ?column?:16, ?column?:16) 1 rows ["t", "f", "t", "t", "t"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error after IS`

### DIFF — b/null-logic:57 (type)

```sql
SELECT COALESCE(NULL, NULL, 3), COALESCE(NULL, 'a'), NULLIF(1, 1), NULLIF(1, 2), NULLIF('a', 'A')
```
- why: col 1 (coalesce) type OID pg=23 stask=25; col 3 (nullif) type OID pg=23 stask=20; col 4 (nullif) type OID pg=23 stask=20
- pg: `cols(coalesce:23, coalesce:25, nullif:23, nullif:23, nullif:25) 1 rows ["3", "a", NULL, "1", "a"] tag=SELECT 1`
- stask: `cols(coalesce:25, coalesce:25, nullif:20, nullif:20, nullif:25) 1 rows ["3", "a", NULL, "1", "a"] tag=SELECT 1`

### DIFF — b/null-logic:59 (type+value)

```sql
SELECT COUNT(*), COUNT(x), SUM(x), AVG(x), MIN(x), MAX(x) FROM (VALUES (1), (NULL), (3)) AS t(x)
```
- why: col 4 (avg) type OID pg=1700 stask=701; col 5 (min) type OID pg=23 stask=20; col 6 (max) type OID pg=23 stask=20; row 1 col avg: pg="2.0000000000000000" stask="2"
- pg: `cols(count:20, count:20, sum:20, avg:1700, min:23, max:23) 1 rows ["3", "2", "4", "2.0000000000000000", "1", "3"] tag=SELECT 1`
- stask: `cols(count:20, count:20, sum:20, avg:701, min:20, max:20) 1 rows ["3", "2", "4", "2", "1", "3"] tag=SELECT 1`

### DIFF — b/null-logic:60 (type)

```sql
SELECT COUNT(*), COUNT(x), SUM(x), MAX(x) FROM (SELECT 1 AS x WHERE FALSE) AS t
```
- why: col 4 (max) type OID pg=23 stask=20
- pg: `cols(count:20, count:20, sum:20, max:23) 1 rows ["0", "0", NULL, NULL] tag=SELECT 1`
- stask: `cols(count:20, count:20, sum:20, max:20) 1 rows ["0", "0", NULL, NULL] tag=SELECT 1`

### DIFF — b/null-logic:61 (type)

```sql
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x
```
- why: col 1 (x) type OID pg=23 stask=20
- pg: `cols(x:23) 3 rows ["1"] ["2"] [NULL] tag=SELECT 3`
- stask: `cols(x:20) 3 rows ["1"] ["2"] [NULL] tag=SELECT 3`

### DIFF — b/null-logic:62 (type)

```sql
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x DESC
```
- why: col 1 (x) type OID pg=23 stask=20
- pg: `cols(x:23) 3 rows [NULL] ["2"] ["1"] tag=SELECT 3`
- stask: `cols(x:20) 3 rows [NULL] ["2"] ["1"] tag=SELECT 3`

### DIFF — b/null-logic:63 (type)

```sql
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x NULLS FIRST
```
- why: col 1 (x) type OID pg=23 stask=20
- pg: `cols(x:23) 3 rows [NULL] ["1"] ["2"] tag=SELECT 3`
- stask: `cols(x:20) 3 rows [NULL] ["1"] ["2"] tag=SELECT 3`

### DIFF — b/null-logic:64 (type)

```sql
SELECT x FROM (VALUES (2), (NULL), (1)) AS t(x) ORDER BY x DESC NULLS LAST
```
- why: col 1 (x) type OID pg=23 stask=20
- pg: `cols(x:23) 3 rows ["2"] ["1"] [NULL] tag=SELECT 3`
- stask: `cols(x:20) 3 rows ["2"] ["1"] [NULL] tag=SELECT 3`

### DIFF — b/null-logic:65 (type)

```sql
SELECT DISTINCT x FROM (VALUES (1), (NULL), (NULL), (1)) AS t(x)
```
- why: col 1 (x) type OID pg=23 stask=20
- pg: `cols(x:23) 2 rows [NULL] ["1"] tag=SELECT 2`
- stask: `cols(x:20) 2 rows ["1"] [NULL] tag=SELECT 2`

### DIFF — b/null-logic:66 (type)

```sql
SELECT x, COUNT(*) FROM (VALUES (1), (NULL), (NULL), (1)) AS t(x) GROUP BY x
```
- why: col 1 (x) type OID pg=23 stask=20
- pg: `cols(x:23, count:20) 2 rows [NULL, "2"] ["1", "2"] tag=SELECT 2`
- stask: `cols(x:20, count:20) 2 rows ["1", "2"] [NULL, "2"] tag=SELECT 2`

### DIFF — b/case-coalesce:69 (type)

```sql
SELECT CASE WHEN 1 = 1 THEN 'y' ELSE 'n' END, CASE 2 WHEN 1 THEN 'one' WHEN 2 THEN 'two' END, CASE WHEN FALSE THEN 1 END
```
- why: col 3 (case) type OID pg=23 stask=20
- pg: `cols(case:25, case:25, case:23) 1 rows ["y", "two", NULL] tag=SELECT 1`
- stask: `cols(case:25, case:25, case:20) 1 rows ["y", "two", NULL] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/case-coalesce:71 (semantics)

```sql
SELECT CASE WHEN TRUE THEN 1 ELSE 'a' END
```
- why: STASK accepts, PostgreSQL rejects: 22P02
- pg: `ERROR 22P02: invalid input syntax for type integer: "a"`
- stask: `cols(case:20) 1 rows ["1"] tag=SELECT 1`

### DIFF — b/case-coalesce:72 (type+value)

```sql
SELECT CASE WHEN TRUE THEN 1 ELSE 2.5 END, pg_typeof(CASE WHEN TRUE THEN 1 ELSE 2.5 END)::text
```
- why: col 1 (case) type OID pg=1700 stask=20; row 1 col pg_typeof: pg="numeric" stask="bigint"
- pg: `cols(case:1700, pg_typeof:25) 1 rows ["1", "numeric"] tag=SELECT 1`
- stask: `cols(case:20, pg_typeof:25) 1 rows ["1", "bigint"] tag=SELECT 1`

### DIFF — b/case-coalesce:74 (type)

```sql
SELECT CASE WHEN TRUE THEN 1 ELSE 1/0 END
```
- why: col 1 (case) type OID pg=23 stask=20
- pg: `cols(case:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(case:20) 1 rows ["1"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/case-coalesce:75 (semantics)

```sql
SELECT COALESCE(1, 'a')
```
- why: STASK accepts, PostgreSQL rejects: 22P02
- pg: `ERROR 22P02: invalid input syntax for type integer: "a"`
- stask: `cols(coalesce:20) 1 rows ["1"] tag=SELECT 1`

### DIFF — b/case-coalesce:76 (value)

```sql
SELECT pg_typeof(COALESCE(NULL, 1))::text, pg_typeof(COALESCE(NULL, 1.5))::text, pg_typeof(NULL)::text
```
- why: row 1 col pg_typeof: pg="integer" stask="bigint"
- pg: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["integer", "numeric", "unknown"] tag=SELECT 1`
- stask: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["bigint", "double precision", NULL] tag=SELECT 1`

### DIFF — b/timestamps:79 (type+value)

```sql
SELECT '2026-01-15 10:30:45'::timestamp, '2026-01-15 10:30:45.123456'::timestamp, '2026-01-15'::date
```
- why: col 1 name pg="timestamp" stask="?column?"; col 1 (timestamp) type OID pg=1114 stask=1184; col 2 name pg="timestamp" stask="?column?"; col 2 (timestamp) type OID pg=1114 stask=1184; col 3 name pg="date" stask="?column?"; row 1 col timestamp: pg="2026-01-15 10:30:45" stask="2026-01-15 10:30:45+00"
- pg: `cols(timestamp:1114, timestamp:1114, date:1082) 1 rows ["2026-01-15 10:30:45", "2026-01-15 10:30:45.123456", "2026-01-15"] tag=SELECT 1`
- stask: `cols(?column?:1184, ?column?:1184, ?column?:1082) 1 rows ["2026-01-15 10:30:45+00", "2026-01-15 10:30:45.123+00", "2026-01-15"] tag=SELECT 1`

### DIFF — b/timestamps:80 (columns)

```sql
SELECT '2026-01-15 10:30:45+07'::timestamptz, '2026-01-15 10:30:45'::timestamptz
```
- why: col 1 name pg="timestamptz" stask="?column?"; col 2 name pg="timestamptz" stask="?column?"
- pg: `cols(timestamptz:1184, timestamptz:1184) 1 rows ["2026-01-15 03:30:45+00", "2026-01-15 10:30:45+00"] tag=SELECT 1`
- stask: `cols(?column?:1184, ?column?:1184) 1 rows ["2026-01-15 03:30:45+00", "2026-01-15 10:30:45+00"] tag=SELECT 1`

### DIFF — b/timestamps:81 (columns)

```sql
SELECT '2026-01-15 10:30:45.5+00'::timestamptz
```
- why: col 1 name pg="timestamptz" stask="?column?"
- pg: `cols(timestamptz:1184) 1 rows ["2026-01-15 10:30:45.5+00"] tag=SELECT 1`
- stask: `cols(?column?:1184) 1 rows ["2026-01-15 10:30:45.5+00"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/timestamps:82 (feature)

```sql
SELECT pg_typeof('2026-01-15'::date)::text, pg_typeof('2026-01-15 10:00'::timestamp)::text, pg_typeof('2026-01-15 10:00+00'::timestamptz)::text, pg_typeof('10:00'::time)::text
```
- why: STASK rejects: 0A000
- pg: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["date", "timestamp without time zone", "timestamp with time zone", "time without time zone"] tag=SELECT 1`
- stask: `ERROR 0A000: type "time" is not supported`

### ERR-ONLY-ONE-SIDE — b/timestamps:83 (feature)

```sql
SELECT '10:30:45'::time, '10:30:45.5'::time
```
- why: STASK rejects: 0A000
- pg: `cols(time:1083, time:1083) 1 rows ["10:30:45", "10:30:45.5"] tag=SELECT 1`
- stask: `ERROR 0A000: type "time" is not supported`

### DIFF — b/timestamps:84 (type)

```sql
SELECT '2026-01-31'::date + 1, '2026-01-31'::date - 31, '2026-03-01'::date - '2026-02-01'::date
```
- why: col 3 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:1082, ?column?:1082, ?column?:23) 1 rows ["2026-02-01", "2025-12-31", "28"] tag=SELECT 1`
- stask: `cols(?column?:1082, ?column?:1082, ?column?:20) 1 rows ["2026-02-01", "2025-12-31", "28"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/timestamps:85 (semantics)

```sql
SELECT '2026-01-31'::date + INTERVAL '1 month', '2026-01-31 10:00'::timestamp + INTERVAL '1 day 2 hours'
```
- why: STASK rejects: 22P02
- pg: `cols(?column?:1114, ?column?:1114) 1 rows ["2026-02-28 00:00:00", "2026-02-01 12:00:00"] tag=SELECT 1`
- stask: `ERROR 22P02: invalid input syntax for type timestamp: "2026-01-31 10:00"`

### ERR-ONLY-ONE-SIDE — b/timestamps:86 (semantics)

```sql
SELECT '2026-01-15 10:00'::timestamp - '2026-01-14 08:30'::timestamp
```
- why: STASK rejects: 22P02
- pg: `cols(?column?:1186) 1 rows ["1 day 01:30:00"] tag=SELECT 1`
- stask: `ERROR 22P02: invalid input syntax for type timestamp: "2026-01-15 10:00"`

### DIFF — b/timestamps:87 (columns)

```sql
SELECT '1 day'::interval, '1 year 2 mons 3 days 04:05:06'::interval, '90 minutes'::interval, '-1 day'::interval
```
- why: col 1 name pg="interval" stask="?column?"; col 2 name pg="interval" stask="?column?"; col 3 name pg="interval" stask="?column?"; col 4 name pg="interval" stask="?column?"; row 1 col interval: pg="-1 days" stask="-1 day"
- pg: `cols(interval:1186, interval:1186, interval:1186, interval:1186) 1 rows ["1 day", "1 year 2 mons 3 days 04:05:06", "01:30:00", "-1 days"] tag=SELECT 1`
- stask: `cols(?column?:1186, ?column?:1186, ?column?:1186, ?column?:1186) 1 rows ["1 day", "1 year 2 mons 3 days 04:05:06", "01:30:00", "-1 day"] tag=SELECT 1`

### DIFF — b/timestamps:89 (type)

```sql
SELECT EXTRACT(year FROM '2026-03-15 10:20:30'::timestamp), EXTRACT(month FROM '2026-03-15'::date), EXTRACT(dow FROM '2026-03-15'::date), EXTRACT(doy FROM '2026-03-15'::date)
```
- why: col 1 name pg="extract" stask="date_part"; col 1 (extract) type OID pg=1700 stask=701; col 2 name pg="extract" stask="date_part"; col 2 (extract) type OID pg=1700 stask=701; col 3 name pg="extract" stask="date_part"; col 3 (extract) type OID pg=1700 stask=701; col 4 name pg="extract" stask="date_part"; col 4 (extract) type OID pg=1700 stask=701
- pg: `cols(extract:1700, extract:1700, extract:1700, extract:1700) 1 rows ["2026", "3", "0", "74"] tag=SELECT 1`
- stask: `cols(date_part:701, date_part:701, date_part:701, date_part:701) 1 rows ["2026", "3", "0", "74"] tag=SELECT 1`

### DIFF — b/timestamps:90 (type+value)

```sql
SELECT EXTRACT(epoch FROM '2026-01-01 00:00:00+00'::timestamptz), EXTRACT(hour FROM '2026-03-15 10:20:30'::timestamp), EXTRACT(second FROM '2026-03-15 10:20:30.5'::timestamp)
```
- why: col 1 name pg="extract" stask="date_part"; col 1 (extract) type OID pg=1700 stask=701; col 2 name pg="extract" stask="date_part"; col 2 (extract) type OID pg=1700 stask=701; col 3 name pg="extract" stask="date_part"; col 3 (extract) type OID pg=1700 stask=701; row 1 col extract: pg="1767225600.000000" stask="1767225600"
- pg: `cols(extract:1700, extract:1700, extract:1700) 1 rows ["1767225600.000000", "10", "30.500000"] tag=SELECT 1`
- stask: `cols(date_part:701, date_part:701, date_part:701) 1 rows ["1767225600", "10", "30.5"] tag=SELECT 1`

### DIFF — b/timestamps:92 (type+value)

```sql
SELECT date_trunc('month', '2026-03-15 10:20:30'::timestamp), date_trunc('day', '2026-03-15 10:20:30'::timestamp), date_trunc('hour', '2026-03-15 10:20:30'::timestamptz)
```
- why: col 1 (date_trunc) type OID pg=1114 stask=1184; col 2 (date_trunc) type OID pg=1114 stask=1184; row 1 col date_trunc: pg="2026-03-01 00:00:00" stask="2026-03-01 00:00:00+00"
- pg: `cols(date_trunc:1114, date_trunc:1114, date_trunc:1184) 1 rows ["2026-03-01 00:00:00", "2026-03-15 00:00:00", "2026-03-15 10:00:00+00"] tag=SELECT 1`
- stask: `cols(date_trunc:1184, date_trunc:1184, date_trunc:1184) 1 rows ["2026-03-01 00:00:00+00", "2026-03-15 00:00:00+00", "2026-03-15 10:00:00+00"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/timestamps:94 (feature)

```sql
SELECT to_timestamp('2026-03-15 10:20:30', 'YYYY-MM-DD HH24:MI:SS'), to_date('15/03/2026', 'DD/MM/YYYY')
```
- why: STASK rejects: 42883
- pg: `cols(to_timestamp:1184, to_date:1082) 1 rows ["2026-03-15 10:20:30+00", "2026-03-15"] tag=SELECT 1`
- stask: `ERROR 42883: function to_timestamp with 2 arguments does not exist`

### ERR-ONLY-ONE-SIDE — b/timestamps:96 (feature)

```sql
SELECT '2026-01-15 10:00:00+00'::timestamptz AT TIME ZONE 'Asia/Ho_Chi_Minh'
```
- why: STASK rejects: 42601
- pg: `cols(timezone:1114) 1 rows ["2026-01-15 17:00:00"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "time"`

### ERR-ONLY-ONE-SIDE — b/timestamps:97 (feature)

```sql
SELECT '2026-01-15 10:00:00'::timestamp AT TIME ZONE 'UTC'
```
- why: STASK rejects: 42601
- pg: `cols(timezone:1184) 1 rows ["2026-01-15 10:00:00+00"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "time"`

### DIFF — b/timestamps:98 (value)

```sql
SELECT age('2026-03-15'::date, '2025-01-10'::date)
```
- why: row 1 col age: pg="1 year 2 mons 5 days" stask="429 days"
- pg: `cols(age:1186) 1 rows ["1 year 2 mons 5 days"] tag=SELECT 1`
- stask: `cols(age:1186) 1 rows ["429 days"] tag=SELECT 1`

### DIFF — b/timestamps:99 (value)

```sql
SELECT now() > '2020-01-01'::timestamptz, current_date > '2020-01-01'::date, pg_typeof(now())::text, pg_typeof(current_date)::text, pg_typeof(current_timestamp)::text
```
- why: row 1 col pg_typeof: pg="timestamp with time zone" stask="timestamptz"
- pg: `cols(?column?:16, ?column?:16, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["t", "t", "timestamp with time zone", "date", "timestamp with time zone"] tag=SELECT 1`
- stask: `cols(?column?:16, ?column?:16, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["t", "t", "timestamptz", "date", "timestamptz"] tag=SELECT 1`

### DIFF — b/timestamps:100 (sqlstate)

```sql
SELECT '2026-02-30'::date
```
- why: SQLSTATE pg=22008 stask=22P02
- pg: `ERROR 22008: date/time field value out of range: "2026-02-30"`
- stask: `ERROR 22P02: invalid input syntax for type date: "2026-02-30"`

### DIFF — b/timestamps:101 (sqlstate)

```sql
SELECT '2026-13-01'::timestamp
```
- why: SQLSTATE pg=22008 stask=22P02
- pg: `ERROR 22008: date/time field value out of range: "2026-13-01"`
- stask: `ERROR 22P02: invalid input syntax for type timestamp: "2026-13-01"`

### DIFF — b/timestamps:102 (sqlstate)

```sql
SELECT 'notadate'::timestamptz
```
- why: SQLSTATE pg=22007 stask=22P02
- pg: `ERROR 22007: invalid input syntax for type timestamp with time zone: "notadate"`
- stask: `ERROR 22P02: invalid input syntax for type timestamp: "notadate"`

### DIFF — b/timestamps:103 (type+value)

```sql
SELECT make_date(2026, 3, 15), make_timestamp(2026, 3, 15, 10, 20, 30)
```
- why: col 2 (make_timestamp) type OID pg=1114 stask=1184; row 1 col make_timestamp: pg="2026-03-15 10:20:30" stask="2026-03-15 10:20:30+00"
- pg: `cols(make_date:1082, make_timestamp:1114) 1 rows ["2026-03-15", "2026-03-15 10:20:30"] tag=SELECT 1`
- stask: `cols(make_date:1082, make_timestamp:1184) 1 rows ["2026-03-15", "2026-03-15 10:20:30+00"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/timestamps:104 (semantics)

```sql
SELECT '2026-01-01'::date < '2026-01-02'::date, '2026-01-01 00:00'::timestamp = '2026-01-01'::date
```
- why: STASK rejects: 22P02
- pg: `cols(?column?:16, ?column?:16) 1 rows ["t", "t"] tag=SELECT 1`
- stask: `ERROR 22P02: invalid input syntax for type timestamp: "2026-01-01 00:00"`

### DIFF — b/timestamps:105 (type)

```sql
SELECT generate_series('2026-01-01'::date, '2026-01-05'::date, '1 day'::interval)
```
- why: col 1 (generate_series) type OID pg=1184 stask=25
- pg: `cols(generate_series:1184) 5 rows ["2026-01-01 00:00:00+00"] ["2026-01-02 00:00:00+00"] ["2026-01-03 00:00:00+00"] ... tag=SELECT 5`
- stask: `cols(generate_series:25) 5 rows ["2026-01-01 00:00:00+00"] ["2026-01-02 00:00:00+00"] ["2026-01-03 00:00:00+00"] ... tag=SELECT 5`

### ERR-ONLY-ONE-SIDE — b/timestamps:106 (semantics)

```sql
SELECT 'infinity'::timestamp, '-infinity'::date
```
- why: STASK rejects: 22P02
- pg: `cols(timestamp:1114, date:1082) 1 rows ["infinity", "-infinity"] tag=SELECT 1`
- stask: `ERROR 22P02: invalid input syntax for type timestamp: "infinity"`

### DIFF — b/timestamps:107 (columns)

```sql
SELECT '2026-03-15 10:20:30.123456'::timestamptz
```
- why: col 1 name pg="timestamptz" stask="?column?"; row 1 col timestamptz: pg="2026-03-15 10:20:30.123456+00" stask="2026-03-15 10:20:30.123+00"
- pg: `cols(timestamptz:1184) 1 rows ["2026-03-15 10:20:30.123456+00"] tag=SELECT 1`
- stask: `cols(?column?:1184) 1 rows ["2026-03-15 10:20:30.123+00"] tag=SELECT 1`

### DIFF — b/timestamps:108 (columns)

```sql
SHOW timezone
```
- why: col 1 name pg="TimeZone" stask="timezone"
- pg: `cols(TimeZone:25) 1 rows ["UTC"] tag=SHOW`
- stask: `cols(timezone:25) 1 rows ["UTC"] tag=SHOW`

### DIFF — b/timestamps:109 (columns)

```sql
SELECT '2026-01-15 10:00:00'::timestamptz
```
- why: col 1 name pg="timestamptz" stask="?column?"
- pg: `cols(timestamptz:1184) 1 rows ["2026-01-15 10:00:00+00"] tag=SELECT 1`
- stask: `cols(?column?:1184) 1 rows ["2026-01-15 10:00:00+00"] tag=SELECT 1`

### DIFF — b/casts:112 (type)

```sql
SELECT '12'::int, '12'::bigint, '12.5'::numeric, '12.5'::float8, ' 12 '::int, '1e2'::float8
```
- why: col 1 name pg="int4" stask="?column?"; col 1 (int4) type OID pg=23 stask=20; col 2 name pg="int8" stask="?column?"; col 3 name pg="numeric" stask="?column?"; col 3 (numeric) type OID pg=1700 stask=701; col 4 name pg="float8" stask="?column?"; col 5 name pg="int4" stask="?column?"; col 5 (int4) type OID pg=23 stask=20; col 6 name pg="float8" stask="?column?"
- pg: `cols(int4:23, int8:20, numeric:1700, float8:701, int4:23, float8:701) 1 rows ["12", "12", "12.5", "12.5", "12", "100"] tag=SELECT 1`
- stask: `cols(?column?:20, ?column?:20, ?column?:701, ?column?:701, ?column?:20, ?column?:701) 1 rows ["12", "12", "12.5", "12.5", "12", "100"] tag=SELECT 1`

### DIFF — b/casts:113 (columns)

```sql
SELECT 12::text, 12.50::text, true::text, false::text, NULL::text
```
- why: col 1 name pg="text" stask="?column?"; col 2 name pg="text" stask="?column?"; col 3 name pg="text" stask="?column?"; col 4 name pg="text" stask="?column?"; col 5 name pg="text" stask="?column?"; row 1 col text: pg="12.50" stask="12.5"
- pg: `cols(text:25, text:25, text:25, text:25, text:25) 1 rows ["12", "12.50", "true", "false", NULL] tag=SELECT 1`
- stask: `cols(?column?:25, ?column?:25, ?column?:25, ?column?:25, ?column?:25) 1 rows ["12", "12.5", "t", "f", NULL] tag=SELECT 1`

### DIFF — b/casts:114 (type)

```sql
SELECT CAST(12 AS varchar), CAST('12' AS integer), 12::smallint, 12::int2, 12::int4, 12::int8
```
- why: col 1 name pg="varchar" stask="?column?"; col 1 (varchar) type OID pg=1043 stask=25; col 2 name pg="int4" stask="?column?"; col 2 (int4) type OID pg=23 stask=20; col 3 name pg="int2" stask="?column?"; col 3 (int2) type OID pg=21 stask=20; col 4 name pg="int2" stask="?column?"; col 4 (int2) type OID pg=21 stask=20; col 5 name pg="int4" stask="?column?"; col 5 (int4) type OID pg=23 stask=20; col 6 name pg="int8" stask="?column?"
- pg: `cols(varchar:1043, int4:23, int2:21, int2:21, int4:23, int8:20) 1 rows ["12", "12", "12", "12", "12", "12"] tag=SELECT 1`
- stask: `cols(?column?:25, ?column?:20, ?column?:20, ?column?:20, ?column?:20, ?column?:20) 1 rows ["12", "12", "12", "12", "12", "12"] tag=SELECT 1`

### DIFF — b/casts:115 (type+value)

```sql
SELECT 1.5::int, 2.5::int, 3.5::int, -1.5::int, 1.4999::int
```
- why: col 1 name pg="int4" stask="?column?"; col 1 (int4) type OID pg=23 stask=20; col 2 name pg="int4" stask="?column?"; col 2 (int4) type OID pg=23 stask=20; col 3 name pg="int4" stask="?column?"; col 3 (int4) type OID pg=23 stask=20; col 4 (?column?) type OID pg=23 stask=20; col 5 name pg="int4" stask="?column?"; col 5 (int4) type OID pg=23 stask=20; row 1 col int4: pg="3" stask="2"
- pg: `cols(int4:23, int4:23, int4:23, ?column?:23, int4:23) 1 rows ["2", "3", "4", "-2", "1"] tag=SELECT 1`
- stask: `cols(?column?:20, ?column?:20, ?column?:20, ?column?:20, ?column?:20) 1 rows ["2", "2", "4", "-2", "1"] tag=SELECT 1`

### PASS-MSG — b/casts:116 (message)

```sql
SELECT 'abc'::int
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "abc"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "abc"`

### PASS-MSG — b/casts:117 (message)

```sql
SELECT '12abc'::int
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "12abc"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "12abc"`

### PASS-MSG — b/casts:118 (message)

```sql
SELECT ''::int
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: ""`
- stask: `ERROR 22P02: invalid input syntax for type bigint: ""`

### ERR-ONLY-ONE-SIDE — b/casts:119 (semantics)

```sql
SELECT '99999999999'::int
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: value "99999999999" is out of range for type integer`
- stask: `cols(?column?:20) 1 rows ["99999999999"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:120 (semantics)

```sql
SELECT 32768::smallint
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: smallint out of range`
- stask: `cols(?column?:20) 1 rows ["32768"] tag=SELECT 1`

### DIFF — b/casts:121 (columns)

```sql
SELECT 1.5::numeric::text, '1.50'::numeric::text, 0::numeric::text, (-0.0)::numeric::text
```
- why: col 1 name pg="text" stask="?column?"; col 2 name pg="text" stask="?column?"; col 3 name pg="text" stask="?column?"; col 4 name pg="text" stask="?column?"; row 1 col text: pg="1.50" stask="1.5"
- pg: `cols(text:25, text:25, text:25, text:25) 1 rows ["1.5", "1.50", "0", "0.0"] tag=SELECT 1`
- stask: `cols(?column?:25, ?column?:25, ?column?:25, ?column?:25) 1 rows ["1.5", "1.5", "0", "-0"] tag=SELECT 1`

### DIFF — b/casts:122 (columns)

```sql
SELECT 't'::boolean, 'true'::boolean, 'yes'::boolean, 'on'::boolean, '1'::boolean, 'f'::boolean, 'no'::boolean, '0'::boolean
```
- why: col 1 name pg="bool" stask="?column?"; col 2 name pg="bool" stask="?column?"; col 3 name pg="bool" stask="?column?"; col 4 name pg="bool" stask="?column?"; col 5 name pg="bool" stask="?column?"; col 6 name pg="bool" stask="?column?"; col 7 name pg="bool" stask="?column?"; col 8 name pg="bool" stask="?column?"
- pg: `cols(bool:16, bool:16, bool:16, bool:16, bool:16, bool:16, bool:16, bool:16) 1 rows ["t", "t", "t", "t", "t", "f", "f", "f"] tag=SELECT 1`
- stask: `cols(?column?:16, ?column?:16, ?column?:16, ?column?:16, ?column?:16, ?column?:16, ?column?:16, ?column?:16) 1 rows ["t", "t", "t", "t", "t", "f", "f", "f"] tag=SELECT 1`

### DIFF — b/casts:124 (columns)

```sql
SELECT 1::boolean, 0::boolean, 5::boolean
```
- why: col 1 name pg="bool" stask="?column?"; col 2 name pg="bool" stask="?column?"; col 3 name pg="bool" stask="?column?"
- pg: `cols(bool:16, bool:16, bool:16) 1 rows ["t", "f", "t"] tag=SELECT 1`
- stask: `cols(?column?:16, ?column?:16, ?column?:16) 1 rows ["t", "f", "t"] tag=SELECT 1`

### DIFF — b/casts:125 (type)

```sql
SELECT true::int, false::int
```
- why: col 1 name pg="int4" stask="?column?"; col 1 (int4) type OID pg=23 stask=20; col 2 name pg="int4" stask="?column?"; col 2 (int4) type OID pg=23 stask=20
- pg: `cols(int4:23, int4:23) 1 rows ["1", "0"] tag=SELECT 1`
- stask: `cols(?column?:20, ?column?:20) 1 rows ["1", "0"] tag=SELECT 1`

### DIFF — b/casts:127 (type+value)

```sql
SELECT 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid, pg_typeof('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid)::text
```
- why: col 1 name pg="uuid" stask="?column?"; col 1 (uuid) type OID pg=2950 stask=25; row 1 col pg_typeof: pg="uuid" stask="text"
- pg: `cols(uuid:2950, pg_typeof:25) 1 rows ["a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "uuid"] tag=SELECT 1`
- stask: `cols(?column?:25, pg_typeof:25) 1 rows ["a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "text"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:128 (semantics)

```sql
SELECT 'zzz'::uuid
```
- why: STASK accepts, PostgreSQL rejects: 22P02
- pg: `ERROR 22P02: invalid input syntax for type uuid: "zzz"`
- stask: `cols(?column?:25) 1 rows ["zzz"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:129 (feature)

```sql
SELECT '\xdeadbeef'::bytea, pg_typeof('\xdeadbeef'::bytea)::text, length('\xdeadbeef'::bytea), encode('abc'::bytea, 'hex'), decode('616263', 'hex')
```
- why: STASK rejects: 0A000
- pg: `cols(bytea:17, pg_typeof:25, length:23, encode:25, decode:17) 1 rows ["\\xdeadbeef", "bytea", "4", "616263", "\\x616263"] tag=SELECT 1`
- stask: `ERROR 0A000: type "bytea" is not supported`

### DIFF — b/casts:130 (type+value)

```sql
SELECT '{"a":1}'::json, '{"a": 1}'::jsonb, pg_typeof('{"a":1}'::jsonb)::text
```
- why: col 1 name pg="json" stask="?column?"; col 1 (json) type OID pg=114 stask=25; col 2 name pg="jsonb" stask="?column?"; col 2 (jsonb) type OID pg=3802 stask=25; row 1 col pg_typeof: pg="jsonb" stask="text"
- pg: `cols(json:114, jsonb:3802, pg_typeof:25) 1 rows ["{\"a\":1}", "{\"a\": 1}", "jsonb"] tag=SELECT 1`
- stask: `cols(?column?:25, ?column?:25, pg_typeof:25) 1 rows ["{\"a\":1}", "{\"a\": 1}", "text"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:131 (semantics)

```sql
SELECT '{bad'::jsonb
```
- why: STASK accepts, PostgreSQL rejects: 22P02
- pg: `ERROR 22P02: invalid input syntax for type json`
- stask: `cols(?column?:25) 1 rows ["{bad"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:132 (feature)

```sql
SELECT '{"a":{"b":[1,2,3]}}'::jsonb -> 'a' -> 'b' ->> 1, '{"a":1}'::jsonb ->> 'a', '{"a":1}'::jsonb @> '{"a":1}'::jsonb, '{"a":1,"b":2}'::jsonb ? 'b'
```
- why: STASK rejects: 42601
- pg: `cols(?column?:25, ?column?:25, ?column?:16, ?column?:16) 1 rows ["2", "1", "t", "t"] tag=SELECT 1`
- stask: `ERROR 42601: unexpected character '@'`

### ERR-ONLY-ONE-SIDE — b/casts:133 (feature)

```sql
SELECT jsonb_build_object('a', 1, 'b', 'x'), json_build_array(1, 'a', NULL), to_jsonb(ARRAY[1,2]), jsonb_typeof('[1]'::jsonb)
```
- why: STASK rejects: 42883
- pg: `cols(jsonb_build_object:3802, json_build_array:114, to_jsonb:3802, jsonb_typeof:25) 1 rows ["{\"a\": 1, \"b\": \"x\"}", "[1, \"a\", null]", "[1, 2]", "array"] tag=SELECT 1`
- stask: `ERROR 42883: function jsonb_build_object does not exist`

### ERR-ONLY-ONE-SIDE — b/casts:134 (semantics)

```sql
SELECT ARRAY[1,2,3], ARRAY['a','b'], ARRAY[1,2,3][2], pg_typeof(ARRAY[1,2,3])::text, array_length(ARRAY[1,2,3], 1), cardinality(ARRAY[1,2,3])
```
- why: STASK accepts, PostgreSQL rejects: 42601
- pg: `ERROR 42601: syntax error at or near "["`
- stask: `cols(array:1009, array:1009, stask_subscript:25, pg_typeof:25, array_length:20, cardinality:20) 1 rows ["{1,2,3}", "{a,b}", "2", "array", "3", "3"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:135 (feature)

```sql
SELECT '{1,2,3}'::int[], ARRAY[1,2] || ARRAY[3], 2 = ANY(ARRAY[1,2,3]), array_append(ARRAY[1], 2), array_to_string(ARRAY[1,2,3], ','), string_to_array('a,b,c', ',')
```
- why: STASK rejects: 42883
- pg: `cols(int4:1007, ?column?:1007, ?column?:16, array_append:1007, array_to_string:25, string_to_array:1009) 1 rows ["{1,2,3}", "{1,2,3}", "t", "{1,2}", "1,2,3", "{a,b,c}"] tag=SELECT 1`
- stask: `ERROR 42883: function array_append does not exist`

### DIFF — b/casts:136 (type)

```sql
SELECT unnest(ARRAY[3,1,2])
```
- why: col 1 (unnest) type OID pg=23 stask=25
- pg: `cols(unnest:23) 3 rows ["3"] ["1"] ["2"] tag=SELECT 3`
- stask: `cols(unnest:25) 3 rows ["3"] ["1"] ["2"] tag=SELECT 3`

### DIFF — b/casts:137 (type)

```sql
SELECT ARRAY[1, NULL, 3], '{}'::int[], ARRAY[[1,2],[3,4]]
```
- why: col 1 (array) type OID pg=1007 stask=1009; col 2 name pg="int4" stask="?column?"; col 2 (int4) type OID pg=1007 stask=25; col 3 (array) type OID pg=1007 stask=1009
- pg: `cols(array:1007, int4:1007, array:1007) 1 rows ["{1,NULL,3}", "{}", "{{1,2},{3,4}}"] tag=SELECT 1`
- stask: `cols(array:1009, ?column?:25, array:1009) 1 rows ["{1,NULL,3}", "{}", "{{1,2},{3,4}}"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:138 (feature)

```sql
SELECT ROW(1, 'a'), (1, 'a') = (1, 'a'), (1, 2) < (1, 3)
```
- why: STASK rejects: 42883
- pg: `cols(row:2249, ?column?:16, ?column?:16) 1 rows ["(1,a)", "t", "t"] tag=SELECT 1`
- stask: `ERROR 42883: function row does not exist`

### DIFF — b/casts:139 (type)

```sql
SELECT '1 2 3'::int2vector
```
- why: col 1 name pg="int2vector" stask="?column?"; col 1 (int2vector) type OID pg=22 stask=25
- pg: `cols(int2vector:22) 1 rows ["1 2 3"] tag=SELECT 1`
- stask: `cols(?column?:25) 1 rows ["1 2 3"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/casts:141 (feature)

```sql
SELECT '$1,234.50'::money
```
- why: STASK rejects: 0A000
- pg: `cols(money:790) 1 rows ["$1,234.50"] tag=SELECT 1`
- stask: `ERROR 0A000: type "money" is not supported`

### ERR-ONLY-ONE-SIDE — b/casts:142 (feature)

```sql
SELECT '192.168.0.1'::inet, '10.0.0.0/8'::cidr
```
- why: STASK rejects: 0A000
- pg: `cols(inet:869, cidr:650) 1 rows ["192.168.0.1", "10.0.0.0/8"] tag=SELECT 1`
- stask: `ERROR 0A000: type "inet" is not supported`

### ERR-ONLY-ONE-SIDE — b/casts:143 (feature)

```sql
SELECT '1010'::bit(4), B'101'
```
- why: STASK rejects: 0A000
- pg: `cols(bit:1560, ?column?:1560) 1 rows ["1010", "101"] tag=SELECT 1`
- stask: `ERROR 0A000: type "bit" is not supported`

### ERR-ONLY-ONE-SIDE — b/casts:144 (feature)

```sql
SELECT int4range(1, 5), daterange('2026-01-01', '2026-02-01')
```
- why: STASK rejects: 42883
- pg: `cols(int4range:3904, daterange:3912) 1 rows ["[1,5)", "[2026-01-01,2026-02-01)"] tag=SELECT 1`
- stask: `ERROR 42883: function int4range does not exist`

### ERR-ONLY-ONE-SIDE — b/casts:145 (feature)

```sql
SELECT 'a'::text::bytea
```
- why: STASK rejects: 0A000
- pg: `cols(bytea:17) 1 rows ["\\x61"] tag=SELECT 1`
- stask: `ERROR 0A000: type "bytea" is not supported`

### DIFF — b/casts:146 (type)

```sql
SELECT length(gen_random_uuid()::text), length(md5(random()::text))
```
- why: col 1 (length) type OID pg=23 stask=20; col 2 (length) type OID pg=23 stask=20
- pg: `cols(length:23, length:23) 1 rows ["36", "32"] tag=SELECT 1`
- stask: `cols(length:20, length:20) 1 rows ["36", "32"] tag=SELECT 1`

### DIFF — b/casts:147 (value)

```sql
SELECT 0.1 = 0.1::float8, 0.1::float4 = 0.1::float8
```
- why: row 1 col ?column?: pg="f" stask="t"
- pg: `cols(?column?:16, ?column?:16) 1 rows ["t", "f"] tag=SELECT 1`
- stask: `cols(?column?:16, ?column?:16) 1 rows ["t", "t"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/like-in-exists:161 (feature)

```sql
SELECT name FROM pd_p WHERE name LIKE 'A!_B' ESCAPE '!'
```
- why: STASK rejects: 42601
- pg: `cols(name:25) 1 rows ["A_B"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "escape"`

### ERR-ONLY-ONE-SIDE — b/like-in-exists:162 (feature)

```sql
SELECT name FROM pd_p WHERE name SIMILAR TO '(A|a)l%'
```
- why: STASK rejects: 42601
- pg: `cols(name:25) 2 rows ["Alice"] ["alan"] tag=SELECT 2`
- stask: `ERROR 42601: syntax error at or near "similar"`

### DIFF — b/like-in-exists:164 (value)

```sql
SELECT name FROM pd_p WHERE lower(name) LIKE 'al%' ORDER BY name
```
- why: row 1 col name: pg="alan" stask="Alice"
- pg: `cols(name:25) 2 rows ["alan"] ["Alice"] tag=SELECT 2`
- stask: `cols(name:25) 2 rows ["Alice"] ["alan"] tag=SELECT 2`

### DIFF — b/like-in-exists:174 (type+value)

```sql
SELECT name, (SELECT SUM(amt) FROM pd_q WHERE pid = p.id) AS total FROM pd_p p
```
- why: col 2 (total) type OID pg=1700 stask=701; row 3 col total: pg="30.50" stask="30.5"
- pg: `cols(name:25, total:1700) 6 rows ["Alice", "30.50"] ["bob", NULL] ["Carol", "5.25"] ... tag=SELECT 6`
- stask: `cols(name:25, total:701) 6 rows ["Alice", "30.5"] ["bob", NULL] ["Carol", "5.25"] ... tag=SELECT 6`

### DIFF — b/joins-setops:187 (type+value)

```sql
SELECT p.name, q.amt FROM pd_p p JOIN pd_q q ON q.pid = p.id
```
- why: col 2 (amt) type OID pg=1700 stask=701; row 1 col amt: pg="10.50" stask="10.5"
- pg: `cols(name:25, amt:1700) 3 rows ["a", "10.50"] ["a", "20.00"] ["c", "5.25"] tag=SELECT 3`
- stask: `cols(name:25, amt:701) 3 rows ["a", "10.5"] ["a", "20"] ["c", "5.25"] tag=SELECT 3`

### DIFF — b/joins-setops:188 (type+value)

```sql
SELECT p.name, q.amt FROM pd_p p LEFT JOIN pd_q q ON q.pid = p.id
```
- why: col 2 (amt) type OID pg=1700 stask=701; row 1 col amt: pg="10.50" stask="10.5"
- pg: `cols(name:25, amt:1700) 4 rows ["a", "10.50"] ["a", "20.00"] ["c", "5.25"] ... tag=SELECT 4`
- stask: `cols(name:25, amt:701) 4 rows ["a", "10.5"] ["a", "20"] ["b", NULL] ... tag=SELECT 4`

### DIFF — b/joins-setops:189 (type+value)

```sql
SELECT p.name, q.amt FROM pd_p p RIGHT JOIN pd_q q ON q.pid = p.id
```
- why: col 2 (amt) type OID pg=1700 stask=701; row 1 col amt: pg="1.00" stask="1"
- pg: `cols(name:25, amt:1700) 4 rows ["a", "10.50"] ["a", "20.00"] ["c", "5.25"] ... tag=SELECT 4`
- stask: `cols(name:25, amt:701) 4 rows ["a", "10.5"] ["a", "20"] ["c", "5.25"] ... tag=SELECT 4`

### DIFF — b/joins-setops:190 (type+value)

```sql
SELECT p.name, q.amt FROM pd_p p FULL JOIN pd_q q ON q.pid = p.id
```
- why: col 2 (amt) type OID pg=1700 stask=701; row 1 col amt: pg="1.00" stask="1"
- pg: `cols(name:25, amt:1700) 5 rows ["a", "10.50"] ["a", "20.00"] ["c", "5.25"] ... tag=SELECT 5`
- stask: `cols(name:25, amt:701) 5 rows ["a", "10.5"] ["a", "20"] ["b", NULL] ... tag=SELECT 5`

### DIFF — b/joins-setops:193 (type+value)

```sql
SELECT * FROM pd_p NATURAL JOIN pd_q
```
- why: col 1 (id) type OID pg=23 stask=20; col 3 (pid) type OID pg=23 stask=20; col 4 (amt) type OID pg=1700 stask=701; row 1 col amt: pg="10.50" stask="10.5"
- pg: `cols(id:23, name:25, pid:23, amt:1700) 3 rows ["1", "a", "1", "10.50"] ["2", "b", "1", "20.00"] ["3", "c", "3", "5.25"] tag=SELECT 3`
- stask: `cols(id:20, name:25, pid:20, amt:701) 3 rows ["1", "a", "1", "10.5"] ["2", "b", "1", "20"] ["3", "c", "3", "5.25"] tag=SELECT 3`

### DIFF — b/joins-setops:194 (type)

```sql
SELECT id FROM pd_p UNION SELECT pid FROM pd_q
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 4 rows ["2"] ["9"] ["3"] ... tag=SELECT 4`
- stask: `cols(id:20) 4 rows ["1"] ["2"] ["3"] ... tag=SELECT 4`

### DIFF — b/joins-setops:195 (type)

```sql
SELECT id FROM pd_p UNION ALL SELECT pid FROM pd_q
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 7 rows ["1"] ["2"] ["3"] ... tag=SELECT 7`
- stask: `cols(id:20) 7 rows ["1"] ["2"] ["3"] ... tag=SELECT 7`

### DIFF — b/joins-setops:196 (type)

```sql
SELECT id FROM pd_p INTERSECT SELECT pid FROM pd_q
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 2 rows ["3"] ["1"] tag=SELECT 2`
- stask: `cols(id:20) 2 rows ["1"] ["3"] tag=SELECT 2`

### DIFF — b/joins-setops:197 (type)

```sql
SELECT id FROM pd_p EXCEPT SELECT pid FROM pd_q
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 1 rows ["2"] tag=SELECT 1`
- stask: `cols(id:20) 1 rows ["2"] tag=SELECT 1`

### DIFF — b/joins-setops:198 (type)

```sql
SELECT id FROM pd_p UNION SELECT pid FROM pd_q ORDER BY 1 DESC
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 4 rows ["9"] ["3"] ["2"] ... tag=SELECT 4`
- stask: `cols(id:20) 4 rows ["9"] ["3"] ["2"] ... tag=SELECT 4`

### ERR-ONLY-ONE-SIDE — b/joins-setops:200 (semantics)

```sql
SELECT id FROM pd_p UNION SELECT name FROM pd_p
```
- why: STASK accepts, PostgreSQL rejects: 42804
- pg: `ERROR 42804: UNION types integer and text cannot be matched`
- stask: `cols(id:20) 6 rows ["1"] ["2"] ["3"] ... tag=SELECT 6`

### DIFF — b/joins-setops:201 (columns)

```sql
SELECT p.name, (SELECT COUNT(*) FROM pd_q q WHERE q.pid = p.id) FROM pd_p p
```
- why: col 2 name pg="count" stask="?column?"
- pg: `cols(name:25, count:20) 3 rows ["a", "2"] ["b", "0"] ["c", "1"] tag=SELECT 3`
- stask: `cols(name:25, ?column?:20) 3 rows ["a", "2"] ["b", "0"] ["c", "1"] tag=SELECT 3`

### DIFF — b/joins-setops:202 (type)

```sql
SELECT p.id FROM pd_p p, LATERAL (SELECT 1 FROM pd_q q WHERE q.pid = p.id LIMIT 1) l
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 2 rows ["1"] ["3"] tag=SELECT 2`
- stask: `cols(id:20) 2 rows ["1"] ["3"] tag=SELECT 2`

### DIFF — b/joins-setops:203 (type)

```sql
SELECT a.id, b.id FROM pd_p a JOIN pd_p b ON a.id < b.id
```
- why: col 1 (id) type OID pg=23 stask=20; col 2 (id) type OID pg=23 stask=20
- pg: `cols(id:23, id:23) 3 rows ["1", "2"] ["1", "3"] ["2", "3"] tag=SELECT 3`
- stask: `cols(id:20, id:20) 3 rows ["1", "2"] ["1", "3"] ["2", "3"] tag=SELECT 3`

### DIFF — b/aggregate-group:211 (type+value)

```sql
SELECT carrier, COUNT(*), SUM(amt), AVG(amt), MIN(amt), MAX(amt) FROM pd_s GROUP BY carrier
```
- why: col 3 (sum) type OID pg=1700 stask=701; col 4 (avg) type OID pg=1700 stask=701; col 5 (min) type OID pg=1700 stask=701; col 6 (max) type OID pg=1700 stask=701; row 1 col sum: pg="1.00" stask="1"
- pg: `cols(carrier:25, count:20, sum:1700, avg:1700, min:1700, max:1700) 4 rows [NULL, "1", "1.00", "1.00000000000000000000", "1.00", "1.00"] ["vina", "1", "100.00", "100.0000000000000000", "100.00", "100.00"] ["viettel", "2", "30.75", "15.3750000000000000", "10.50", "20.25"] ... tag=SELECT 4`
- stask: `cols(carrier:25, count:20, sum:701, avg:701, min:701, max:701) 4 rows ["viettel", "2", "30.75", "15.375", "10.5", "20.25"] ["mobi", "2", "12.75", "6.375", "5", "7.75"] ["vina", "1", "100", "100", "100", "100"] ... tag=SELECT 4`

### DIFF — b/aggregate-group:212 (type+value)

```sql
SELECT carrier, SUM(qty), AVG(qty) FROM pd_s GROUP BY carrier
```
- why: col 3 (avg) type OID pg=1700 stask=701; row 1 col avg: pg="1.00000000000000000000" stask="1"
- pg: `cols(carrier:25, sum:20, avg:1700) 4 rows [NULL, "1", "1.00000000000000000000"] ["vina", "5", "5.0000000000000000"] ["viettel", "3", "1.5000000000000000"] ... tag=SELECT 4`
- stask: `cols(carrier:25, sum:20, avg:701) 4 rows ["viettel", "3", "1.5"] ["mobi", "3", "3"] ["vina", "5", "5"] ... tag=SELECT 4`

### DIFF — b/aggregate-group:215 (type+value)

```sql
SELECT carrier, SUM(amt) AS total FROM pd_s GROUP BY carrier ORDER BY total DESC NULLS LAST
```
- why: col 2 (total) type OID pg=1700 stask=701; row 1 col total: pg="100.00" stask="100"
- pg: `cols(carrier:25, total:1700) 4 rows ["vina", "100.00"] ["viettel", "30.75"] ["mobi", "12.75"] ... tag=SELECT 4`
- stask: `cols(carrier:25, total:701) 4 rows ["vina", "100"] ["viettel", "30.75"] ["mobi", "12.75"] ... tag=SELECT 4`

### DIFF — b/aggregate-group:217 (type+value)

```sql
SELECT COUNT(*), SUM(amt), AVG(amt), pg_typeof(SUM(amt))::text, pg_typeof(AVG(amt))::text, pg_typeof(COUNT(*))::text, pg_typeof(SUM(qty))::text FROM pd_s
```
- why: col 2 (sum) type OID pg=1700 stask=701; col 3 (avg) type OID pg=1700 stask=701; row 1 col sum: pg="144.50" stask="144.5"
- pg: `cols(count:20, sum:1700, avg:1700, pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["6", "144.50", "24.0833333333333333", "numeric", "numeric", "bigint", "bigint"] tag=SELECT 1`
- stask: `cols(count:20, sum:701, avg:701, pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["6", "144.5", "24.083333333333332", "double precision", "double precision", "bigint", "bigint"] tag=SELECT 1`

### DIFF — b/aggregate-group:218 (type+value)

```sql
SELECT carrier, SUM(amt) FROM pd_s GROUP BY 1
```
- why: col 2 (sum) type OID pg=1700 stask=701; row 1 col sum: pg="1.00" stask="1"
- pg: `cols(carrier:25, sum:1700) 4 rows [NULL, "1.00"] ["vina", "100.00"] ["viettel", "30.75"] ... tag=SELECT 4`
- stask: `cols(carrier:25, sum:701) 4 rows ["viettel", "30.75"] ["mobi", "12.75"] ["vina", "100"] ... tag=SELECT 4`

### DIFF — b/aggregate-group:219 (type)

```sql
SELECT region, SUM(amt) FROM pd_s GROUP BY region ORDER BY 1
```
- why: col 2 (sum) type OID pg=1700 stask=701
- pg: `cols(region:25, sum:1700) 2 rows ["n", "24.25"] ["s", "120.25"] tag=SELECT 2`
- stask: `cols(region:25, sum:701) 2 rows ["n", "24.25"] ["s", "120.25"] tag=SELECT 2`

### PASS-MSG — b/aggregate-group:220 (message)

```sql
SELECT id, SUM(amt) FROM pd_s GROUP BY region
```
- why: same SQLSTATE 42803, message differs
- pg: `ERROR 42803: column "pd_s.id" must appear in the GROUP BY clause or be used in an aggregate function`
- stask: `ERROR 42803: column "id" must appear in the GROUP BY clause or be used in an aggregate function`

### DIFF — b/aggregate-group:221 (type)

```sql
SELECT bool_and(qty > 0), bool_or(qty > 4), array_agg(id ORDER BY id), string_agg(carrier, ',' ORDER BY id) FROM pd_s
```
- why: col 3 (array_agg) type OID pg=1007 stask=1009
- pg: `cols(bool_and:16, bool_or:16, array_agg:1007, string_agg:25) 1 rows ["t", "t", "{1,2,3,4,5,6}", "viettel,viettel,mobi,mobi,vina"] tag=SELECT 1`
- stask: `cols(bool_and:16, bool_or:16, array_agg:1009, string_agg:25) 1 rows ["t", "t", "{1,2,3,4,5,6}", "viettel,viettel,mobi,mobi,vina"] tag=SELECT 1`

### DIFF — b/aggregate-group:222 (type+value)

```sql
SELECT stddev(amt), variance(amt) FROM pd_s
```
- why: col 1 (stddev) type OID pg=1700 stask=701; col 2 (variance) type OID pg=1700 stask=701; row 1 col stddev: pg="37.7527040974109120" stask="37.75270409741091"
- pg: `cols(stddev:1700, variance:1700) 1 rows ["37.7527040974109120", "1425.2666666666666667"] tag=SELECT 1`
- stask: `cols(stddev:701, variance:701) 1 rows ["37.75270409741091", "1425.2666666666669"] tag=SELECT 1`

### DIFF — b/aggregate-group:224 (type)

```sql
SELECT COUNT(*) FILTER (WHERE region = 'n'), SUM(amt) FILTER (WHERE carrier = 'mobi') FROM pd_s
```
- why: col 2 (sum) type OID pg=1700 stask=701
- pg: `cols(count:20, sum:1700) 1 rows ["4", "12.75"] tag=SELECT 1`
- stask: `cols(count:20, sum:701) 1 rows ["4", "12.75"] tag=SELECT 1`

### DIFF — b/aggregate-group:225 (type+value)

```sql
SELECT carrier, region, SUM(amt) FROM pd_s GROUP BY ROLLUP (carrier, region)
```
- why: col 3 (sum) type OID pg=1700 stask=701; row 1 col sum: pg="1.00" stask="1"
- pg: `cols(carrier:25, region:25, sum:1700) 10 rows [NULL, NULL, "144.50"] ["mobi", "n", "12.75"] [NULL, "n", "1.00"] ... tag=SELECT 10`
- stask: `cols(carrier:25, region:25, sum:701) 10 rows ["viettel", "n", "10.5"] ["viettel", NULL, "30.75"] [NULL, NULL, "144.5"] ... tag=SELECT 10`

### DIFF — b/aggregate-group:226 (type+value)

```sql
SELECT carrier, SUM(amt) FROM pd_s GROUP BY GROUPING SETS ((carrier), ())
```
- why: col 2 (sum) type OID pg=1700 stask=701; row 1 col sum: pg="1.00" stask="1"
- pg: `cols(carrier:25, sum:1700) 5 rows [NULL, "144.50"] [NULL, "1.00"] ["vina", "100.00"] ... tag=SELECT 5`
- stask: `cols(carrier:25, sum:701) 5 rows ["viettel", "30.75"] [NULL, "144.5"] ["mobi", "12.75"] ... tag=SELECT 5`

### DIFF — b/aggregate-group:229 (type)

```sql
SELECT DISTINCT ON (carrier) carrier, id FROM pd_s ORDER BY carrier, id DESC
```
- why: col 2 (id) type OID pg=23 stask=20
- pg: `cols(carrier:25, id:23) 4 rows ["mobi", "4"] ["viettel", "2"] ["vina", "5"] ... tag=SELECT 4`
- stask: `cols(carrier:25, id:20) 4 rows ["mobi", "4"] ["viettel", "2"] ["vina", "5"] ... tag=SELECT 4`

### DIFF — b/aggregate-group:230 (type)

```sql
SELECT SUM(amt) FROM pd_s WHERE id > 100
```
- why: col 1 (sum) type OID pg=1700 stask=701
- pg: `cols(sum:1700) 1 rows [NULL] tag=SELECT 1`
- stask: `cols(sum:701) 1 rows [NULL] tag=SELECT 1`

### DIFF — b/aggregate-group:232 (type+value)

```sql
SELECT AVG(qty)::numeric(10,2), ROUND(AVG(amt), 1) FROM pd_s
```
- why: col 1 (avg) type OID pg=1700 stask=701; col 2 (round) type OID pg=1700 stask=701; row 1 col avg: pg="2.40" stask="2.4"
- pg: `cols(avg:1700, round:1700) 1 rows ["2.40", "24.1"] tag=SELECT 1`
- stask: `cols(avg:701, round:701) 1 rows ["2.4", "24.1"] tag=SELECT 1`

### DIFF — b/order-limit:239 (type)

```sql
SELECT id FROM pd_o ORDER BY id LIMIT 5
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 5 rows ["1"] ["2"] ["3"] ... tag=SELECT 5`
- stask: `cols(id:20) 5 rows ["1"] ["2"] ["3"] ... tag=SELECT 5`

### DIFF — b/order-limit:240 (type)

```sql
SELECT id FROM pd_o ORDER BY id LIMIT 5 OFFSET 5
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 5 rows ["6"] ["7"] ["8"] ... tag=SELECT 5`
- stask: `cols(id:20) 5 rows ["6"] ["7"] ["8"] ... tag=SELECT 5`

### DIFF — b/order-limit:241 (type)

```sql
SELECT id FROM pd_o ORDER BY id OFFSET 18
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 2 rows ["19"] ["20"] tag=SELECT 2`
- stask: `cols(id:20) 2 rows ["19"] ["20"] tag=SELECT 2`

### DIFF — b/order-limit:243 (type)

```sql
SELECT id FROM pd_o ORDER BY id LIMIT ALL OFFSET 19
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 1 rows ["20"] tag=SELECT 1`
- stask: `cols(id:20) 1 rows ["20"] tag=SELECT 1`

### DIFF — b/order-limit:244 (type)

```sql
SELECT id FROM pd_o ORDER BY id FETCH FIRST 2 ROWS ONLY
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 2 rows ["1"] ["2"] tag=SELECT 2`
- stask: `cols(id:20) 2 rows ["1"] ["2"] tag=SELECT 2`

### DIFF — b/order-limit:245 (type)

```sql
SELECT v, id FROM pd_o ORDER BY v, id DESC LIMIT 6
```
- why: col 2 (id) type OID pg=23 stask=20
- pg: `cols(v:25, id:23) 6 rows ["a", "20"] ["a", "15"] ["a", "10"] ... tag=SELECT 6`
- stask: `cols(v:25, id:20) 6 rows ["a", "20"] ["a", "15"] ["a", "10"] ... tag=SELECT 6`

### ERR-ONLY-ONE-SIDE — b/order-limit:247 (semantics)

```sql
SELECT id FROM pd_o ORDER BY id LIMIT -1
```
- why: STASK accepts, PostgreSQL rejects: 2201W
- pg: `ERROR 2201W: LIMIT must not be negative`
- stask: `cols(id:20) 20 rows ["1"] ["2"] ["3"] ... tag=SELECT 20`

### DIFF — b/order-limit:248 (type)

```sql
SELECT id FROM pd_o ORDER BY v DESC, id LIMIT 3
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 3 rows ["4"] ["9"] ["14"] tag=SELECT 3`
- stask: `cols(id:20) 3 rows ["4"] ["9"] ["14"] tag=SELECT 3`

### DIFF — b/order-limit:249 (type)

```sql
SELECT id, v FROM pd_o ORDER BY lower(v), id LIMIT 3
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, v:25) 3 rows ["5", "a"] ["10", "a"] ["15", "a"] tag=SELECT 3`
- stask: `cols(id:20, v:25) 3 rows ["5", "a"] ["10", "a"] ["15", "a"] tag=SELECT 3`

### DIFF — b/order-limit:250 (sqlstate)

```sql
SELECT id FROM pd_o ORDER BY 5
```
- why: SQLSTATE pg=42P10 stask=42703
- pg: `ERROR 42P10: ORDER BY position 5 is not in select list`
- stask: `ERROR 42703: ORDER BY position 5 is not in select list`

### DIFF — b/order-limit:251 (type)

```sql
SELECT id AS x FROM pd_o ORDER BY x DESC LIMIT 2
```
- why: col 1 (x) type OID pg=23 stask=20
- pg: `cols(x:23) 2 rows ["20"] ["19"] tag=SELECT 2`
- stask: `cols(x:20) 2 rows ["20"] ["19"] tag=SELECT 2`

### DIFF — b/order-limit:253 (type)

```sql
SELECT id FROM pd_o ORDER BY id LIMIT $1 OFFSET $2
```
args: "3", "2"

- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 3 rows ["3"] ["4"] ["5"] tag=SELECT 3`
- stask: `cols(id:20) 3 rows ["3"] ["4"] ["5"] tag=SELECT 3`

### DIFF — b/order-limit:254 (type)

```sql
SELECT (SELECT MAX(id) FROM pd_o) - (SELECT MIN(id) FROM pd_o)
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["19"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["19"] tag=SELECT 1`

### DIFF — b/order-limit:255 (type)

```sql
SELECT id FROM pd_o WHERE id > 15 FOR UPDATE
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 5 rows ["16"] ["17"] ["18"] ... tag=SELECT 5`
- stask: `cols(id:20) 5 rows ["16"] ["17"] ["18"] ... tag=SELECT 5`

### DIFF — b/order-limit:256 (type)

```sql
SELECT id FROM pd_o ORDER BY id LIMIT 2 FOR UPDATE SKIP LOCKED
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23) 2 rows ["1"] ["2"] tag=SELECT 2`
- stask: `cols(id:20) 2 rows ["1"] ["2"] tag=SELECT 2`

### DIFF — b/window-cte:262 (type)

```sql
SELECT id, row_number() OVER (ORDER BY id) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, row_number:20) 5 rows ["1", "1"] ["2", "2"] ["3", "3"] ... tag=SELECT 5`
- stask: `cols(id:20, row_number:20) 5 rows ["1", "1"] ["2", "2"] ["3", "3"] ... tag=SELECT 5`

### DIFF — b/window-cte:263 (type)

```sql
SELECT id, row_number() OVER (PARTITION BY grp ORDER BY v DESC, id) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, row_number:20) 5 rows ["1", "3"] ["2", "1"] ["3", "2"] ... tag=SELECT 5`
- stask: `cols(id:20, row_number:20) 5 rows ["1", "3"] ["2", "1"] ["3", "2"] ... tag=SELECT 5`

### DIFF — b/window-cte:264 (type)

```sql
SELECT id, rank() OVER (PARTITION BY grp ORDER BY v), dense_rank() OVER (PARTITION BY grp ORDER BY v) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, rank:20, dense_rank:20) 5 rows ["1", "1", "1"] ["2", "2", "2"] ["3", "2", "2"] ... tag=SELECT 5`
- stask: `cols(id:20, rank:20, dense_rank:20) 5 rows ["1", "1", "1"] ["2", "2", "2"] ["3", "2", "2"] ... tag=SELECT 5`

### DIFF — b/window-cte:265 (type)

```sql
SELECT id, SUM(v) OVER (PARTITION BY grp), SUM(v) OVER (ORDER BY id) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, sum:20, sum:20) 5 rows ["1", "50", "10"] ["2", "50", "30"] ["3", "50", "50"] ... tag=SELECT 5`
- stask: `cols(id:20, sum:20, sum:20) 5 rows ["1", "50", "10"] ["2", "50", "30"] ["3", "50", "50"] ... tag=SELECT 5`

### DIFF — b/window-cte:266 (type)

```sql
SELECT id, lag(v) OVER (ORDER BY id), lead(v, 2, -1) OVER (ORDER BY id) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20; col 2 (lag) type OID pg=23 stask=20; col 3 (lead) type OID pg=23 stask=20
- pg: `cols(id:23, lag:23, lead:23) 5 rows ["1", NULL, "20"] ["2", "10", "5"] ["3", "20", "15"] ... tag=SELECT 5`
- stask: `cols(id:20, lag:20, lead:20) 5 rows ["1", NULL, "20"] ["2", "10", "5"] ["3", "20", "15"] ... tag=SELECT 5`

### DIFF — b/window-cte:267 (type)

```sql
SELECT id, first_value(v) OVER (PARTITION BY grp ORDER BY id), last_value(v) OVER (PARTITION BY grp ORDER BY id) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20; col 2 (first_value) type OID pg=23 stask=20; col 3 (last_value) type OID pg=23 stask=20
- pg: `cols(id:23, first_value:23, last_value:23) 5 rows ["1", "10", "10"] ["2", "10", "20"] ["3", "10", "20"] ... tag=SELECT 5`
- stask: `cols(id:20, first_value:20, last_value:20) 5 rows ["1", "10", "10"] ["2", "10", "20"] ["3", "10", "20"] ... tag=SELECT 5`

### DIFF — b/window-cte:268 (type+value)

```sql
SELECT id, AVG(v) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20; col 2 (avg) type OID pg=1700 stask=701; row 1 col avg: pg="10.0000000000000000" stask="10"
- pg: `cols(id:23, avg:1700) 5 rows ["1", "10.0000000000000000"] ["2", "15.0000000000000000"] ["3", "20.0000000000000000"] ... tag=SELECT 5`
- stask: `cols(id:20, avg:701) 5 rows ["1", "10"] ["2", "15"] ["3", "20"] ... tag=SELECT 5`

### DIFF — b/window-cte:269 (type)

```sql
SELECT id, ntile(2) OVER (ORDER BY id), percent_rank() OVER (ORDER BY id), cume_dist() OVER (ORDER BY id) FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20; col 2 (ntile) type OID pg=23 stask=20
- pg: `cols(id:23, ntile:23, percent_rank:701, cume_dist:701) 5 rows ["1", "1", "0", "0.2"] ["2", "1", "0.25", "0.4"] ["3", "1", "0.5", "0.6"] ... tag=SELECT 5`
- stask: `cols(id:20, ntile:20, percent_rank:701, cume_dist:701) 5 rows ["1", "1", "0", "0.2"] ["2", "1", "0.25", "0.4"] ["3", "1", "0.5", "0.6"] ... tag=SELECT 5`

### DIFF — b/window-cte:270 (type)

```sql
SELECT id, COUNT(*) OVER () FROM pd_w ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, count:20) 5 rows ["1", "5"] ["2", "5"] ["3", "5"] ... tag=SELECT 5`
- stask: `cols(id:20, count:20) 5 rows ["1", "5"] ["2", "5"] ["3", "5"] ... tag=SELECT 5`

### DIFF — b/window-cte:271 (type)

```sql
SELECT id, SUM(v) OVER w FROM pd_w WINDOW w AS (PARTITION BY grp) ORDER BY id
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, sum:20) 5 rows ["1", "50"] ["2", "50"] ["3", "50"] ... tag=SELECT 5`
- stask: `cols(id:20, sum:20) 5 rows ["1", "50"] ["2", "50"] ["3", "50"] ... tag=SELECT 5`

### DIFF — b/window-cte:273 (type)

```sql
WITH x AS (SELECT 1 AS a), y AS (SELECT a + 1 AS b FROM x) SELECT a, b FROM x, y
```
- why: col 1 (a) type OID pg=23 stask=20; col 2 (b) type OID pg=23 stask=20
- pg: `cols(a:23, b:23) 1 rows ["1", "2"] tag=SELECT 1`
- stask: `cols(a:20, b:20) 1 rows ["1", "2"] tag=SELECT 1`

### DIFF — b/window-cte:274 (type)

```sql
WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r WHERE n < 5) SELECT n FROM r
```
- why: col 1 (n) type OID pg=23 stask=20
- pg: `cols(n:23) 5 rows ["1"] ["2"] ["3"] ... tag=SELECT 5`
- stask: `cols(n:20) 5 rows ["1"] ["2"] ["3"] ... tag=SELECT 5`

### ERR-ONLY-ONE-SIDE — b/window-cte:275 (feature)

```sql
WITH ins AS (INSERT INTO pd_w VALUES (9, 'z', 1) RETURNING id) SELECT id FROM ins
```
- why: STASK rejects: 42601
- pg: `cols(id:23) 1 rows ["9"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "insert"`

### ERR-ONLY-ONE-SIDE — b/window-cte:276 (feature)

```sql
WITH del AS (DELETE FROM pd_w WHERE id = 9 RETURNING id) SELECT COUNT(*) FROM del
```
- why: STASK rejects: 42601
- pg: `cols(count:20) 1 rows ["1"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "delete"`

### ERR-ONLY-ONE-SIDE — b/window-cte:277 (feature)

```sql
WITH upd AS (UPDATE pd_w SET v = v + 1 WHERE id = 1 RETURNING v) SELECT v FROM upd
```
- why: STASK rejects: 42601
- pg: `cols(v:23) 1 rows ["11"] tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "update"`

### DIFF — b/window-cte:278 (type)

```sql
SELECT * FROM (SELECT grp, MAX(v) AS m FROM pd_w GROUP BY grp) s WHERE m > 10 ORDER BY grp
```
- why: col 2 (m) type OID pg=23 stask=20
- pg: `cols(grp:25, m:23) 2 rows ["a", "20"] ["b", "15"] tag=SELECT 2`
- stask: `cols(grp:25, m:20) 2 rows ["a", "20"] ["b", "15"] tag=SELECT 2`

### DIFF — b/window-cte:279 (type)

```sql
SELECT * FROM generate_series(1, 5) AS g(n) WHERE n % 2 = 1
```
- why: col 1 (n) type OID pg=23 stask=20
- pg: `cols(n:23) 3 rows ["1"] ["3"] ["5"] tag=SELECT 3`
- stask: `cols(n:20) 3 rows ["1"] ["3"] ["5"] tag=SELECT 3`

### DIFF — b/window-cte:280 (type)

```sql
SELECT g, g * g FROM generate_series(1, 4) g
```
- why: col 1 (g) type OID pg=23 stask=20; col 2 (?column?) type OID pg=23 stask=20
- pg: `cols(g:23, ?column?:23) 4 rows ["1", "1"] ["2", "4"] ["3", "9"] ... tag=SELECT 4`
- stask: `cols(g:20, ?column?:20) 4 rows ["1", "1"] ["2", "4"] ["3", "9"] ... tag=SELECT 4`

### DIFF — b/window-cte:281 (type)

```sql
SELECT * FROM (VALUES (1, 'a'), (2, 'b')) AS t(id, name)
```
- why: col 1 (id) type OID pg=23 stask=20
- pg: `cols(id:23, name:25) 2 rows ["1", "a"] ["2", "b"] tag=SELECT 2`
- stask: `cols(id:20, name:25) 2 rows ["1", "a"] ["2", "b"] tag=SELECT 2`

### DIFF — b/window-cte:282 (type)

```sql
SELECT g FROM generate_series(10, 1, -3) g
```
- why: col 1 (g) type OID pg=23 stask=20
- pg: `cols(g:23) 4 rows ["10"] ["7"] ["4"] ... tag=SELECT 4`
- stask: `cols(g:20) 4 rows ["10"] ["7"] ["4"] ... tag=SELECT 4`

### DIFF — b/window-cte:283 (type)

```sql
SELECT generate_series(1, 3) AS g, 'x'
```
- why: col 1 (g) type OID pg=23 stask=25
- pg: `cols(g:23, ?column?:25) 3 rows ["1", "x"] ["2", "x"] ["3", "x"] tag=SELECT 3`
- stask: `cols(g:25, ?column?:25) 3 rows ["1", "x"] ["2", "x"] ["3", "x"] tag=SELECT 3`

### DIFF — b/functions-misc:287 (type)

```sql
SELECT current_database() IS NOT NULL, current_schema(), current_user IS NOT NULL, session_user IS NOT NULL
```
- why: col 2 (current_schema) type OID pg=19 stask=25
- pg: `cols(?column?:16, current_schema:19, ?column?:16, ?column?:16) 1 rows ["t", "public", "t", "t"] tag=SELECT 1`
- stask: `cols(?column?:16, current_schema:25, ?column?:16, ?column?:16) 1 rows ["t", "public", "t", "t"] tag=SELECT 1`

### DIFF — b/functions-misc:288 (value)

```sql
SELECT pg_typeof(1)::text, pg_typeof('a'::text)::text, pg_typeof(now())::text, pg_typeof(1 = 1)::text, pg_typeof(1.5::float4)::text
```
- why: row 1 col pg_typeof: pg="integer" stask="bigint"
- pg: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["integer", "text", "timestamp with time zone", "boolean", "real"] tag=SELECT 1`
- stask: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["bigint", "text", "timestamptz", "boolean", "double precision"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/functions-misc:289 (feature)

```sql
SELECT random() >= 0 AND random() < 1, setseed(0.5) IS NULL
```
- why: STASK rejects: 42883
- pg: `cols(?column?:16, ?column?:16) 1 rows ["t", "f"] tag=SELECT 1`
- stask: `ERROR 42883: function setseed does not exist`

### ERR-ONLY-ONE-SIDE — b/functions-misc:293 (feature)

```sql
SELECT set_config('application.name', 'x', FALSE), current_setting('application.name')
```
- why: STASK rejects: 42704
- pg: `cols(set_config:25, current_setting:25) 1 rows ["x", "x"] tag=SELECT 1`
- stask: `ERROR 42704: unrecognized configuration parameter "application.name"`

### ERR-ONLY-ONE-SIDE — b/functions-misc:294 (feature)

```sql
SHOW server_version_num
```
- why: STASK rejects: 42704
- pg: `cols(server_version_num:25) 1 rows ["160015"] tag=SHOW`
- stask: `ERROR 42704: unrecognized configuration parameter "server_version_num"`

### DIFF — b/functions-misc:297 (columns)

```sql
SHOW DateStyle
```
- why: col 1 name pg="DateStyle" stask="datestyle"
- pg: `cols(DateStyle:25) 1 rows ["ISO, MDY"] tag=SHOW`
- stask: `cols(datestyle:25) 1 rows ["ISO, MDY"] tag=SHOW`

### DIFF — b/functions-misc:298 (value)

```sql
SHOW search_path
```
- why: row 1 col search_path: pg="\"$user\", public" stask="public"
- pg: `cols(search_path:25) 1 rows ["\"$user\", public"] tag=SHOW`
- stask: `cols(search_path:25) 1 rows ["public"] tag=SHOW`

### ERR-ONLY-ONE-SIDE — b/functions-misc:307 (feature)

```sql
RESET ALL
```
- why: STASK rejects: 0A000
- pg: `tag=RESET`
- stask: `ERROR 0A000: statement "RESET" is not supported`

### DIFF — b/functions-misc:308 (type)

```sql
SELECT 1 AS "Quoted Name", 2 AS lower, 3 AS "UPPER"
```
- why: col 1 (Quoted Name) type OID pg=23 stask=20; col 2 (lower) type OID pg=23 stask=20; col 3 (UPPER) type OID pg=23 stask=20
- pg: `cols(Quoted Name:23, lower:23, UPPER:23) 1 rows ["1", "2", "3"] tag=SELECT 1`
- stask: `cols(Quoted Name:20, lower:20, UPPER:20) 1 rows ["1", "2", "3"] tag=SELECT 1`

### DIFF — b/functions-misc:309 (type)

```sql
SELECT 1 AS UPPERCASE
```
- why: col 1 (uppercase) type OID pg=23 stask=20
- pg: `cols(uppercase:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(uppercase:20) 1 rows ["1"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — b/functions-misc:310 (feature)

```sql
SELECT
```
- why: STASK rejects: 42601
- pg: `tag=SELECT 1`
- stask: `ERROR 42601: syntax error at end of input`

### DIFF — b/functions-misc:311 (type)

```sql
SELECT 1, 1
```
- why: col 1 (?column?) type OID pg=23 stask=20; col 2 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23, ?column?:23) 1 rows ["1", "1"] tag=SELECT 1`
- stask: `cols(?column?:20, ?column?:20) 1 rows ["1", "1"] tag=SELECT 1`

### DIFF — b/functions-misc:313 (type)

```sql
SELECT 1 /* inline comment */ + 1; -- trailing comment
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["2"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["2"] tag=SELECT 1`

### DIFF — b/functions-misc:314 (type)

```sql
SELECT * FROM (SELECT 1) s
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### PASS-MSG — b/functions-misc:315 (message)

```sql
SELECT ?column?
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near "column"`
- stask: `ERROR 42601: syntax error at or near "?"`

### DIFF — b/functions-misc:318 (type)

```sql
SELECT $1 + 1
```
args: "5"

- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["6"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["6"] tag=SELECT 1`

### DIFF — b/functions-misc:322 (type+value)

```sql
SELECT $1::int + 1, pg_typeof($1)::text
```
args: "5"

- why: col 1 (?column?) type OID pg=23 stask=20; row 1 col pg_typeof: pg="integer" stask="bigint"
- pg: `cols(?column?:23, pg_typeof:25) 1 rows ["6", "integer"] tag=SELECT 1`
- stask: `cols(?column?:20, pg_typeof:25) 1 rows ["6", "bigint"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:5 (feature)

```sql
CREATE TABLE pd_e1 (id INTEGER PRIMARY KEY, email TEXT UNIQUE, code TEXT)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: column constraint UNIQUE is not supported`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:6 (semantics)

```sql
INSERT INTO pd_e1 VALUES (1, 'a@x', 'c1')
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 1`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### DIFF — c/unique-and-keys:7 (sqlstate)

```sql
INSERT INTO pd_e1 VALUES (1, 'b@x', 'c2')
```
- why: SQLSTATE pg=23505 stask=42P01
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_e1_pkey"`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### DIFF — c/unique-and-keys:8 (sqlstate)

```sql
INSERT INTO pd_e1 VALUES (2, 'a@x', 'c3')
```
- why: SQLSTATE pg=23505 stask=42P01
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_e1_email_key"`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:9 (semantics)

```sql
INSERT INTO pd_e1 VALUES (3, NULL, 'c4'), (4, NULL, 'c5')
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 2`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:10 (semantics)

```sql
SELECT COUNT(*) FROM pd_e1
```
- why: STASK rejects: 42P01
- pg: `cols(count:20) 1 rows ["3"] tag=SELECT 1`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### DIFF — c/unique-and-keys:11 (sqlstate)

```sql
INSERT INTO pd_e1 VALUES (NULL, 'z@x', 'c6')
```
- why: SQLSTATE pg=23502 stask=42P01
- pg: `ERROR 23502: null value in column "id" of relation "pd_e1" violates not-null constraint`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:12 (feature)

```sql
CREATE UNIQUE INDEX pd_e1_code ON pd_e1 (code)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE INDEX`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### DIFF — c/unique-and-keys:13 (sqlstate)

```sql
INSERT INTO pd_e1 VALUES (5, 'q@x', 'c1')
```
- why: SQLSTATE pg=23505 stask=42P01
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_e1_code"`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:14 (feature)

```sql
CREATE INDEX pd_e1_email ON pd_e1 (email)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE INDEX`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:15 (feature)

```sql
CREATE TABLE pd_e2 (id INTEGER PRIMARY KEY, e1 INTEGER REFERENCES pd_e1 (id))
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: column constraint REFERENCES is not supported`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:16 (semantics)

```sql
INSERT INTO pd_e2 VALUES (1, 1)
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 1`
- stask: `ERROR 42P01: relation "pd_e2" does not exist`

### DIFF — c/unique-and-keys:17 (sqlstate)

```sql
INSERT INTO pd_e2 VALUES (2, 99)
```
- why: SQLSTATE pg=23503 stask=42P01
- pg: `ERROR 23503: insert or update on table "pd_e2" violates foreign key constraint "pd_e2_e1_fkey"`
- stask: `ERROR 42P01: relation "pd_e2" does not exist`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:18 (semantics)

```sql
INSERT INTO pd_e2 VALUES (3, NULL)
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 1`
- stask: `ERROR 42P01: relation "pd_e2" does not exist`

### DIFF — c/unique-and-keys:19 (sqlstate)

```sql
DELETE FROM pd_e1 WHERE id = 1
```
- why: SQLSTATE pg=23503 stask=42P01
- pg: `ERROR 23503: update or delete on table "pd_e1" violates foreign key constraint "pd_e2_e1_fkey" on table "pd_e2"`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### DIFF — c/unique-and-keys:20 (sqlstate)

```sql
UPDATE pd_e1 SET id = 100 WHERE id = 1
```
- why: SQLSTATE pg=23503 stask=42P01
- pg: `ERROR 23503: update or delete on table "pd_e1" violates foreign key constraint "pd_e2_e1_fkey" on table "pd_e2"`
- stask: `ERROR 42P01: relation "pd_e1" does not exist`

### DIFF — c/unique-and-keys:21 (sqlstate)

```sql
DROP TABLE pd_e1
```
- why: SQLSTATE pg=2BP01 stask=42P01
- pg: `ERROR 2BP01: cannot drop table pd_e1 because other objects depend on it`
- stask: `ERROR 42P01: table "pd_e1" does not exist`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:22 (semantics)

```sql
CREATE TABLE pd_e1 (id INTEGER PRIMARY KEY)
```
- why: STASK accepts, PostgreSQL rejects: 42P07
- pg: `ERROR 42P07: relation "pd_e1" already exists`
- stask: `tag=CREATE TABLE`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:23 (feature)

```sql
CREATE TABLE pd_e3 (a INTEGER, b INTEGER, PRIMARY KEY (a, b))
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: composite primary keys are not supported`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:24 (semantics)

```sql
INSERT INTO pd_e3 VALUES (1, 1), (1, 2)
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 2`
- stask: `ERROR 42P01: relation "pd_e3" does not exist`

### DIFF — c/unique-and-keys:25 (sqlstate)

```sql
INSERT INTO pd_e3 VALUES (1, 1)
```
- why: SQLSTATE pg=23505 stask=42P01
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_e3_pkey"`
- stask: `ERROR 42P01: relation "pd_e3" does not exist`

### ERR-ONLY-ONE-SIDE — c/unique-and-keys:26 (semantics)

```sql
DROP TABLE pd_e3
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_e3" does not exist`

### PASS-MSG — c/missing-objects:45 (message)

```sql
SELECT nosuchfn(1)
```
- why: same SQLSTATE 42883, message differs
- pg: `ERROR 42883: function nosuchfn(integer) does not exist`
- stask: `ERROR 42883: function nosuchfn does not exist`

### PASS-MSG — c/missing-objects:46 (message)

```sql
SELECT upper()
```
- why: same SQLSTATE 42883, message differs
- pg: `ERROR 42883: function upper() does not exist`
- stask: `ERROR 42883: function upper with 0 arguments does not exist`

### PASS-MSG — c/missing-objects:47 (message)

```sql
SELECT upper(1, 2, 3)
```
- why: same SQLSTATE 42883, message differs
- pg: `ERROR 42883: function upper(integer, integer, integer) does not exist`
- stask: `ERROR 42883: function upper with 3 arguments does not exist`

### DIFF — c/missing-objects:51 (sqlstate)

```sql
DROP INDEX pd_no_such_idx
```
- why: SQLSTATE pg=42704 stask=42601
- pg: `ERROR 42704: index "pd_no_such_idx" does not exist`
- stask: `ERROR 42601: syntax error at or near "index" (expected TABLE)`

### DIFF — c/missing-objects:52 (sqlstate)

```sql
CREATE INDEX ON pd_no_such_table (a)
```
- why: SQLSTATE pg=42P01 stask=0A000
- pg: `ERROR 42P01: relation "pd_no_such_table" does not exist`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### PASS-MSG — c/missing-objects:53 (message)

```sql
TRUNCATE pd_no_such_table
```
- why: same SQLSTATE 42P01, message differs
- pg: `ERROR 42P01: relation "pd_no_such_table" does not exist`
- stask: `ERROR 42P01: table "pd_no_such_table" does not exist`

### DIFF — c/missing-objects:54 (sqlstate)

```sql
SELECT t.x FROM (SELECT 1 AS a) s
```
- why: SQLSTATE pg=42P01 stask=42703
- pg: `ERROR 42P01: missing FROM-clause entry for table "t"`
- stask: `ERROR 42703: column t.x does not exist`

### PASS-MSG — c/missing-objects:55 (message)

```sql
SELECT * FROM pd_no_schema.t
```
- why: same SQLSTATE 42P01, message differs
- pg: `ERROR 42P01: relation "pd_no_schema.t" does not exist`
- stask: `ERROR 42P01: schema "pd_no_schema" does not exist`

### ERR-ONLY-ONE-SIDE — c/missing-objects:56 (semantics)

```sql
SELECT x.* FROM (SELECT 1) s
```
- why: STASK accepts, PostgreSQL rejects: 42P01
- pg: `ERROR 42P01: missing FROM-clause entry for table "x"`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### DIFF — c/missing-objects:58 (sqlstate)

```sql
CREATE TABLE pd_dup (a INT, a INT)
```
- why: SQLSTATE pg=42701 stask=0A000
- pg: `ERROR 42701: column "a" specified more than once`
- stask: `ERROR 0A000: table "pd_dup" needs a single-column PRIMARY KEY`

### ERR-ONLY-ONE-SIDE — c/missing-objects:59 (feature)

```sql
CREATE TABLE pd_dup (a INT)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: table "pd_dup" needs a single-column PRIMARY KEY`

### DIFF — c/missing-objects:60 (sqlstate)

```sql
CREATE TABLE pd_dup (a INT)
```
- why: SQLSTATE pg=42P07 stask=0A000
- pg: `ERROR 42P07: relation "pd_dup" already exists`
- stask: `ERROR 0A000: table "pd_dup" needs a single-column PRIMARY KEY`

### ERR-ONLY-ONE-SIDE — c/missing-objects:61 (feature)

```sql
CREATE TABLE IF NOT EXISTS pd_dup (a INT)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: table "pd_dup" needs a single-column PRIMARY KEY`

### ERR-ONLY-ONE-SIDE — c/missing-objects:62 (semantics)

```sql
DROP TABLE pd_dup
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_dup" does not exist`

### DIFF — c/missing-objects:63 (sqlstate)

```sql
CREATE TABLE pd_bad (a NOSUCHTYPE)
```
- why: SQLSTATE pg=42704 stask=0A000
- pg: `ERROR 42704: type "nosuchtype" does not exist`
- stask: `ERROR 0A000: type "nosuchtype" is not supported`

### DIFF — c/missing-objects:64 (sqlstate)

```sql
SELECT 1::nosuchtype
```
- why: SQLSTATE pg=42704 stask=0A000
- pg: `ERROR 42704: type "nosuchtype" does not exist`
- stask: `ERROR 0A000: type "nosuchtype" is not supported`

### PASS-MSG — c/missing-objects:68 (message)

```sql
SELECT * FROM generate_series(1)
```
- why: same SQLSTATE 42883, message differs
- pg: `ERROR 42883: function generate_series(integer) does not exist`
- stask: `ERROR 42883: function generate_series with 1 arguments does not exist`

### DIFF — c/missing-objects:69 (sqlstate)

```sql
COMMENT ON TABLE pd_no_such_table IS 'x'
```
- why: SQLSTATE pg=42P01 stask=0A000
- pg: `ERROR 42P01: relation "pd_no_such_table" does not exist`
- stask: `ERROR 0A000: statement "COMMENT" is not supported`

### DIFF — c/syntax:72 (sqlstate)

```sql
SELEC 1
```
- why: SQLSTATE pg=42601 stask=0A000
- pg: `ERROR 42601: syntax error at or near "SELEC"`
- stask: `ERROR 0A000: statement "SELEC" is not supported`

### PASS-MSG — c/syntax:73 (message)

```sql
SELECT FROM
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at end of input`
- stask: `ERROR 42601: syntax error at or near "from"`

### PASS-MSG — c/syntax:75 (message)

```sql
SELECT (1
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at end of input`
- stask: `ERROR 42601: syntax error at or near "" (expected ")")`

### PASS-MSG — c/syntax:76 (message)

```sql
SELECT 1 FROM
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at end of input`
- stask: `ERROR 42601: syntax error at or near "" (expected an identifier)`

### PASS-MSG — c/syntax:77 (message)

```sql
SELECT * FORM t
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near "FORM"`
- stask: `ERROR 42601: syntax error at or near "form"`

### PASS-MSG — c/syntax:78 (message)

```sql
INSERT INTO VALUES (1)
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near "1"`
- stask: `ERROR 42601: syntax error at or near "values" (expected an identifier)`

### PASS-MSG — c/syntax:79 (message)

```sql
INSERT INTO t VALUES (1
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at end of input`
- stask: `ERROR 42601: syntax error at or near "" (expected ",")`

### PASS-MSG — c/syntax:80 (message)

```sql
UPDATE SET a = 1
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near "="`
- stask: `ERROR 42601: syntax error at or near "set" (expected an identifier)`

### PASS-MSG — c/syntax:81 (message)

```sql
DELETE t
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near "t"`
- stask: `ERROR 42601: syntax error at or near "t" (expected FROM)`

### PASS-MSG — c/syntax:82 (message)

```sql
CREATE TABLE ()
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near "("`
- stask: `ERROR 42601: syntax error at or near "(" (expected an identifier)`

### PASS-MSG — c/syntax:83 (message)

```sql
CREATE TABLE t (a INT,)
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near ")"`
- stask: `ERROR 42601: syntax error at or near ")" (expected an identifier)`

### DIFF — c/syntax:86 (sqlstate)

```sql
GARBAGE
```
- why: SQLSTATE pg=42601 stask=0A000
- pg: `ERROR 42601: syntax error at or near "GARBAGE"`
- stask: `ERROR 0A000: statement "GARBAGE" is not supported`

### DIFF — c/syntax:88 (type)

```sql
SELECT 1;
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### DIFF — c/syntax:89 (type)

```sql
SELECT * FROM (SELECT 1)
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### PASS-MSG — c/syntax:92 (message)

```sql
SELECT 1 ORDER
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at end of input`
- stask: `ERROR 42601: syntax error at or near "order"`

### PASS-MSG — c/syntax:94 (message)

```sql
SELECT 1 FROM pd_t AS
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at end of input`
- stask: `ERROR 42601: syntax error at or near "" (expected an identifier)`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:106 (semantics)

```sql
SELECT sqrt(-1)
```
- why: STASK accepts, PostgreSQL rejects: 2201F
- pg: `ERROR 2201F: cannot take square root of a negative number`
- stask: `cols(sqrt:701) 1 rows ["NaN"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:107 (semantics)

```sql
SELECT ln(0)
```
- why: STASK accepts, PostgreSQL rejects: 2201E
- pg: `ERROR 2201E: cannot take logarithm of zero`
- stask: `cols(ln:701) 1 rows ["-Infinity"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:108 (semantics)

```sql
SELECT ln(-1)
```
- why: STASK accepts, PostgreSQL rejects: 2201E
- pg: `ERROR 2201E: cannot take logarithm of a negative number`
- stask: `cols(ln:701) 1 rows ["NaN"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:109 (semantics)

```sql
SELECT power(0, -1)
```
- why: STASK accepts, PostgreSQL rejects: 2201F
- pg: `ERROR 2201F: zero raised to a negative power is undefined`
- stask: `cols(power:701) 1 rows ["Infinity"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:110 (semantics)

```sql
SELECT 2147483647 * 2
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: integer out of range`
- stask: `cols(?column?:20) 1 rows ["4294967294"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:111 (semantics)

```sql
SELECT 2147483648::int
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: integer out of range`
- stask: `cols(?column?:20) 1 rows ["2147483648"] tag=SELECT 1`

### PASS-MSG — c/arith-and-types:112 (message)

```sql
SELECT -(-9223372036854775808::bigint)
```
- why: same SQLSTATE 22003, message differs
- pg: `ERROR 22003: bigint out of range`
- stask: `ERROR 22003: value 9.223372036854776e+18 is not an integer`

### PASS-MSG — c/arith-and-types:113 (message)

```sql
SELECT 1 + 'a'
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "a"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "a"`

### DIFF — c/arith-and-types:114 (sqlstate)

```sql
SELECT 1 + 'a'::text
```
- why: SQLSTATE pg=42883 stask=22P02
- pg: `ERROR 42883: operator does not exist: integer + text`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "a"`

### PASS-MSG — c/arith-and-types:115 (message)

```sql
SELECT 'a' + 1
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "a"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "a"`

### DIFF — c/arith-and-types:116 (sqlstate)

```sql
SELECT 'a' - 'b'
```
- why: SQLSTATE pg=42725 stask=42883
- pg: `ERROR 42725: operator is not unique: unknown - unknown`
- stask: `ERROR 42883: operator does not exist: text - text`

### DIFF — c/arith-and-types:117 (sqlstate)

```sql
SELECT 1 || 2 + 'x'
```
- why: SQLSTATE pg=22P02 stask=42883
- pg: `ERROR 22P02: invalid input syntax for type integer: "x"`
- stask: `ERROR 42883: operator does not exist: text + text`

### PASS-MSG — c/arith-and-types:118 (message)

```sql
SELECT TRUE + 1
```
- why: same SQLSTATE 42883, message differs
- pg: `ERROR 42883: operator does not exist: boolean + integer`
- stask: `ERROR 42883: operator does not exist: boolean + bigint`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:119 (semantics)

```sql
SELECT 1 AND TRUE
```
- why: STASK accepts, PostgreSQL rejects: 42804
- pg: `ERROR 42804: argument of AND must be type boolean, not type integer`
- stask: `cols(?column?:16) 1 rows ["t"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:120 (semantics)

```sql
SELECT NOT 1
```
- why: STASK accepts, PostgreSQL rejects: 42804
- pg: `ERROR 42804: argument of NOT must be type boolean, not type integer`
- stask: `cols(?column?:16) 1 rows ["f"] tag=SELECT 1`

### PASS-MSG — c/arith-and-types:121 (message)

```sql
SELECT 'abc' = 1
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "abc"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "abc"`

### PASS-MSG — c/arith-and-types:124 (message)

```sql
SELECT 1 = '1x'
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "1x"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "1x"`

### PASS-MSG — c/arith-and-types:125 (message)

```sql
SELECT 1 < 'a'
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "a"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "a"`

### PASS-MSG — c/arith-and-types:126 (message)

```sql
SELECT 'a'::int
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "a"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "a"`

### PASS-MSG — c/arith-and-types:127 (message)

```sql
SELECT '1.5'::int
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type integer: "1.5"`
- stask: `ERROR 22P02: invalid input syntax for type bigint: "1.5"`

### DIFF — c/arith-and-types:128 (type)

```sql
SELECT 1.5::int2
```
- why: col 1 name pg="int2" stask="?column?"; col 1 (int2) type OID pg=21 stask=20
- pg: `cols(int2:21) 1 rows ["2"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["2"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:129 (semantics)

```sql
SELECT 99999::smallint
```
- why: STASK accepts, PostgreSQL rejects: 22003
- pg: `ERROR 22003: smallint out of range`
- stask: `cols(?column?:20) 1 rows ["99999"] tag=SELECT 1`

### DIFF — c/arith-and-types:131 (sqlstate)

```sql
SELECT 'x'::date
```
- why: SQLSTATE pg=22007 stask=22P02
- pg: `ERROR 22007: invalid input syntax for type date: "x"`
- stask: `ERROR 22P02: invalid input syntax for type date: "x"`

### PASS-MSG — c/arith-and-types:132 (message)

```sql
SELECT 'x'::numeric
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type numeric: "x"`
- stask: `ERROR 22P02: invalid input syntax for type double precision: "x"`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:135 (semantics)

```sql
SELECT substring('abc', 1, -1)
```
- why: STASK accepts, PostgreSQL rejects: 22011
- pg: `ERROR 22011: negative substring length not allowed`
- stask: `cols(substr:25) 1 rows [""] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:136 (semantics)

```sql
SELECT chr(0)
```
- why: STASK accepts, PostgreSQL rejects: 54000
- pg: `ERROR 54000: null character not permitted`
- stask: `cols(chr:25) 1 rows ["\x00"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:137 (semantics)

```sql
SELECT chr(-1)
```
- why: STASK accepts, PostgreSQL rejects: 22023
- pg: `ERROR 22023: character number must be positive`
- stask: `cols(chr:25) 1 rows ["�"] tag=SELECT 1`

### DIFF — c/arith-and-types:139 (sqlstate)

```sql
SELECT to_date('xx', 'YYYY')
```
- why: SQLSTATE pg=22007 stask=42883
- pg: `ERROR 22007: invalid value "xx" for "YYYY"`
- stask: `ERROR 42883: function to_date does not exist`

### DIFF — c/arith-and-types:140 (value)

```sql
SELECT to_char(1, 'ZZZ')
```
- why: row 1 col to_char: pg="ZZZ" stask=""
- pg: `cols(to_char:25) 1 rows ["ZZZ"] tag=SELECT 1`
- stask: `cols(to_char:25) 1 rows [""] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:141 (semantics)

```sql
SELECT array_length(1, 1)
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: function array_length(integer, integer) does not exist`
- stask: `cols(array_length:20) 1 rows ["0"] tag=SELECT 1`

### DIFF — c/arith-and-types:142 (type)

```sql
SELECT (ARRAY[1,2])[3]
```
- why: col 1 name pg="array" stask="stask_subscript"; col 1 (array) type OID pg=23 stask=25
- pg: `cols(array:23) 1 rows [NULL] tag=SELECT 1`
- stask: `cols(stask_subscript:25) 1 rows [NULL] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:143 (semantics)

```sql
SELECT ARRAY[1, 'a']
```
- why: STASK accepts, PostgreSQL rejects: 22P02
- pg: `ERROR 22P02: invalid input syntax for type integer: "a"`
- stask: `cols(array:1009) 1 rows ["{1,a}"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:144 (semantics)

```sql
SELECT '{1,2'::int[]
```
- why: STASK accepts, PostgreSQL rejects: 22P02
- pg: `ERROR 22P02: malformed array literal: "{1,2"`
- stask: `cols(?column?:25) 1 rows ["{1,2"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:149 (semantics)

```sql
SELECT 1 IN (SELECT 1, 2)
```
- why: STASK accepts, PostgreSQL rejects: 42601
- pg: `ERROR 42601: subquery has too many columns`
- stask: `cols(?column?:16) 1 rows ["f"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:150 (semantics)

```sql
SELECT 1 FROM (VALUES (1)) t(a) WHERE a
```
- why: STASK accepts, PostgreSQL rejects: 42804
- pg: `ERROR 42804: argument of WHERE must be type boolean, not type integer`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:153 (semantics)

```sql
SELECT a, COUNT(*) FROM (VALUES (1)) t(a)
```
- why: STASK accepts, PostgreSQL rejects: 42803
- pg: `ERROR 42803: column "t.a" must appear in the GROUP BY clause or be used in an aggregate function`
- stask: `cols(a:20, count:20) 1 rows ["1", "1"] tag=SELECT 1`

### DIFF — c/arith-and-types:155 (sqlstate)

```sql
SELECT sum('a')
```
- why: SQLSTATE pg=42725 stask=42883
- pg: `ERROR 42725: function sum(unknown) is not unique`
- stask: `ERROR 42883: function sum(text) does not exist`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:156 (semantics)

```sql
SELECT sum(TRUE)
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: function sum(boolean) does not exist`
- stask: `cols(sum:701) 1 rows ["1"] tag=SELECT 1`

### DIFF — c/arith-and-types:158 (type)

```sql
SELECT max(ARRAY[1])
```
- why: col 1 (max) type OID pg=1007 stask=1009
- pg: `cols(max:1007) 1 rows ["{1}"] tag=SELECT 1`
- stask: `cols(max:1009) 1 rows ["{1}"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:159 (semantics)

```sql
SELECT CASE WHEN 1 THEN 1 END
```
- why: STASK accepts, PostgreSQL rejects: 42804
- pg: `ERROR 42804: argument of CASE/WHEN must be type boolean, not type integer`
- stask: `cols(case:20) 1 rows ["1"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:160 (semantics)

```sql
SELECT COALESCE()
```
- why: STASK accepts, PostgreSQL rejects: 42601
- pg: `ERROR 42601: syntax error at or near ")"`
- stask: `cols(coalesce:25) 1 rows [NULL] tag=SELECT 1`

### DIFF — c/arith-and-types:161 (sqlstate)

```sql
SELECT NULLIF(1)
```
- why: SQLSTATE pg=42601 stask=42883
- pg: `ERROR 42601: syntax error at or near ")"`
- stask: `ERROR 42883: function nullif with 1 arguments does not exist`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:163 (semantics)

```sql
SELECT length(1)
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: function length(integer) does not exist`
- stask: `cols(length:20) 1 rows ["1"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:164 (semantics)

```sql
SELECT upper(1)
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: function upper(integer) does not exist`
- stask: `cols(upper:25) 1 rows ["1"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:166 (semantics)

```sql
SELECT now(1)
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: function now(integer) does not exist`
- stask: `cols(now:1184) 1 rows ["2026-10-29 14:18:00.266+00"] tag=SELECT 1`

### PASS-MSG — c/arith-and-types:167 (message)

```sql
SELECT extract(foo FROM now())
```
- why: same SQLSTATE 22023, message differs
- pg: `ERROR 22023: unit "foo" not recognized for type timestamp with time zone`
- stask: `ERROR 22023: unit "foo" not recognized for type timestamp`

### PASS-MSG — c/arith-and-types:168 (message)

```sql
SELECT date_trunc('foo', now())
```
- why: same SQLSTATE 22023, message differs
- pg: `ERROR 22023: unit "foo" not recognized for type timestamp with time zone`
- stask: `ERROR 22023: unit "foo" not recognized`

### DIFF — c/arith-and-types:170 (sqlstate)

```sql
SELECT now() - 'x'
```
- why: SQLSTATE pg=22007 stask=42883
- pg: `ERROR 22007: invalid input syntax for type timestamp with time zone: "x"`
- stask: `ERROR 42883: operator does not exist: timestamptz - text`

### DIFF — c/arith-and-types:171 (sqlstate)

```sql
SELECT INTERVAL 'x'
```
- why: SQLSTATE pg=22007 stask=22P02
- pg: `ERROR 22007: invalid input syntax for type interval: "x"`
- stask: `ERROR 22P02: invalid input syntax for type interval: "x"`

### DIFF — c/arith-and-types:172 (sqlstate)

```sql
SELECT '1 day'::interval::int
```
- why: SQLSTATE pg=42846 stask=42804
- pg: `ERROR 42846: cannot cast type interval to integer`
- stask: `ERROR 42804: cannot convert interval to bigint`

### DIFF — c/arith-and-types:173 (sqlstate)

```sql
SELECT '{"a":1}'::jsonb -> 1.5
```
- why: SQLSTATE pg=42883 stask=42601
- pg: `ERROR 42883: operator does not exist: jsonb -> numeric`
- stask: `ERROR 42601: syntax error at or near "->"`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:174 (semantics)

```sql
SELECT 'abc' LIKE 1
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: operator does not exist: unknown ~~ integer`
- stask: `cols(?column?:16) 1 rows ["f"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/arith-and-types:175 (semantics)

```sql
SELECT 1 LIKE 'a'
```
- why: STASK accepts, PostgreSQL rejects: 42883
- pg: `ERROR 42883: operator does not exist: integer ~~ unknown`
- stask: `cols(?column?:16) 1 rows ["f"] tag=SELECT 1`

### PASS-MSG — c/arith-and-types:176 (message)

```sql
SELECT 'abc' ~ '('
```
- why: same SQLSTATE 2201B, message differs
- pg: `ERROR 2201B: invalid regular expression: parentheses () not balanced`
- stask: `ERROR 2201B: invalid regular expression: error parsing regexp: missing closing ): `(``

### PASS-MSG — c/arith-and-types:177 (message)

```sql
SELECT regexp_replace('a', '(', 'b')
```
- why: same SQLSTATE 2201B, message differs
- pg: `ERROR 2201B: invalid regular expression: parentheses () not balanced`
- stask: `ERROR 2201B: invalid regular expression: error parsing regexp: missing closing ): `(``

### DIFF — c/arith-and-types:178 (sqlstate)

```sql
SELECT 'a' SIMILAR TO '('
```
- why: SQLSTATE pg=2201B stask=42601
- pg: `ERROR 2201B: invalid regular expression: parentheses () not balanced`
- stask: `ERROR 42601: syntax error at or near "to"`

### PASS-MSG — c/transaction-errors:185 (message)

```sql
INSERT INTO pd_te VALUES (1)
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_te_pkey"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "pd_te_pkey": Key (id)=(1) already exists`

### ERR-ONLY-ONE-SIDE — c/transaction-errors:192 (feature)

```sql
SAVEPOINT a
```
- why: STASK rejects: 0A000
- pg: `tag=SAVEPOINT`
- stask: `ERROR 0A000: statement "SAVEPOINT" is not supported`

### PASS-MSG — c/transaction-errors:193 (message)

```sql
INSERT INTO pd_te VALUES (1)
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "pd_te_pkey"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "pd_te_pkey": Key (id)=(1) already exists`

### ERR-ONLY-ONE-SIDE — c/transaction-errors:194 (feature)

```sql
ROLLBACK TO a
```
- why: STASK rejects: 42601
- pg: `tag=ROLLBACK`
- stask: `ERROR 42601: syntax error at or near "to"`

### ERR-ONLY-ONE-SIDE — c/transaction-errors:195 (semantics)

```sql
INSERT INTO pd_te VALUES (2)
```
- why: STASK rejects: 25P02
- pg: `tag=INSERT 0 1`
- stask: `ERROR 25P02: current transaction is aborted, commands ignored until end of transaction block`

### DIFF — c/transaction-errors:196 (tag)

```sql
COMMIT
```
- why: command tag pg="COMMIT" stask="ROLLBACK"
- pg: `tag=COMMIT`
- stask: `tag=ROLLBACK`

### DIFF — c/transaction-errors:197 (columns)

```sql
SELECT id FROM pd_te ORDER BY id
```
- why: column count pg=1 stask=0; command tag pg="SELECT 2" stask="SELECT 0"
- pg: `cols(id:23) 2 rows ["1"] ["2"] tag=SELECT 2`
- stask: `tag=SELECT 0`

### DIFF — c/transaction-errors:198 (sqlstate)

```sql
ROLLBACK TO SAVEPOINT nosuch
```
- why: SQLSTATE pg=25P01 stask=42601
- pg: `ERROR 25P01: ROLLBACK TO SAVEPOINT can only be used in transaction blocks`
- stask: `ERROR 42601: syntax error at or near "to"`

### DIFF — c/transaction-errors:199 (sqlstate)

```sql
RELEASE SAVEPOINT nosuch
```
- why: SQLSTATE pg=25P01 stask=0A000
- pg: `ERROR 25P01: RELEASE SAVEPOINT can only be used in transaction blocks`
- stask: `ERROR 0A000: statement "RELEASE" is not supported`

### DIFF — c/transaction-errors:200 (sqlstate)

```sql
SAVEPOINT x
```
- why: SQLSTATE pg=25P01 stask=0A000
- pg: `ERROR 25P01: SAVEPOINT can only be used in transaction blocks`
- stask: `ERROR 0A000: statement "SAVEPOINT" is not supported`

### DIFF — c/transaction-errors:208 (type)

```sql
SELECT 1
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — c/transaction-errors:210 (semantics)

```sql
INSERT INTO pd_te VALUES (50)
```
- why: STASK accepts, PostgreSQL rejects: 25006
- pg: `ERROR 25006: cannot execute INSERT in a read-only transaction`
- stask: `tag=INSERT 0 1`

### DIFF — c/transaction-errors:215 (type)

```sql
SELECT 1
```
- why: col 1 (?column?) type OID pg=23 stask=20
- pg: `cols(?column?:23) 1 rows ["1"] tag=SELECT 1`
- stask: `cols(?column?:20) 1 rows ["1"] tag=SELECT 1`

### PASS-MSG — c/transaction-errors:217 (message)

```sql
BEGIN ISOLATION LEVEL BOGUS
```
- why: same SQLSTATE 42601, message differs
- pg: `ERROR 42601: syntax error at or near "BOGUS"`
- stask: `ERROR 42601: unknown isolation level "bogus"`

### DIFF — c/transaction-errors:218 (sqlstate)

```sql
LOCK TABLE pd_te IN ACCESS EXCLUSIVE MODE
```
- why: SQLSTATE pg=25P01 stask=0A000
- pg: `ERROR 25P01: LOCK TABLE can only be used in transaction blocks`
- stask: `ERROR 0A000: statement "LOCK" is not supported`

### ERR-ONLY-ONE-SIDE — c/transaction-errors:220 (feature)

```sql
LOCK TABLE pd_te IN ACCESS EXCLUSIVE MODE
```
- why: STASK rejects: 0A000
- pg: `tag=LOCK TABLE`
- stask: `ERROR 0A000: statement "LOCK" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:231 (feature)

```sql
CREATE SCHEMA pd_sch
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE SCHEMA`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:232 (semantics)

```sql
CREATE TABLE pd_sch.t (a INT)
```
- why: STASK rejects: 42P01
- pg: `tag=CREATE TABLE`
- stask: `ERROR 42P01: schema "pd_sch" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:233 (semantics)

```sql
INSERT INTO pd_sch.t VALUES (1)
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 1`
- stask: `ERROR 42P01: schema "pd_sch" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:234 (semantics)

```sql
SELECT a FROM pd_sch.t
```
- why: STASK rejects: 42P01
- pg: `cols(a:23) 1 rows ["1"] tag=SELECT 1`
- stask: `ERROR 42P01: schema "pd_sch" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:235 (feature)

```sql
DROP SCHEMA pd_sch CASCADE
```
- why: STASK rejects: 42601
- pg: `tag=DROP SCHEMA`
- stask: `ERROR 42601: syntax error at or near "schema" (expected TABLE)`

### DIFF — c/permissions-and-ddl-misc:237 (type)

```sql
SELECT nextval('pd_seq'), nextval('pd_seq'), currval('pd_seq')
```
- why: col 1 (nextval) type OID pg=20 stask=25; col 2 (nextval) type OID pg=20 stask=25; col 3 (currval) type OID pg=20 stask=25
- pg: `cols(nextval:20, nextval:20, currval:20) 1 rows ["1", "2", "2"] tag=SELECT 1`
- stask: `cols(nextval:25, nextval:25, currval:25) 1 rows ["1", "2", "2"] tag=SELECT 1`

### PASS-MSG — c/permissions-and-ddl-misc:239 (message)

```sql
SELECT nextval('pd_seq_nosuch')
```
- why: same SQLSTATE 42P01, message differs
- pg: `ERROR 42P01: relation "pd_seq_nosuch" does not exist`
- stask: `ERROR 42P01: sequence does not exist: pd_seq_nosuch`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:242 (semantics)

```sql
INSERT INTO pd_vw VALUES (1)
```
- why: STASK rejects: 55000
- pg: `tag=INSERT 0 1`
- stask: `ERROR 55000: cannot change view "pd_vw"`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:245 (feature)

```sql
CREATE EXTENSION IF NOT EXISTS "uuid-ossp"
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE EXTENSION`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### PASS-MSG — c/permissions-and-ddl-misc:246 (message)

```sql
CREATE EXTENSION nosuchext
```
- why: same SQLSTATE 0A000, message differs
- pg: `ERROR 0A000: extension "nosuchext" is not available`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:247 (feature)

```sql
CREATE FUNCTION pd_f(a int) RETURNS int AS $$ SELECT a + 1 $$ LANGUAGE sql
```
- why: STASK rejects: 42601
- pg: `tag=CREATE FUNCTION`
- stask: `ERROR 42601: unexpected character '$'`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:248 (feature)

```sql
SELECT pd_f(1)
```
- why: STASK rejects: 42883
- pg: `cols(pd_f:23) 1 rows ["2"] tag=SELECT 1`
- stask: `ERROR 42883: function pd_f does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:249 (feature)

```sql
DROP FUNCTION pd_f(int)
```
- why: STASK rejects: 42601
- pg: `tag=DROP FUNCTION`
- stask: `ERROR 42601: syntax error at or near "function" (expected TABLE)`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:250 (feature)

```sql
CREATE TYPE pd_mood AS ENUM ('sad', 'ok', 'happy')
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TYPE`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### DIFF — c/permissions-and-ddl-misc:251 (sqlstate)

```sql
SELECT 'sad'::pd_mood, 'x'::pd_mood
```
- why: SQLSTATE pg=22P02 stask=0A000
- pg: `ERROR 22P02: invalid input value for enum pd_mood: "x"`
- stask: `ERROR 0A000: type "pd_mood" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:252 (feature)

```sql
DROP TYPE pd_mood
```
- why: STASK rejects: 42601
- pg: `tag=DROP TYPE`
- stask: `ERROR 42601: syntax error at or near "type" (expected TABLE)`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:253 (feature)

```sql
CREATE TABLE pd_pm2 (id INTEGER PRIMARY KEY, c TEXT) PARTITION BY RANGE (id)
```
- why: STASK rejects: 42601
- pg: `tag=CREATE TABLE`
- stask: `ERROR 42601: syntax error at or near "partition"`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:254 (semantics)

```sql
DROP TABLE pd_pm2
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_pm2" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:255 (feature)

```sql
CREATE TEMP TABLE pd_tmp (a INT)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:256 (semantics)

```sql
INSERT INTO pd_tmp VALUES (1)
```
- why: STASK rejects: 42P01
- pg: `tag=INSERT 0 1`
- stask: `ERROR 42P01: relation "pd_tmp" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:257 (semantics)

```sql
SELECT a FROM pd_tmp
```
- why: STASK rejects: 42P01
- pg: `cols(a:23) 1 rows ["1"] tag=SELECT 1`
- stask: `ERROR 42P01: relation "pd_tmp" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:258 (semantics)

```sql
DROP TABLE pd_tmp
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_tmp" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:259 (feature)

```sql
CREATE UNLOGGED TABLE pd_unl (a INT)
```
- why: STASK rejects: 0A000
- pg: `tag=CREATE TABLE`
- stask: `ERROR 0A000: only CREATE TABLE is supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:260 (semantics)

```sql
DROP TABLE pd_unl
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_unl" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:261 (feature)

```sql
ANALYZE pd_pm
```
- why: STASK rejects: 0A000
- pg: `tag=ANALYZE`
- stask: `ERROR 0A000: statement "ANALYZE" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:262 (feature)

```sql
VACUUM pd_pm
```
- why: STASK rejects: 0A000
- pg: `tag=VACUUM`
- stask: `ERROR 0A000: statement "VACUUM" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:263 (feature)

```sql
EXPLAIN SELECT 1
```
- why: STASK rejects: 0A000
- pg: `cols(QUERY PLAN:25) 1 rows ["Result (cost=0.00..0.01 rows=1 width=4)"] tag=EXPLAIN`
- stask: `ERROR 0A000: statement "EXPLAIN" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:264 (feature)

```sql
COMMENT ON TABLE pd_pm IS 'hello'
```
- why: STASK rejects: 0A000
- pg: `tag=COMMENT`
- stask: `ERROR 0A000: statement "COMMENT" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:265 (feature)

```sql
COMMENT ON COLUMN pd_pm.id IS 'pk'
```
- why: STASK rejects: 0A000
- pg: `tag=COMMENT`
- stask: `ERROR 0A000: statement "COMMENT" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:266 (feature)

```sql
CREATE TABLE pd_like (LIKE pd_pm)
```
- why: STASK rejects: 42601
- pg: `tag=CREATE TABLE`
- stask: `ERROR 42601: syntax error at or near "like" (expected an identifier)`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:267 (semantics)

```sql
DROP TABLE pd_like
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_like" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:268 (feature)

```sql
CREATE TABLE pd_ctas AS SELECT id FROM pd_pm
```
- why: STASK rejects: 42601
- pg: `tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "as" (expected "(")`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:269 (semantics)

```sql
SELECT COUNT(*) FROM pd_ctas
```
- why: STASK rejects: 42P01
- pg: `cols(count:20) 1 rows ["1"] tag=SELECT 1`
- stask: `ERROR 42P01: relation "pd_ctas" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:270 (semantics)

```sql
DROP TABLE pd_ctas
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_ctas" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:271 (feature)

```sql
SELECT * INTO pd_into FROM pd_pm
```
- why: STASK rejects: 42601
- pg: `tag=SELECT 1`
- stask: `ERROR 42601: syntax error at or near "into"`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:272 (semantics)

```sql
DROP TABLE pd_into
```
- why: STASK rejects: 42P01
- pg: `tag=DROP TABLE`
- stask: `ERROR 42P01: table "pd_into" does not exist`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:273 (feature)

```sql
DO $$ BEGIN PERFORM 1; END $$
```
- why: STASK rejects: 42601
- pg: `tag=DO`
- stask: `ERROR 42601: unexpected character '$'`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:278 (feature)

```sql
DISCARD ALL
```
- why: STASK rejects: 0A000
- pg: `tag=DISCARD ALL`
- stask: `ERROR 0A000: statement "DISCARD" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:279 (feature)

```sql
CHECKPOINT
```
- why: STASK rejects: 0A000
- pg: `tag=CHECKPOINT`
- stask: `ERROR 0A000: statement "CHECKPOINT" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:280 (feature)

```sql
PREPARE pd_p1 AS SELECT 1
```
- why: STASK rejects: 0A000
- pg: `tag=PREPARE`
- stask: `ERROR 0A000: statement "PREPARE" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:281 (feature)

```sql
EXECUTE pd_p1
```
- why: STASK rejects: 0A000
- pg: `cols(?column?:23) 1 rows ["1"] tag=SELECT 1`
- stask: `ERROR 0A000: statement "EXECUTE" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:282 (feature)

```sql
DEALLOCATE pd_p1
```
- why: STASK rejects: 0A000
- pg: `tag=DEALLOCATE`
- stask: `ERROR 0A000: statement "DEALLOCATE" is not supported`

### DIFF — c/permissions-and-ddl-misc:283 (sqlstate)

```sql
EXECUTE pd_nosuchprep
```
- why: SQLSTATE pg=26000 stask=0A000
- pg: `ERROR 26000: prepared statement "pd_nosuchprep" does not exist`
- stask: `ERROR 0A000: statement "EXECUTE" is not supported`

### DIFF — c/permissions-and-ddl-misc:284 (sqlstate)

```sql
DECLARE pd_c CURSOR FOR SELECT 1
```
- why: SQLSTATE pg=25P01 stask=0A000
- pg: `ERROR 25P01: DECLARE CURSOR can only be used in transaction blocks`
- stask: `ERROR 0A000: statement "DECLARE" is not supported`

### ERR-ONLY-ONE-SIDE — c/permissions-and-ddl-misc:285 (feature)

```sql
CLOSE ALL
```
- why: STASK rejects: 0A000
- pg: `tag=CLOSE CURSOR ALL`
- stask: `ERROR 0A000: statement "CLOSE" is not supported`

### DIFF — d/sim-setup:8 (type+value)

```sql
SELECT number, price FROM sims ORDER BY price DESC, number LIMIT 3
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="12000000.00" stask="12000000"
- pg: `cols(number:25, price:1700) 3 rows ["0912345678", "12000000.00"] ["0903000004", "3000000.00"] ["0901000001", "1500000.00"] tag=SELECT 3`
- stask: `cols(number:25, price:701) 3 rows ["0912345678", "12000000"] ["0903000004", "3000000"] ["0901000001", "1500000"] tag=SELECT 3`

### DIFF — d/sim-setup:21 (type+value)

```sql
SELECT number, price FROM sims WHERE carrier = $1 AND status = $2 AND price BETWEEN $3::numeric AND $4::numeric ORDER BY price
```
args: "viettel", "available", "1000000", "20000000"

- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="1500000.00" stask="1500000"
- pg: `cols(number:25, price:1700) 2 rows ["0901000001", "1500000.00"] ["0912345678", "12000000.00"] tag=SELECT 2`
- stask: `cols(number:25, price:701) 2 rows ["0901000001", "1500000"] ["0912345678", "12000000"] tag=SELECT 2`

### DIFF — d/sim-setup:23 (type+value)

```sql
SELECT carrier, COUNT(*), SUM(price), MIN(price), MAX(price), AVG(price) FROM sims GROUP BY carrier ORDER BY carrier
```
- why: col 3 (sum) type OID pg=1700 stask=701; col 4 (min) type OID pg=1700 stask=701; col 5 (max) type OID pg=1700 stask=701; col 6 (avg) type OID pg=1700 stask=701; row 1 col sum: pg="250000.00" stask="250000"
- pg: `cols(carrier:25, count:20, sum:1700, min:1700, max:1700, avg:1700) 3 rows ["mobifone", "2", "250000.00", "0.00", "250000.00", "125000.000000000000"] ["viettel", "4", "14800000.50", "500000.50", "12000000.00", "3700000.125000000000"] ["vinaphone", "2", "3099000.00", "99000.00", "3000000.00", "1549500.000000000000"] tag=SELECT 3`
- stask: `cols(carrier:25, count:20, sum:701, min:701, max:701, avg:701) 3 rows ["mobifone", "2", "250000", "0", "250000", "125000"] ["viettel", "4", "14800000.5", "500000.5", "12000000", "3700000.125"] ["vinaphone", "2", "3099000", "99000", "3000000", "1549500"] tag=SELECT 3`

### DIFF — d/sim-setup:27 (type+value)

```sql
SELECT price FROM sims WHERE number = '0901000002'
```
- why: col 1 (price) type OID pg=1700 stask=701; row 1 col price: pg="500000.50" stask="500000.5"
- pg: `cols(price:1700) 1 rows ["500000.50"] tag=SELECT 1`
- stask: `cols(price:701) 1 rows ["500000.5"] tag=SELECT 1`

### DIFF — d/sim-setup:28 (type+value)

```sql
SELECT price * 2, price + 0.5, price::bigint, price::text, round(price / 3, 2) FROM sims WHERE number = '0901000002'
```
- why: col 1 (?column?) type OID pg=1700 stask=701; col 2 (?column?) type OID pg=1700 stask=701; col 5 (round) type OID pg=1700 stask=701; row 1 col ?column?: pg="1000001.00" stask="1000001"
- pg: `cols(?column?:1700, ?column?:1700, price:20, price:25, round:1700) 1 rows ["1000001.00", "500001.00", "500001", "500000.50", "166666.83"] tag=SELECT 1`
- stask: `cols(?column?:701, ?column?:701, price:20, price:25, round:701) 1 rows ["1000001", "500001", "500000", "500000.5", "166666.83"] tag=SELECT 1`

### DIFF — d/sim-setup:29 (type+value)

```sql
SELECT SUM(price), AVG(price) FROM sims WHERE status = 'available'
```
- why: col 1 (sum) type OID pg=1700 stask=701; col 2 (avg) type OID pg=1700 stask=701; row 1 col sum: pg="14349000.50" stask="14349000.5"
- pg: `cols(sum:1700, avg:1700) 1 rows ["14349000.50", "2391500.083333333333"] tag=SELECT 1`
- stask: `cols(sum:701, avg:701) 1 rows ["14349000.5", "2391500.0833333335"] tag=SELECT 1`

### DIFF — d/sim-setup:30 (value)

```sql
SELECT pg_typeof(price)::text, pg_typeof(updated_at)::text, pg_typeof(number)::text FROM sims LIMIT 1
```
- why: row 1 col pg_typeof: pg="numeric" stask="double precision"
- pg: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["numeric", "timestamp with time zone", "text"] tag=SELECT 1`
- stask: `cols(pg_typeof:25, pg_typeof:25, pg_typeof:25) 1 rows ["double precision", "timestamptz", "text"] tag=SELECT 1`

### DIFF — d/sim-setup:37 (type+value)

```sql
SELECT number, price, rank() OVER (PARTITION BY carrier ORDER BY price DESC) AS r FROM sims ORDER BY carrier, r, number
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="250000.00" stask="250000"
- pg: `cols(number:25, price:1700, r:20) 8 rows ["0902000003", "250000.00", "1"] ["0777000111", "0.00", "2"] ["0912345678", "12000000.00", "1"] ... tag=SELECT 8`
- stask: `cols(number:25, price:701, r:20) 8 rows ["0902000003", "250000", "1"] ["0777000111", "0", "2"] ["0912345678", "12000000", "1"] ... tag=SELECT 8`

### DIFF — d/sim-setup:45 (value)

```sql
SELECT to_char(price, 'FM999,999,999.00') FROM sims WHERE number = '0912345678'
```
- why: row 1 col to_char: pg="12,000,000.00" stask="12,000,000."
- pg: `cols(to_char:25) 1 rows ["12,000,000.00"] tag=SELECT 1`
- stask: `cols(to_char:25) 1 rows ["12,000,000."] tag=SELECT 1`

### DIFF — d/sim-setup:46 (type)

```sql
SELECT length(number) FROM sims GROUP BY length(number)
```
- why: col 1 (length) type OID pg=23 stask=20
- pg: `cols(length:23) 1 rows ["10"] tag=SELECT 1`
- stask: `cols(length:20) 1 rows ["10"] tag=SELECT 1`

### DIFF — d/sim-setup:48 (type+value)

```sql
SELECT status, price FROM sims WHERE number = '0905555555'
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="0.00" stask="0"
- pg: `cols(status:25, price:1700) 1 rows ["available", "0.00"] tag=SELECT 1`
- stask: `cols(status:25, price:701) 1 rows ["available", "0"] tag=SELECT 1`

### PASS-MSG — d/sim-setup:49 (message)

```sql
INSERT INTO sims (number, carrier, status, price) VALUES ('0905555555', 'viettel', 'available', 1)
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "sims_pkey"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "sims_pkey": Key (number)=(0905555555) already exists`

### PASS-MSG — d/sim-setup:51 (message)

```sql
INSERT INTO sims (number, carrier, price) VALUES ('0906666666', 'viettel', 'abc')
```
- why: same SQLSTATE 22P02, message differs
- pg: `ERROR 22P02: invalid input syntax for type numeric: "abc"`
- stask: `ERROR 22P02: invalid input syntax for type double precision: "abc"`

### DIFF — d/sim-reserve:60 (type+value)

```sql
UPDATE sims SET status = 'reserved', updated_at = now() WHERE number = '0901000001' AND status = 'available' RETURNING number, status, price
```
- why: col 3 (price) type OID pg=1700 stask=701; row 1 col price: pg="1500000.00" stask="1500000"
- pg: `cols(number:25, status:25, price:1700) 1 rows ["0901000001", "reserved", "1500000.00"] tag=UPDATE 1`
- stask: `cols(number:25, status:25, price:701) 1 rows ["0901000001", "reserved", "1500000"] tag=UPDATE 1`

### DIFF — d/sim-reserve:65 (type+value)

```sql
UPDATE sims SET status = 'sold', updated_at = now() WHERE number = $1 AND status = 'available' RETURNING number, carrier, status, price
```
args: "0901000002"

- why: col 4 (price) type OID pg=1700 stask=701; row 1 col price: pg="500000.00" stask="500000"
- pg: `cols(number:25, carrier:25, status:25, price:1700) 1 rows ["0901000002", "viettel", "sold", "500000.00"] tag=UPDATE 1`
- stask: `cols(number:25, carrier:25, status:25, price:701) 1 rows ["0901000002", "viettel", "sold", "500000"] tag=UPDATE 1`

### DIFF — d/sim-reserve:71 (type+value)

```sql
UPDATE sims SET price = price * 1.1 WHERE carrier = 'viettel' RETURNING number, price
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="1650000.00" stask="1650000.0000000002"
- pg: `cols(number:25, price:1700) 2 rows ["0901000002", "550000.00"] ["0901000001", "1650000.00"] tag=UPDATE 2`
- stask: `cols(number:25, price:701) 2 rows ["0901000001", "1650000.0000000002"] ["0901000002", "550000"] tag=UPDATE 2`

### DIFF — d/sim-reserve:73 (type+value)

```sql
SELECT number, price, status FROM sims ORDER BY number
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="1485000.00" stask="1485000"
- pg: `cols(number:25, price:1700, status:25) 3 rows ["0901000001", "1485000.00", "reserved"] ["0901000002", "495000.00", "sold"] ["0902000003", "250000.00", "sold"] tag=SELECT 3`
- stask: `cols(number:25, price:701, status:25) 3 rows ["0901000001", "1485000", "reserved"] ["0901000002", "495000", "sold"] ["0902000003", "250000", "sold"] tag=SELECT 3`

### DIFF — d/sim-bulk-import:91 (type+value)

```sql
SELECT number, carrier, price FROM sims ORDER BY number
```
- why: col 3 (price) type OID pg=1700 stask=701; row 1 col price: pg="100.00" stask="100"
- pg: `cols(number:25, carrier:25, price:1700) 5 rows ["0901000001", "viettel", "100.00"] ["0901000002", "vinaphone", "250.00"] ["0901000003", "mobifone", "300.00"] ... tag=SELECT 5`
- stask: `cols(number:25, carrier:25, price:701) 5 rows ["0901000001", "viettel", "100"] ["0901000002", "vinaphone", "250"] ["0901000003", "mobifone", "300"] ... tag=SELECT 5`

### DIFF — d/sim-bulk-import:92 (type+value)

```sql
INSERT INTO sims (number, carrier, status, price) VALUES ('0901000001', 'x', 'sold', 1) ON CONFLICT (number) DO UPDATE SET price = EXCLUDED.price WHERE sims.status = 'available' RETURNING number, price, status
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="1.00" stask="1"
- pg: `cols(number:25, price:1700, status:25) 1 rows ["0901000001", "1.00", "available"] tag=INSERT 0 1`
- stask: `cols(number:25, price:701, status:25) 1 rows ["0901000001", "1", "available"] tag=INSERT 0 1`

### DIFF — d/sim-bulk-import:96 (type+value)

```sql
SELECT COUNT(*), SUM(price) FROM sims
```
- why: col 2 (sum) type OID pg=1700 stask=701; row 1 col sum: pg="1276451.00" stask="1276451"
- pg: `cols(count:20, sum:1700) 1 rows ["55", "1276451.00"] tag=SELECT 1`
- stask: `cols(count:20, sum:701) 1 rows ["55", "1276451"] tag=SELECT 1`

### PASS-MSG — d/sim-bulk-import:102 (message)

```sql
INSERT INTO sims (number, carrier, price) SELECT number, carrier, price FROM sims WHERE number LIKE '0903%'
```
- why: same SQLSTATE 23505, message differs
- pg: `ERROR 23505: duplicate key value violates unique constraint "sims_pkey"`
- stask: `ERROR 23505: duplicate key value violates unique constraint "sims_pkey": Key (number)=(0903000001) already exists`

### DIFF — d/sim-bulk-import:104 (type)

```sql
INSERT INTO sims (number, carrier, price) VALUES ($1, $2, $3) ON CONFLICT (number) DO UPDATE SET price = EXCLUDED.price RETURNING number, price
```
args: "0909111111", "viettel", "1234567.89"

- why: col 2 (price) type OID pg=1700 stask=701
- pg: `cols(number:25, price:1700) 1 rows ["0909111111", "1234567.89"] tag=INSERT 0 1`
- stask: `cols(number:25, price:701) 1 rows ["0909111111", "1234567.89"] tag=INSERT 0 1`

### DIFF — d/sim-bulk-import:106 (type+value)

```sql
INSERT INTO sims (number, carrier, price) VALUES ($1, $2, $3) ON CONFLICT (number) DO UPDATE SET price = EXCLUDED.price RETURNING number, price
```
args: "0909111111", "viettel", "7.5"

- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="7.50" stask="7.5"
- pg: `cols(number:25, price:1700) 1 rows ["0909111111", "7.50"] tag=INSERT 0 1`
- stask: `cols(number:25, price:701) 1 rows ["0909111111", "7.5"] tag=INSERT 0 1`

### DIFF — d/sim-order-transaction:122 (type+value)

```sql
UPDATE sims SET status = 'sold', updated_at = now() WHERE number = '0901000001' AND status = 'available' RETURNING number, price
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="1500000.00" stask="1500000"
- pg: `cols(number:25, price:1700) 1 rows ["0901000001", "1500000.00"] tag=UPDATE 1`
- stask: `cols(number:25, price:701) 1 rows ["0901000001", "1500000"] tag=UPDATE 1`

### DIFF — d/sim-order-transaction:123 (type+value)

```sql
INSERT INTO order_lines (order_id, number, price) VALUES (1, '0901000001', 1500000) RETURNING id, order_id, number, price
```
- why: col 4 (price) type OID pg=1700 stask=701; row 1 col price: pg="1500000.00" stask="1500000"
- pg: `cols(id:20, order_id:20, number:25, price:1700) 1 rows ["1", "1", "0901000001", "1500000.00"] tag=INSERT 0 1`
- stask: `cols(id:20, order_id:20, number:25, price:701) 1 rows ["1", "1", "0901000001", "1500000"] tag=INSERT 0 1`

### DIFF — d/sim-order-transaction:124 (type+value)

```sql
UPDATE sims SET status = 'sold', updated_at = now() WHERE number = '0902000003' AND status = 'available' RETURNING number, price
```
- why: col 2 (price) type OID pg=1700 stask=701; row 1 col price: pg="250000.00" stask="250000"
- pg: `cols(number:25, price:1700) 1 rows ["0902000003", "250000.00"] tag=UPDATE 1`
- stask: `cols(number:25, price:701) 1 rows ["0902000003", "250000"] tag=UPDATE 1`

### DIFF — d/sim-order-transaction:126 (type+value)

```sql
UPDATE orders SET total = (SELECT SUM(price) FROM order_lines WHERE order_id = 1) WHERE id = 1 RETURNING id, total
```
- why: col 2 (total) type OID pg=1700 stask=701; row 1 col total: pg="1750000.00" stask="1750000"
- pg: `cols(id:20, total:1700) 1 rows ["1", "1750000.00"] tag=UPDATE 1`
- stask: `cols(id:20, total:701) 1 rows ["1", "1750000"] tag=UPDATE 1`

### DIFF — d/sim-order-transaction:129 (type+value)

```sql
SELECT id, customer, total FROM orders
```
- why: col 3 (total) type OID pg=1700 stask=701; row 1 col total: pg="1750000.00" stask="1750000"
- pg: `cols(id:20, customer:25, total:1700) 1 rows ["1", "Nguyen Van A", "1750000.00"] tag=SELECT 1`
- stask: `cols(id:20, customer:25, total:701) 1 rows ["1", "Nguyen Van A", "1750000"] tag=SELECT 1`

### ERR-ONLY-ONE-SIDE — d/sim-order-transaction:146 (feature)

```sql
SAVEPOINT l1
```
- why: STASK rejects: 0A000
- pg: `tag=SAVEPOINT`
- stask: `ERROR 0A000: statement "SAVEPOINT" is not supported`

### ERR-ONLY-ONE-SIDE — d/sim-order-transaction:148 (feature)

```sql
ROLLBACK TO SAVEPOINT l1
```
- why: STASK rejects: 42601
- pg: `tag=ROLLBACK`
- stask: `ERROR 42601: syntax error at or near "to"`

### ERR-ONLY-ONE-SIDE — d/sim-order-transaction:149 (semantics)

```sql
INSERT INTO order_lines (order_id, number, price) VALUES (3, '0901000002', 500000)
```
- why: STASK rejects: 25P02
- pg: `tag=INSERT 0 1`
- stask: `ERROR 25P02: current transaction is aborted, commands ignored until end of transaction block`

### ERR-ONLY-ONE-SIDE — d/sim-order-transaction:150 (semantics)

```sql
UPDATE orders SET total = 500000 WHERE id = 3
```
- why: STASK rejects: 25P02
- pg: `tag=UPDATE 0`
- stask: `ERROR 25P02: current transaction is aborted, commands ignored until end of transaction block`

### DIFF — d/sim-order-transaction:151 (tag)

```sql
COMMIT
```
- why: command tag pg="COMMIT" stask="ROLLBACK"
- pg: `tag=COMMIT`
- stask: `tag=ROLLBACK`

### DIFF — d/sim-order-transaction:152 (type)

```sql
SELECT o.id, o.customer, o.total, COUNT(l.id) AS lines, SUM(l.price) AS line_sum FROM orders o LEFT JOIN order_lines l ON l.order_id = o.id GROUP BY o.id, o.customer, o.total ORDER BY o.id
```
- why: col 3 (total) type OID pg=1700 stask=701; col 5 (line_sum) type OID pg=1700 stask=701; command tag pg="SELECT 2" stask="SELECT 1"; row count pg=2 stask=1
- pg: `cols(id:20, customer:25, total:1700, lines:20, line_sum:1700) 2 rows ["1", "Nguyen Van A", "1750000.00", "2", "1750000.00"] ["4", "Le C", "0.00", "0", NULL] tag=SELECT 2`
- stask: `cols(id:20, customer:25, total:701, lines:20, line_sum:701) 1 rows ["1", "Nguyen Van A", "1750000", "2", "1750000"] tag=SELECT 1`

### DIFF — d/sim-order-transaction:153 (type+value)

```sql
SELECT o.customer, l.number, s.carrier, l.price FROM orders o JOIN order_lines l ON l.order_id = o.id JOIN sims s ON s.number = l.number ORDER BY o.id, l.number
```
- why: col 4 (price) type OID pg=1700 stask=701; row 1 col price: pg="1500000.00" stask="1500000"
- pg: `cols(customer:25, number:25, carrier:25, price:1700) 2 rows ["Nguyen Van A", "0901000001", "viettel", "1500000.00"] ["Nguyen Van A", "0902000003", "mobifone", "250000.00"] tag=SELECT 2`
- stask: `cols(customer:25, number:25, carrier:25, price:701) 2 rows ["Nguyen Van A", "0901000001", "viettel", "1500000"] ["Nguyen Van A", "0902000003", "mobifone", "250000"] tag=SELECT 2`

### DIFF — d/sim-order-transaction:154 (type+value)

```sql
SELECT s.carrier, SUM(l.price) FROM order_lines l JOIN sims s ON s.number = l.number GROUP BY s.carrier ORDER BY s.carrier
```
- why: col 2 (sum) type OID pg=1700 stask=701; row 1 col sum: pg="250000.00" stask="250000"
- pg: `cols(carrier:25, sum:1700) 2 rows ["mobifone", "250000.00"] ["viettel", "2000000.00"] tag=SELECT 2`
- stask: `cols(carrier:25, sum:701) 2 rows ["mobifone", "250000"] ["viettel", "1500000"] tag=SELECT 2`

### DIFF — d/sim-order-transaction:155 (tag)

```sql
SELECT s.number FROM sims s WHERE NOT EXISTS (SELECT 1 FROM order_lines l WHERE l.number = s.number) ORDER BY s.number
```
- why: command tag pg="SELECT 1" stask="SELECT 2"; row count pg=1 stask=2
- pg: `cols(number:25) 1 rows ["0903000004"] tag=SELECT 1`
- stask: `cols(number:25) 2 rows ["0901000002"] ["0903000004"] tag=SELECT 2`

### DIFF — d/sim-order-transaction:156 (value)

```sql
SELECT s.number, l.id FROM sims s LEFT JOIN order_lines l ON l.number = s.number ORDER BY s.number
```
- why: row 2 col id: pg="5" stask=NULL
- pg: `cols(number:25, id:20) 4 rows ["0901000001", "1"] ["0901000002", "5"] ["0902000003", "2"] ... tag=SELECT 4`
- stask: `cols(number:25, id:20) 4 rows ["0901000001", "1"] ["0901000002", NULL] ["0902000003", "2"] ... tag=SELECT 4`

### ERR-ONLY-ONE-SIDE — d/sim-order-transaction:157 (feature)

```sql
WITH t AS (SELECT order_id, SUM(price) AS s FROM order_lines GROUP BY order_id) UPDATE orders o SET total = t.s FROM t WHERE t.order_id = o.id RETURNING o.id, o.total
```
- why: STASK rejects: 42601
- pg: `cols(id:20, total:1700) 1 rows ["1", "1750000.00"] tag=UPDATE 1`
- stask: `ERROR 42601: syntax error at or near "update"`

### DIFF — d/sim-order-transaction:158 (columns)

```sql
DELETE FROM order_lines WHERE order_id = 3 RETURNING id, number
```
- why: column count pg=2 stask=0; command tag pg="DELETE 1" stask="DELETE 0"
- pg: `cols(id:20, number:25) 1 rows ["5", "0901000002"] tag=DELETE 1`
- stask: `tag=DELETE 0`

### DIFF — d/sim-order-transaction:159 (type)

```sql
SELECT currval('orders_id_seq')
```
- why: col 1 (currval) type OID pg=20 stask=25
- pg: `cols(currval:20) 1 rows ["4"] tag=SELECT 1`
- stask: `cols(currval:25) 1 rows ["4"] tag=SELECT 1`
