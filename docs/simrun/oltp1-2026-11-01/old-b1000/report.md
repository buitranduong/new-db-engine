# stask-m2 report: stask

- run: 2026-11-01T07:30:31Z, harness commit 8a73617, **DIRTY (load1 at start 2.02 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 2.02 / max 7.42 / end 7.42, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 10 | 0 |  |
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
| tear_q13_at_one_snapshot | 13 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 12 | 0 |  |
| tear_reserved_equals_open_items | 13 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 306 | map[] | 5.1 | 63.43 | 106.16 | 127.18 | 159.35 |
| r1_keyset | 15128 | map[] | 252.1 | 5.18 | 18.86 | 34.05 | 85.89 |
| r1_point | 15031 | map[] | 250.5 | 1.69 | 13.46 | 26.74 | 77.98 |
| r2_page | 87 | map[] | 1.4 | 1299.74 | 1408.51 | 1450.15 | 1450.15 |
| r_join | 10 | map[] | 0.2 | 525.02 | 2530.06 | 2530.06 | 2530.06 |
| t_cancel | 2441 | map[] | 40.7 | 10.97 | 50.98 | 89.22 | 170.10 |
| t_order | 4889 | map[] | 81.5 | 12.13 | 54.28 | 88.04 | 189.43 |
| t_pay | 2441 | map[] | 40.7 | 10.86 | 54.63 | 90.02 | 188.56 |
| tear_tx | 9 | map[] | 0.1 | 6200.41 | 6809.99 | 6809.99 | 6809.99 |

## Import shape

shape that ran: **batched (1000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 306 ok, errors map[], p50 63 ms, p95 106 ms, p99 127 ms, max 159 ms; batches 306, rows 306000

## Counters

- import_batches = 306
- import_retries = 0
- import_rows = 306000
- oracle_passes = 10
- t_noop_cancel = 1031
- t_noop_order = 2632
- t_noop_pay = 972
- t_retries = 0
- tear_snapshots = 12
