# stask-m2 report: stask

- run: 2026-11-01T07:44:31Z, harness commit 8a73617, **DIRTY (load1 at start 1.98 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- **ABLATION (diagnosis only, not an acceptance run): nohist**; import batch 5000 rows
- machine: 8 CPUs, load1 start 1.98 / max 7.21 / end 7.11, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | **1** | t+76.5s import history rows=0, journal says 428000 rows in 95 imports |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 11 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 11 | 0 |  |
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

**1 oracle violations.**

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 95 | map[] | 1.6 | 119.91 | 175.66 | 208.72 | 208.72 |
| r1_keyset | 14979 | map[] | 249.3 | 5.16 | 16.69 | 49.63 | 166.17 |
| r1_point | 14904 | map[] | 248.0 | 1.67 | 11.06 | 42.24 | 163.15 |
| r2_page | 86 | map[] | 1.4 | 1298.68 | 1400.61 | 1474.33 | 1474.33 |
| r_join | 11 | map[] | 0.2 | 580.91 | 2518.64 | 2518.64 | 2518.64 |
| t_cancel | 2463 | map[] | 41.0 | 10.43 | 52.49 | 95.33 | 213.45 |
| t_order | 4991 | map[] | 83.1 | 11.69 | 55.33 | 89.85 | 271.90 |
| t_pay | 2523 | map[] | 42.0 | 10.80 | 49.48 | 76.45 | 188.12 |
| tear_tx | 7 | map[] | 0.1 | 8069.24 | 8230.83 | 8230.83 | 8230.83 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 95 ok, errors map[], p50 120 ms, p95 176 ms, p99 209 ms, max 209 ms; batches 95, rows 428000

## Counters

- import_batches = 95
- import_retries = 0
- import_rows = 428000
- oracle_passes = 10
- t_noop_cancel = 1036
- t_noop_order = 2640
- t_noop_pay = 977
- t_retries = 0
- tear_snapshots = 10
