# 26. CDC and events correctness suite (`scripts/suite-cdc`)

Branch `cdcsuite`. A ready-to-run suite that exercises STASK's change capture (pgoutput over the walsender, the HTTP/wal2json stream) and LISTEN/NOTIFY together, with concurrent DML and DDL, against one staskd and a 3-node Raft cluster. **One objective: prove they work and are correct.** It measures no performance and tunes nothing (docs/AGENT_PROTOCOL section 6; docs/24 is where performance goes). Volumes are small and seeded; the whole suite runs in about a minute.

Language of this document: English (like the recent appendices of docs/14); the other docs are Vietnamese.

## 1. How to run

```
scripts/suite-cdc/run.sh                    # everything: builds staskd, runs, prints PASS/FAIL per scenario
scripts/suite-cdc/run.sh -only 'd_|g_'      # scenarios whose id matches a regexp
OUT=/some/dir BASE_PORT=29400 scripts/suite-cdc/run.sh -seed 7
```

- Builds `staskd` from the worktree into `bin-cdcsuite/` (never `bin/`) and the suite into the same directory.
- Starts and stops its own staskd processes (it keeps their handles; it never signals by name or pattern), ports from `BASE_PORT` (default 29400, each port checked free first), data in a temporary directory removed at the end.
- Report: `$OUT/report.txt` (default `results/cdc-suite/`, git-ignored): the summary, then for each scenario its log, the **GAP** lines (known differences from PostgreSQL that are recorded, not failed) and, on a timeout, a goroutine dump.
- Exit status 1 if any scenario fails. Needs Docker with the `postgres:16` image for scenario j (and the `pg_dump` probe in i); without it those two report a GAP instead of failing.
- Stops if less than 3 GB is free (docs/AGENT_PROTOCOL section 7).

## 2. What is in it

