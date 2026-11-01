# stask-m2 report: pg16

- run: 2026-11-01T08:20:39Z, harness commit 8a73617, **DIRTY (load1 at start 1.97 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=0, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.97 / max 3.77 / end 3.77, disk free 2.1 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 30 | 0 |  |
| no_orphan_order_items | 30 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 30 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 30 | 0 |  |
| r2_pages_prefix_consistent_frozen | 30 | 0 |  |
| reserved_stock_equals_open_items | 30 | 0 |  |
| stock_invariant_q13 | 30 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 36299 | map[] | 604.9 | 1.57 | 2.30 | 2.90 | 8.99 |
| r1_point | 36280 | map[] | 604.6 | 0.63 | 0.85 | 1.61 | 5.93 |
| t_cancel | 6655 | map[] | 110.9 | 4.74 | 7.20 | 9.36 | 34.64 |
| t_order | 13427 | map[] | 223.8 | 5.17 | 8.03 | 10.23 | 44.95 |
| t_pay | 6778 | map[] | 113.0 | 4.77 | 7.32 | 9.52 | 41.82 |

## Counters

- oracle_passes = 29
- t_noop_cancel = 2659
- t_noop_order = 7207
- t_noop_pay = 2587
- t_retries = 0
