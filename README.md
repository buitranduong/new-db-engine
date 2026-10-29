# STASK

STASK is an experimental relational database engine written in Go. It speaks the
PostgreSQL wire protocol, so `psql`, JDBC, pgx and most ORMs can connect to it,
and it is built around one idea: **the write-ahead log is the single source of
truth.**

One process (`staskd`) owns one authoritative, segmented log. The same log is the
WAL and the value store. On top of it sit an MVCC primary index and three derived
representations (columnar, full-text search, time-series) that are fed one way,
from the log, and can always be rebuilt from it. The planner picks an execution
path from the *consistency the query asked for* and reports which plan it used
(engine, LSN, view lag).

> **Status: research prototype. Not for production use.** It is roughly a third of
> the way to the feature set listed in [docs/11](docs/11_rust_criteria.md) (see the
> [roadmap](#roadmap)). On-disk formats may still change. Do not keep data you
> cannot lose in it.

## Features

Working today. Every claim below is backed by a test or by a probe in
[docs/14](docs/14_plan_target_actual.md), which records the plan, the target and
what the code actually does for each task, including where the docs were wrong:

- **Storage and transactions**: segmented WAL with CRC32C frames and pipelined
  group commit; MVCC with Snapshot Isolation and Serializable for point reads;
  checkpoints; crash recovery; log GC and compressed segments
  ([docs/03](docs/03_transaction_recovery.md), [docs/07](docs/07_storage.md)).
- **SQL over the PostgreSQL protocol**: `SELECT`/`INSERT`/`UPDATE`/`DELETE`,
  joins, subqueries, `ON CONFLICT`, transactions, sequences and `SERIAL`,
  `DEFAULT`/`CHECK`/`NOT NULL`, `ALTER TABLE` (add, drop, rename, retype),
  `DROP`/`TRUNCATE`, `COPY` (text and csv), `CREATE VIEW`, and
  `pg_catalog`/`information_schema` for `psql \d` and JDBC metadata
  ([docs/05](docs/05_sql_pgwire.md)).
- **Analytics**: aggregates served from a columnar view, with a clear
  fall-back to the core path when the view lags
  ([docs/08](docs/08_memory.md)).
- **Change data capture**: logical replication slots with `test_decoding`,
  `wal2json` and `pgoutput`, publications, `LISTEN`/`NOTIFY`
  ([docs/10](docs/10_interop.md)).
- **Replication and recovery**: asynchronous replicas, base backup, continuous
  WAL archiving and point-in-time recovery ([docs/06](docs/06_cluster.md),
  [docs/05 §14](docs/05_sql_pgwire.md)).
- **Security and operations**: TLS and SCRAM-SHA-256 on the SQL port, mutual
  TLS and challenge-response authentication on the replication port, TLS and
  bearer tokens on the HTTP API, cancellation of running table scans and
  aggregates, `statement_timeout` (not yet applied to set-returning function scans
  such as `generate_series`), idle-session timeouts, per-query memory budget
  ([docs/13](docs/13_http_security.md)).

Not there yet: secondary indexes, foreign keys, roles and `GRANT`, exact
`numeric`/`uuid`/`bytea`/array types, triggers, data larger than RAM, and
consensus-based replication. See the roadmap.

## Quick start

Requirements: Linux (the log uses `fdatasync` and `fallocate`), Go 1.25 or newer.

```sh
git clone https://github.com/buitranduong/new-db-engine.git
cd new-db-engine
go build -o bin/ ./cmd/...
./bin/staskd -dir /var/lib/stask -addr 127.0.0.1:17070 -pg-addr 127.0.0.1:5432
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
`127.0.0.1:17432`) accept loopback connections only. The HTTP API, the profiling
endpoint and the replication port refuse to start on a non-loopback address
unless authentication is configured. **The SQL port does not yet:** with an empty
`-pg-password` it runs in trust mode on whatever address you give it, so only
bind it beyond loopback together with `-pg-password` and `-pg-auth scram`
(tracked in [docs/14](docs/14_plan_target_actual.md)). A JSON/HTTP API on `-addr`
offers the same data with a per-query consistency contract (`strong`, `bounded`,
`session`, `eventual`); see [docs/README.vi.md](docs/README.vi.md) for examples.

## Building and testing

```sh
go build ./...
go vet ./...
go test ./internal/...
go test -race ./internal/...     # includes crash, torn-tail and power-loss simulations
```

Benchmarks and workloads (`staskbench`, `staskmix`, `aggbench`, `staskchaos`) are
described in [docs/04](docs/04_results.md) and [docs/11 §5](docs/11_rust_criteria.md).
Measurements follow fixed rules (pinned CPUs, at least three runs, client CPU
recorded). Results that violate them are marked invalid in the docs.

## Repository layout

```
cmd/            staskd (server) and the benchmark, chaos and inspection tools
internal/wal    segmented log, CRC frames, group-commit I/O
internal/core   engine: committer, MVCC, checkpoint, recovery, archive/PITR, replica
internal/sql    parser, planner glue, executor, catalog, views, COPY
internal/pgwire PostgreSQL wire protocol (simple and extended), TLS, SCRAM, COPY
internal/proj   derived views: columnar, search (BM25), time-series
internal/repl   replication, base backup, logical decoding
internal/server HTTP/JSON API and admin endpoints
docs/           design, measurements, specification and plan (mostly Vietnamese)
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

Most documents are written in Vietnamese. English translations are welcome (see
[Contributing](#contributing)).

## Roadmap

The roadmap is public and mirrors [docs/12_plan.md](docs/12_plan.md), which has
the sizing, dependencies and exit criteria for each item. Status as of
2026-10-29: **done** = merged to `main` with tests; **in progress** = being
built on a branch, not yet merged; **open** = not started. "Done" means the
tests pass, not that the feature has been proven with real applications.

| Phase | Goal | Status |
|---|---|---|
| **P0 Measurement** | Fair comparisons with PostgreSQL and Redis (same client, pinned CPUs); read priority under overload | Mostly done. Open: an unexplained read stall at 100M rows (P0.5), long-run GC test (P0.6), read priority at 2x load (P0.8, in progress) |
| **P1 Types and schema** | `ALTER TABLE`, defaults, `CHECK`, sequences, `DROP`/`TRUNCATE`, `COPY`, `CREATE VIEW` | Done, except exact PostgreSQL data types: microsecond timestamps, exact `numeric`, `uuid`, `bytea`, `jsonb`, arrays (P1.1, in progress) |
| **P2 Indexes and constraints** | Secondary indexes, `UNIQUE`, foreign keys; serializable scans (SSI); spill-to-disk joins and sorts | Secondary index, `UNIQUE`, foreign keys in progress. SSI scans and spilling open |
| **P3 Data larger than RAM** | On-disk primary index; on-disk compressed columnar view; incremental checkpoint; time-series and search from SQL | Open. This is the largest remaining change to the storage format |
| **P4 Ecosystem** | `pg_catalog` and `information_schema` (done, tested only against recorded client queries); acceptance with a real Jmix 2 / Hibernate / Liquibase application; Debezium; snapshot export for `copy_data` subscribers | Catalog done. Real-application acceptance, Debezium and snapshot export open |
| **P5 Security and operations** | TLS, SCRAM, cancel and timeouts, PITR, HTTP auth (done); roles and `GRANT`, Prometheus metrics, `pg_stat_activity`, background CRC scrub (done, tested with pgx only) | Largely done. PITR has unit tests but no end-to-end run over TLS yet |
| **P6 Cluster** | Read-your-writes through replicas; majority commit with leader election (Raft) and failover; replication slots that survive failover; multi-machine measurements | Asynchronous replicas, read-your-writes routing and CDC state through the log are done. Raft exists as a simulated package, not yet wired into the engine (in progress). Measurements need separate machines |
| **P7 Triggers** | SQL-bodied `BEFORE`/`AFTER` row triggers | Done for the SQL-body syntax; PostgreSQL `EXECUTE FUNCTION` triggers are not supported |
| **P8 Language decision** | Re-run the full benchmark suite on the finished Go engine and decide whether a Rust port is justified (docs/11) | Open, after P0-P7 |

Explicitly **out of scope**: stored procedures and procedural languages,
sharding and cross-shard transactions (decided after P8), streaming of in-flight
transactions, DDL as CDC events.

### What "complete" means

The feature gate in [docs/11 §2](docs/11_rust_criteria.md) requires every item
marked "yes" to be finished and covered by the acceptance suite. Today about 30%
of that list is done; the remaining effort is dominated by the on-disk index,
exact types and consensus replication.

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
