# stask-m2 report: stask

- run: 2026-11-01T07:38:49Z, harness commit 8a73617, **DIRTY (load1 at start 1.97 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- **ABLATION (diagnosis only, not an acceptance run): othertable**; import batch 5000 rows
- machine: 8 CPUs, load1 start 1.97 / max 7.40 / end 7.40, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | **1** | t+76.5s import history rows=0, journal says 333000 rows in 74 imports |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 10 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 10 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 10 | 0 |  |
| r2_pages_prefix_consistent_frozen | 10 | 0 |  |
| reserved_stock_equals_open_items | 10 | 0 |  |
| stock_invariant_q13 | 11 | 0 |  |
| tear_no_order_without_items | 16 | 0 |  |
| tear_no_orphan_items | 16 | 0 |  |
| tear_order_total_equals_items | 16 | 0 |  |
| tear_q13_at_one_snapshot | 16 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 15 | 0 |  |
| tear_reserved_equals_open_items | 16 | 0 |  |

**1 oracle violations.**

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 74 | map[] | 1.2 | 299.35 | 438.41 | 467.65 | 467.65 |
| r1_keyset | 16354 | map[] | 272.5 | 5.30 | 14.80 | 22.61 | 78.35 |
| r1_point | 16261 | map[] | 271.0 | 1.71 | 10.33 | 17.38 | 59.12 |
| r2_page | 86 | map[] | 1.4 | 1293.90 | 1435.03 | 1486.98 | 1486.98 |
| r_join | 10 | map[] | 0.2 | 536.45 | 2247.40 | 2247.40 | 2247.40 |
| t_cancel | 2073 | map[] | 34.5 | 10.04 | 46.97 | 296.96 | 401.90 |
| t_order | 4121 | map[] | 68.7 | 10.88 | 50.25 | 313.30 | 412.38 |
| t_pay | 2047 | map[] | 34.1 | 9.61 | 47.34 | 315.06 | 622.92 |
| tear_tx | 12 | map[] | 0.2 | 4617.56 | 4898.42 | 4898.42 | 4898.42 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 74 ok, errors map[], p50 299 ms, p95 438 ms, p99 468 ms, max 468 ms; batches 74, rows 333000

## Counters

- import_batches = 74
- import_retries = 0
- import_rows = 333000
- oracle_passes = 10
- t_noop_cancel = 895
- t_noop_order = 2285
- t_noop_pay = 851
- t_retries = 0
- tear_snapshots = 15
