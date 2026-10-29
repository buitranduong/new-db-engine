# accept/clients: real client stacks against STASK (Liquibase, JDBC, SQLAlchemy)

Task `realclients` (2026-10-29/30). STASK was tried with pgx, psql 16 and a hand-written JDBC trace; this directory runs the
**real** Liquibase 4.31.1, pgjdbc 42.7.14 and SQLAlchemy 2.0.55 / psycopg2 2.9.13, always side by side with PostgreSQL 16.15
(container `rc-pg`). Engine code (`internal/*`) was not touched. Results of the last run are in `results/`.

## Tools (all under `~/rc-tools`, reused between runs; nothing installed system-wide, no apt, no sudo used)

Every download sat in its own new directory, was checksummed before use, and no interpreter was started inside those directories.

| Tool | Version | Source | Integrity check done | License |
|---|---|---|---|---|
| Eclipse Temurin JDK | 21.0.13+9 | https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13%2B9/OpenJDK21U-jdk_x64_linux_hotspot_21.0.13_9.tar.gz | SHA-256 `d7f5d5cf865a2902b70ff549b97586a7d3f234e016f041ab447ea740c7d66d1c` = value in the release's `.sha256.txt` = value in the Adoptium API | GPLv2 + Classpath Exception (used as a tool, not linked or redistributed) |
| Liquibase CLI (Community) | 4.31.1 | https://github.com/liquibase/liquibase/releases/download/v4.31.1/liquibase-4.31.1.tar.gz | SHA-1 `d0eb986e5f3273c5102e05e3de29756f37ce7b84` and MD5 `d322c8e18036cad640fe7eb16a387ded` = the hashes the release publishes inside `liquibase-additional-4.31.1.zip`; SHA-256 computed locally `0555808b59941d497f0c1114c3f2225698afde11c60d191c88e449506a60a3ea` (not published by the project). The `.asc` PGP signature was downloaded but NOT verified (no key imported); say so if that matters. | Apache-2.0. The tarball also ships `liquibase-commercial.jar` and other vendors' JDBC drivers under their own licenses: they are never put on the command path of these scripts |
| pgjdbc | 42.7.14 | https://repo1.maven.org/maven2/org/postgresql/postgresql/42.7.14/postgresql-42.7.14.jar | SHA-1 `0e7c94ce80cb1dd9875ff44ffe6a40a8aaa3eefd` = Maven Central `.sha1`; SHA-256 computed `73914527305a40cce504b0d3d90b23caf565136912d607ae8ae7c5895512332c` (Central publishes no `.sha256` for it: 404) | BSD-2-Clause |
| pip (only to install the wheels below, run from the wheel with `python -I`) | 26.2.2 | files.pythonhosted.org | SHA-256 `fd7f4dfa8190e014013b9f08f3e973f4e4c26ffd179a204895533a7254813a70` = PyPI JSON `digests.sha256` | MIT |
| SQLAlchemy | 2.0.55 (latest 2.0.x; 2.1.6 also exists, not used) | PyPI wheel `cp312 manylinux x86_64` | SHA-256 `6e7694e736d63b476d81a0eae7b304d9b747a3f0a1b7dc10134655c4cccf5eb7` = PyPI | MIT |
| psycopg2-binary (the wheel form of psycopg2; no libpq-dev on the VM) | 2.9.13 | PyPI | SHA-256 `e3861eba31f8ea8663fd876166b032fd89179e42aa63764d6feb281f13f9eb60` = PyPI | LGPL with exceptions (installed in a venv, not copied into the repo) |
| greenlet, typing_extensions (mandatory dependencies of SQLAlchemy 2.0, outside the list but required) | 3.5.6, 4.16.0 | PyPI | SHA-256 `975736b0...3aa1`, `481caa48...c2e8` = PyPI | MIT AND PSF-2.0, PSF-2.0 |

`python3-venv`'s `ensurepip` is missing on the VM and installing it would have been outside the list, so the venv is created with
`python3 -m venv --without-pip` and pip is run straight from its checksummed wheel. Disk: `~/rc-tools` is about 0.8 GB (JDK 0.3, Liquibase 0.36, venv 0.1).
Verified-integrity caveat: for PyPI the "official digest" and the file come from the same site; for Liquibase the hashes come from the same GitHub release.

## Re-run

```sh
# worktree ~/wt/realclients, binary built with: go build -o /tmp/rc-bin/ ./cmd/staskd   (ports 28520-28529, rc-pg on 28530)
cd ~/wt/realclients/accept/clients
. ./lib.sh && rc_start_pg                    # postgres:16 container rc-pg, log_statement=all   (stop: docker rm -f rc-pg)
liquibase/run.sh pg;  liquibase/run.sh stask # 17-step Liquibase scenario, one file per step in /tmp/rc-run/lb-<target>/
liquibase/compare.sh                         # update on both, diff catalog + data seen through psql
liquibase/stmts.sh                           # every statement Liquibase+pgjdbc send, from the SERVER logs (staskd -slow-query 1ns, pg log_statement)
LB_FILTER= liquibase/run.sh stask            # no label filter: shows the raw first failure
liquibase/lb.sh stask --changelog-file=known-fail/blob-oid.xml update    # Liquibase BLOB -> OID
. ./lib.sh && rc_psql stask < liquibase/gaps.sql; rc_psql pg < liquibase/gaps.sql   # the failing statements, VERBOSITY verbose (SQLSTATE + source line)
jdbc/run.sh                                  # 94 JDBC operations; diff of pg vs stask
sqla/run.sh                                  # 107 SQLAlchemy/psycopg2 operations (incl. 36 Superset-style queries); diff of pg vs stask
stop: . ./lib.sh && rc_stop_stask; docker rm -f rc-pg
```

