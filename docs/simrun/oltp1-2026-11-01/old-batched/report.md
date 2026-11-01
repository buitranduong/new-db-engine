# stask-m2 report: stask

- run: 2026-11-01T07:05:34Z, harness commit 8a73617, **DIRTY (load1 at start 1.03 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.03 / max 6.51 / end 6.51, disk free 3.4 GB

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
| tear_no_order_without_items | 12 | 0 |  |
| tear_no_orphan_items | 12 | 0 |  |
| tear_order_total_equals_items | 12 | 0 |  |
| tear_q13_at_one_snapshot | 12 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 12 | 0 |  |
| tear_reserved_equals_open_items | 12 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 64 | map[] | 1.1 | 433.69 | 578.28 | 605.97 | 605.97 |
| r1_keyset | 15094 | map[] | 250.6 | 5.32 | 15.82 | 41.92 | 150.25 |
| r1_point | 14992 | map[] | 248.9 | 1.88 | 11.16 | 23.88 | 147.82 |
| r2_page | 86 | map[] | 1.4 | 1327.89 | 1461.27 | 1530.98 | 1530.98 |
| r_join | 10 | map[] | 0.2 | 523.57 | 2083.31 | 2083.31 | 2083.31 |
| t_cancel | 1879 | map[] | 31.2 | 10.38 | 51.13 | 378.79 | 603.09 |
| t_order | 3723 | map[] | 61.8 | 12.02 | 52.83 | 384.56 | 547.92 |
| t_pay | 1841 | map[] | 30.6 | 10.82 | 54.26 | 363.85 | 455.53 |
| tear_tx | 9 | map[] | 0.1 | 6592.99 | 7378.30 | 7378.30 | 7378.30 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 64 ok, errors map[], p50 434 ms, p95 578 ms, p99 606 ms, max 606 ms; batches 64, rows 288000

## Counters

- import_batches = 64
- import_retries = 0
- import_rows = 288000
- oracle_passes = 10
- t_noop_cancel = 804
- t_noop_order = 2105
- t_noop_pay = 805
- t_retries = 0
- tear_snapshots = 12
