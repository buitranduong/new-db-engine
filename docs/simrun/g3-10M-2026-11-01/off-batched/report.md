# stask-m2 report: stask

- run: 2026-11-01T05:28:44Z, harness commit 6afb539, **INVALID: LOAD NOT APPLIED**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import always every 5s, hot set 200 at 30%; 120 s elapsed
- machine: 8 CPUs, load1 start 2.10 / max 8.13 / end 8.11, disk free 9.1 GB

## LOAD NOT APPLIED

**This run is INVALID.** The load it was configured to apply did not happen, so "all oracles held" below compares empty sets and proves nothing, and no number of this report may be quoted.

- r2_page: 11 successes, minimum 30 (-r 2, -min-r2)

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 3 | 0 |  |
| no_orphan_order_items | 3 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 3 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 2 | 0 |  |
| r2_pages_prefix_consistent_frozen | 2 | 0 |  |
| reserved_stock_equals_open_items | 3 | 0 |  |
| stock_invariant_q13 | 3 | 0 |  |
| tear_no_order_without_items | 2 | 0 |  |
| tear_no_orphan_items | 2 | 0 |  |
| tear_order_total_equals_items | 2 | 0 |  |
| tear_q13_at_one_snapshot | 2 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 1 | 0 |  |
| tear_reserved_equals_open_items | 2 | 0 |  |

No oracle was violated, but the run is INVALID (load not applied).

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 114 | map[] | 0.9 | 641.52 | 1283.37 | 1356.86 | 1718.09 |
| r1_keyset | 16763 | map[] | 139.7 | 9.16 | 30.05 | 91.91 | 574.57 |
| r1_point | 16717 | map[] | 139.3 | 5.93 | 23.59 | 95.31 | 572.68 |
| r2_page | 11 | map[] | 0.1 | 19296.02 | 20021.01 | 20021.01 | 20021.01 |
| r_join | 1 | map[] | 0.0 | 4902.29 | 4902.29 | 4902.29 | 4902.29 |
| t_cancel | 1322 | map[] | 11.0 | 23.25 | 436.86 | 841.83 | 1688.46 |
| t_order | 2631 | map[] | 21.9 | 28.18 | 469.23 | 757.11 | 1598.43 |
| t_pay | 1310 | map[] | 10.9 | 24.82 | 451.35 | 762.31 | 1191.47 |
| tear_tx | 1 | map[] | 0.0 | 73482.68 | 73482.68 | 73482.68 | 73482.68 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (100000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 114 ok, errors map[], p50 642 ms, p95 1283 ms, p99 1357 ms, max 1718 ms; batches 114, rows 570000

## Counters

- import_batches = 114
- import_retries = 0
- import_rows = 570000
- oracle_passes = 2
- t_noop_cancel = 749
- t_noop_order = 1745
- t_noop_pay = 703
- t_retries = 0
- tear_snapshots = 1
