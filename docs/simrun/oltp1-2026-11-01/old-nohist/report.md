# stask-m2 report: stask

- run: 2026-11-01T07:33:17Z, harness commit 8a73617, **DIRTY (load1 at start 2.02 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- **ABLATION (diagnosis only, not an acceptance run): nohist**; import batch 5000 rows
- machine: 8 CPUs, load1 start 2.02 / max 7.08 / end 7.08, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | **1** | t+76.4s import history rows=0, journal says 315000 rows in 70 imports |
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
| tear_r2_page_repeatable_in_snapshot | 11 | 0 |  |
| tear_reserved_equals_open_items | 11 | 0 |  |

**1 oracle violations.**

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 70 | map[] | 1.2 | 367.91 | 448.43 | 499.12 | 499.12 |
| r1_keyset | 15270 | map[] | 254.5 | 5.32 | 15.33 | 34.37 | 157.71 |
| r1_point | 15166 | map[] | 252.7 | 1.89 | 10.94 | 35.22 | 159.53 |
| r2_page | 84 | map[] | 1.4 | 1338.99 | 1447.33 | 1475.96 | 1475.96 |
| r_join | 11 | map[] | 0.2 | 580.89 | 7170.13 | 7170.13 | 7170.13 |
| t_cancel | 1940 | map[] | 32.3 | 10.20 | 49.90 | 357.01 | 537.22 |
| t_order | 3834 | map[] | 63.9 | 11.72 | 52.78 | 347.88 | 500.74 |
| t_pay | 1893 | map[] | 31.5 | 11.00 | 48.02 | 339.93 | 466.92 |
| tear_tx | 8 | map[] | 0.1 | 6964.46 | 7927.56 | 7927.56 | 7927.56 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 70 ok, errors map[], p50 368 ms, p95 448 ms, p99 499 ms, max 499 ms; batches 70, rows 315000

## Counters

- import_batches = 70
- import_retries = 0
- import_rows = 315000
- oracle_passes = 9
- t_noop_cancel = 790
- t_noop_order = 2149
- t_noop_pay = 781
- t_retries = 0
- tear_snapshots = 11
