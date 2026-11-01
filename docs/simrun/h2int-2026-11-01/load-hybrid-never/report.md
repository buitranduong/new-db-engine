# stask-m2 report: h2-hybrid-never

- run: 2026-11-01T11:39:40Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.94 / max 5.35 / end 5.24, disk free 2.6 GB

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
| r1_keyset | 15392 | map[] | 384.8 | 3.87 | 8.91 | 14.82 | 63.72 |
| r1_point | 15278 | map[] | 381.9 | 0.42 | 4.28 | 9.15 | 39.05 |
| r2_page | 448 | map[] | 11.2 | 10.95 | 17.98 | 23.07 | 29.84 |
| r_join | 46 | map[] | 1.1 | 547.67 | 6640.00 | 6811.19 | 6811.19 |
| t_cancel | 3247 | map[] | 81.2 | 5.45 | 18.06 | 30.20 | 95.36 |
| t_order | 6550 | map[] | 163.7 | 6.18 | 19.32 | 33.29 | 79.05 |
| t_pay | 3289 | map[] | 82.2 | 5.50 | 17.62 | 30.85 | 85.77 |

## Counters

- oracle_passes = 13
- t_noop_cancel = 1215
- t_noop_order = 3399
- t_noop_pay = 1202
- t_retries = 0
