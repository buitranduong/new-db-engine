# stask-m2 report: stask

- run: 2026-11-01T07:36:03Z, harness commit 8a73617, **DIRTY (load1 at start 2.09 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- **ABLATION (diagnosis only, not an acceptance run): noseq**; import batch 5000 rows
- machine: 8 CPUs, load1 start 2.09 / max 6.98 / end 6.98, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | **1** | t+75.4s journal sequence has a gap or duplicate (n=12445) |
| no_order_without_items | 10 | 0 |  |
| no_orphan_order_items | 10 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 10 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 10 | 0 |  |
| r2_pages_prefix_consistent_frozen | 10 | 0 |  |
| reserved_stock_equals_open_items | 10 | 0 |  |
| stock_invariant_q13 | 10 | 0 |  |
| tear_no_order_without_items | 12 | 0 |  |
| tear_no_orphan_items | 12 | 0 |  |
| tear_order_total_equals_items | 12 | 0 |  |
| tear_q13_at_one_snapshot | 12 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 11 | 0 |  |
| tear_reserved_equals_open_items | 12 | 0 |  |

**1 oracle violations.**

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 63 | map[] | 1.0 | 477.26 | 579.41 | 609.69 | 609.69 |
| r1_keyset | 14869 | map[] | 246.9 | 5.34 | 17.12 | 42.41 | 150.02 |
| r1_point | 14752 | map[] | 245.0 | 1.79 | 11.53 | 31.42 | 157.11 |
| r2_page | 84 | map[] | 1.4 | 1333.44 | 1440.99 | 1464.37 | 1464.37 |
| r_join | 10 | map[] | 0.2 | 556.80 | 2612.72 | 2612.72 | 2612.72 |
| t_cancel | 2219 | map[] | 36.8 | 8.18 | 46.75 | 282.35 | 574.47 |
| t_order | 4435 | map[] | 73.6 | 9.80 | 54.70 | 308.12 | 457.62 |
| t_pay | 2217 | map[] | 36.8 | 8.56 | 47.16 | 293.99 | 383.13 |
| tear_tx | 8 | map[] | 0.1 | 6777.12 | 7941.98 | 7941.98 | 7941.98 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 63 ok, errors map[], p50 477 ms, p95 579 ms, p99 610 ms, max 610 ms; batches 63, rows 284000

## Counters

- import_batches = 63
- import_retries = 0
- import_rows = 284000
- oracle_passes = 10
- t_noop_cancel = 931
- t_noop_order = 2543
- t_noop_pay = 909
- t_retries = 0
- tear_snapshots = 11
