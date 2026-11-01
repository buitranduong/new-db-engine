# stask-m2 report: h2-hybrid-always

- run: 2026-11-01T11:45:53Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.96 / max 6.02 / end 6.02, disk free 2.5 GB

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
| import | 53 | map[] | 1.3 | 235.30 | 360.32 | 380.80 | 380.80 |
| r1_keyset | 13677 | map[] | 340.7 | 3.90 | 10.18 | 25.00 | 176.26 |
| r1_point | 13657 | map[] | 340.2 | 0.44 | 5.64 | 17.79 | 174.59 |
| r2_page | 303 | map[] | 7.5 | 12.60 | 21.91 | 27.62 | 33.10 |
| r_join | 30 | map[] | 0.7 | 541.06 | 10276.92 | 10792.29 | 10792.29 |
| t_cancel | 2502 | map[] | 62.3 | 5.77 | 30.11 | 102.94 | 195.33 |
| t_order | 4994 | map[] | 124.4 | 6.39 | 33.19 | 111.30 | 253.64 |
| t_pay | 2491 | map[] | 62.1 | 5.61 | 30.54 | 94.97 | 268.78 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 53 ok, errors map[], p50 235 ms, p95 360 ms, p99 381 ms, max 381 ms; batches 53, rows 239000

## Counters

- import_batches = 53
- import_retries = 0
- import_rows = 239000
- oracle_passes = 13
- t_noop_cancel = 1033
- t_noop_order = 2734
- t_noop_pay = 1015
- t_retries = 0
