# stask-m2 report: stask

- run: 2026-11-01T07:55:44Z, harness commit 8a73617, **DIRTY (load1 at start 2.06 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 2.06 / max 7.15 / end 7.15, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 19 | 0 |  |
| no_orphan_order_items | 19 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 18 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 18 | 0 |  |
| r2_pages_prefix_consistent_frozen | 18 | 0 |  |
| reserved_stock_equals_open_items | 18 | 0 |  |
| stock_invariant_q13 | 19 | 0 |  |
| tear_no_order_without_items | 35 | 0 |  |
| tear_no_orphan_items | 35 | 0 |  |
| tear_order_total_equals_items | 35 | 0 |  |
| tear_q13_at_one_snapshot | 35 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 35 | 0 |  |
| tear_reserved_equals_open_items | 35 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 78 | map[] | 1.3 | 246.07 | 407.49 | 512.15 | 512.15 |
| r1_keyset | 16650 | map[] | 277.5 | 4.66 | 14.35 | 43.27 | 158.89 |
| r1_point | 16459 | map[] | 274.3 | 1.00 | 9.06 | 37.95 | 156.23 |
| r2_page | 429 | map[] | 7.1 | 15.52 | 32.73 | 84.19 | 118.11 |
| r_join | 49 | map[] | 0.8 | 630.10 | 11171.12 | 11710.16 | 11710.16 |
| t_cancel | 2753 | map[] | 45.9 | 8.52 | 43.98 | 118.72 | 380.64 |
| t_order | 5533 | map[] | 92.2 | 9.32 | 51.28 | 129.69 | 351.57 |
| t_pay | 2781 | map[] | 46.3 | 8.44 | 50.41 | 116.52 | 315.27 |
| tear_tx | 28 | map[] | 0.5 | 2022.32 | 2334.00 | 2362.80 | 2362.80 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 78 ok, errors map[], p50 246 ms, p95 407 ms, p99 512 ms, max 512 ms; batches 78, rows 351000

## Counters

- import_batches = 78
- import_retries = 0
- import_rows = 351000
- oracle_passes = 18
- t_noop_cancel = 1149
- t_noop_order = 3043
- t_noop_pay = 1093
- t_retries = 0
- tear_snapshots = 35
