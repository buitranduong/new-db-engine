# stask-m2 report: stask

- run: 2026-11-01T05:06:08Z, harness commit 6afb539, **DIRTY (load1 at start 1.53 > 1.0)**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import always every 5s, hot set 200 at 30%; 120 s elapsed
- machine: 8 CPUs, load1 start 1.53 / max 8.19 / end 7.69, disk free 9.1 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 5 | 0 |  |
| no_orphan_order_items | 5 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 5 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 5 | 0 |  |
| r2_pages_prefix_consistent_frozen | 5 | 0 |  |
| reserved_stock_equals_open_items | 5 | 0 |  |
| stock_invariant_q13 | 6 | 0 |  |
| tear_no_order_without_items | 5 | 0 |  |
| tear_no_orphan_items | 5 | 0 |  |
| tear_order_total_equals_items | 5 | 0 |  |
| tear_q13_at_one_snapshot | 5 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 4 | 0 |  |
| tear_reserved_equals_open_items | 4 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 112 | map[] | 0.9 | 644.61 | 1406.65 | 1566.74 | 2178.05 |
| r1_keyset | 20145 | map[] | 167.3 | 7.04 | 27.17 | 75.77 | 457.89 |
| r1_point | 20153 | map[] | 167.4 | 3.45 | 19.38 | 69.99 | 469.64 |
| r2_page | 66 | map[] | 0.5 | 133.24 | 218.74 | 627.10 | 627.10 |
| r_join | 8 | map[53200:2] | 0.1 | 8754.31 | 13194.09 | 13194.09 | 13194.09 |
| t_cancel | 1707 | map[] | 14.2 | 16.66 | 409.00 | 854.93 | 2068.03 |
| t_order | 3363 | map[] | 27.9 | 17.30 | 408.31 | 783.99 | 1228.96 |
| t_pay | 1659 | map[] | 13.8 | 16.59 | 379.07 | 811.84 | 2027.44 |
| tear_tx | 4 | map[] | 0.0 | 30070.70 | 32397.62 | 32397.62 | 32397.62 |

## Error samples (up to 3 distinct messages per class, full text; first_s = seconds into the measured window)

| class | code | count | first_s | message |
|---|---|---|---|---|
| r_join | 53200 | 2 | 2.8 | ERROR: out of memory: the query holds more than its budget of 2005 MB (-query-mem-mb) (SQLSTATE 53200) |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (100000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 112 ok, errors map[], p50 645 ms, p95 1407 ms, p99 1567 ms, max 2178 ms; batches 112, rows 560000

## Counters

- import_batches = 112
- import_retries = 0
- import_rows = 560000
- oracle_passes = 5
- t_noop_cancel = 855
- t_noop_order = 2133
- t_noop_pay = 846
- t_retries = 0
- tear_snapshots = 4
