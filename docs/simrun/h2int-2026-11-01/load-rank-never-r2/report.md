# stask-m2 report: h2-rank-never

- run: 2026-11-01T11:49:06Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.93 / max 5.59 / end 5.38, disk free 2.6 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 14 | 0 |  |
| no_orphan_order_items | 14 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 14 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 14 | 0 |  |
| r2_pages_prefix_consistent_frozen | 14 | 0 |  |
| reserved_stock_equals_open_items | 14 | 0 |  |
| stock_invariant_q13 | 14 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 15635 | map[] | 390.8 | 3.73 | 8.80 | 15.66 | 57.80 |
| r1_point | 15507 | map[] | 387.6 | 0.39 | 4.39 | 10.09 | 34.15 |
| r2_page | 567 | map[] | 14.2 | 1.93 | 4.04 | 8.00 | 11.45 |
| r_join | 56 | map[] | 1.4 | 520.52 | 6558.02 | 6658.33 | 6658.33 |
| t_cancel | 3397 | map[] | 84.9 | 5.03 | 18.87 | 36.71 | 64.16 |
| t_order | 6769 | map[] | 169.2 | 5.56 | 19.10 | 34.49 | 116.83 |
| t_pay | 3370 | map[] | 84.2 | 4.96 | 17.05 | 34.38 | 115.98 |

## Counters

- oracle_passes = 13
- t_noop_cancel = 1248
- t_noop_order = 3494
- t_noop_pay = 1268
- t_retries = 0
