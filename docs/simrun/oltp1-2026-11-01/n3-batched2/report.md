# stask-m2 report: stask

- run: 2026-11-01T08:37:59Z, harness commit 8775433, **DIRTY (load1 at start 1.81 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.81 / max 7.07 / end 7.07, disk free 3.0 GB

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
| stock_invariant_q13 | 11 | 0 |  |
| tear_no_order_without_items | 11 | 0 |  |
| tear_no_orphan_items | 11 | 0 |  |
| tear_order_total_equals_items | 11 | 0 |  |
| tear_q13_at_one_snapshot | 11 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 10 | 0 |  |
| tear_reserved_equals_open_items | 11 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 84 | map[] | 1.4 | 208.13 | 309.48 | 400.82 | 400.82 |
| r1_keyset | 15491 | map[] | 258.2 | 5.30 | 17.12 | 30.21 | 90.64 |
| r1_point | 15400 | map[] | 256.6 | 1.77 | 11.64 | 21.98 | 88.94 |
| r2_page | 84 | map[] | 1.4 | 1347.84 | 1431.75 | 1463.07 | 1463.07 |
| r_join | 10 | map[] | 0.2 | 543.95 | 3141.77 | 3141.77 | 3141.77 |
| t_cancel | 2413 | map[] | 40.2 | 10.68 | 57.65 | 117.16 | 376.21 |
| t_order | 4787 | map[] | 79.8 | 11.94 | 58.32 | 117.55 | 430.89 |
| t_pay | 2378 | map[] | 39.6 | 10.66 | 54.71 | 109.60 | 383.84 |
| tear_tx | 7 | map[] | 0.1 | 7867.79 | 8442.03 | 8442.03 | 8442.03 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 84 ok, errors map[], p50 208 ms, p95 309 ms, p99 401 ms, max 401 ms; batches 84, rows 378000

## Counters

- import_batches = 84
- import_retries = 0
- import_rows = 378000
- oracle_passes = 10
- t_noop_cancel = 981
- t_noop_order = 2568
- t_noop_pay = 1039
- t_retries = 0
- tear_snapshots = 10
