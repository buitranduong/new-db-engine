# stask-m2 report: h2-hybrid-always

- run: 2026-11-01T11:58:07Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.97 / max 4.81 / end 5.15, disk free 2.5 GB

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
| stock_invariant_q13 | 13 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 54 | map[] | 1.3 | 232.38 | 321.29 | 339.99 | 339.99 |
| r1_keyset | 13306 | map[] | 332.5 | 3.96 | 10.70 | 31.75 | 160.92 |
| r1_point | 13275 | map[] | 331.8 | 0.46 | 5.69 | 20.52 | 162.89 |
| r2_page | 303 | map[] | 7.6 | 12.08 | 19.56 | 30.89 | 38.40 |
| r_join | 30 | map[] | 0.7 | 629.34 | 10308.08 | 10355.88 | 10355.88 |
| t_cancel | 2386 | map[] | 59.6 | 5.86 | 30.03 | 105.30 | 233.09 |
| t_order | 4794 | map[] | 119.8 | 6.61 | 34.37 | 99.85 | 220.38 |
| t_pay | 2405 | map[] | 60.1 | 6.04 | 31.62 | 105.63 | 302.32 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 54 ok, errors map[], p50 232 ms, p95 321 ms, p99 340 ms, max 340 ms; batches 54, rows 243000

## Counters

- import_batches = 54
- import_retries = 0
- import_rows = 243000
- oracle_passes = 13
- t_noop_cancel = 1005
- t_noop_order = 2659
- t_noop_pay = 1014
- t_retries = 0
