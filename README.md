# STASK

[![ci](https://github.com/buitranduong/new-db-engine/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/buitranduong/new-db-engine/actions/workflows/ci.yml)

STASK is an experimental relational database engine written in Go. It speaks the
PostgreSQL wire protocol, so `psql`, JDBC, pgx and most ORMs can connect to it,
and it is built around one idea: **the write-ahead log is the single source of
truth.**

In one paragraph: STASK is a from-scratch Go engine (it does not use PostgreSQL
code) built on the mechanisms of two databases: PostgreSQL (wire protocol, SQL,
transactions, indexes, replication) and ClickHouse (a columnar view for
aggregates and facets, time-series, history). psql, pgjdbc, pgx and Liquibase work
unchanged. One durable log is the source of truth and the other structures are
derived from it: one copy of the data and one recovery path instead of several
systems to keep in sync. Redis and Elasticsearch are not considered databases and
are not part of the design: from Redis it only borrows techniques for controlling
and compacting RAM (the in-RAM primary index), and neither is used as a reference
or criterion for further development; the code already in place stays as it is.
The claim is functional coverage; performance parity is not claimed until the
final verification phase (see `docs/24_final_measurement.md`).

One process (`staskd`) owns one authoritative, segmented log. The same log is the
WAL and the value store. On top of it sit an MVCC primary index and three derived
representations (columnar, full-text search, time-series) that are fed one way,
from the log, and can always be rebuilt from it. The planner picks an execution
path from the *consistency the query asked for* and reports which plan it used
(engine, LSN, view lag).

Direction: not "one database for everything", but a **tiered, tightly controlled engine** that gathers the best architectures of existing systems (log and MVCC core, derived columnar and search views, time-bounded history, archive), each tier with a declared role and freshness. See [docs/16](docs/16_tiered_engine_vision.md).

> **Status: research prototype, code phase closed, verification phase next.**
> The feature set for the target workload is implemented and covered by tests;
> nothing has been measured at scale yet, and no performance parity with
> PostgreSQL or ClickHouse is claimed. Current state, known
> gaps and the live work queue: [docs/STATUS.md](docs/STATUS.md) and
> [docs/27_handoff.md](docs/27_handoff.md). Criteria for production use:
> [docs/PROCESS.md](docs/PROCESS.md) D34 (seven criteria, none reached yet).
> On-disk formats may still change (additive changes are recorded in D33). Do not
> keep data you cannot lose in it.

## Features

Working today. Every claim below is backed by a test or by a probe in
[docs/14](docs/14_plan_target_actual.md), which records the plan, the target and
what the code actually does for each task, including where the docs were wrong.
`cmd/claimcheck` (603 of 604 probes pass at the last gate) and `cmd/pgdiff`
(statement-by-statement comparison with a real PostgreSQL 16) are the
reproducible evidence.

- **Storage and transactions**: segmented WAL with CRC32C frames and pipelined
  group commit; MVCC with Snapshot Isolation, Serializable and READ COMMITTED
  with re-evaluation; optimistic concurrency with repair; row locks (`SELECT ...
  FOR UPDATE/SHARE [NOWAIT|SKIP LOCKED]`, `UPDATE`/`DELETE` that wait, deadlock
  detection `40P01`, `lock_timeout`); transactional DDL; checkpoints; crash
  recovery; log GC and compressed segments
  ([docs/03](docs/03_transaction_recovery.md), [docs/07](docs/07_storage.md)).
- **Indexes and larger-than-RAM data**: in-RAM primary index; secondary indexes
  (btree, expression, partial, covering), `UNIQUE` and foreign keys; indexes can
  live on disk (`pindex`, a B+tree with an `-index-mem-budget` and an online
  `ALTER ... SET (index_storage)`), operators spill to disk under `work_mem`
  ([docs/18](docs/18_disk_index.md)).
- **SQL over the PostgreSQL protocol**: joins, subqueries, CTEs (including
  recursive), window functions, `GROUPING SETS`, `ON CONFLICT`, `MERGE`, `COPY` (text,
  csv and binary), views, sequences, `ALTER TABLE`, partitioned tables,
  collations, composite types, arrays, `jsonb`, roles with `GRANT`/`REVOKE`,
  triggers (PostgreSQL form, `INSTEAD OF`, transition tables, event triggers),
  PL/pgSQL (a large subset), `pg_catalog`/`information_schema` for `psql`, JDBC
  and ORM metadata ([docs/05](docs/05_sql_pgwire.md)). Row-level security
  (`CREATE POLICY`) is not supported and fails loudly.
- **Derived views**: a columnar view for aggregates and facets, a trigram view for
  `LIKE`/`ILIKE`, suffix and substring search, time-series, and a time-bounded
  history store (`AS OF`, TTL by whole segment). The planner reports which plan it
  used and falls back to the core path when a view lags
  ([docs/08](docs/08_memory.md), [docs/19](docs/19_history_retention.md)).
- **Change data capture**: logical replication slots with `test_decoding`,
  `wal2json` and `pgoutput`, publications, an HTTP change stream,
  `LISTEN`/`NOTIFY` ([docs/10](docs/10_interop.md)).
- **Replication and recovery**: a 3-node Raft cluster (majority commit, leader
  election, failover, runtime membership change; a follower refuses writes and
  row locks with `25006`, clients find the leader with a multi-host DSN),
  asynchronous replicas, base backup, continuous WAL archiving and point-in-time
  recovery (single node and cluster), read-your-writes through replicas, and a
  shadow mode (`stashadow`) to run next to an existing PostgreSQL
  ([docs/06](docs/06_cluster.md), [docs/21](docs/21_operations.md)).
- **Import**: `staskimport` loads xlsx/csv (resumable, binary `COPY` with fallback)
  with history rows ([docs/20](docs/20_import_runbook.md)).
- **Security and operations**: TLS and SCRAM-SHA-256 on the SQL port, `-pg-hba`,
  mutual TLS and challenge-response on the replication port, TLS and bearer
  tokens on the HTTP API, a hash-chained audit log
  ([docs/22](docs/22_audit_log.md)), AES-256-GCM encrypted backups and archives
  (volume encryption such as LUKS2 is the at-rest baseline,
  [docs/23](docs/23_encryption.md)), statement cancellation, `statement_timeout`,
  idle-session timeouts, per-query memory budget, `/metrics`, `/v1/ready`
  ([docs/13](docs/13_http_security.md)).

Known gaps (listed with reasons in [docs/STATUS.md](docs/STATUS.md)): row-level
security, Raft learner role and leader transfer, row locks for `INSERT`/`MERGE`/
`COPY`, trigram posting-list compression, disk index files that never shrink,
engine-level encryption at rest.

## Quick start

Requirements: Linux (the log uses `fdatasync` and `fallocate`), Go 1.25 or newer.

```sh
git clone https://github.com/buitranduong/new-db-engine.git
cd new-db-engine
go build -o bin-main/ ./cmd/...
./bin-main/staskd -dir /var/lib/stask -addr 127.0.0.1:17070 -pg-addr 127.0.0.1:5432
psql "postgres://app@127.0.0.1:5432/stask?sslmode=disable"
```

```sql
CREATE TABLE orders (
  id BIGINT PRIMARY KEY, customer_id BIGINT NOT NULL,
  amount DOUBLE PRECISION, status TEXT, created TIMESTAMPTZ
) WITH (columnar = true);

INSERT INTO orders VALUES (1, 42, 9.5, 'paid', now());
SELECT status, count(*), sum(amount) FROM orders GROUP BY status;
```

By default both listeners (`-addr` for HTTP, `-pg-addr` for SQL, default
`127.0.0.1:17432`) accept loopback connections only. The SQL port, the HTTP API,
the profiling endpoint and the replication port refuse to start on a non-loopback
address unless authentication is configured (`-pg-password`, `-http-token`,
`-repl-secret`), or you pass the matching `-pg-insecure`, `-http-insecure`,
`-pprof-insecure`, `-repl-insecure`. `accept/security.sh` exercises these
refusals, the TLS, SCRAM, token, audit, encrypted-backup and cluster handshakes with psql, pgx and
curl. A JSON/HTTP API on `-addr`
offers the same data with a per-query consistency contract (`strong`, `bounded`,
`session`, `eventual`); see [docs/README.vi.md](docs/README.vi.md) for examples.

## Building and testing

```sh
go build ./...
go vet ./...
go test ./internal/...
go test -race ./internal/...     # includes crash, torn-tail and power-loss simulations
```

System-level acceptance (several `staskd` processes, real clients; needs Linux,
openssl, curl and either `psql` or Docker with `postgres:16`) lives in `accept/`:
`pitr.sh` (backup over mTLS while writing, restore to a time and to an LSN, row
by row comparison), `kill9.sh` (acknowledged transactions survive `kill -9`),
`security.sh` (refusals on the SQL, HTTP and replication ports), `psql.sh`
(`\d`, `\copy`). Each prints OK/FAIL lines and exits non-zero on failure.

Changes reach `main` only through `scripts/merge-gate.sh` (`gofmt`, build, vet,
tests, `-race` on the concurrent packages, and `claimcheck` must not drop below
`docs/claimcheck_baseline.txt`). GitHub Actions (`.github/workflows/ci.yml`) runs
the same checks on a clean machine. Acceptance suites that any agent or person can
run against any PostgreSQL-wire server: `scripts/pgdiff-run.sh` (differential
corpus against `postgres:16`), `scripts/suite/` ([docs/25](docs/25_acceptance_suite.md))
and `scripts/suite-cdc/` ([docs/26](docs/26_cdc_events_suite.md)), when present.

Performance is deliberately measured once, at the end, after the code is closed:
the ordered plan, scale ladder and pass rule are in
[docs/24_final_measurement.md](docs/24_final_measurement.md). Benchmark tools
(`staskbench`, `staskmix`, `aggbench`, `simbench`, `memmap`, `staskchaos`) follow
fixed rules (exclusive machine via `scripts/perf-run.sh`, at least three runs,
client CPU recorded); results that violate them are marked invalid.

## Repository layout

```
cmd/            staskd (server), staskctl, staskimport, claimcheck, pgdiff, benchmark and chaos tools
internal/wal    segmented log, CRC frames, group-commit I/O
internal/core   engine: committer, MVCC, checkpoint, recovery, archive/PITR, replica, disk secondary indexes
internal/sql    parser, planner glue, executor, catalog, views, triggers, PL/pgSQL, COPY, row locks
internal/pgwire PostgreSQL wire protocol (simple and extended), TLS, SCRAM, COPY, walsender
internal/proj   derived views: columnar, search (trigram), time-series
internal/histstore time-bucketed history store
internal/pindex disk B+tree used for indexes larger than RAM
internal/spill  spill-to-disk operators
internal/repl   replication, base backup, logical decoding
internal/raft, internal/cluster   Raft and the wired 3-node cluster (membership, PITR)
internal/router read-your-writes HTTP router (cmd/staskroute)
internal/server HTTP/JSON API and admin endpoints
internal/audit, internal/seccrypt   audit log and backup encryption
internal/planner, schema, conf, tune, shadow   consistency planner, catalog types, config, auto-tuning, shadow mode
accept/         system-level acceptance scripts (see Building and testing)
scripts/        merge gate, differential and acceptance suites, measurement runners
docs/           design, measurements, specification, plan and handoff (mostly Vietnamese)
```

## Documentation

| Document | Contents |
|---|---|
| [docs/09_spec.md](docs/09_spec.md) | On-disk and wire formats; the acceptance suite (A1-A10) |
| [docs/05_sql_pgwire.md](docs/05_sql_pgwire.md) | SQL surface, PostgreSQL compatibility, security, PITR |
| [docs/03_transaction_recovery.md](docs/03_transaction_recovery.md) | Log, commit, MVCC, checkpoint, recovery |
| [docs/06_cluster.md](docs/06_cluster.md) | Replication and clustering |
| [docs/10_interop.md](docs/10_interop.md) | CDC and PostgreSQL ecosystem interoperability |
| [docs/14_plan_target_actual.md](docs/14_plan_target_actual.md) | Plan vs target vs actual for every task, with the probe results (`cmd/claimcheck`) |
| [docs/12_plan.md](docs/12_plan.md) | Detailed plan: every open item, size, dependency and exit test |
| [docs/STATUS.md](docs/STATUS.md), [docs/PROCESS.md](docs/PROCESS.md) | Current state and known gaps; decision log (D1-D34) including the production criteria |
| [docs/24_final_measurement.md](docs/24_final_measurement.md) | The single final measurement plan |
| [docs/27_handoff.md](docs/27_handoff.md) | Standing rules, environment and live work queue for anyone continuing the work |

Most documents are written in Vietnamese. English translations are welcome (see
[Contributing](#contributing)).

## Roadmap

The earlier phase table (P0-P8) is superseded: its code items are done or listed as
known gaps in [docs/STATUS.md](docs/STATUS.md), and the detailed history stays in
[docs/12_plan.md](docs/12_plan.md) and [docs/14](docs/14_plan_target_actual.md).
What remains is, in order:

1. Close the last code items and merge the acceptance suites (driver, SQL and CDC).
2. One verification and measurement programme ([docs/24](docs/24_final_measurement.md)):
   memory per structure, point lookups and aggregates against PostgreSQL 16 on the
   same machine, daily import, lanes, disk index, history, durability, restore,
   a long soak.
3. Only then adjust the design from the results, run shadow mode next to the
   existing system, and decide on production use against the criteria in
   [docs/PROCESS.md](docs/PROCESS.md) D34.

Out of scope for now: sharding and cross-shard transactions, streaming of
in-flight transactions, DDL as CDC events, Superset and Jmix as requirements (a
Jmix CRM is used only as one driver and data test case).

## Contributing

Issues and pull requests are welcome, especially:

- compatibility reports (the exact client, version and query that fails),
- tests that break an invariant (crash, partial write, clock, concurrency),
- measurements on other hardware, following the rules in docs/12,
- English translations of the documents.

Rules for any change are listed at the top of [docs/12](docs/12_plan.md): prior
art is read and linked, unit tests and `-race` tests are included, the
acceptance suite is extended, and a change to an on-disk format updates
docs/09 and keeps old data readable. Please run `gofmt`, `go vet` and the tests
before opening a pull request.

## Security

Do not expose `staskd` to an untrusted network without TLS and authentication
(see [docs/13](docs/13_http_security.md)). To report a vulnerability, open a
private security advisory on the GitHub repository instead of a public issue.

## License

MIT. See [LICENSE](LICENSE).
