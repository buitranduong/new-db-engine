# stask-m2 report: stask

- run: 2026-11-01T08:14:49Z, harness commit 8a73617, **DIRTY (load1 at start 2.03 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 2.03 / max 7.29 / end 7.29, disk free 3.2 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 10 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 10 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 10 | 0 |  |
| r2_pages_prefix_consistent_frozen | 10 | 0 |  |
| reserved_stock_equals_open_items | 10 | 0 |  |
| stock_invariant_q13 | 11 | 0 |  |
| tear_no_order_without_items | 11 | 0 |  |
| tear_no_orphan_items | 11 | 0 |  |
| tear_order_total_equals_items | 11 | 0 |  |
| tear_q13_at_one_snapshot | 11 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 11 | 0 |  |
| tear_reserved_equals_open_items | 11 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 82 | map[] | 1.4 | 220.58 | 306.66 | 341.03 | 341.03 |
| r1_keyset | 14901 | map[] | 248.3 | 5.20 | 17.05 | 50.04 | 132.71 |
| r1_point | 14833 | map[] | 247.2 | 1.79 | 11.29 | 37.24 | 138.92 |
| r2_page | 85 | map[] | 1.4 | 1336.38 | 1397.77 | 1476.08 | 1476.08 |
| r_join | 10 | map[] | 0.2 | 558.36 | 3210.44 | 3210.44 | 3210.44 |
| t_cancel | 2317 | map[] | 38.6 | 10.86 | 56.20 | 106.39 | 234.00 |
| t_order | 4673 | map[] | 77.9 | 11.97 | 68.08 | 117.31 | 320.73 |
| t_pay | 2356 | map[] | 39.3 | 10.95 | 56.74 | 108.08 | 235.62 |
| tear_tx | 8 | map[] | 0.1 | 7012.19 | 8164.71 | 8164.71 | 8164.71 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 82 ok, errors map[], p50 221 ms, p95 307 ms, p99 341 ms, max 341 ms; batches 82, rows 369000

## Counters

- import_batches = 82
- import_retries = 0
- import_rows = 369000
- oracle_passes = 10
- t_noop_cancel = 997
- t_noop_order = 2530
- t_noop_pay = 952
- t_retries = 0
- tear_snapshots = 11
