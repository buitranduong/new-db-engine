# stask-m2 report: stask

- run: 2026-11-01T08:32:27Z, harness commit 8775433, **DIRTY (load1 at start 1.04 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.04 / max 6.10 / end 6.66, disk free 3.0 GB

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
| tear_no_order_without_items | 10 | 0 |  |
| tear_no_orphan_items | 10 | 0 |  |
| tear_order_total_equals_items | 10 | 0 |  |
| tear_q13_at_one_snapshot | 11 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 10 | 0 |  |
| tear_reserved_equals_open_items | 10 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 82 | map[] | 1.4 | 221.62 | 281.73 | 418.89 | 418.89 |
| r1_keyset | 15258 | map[] | 254.2 | 5.38 | 17.30 | 31.41 | 128.76 |
| r1_point | 15133 | map[] | 252.1 | 1.79 | 12.03 | 22.72 | 76.62 |
| r2_page | 83 | map[] | 1.4 | 1337.07 | 1466.84 | 1587.45 | 1587.45 |
| r_join | 10 | map[] | 0.2 | 577.48 | 3364.38 | 3364.38 | 3364.38 |
| t_cancel | 2350 | map[] | 39.2 | 11.12 | 58.59 | 105.37 | 217.41 |
| t_order | 4670 | map[] | 77.8 | 11.93 | 62.58 | 118.19 | 295.06 |
| t_pay | 2326 | map[] | 38.8 | 11.02 | 52.98 | 114.00 | 321.99 |
| tear_tx | 7 | map[] | 0.1 | 8088.71 | 8415.55 | 8415.55 | 8415.55 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 82 ok, errors map[], p50 222 ms, p95 282 ms, p99 419 ms, max 419 ms; batches 82, rows 369000

## Counters

- import_batches = 82
- import_retries = 0
- import_rows = 369000
- oracle_passes = 10
- t_noop_cancel = 977
- t_noop_order = 2500
- t_noop_pay = 972
- t_retries = 0
- tear_snapshots = 10
