# stask-m2 report: stask

- run: 2026-11-01T07:17:57Z, harness commit 8a73617, **DIRTY (load1 at start 1.81 > 1.0; other build/test process started during the run: compile; other build/test process started during the run: go)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.81 / max 6.83 / end 6.83, disk free 3.4 GB

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
| stock_invariant_q13 | 10 | 0 |  |
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
| import | 80 | map[] | 1.3 | 225.43 | 333.98 | 398.10 | 398.10 |
| r1_keyset | 14794 | map[] | 246.5 | 5.23 | 17.16 | 50.13 | 145.90 |
| r1_point | 14762 | map[] | 246.0 | 1.69 | 11.75 | 42.82 | 141.09 |
| r2_page | 83 | map[] | 1.4 | 1304.94 | 1431.52 | 1593.00 | 1593.00 |
| r_join | 11 | map[] | 0.2 | 550.72 | 7723.19 | 7723.19 | 7723.19 |
| t_cancel | 2328 | map[] | 38.8 | 10.33 | 54.00 | 113.59 | 255.90 |
| t_order | 4649 | map[] | 77.5 | 12.09 | 64.00 | 115.92 | 284.51 |
| t_pay | 2325 | map[] | 38.7 | 10.92 | 55.84 | 110.16 | 330.67 |
| tear_tx | 7 | map[] | 0.1 | 8088.16 | 8292.35 | 8292.35 | 8292.35 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 80 ok, errors map[], p50 225 ms, p95 334 ms, p99 398 ms, max 398 ms; batches 80, rows 360000

## Counters

- import_batches = 80
- import_retries = 0
- import_rows = 360000
- oracle_passes = 10
- t_noop_cancel = 947
- t_noop_order = 2450
- t_noop_pay = 927
- t_retries = 0
- tear_snapshots = 10
