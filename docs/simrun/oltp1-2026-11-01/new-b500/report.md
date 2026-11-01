# stask-m2 report: stask

- run: 2026-11-01T07:41:45Z, harness commit 8a73617, **DIRTY (load1 at start 1.76 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.76 / max 7.37 / end 7.37, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 11 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
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
| tear_q13_at_one_snapshot | 14 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 13 | 0 |  |
| tear_reserved_equals_open_items | 13 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 557 | map[] | 9.3 | 31.18 | 57.92 | 70.79 | 95.87 |
| r1_keyset | 15535 | map[] | 258.9 | 5.34 | 16.96 | 27.65 | 91.91 |
| r1_point | 15449 | map[] | 257.5 | 1.71 | 11.92 | 20.66 | 60.11 |
| r2_page | 86 | map[] | 1.4 | 1325.95 | 1404.80 | 1427.60 | 1427.60 |
| r_join | 10 | map[] | 0.2 | 554.49 | 2263.34 | 2263.34 | 2263.34 |
| t_cancel | 2565 | map[] | 42.7 | 11.47 | 40.58 | 65.04 | 91.25 |
| t_order | 5156 | map[] | 85.9 | 13.12 | 45.46 | 65.47 | 122.62 |
| t_pay | 2599 | map[] | 43.3 | 11.98 | 44.83 | 70.03 | 103.98 |
| tear_tx | 10 | map[] | 0.2 | 5771.83 | 6086.52 | 6086.52 | 6086.52 |

## Import shape

shape that ran: **batched (500 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 557 ok, errors map[], p50 31 ms, p95 58 ms, p99 71 ms, max 96 ms; batches 557, rows 278500

## Counters

- import_batches = 557
- import_retries = 0
- import_rows = 278500
- oracle_passes = 10
- t_noop_cancel = 1056
- t_noop_order = 2724
- t_noop_pay = 1064
- t_retries = 0
- tear_snapshots = 13
