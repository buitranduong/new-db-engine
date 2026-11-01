# stask-m2 report: h2-rank-always

- run: 2026-11-01T11:42:52Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.87 / max 5.04 / end 5.44, disk free 2.5 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 14 | 0 |  |
| no_orphan_order_items | 14 | 0 |  |
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
| import | 54 | map[] | 1.3 | 233.50 | 338.44 | 371.46 | 371.46 |
| r1_keyset | 13524 | map[] | 338.0 | 3.92 | 10.80 | 34.62 | 147.36 |
| r1_point | 13484 | map[] | 337.0 | 0.44 | 5.69 | 18.19 | 143.61 |
| r2_page | 311 | map[] | 7.8 | 3.32 | 8.33 | 14.72 | 24.97 |
| r_join | 32 | map[] | 0.8 | 587.77 | 10318.75 | 10476.65 | 10476.65 |
| t_cancel | 2531 | map[] | 63.3 | 5.51 | 28.54 | 99.44 | 310.99 |
| t_order | 5082 | map[] | 127.0 | 6.27 | 32.70 | 109.22 | 414.40 |
| t_pay | 2540 | map[] | 63.5 | 5.66 | 34.70 | 110.95 | 206.39 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 54 ok, errors map[], p50 234 ms, p95 338 ms, p99 371 ms, max 371 ms; batches 54, rows 243000

## Counters

- import_batches = 54
- import_retries = 0
- import_rows = 243000
- oracle_passes = 13
- t_noop_cancel = 987
- t_noop_order = 2738
- t_noop_pay = 1005
- t_retries = 0