| File | Role |
|---|---|
| `consumer.go` | The pgoutput consumer on `github.com/jackc/pglogrepl` (already in `go.mod`, **no new dependency**): replication connection, slot create/drop, `START_REPLICATION` with proto 1-4 and `binary`, Begin/Commit/Relation/Insert/Update/Delete/Truncate decoding, text and binary tuples, standby status updates (ack), keepalives |
| `sqlutil.go` | SQL helpers, `Applier`: replays decoded transactions into empty destination tables over SQL, one destination transaction per source transaction, with the applied LSN written in the same transaction (the pattern of PostgreSQL's apply worker and its replication origin), skipping what it already applied |
| `pump.go` | The consumer loop: receive, apply, then acknowledge (so delivery is at-least-once and the apply idempotent); reconnects to another node or after a restart |
| `events.go` | HTTP NDJSON consumer (`/v1/cdc/{slot}/stream` + ack) and LISTEN/NOTIFY listeners, in parallel with the walsender consumer |
| `workload.go` | The "bank" workload: writers doing transfers on 8 hot accounts (conflicts and retries), upserts and deletes of volatile accounts, rolled-back transactions, savepoint rollbacks and COPY; every transaction leaves marker rows `(writer, seq)` so the stream can be checked against what the writers were told |
| `harness.go` | Starts/kills single nodes and a 3-node cluster |
| `scen_*.go` | The scenarios |

The checks that make "correct" concrete (`verifyBank`): every transaction a writer saw committed arrives exactly once and whole (all its marker rows in one delivered transaction, the expected number of changes per table); nothing of a rolled-back transaction or of a rolled-back savepoint arrives; commit LSNs strictly increase; each writer's transactions arrive in its own order; a transaction whose COMMIT outcome the writer does not know (connection died) may be present or absent but never torn; nothing arrives that no writer committed.

## 3. Scenarios, expected behaviour and precedent

Precedent (AGENT_PROTOCOL section 8), used throughout: PostgreSQL documentation on logical decoding (slots, output plugins, `confirmed_flush_lsn`, commit-order delivery, at-least-once restart from the confirmed position), the streaming replication protocol (`START_REPLICATION`, standby status update, keepalive) and the logical replication message formats (pgoutput); `pglogrepl` and its examples (acknowledge after the change is durable in the sink); `pg_recvlogical` (stock consumer, `--fsync-interval`, resumes at the slot's position); the Debezium PostgreSQL connector (at-least-once, offsets are LSNs, restart from the last committed offset, replica identity FULL for old values, retries while a slot is still active, heartbeats for transactions of unrelated tables); the PostgreSQL apply worker (progress stored in the same transaction as the change); `NOTIFY` documentation and `async.c` (`AtSubAbort_Notify`: a rolled-back subtransaction drops its notifications; one queue, so one order for every listener).

| Id | Scenario | Expected (PostgreSQL behaviour) |
|---|---|---|
| a_basic | table + publication + slots; insert (all types, NULLs), update, key update, delete, upsert, `ON CONFLICT DO NOTHING`, MERGE, COPY text, COPY binary, multi-row statements, no-op update, REPLICA IDENTITY FULL, TRUNCATE, an unpublished table written concurrently (noise) in the same and in other transactions; a text and a binary stream on two slots | exactly the expected change list per transaction, `K`/`O` old tuples as the identity says, binary values equal text values, no empty transactions for unpublished tables, relation messages before first use, `confirmed_flush_lsn` equals the last acknowledged commit |
| b_tx | open transaction invisible until COMMIT; two interleaved transactions arrive in commit order; ROLLBACK; nested savepoints, `ROLLBACK TO`, `RELEASE`; errors, aborted transactions, duplicate keys; DDL in a rolled-back transaction; 20,000-row insert/update/delete in one transaction; a 2 MiB value; a burst of 300 transactions; `PREPARE TRANSACTION` | atomic at commit in commit order; nothing from rolled-back work; large transactions whole |
| c_ddl | ADD COLUMN (with and without default), RENAME COLUMN, DROP COLUMN, ALTER COLUMN TYPE, table added to / dropped from a publication (`ALTER PUBLICATION`), RENAME TABLE, DROP and re-CREATE a table; a live consumer and a second slot read only at the end | a new Relation message at each change, then rows in the new layout; PostgreSQL decodes the lagging consumer's rows with the catalog as it was (historic snapshot) |
| d_oracle | 4 writers x 150 transactions with conflicts and retries; the stream replayed into empty tables | replay equals the source row by row (`acct`, `ev`), sum of balances invariant, every position acknowledged |
| e_slot | consumer crash and restart from the confirmed position (start 0) and from the consumer's own position; staskd `SIGTERM` restart and `kill -9` restart while writers and consumer run; second consumer of a busy slot (55006); DROP of an active slot (55006), idle slot, missing slot (42704); re-create (42710) starts at "now"; two slots; unacknowledged retention (`lag_bytes` grows, falls after acknowledging); TEMPORARY slot | no loss, duplicates only above the confirmed position and removed by the applier, slot state survives restarts, codes as in PostgreSQL |
| e_slot_lost | `-slot-max-lag-mb 1` with 16 MB written | the idle slot is invalidated (`wal_status = lost`), START fails with 55000, the healthy slot is unaffected, the lost slot can be dropped (PostgreSQL `max_slot_wal_keep_size`) |
| f_notify | payload bytes, `pg_notify`, empty payload, delivery only at COMMIT, ROLLBACK, savepoint rollback, duplicate collapse, channel case folding, 8000-byte limit, UNLISTEN / LISTEN in a transaction, own notifications, 30 listeners x 3 senders, NOTIFY from an AFTER INSERT trigger | PostgreSQL `NOTIFY` semantics; every listener sees the same global order |
| g_failover | 3-node cluster, writers through a multi-host DSN, consumer on the leader, `kill -9` of the leader, consumer reconnects to the new leader and resumes from its own position; the old leader rejoins | oracle still holds, no change streamed that the cluster did not keep, slot present on the new leader (PostgreSQL 17 failover slots, Patroni permanent slots: the slot must exist on the new primary) |
| h_order | the same transactions through pgoutput, the HTTP stream and NOTIFY (4 writers) | same set, same global order, same commit position in pgoutput and HTTP; rolled-back notifications never arrive |
| i_views | CDC on a table with `columnar` + trigram + history store, while repeatable-read snapshot readers run `COPY TO` and aggregates, and a real `pg_dump` from `postgres:16` | the stream keeps delivering during long reads, the snapshot reads see constant sums (transfers), columnar aggregate and trigram `LIKE` agree with the replay |
| j_stock | `pg_recvlogical` from `postgres:16` against STASK (test_decoding) | the stock tool creates the slot and prints BEGIN / INSERT / UPDATE / DELETE / COMMIT correctly |

Not done: Debezium Server (no image locally, large, and the pgoutput framing it consumes is what `pglogrepl` and `accept/pgsub.sh` already check; recorded here, not claimed); a PostgreSQL 16 `CREATE SUBSCRIPTION` is covered by the existing `accept/pgsub.sh` (not rerun here).

## 4. First run (commit in the docs/14 row)

Run on the shared VM at the branch head (see the docs/14 row for the hash), one command, about 65 s. Full suite: seeds 1 and 2 gave 11 of 11; seed 3 failed once in e_slot on a harness flaw (an acknowledgement on a connection that had just died was not retried), fixed, after which e_slot and g_failover passed seeds 3, 4 and 5:

| Id | Result | Time | What the run showed |
|---|---|---|---|
| a_basic | PASS | 1.7 s | all DML kinds as expected; text and binary streams equal; 2 GAPs (G1, G2) |
| b_tx | PASS | 18 s | 20,000-row transactions whole, 2 MiB value, 300-transaction burst in order; 2 GAPs (G2, G4) |
| c_ddl | PASS | 3 s | live consumer: every DDL step delivers a new Relation message and rows in the new layout; lagging consumer: 2 GAPs (G3) |
| d_oracle | PASS | 2-4 s | about 530 committed transactions, 60 rolled back, 7-11 conflict retries; replay equals source in `acct` and `ev` |
| e_slot | PASS | 8-11 s | consumer crash: 16-23 redeliveries removed by the applier; SIGTERM and `kill -9` restarts: slot kept, consumer reconnects; slot freed 0.2-1.2 s after a dead consumer (PostgreSQL also needs time to notice; the suite retries 55006) |
| e_slot_lost | PASS | 1 s | idle slot becomes `lost`, START gives 55000, healthy slot unaffected |
| f_notify | PASS | 8 s | all PostgreSQL semantics listed in section 3; 30 listeners see one global order |
| g_failover | PASS | 5-6 s | leader killed after about 40 streamed transactions, new leader after about 2 s, 1 reconnect, 0-1 redelivered, 0-1 transaction of unknown outcome, replay equals source, no phantom changes |
| h_order | PASS | 1 s | 225 transactions: pgoutput, HTTP and NOTIFY give the same order; commit position equal in pgoutput and HTTP |
| i_views | PASS | 2 s | 6 long snapshot reads with `COPY`, none stalled the stream; columnar aggregate and trigram `LIKE` equal the replay; `pg_dump` GAP (G5) |
| j_stock | PASS | 1-2 s | `pg_recvlogical` from `postgres:16` creates the slot and prints the transactions (after bug B4) |

Real bugs the suite found, fixed in STASK, each with a regression test (commits `f476569`, `7bd5530` and the last one of the branch):

| # | Bug | Root cause | Fix | Regression test |
|---|---|---|---|---|
| B1 | A `NOTIFY` made after a savepoint that was rolled back (`ROLLBACK TO`) was still delivered at COMMIT | `Session.notes` / `listenOps` were not part of the savepoint state | `savepointState` remembers their length, `ROLLBACK TO` truncates (PostgreSQL `AtSubAbort_Notify`); PL/pgSQL exception blocks use the same path | `TestNotifySavepointRollback` (fails without the fix), scenario f_notify |
| B2 | Concurrent senders: two listeners saw the notifications in different orders (22 of 30 listeners differed from listener 0) | `notifyHub.publish` only held a read lock, so two publishes interleaved differently per listener; PostgreSQL has one queue | a publish-wide mutex | `TestNotifyOrderSameForAllListeners` (fails without the fix), scenario f_notify |
| B3 | `ALTER PUBLICATION ... ADD/SET/DROP TABLE`, `SET (publish = ...)`, `RENAME TO` were syntax errors, so a table could only be added to a publication by dropping and recreating it | not implemented | `AlterPublication` in the parser, `Engine.AlterPublication`, rolled back with the transaction like CREATE/DROP PUBLICATION | `TestAlterPublication`, scenario c_ddl |
| B4 | The stock `pg_recvlogical` (PostgreSQL 16) stopped at its first command: `SHOW data_directory_mode` was an unknown parameter | missing setting | `data_directory_mode = 0700` | `TestShowDataDirectoryMode`, scenario j_stock |
| B5 | `pg_listening_channels()` did not exist | not implemented | session function | `TestListeningChannels`, scenario f_notify |

Harness problems found and fixed on the way (not STASK bugs): expectation errors of mine (boolean text form is `t`; `RELEASE SAVEPOINT` keeps the work; DROP TABLE removes the table from a publication, as in PostgreSQL), a wrong reading of `pglogrepl.SendStandbyCopyDone` (it already reads to ReadyForQuery), reads of `confirmed_flush_lsn` before the server had processed the last acknowledgement, and a missing reconnect on a broken acknowledgement.

## 5. Gaps found (not failed: recorded with a minimal reproduction)

Recorded in the report as `GAP:` lines and not failed. "Design" means the engine works that way on purpose and docs/10 or docs/05 say so; the suite checks that the consequence is exactly what is written here.

| # | Difference from PostgreSQL | Minimal reproduction | Effect | Status |
|---|---|---|---|---|
| G1 | `UPDATE` of the primary key is delivered as `D`(key) + `I`(new row), PostgreSQL sends one `U` with an old key tuple `K` | `CREATE TABLE t(id bigint PRIMARY KEY, a text); CREATE PUBLICATION p FOR TABLE t; INSERT INTO t VALUES (2,'x'); UPDATE t SET id = 20 WHERE id = 2;` | Replay and final state are the same; a consumer that tells key changes apart from delete+insert cannot. Debezium itself turns a key change into delete + create | design (a key is the row's identity in the log) |
| G2 | Changes of one row inside one transaction are coalesced to the net change: insert+delete gives nothing, two updates give the last, insert+update gives one insert of the final row; PostgreSQL sends every change | `BEGIN; INSERT INTO t VALUES (60,'a'); DELETE FROM t WHERE id = 60; UPDATE t SET a='u1' WHERE id = 50; UPDATE t SET a='u2' WHERE id = 50; COMMIT;` gives one `U` (50,u2) | Intermediate states inside a transaction are not visible; an audit/outbox that needs every statement must not rely on CDC | design (one write set per transaction, docs/10 section 2) |
| G3 | A consumer that lags across DDL is decoded with the table as it is at decode time: the table name, the column list and types, and publication membership are the current ones, not those of the time of the write. Rows written before an `ADD COLUMN` get the new column as `NULL` or default, rows of a renamed or dropped-and-recreated table carry the final name, and changes of a table removed from the publication since are not delivered | slot created; `INSERT` into x; `ALTER TABLE x RENAME TO x2`; read the slot: the insert arrives as table `x2`. In c_ddl 3 of 13 written changes were lost this way (the table had been removed from the publication) | Silent for a consumer that is behind during a migration. Live consumers (caught up before and after each DDL) are exact (c_ddl) | **mostly fixed 2026-11-04** (docs/14 CDCHIST): the decoder keeps a historic catalog (table names, publications and REPLICA IDENTITY by relation OID, from the DDL records and the CDC catalog row in the log), saved with the slot; changes arrive under the name and publications of their time. Still open: the column list of the Relation message is the one of decode time (rows written before `ADD COLUMN` carry the new column as NULL or default; no change is lost) |
| G4 | `PREPARE TRANSACTION` / `COMMIT PREPARED` are not supported (syntax error), so two-phase decoding (`TWO_PHASE`) is refused too | `BEGIN; INSERT ...; PREPARE TRANSACTION 'g';` | Applications using XA/2PC cannot run against STASK | not done |
| G5 | `pg_dump` fails on the catalog queries before reading data | `pg_dump -h .. --data-only -t acct stask` gives `column x.tableoid does not exist` (query on `pg_extension`) | Logical backup with the stock tool is not possible; `COPY TO` and the repeatable-read snapshot reads of i_views work. Outside CDC; belongs to the pg_dump compatibility work | open |
| G6 | After `TRUNCATE` the table has a new relation id: the stream sends a second Relation message for the same name, and `T` names the old id | a_basic: Relation `public.t` is sent in transaction 0 and again after the TRUNCATE | Benign for consumers keyed by name (pglogrepl, Debezium handle a replaced Relation message); a consumer keyed by relation id must replace its entry | design (TRUNCATE replaces the table, docs/10) |
| G7 | The replica identity in force when the change is decoded decides whether the old row is sent (PostgreSQL: when it was written); implicit FULL for tables without a single-column key | docs/10 section 6 | already documented | design |
| G8 | `CREATE_REPLICATION_SLOT` on a Raft follower answers `XX000 read-only replica: writes go to the leader`; PostgreSQL on a standby answers `55000` ("logical decoding cannot be used while in recovery") | g_failover | Message and code only; the connection stays usable | cosmetic |
| G9 | Debezium Server was not run (not available locally, large image) | | The pgoutput framing it consumes is what pglogrepl and `accept/pgsub.sh` check; Debezium itself is **not proven** | not done |

## 6. Not proven

- Volumes are small on purpose (hundreds of transactions, one 20,000-row transaction, 2 MiB value, 16 MB of log): no claim about large backlogs, long retention or very wide tables.
- One failover, one kill point per run (`g_failover`); the kill lands mid-stream but the exact instruction is random. Several runs with other seeds (`-seed N`) would widen it; a network partition (instead of `kill -9`) is not exercised here (docs/06 has the cluster partition tests).
- Debezium itself was not run.
- Timeouts in the harness are liveness bounds (a stream that stops is a failure), not performance claims.
