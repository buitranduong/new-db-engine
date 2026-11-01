# stask-m2 report: h1-hybrid-always

- run: 2026-11-01T10:51:33Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 1.00 / max 5.43 / end 5.39, disk free 2.5 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 13 | 0 |  |
| no_orphan_order_items | 13 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 13 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 13 | 0 |  |
| r2_pages_prefix_consistent_frozen | 13 | 0 |  |
| reserved_stock_equals_open_items | 13 | 0 |  |
| stock_invariant_q13 | 14 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 53 | map[] | 1.3 | 239.57 | 330.98 | 356.28 | 356.28 |
| r1_keyset | 13887 | map[] | 346.5 | 3.83 | 9.73 | 25.07 | 137.40 |
| r1_point | 13846 | map[] | 345.4 | 0.43 | 5.09 | 16.04 | 131.36 |
| r2_page | 288 | map[] | 7.2 | 12.63 | 18.97 | 67.03 | 108.52 |
| r_join | 28 | map[] | 0.7 | 588.04 | 10333.67 | 10528.31 | 10528.31 |
| t_cancel | 2505 | map[] | 62.5 | 5.66 | 27.46 | 98.00 | 263.78 |
| t_order | 5069 | map[] | 126.5 | 6.28 | 32.19 | 110.27 | 247.76 |
| t_pay | 2564 | map[] | 64.0 | 5.67 | 25.66 | 109.01 | 239.71 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 53 ok, errors map[], p50 240 ms, p95 331 ms, p99 356 ms, max 356 ms; batches 53, rows 239000

## Counters

- import_batches = 53
- import_retries = 0
- import_rows = 239000
- oracle_passes = 13
- t_noop_cancel = 1052
- t_noop_order = 2764
- t_noop_pay = 964
- t_retries = 0
