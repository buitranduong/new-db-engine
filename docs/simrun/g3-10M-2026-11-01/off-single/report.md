# stask-m2 report: stask

- run: 2026-11-01T05:36:32Z, harness commit 6afb539, **INVALID: LOAD NOT APPLIED**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import always every 5s, hot set 200 at 30%; 125 s elapsed
- machine: 8 CPUs, load1 start 2.09 / max 8.02 / end 7.65, disk free 9.2 GB

## LOAD NOT APPLIED

**This run is INVALID.** The load it was configured to apply did not happen, so "all oracles held" below compares empty sets and proves nothing, and no number of this report may be quoted.

- r2_page: 11 successes, minimum 30 (-r 2, -min-r2)

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 3 | 0 |  |
| no_orphan_order_items | 3 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 3 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 2 | 0 |  |
| r2_pages_prefix_consistent_frozen | 2 | 0 |  |
| reserved_stock_equals_open_items | 3 | 0 |  |
| stock_invariant_q13 | 3 | 0 |  |
| tear_no_order_without_items | 2 | 0 |  |
| tear_no_orphan_items | 2 | 0 |  |
| tear_order_total_equals_items | 2 | 0 |  |
| tear_q13_at_one_snapshot | 2 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 2 | 0 |  |
| tear_reserved_equals_open_items | 2 | 0 |  |

No oracle was violated, but the run is INVALID (load not applied).

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 2 | map[conn:1] | 0.0 | 6821.50 | 48508.98 | 48508.98 | 48508.98 |
| r1_keyset | 18745 | map[] | 149.4 | 9.65 | 26.97 | 43.48 | 2819.59 |
| r1_point | 18745 | map[] | 149.4 | 6.19 | 20.73 | 34.41 | 2817.86 |
| r2_page | 11 | map[] | 0.1 | 19353.36 | 20961.94 | 20961.94 | 20961.94 |
| r_join | 1 | map[] | 0.0 | 3743.14 | 3743.14 | 3743.14 | 3743.14 |
| t_cancel | 1272 | map[] | 10.1 | 29.40 | 97.76 | 156.47 | 7200.75 |
| t_order | 2571 | map[] | 20.5 | 32.26 | 115.26 | 2755.66 | 3366.90 |
| t_pay | 1301 | map[] | 10.4 | 29.12 | 105.80 | 2046.92 | 7208.27 |
| tear_tx | 2 | map[] | 0.0 | 69693.90 | 71613.62 | 71613.62 | 71613.62 |

## Error samples (up to 3 distinct messages per class, full text; first_s = seconds into the measured window)

| class | code | count | first_s | message |
|---|---|---|---|---|
| import | conn | 1 | 60.1 | timeout: context deadline exceeded |

## Import shape

shape that ran: **single (1% in ONE transaction, worst case)**; every import re-prices 1% of the sims (100000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 2 ok, errors map[conn:1], p50 6821 ms, p95 48509 ms, p99 48509 ms, max 48509 ms; batches 2, rows 180000

## Counters

- import_batches = 2
- import_retries = 17
- import_rows = 180000
- oracle_passes = 2
- t_noop_cancel = 753
- t_noop_order = 1765
- t_noop_pay = 706
- t_retries = 0
- tear_snapshots = 2
