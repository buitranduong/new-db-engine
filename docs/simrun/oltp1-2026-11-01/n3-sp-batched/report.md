# stask-m2 report: stask

- run: 2026-11-01T08:43:41Z, harness commit 8775433, **DIRTY (load1 at start 1.90 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.90 / max 7.06 / end 7.06, disk free 3.0 GB

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
| import | 84 | map[] | 1.4 | 211.49 | 278.70 | 354.24 | 354.24 |
| r1_keyset | 15560 | map[] | 259.3 | 5.33 | 16.54 | 27.77 | 72.53 |
| r1_point | 15461 | map[] | 257.6 | 1.90 | 11.40 | 20.74 | 70.19 |
| r2_page | 85 | map[] | 1.4 | 1327.46 | 1416.95 | 1442.95 | 1442.95 |
| r_join | 10 | map[] | 0.2 | 601.23 | 2699.22 | 2699.22 | 2699.22 |
| t_cancel | 2394 | map[] | 39.9 | 10.79 | 57.19 | 111.33 | 190.18 |
| t_order | 4785 | map[] | 79.7 | 12.16 | 56.25 | 111.34 | 312.90 |
| t_pay | 2390 | map[] | 39.8 | 10.84 | 53.88 | 110.95 | 340.32 |
| tear_tx | 7 | map[] | 0.1 | 7957.02 | 8341.91 | 8341.91 | 8341.91 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 84 ok, errors map[], p50 211 ms, p95 279 ms, p99 354 ms, max 354 ms; batches 84, rows 378000

## Counters

- import_batches = 84
- import_retries = 0
- import_rows = 378000
- oracle_passes = 10
- t_noop_cancel = 985
- t_noop_order = 2573
- t_noop_pay = 1021
- t_retries = 0
- tear_snapshots = 10
