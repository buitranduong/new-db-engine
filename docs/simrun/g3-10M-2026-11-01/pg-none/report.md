# stask-m2 report: pg16

- run: 2026-11-01T05:41:11Z, harness commit 6afb539, **INVALID: LOAD NOT APPLIED**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 120 s elapsed
- machine: 8 CPUs, load1 start 1.08 / max 14.11 / end 13.51, disk free 4.9 GB

## LOAD NOT APPLIED

**This run is INVALID.** The load it was configured to apply did not happen, so "all oracles held" below compares empty sets and proves nothing, and no number of this report may be quoted.

- r2_page: 25 successes, minimum 30 (-r 2, -min-r2)

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 18 | 0 |  |
| no_orphan_order_items | 6 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 18 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 18 | 0 |  |
| r2_pages_prefix_consistent_frozen | 18 | 0 |  |
| reserved_stock_equals_open_items | 18 | 0 |  |
| stock_invariant_q13 | 18 | 0 |  |
| tear_no_order_without_items | 8 | 0 |  |
| tear_no_orphan_items | 8 | 0 |  |
| tear_order_total_equals_items | 8 | 0 |  |
| tear_q13_at_one_snapshot | 8 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 7 | 0 |  |
| tear_reserved_equals_open_items | 8 | 0 |  |

No oracle was violated, but the run is INVALID (load not applied).

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 46362 | map[] | 386.3 | 3.31 | 7.54 | 10.05 | 24.36 |
| r1_point | 46862 | map[] | 390.5 | 1.51 | 4.70 | 6.62 | 17.41 |
| r2_page | 25 | map[53100:1] | 0.2 | 8296.19 | 10686.05 | 11223.78 | 11223.78 |
| r_join | 2 | map[53100:1] | 0.0 | 1800.04 | 9477.09 | 9477.09 | 9477.09 |
| t_cancel | 7246 | map[] | 60.4 | 10.28 | 18.23 | 22.85 | 74.56 |
| t_order | 14696 | map[] | 122.5 | 11.54 | 20.06 | 25.58 | 93.76 |
| t_pay | 7451 | map[] | 62.1 | 10.17 | 18.11 | 23.55 | 70.98 |
| tear_tx | 6 | map[] | 0.0 | 19234.28 | 20277.15 | 20277.15 | 20277.15 |

## Error samples (up to 3 distinct messages per class, full text; first_s = seconds into the measured window)

| class | code | count | first_s | message |
|---|---|---|---|---|
| r2_page | 53100 | 1 | 48.7 | ERROR: could not resize shared memory segment "/PostgreSQL.2587543428" to 6311936 bytes: No space left on device (SQLSTATE 53100) |
| r_join | 53100 | 1 | 25.8 | ERROR: could not resize shared memory segment "/PostgreSQL.3141562230" to 8388608 bytes: No space left on device (SQLSTATE 53100) |

## Counters

- oracle_passes = 18
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1242113550" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1746204804" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2053024170" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2493842150" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2743200168" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.3772353648" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.380588458" to 8388608 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.3953950746" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.4110923442" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.4217267802" to 4194304 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.511206752" to 8388608 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.757650778" to 8388608 bytes: No = 1
- oracle_query_errors = 12
- t_noop_cancel = 2999
- t_noop_order = 7807
- t_noop_pay = 2974
- t_retries = 0
- tear_snapshots = 7
