# stask-m2 report: h1-off-always

- run: 2026-11-01T10:57:30Z, harness commit , **DIRTY (load1 at start 1.12 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 1.12 / max 5.90 / end 5.90, disk free 2.5 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 8 | 0 |  |
| no_orphan_order_items | 8 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 8 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 7 | 0 |  |
| r2_pages_prefix_consistent_frozen | 7 | 0 |  |
| reserved_stock_equals_open_items | 8 | 0 |  |
| stock_invariant_q13 | 8 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 54 | map[] | 1.3 | 221.48 | 361.79 | 413.75 | 413.75 |
| r1_keyset | 13554 | map[] | 338.8 | 4.04 | 9.86 | 25.04 | 116.39 |
| r1_point | 13518 | map[] | 337.9 | 0.52 | 6.04 | 20.89 | 116.62 |
| r2_page | 60 | map[] | 1.5 | 1199.87 | 1295.01 | 1345.49 | 1345.49 |
| r_join | 6 | map[] | 0.1 | 475.93 | 7241.60 | 7241.60 | 7241.60 |
| t_cancel | 2433 | map[] | 60.8 | 6.21 | 32.44 | 93.02 | 275.45 |
| t_order | 4847 | map[] | 121.2 | 6.94 | 33.29 | 98.72 | 284.32 |
| t_pay | 2415 | map[] | 60.4 | 6.16 | 35.89 | 95.73 | 292.55 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 54 ok, errors map[], p50 221 ms, p95 362 ms, p99 414 ms, max 414 ms; batches 54, rows 243000

## Counters

- import_batches = 54
- import_retries = 0
- import_rows = 243000
- oracle_passes = 7
- t_noop_cancel = 955
- t_noop_order = 2592
- t_noop_pay = 953
- t_retries = 0
