# stask-m2 report: h1-hybrid-always

- run: 2026-11-01T11:06:00Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.99 / max 5.10 / end 5.10, disk free 2.4 GB

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
| import | 52 | map[] | 1.3 | 236.95 | 353.21 | 437.67 | 437.67 |
| r1_keyset | 13737 | map[] | 343.4 | 3.88 | 9.87 | 32.67 | 134.54 |
| r1_point | 13691 | map[] | 342.3 | 0.45 | 5.08 | 16.24 | 128.80 |
| r2_page | 321 | map[] | 8.0 | 12.31 | 21.41 | 25.21 | 30.78 |
| r_join | 32 | map[] | 0.8 | 598.04 | 9323.65 | 10941.35 | 10941.35 |
| t_cancel | 2497 | map[] | 62.4 | 6.08 | 27.05 | 105.09 | 261.57 |
| t_order | 4965 | map[] | 124.1 | 6.46 | 28.86 | 106.33 | 281.01 |
| t_pay | 2470 | map[] | 61.7 | 5.89 | 27.76 | 103.94 | 471.29 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 52 ok, errors map[], p50 237 ms, p95 353 ms, p99 438 ms, max 438 ms; batches 52, rows 234000

## Counters

- import_batches = 52
- import_retries = 0
- import_rows = 234000
- oracle_passes = 13
- t_noop_cancel = 1012
- t_noop_order = 2706
- t_noop_pay = 962
- t_retries = 0