`java -cp postgresql-42.7.14.jar jdbc/Jdbc.java host:port user pass [url-params]` and `python3 -I sqla/run_sqla.py host:port user pass` run one target.
STASK runs with `-slow-query 1ns`, which logs every statement (also failing ones, without the outcome); that is the "statements sent" source. pgjdbc's own `loggerFile` produced no file under Liquibase, so the server logs are used.

## Result summary (last run, STASK from `main` @ bcb74f9, PG 16.15)

| Suite | PostgreSQL 16 | STASK |
|---|---|---|
| Liquibase scenario (17 steps: update, status, update again, history, validate, tag, rollback --tag, status, update, rollback-count, update, update-sql, snapshot, generate-changelog, rollback-sql, drop-all, update) | 17 PASS | **15 PASS, 2 FAIL** (snapshot, generate-changelog) **and only after 4 of 25 changesets were skipped with a label filter** (raw `update` fails at the first of them) |
| JDBC (pgjdbc 42.7.14) | 94 ok / 0 fail | 82 ok / **12 fail** (+ 8 ops that "pass" with a different result) |
| SQLAlchemy 2.0.55 + psycopg2 (DDL, reflection, ORM, Superset-style SQL) | 107 ok / 0 fail | 74 ok / **33 fail** |

### Liquibase (STASK)

Changelog: `liquibase/changelog/` (XML + YAML + formatted SQL + two CSV `loadData`; Jmix style: upper-case names, uuid `ID`,
`VERSION`, `DATETIME`, FK, unique, index, addColumn, addNotNull, rename, sequence, default, insert/update/delete, precondition, raw SQL, tags, explicit rollbacks).
Changelog mistakes that were mine and fixed (PG failed too): `preConditions` placed after `comment`; `<sql>` without `<rollback/>`; `--snapshot-format=text`.
Everything else that fails is the engine. Engine-gap changesets carry `labels="gap-..."` and are skipped on STASK with `--label-filter`.

Works on STASK, with results identical to PG (`results/cmp.diff` shows no row difference in `customer`, `sim`, `audit_log`, view, sequence): createTable (bigint identity PK, uuid PK, DECIMAL, BOOLEAN, CLOB->text, DATETIME, TIMESTAMP WITH TIME ZONE),
FK to a bigint PK, UNIQUE on varchar, plain and composite-over-varchar/boolean indexes, unique index, addColumn, addNotNull with default, renameColumn,
createSequence, addDefaultValue, loadData from CSV (quoted comma, uuid, boolean, timestamp, empty -> NULL), insert/update/delete, preconditions (`MARK_RAN`), raw `create view`, raw `CHECK` constraint,
tagDatabase, `rollback --tag` (objects really dropped), `rollback-count`, re-`update` after rollback, `update` run twice (0 run, 21 previously run), `drop-all`, `update-sql`, `history`, `validate`, `status --verbose`.

Statements that fail on STASK (and succeed on PG):

| # | Changeset | SQL sent by Liquibase | SQLSTATE and message | PG |
|---|---|---|---|---|
| L1 | `BLOB` column | `CREATE TABLE public.SIM (..., PHOTO OID, ...)` | 42P16 `unknown column type "oid"` | creates the column (Liquibase maps BLOB to OID on PG; BYTEA works on STASK) |
| L2 | FK to a uuid PK (5b, 9b) | `ALTER TABLE public.ORDER_ ADD CONSTRAINT FK_ORDER_ON_SIM FOREIGN KEY (SIM_ID) REFERENCES public.SIM (ID)` | 0A000 `columns of type uuid cannot be indexed` | ok |
| L3 | index on a uuid column | `CREATE INDEX IDX_ORDER_SIM ON public.ORDER_(SIM_ID)` | 0A000 `columns of type uuid cannot be indexed` (also `UNIQUE` on a uuid column) | ok |
| L4 | expression index | `create index IDX_SIM_MSISDN_LOWER on SIM (lower(MSISDN))` | 0A000 `indexes on expressions are not supported` | ok |
| L5 | `snapshot`, `generate-changelog` (and `diff`) | pgjdbc `getIndexInfo` / `getPrimaryKeys` (`information_schema._pg_expandarray(i.indkey)`) on a table with a composite index or composite PK | 0A000 `_pg_expandarray over more than one element is not supported` | ok |

Silent differences seen by Liquibase users: `CONSTRAINT PK_SIM PRIMARY KEY` is stored as `sim_pkey` (name ignored; PG keeps `pk_sim`), so name-based `dropPrimaryKey` and Jmix diffs would not match;
`pg_constraint.conrelid::regclass::text` returns the numeric OID (`16388`) instead of the table name; `information_schema.columns.column_default` drops the cast (`'NEW'` vs `'NEW'::character varying`) and prints `TRUE` for `true`;
`information_schema.columns` types of a view are all `text`; the 22 `DATABASECHANGELOG` rows and MD5 sums are identical to PG.
DDL is not transactional (JDBC probe `jmix/transaction w/ DDL`: a `CREATE TABLE` inside a rolled-back transaction survives), which matters when a Liquibase changeset with several statements fails half way.

### JDBC and SQLAlchemy failures

See `results/jdbc-stask.txt`, `results/sqla-stask.txt` (every line `OK|FAIL op :: result | SQLSTATE: message [SQL]`), and docs/14 appendix E for the table with PG behaviour.
