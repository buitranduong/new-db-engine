# stask-m2 report: stask

- run: 2026-11-01T05:13:38Z, harness commit 6afb539, **DIRTY (load1 at start 1.54 > 1.0)**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import always every 5s, hot set 200 at 30%; 125 s elapsed
- machine: 8 CPUs, load1 start 1.54 / max 8.40 / end 7.78, disk free 9.1 GB

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
| tear_order_total_equals_items | 4 | 0 |  |
| tear_q13_at_one_snapshot | 5 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 4 | 0 |  |
| tear_reserved_equals_open_items | 4 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 4 | map[] | 0.0 | 24417.71 | 48718.20 | 48718.20 | 48718.20 |
| r1_keyset | 19959 | map[] | 160.1 | 7.82 | 27.67 | 50.17 | 3029.17 |
| r1_point | 19997 | map[] | 160.4 | 4.09 | 19.97 | 36.45 | 3020.14 |
| r2_page | 52 | map[] | 0.4 | 120.97 | 213.48 | 518.51 | 518.51 |
| r_join | 8 | map[53200:2] | 0.1 | 8643.71 | 13646.56 | 13646.56 | 13646.56 |
| t_cancel | 1828 | map[] | 14.7 | 19.39 | 93.91 | 148.02 | 2880.04 |
| t_order | 3598 | map[] | 28.9 | 21.66 | 101.36 | 1981.25 | 3036.43 |
| t_pay | 1770 | map[] | 14.2 | 19.58 | 90.94 | 174.58 | 3018.24 |
| tear_tx | 4 | map[] | 0.0 | 31839.72 | 31989.83 | 31989.83 | 31989.83 |

## Error samples (up to 3 distinct messages per class, full text; first_s = seconds into the measured window)

| class | code | count | first_s | message |
|---|---|---|---|---|
| r_join | 53200 | 2 | 2.9 | ERROR: out of memory: the query holds more than its budget of 2005 MB (-query-mem-mb) (SQLSTATE 53200) |

## Import shape

shape that ran: **single (1% in ONE transaction, worst case)**; every import re-prices 1% of the sims (100000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 4 ok, errors map[], p50 24418 ms, p95 48718 ms, p99 48718 ms, max 48718 ms; batches 4, rows 360000

## Counters

- import_batches = 4
- import_retries = 15
- import_rows = 360000
- oracle_passes = 5
- t_noop_cancel = 940
- t_noop_order = 2195
- t_noop_pay = 885
- t_retries = 0
- tear_snapshots = 4
