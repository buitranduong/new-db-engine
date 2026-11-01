# stask-m2 report: stask

- run: 2026-11-01T07:50:13Z, harness commit 8a73617, **DIRTY (load1 at start 2.27 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- **ABLATION (diagnosis only, not an acceptance run): othertable**; import batch 5000 rows
- machine: 8 CPUs, load1 start 2.27 / max 7.52 / end 7.52, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | **1** | t+76.5s import history rows=0, journal says 459000 rows in 102 imports |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 11 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 11 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 10 | 0 |  |
| r2_pages_prefix_consistent_frozen | 10 | 0 |  |
| reserved_stock_equals_open_items | 11 | 0 |  |
| stock_invariant_q13 | 11 | 0 |  |
| tear_no_order_without_items | 16 | 0 |  |
| tear_no_orphan_items | 16 | 0 |  |
| tear_order_total_equals_items | 16 | 0 |  |
| tear_q13_at_one_snapshot | 16 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 16 | 0 |  |
| tear_reserved_equals_open_items | 16 | 0 |  |

**1 oracle violations.**

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 102 | map[] | 1.7 | 56.65 | 185.06 | 226.88 | 287.92 |
| r1_keyset | 16926 | map[] | 282.0 | 5.05 | 14.48 | 23.79 | 70.19 |
| r1_point | 16771 | map[] | 279.5 | 1.44 | 9.57 | 16.37 | 52.73 |
| r2_page | 87 | map[] | 1.4 | 1276.97 | 1394.95 | 1437.76 | 1437.76 |
| r_join | 11 | map[] | 0.2 | 556.92 | 2131.18 | 2131.18 | 2131.18 |
| t_cancel | 2938 | map[] | 49.0 | 9.99 | 36.65 | 59.67 | 99.40 |
| t_order | 5865 | map[] | 97.7 | 10.95 | 38.92 | 58.88 | 145.70 |
| t_pay | 2922 | map[] | 48.7 | 9.96 | 37.57 | 56.40 | 114.63 |
| tear_tx | 13 | map[] | 0.2 | 4522.77 | 4791.11 | 4791.11 | 4791.11 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 102 ok, errors map[], p50 57 ms, p95 185 ms, p99 227 ms, max 288 ms; batches 102, rows 459000

## Counters

- import_batches = 102
- import_retries = 0
- import_rows = 459000
- oracle_passes = 10
- t_noop_cancel = 1153
- t_noop_order = 3036
- t_noop_pay = 1133
- t_retries = 0
- tear_snapshots = 16
