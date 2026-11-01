# stask-m2 report: stask

- run: 2026-11-01T07:47:17Z, harness commit 8a73617, **DIRTY (load1 at start 2.00 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import always every 1s, hot set 200 at 30%; 60 s elapsed
- **ABLATION (diagnosis only, not an acceptance run): noseq**; import batch 5000 rows
- machine: 8 CPUs, load1 start 2.00 / max 8.19 / end 8.19, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | **1** | t+75.2s journal sequence has a gap or duplicate (n=14325) |
| no_order_without_items | 11 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
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

**1 oracle violations.**

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 82 | map[] | 1.4 | 221.46 | 288.86 | 323.45 | 323.45 |
| r1_keyset | 14613 | map[] | 243.5 | 5.30 | 17.49 | 47.78 | 127.10 |
| r1_point | 14563 | map[] | 242.7 | 1.84 | 12.20 | 46.92 | 127.72 |
| r2_page | 86 | map[] | 1.4 | 1298.28 | 1432.29 | 1488.85 | 1488.85 |
| r_join | 10 | map[] | 0.2 | 569.59 | 2736.97 | 2736.97 | 2736.97 |
| t_cancel | 2690 | map[] | 44.8 | 9.41 | 48.71 | 83.09 | 306.06 |
| t_order | 5446 | map[] | 90.8 | 10.14 | 50.38 | 80.93 | 230.58 |
| t_pay | 2745 | map[] | 45.7 | 9.20 | 46.38 | 81.16 | 237.44 |
| tear_tx | 7 | map[] | 0.1 | 7722.66 | 8166.30 | 8166.30 | 8166.30 |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (10000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 82 ok, errors map[], p50 221 ms, p95 289 ms, p99 323 ms, max 323 ms; batches 82, rows 369000

## Counters

- import_batches = 82
- import_retries = 0
- import_rows = 369000
- oracle_passes = 10
- t_noop_cancel = 1093
- t_noop_order = 2917
- t_noop_pay = 1060
- t_retries = 0
- tear_snapshots = 10
