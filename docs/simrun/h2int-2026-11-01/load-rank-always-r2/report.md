# stask-m2 report: h2-rank-always

- run: 2026-11-01T11:55:08Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.97 / max 4.54 / end 5.14, disk free 2.5 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 13 | 0 |  |
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
| import | 54 | map[] | 1.3 | 233.07 | 317.77 | 397.17 | 397.17 |
| r1_keyset | 13646 | map[] | 340.5 | 3.85 | 10.44 | 27.01 | 165.07 |
| r1_point | 13621 | map[] | 339.9 | 0.43 | 5.68 | 20.27 | 162.09 |
| r2_page | 320 | map[] | 8.0 | 3.59 | 7.57 | 11.11 | 68.55 |
| r_join | 35 | map[] | 0.9 | 547.43 | 10381.84 | 10513.10 | 10513.10 |
| t_cancel | 2522 | map[] | 62.9 | 5.63 | 30.04 | 101.13 | 267.80 |
| t_order | 5054 | map[] | 126.1 | 6.20 | 34.55 | 109.04 | 394.57 |
| t_pay | 2525 | map[] | 63.0 | 5.74 | 34.71 | 112.35 | 221.79 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 54 ok, errors map[], p50 233 ms, p95 318 ms, p99 397 ms, max 397 ms; batches 54, rows 243000

## Counters

- import_batches = 54
- import_retries = 0
- import_rows = 243000
- oracle_passes = 13
- t_noop_cancel = 1027
- t_noop_order = 2817
- t_noop_pay = 1010
- t_retries = 0
