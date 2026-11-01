# stask-m2 report: stask

- run: 2026-11-01T07:27:46Z, harness commit 8a73617, **DIRTY (load1 at start 1.74 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.74 / max 7.21 / end 7.21, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 10 | 0 |  |
| no_orphan_order_items | 10 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 10 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 10 | 0 |  |
| r2_pages_prefix_consistent_frozen | 10 | 0 |  |
| reserved_stock_equals_open_items | 10 | 0 |  |
| stock_invariant_q13 | 11 | 0 |  |
| tear_no_order_without_items | 13 | 0 |  |
| tear_no_orphan_items | 13 | 0 |  |
| tear_order_total_equals_items | 13 | 0 |  |
| tear_q13_at_one_snapshot | 13 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 13 | 0 |  |
| tear_reserved_equals_open_items | 13 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 527 | map[] | 8.8 | 34.88 | 69.94 | 93.96 | 128.51 |
| r1_keyset | 15182 | map[] | 253.0 | 5.38 | 17.96 | 29.52 | 97.45 |
| r1_point | 15078 | map[] | 251.3 | 1.78 | 12.77 | 22.34 | 61.35 |
| r2_page | 86 | map[] | 1.4 | 1319.35 | 1410.83 | 1468.93 | 1468.93 |
| r_join | 10 | map[] | 0.2 | 553.39 | 2312.01 | 2312.01 | 2312.01 |
| t_cancel | 2513 | map[] | 41.9 | 11.37 | 44.99 | 68.77 | 143.15 |
| t_order | 5063 | map[] | 84.4 | 12.96 | 50.61 | 76.30 | 163.04 |
| t_pay | 2549 | map[] | 42.5 | 11.31 | 45.40 | 69.37 | 143.80 |
| tear_tx | 10 | map[] | 0.2 | 5924.88 | 6201.84 | 6201.84 | 6201.84 |

## Import shape

shape that ran: **batched (500 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 527 ok, errors map[], p50 35 ms, p95 70 ms, p99 94 ms, max 129 ms; batches 527, rows 263500

## Counters

- import_batches = 527
- import_retries = 0
- import_rows = 263500
- oracle_passes = 10
- t_noop_cancel = 1034
- t_noop_order = 2665
- t_noop_pay = 1015
- t_retries = 0
- tear_snapshots = 13
