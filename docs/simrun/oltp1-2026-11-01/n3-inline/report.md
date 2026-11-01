# stask-m2 report: stask

- run: 2026-11-01T08:35:13Z, harness commit 8775433, **DIRTY (load1 at start 1.86 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.86 / max 6.67 / end 6.67, disk free 3.0 GB

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
| import | 82 | map[] | 1.4 | 223.43 | 297.57 | 334.15 | 334.15 |
| r1_keyset | 14632 | map[] | 243.8 | 5.33 | 17.19 | 47.93 | 151.45 |
| r1_point | 14572 | map[] | 242.8 | 1.74 | 11.98 | 43.98 | 147.99 |
| r2_page | 84 | map[] | 1.4 | 1326.86 | 1429.73 | 1474.18 | 1474.18 |
| r_join | 10 | map[] | 0.2 | 580.29 | 3206.77 | 3206.77 | 3206.77 |
| t_cancel | 2322 | map[] | 38.7 | 11.23 | 63.22 | 116.35 | 241.94 |
| t_order | 4600 | map[] | 76.6 | 12.11 | 65.02 | 124.57 | 349.95 |
| t_pay | 2279 | map[] | 38.0 | 10.79 | 58.24 | 113.02 | 257.02 |
| tear_tx | 7 | map[] | 0.1 | 7668.19 | 8297.39 | 8297.39 | 8297.39 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 82 ok, errors map[], p50 223 ms, p95 298 ms, p99 334 ms, max 334 ms; batches 82, rows 369000

## Counters

- import_batches = 82
- import_retries = 0
- import_rows = 369000
- oracle_passes = 10
- t_noop_cancel = 970
- t_noop_order = 2533
- t_noop_pay = 1033
- t_retries = 0
- tear_snapshots = 10
