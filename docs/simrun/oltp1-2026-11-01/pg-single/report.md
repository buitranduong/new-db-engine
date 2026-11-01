# stask-m2 report: pg16

- run: 2026-11-01T08:05:23Z, harness commit 8a73617, **DIRTY (load1 at start 2.17 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 2.17 / max 7.61 / end 7.61, disk free 1.8 GB

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
| import | 40 | map[] | 0.7 | 501.36 | 679.55 | 696.64 | 696.64 |
| r1_keyset | 30758 | map[] | 510.4 | 2.05 | 4.50 | 6.43 | 15.36 |
| r1_point | 30619 | map[] | 508.1 | 0.79 | 2.23 | 3.77 | 9.63 |
| r2_page | 144 | map[] | 2.4 | 805.92 | 874.60 | 917.99 | 926.27 |
| r_join | 16 | map[] | 0.3 | 210.31 | 427.90 | 427.90 | 427.90 |
| t_cancel | 4750 | map[] | 78.8 | 6.86 | 12.52 | 17.58 | 221.85 |
| t_order | 9524 | map[] | 158.1 | 7.62 | 14.30 | 20.74 | 614.10 |
| t_pay | 4771 | map[] | 79.2 | 6.84 | 12.31 | 17.20 | 82.32 |
| tear_tx | 29 | map[] | 0.5 | 1935.07 | 2012.92 | 2032.50 | 2032.50 |

## Import shape

shape that ran: **single (1% in ONE transaction, worst case)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 40 ok, errors map[], p50 501 ms, p95 680 ms, p99 697 ms, max 697 ms; batches 40, rows 360000

## Counters

- import_batches = 40
- import_retries = 0
- import_rows = 360000
- oracle_passes = 28
- t_noop_cancel = 1964
- t_noop_order = 5231
- t_noop_pay = 1925
- t_retries = 0
- tear_snapshots = 36
