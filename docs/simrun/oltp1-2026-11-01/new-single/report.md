# stask-m2 report: stask

- run: 2026-11-01T07:23:29Z, harness commit 8a73617, **DIRTY (load1 at start 1.53 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.53 / max 6.87 / end 6.87, disk free 3.4 GB

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
| tear_no_order_without_items | 11 | 0 |  |
| tear_no_orphan_items | 11 | 0 |  |
| tear_order_total_equals_items | 11 | 0 |  |
| tear_q13_at_one_snapshot | 11 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 10 | 0 |  |
| tear_reserved_equals_open_items | 11 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 42 | map[] | 0.7 | 434.15 | 531.48 | 542.62 | 542.62 |
| r1_keyset | 14857 | map[] | 247.6 | 5.18 | 16.20 | 34.54 | 324.75 |
| r1_point | 14779 | map[] | 246.3 | 1.67 | 11.09 | 25.94 | 322.15 |
| r2_page | 85 | map[] | 1.4 | 1304.87 | 1474.09 | 1500.19 | 1500.19 |
| r_join | 10 | map[] | 0.2 | 583.58 | 2433.22 | 2433.22 | 2433.22 |
| t_cancel | 2221 | map[] | 37.0 | 10.55 | 50.31 | 185.68 | 353.01 |
| t_order | 4483 | map[] | 74.7 | 11.47 | 59.43 | 194.21 | 500.56 |
| t_pay | 2262 | map[] | 37.7 | 10.35 | 59.47 | 195.14 | 543.61 |
| tear_tx | 7 | map[] | 0.1 | 8091.95 | 8322.06 | 8322.06 | 8322.06 |

## Import shape

shape that ran: **single (1% in ONE transaction, worst case)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 42 ok, errors map[], p50 434 ms, p95 531 ms, p99 543 ms, max 543 ms; batches 42, rows 378000

## Counters

- import_batches = 42
- import_retries = 0
- import_rows = 378000
- oracle_passes = 10
- t_noop_cancel = 967
- t_noop_order = 2436
- t_noop_pay = 879
- t_retries = 0
- tear_snapshots = 10
