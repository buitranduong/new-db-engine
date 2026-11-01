# stask-m2 report: pg16

- run: 2026-11-01T08:02:12Z, harness commit 8a73617, **DIRTY (load1 at start 3.16 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 3.16 / max 7.34 / end 7.34, disk free 1.8 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 29 | 0 |  |
| no_orphan_order_items | 29 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 29 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 29 | 0 |  |
| r2_pages_prefix_consistent_frozen | 29 | 0 |  |
| reserved_stock_equals_open_items | 29 | 0 |  |
| stock_invariant_q13 | 29 | 0 |  |
| tear_no_order_without_items | 37 | 0 |  |
| tear_no_orphan_items | 37 | 0 |  |
| tear_order_total_equals_items | 37 | 0 |  |
| tear_q13_at_one_snapshot | 37 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 36 | 0 |  |
| tear_reserved_equals_open_items | 37 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 77 | map[] | 1.3 | 272.47 | 421.94 | 492.68 | 492.68 |
| r1_keyset | 30806 | map[] | 511.0 | 2.04 | 4.43 | 6.58 | 15.55 |
| r1_point | 30677 | map[] | 508.9 | 0.80 | 2.23 | 3.69 | 12.37 |
| r2_page | 145 | map[] | 2.4 | 796.50 | 861.07 | 922.74 | 926.10 |
| r_join | 16 | map[] | 0.3 | 218.75 | 500.43 | 500.43 | 500.43 |
| t_cancel | 4807 | map[] | 79.7 | 6.89 | 12.71 | 18.45 | 71.66 |
| t_order | 9716 | map[] | 161.2 | 7.68 | 14.39 | 20.05 | 239.38 |
| t_pay | 4905 | map[] | 81.4 | 6.76 | 12.31 | 16.82 | 47.35 |
| tear_tx | 29 | map[] | 0.5 | 1925.50 | 2045.71 | 2091.93 | 2091.93 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 77 ok, errors map[], p50 272 ms, p95 422 ms, p99 493 ms, max 493 ms; batches 77, rows 347000

## Counters

- import_batches = 77
- import_retries = 0
- import_rows = 347000
- oracle_passes = 28
- t_noop_cancel = 2069
- t_noop_order = 5264
- t_noop_pay = 2014
- t_retries = 0
- tear_snapshots = 36
